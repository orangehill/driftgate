package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/chaos"
)

// Seed copies the working file into a new pristine file. VACUUM INTO
// writes the copy (one moment of the working file, read once a write the
// application had under way has ended) into a new file next to the pristine one; there it gets the mark
// (pristineTable), the excluded tables lose their rows (their triggers do
// not fire) and the journal mode becomes DELETE. Only then is the file
// renamed over the old pristine file, so a seed that stops before leaves
// the old copy as it was. The pristine file replaced must carry the mark,
// be empty, or be adopted (Options.AdoptPristine): any other file named as
// the pristine one is someone's data and is not replaced.
func (s *SQLite) Seed(ctx context.Context) (err error) {
	defer s.busy(&err)
	if err := s.current(); err != nil {
		return err
	}
	if err := s.checkPristine(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.o.Pristine), 0o755); err != nil {
		return err
	}
	tmp := s.o.Pristine + sqliteNew
	if err := removeNew(tmp); err != nil {
		return err // left by a seed that stopped
	}
	err = s.vacuumInto(ctx, tmp)
	if err == nil {
		err = s.prepare(ctx, tmp)
	}
	if err == nil {
		err = syncFile(tmp)
	}
	if err == nil && s.o.Replacing != nil {
		err = s.o.Replacing()
	}
	if err != nil {
		removeNew(tmp)
		return err
	}
	chaos.Point("state.sqlite.seed")
	// What SQLite left next to the old pristine file would be read as the
	// new one's: a -wal file's pages, a -journal file rolled back into it.
	for _, suf := range sqliteSidecars {
		if err := os.Remove(s.o.Pristine + suf); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmp, s.o.Pristine); err != nil {
		return err
	}
	syncDir(filepath.Dir(s.o.Pristine))
	s.pristine = nil
	return nil
}

// vacuumInto writes the working file's content into a new file. A
// BEGIN IMMEDIATE first waits for a write the application has under way;
// the copy then reads a moment of its own without holding the write lock,
// so the application's writers are not kept waiting while it is made.
func (s *SQLite) vacuumInto(ctx context.Context, path string) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "ROLLBACK"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "VACUUM main INTO ?", path); err != nil {
		return fmt.Errorf("state %s: copy of %s: %w", s.o.Name, s.o.Working, err)
	}
	return nil
}

