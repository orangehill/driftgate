package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/orangehill/driftgate/internal/appconf"
)

// sqliteSidecars are the files SQLite keeps next to a database file: its
// write-ahead log, the index of it, and the rollback journal.
var sqliteSidecars = []string{"-wal", "-shm", "-journal"}

// validateSQLite checks a component of an SQLite database: working is the
// file the application uses, relative to the project root and inside it;
// the pristine copy, when set, is another file inside it, never working or
// one of working's sidecar files. A DSN is optional: when set, the file it
// names must be working (ReadDSN). SQLite has no server, so allow_remote
// has nothing to allow.
func (c Component) validateSQLite() error {
	if !c.DSN.IsZero() {
		if err := c.DSN.validate(); err != nil {
			return fmt.Errorf("dsn: %w", err)
		}
	}
	if c.Working == "" {
		return errors.New("needs working (the database file the application uses, relative to the project root)")
	}
	if err := appconf.CheckFile(c.Working); err != nil {
		return fmt.Errorf("working: %w", err)
	}
	if c.AllowRemote {
		return errors.New("allow_remote is for a database server; an SQLite database is a file of the project")
	}
	if c.Pristine == "" {
		return nil
	}
	if err := appconf.CheckFile(c.Pristine); err != nil {
		return fmt.Errorf("pristine: %w", err)
	}
	// Compared in any letter case: macOS and Windows take App.db and app.db
	// for the same file.
	w, p := sqlitePath(c.Working), sqlitePath(c.Pristine)
	if strings.EqualFold(p, w) {
		return errors.New("pristine must be another file than working (also in letter case)")
	}
	for _, s := range sqliteSidecars {
		if strings.EqualFold(p, w+s) {
			return fmt.Errorf("pristine must not be working's %s file", s)
		}
	}
	// A seed makes and removes files named after the pristine file.
	if len(w) >= len(p) && strings.EqualFold(w[:len(p)], p) {
		return errors.New("working must not be named like pristine or a file a seed makes or removes next to it (pristine and anything after it); name another pristine file")
	}
	return nil
}

// sqlitePath is a file of the project as configured, with / and cleaned.
func sqlitePath(file string) string {
	return path.Clean(strings.ReplaceAll(file, `\`, "/"))
}

// sameSQLiteFile reports whether the file a setting names (named, as
// appconf reads it: relative to the project root, or absolute) is working:
// the same path as the system opens them (appconf.SQLitePath, which resolves
// working as it resolves the setting), or the same file when both exist.
func sameSQLiteFile(root, named, working string) bool {
	a, errA := appconf.SQLitePath(root, named)
	b, errB := appconf.SQLitePath(root, working)
	if errA != nil || errB != nil {
		return false
	}
	if a == b {
		return true
	}
	// Stat only: the file is never opened, as closing a file SQLite has
	// open in this process would drop its locks.
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// checkSQLiteDSN checks an SQLite DSN of the environment or 1Password
// (variable is the environment variable, "" for 1Password): the file it
// names, read as appconf reads a setting (appconf.SQLiteFile), must be the
// one working is (sameSQLiteFile). No error quotes the DSN, which may be
// meant for another database and hold a password; the file it names is
// quoted only when the value has the form of an SQLite setting (a bare
// word read as a file name may be a password: a dsn that names the wrong
// field or variable).
func checkSQLiteDSN(root, variable, dsn, working string) error {
	file, err := appconf.SQLiteFile(root, variable, dsn)
	if err != nil {
		return invalid{err}
	}
	if !sameSQLiteFile(root, file, working) {
		if !sqliteShaped(dsn) {
			return invalid{fmt.Errorf("it names another file than working %s (not shown: the value is not written as an SQLite setting, a file: or sqlite: URL or a .db, .sqlite or .sqlite3 file; check that the dsn names the right variable or field)", working)}
		}
		return invalid{fmt.Errorf("it names the database file %s, but working is %s: the application runs on %s, and DriftGate would restore %s; set working: %s, or change the DSN", file, working, file, working, file)}
	}
	return nil
}

// sqliteShaped reports whether a DSN is written as an SQLite setting: a
// URL of an SQLite scheme, or a file named .db, .sqlite or .sqlite3.
func sqliteShaped(dsn string) bool {
	name := strings.ToLower(dsn)
	if scheme, _, ok := strings.Cut(name, ":"); ok {
		switch scheme {
		case "file", "sqlite", "sqlite3", "spatialite", "jdbc":
			return true
		}
	}
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	for _, ext := range []string{".db", ".sqlite", ".sqlite3"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}
