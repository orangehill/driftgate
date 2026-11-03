package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/orangehill/driftgate/internal/chaos"
)

// A restore that cannot bring items back one by one (sqlserver_restore.go)
// restores the whole working database from the backup Seed made of the
// pristine copy (its slot file, named in no other way than by the backup
// set GUID of the mark). It ends every session of the working database:
// SET OFFLINE WITH ROLLBACK IMMEDIATE, then RESTORE … WITH REPLACE onto
// the working database's own files. The owner, TRUSTWORTHY and
// DB_CHAINING live in master and stay; Service Broker is switched on
// again when it was on; the recovery model is set back. An application's
// pooled connection fails once afterwards (the server ended it).
//
// Excluded tables are not part of the pristine copy's rows, so their rows
// are saved first into <pristine>_dgx, a database of DriftGate's own
// (marked), and copied back after the RESTORE. Its manifest says when the
// save is complete and which recovery fork the working database was on:
// RESTORE starts a new one, so a save left by a restore that stopped is
// copied back only when the working database was restored since, and
// dropped otherwise.

// ssPaths are the server's default directories (no trailing separator
// needed: ssJoin adds the server's own).
type ssPaths struct{ data, log, backup string }

func (s *SQLServer) paths(ctx context.Context, c *sql.Conn) (*ssPaths, error) {
	p := &ssPaths{}
	err := c.QueryRowContext(ctx, `SELECT ISNULL(CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512)), N''),
		ISNULL(CAST(SERVERPROPERTY('InstanceDefaultLogPath') AS nvarchar(512)), N''),
		ISNULL(CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(512)), N'')`).Scan(&p.data, &p.log, &p.backup)
	if err != nil {
		return nil, err
	}
	if p.data == "" {
		return nil, fmt.Errorf("state %s: the server does not report its default data directory (InstanceDefaultDataPath)", s.o.Name)
	}
	if p.log == "" {
		p.log = p.data
	}
	if p.backup == "" { // SQL Server 2017
		p.backup = p.data
	}
	return p, nil
}

