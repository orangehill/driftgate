package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
		return fmt.Errorf("state %s: working database %s is %s", s.o.Name, s.o.Working, w.State)
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
	if _, err := m.ExecContext(ctx, "BACKUP DATABASE "+qs(s.o.Working)+" TO DISK = "+sqlServerString(slot)+options); err != nil {
		return fmt.Errorf("state %s: backup of %s to %s: %w", s.o.Name, s.o.Working, slot, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.seed.backup")
	files, err := backupFiles(ctx, m, slot)
	if err != nil {
		return fmt.Errorf("state %s: %s: %w", s.o.Name, slot, err)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", strings.ToLower(s.o.Pristine), time.Now().UnixNano())))
	tag := hex.EncodeToString(h[:4])
	restore := "RESTORE DATABASE " + qs(newDB) + " FROM DISK = " + sqlServerString(slot) + " WITH RECOVERY, CHECKSUM"
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
	if _, err := m.ExecContext(ctx, restore); err != nil {
		s.dropLeft(context.Background(), newDB, true)
		return fmt.Errorf("state %s: restore of %s as %s: %w", s.o.Name, s.o.Working, newDB, sqlServerFirst(err))
	}
	chaos.Point("state.sqlserver.seed.restored")
	ok := false
	defer func() {
		if !ok {
			s.dropLeft(context.Background(), newDB, true)
		}
	}()
	if err := s.leaveOutExcluded(ctx, newDB); err != nil {
		return err
	}
	s.closePool(newDB)
	if _, err := m.ExecContext(ctx, "BACKUP DATABASE "+qs(newDB)+" TO DISK = "+sqlServerString(slot)+options); err != nil {
		return fmt.Errorf("state %s: backup of %s to %s: %w", s.o.Name, newDB, slot, sqlServerFirst(err))
	}
	guid, err := backupSet(ctx, m, slot)
	if err != nil {
		return fmt.Errorf("state %s: %s: %w", s.o.Name, slot, err)
	}
	if err := s.writeMark(ctx, newDB, guid); err != nil {
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
	p := qs(s.o.Pristine)
	for _, q := range []string{
		"ALTER DATABASE " + qs(newDB) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"ALTER DATABASE " + qs(newDB) + " MODIFY NAME = " + p,
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
	return nil
}

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
// that is not excluded (its rows are in no backup), and a server trigger
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
		if t.MemoryOptimized && t.Durability == "SCHEMA_ONLY" && !t.Excluded {
			return fmt.Errorf("state %s: table %s.%s is memory-optimized with DURABILITY = SCHEMA_ONLY: its rows are in no backup, so the pristine copy cannot have them; exclude it", s.o.Name, t.Schema, t.Name)
		}
	}
	var name sql.NullString
	err = c.QueryRowContext(ctx, `SELECT TOP (1) t.name FROM sys.server_triggers t JOIN sys.server_trigger_events e ON e.object_id = t.object_id
		WHERE t.is_disabled = 0 AND e.type_desc IN (N'CREATE_DATABASE', N'ALTER_DATABASE', N'DROP_DATABASE')`).Scan(&name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if name.Valid {
		return fmt.Errorf("state %s: server trigger %s fires on CREATE, ALTER or DROP DATABASE and may refuse what a seed and a restore do; disable it (DISABLE TRIGGER %s ON ALL SERVER) while DriftGate runs", s.o.Name, qs(name.String), qs(name.String))
	}
	return nil
}

// leaveOutExcluded deletes the rows of excluded tables in db (a new copy
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
		if t.Excluded && !t.External {
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
	g := ssGuardFor(inv, set, func(*ssTable) bool { return false })
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
// backup has the GUID fallback. The database's DDL triggers are off
// meanwhile (sp_addextendedproperty fires them).
func (s *SQLServer) writeMark(ctx context.Context, db, fallback string) error {
	mark, err := s.markValue(ctx, fallback, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
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
		off = append(off, "DISABLE TRIGGER "+qs(name)+" ON DATABASE")
		on = append(on, "ENABLE TRIGGER "+qs(name)+" ON DATABASE")
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
