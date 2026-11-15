package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
)

// Seed makes the working database the new pristine copy:
//
//  1. BACKUP … COPY_ONLY of the working database (its own backup chain is
//     left as it is) into a slot file (backupSlots);
//  2. RESTORE of it, without REPLACE and with every file moved to a new
//     name, as <pristine>_dgnew;
//  3. the rows of excluded tables deleted there (no trigger fires, no
//     cascade runs);
//  4. BACKUP of <pristine>_dgnew into the same file, which a restore of
//     the whole database restores from (sqlserver_database.go), and the
//     mark written, with that backup's GUID;
//  5. Options.Replacing, the old pristine copy dropped, <pristine>_dgnew
//     renamed to the pristine database (a rename needs it writable and
//     without sessions) and made read-only.
//
// A seed that stops anywhere leaves the old pristine copy, or none, and a
// <pristine>_dgnew the next seed drops; DriftGate's record of the pristine
// fingerprint is removed (Replacing) before the old copy goes. The
// recovery model is the working database's, so a restore of the whole
// database keeps it.
func (s *SQLServer) Seed(ctx context.Context) error {
	return s.retryOthers(ctx, func() error { return s.seed(ctx) })
}

func (s *SQLServer) seed(ctx context.Context) (err error) {
	defer func() { err = s.denied(err) }()
	info, err := s.server(ctx)
	if err != nil {
		return err
	}
	if err := s.checkWorking(ctx); err != nil {
		return err
	}
	w, err := s.database(ctx, s.o.Working)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("state %s: working database %s does not exist", s.o.Name, s.o.Working)
	}
	if w.State != "ONLINE" {
		// A restore that stopped, or the user, left it so; a restore
		// brings it back from the pristine copy.
		return fmt.Errorf("state %s: working database %s is %s; run a capture or a restore first: it brings the database back from the pristine copy", s.o.Name, s.o.Working, w.State)
	}
	newDB := s.o.Pristine + "_dgnew"
	if len([]rune(s.o.Pristine)) > 122 {
		return fmt.Errorf("state %s: the name of pristine database %s is too long: DriftGate makes %s next to it, and a name has at most 128 characters", s.o.Name, s.o.Pristine, newDB)
	}
	// Rows of excluded tables a stopped restore saved go back first.
	if err := s.recoverExcluded(ctx, w); err != nil {
		return err
	}
	if err := s.checkSeedable(ctx); err != nil {
		return err
	}
	if err := s.checkPristine(ctx, s.o.Pristine); err != nil {
		return err
	}
	if err := s.dropLeft(ctx, newDB, true); err != nil {
		return err
	}
	m, err := s.conn(ctx, "")
	if err != nil {
		return err
	}
	defer m.Close()
	paths, err := s.paths(ctx, m)
	if err != nil {
		return err
	}
	slot := s.backupSlots(paths)[0]
	if old, err := s.readMarkIfOnline(ctx); err != nil {
		return err
	} else if old != nil {
		if f, err := s.fallbackFile(ctx, m, old); err == nil && f == slot {
			slot = s.backupSlots(paths)[1]
		}
	}
	options := " WITH COPY_ONLY, INIT, FORMAT, CHECKSUM"
	if !info.Express() {
		options += ", COMPRESSION"
	}
	if _, err := m.ExecContext(ctx, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(slot)+options); err != nil {
		return fmt.Errorf("state %s: backup of %s to %s: %w", s.o.Name, s.o.Working, slot, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.seed.backup")
	files, err := backupFiles(ctx, m, slot)
	if err != nil {
		return fmt.Errorf("state %s: %s: %w", s.o.Name, slot, err)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", strings.ToLower(s.o.Pristine), time.Now().UnixNano())))
	tag := hex.EncodeToString(h[:4])
	restore := "RESTORE DATABASE " + qms(newDB) + " FROM DISK = " + sqlServerString(slot) + " WITH RECOVERY, CHECKSUM"
	for _, f := range files {
		dir, ext := paths.data, ""
		switch f.Type {
		case "D":
			ext = ".ndf"
			if f.ID == 1 {
				ext = ".mdf"
			}
		case "L":
			dir, ext = paths.log, ".ldf"
		}
		restore += ", MOVE " + sqlServerString(f.Logical) + " TO " + sqlServerString(ssJoin(dir, fmt.Sprintf("dg_%s_%d%s", tag, f.ID, ext)))
	}
	// RESTORE without REPLACE overwrites a database of the same family
	// without a word: the name must be free right before it.
	if d, err := s.database(ctx, newDB); err != nil || d != nil {
		if err == nil {
			err = fmt.Errorf("state %s: database %s appeared during the seed", s.o.Name, newDB)
		}
		return err
	}
	// The copy goes again when the seed fails or is stopped (ctx), on a
	// context of its own: a RESTORE stopped halfway leaves it restoring.
	drop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), sqlServerCleanup)
		defer cancel()
		s.dropLeft(ctx, newDB, true)
	}
	if _, err := m.ExecContext(ctx, restore); err != nil {
		drop()
		return fmt.Errorf("state %s: restore of %s as %s: %w", s.o.Name, s.o.Working, newDB, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.seed.restored")
	ok := false
	defer func() {
		if !ok {
			drop()
		}
	}()
	if err := s.leaveOutExcluded(ctx, newDB); err != nil {
		return err
	}
	s.closePool(newDB)
	if _, err := m.ExecContext(ctx, "BACKUP DATABASE "+qms(newDB)+" TO DISK = "+sqlServerString(slot)+options); err != nil {
		return fmt.Errorf("state %s: backup of %s to %s: %w", s.o.Name, newDB, slot, sqlServerFirst(err))
	}
	guid, err := backupSet(ctx, m, slot)
	if err != nil {
		return fmt.Errorf("state %s: %s: %w", s.o.Name, slot, err)
	}
	if err := s.shrinkLog(ctx, m, newDB, w.Recovery); err != nil {
		return err
	}
	if err := s.writeMark(ctx, newDB, guid, w.Broker); err != nil {
		return err
	}
	chaos.Point("state.sqlserver.seed.marked")
	if s.o.Replacing != nil {
		if err := s.o.Replacing(); err != nil {
			return err
		}
	}
	if err := s.dropLeft(ctx, s.o.Pristine, false); err != nil {
		return err
	}
	chaos.Point("state.sqlserver.seed.swap")
	s.closePool(newDB)
	p := qms(s.o.Pristine)
	for _, q := range []string{
		"ALTER DATABASE " + qms(newDB) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"ALTER DATABASE " + qms(newDB) + " MODIFY NAME = " + p,
		"ALTER DATABASE " + p + " SET MULTI_USER",
		"ALTER DATABASE " + p + " SET READ_ONLY WITH ROLLBACK IMMEDIATE",
	} {
		if _, err := m.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("state %s: %s: %w", s.o.Name, q, err)
		}
		// Once renamed the new pristine copy stays, marked; a restore
		// makes it read-only if this stops before.
		ok = ok || strings.Contains(q, "MODIFY NAME")
	}
	s.forgetPristine()
	s.seen = guid
	return nil
}