// ssJoin joins a directory of the server and a file name with the
// server's separator (a Windows server's paths have backslashes, whatever
// DriftGate runs on).
func ssJoin(dir, name string) string {
	sep := "/"
	if strings.Contains(dir, `\`) {
		sep = `\`
	}
	return strings.TrimRight(dir, `/\`) + sep + name
}

// backupSlots are the two files the fallback backups of a pristine copy
// alternate between: a seed writes the one the current copy does not use,
// so that copy keeps a valid backup until the new one replaces it, and no
// more than two files are ever left (deleting a file needs sysadmin).
func (s *SQLServer) backupSlots(p *ssPaths) [2]string {
	h := sha256.Sum256([]byte(strings.ToLower(s.o.Working + "\x00" + s.o.Pristine)))
	base := "driftgate-" + hex.EncodeToString(h[:4])
	return [2]string{ssJoin(p.backup, base+"-a.bak"), ssJoin(p.backup, base+"-b.bak")}
}

// ssColumns reads a result set whose columns are known by name only
// (RESTORE HEADERONLY and FILELISTONLY return dozens, which differ by
// version): one map per row.
func ssColumns(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, n := range names {
			row[strings.ToLower(n)] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func ssText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		if len(x) == 16 { // uniqueidentifier
			var u mssql.UniqueIdentifier
			if u.Scan(x) == nil {
				return u.String()
			}
		}
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

// backupSet reads the backup set GUID of the backup in a file.
func backupSet(ctx context.Context, c *sql.Conn, path string) (string, error) {
	rows, err := c.QueryContext(ctx, "RESTORE HEADERONLY FROM DISK = "+sqlServerString(path))
	if err != nil {
		return "", sqlServerFirst(err)
	}
	sets, err := ssColumns(rows)
	if err != nil {
		return "", sqlServerFirst(err)
	}
	if len(sets) != 1 {
		return "", fmt.Errorf("%s holds %d backup sets, not one", path, len(sets))
	}
	return ssText(sets[0]["backupsetguid"]), nil
}

// ssBackupFile is a file of the database in a backup.
type ssBackupFile struct {
	Logical, Physical, Type string // Type: D data, L log, S FILESTREAM or memory-optimized container, F full-text
	ID                      int64
}

// backupFiles lists the files of the database in a backup.
func backupFiles(ctx context.Context, c *sql.Conn, path string) ([]ssBackupFile, error) {
	rows, err := c.QueryContext(ctx, "RESTORE FILELISTONLY FROM DISK = "+sqlServerString(path))
	if err != nil {
		return nil, sqlServerFirst(err)
	}
	list, err := ssColumns(rows)
	if err != nil {
		return nil, sqlServerFirst(err)
	}
	var out []ssBackupFile
	for _, r := range list {
		id, _ := strconv.ParseInt(ssText(r["fileid"]), 10, 64)
		out = append(out, ssBackupFile{Logical: ssText(r["logicalname"]), Physical: ssText(r["physicalname"]), Type: ssText(r["type"]), ID: id})
	}
	return out, nil
}

// fallbackFile finds the slot file that holds the pristine copy's backup
// named by its mark.
func (s *SQLServer) fallbackFile(ctx context.Context, c *sql.Conn, mark *sqlServerMark) (string, error) {
	p, err := s.paths(ctx, c)
	if err != nil {
		return "", err
	}
	slots := s.backupSlots(p)
	for _, f := range slots {
		if mark.Fallback == "" {
			break
		}
		if guid, err := backupSet(ctx, c, f); err == nil && strings.EqualFold(guid, mark.Fallback) {
			return f, nil
		}
	}
	return "", fmt.Errorf("state %s: the backup of pristine database %s (%s or %s) is missing or was replaced; run driftgate seed", s.o.Name, s.o.Pristine, slots[0], slots[1])
}

// restoreDatabase brings the whole working database w back from the
// backup of the pristine copy (see above).
func (s *SQLServer) restoreDatabase(ctx context.Context, mark *sqlServerMark, w *sqlServerDatabase) error {
	s.forget()
	defer s.forget()
	info, err := s.server(ctx)
	if err != nil {
		return err
	}
	m, err := s.conn(ctx, "")
	if err != nil {
		return err
	}
	defer m.Close()
	path, err := s.fallbackFile(ctx, m, mark)
	if err != nil {
		return err
	}
	files, err := backupFiles(ctx, m, path)
	if err != nil {
		return fmt.Errorf("state %s: %s: %w", s.o.Name, path, err)
	}
	// RESTORE … WITH REPLACE writes over the working database's own files.
	own := map[string]string{}
	rows, err := m.QueryContext(ctx, "SELECT name, physical_name FROM sys.master_files WHERE database_id = @p1", w.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, physical string
		if err := rows.Scan(&name, &physical); err != nil {
			rows.Close()
			return err
		}
		own[strings.ToLower(name)] = physical
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	restore := "RESTORE DATABASE " + qs(s.o.Working) + " FROM DISK = " + sqlServerString(path) + " WITH REPLACE, RECOVERY, CHECKSUM"
	for _, f := range files {
		physical, ok := own[strings.ToLower(f.Logical)]
		if !ok || len(files) != len(own) {
			return fmt.Errorf("state %s: the files of database %s changed since the seed (%s is not one of them now); run driftgate seed", s.o.Name, s.o.Working, f.Logical)
		}
		restore += ", MOVE " + sqlServerString(f.Logical) + " TO " + sqlServerString(physical)
	}
	online := w.State == "ONLINE"
	if w.Broker && (online || w.State == "OFFLINE") { // a restoring database's flags are not known
		restore += ", ENABLE_BROKER"
	}
	saved, err := s.saveExcluded(ctx, w)
	if err != nil {
		return err
	}
	if saved {
		chaos.Point("state.sqlserver.restore.saved")
	}
	s.closePool(s.o.Working)
	if online {
		if info.Sysadmin {
			// Ending the sessions first saves the ~3 s ROLLBACK IMMEDIATE
			// waits for each one otherwise; only sysadmin may KILL.
			s.endSessions(ctx, m, w.ID)
		}
		if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qs(s.o.Working)+" SET OFFLINE WITH ROLLBACK IMMEDIATE"); err != nil {
			return fmt.Errorf("state %s: taking %s offline: %w", s.o.Name, s.o.Working, err)
		}
	}
	chaos.Point("state.sqlserver.restore.offline")
	if _, err := m.ExecContext(ctx, restore); err != nil {
		// A RESTORE that failed before it began leaves the database
		// offline; one that stopped halfway, restoring, and the next
		// restore runs it again.
		if d, derr := s.database(context.Background(), s.o.Working); derr == nil && d != nil && d.State == "OFFLINE" {
			m.ExecContext(context.Background(), "ALTER DATABASE "+qs(s.o.Working)+" SET ONLINE")
		}
		return fmt.Errorf("state %s: restore of %s from %s: %w", s.o.Name, s.o.Working, path, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.restore.restored")
	if w.State == "ONLINE" || w.State == "OFFLINE" {
		if now, err := s.database(ctx, s.o.Working); err == nil && now != nil && now.Recovery != w.Recovery && w.Recovery != "" {
			if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qs(s.o.Working)+" SET RECOVERY "+w.Recovery); err != nil {
				return err
			}
		}
	}
	if saved {
		return s.copyBack(ctx)
	}
	return nil
}

// endSessions ends every other session in a database (sysadmin only).
func (s *SQLServer) endSessions(ctx context.Context, m *sql.Conn, id int) {
	rows, err := m.QueryContext(ctx, "SELECT session_id FROM sys.dm_exec_sessions WHERE database_id = @p1 AND session_id <> @@SPID", id)
	if err != nil {
		return
	}
	var ids []int
	for rows.Next() {
		var n int
		if rows.Scan(&n) == nil {
			ids = append(ids, n)
		}
	}
	rows.Close()
	for _, n := range ids {
		m.ExecContext(ctx, "KILL "+strconv.Itoa(n))
	}
}

// excludedDB is the database excluded tables are saved in during a
// restore of the whole working database.
func (s *SQLServer) excludedDB() string { return s.o.Pristine + "_dgx" }

// recoveryFork is the working database's recovery fork: every RESTORE
// WITH RECOVERY starts a new one.
func (s *SQLServer) recoveryFork(ctx context.Context, id int) (string, error) {
	c, err := s.conn(ctx, "")
	if err != nil {
		return "", err
	}
	defer c.Close()
	var fork sql.NullString
	err = c.QueryRowContext(ctx, "SELECT CONVERT(nvarchar(36), recovery_fork_guid) FROM sys.database_recovery_status WHERE database_id = @p1", id).Scan(&fork)
	if err != nil {
		return "", err
	}
	return fork.String, nil
}

// leftSave looks at a <pristine>_dgx a restore of the whole database left
// (it stopped): whether its rows must still be copied back (the save is
// complete and the working database w was restored since, or is being
// restored). Any other one is dropped.
func (s *SQLServer) leftSave(ctx context.Context, w *sqlServerDatabase) (bool, error) {
	dgx := s.excludedDB()
	d, err := s.database(ctx, dgx)
	if err != nil || d == nil {
		return false, err
	}
	if err := s.checkPristine(ctx, dgx); err != nil {
		return false, err
	}
	c, err := s.conn(ctx, "")
	if err != nil {
		return false, err
	}
	var fork sql.NullString
	err = c.QueryRowContext(ctx, "SELECT item FROM "+qs(dgx)+".dbo.driftgate_saved WHERE kind = N'complete'").Scan(&fork)
	c.Close()
	switch {
	case errors.Is(err, sql.ErrNoRows), sqlServerNumber(err) == 208: // a save that did not finish, or copied back already
	case err != nil:
		return false, err
	case w.State != "ONLINE":
		return true, nil
	default:
		now, err := s.recoveryFork(ctx, w.ID)
		if err != nil {
			return false, err
		}
		if !strings.EqualFold(now, fork.String) {
			return true, nil
		}
	}
	return false, s.dropLeft(ctx, dgx, false)
}

// recoverExcluded copies back the rows of excluded tables a restore of the
// whole database saved and did not copy back (it stopped).
func (s *SQLServer) recoverExcluded(ctx context.Context, w *sqlServerDatabase) error {
	if w == nil || w.State != "ONLINE" {
		return nil
	}
	left, err := s.leftSave(ctx, w)
	if err != nil || !left {
		return err
	}
	fmt.Fprintf(s.o.Log, "state %s: copying back the rows of excluded tables a restore of %s saved before it stopped\n", s.o.Name, s.o.Working)
	return s.copyBack(ctx)
}

// ssExcludedSequences reads the excluded sequences of the database c is
// connected to: schema, name, current_value, last_used_value ("NULL").
func (s *SQLServer) excludedSequences(ctx context.Context, c *sql.Conn) ([][4]string, error) {
	rows, err := c.QueryContext(ctx, `SELECT SCHEMA_NAME(schema_id), name, CONVERT(nvarchar(40), current_value), ISNULL(CONVERT(nvarchar(40), last_used_value), N'NULL')
		FROM sys.sequences WHERE is_ms_shipped = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][4]string
	for rows.Next() {
		var q [4]string
		if err := rows.Scan(&q[0], &q[1], &q[2], &q[3]); err != nil {
			return nil, err
		}
		if s.excluded(q[0], q[1]) {
			out = append(out, q)
		}
	}
	return out, rows.Err()
}

