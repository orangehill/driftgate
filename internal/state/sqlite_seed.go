package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/orangehill/driftgate/internal/chaos"
)

// Seed copies the working file into a new pristine file. VACUUM INTO
// writes the copy (one moment of the working file, read while a write
// transaction of DriftGate's keeps the application's writes waiting) into
// a new file next to the pristine one; there it gets the mark
// (pristineTable), the excluded tables lose their rows (their triggers do
// not fire) and the journal mode becomes DELETE. Only then is the file
// renamed over the old pristine file, so a seed that stops before leaves
// the old copy as it was. The pristine file replaced must carry the mark,
// be empty, or be adopted (Options.AdoptPristine): any other file named as
// the pristine one is someone's data and is not replaced.
func (s *SQLite) Seed(ctx context.Context) error {
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
	for _, f := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err // left by a seed that stopped
		}
	}
	if err := s.vacuumInto(ctx, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := s.prepare(ctx, tmp); err != nil {
		os.Remove(tmp)
		os.Remove(tmp + "-journal")
		return err
	}
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if s.o.Replacing != nil {
		if err := s.o.Replacing(); err != nil {
			os.Remove(tmp)
			return err
		}
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

// vacuumInto writes the working file's content into a new file. A write
// transaction on another connection is held meanwhile: it waits for the
// application's write under way, and keeps the next one waiting until the
// copy is made.
func (s *SQLite) vacuumInto(ctx context.Context, path string) error {
	lock, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := lock.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer lock.ExecContext(context.Background(), "ROLLBACK")
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "VACUUM main INTO ?", path); err != nil {
		return fmt.Errorf("state %s: copy of %s: %w", s.o.Name, s.o.Working, err)
	}
	return nil
}

// prepare makes the copy at path a pristine file: the mark, the excluded
// tables without rows, the journal mode DELETE (a pristine file is only
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
	if _, err := c.ExecContext(ctx, "PRAGMA journal_mode = DELETE"); err != nil {
		return err
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
	if len(objs.excluded) > 0 {
		// The triggers of excluded tables are no items of objs: read them
		// with every other, so they are dropped and made again around the
		// deletes.
		all := &SQLite{o: Options{Name: s.o.Name}}
		every, err := all.objects(ctx, c, "main")
		if err != nil {
			return err
		}
		excluded := set(objs.excluded)
		var trigs []string
		for _, name := range every.torder {
			if tr := every.triggers[name]; excluded[tr.table] {
				trigs = append(trigs, every.triggers[name].sql)
				if _, err := c.ExecContext(ctx, "DROP TRIGGER main."+qs(name)); err != nil {
					return err
				}
			}
		}
		for _, t := range objs.excluded {
			if _, err := c.ExecContext(ctx, "DELETE FROM main."+qs(t)); err != nil {
				return tableErr(t, every.tables[t], err)
			}
		}
		for _, q := range trigs {
			if _, err := c.ExecContext(ctx, q); err != nil {
				return err
			}
		}
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
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
	if _, err := sqliteFile(s.o.Pristine); err != nil {
		return fmt.Errorf("state %s: file %s, named as the pristine copy, was not made by driftgate seed (it %v), so it is not replaced; delete it yourself or name another pristine file", s.o.Name, s.o.Pristine, err)
	}
	db, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		return err
	}
	defer db.Close()
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