// shrinkLog shrinks the log of the new copy db once its backup is made
// (the backup keeps the working database's log size, which a restore of
// the whole database gives the working database again): the copy is
// read-only and needs none of it. The recovery model is the working
// database's again after.
//
// A shrink stops at the active part of the log. When that part cannot
// move to the start of the file yet (a transaction of another session or
// a task of the server still holds it), the log stays as large as the end
// of the active part, up to the working database's whole log. The shrink
// then runs again, in the same batch: sqlServerShrinkSQL.
func (s *SQLServer) shrinkLog(ctx context.Context, m *sql.Conn, db, recovery string) error {
	if _, err := m.ExecContext(ctx, "ALTER DATABASE "+qms(db)+" SET RECOVERY SIMPLE"); err != nil {
		return err
	}
	c, err := s.conn(ctx, db)
	if err != nil {
		return err
	}
	var mb int64
	var wait string
	var held bool
	err = c.QueryRowContext(ctx, sqlServerShrinkSQL).Scan(&mb, &wait, &held)
	c.Close()
	s.closePool(db)
	if err != nil {
		return fmt.Errorf("state %s: shrinking the log of %s: %w", s.o.Name, db, err)
	}
	// NOTHING: no one holds the log; its active part is in a later file of
	// a log of several, which only the log's own wrap moves.
	if held && wait != "NOTHING" {
		fmt.Fprintf(s.o.Log, "state %s: the log of the pristine copy stays %d MB: the server still holds part of it (%s); the next seed shrinks it\n", s.o.Name, mb, wait)
	}
	if recovery != "" && recovery != "SIMPLE" {
		_, err = m.ExecContext(ctx, "ALTER DATABASE "+qms(db)+" SET RECOVERY "+recovery)
	}
	return err
}