// ssColumnsText is what a copy of a table's rows needs to stay the same:
// its columns, their types and identity.
func ssColumnsText(t *ssTable) string {
	var b strings.Builder
	for _, c := range t.Columns {
		fmt.Fprintf(&b, "%s %s %d %t %t %t %d;", c.Name, c.Type, c.MaxLength, c.Nullable, c.Identity, c.Computed, c.Generated)
	}
	return b.String()
}

// saveExcluded saves the rows of the excluded tables of the working
// database w, with where their identity columns stand, and the excluded
// sequences, into <pristine>_dgx; false when there is nothing to save. A
// complete save a stopped restore left is used as it is.
func (s *SQLServer) saveExcluded(ctx context.Context, w *sqlServerDatabase) (bool, error) {
	left, err := s.leftSave(ctx, w)
	if err != nil || left || w.State != "ONLINE" {
		return left, err
	}
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return false, err
	}
	defer c.Close()
	inv, err := s.catalog(ctx, c)
	if err != nil {
		return false, err
	}
	seqs, err := s.excludedSequences(ctx, c)
	if err != nil {
		return false, err
	}
	var tables []*ssTable
	for _, t := range inv.Tables {
		if t.Excluded && !t.External {
			tables = append(tables, t)
		}
	}
	if len(tables) == 0 && len(seqs) == 0 {
		return false, nil
	}
	// The rows go back into the tables as the pristine copy has them.
	pc, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return false, err
	}
	pinv, err := s.inventory(ctx, pc)
	pc.Close()
	if err != nil {
		return false, err
	}
	for _, t := range tables {
		if p := pinv.ByItem[t.Item]; p == nil || ssColumnsText(p) != ssColumnsText(t) {
			return false, fmt.Errorf("state %s: excluded table %s.%s is not as in the pristine copy (its columns changed, or it is new), so its rows would be lost by a restore of the whole database; run driftgate seed", s.o.Name, t.Schema, t.Name)
		}
	}
	fork, err := s.recoveryFork(ctx, w.ID)
	if err != nil {
		return false, err
	}
	dgx := s.excludedDB()
	if err := s.checkPristine(ctx, dgx); err != nil {
		return false, err
	}
	mark, err := s.markValue(ctx, "", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	m, err := s.conn(ctx, "")
	if err != nil {
		return false, err
	}
	defer m.Close()
	if _, err := m.ExecContext(ctx, "CREATE DATABASE "+qs(dgx)); err != nil {
		return false, fmt.Errorf("state %s: creating %s for the rows of excluded tables: %w", s.o.Name, dgx, err)
	}
	// Marked first: a restore that stops from here on leaves a database
	// the next one may drop.
	if _, err := m.ExecContext(ctx, "EXEC "+qs(dgx)+".sys.sp_addextendedproperty @name = @p1, @value = @p2", sqlServerMarkName, mark); err != nil {
		return false, err
	}
	manifest := qs(dgx) + ".dbo.driftgate_saved"
	if _, err := m.ExecContext(ctx, "CREATE TABLE "+manifest+" (n int NOT NULL PRIMARY KEY, kind nvarchar(10) NOT NULL, item nvarchar(400) NOT NULL, a nvarchar(40) NULL, b nvarchar(40) NULL)"); err != nil {
		return false, err
	}
	add := func(n int, kind, item string, a, b any) error {
		_, err := m.ExecContext(ctx, "INSERT INTO "+manifest+" VALUES (@p1, @p2, @p3, @p4, @p5)", n, kind, item, a, b)
		return err
	}
	for i, t := range tables {
		var exprs []string
		for _, col := range ssInsertColumns(t) {
			e := qs(col.Name)
			if col.Type == "xml" { // a typed column's schema collection is not in dgx
				e = "CAST(" + e + " AS nvarchar(max)) AS " + e
			}
			exprs = append(exprs, e)
		}
		q := fmt.Sprintf("SELECT %s INTO %s.dbo.%s FROM %s", strings.Join(exprs, ", "), qs(dgx), qs(fmt.Sprintf("t%d", i+1)), t.qname())
		if _, err := c.ExecContext(ctx, q); err != nil {
			return false, fmt.Errorf("state %s: saving the rows of excluded table %s.%s: %w", s.o.Name, t.Schema, t.Name, err)
		}
		var a, b any
		if t.Identity != nil {
			a, b = t.Identity.Last.String, t.Identity.Current
			if !t.Identity.Last.Valid {
				a = "NULL"
			}
		}
		if err := add(i+1, "table", t.Item, a, b); err != nil {
			return false, err
		}
	}
	for i, q := range seqs {
		if err := add(len(tables)+i+1, "sequence", "sequence:"+q[0]+"."+q[1], q[2], q[3]); err != nil {
			return false, err
		}
	}
	return true, add(0, "complete", fork, nil, nil)
}

