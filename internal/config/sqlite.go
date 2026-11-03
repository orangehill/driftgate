package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
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
	return nil
}

// sqlitePath is a file of the project as configured, with / and cleaned.
func sqlitePath(file string) string {
	return path.Clean(strings.ReplaceAll(file, `\`, "/"))
}

// sameSQLiteFile reports whether the file a setting names (named, as
// appconf reads it: relative to the project root, or absolute) is working:
// the same file when both exist, else the same path.
func sameSQLiteFile(root, named, working string) bool {
	abs := func(file string) string {
		f := filepath.FromSlash(strings.ReplaceAll(file, `\`, "/"))
		if !filepath.IsAbs(f) {
			f = filepath.Join(root, f)
		}
		return filepath.Clean(f)
	}
	a, b := abs(named), abs(working)
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
// meant for another database and hold a password.
func checkSQLiteDSN(root, variable, dsn, working string) error {
	file, err := appconf.SQLiteFile(root, variable, dsn)
	if err != nil {
		return invalid{fmt.Errorf("dsn: %w", err)}
	}
	if !sameSQLiteFile(root, file, working) {
		return invalid{fmt.Errorf("dsn: it names the database file %s, but working is %s: the application runs on %s, and DriftGate would restore %s; set working: %s, or change the DSN", file, working, file, working, file)}
	}
	return nil
}