// sqlServerShrinkSQL shrinks every log file of the current database, up to
// 5 times 200 ms apart, in one round trip. It stops once nothing more can
// be freed: no file has a part of the log in use (sys.dm_db_log_info)
// after its first two parts, as a shrink keeps at least two and a log that
// is not held keeps its active part at the start. Where that view cannot
// be read, it shrinks once, as without a check.
//
// Its one result set (DBCC … WITH NO_INFOMSGS returns none;
// TestSQLServerSeedLog reads it on every version CI runs) is the log's
// size in MB, why the server keeps it (log_reuse_wait_desc; MAX only for
// the SUM, a database has one reason), and held: whether a part in use
// still lies after the first two after the last try. That is the
// threshold of the note rather than a size: how small a clean shrink gets
// depends on the log's layout, not on a number.
const sqlServerShrinkSQL = `SET NOCOUNT ON;
DECLARE @pass int = 0, @done bit = 0, @held bit = 0, @f int;
WHILE @pass < 5 AND @done = 0 BEGIN
  IF @pass > 0 WAITFOR DELAY '00:00:00.200';
  CHECKPOINT;
  SET @f = (SELECT MIN(file_id) FROM sys.database_files WHERE type = 1);
  WHILE @f IS NOT NULL BEGIN
    DBCC SHRINKFILE (@f, 1) WITH NO_INFOMSGS;
    SET @f = (SELECT MIN(file_id) FROM sys.database_files WHERE type = 1 AND file_id > @f);
  END;
  SET @pass += 1;
  BEGIN TRY
    EXEC sp_executesql N'SET @held = CASE WHEN EXISTS (SELECT 1 FROM (SELECT vlf_active,
        ROW_NUMBER() OVER (PARTITION BY file_id ORDER BY vlf_begin_offset) AS n FROM sys.dm_db_log_info(DB_ID())) v
        WHERE v.vlf_active = 1 AND v.n > 2) THEN 1 ELSE 0 END', N'@held bit OUTPUT', @held = @held OUTPUT;
    SET @done = 1 - @held;
  END TRY
  BEGIN CATCH
    SELECT @done = 1, @held = 0;
  END CATCH;
END;
SELECT CAST(SUM(CAST(f.size AS bigint)) * 8 / 1024 AS bigint), MAX(d.log_reuse_wait_desc), @held
FROM sys.database_files f CROSS JOIN sys.databases d WHERE f.type = 1 AND d.database_id = DB_ID()`

// readMarkIfOnline reads the mark of the pristine database, nil when there
// is none or it is not online.
func (s *SQLServer) readMarkIfOnline(ctx context.Context) (*sqlServerMark, error) {
	d, err := s.database(ctx, s.o.Pristine)
	if err != nil || d == nil || d.State != "ONLINE" {
		return nil, err
	}
	return s.readMark(ctx, s.o.Pristine)
}