// copyBack copies the saved rows of excluded tables and the saved
// sequences into the working database, as they were before the restore,
// and drops <pristine>_dgx. Triggers do not fire and cascades do not run;
// a foreign key of an excluded table whose rows now refer to rows the
// restore removed stays on but no longer trusted.
func (s *SQLServer) copyBack(ctx context.Context) (err error) {
	dgx := s.excludedDB()
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if done {
			c.Close()
		} else {
			discard(c)
		}
	}()
	type entry struct {
		n          int
		kind, item string
		a, b       sql.NullString
		rows       bool
	}
	var list []*entry
	rows, err := c.QueryContext(ctx, `SELECT m.n, m.kind, m.item, m.a, m.b,
		CASE WHEN EXISTS (SELECT 1 FROM `+qs(dgx)+`.sys.partitions p WHERE p.object_id = OBJECT_ID(N'`+strings.ReplaceAll(qs(dgx), "'", "''")+`.dbo.t' + CAST(m.n AS nvarchar(10))) AND p.index_id IN (0, 1) AND p.rows > 0) THEN 1 ELSE 0 END
		FROM `+qs(dgx)+`.dbo.driftgate_saved m WHERE m.kind <> N'complete' ORDER BY m.n`)
	if err != nil {
		return err
	}
	for rows.Next() {
		e := &entry{}
		if err := rows.Scan(&e.n, &e.kind, &e.item, &e.a, &e.b, &e.rows); err != nil {
			rows.Close()
			return err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	inv, err := s.catalog(ctx, c)
	if err != nil {
		return err
	}
	facts, err := s.restoreFacts(ctx, c, inv)
	if err != nil {
		return err
	}
	set := map[int64]bool{}
	var body strings.Builder
	seqs := map[string]*ssSequence{}
	for _, q := range inv.Sequences {
		seqs[q.Item] = q
	}
	for _, e := range list {
		if e.kind == "sequence" {
			schema, name, _ := strings.Cut(strings.TrimPrefix(e.item, "sequence:"), ".")
			want := &ssSequence{Current: e.a.String, LastUsed: sql.NullString{String: e.b.String, Valid: e.b.String != "NULL"}}
			stmt, ok := ssRestartSQL(qs2(schema, name), want, e.n)
			if !ok {
				return fmt.Errorf("state %s: the saved value of %s cannot be read", s.o.Name, e.item)
			}
			body.WriteString(stmt)
			continue
		}
		t := inv.ByItem[e.item]
		if t == nil {
			fmt.Fprintf(s.o.Log, "state %s: excluded table %s is gone after the restore; its saved rows are not copied back\n", s.o.Name, e.item)
			continue
		}
		set[t.ID] = true
		src := qs(dgx) + ".dbo." + qs(fmt.Sprintf("t%d", e.n))
		cols, err := ssSavedColumns(ctx, c, dgx, e.n)
		if err != nil {
			return err
		}
		clear, reseed := "DELETE", ""
		if t.Identity != nil {
			target := &ssIdentity{Seed: t.Identity.Seed, Last: sql.NullString{String: e.a.String, Valid: e.a.String != "NULL"}, Current: e.b.String}
			var ok bool
			clear, reseed, ok = ssIdentityPlan(target, t.Identity, !e.rows, facts.truncatable(t.ID))
			if !ok {
				clear, reseed = "DELETE", ""
				fmt.Fprintf(s.o.Log, "state %s: the identity of excluded table %s cannot be set back to where it stood\n", s.o.Name, e.item)
			}
		}
		if t.MemoryOptimized && clear == "TRUNCATE" {
			clear = "DELETE"
		}
		fmt.Fprintf(&body, "%s;\n", ssClearSQL(clear, t))
		list := strings.Join(cols, ", ")
		if t.Identity != nil {
			fmt.Fprintf(&body, "SET IDENTITY_INSERT %s ON;\n", t.qname())
		}
		fmt.Fprintf(&body, "INSERT INTO %s (%s) SELECT %s FROM %s;\n", t.qname(), list, list, src)
		if t.Identity != nil {
			fmt.Fprintf(&body, "SET IDENTITY_INSERT %s OFF;\n", t.qname())
			if reseed != "" {
				if !ssInteger.MatchString(reseed) {
					return fmt.Errorf("state %s: the saved identity value of %s cannot be read", s.o.Name, e.item)
				}
				fmt.Fprintf(&body, "DBCC CHECKIDENT (%s, RESEED, %s) WITH NO_INFOMSGS;\n", sqlServerString(t.qname()), reseed)
			}
		}
	}
	fmt.Fprintf(&body, "DELETE FROM %s.dbo.driftgate_saved WHERE kind = N'complete';\n", qs(dgx))
	g := ssGuardFor(inv, set, func(t *ssTable) bool { return t.Excluded })
	if _, err := c.ExecContext(ctx, "BEGIN TRANSACTION;\n"+g.offSQL()+body.String()); err != nil {
		return fmt.Errorf("state %s: copying back the rows of excluded tables: %w", s.o.Name, err)
	}
	on, err := s.tolerate(ctx, c, g)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, on+"COMMIT"); err != nil {
		return fmt.Errorf("state %s: copying back the rows of excluded tables: %w", s.o.Name, err)
	}
	done = true
	return s.dropLeft(ctx, dgx, false)
}

