package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/lazy"
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

// sqlServerCleanup bounds what a restore or seed that failed or was
// stopped does to leave the databases usable: it runs on a context of its
// own, the caller's may be done.
var sqlServerCleanup = 30 * time.Second

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
func (s *SQLServer) restoreDatabase(ctx context.Context, mark *sqlServerMark, w *sqlServerDatabase) (err error) {
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
	restore := "RESTORE DATABASE " + qms(s.o.Working) + " FROM DISK = " + sqlServerString(path) + " WITH REPLACE, RECOVERY, CHECKSUM"
	for _, f := range files {
		physical, ok := own[strings.ToLower(f.Logical)]
		if !ok || len(files) != len(own) {
			return fmt.Errorf("state %s: the files of database %s changed since the seed (%s is not one of them now); run driftgate seed", s.o.Name, s.o.Working, f.Logical)
		}
		restore += ", MOVE " + sqlServerString(f.Logical) + " TO " + sqlServerString(physical)
	}
	online := w.State == "ONLINE"
	broker := w.Broker
	if !online && w.State != "OFFLINE" { // a restoring database's flags are not known: the seed's
		broker = mark.Broker
	}
	if broker {
		restore += ", ENABLE_BROKER"
	}
	saved, err := s.saveExcluded(ctx, w)
	if err != nil {
		return err
	}
	if saved {
		// The application stays out until the rows are copied back
		// (saveExcluded); a restore that fails lets it in again.
		restore += ", RESTRICTED_USER"
		defer func() {
			if err != nil {
				s.letIn()
			}
		}()
		chaos.Point("state.sqlserver.restore.saved")
	}
	s.closePool(s.o.Working)
	if online {
		if info.Sysadmin {
			// Ending the sessions first saves the ~3 s ROLLBACK IMMEDIATE
			// waits for each one otherwise; only sysadmin may KILL.
			s.endSessions(ctx, m, w.ID)
		}
		if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qms(s.o.Working)+" SET OFFLINE WITH ROLLBACK IMMEDIATE"); err != nil {
			return fmt.Errorf("state %s: taking %s offline: %w", s.o.Name, s.o.Working, err)
		}
	}
	chaos.Point("state.sqlserver.restore.offline")
	if _, err := m.ExecContext(ctx, restore); err != nil {
		// A RESTORE that failed before it began leaves the database
		// offline; one that stopped halfway, restoring, and the next
		// restore runs it again.
		cctx, cancel := context.WithTimeout(context.Background(), sqlServerCleanup)
		if d, derr := s.database(cctx, s.o.Working); derr == nil && d != nil && d.State == "OFFLINE" {
			m.ExecContext(cctx, "ALTER DATABASE "+qms(s.o.Working)+" SET ONLINE")
		}
		cancel()
		return fmt.Errorf("state %s: restore of %s from %s: %w (the database is unusable until a restore completes; run the restore again)", s.o.Name, s.o.Working, path, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.restore.restored")
	if w.State == "ONLINE" || w.State == "OFFLINE" {
		if now, err := s.database(ctx, s.o.Working); err == nil && now != nil && now.Recovery != w.Recovery && w.Recovery != "" {
			if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qms(s.o.Working)+" SET RECOVERY "+w.Recovery); err != nil {
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
	rows, err := m.QueryContext(ctx, "SELECT session_id FROM sys.dm_exec_sessions WHERE database_id = @p1 AND session_id <> @@SPID AND is_user_process = 1", id)
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
	d, err := s.database(ctx, s.excludedDB())
	if err != nil {
		return false, err
	}
	return s.leftSaveOf(ctx, w, d)
}

// leftSaveOf is leftSave of d, the <pristine>_dgx as read last (nil: none).
func (s *SQLServer) leftSaveOf(ctx context.Context, w, d *sqlServerDatabase) (bool, error) {
	if d == nil {
		return false, nil
	}
	dgx := s.excludedDB()
	if err := s.checkPristine(ctx, dgx); err != nil {
		return false, err
	}
	c, err := s.conn(ctx, "")
	if err != nil {
		return false, err
	}
	var fork sql.NullString
	err = c.QueryRowContext(ctx, "SELECT item FROM "+qms(dgx)+".dbo.driftgate_saved WHERE kind = N'complete'").Scan(&fork)
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
	// The restore that made it stopped before it let the logins in again
	// (saveExcluded).
	if err := s.letIn(); err != nil {
		return false, err
	}
	return false, s.dropLeft(ctx, dgx, false)
}

// recoverExcluded copies back the rows of excluded tables a restore of the
// whole database saved and did not copy back (it stopped).
func (s *SQLServer) recoverExcluded(ctx context.Context, w *sqlServerDatabase) error {
	if w == nil || w.State != "ONLINE" {
		return nil
	}
	d, err := s.database(ctx, s.excludedDB())
	if err != nil {
		return err
	}
	return s.recoverExcludedOf(ctx, w, d)
}

// recoverExcludedOf is recoverExcluded with d, the <pristine>_dgx as read
// last (nil: none).
func (s *SQLServer) recoverExcludedOf(ctx context.Context, w, d *sqlServerDatabase) error {
	if w == nil || w.State != "ONLINE" {
		return nil
	}
	left, err := s.leftSaveOf(ctx, w, d)
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

// ssTableDefsSQL reads, per table, what a copy of its rows needs to stay
// the same: memory-optimized and durability, and every column with its
// type (an alias type by name), length, precision, scale, collation,
// nullability, identity, computed, sparse and XML schema collection.
const ssTableDefsSQL = `SELECT s.name, t.name, CONCAT(t.is_memory_optimized, N' ', t.durability_desc COLLATE DATABASE_DEFAULT, N' ',
	(SELECT STRING_AGG(CONVERT(nvarchar(max), CONCAT(QUOTENAME(c.name), N' ', TYPE_NAME(c.user_type_id), N' ', c.max_length, N' ', c.precision, N' ', c.scale,
		N' ', c.collation_name COLLATE DATABASE_DEFAULT, N' ', c.is_nullable, c.is_identity, c.is_computed, c.is_sparse, c.is_column_set, c.is_rowguidcol, N' ', c.generated_always_type,
		N' ', ISNULL(x.name COLLATE DATABASE_DEFAULT, N''))), N'; ') WITHIN GROUP (ORDER BY c.column_id)
	   FROM sys.columns c LEFT JOIN sys.xml_schema_collections x ON x.xml_collection_id = c.xml_collection_id AND c.xml_collection_id <> 0
	  WHERE c.object_id = t.object_id))
  FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id WHERE t.is_ms_shipped = 0`

// excludedDefs reads the definitions (ssTableDefsSQL) of the excluded
// tables of the database c is connected to, by item.
func (s *SQLServer) excludedDefs(ctx context.Context, c *sql.Conn) (map[string]string, error) {
	rows, err := c.QueryContext(ctx, ssTableDefsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var schema, name, def string
		if err := rows.Scan(&schema, &name, &def); err != nil {
			return nil, err
		}
		if s.excluded(schema, name) {
			out["table:"+schema+"."+name] = def
		}
	}
	return out, rows.Err()
}

// saveExcluded saves the rows of the excluded tables of the working
// database w, with where their identity columns stand, and the excluded
// sequences, into <pristine>_dgx; false when there is nothing to save. A
// complete save a stopped restore left is used as it is.
//
// From before the rows are read until they are copied back the working
// database is RESTRICTED_USER (keepOut): what the application writes to
// an excluded table meanwhile would be lost or, after the RESTORE, collide
// with the rows copied back. A login that is a member of db_owner,
// dbcreator or sysadmin still gets in: of what it writes in that time,
// rows committed after the rows are read and before the database goes
// offline are lost, and rows written after the RESTORE are kept, except
// those whose primary key a saved row has (copyBack).
func (s *SQLServer) saveExcluded(ctx context.Context, w *sqlServerDatabase) (saved bool, err error) {
	left, err := s.leftSave(ctx, w)
	if err != nil || left || w.State != "ONLINE" {
		return left, err
	}
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return false, err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
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
		switch {
		case !t.Excluded || t.External:
		case t.MemoryOptimized && t.Durability == "SCHEMA_ONLY":
			// Its rows are in no backup and gone with every restart of
			// the server; a restore of the whole database empties it.
			fmt.Fprintf(s.o.Log, "state %s: excluded table %s.%s is memory-optimized with DURABILITY = SCHEMA_ONLY; the restore of the whole database empties it\n", s.o.Name, t.Schema, t.Name)
		case t.MemoryOptimized:
			return false, ssMemoryExcluded(s.o.Name, t)
		default:
			tables = append(tables, t)
		}
	}
	// With tables excluded the save is made even when there is nothing to
	// save: a restore that stops halfway then knows that nothing is lost
	// (notOnline).
	if len(tables) == 0 && len(seqs) == 0 && len(s.o.Exclude) == 0 {
		return false, nil
	}
	// The rows go back into the tables as the pristine copy has them.
	pc, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return false, err
	}
	pdefs, err := s.excludedDefs(ctx, pc)
	pc.Close()
	if err != nil {
		return false, err
	}
	wdefs, err := s.excludedDefs(ctx, c)
	if err != nil {
		return false, err
	}
	for _, t := range tables {
		if p, ok := pdefs[t.Item]; !ok || p != wdefs[t.Item] {
			return false, fmt.Errorf("state %s: excluded table %s.%s is not as in the pristine copy (it is new, or its columns changed), so a restore of the whole database cannot keep its rows; run driftgate seed", s.o.Name, t.Schema, t.Name)
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
	if _, err := m.ExecContext(ctx, "CREATE DATABASE "+qms(dgx)+"; ALTER DATABASE "+qms(dgx)+" SET RECOVERY SIMPLE"); err != nil {
		return false, fmt.Errorf("state %s: creating %s for the rows of excluded tables: %w", s.o.Name, dgx, err)
	}
	// Marked first: a restore that stops from here on leaves a database
	// the next one may drop.
	if _, err := m.ExecContext(ctx, "EXEC "+qms(dgx)+".sys.sp_addextendedproperty @name = @p1, @value = @p2", sqlServerMarkName, mark); err != nil {
		return false, err
	}
	manifest := qms(dgx) + ".dbo.driftgate_saved"
	if _, err := m.ExecContext(ctx, "CREATE TABLE "+manifest+" (n int NOT NULL PRIMARY KEY, kind nvarchar(10) NOT NULL, item nvarchar(400) NOT NULL, a nvarchar(40) NULL, b nvarchar(40) NULL)"); err != nil {
		return false, err
	}
	add := func(n int, kind, item string, a, b any) error {
		_, err := m.ExecContext(ctx, "INSERT INTO "+manifest+" VALUES (@p1, @p2, @p3, @p4, @p5)", n, kind, item, a, b)
		return err
	}
	if len(tables) > 0 || len(seqs) > 0 {
		// With <pristine>_dgx made: a restore that stops from here on
		// lets the logins in again (leftSave, copyBack).
		c.Close()
		c = nil
		defer func() {
			if err != nil {
				s.letIn()
			}
		}()
		if err := s.keepOut(ctx, m, w); err != nil {
			return false, err
		}
		if c, err = s.conn(ctx, s.o.Working); err != nil {
			return false, err
		}
	}
	for i, t := range tables {
		var exprs []string
		for _, col := range ssInsertColumns(t) {
			e := qms(col.Name)
			if col.Type == "xml" { // a typed column's schema collection is not in dgx
				e = "CAST(" + e + " AS nvarchar(max)) AS " + e
			}
			exprs = append(exprs, e)
		}
		saved := qms(dgx) + ".dbo." + qms(fmt.Sprintf("t%d", i+1))
		q := fmt.Sprintf("SELECT %s INTO %s FROM %s", strings.Join(exprs, ", "), saved, t.qname())
		if _, err := c.ExecContext(ctx, q); err != nil {
			return false, fmt.Errorf("state %s: saving the rows of excluded table %s.%s: %w", s.o.Name, t.Schema, t.Name, err)
		}
		var a, b any
		if t.Identity != nil {
			// Where it stands now, after the rows were read: at least as
			// far as every saved row (unless it was reseeded below one).
			var last, current string
			err := c.QueryRowContext(ctx, `SELECT ISNULL(CONVERT(nvarchar(40), CASE WHEN c.last_value IS NULL THEN NULL
					WHEN IDENT_INCR(@p1) > 0 AND x.hi > CAST(c.last_value AS numeric(38, 0)) THEN x.hi
					WHEN IDENT_INCR(@p1) < 0 AND x.lo < CAST(c.last_value AS numeric(38, 0)) THEN x.lo
					ELSE CAST(c.last_value AS numeric(38, 0)) END), N'NULL'),
				CASE WHEN c.last_value IS NULL THEN ISNULL(CONVERT(nvarchar(40), IDENT_CURRENT(@p1)), N'') ELSE N'' END
				FROM sys.identity_columns c
				CROSS JOIN (SELECT MAX(CAST(`+qms(t.Identity.Column)+` AS numeric(38, 0))) AS hi, MIN(CAST(`+qms(t.Identity.Column)+` AS numeric(38, 0))) AS lo FROM `+saved+`) x
				WHERE c.object_id = OBJECT_ID(@p1)`, t.qname()).Scan(&last, &current)
			if err != nil {
				return false, fmt.Errorf("state %s: reading the identity of excluded table %s.%s: %w", s.o.Name, t.Schema, t.Name, err)
			}
			a, b = last, current
		}
		if err := add(i+1, "table", t.Item, a, b); err != nil {
			return false, err
		}
	}
	// The sequences as they stand now, after the rows.
	if len(seqs) > 0 {
		if seqs, err = s.excludedSequences(ctx, c); err != nil {
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

// keepOut keeps every login that is not a member of db_owner, dbcreator
// or sysadmin out of the working database w (RESTRICTED_USER), ending its
// sessions; it ends DriftGate's own first (sysadmin: every other one too),
// which saves the ~3 s ROLLBACK IMMEDIATE waits for each.
func (s *SQLServer) keepOut(ctx context.Context, m *sql.Conn, w *sqlServerDatabase) error {
	if w.UserAccess != "MULTI_USER" {
		return nil // RESTRICTED_USER already, or SINGLE_USER: the user's choice
	}
	s.closePool(s.o.Working)
	if s.info.Sysadmin {
		s.endSessions(ctx, m, w.ID)
	}
	if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qms(s.o.Working)+" SET RESTRICTED_USER WITH ROLLBACK IMMEDIATE"); err != nil {
		return fmt.Errorf("state %s: keeping the application out of %s while the rows of excluded tables are saved: %w", s.o.Name, s.o.Working, err)
	}
	return nil
}

// letIn lets every login into the working database again (MULTI_USER)
// when keepOut or the RESTORE kept them out, on a context of
// its own (the caller's may be done).
func (s *SQLServer) letIn() error {
	ctx, cancel := context.WithTimeout(context.Background(), sqlServerCleanup)
	defer cancel()
	m, err := s.conn(ctx, "")
	if err != nil {
		return err
	}
	defer m.Close()
	_, err = m.ExecContext(ctx, "IF EXISTS (SELECT 1 FROM sys.databases WHERE name = @p1 AND state_desc = N'ONLINE' AND user_access_desc = N'RESTRICTED_USER') ALTER DATABASE "+qms(s.o.Working)+" SET MULTI_USER", s.o.Working)
	if err != nil {
		return fmt.Errorf("state %s: letting the application into %s again (ALTER DATABASE … SET MULTI_USER): %w", s.o.Name, s.o.Working, err)
	}
	return nil
}

// ssMemoryExcluded refuses an excluded memory-optimized table whose rows
// are kept (SCHEMA_AND_DATA): SQL Server does not let a transaction that
// reads it write to another database, so its rows cannot be saved for a
// restore of the whole database.
func ssMemoryExcluded(component string, t *ssTable) error {
	return fmt.Errorf("state %s: excluded table %s.%s is memory-optimized, and the rows of such a table cannot be kept through a restore of the whole database; do not exclude it (or make it DURABILITY = SCHEMA_ONLY)", component, t.Schema, t.Name)
}

// copyBack copies the saved rows of excluded tables and the saved
// sequences into the working database, as they were before the restore,
// lets the logins in again (saveExcluded) and drops <pristine>_dgx.
// Triggers do not fire and cascades do not run; a foreign key of an
// excluded table whose rows now refer to rows the restore removed stays
// on but no longer trusted. Rows a login that got in wrote after the
// RESTORE (the pristine copy has none) are kept, in a table with a
// primary key: a saved row with the key of one of them is not copied
// back, and the identity goes no further back than where it stood.
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
		CASE WHEN EXISTS (SELECT 1 FROM `+qms(dgx)+`.sys.partitions p WHERE p.object_id = OBJECT_ID(N'`+strings.ReplaceAll(qms(dgx), "'", "''")+`.dbo.t' + CAST(m.n AS nvarchar(10))) AND p.index_id IN (0, 1) AND p.rows > 0) THEN 1 ELSE 0 END
		FROM `+qms(dgx)+`.dbo.driftgate_saved m WHERE m.kind <> N'complete' ORDER BY m.n`)
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
		src := qms(dgx) + ".dbo." + qms(fmt.Sprintf("t%d", e.n))
		cols, err := ssSavedColumns(ctx, c, dgx, e.n)
		if err != nil {
			return err
		}
		merge := s.mergeable(ctx, c, t, cols)
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
		list := strings.Join(cols, ", ")
		if merge != "" {
			// The table lock holds until COMMIT: no row comes in between.
			fmt.Fprintf(&body, "IF EXISTS (SELECT 1 FROM %s WITH (TABLOCKX, HOLDLOCK))\nBEGIN\n", t.qname())
			if t.Identity != nil {
				fmt.Fprintf(&body, "SET IDENTITY_INSERT %s ON;\n", t.qname())
			}
			fmt.Fprintf(&body, "INSERT INTO %s (%s) SELECT %s FROM %s s WHERE NOT EXISTS (SELECT 1 FROM %s x WHERE %s);\n",
				t.qname(), list, "s."+strings.Join(cols, ", s."), src, t.qname(), merge)
			if t.Identity != nil {
				fmt.Fprintf(&body, "SET IDENTITY_INSERT %s OFF;\n", t.qname())
				if last := e.a.String; e.a.Valid && last != "NULL" {
					if !ssInteger().MatchString(last) {
						return fmt.Errorf("state %s: the saved identity value of %s cannot be read", s.o.Name, e.item)
					}
					name := sqlServerString(t.qname())
					fmt.Fprintf(&body, "IF (IDENT_INCR(%s) > 0 AND IDENT_CURRENT(%s) < %s) OR (IDENT_INCR(%s) < 0 AND IDENT_CURRENT(%s) > %s)\n  DBCC CHECKIDENT (%s, RESEED, %s) WITH NO_INFOMSGS;\n",
						name, name, last, name, name, last, name, last)
				}
			}
			body.WriteString("END\nELSE\nBEGIN\n")
		}
		fmt.Fprintf(&body, "%s;\n", ssClearSQL(clear, t))
		if t.Identity != nil {
			fmt.Fprintf(&body, "SET IDENTITY_INSERT %s ON;\n", t.qname())
		}
		fmt.Fprintf(&body, "INSERT INTO %s (%s) SELECT %s FROM %s;\n", t.qname(), list, list, src)
		if t.Identity != nil {
			fmt.Fprintf(&body, "SET IDENTITY_INSERT %s OFF;\n", t.qname())
			if reseed != "" {
				if !ssInteger().MatchString(reseed) {
					return fmt.Errorf("state %s: the saved identity value of %s cannot be read", s.o.Name, e.item)
				}
				fmt.Fprintf(&body, "DBCC CHECKIDENT (%s, RESEED, %s) WITH NO_INFOMSGS;\n", sqlServerString(t.qname()), reseed)
			}
		}
		if merge != "" {
			body.WriteString("END;\n")
		}
	}
	fmt.Fprintf(&body, "DELETE FROM %s.dbo.driftgate_saved WHERE kind = N'complete';\n", qms(dgx))
	g := ssGuardFor(inv, set, func(t *ssTable) bool { return t.Excluded }, true)
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
	if err := s.letIn(); err != nil {
		return err
	}
	return s.dropLeft(ctx, dgx, false)
}

// mergeable is, for an excluded table whose rows written after the RESTORE
// copyBack keeps, how a saved row and a row of the table match by primary
// key ("s" the saved row, "x" the table's); "" when none are kept: the
// table has no primary key, one of its columns is not saved (computed),
// it is memory-optimized, or the pristine copy has rows in it (excluded
// after the seed: those rows are not the application's).
func (s *SQLServer) mergeable(ctx context.Context, c *sql.Conn, t *ssTable, cols []string) string {
	if len(t.Key) == 0 || t.MemoryOptimized {
		return ""
	}
	var on []string
	for _, k := range t.Key {
		if !slices.Contains(cols, qms(k)) {
			return ""
		}
		on = append(on, "x."+qms(k)+" = s."+qms(k))
	}
	var rows int
	err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT TOP (1) 1 AS n FROM "+qms(s.o.Pristine)+"."+t.qname()+") p").Scan(&rows)
	if err != nil || rows > 0 {
		return "" // not known: as with rows
	}
	return strings.Join(on, " AND ")
}

// ssSavedColumns lists the columns of a saved table, quoted.
func ssSavedColumns(ctx context.Context, c *sql.Conn, dgx string, n int) ([]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT name FROM "+qms(dgx)+".sys.columns WHERE object_id = OBJECT_ID(@p1) ORDER BY column_id",
		qms(dgx)+".dbo."+qms(fmt.Sprintf("t%d", n)))
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
		out = append(out, qms(name))
	}
	if len(out) == 0 && rows.Err() == nil {
		return nil, fmt.Errorf("the saved table t%d of %s is missing", n, dgx)
	}
	return out, rows.Err()
}

// sameFamily reports whether a database is a copy a seed restored of the
// working database: the same family GUID (it can be read while the copy
// is restoring) or, for one a RESTORE that was stopped left restoring
// without it, owned by this login;
func (s *SQLServer) sameFamily(ctx context.Context, db string) bool {
	c, err := s.conn(ctx, "")
	if err != nil {
		return false
	}
	defer c.Close()
	var state string
	var own bool
	var family, working sql.NullString
	err = c.QueryRowContext(ctx, `SELECT d.state_desc, CAST(CASE WHEN d.owner_sid = SUSER_SID() THEN 1 ELSE 0 END AS bit), CONVERT(nvarchar(36), r.family_guid),
		(SELECT CONVERT(nvarchar(36), family_guid) FROM sys.database_recovery_status WHERE database_id = DB_ID(@p2))
		FROM sys.databases d LEFT JOIN sys.database_recovery_status r ON r.database_id = d.database_id WHERE d.name = @p1`, db, s.o.Working).Scan(&state, &own, &family, &working)
	switch {
	case err != nil:
		return false
	case family.Valid:
		if !working.Valid || !strings.EqualFold(family.String, working.String) {
			return false
		}
	case state != "RESTORING" || !own:
		return false
	}
	// and every file has a name a seed gives (dg_<8 hex>_<file id>).
	files, err := c.QueryContext(ctx, "SELECT physical_name FROM sys.master_files WHERE database_id = DB_ID(@p1)", db)
	if err != nil {
		return false
	}
	defer files.Close()
	n := 0
	for files.Next() {
		var path string
		if files.Scan(&path) != nil || !ssSeedFile().MatchString(path[strings.LastIndexAny(path, `/\`)+1:]) {
			return false
		}
		n++
	}
	return files.Err() == nil && n > 0
}

// ssSeedFile is the name Seed gives a file of the copy it restores.
var ssSeedFile = lazy.Regexp(`^dg_[0-9a-f]{8}_[0-9]+(\.(mdf|ndf|ldf))?$`)

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
	q := "DROP DATABASE " + qms(db)
	if d.State == "ONLINE" {
		q = "ALTER DATABASE " + qms(db) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE; " + q
	}
	if _, err := m.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("state %s: dropping %s: %w", s.o.Name, db, err)
	}
	return nil
}