// checkSeedable refuses a working database whose pristine copy would not
// be its copy: a memory-optimized table with DURABILITY = SCHEMA_ONLY
// that is not excluded (its rows are in no backup), one with
// SCHEMA_AND_DATA that is (ssMemoryExcluded), and a server trigger
// on CREATE, ALTER or DROP DATABASE, which may refuse what a seed and a
// restore do to the databases (only sysadmin can turn it off).
func (s *SQLServer) checkSeedable(ctx context.Context) error {
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return err
	}
	defer c.Close()
	inv, err := s.inventory(ctx, c)
	if err != nil {
		return err
	}
	for _, t := range inv.Tables {
		switch {
		case !t.MemoryOptimized:
		case t.Durability == "SCHEMA_ONLY" && !t.Excluded:
			return fmt.Errorf("state %s: table %s.%s is memory-optimized with DURABILITY = SCHEMA_ONLY: its rows are in no backup, so the pristine copy cannot have them; exclude it", s.o.Name, t.Schema, t.Name)
		case t.Durability != "SCHEMA_ONLY" && t.Excluded:
			return ssMemoryExcluded(s.o.Name, t)
		}
	}
	var name sql.NullString
	err = c.QueryRowContext(ctx, `SELECT TOP (1) t.name FROM sys.server_triggers t JOIN sys.server_trigger_events e ON e.object_id = t.object_id
		WHERE t.is_disabled = 0 AND e.type_desc IN (N'CREATE_DATABASE', N'ALTER_DATABASE', N'DROP_DATABASE')`).Scan(&name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if name.Valid {
		return fmt.Errorf("state %s: server trigger %s fires on CREATE, ALTER or DROP DATABASE and may refuse what a seed and a restore do; disable it (DISABLE TRIGGER %s ON ALL SERVER) while DriftGate runs", s.o.Name, qms(name.String), qms(name.String))
	}
	return nil
}

// leaveOutExcluded deletes the rows of excluded and restore_empty tables in db (a new copy
// of the working database) without firing a trigger or running a cascade.
// A trusted foreign key of a table that is not excluded must still hold,
// or the pristine copy's definition would differ from the working
// database's.
func (s *SQLServer) leaveOutExcluded(ctx context.Context, db string) error {
	c, err := s.conn(ctx, db)
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
	inv, err := s.inventory(ctx, c)
	if err != nil {
		return err
	}
	set := map[int64]bool{}
	for _, t := range inv.Tables {
		if (t.Excluded || t.RestoreEmpty) && !t.External {
			set[t.ID] = true
		}
	}
	if len(set) == 0 {
		done = true
		return nil
	}
	facts, err := s.restoreFacts(ctx, c, inv)
	if err != nil {
		return err
	}
	g := ssGuardFor(inv, set, func(*ssTable) bool { return false }, false)
	var b strings.Builder
	b.WriteString("BEGIN TRANSACTION;\n" + g.offSQL())
	for _, t := range inv.Tables {
		if !set[t.ID] {
			continue
		}
		clear := "DELETE"
		if facts.truncatable(t.ID) && !t.MemoryOptimized {
			clear = "TRUNCATE"
		}
		b.WriteString(ssClearSQL(clear, t) + ";\n")
	}
	b.WriteString(ssStatements(g.checks) + ssStatements(g.triggers) + "COMMIT")
	if _, err := c.ExecContext(ctx, b.String()); err != nil {
		if sqlServerNumber(err) == 547 {
			return fmt.Errorf("state %s: a table that is not excluded refers to rows of an excluded table, which the pristine copy leaves out; exclude that table too, or drop its foreign key: %w", s.o.Name, err)
		}
		return fmt.Errorf("state %s: leaving the rows of excluded tables out of the pristine copy: %w", s.o.Name, err)
	}
	done = true
	return nil
}

// writeMark marks db as the pristine copy of the working database whose
// backup has the GUID fallback (and whether the working database has
// Service Broker on). The database's DDL triggers are off
// meanwhile (sp_addextendedproperty fires them).
func (s *SQLServer) writeMark(ctx context.Context, db, fallback string, broker bool) error {
	value, err := s.markValue(ctx, fallback, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	var m sqlServerMark
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return err
	}
	m.Broker = broker
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mark := string(b)
	c, err := s.conn(ctx, db)
	if err != nil {
		return err
	}
	defer c.Close()
	var off, on []string
	rows, err := c.QueryContext(ctx, "SELECT name FROM sys.triggers WHERE parent_class = 0 AND is_disabled = 0")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		off = append(off, "DISABLE TRIGGER "+qms(name)+" ON DATABASE")
		on = append(on, "ENABLE TRIGGER "+qms(name)+" ON DATABASE")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	q := "BEGIN TRANSACTION;\n" + ssStatements(off) + "EXEC sys.sp_addextendedproperty @name = @p1, @value = @p2;\n" + ssStatements(on) + "COMMIT"
	if _, err := c.ExecContext(ctx, q, sqlServerMarkName, mark); err != nil {
		return fmt.Errorf("state %s: marking %s: %w", s.o.Name, db, err)
	}
	return nil
}