// ssSavedColumns lists the columns of a saved table, quoted.
func ssSavedColumns(ctx context.Context, c *sql.Conn, dgx string, n int) ([]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT name FROM "+qs(dgx)+".sys.columns WHERE object_id = OBJECT_ID(@p1) ORDER BY column_id",
		qs(dgx)+".dbo."+qs(fmt.Sprintf("t%d", n)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, qs(name))
	}
	if len(out) == 0 && rows.Err() == nil {
		return nil, fmt.Errorf("the saved table t%d of %s is missing", n, dgx)
	}
	return out, rows.Err()
}

// sameFamily reports whether a database is a restored copy of the working
// database (the same family GUID); it can be read while it is restoring.
func (s *SQLServer) sameFamily(ctx context.Context, db string) bool {
	c, err := s.conn(ctx, "")
	if err != nil {
		return false
	}
	defer c.Close()
	var rows, families int
	err = c.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT family_guid) FROM sys.database_recovery_status
		WHERE database_id IN (DB_ID(@p1), DB_ID(@p2)) AND family_guid IS NOT NULL`, db, s.o.Working).Scan(&rows, &families)
	return err == nil && rows == 2 && families == 1
}

// dropLeft drops a database DriftGate made next to the pristine copy and
// left behind (<pristine>_dgnew, <pristine>_dgx) when checkPristine allows
// it or, with family, when it is a copy of the working database not yet
// marked or still being restored: a seed that stopped between its RESTORE
// and its mark leaves one.
func (s *SQLServer) dropLeft(ctx context.Context, db string, family bool) error {
	d, err := s.database(ctx, db)
	if err != nil || d == nil {
		return err
	}
	if err := s.checkPristine(ctx, db); err != nil && (!family || !s.sameFamily(ctx, db)) {
		return err
	}
	s.closePool(db)
	m, err := s.conn(ctx, "")
	if err != nil {
		return err
	}
	defer m.Close()
	q := "DROP DATABASE " + qs(db)
	if d.State == "ONLINE" {
		q = "ALTER DATABASE " + qs(db) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE; " + q
	}
	if _, err := m.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("state %s: dropping %s: %w", s.o.Name, db, err)
	}
	return nil
}
