package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Dropper is a database component that can remove its pristine copy
// (driftgate clean).
type Dropper interface {
	// DropPristine removes the pristine copy when driftgate seed made it
	// (its mark), never the working database; a copy without the mark is
	// refused and left as it is. It reports whether there was one.
	DropPristine(ctx context.Context) (bool, error)
}

// notSeeded is the refusal of a pristine copy seed did not make.
func notSeeded(name, what, db, why string) error {
	return fmt.Errorf("state %s: %s %s, named as the pristine copy, %s, so it is not removed; if it is this project's copy made by an earlier release, remove it yourself", name, what, db, why)
}

// sameAsWorking refuses a pristine copy named as the working database (the
// configuration refuses it too).
func sameAsWorking(o Options) error {
	if o.Pristine == "" || strings.EqualFold(o.Pristine, o.Working) {
		return fmt.Errorf("state %s: the pristine copy %q is the working database; it is never removed", o.Name, o.Pristine)
	}
	return nil
}

func (m *MySQL) DropPristine(ctx context.Context) (bool, error) {
	if err := sameAsWorking(m.o); err != nil {
		return false, err
	}
	c, err := m.session(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close()
	db := m.o.Pristine
	var exists, marked int
	if err := c.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?),
			(SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND TABLE_TYPE = 'BASE TABLE')`,
		db, db, pristineTable).Scan(&exists, &marked); err != nil {
		return false, err
	}
	switch {
	case exists == 0:
		return false, nil
	case marked == 0:
		return false, notSeeded(m.o.Name, "database", db, "has no "+pristineTable+" table (driftgate seed makes it)")
	}
	// The mark names the working database it is a copy of (since this
	// release): a copy of another project's database of the same name is
	// not dropped, nor one an earlier release made.
	var note sql.NullString
	if err := c.QueryRowContext(ctx, "SELECT note FROM "+qi(db)+"."+qi(pristineTable)+" WHERE id = 1").Scan(&note); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if note.String != mysqlMarkNote(m.o.Working) {
		return false, notSeeded(m.o.Name, "database", db, "is not marked as the copy of "+m.o.Working+" (another project's, or made by an earlier release)")
	}
	_, err = c.ExecContext(ctx, "DROP DATABASE "+qi(db))
	return err == nil, err
}

func (p *Postgres) DropPristine(ctx context.Context) (bool, error) {
	if err := sameAsWorking(p.o); err != nil {
		return false, err
	}
	p.wait()
	db := p.o.Pristine
	var comment sql.NullString
	err := p.admin.QueryRowContext(ctx, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", db).Scan(&comment)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(comment.String, pristineComment+" of "+p.o.Working+",") {
		return false, notSeeded(p.o.Name, "database", db, "is not marked as the copy of "+p.o.Working+" (its comment; another project's, or not made by driftgate seed)")
	}
	_, err = p.admin.ExecContext(ctx, "DROP DATABASE "+qp(db)+" WITH (FORCE)")
	return err == nil, err
}

func (s *SQLServer) DropPristine(ctx context.Context) (bool, error) {
	if err := sameAsWorking(s.o); err != nil {
		return false, err
	}
	db := s.o.Pristine
	d, err := s.database(ctx, db)
	if err != nil || d == nil {
		return false, err
	}
	if d.State != "ONLINE" {
		return false, notSeeded(s.o.Name, "database", db, "is "+d.State+", so DriftGate cannot see whether it made it")
	}
	m, err := s.readMark(ctx, db)
	if err != nil {
		return false, err
	}
	if m == nil {
		return false, notSeeded(s.o.Name, "database", db, "has no mark of driftgate seed")
	}
	if !strings.EqualFold(m.Working, s.o.Working) {
		return false, notSeeded(s.o.Name, "database", db, "is the pristine copy of "+m.Working+", not of "+s.o.Working)
	}
	if err := s.dropLeft(ctx, db, false); err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLite) DropPristine(ctx context.Context) (bool, error) {
	if err := sameAsWorking(s.o); err != nil {
		return false, err
	}
	fi, err := os.Stat(s.o.Pristine)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.IsDir() {
		return false, notSeeded(s.o.Name, "file", s.o.Pristine, "is a folder")
	}
	db, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		return false, err
	}
	var marked int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", pristineTable).Scan(&marked)
	db.Close()
	if err != nil {
		return false, notSeeded(s.o.Name, "file", s.o.Pristine, "cannot be read as an SQLite database ("+err.Error()+")")
	}
	if marked == 0 {
		return false, notSeeded(s.o.Name, "file", s.o.Pristine, "has no "+pristineTable+" table (driftgate seed makes it)")
	}
	// The sidecar files first: one left without its database file would
	// be read as the next copy's.
	for _, suf := range append(append([]string{}, sqliteSidecars...), "") {
		if err := os.Remove(s.o.Pristine + suf); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, removeNew(s.o.Pristine + sqliteNew)
}