// removeNew removes the new file of a seed and what SQLite kept next to it.
func removeNew(path string) error {
	for _, f := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// prepare makes the copy at path a pristine file: the mark, the excluded
// tables without rows (and without their bytes in free pages), the journal mode DELETE (a pristine file is only
// read; a -wal file next to it would be one more file to keep with it).
func (s *SQLite) prepare(ctx context.Context, path string) error {
	db, err := sqliteOpen(path, []string{"mode=rw", "_pragma=foreign_keys(0)", "_pragma=trusted_schema(0)"})
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	// secure_delete overwrites what the deletes free: the rows of excluded
	// tables (sessions, tokens) do not stay in the pristine file's free
	// pages.
	for _, q := range []string{"PRAGMA journal_mode = DELETE", "PRAGMA secure_delete = ON"} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	objs, err := s.objects(ctx, c, "main")
	if err != nil {
		return err
	}
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS " + qs(pristineTable) + " (id INTEGER NOT NULL PRIMARY KEY, note TEXT NOT NULL)",
		"INSERT OR REPLACE INTO " + qs(pristineTable) + " VALUES (1, 'DriftGate pristine copy, made by driftgate seed and replaced by the next one')",
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// The excluded tables and the restore_empty ones lose their rows; a
	// virtual table among them is made again (recreate).
	emptied := append([]string{}, objs.excluded...)
	recreate := maps.Clone(objs.exvirtual)
	for _, name := range objs.order {
		if t := objs.tables[name]; t.restoreOnly && !t.shadow && s.o.restoreEmptyTable(owned(t)) {
			emptied = append(emptied, name)
			if t.virtual {
				recreate[name] = t.sql
			}
		}
	}
	if len(emptied) > 0 {
		// The triggers of the tables emptied are dropped around the
		// deletes, so none fires.
		excluded := map[string]bool{}
		for _, t := range emptied {
			excluded[asciiLower(t)] = true
		}
		var trigs []string
		for _, name := range objs.torder {
			if tr := objs.triggers[name]; excluded[asciiLower(tr.table)] {
				trigs = append(trigs, tr.sql)
				if _, err := c.ExecContext(ctx, "DROP TRIGGER main."+qs(name)); err != nil {
					return err
				}
			}
		}
		// A virtual table is made again instead: a delete from a full-text
		// index only adds delete markers, and its terms stay in the pages
		// of its shadow tables.
		for _, t := range emptied {
			stmts := []string{"DELETE FROM main." + qs(t)}
			if def, ok := recreate[t]; ok {
				stmts = []string{"DROP TABLE main." + qs(t), def}
			}
			for _, q := range stmts {
				if _, err := c.ExecContext(ctx, q); err != nil {
					return tableErr(t, nil, err)
				}
			}
		}
		for _, q := range trigs {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		// ANALYZE's statistics of the excluded tables and their indexes:
		// sqlite_stat4 (and stat3) keep samples of index keys, real values.
		if err := dropStats(ctx, c, append(emptied, objs.exshadow...)); err != nil {
			return err
		}
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// owned is the name of a table, or of a shadow table's virtual table.
func owned(t *sqliteTable) string {
	if t.owner != "" {
		return t.owner
	}
	return t.name
}

// dropStats deletes the statistics ANALYZE kept of the tables (and their
// indexes) from whichever of sqlite_stat1, sqlite_stat3 and sqlite_stat4
// the file has.
func dropStats(ctx context.Context, c *sql.Conn, tables []string) error {
	if len(tables) == 0 {
		return nil
	}
	in := make([]string, len(tables))
	args := make([]any, len(tables))
	for i, t := range tables {
		in[i], args[i] = "?", asciiLower(t)
	}
	for _, stat := range []string{"sqlite_stat1", "sqlite_stat3", "sqlite_stat4"} {
		var n int
		if err := c.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_schema WHERE type = 'table' AND name = ?", stat).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		q := "DELETE FROM main." + stat + " WHERE lower(tbl) IN (" + strings.Join(in, ", ") + ")"
		if _, err := c.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s: %w", stat, err)
		}
	}
	return nil
}

// checkPristine refuses to replace a pristine file that Seed did not make:
// an SQLite file that holds tables, views or triggers and has no
// pristineTable, or a file that is no SQLite database, unless it is adopted.
func (s *SQLite) checkPristine(ctx context.Context) error {
	if s.o.AdoptPristine {
		return nil
	}
	fi, err := os.Stat(s.o.Pristine)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("state %s: the pristine file %s is a folder", s.o.Name, s.o.Pristine)
	}
	if fi.Size() == 0 {
		return nil
	}
	notMade := func(why error) error {
		return fmt.Errorf("state %s: file %s, named as the pristine copy, was not made by driftgate seed (it %v), so it is not replaced; delete it yourself or name another pristine file", s.o.Name, s.o.Pristine, why)
	}
	db, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sqliteCheck(ctx, db); err != nil {
		if errors.Is(err, errNotSQLite) {
			return notMade(err)
		}
		return fmt.Errorf("state %s: pristine file %s: %v", s.o.Name, s.o.Pristine, err)
	}
	var objects, marked int
	if err := db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM sqlite_schema WHERE name <> ? AND name NOT LIKE 'sqlite\_%' ESCAPE '\'),
			(SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?)`, pristineTable, pristineTable).Scan(&objects, &marked); err != nil {
		return fmt.Errorf("state %s: pristine file %s: %v", s.o.Name, s.o.Pristine, err)
	}
	if objects == 0 || marked > 0 {
		return nil
	}
	return fmt.Errorf("state %s: file %s, named as the pristine copy, was not made by driftgate seed and holds %d objects, so it is not replaced; delete it yourself or name another pristine file", s.o.Name, s.o.Pristine, objects)
}

// syncFile hands a file's data to the disk. It opens the file, so it is
// called only once no connection has it open.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename in dir durable. Not every system can sync a
// folder (Windows cannot open one for it): there the rename is left to the
// file system.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}

// SQLiteFromPristine makes the database file working, which must not
// exist, a copy of the pristine file pristine (made by Seed) without the
// pristine mark: the working file of a new checkout, with the data the
// pristine copy's baselines were captured from. The copy is written next
// to it and linked into place, which fails when a file got there
// meanwhile, so a copy that stops leaves no file and nothing is written
// over. Files SQLite keeps next to a database (-wal, -shm, -journal) that
// are there already would be read as the new file's: refused.
func SQLiteFromPristine(ctx context.Context, pristine, working string) error {
	for _, suf := range append([]string{""}, sqliteSidecars...) {
		if _, err := os.Lstat(working + suf); err == nil {
			return fmt.Errorf("%s exists", working+suf)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Read-only, but not query_only: VACUUM INTO writes the new file.
	src, err := sqliteOpen(pristine, []string{"mode=ro", "_pragma=busy_timeout(10000)", "_pragma=trusted_schema(0)"})
	if err != nil {
		return err
	}
	defer src.Close()
	var marked int
	if err := src.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?", pristineTable).Scan(&marked); err != nil {
		return fmt.Errorf("pristine file %s: %w", pristine, err)
	}
	if marked == 0 {
		return fmt.Errorf("%s is no pristine copy made by driftgate seed", pristine)
	}
	if err := os.MkdirAll(filepath.Dir(working), 0o755); err != nil {
		return err
	}
	tmp := working + sqliteNew
	if err := removeNew(tmp); err != nil {
		return err
	}
	err = func() error {
		if _, err := src.ExecContext(ctx, "VACUUM main INTO ?", tmp); err != nil {
			return fmt.Errorf("copy of %s: %w", pristine, err)
		}
		db, err := sqliteOpen(tmp, []string{"mode=rw", "_pragma=trusted_schema(0)"})
		if err != nil {
			return err
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, "DROP TABLE "+qs(pristineTable)); err != nil {
			return err
		}
		if err := db.Close(); err != nil {
			return err
		}
		return syncFile(tmp)
	}()
	if err == nil {
		err = placeNew(tmp, working)
	}
	removeNew(tmp)
	if err != nil {
		return err
	}
	syncDir(filepath.Dir(working))
	return nil
}

// linkFile is os.Link (a test makes it fail as a file system without
// hard links does).
var linkFile = os.Link

// placeNew puts the new file tmp at path, which must not exist: a hard
// link, which fails when a file is there; where the file system has none
// (exFAT, some network and container mounts), an empty file made there
// with O_EXCL, which fails the same way, and the new file renamed over
// it.
func placeNew(tmp, path string) error {
	err := linkFile(tmp, path)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	f, cerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if cerr != nil {
		return cerr
	}
	f.Close()
	if rerr := os.Rename(tmp, path); rerr != nil {
		os.Remove(path)
		return fmt.Errorf("%w (and without a hard link: %v)", err, rerr)
	}
	return nil
}
