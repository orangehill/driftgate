package config

import (
	"errors"
	"fmt"
	"net/url"
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

// errSQLiteDSN refuses a DSN that names no SQLite file; the value is not
// shown, as a DSN meant for another database may hold a password.
var errSQLiteDSN = invalid{errors.New("dsn: not the path of an SQLite file, a file: URI or an sqlite: URL (the value is not shown)")}

// checkSQLiteDSN checks an SQLite DSN of the environment or 1Password: it
// must name the file working is (sameSQLiteFile).
func checkSQLiteDSN(root, dsn, working string) error {
	file, ok := sqliteDSNFile(dsn)
	if !ok {
		return errSQLiteDSN
	}
	if !sameSQLiteFile(root, file, working) {
		return invalid{fmt.Errorf("dsn: it names the database file %s, but working is %s: the application runs on %s, and DriftGate would restore %s; set working: %s, or change the DSN", file, working, file, working, file)}
	}
	return nil
}

// sqliteDSNFile is the file an SQLite DSN names, relative to the project
// root or absolute: a path; a file: URI (file:app.db, file:///srv/app.db,
// file://localhost/srv/app.db, with ?query and #fragment, %-encoded); or
// an sqlite: URL as SQLAlchemy writes it (sqlite:///app.db is relative,
// sqlite:////srv/app.db absolute) or sqlite:app.db. ok is false for
// anything else: another URL, a connection string, :memory:, a temporary
// database ("").
func sqliteDSNFile(dsn string) (file string, ok bool) {
	v := strings.TrimSpace(dsn)
	var uri bool
	switch {
	case strings.HasPrefix(v, "file:"):
		v, uri = v[len("file:"):], true
		if rest, ok := strings.CutPrefix(v, "//"); ok {
			host, p, found := strings.Cut(rest, "/")
			if !found || host != "" && !strings.EqualFold(host, "localhost") {
				return "", false
			}
			v = "/" + p
		}
	case strings.HasPrefix(v, "sqlite:"):
		v, uri = v[len("sqlite:"):], true
		if rest, ok := strings.CutPrefix(v, "//"); ok {
			host, p, found := strings.Cut(rest, "/")
			if !found || host != "" {
				return "", false
			}
			v = p // sqlite:///app.db: app.db; sqlite:////srv/app.db: /srv/app.db
		}
	case strings.Contains(v, "://"), strings.ContainsAny(v, "=;"):
		return "", false
	}
	if uri {
		if i := strings.IndexAny(v, "?#"); i >= 0 {
			v = v[:i]
		}
		var err error
		if v, err = url.PathUnescape(v); err != nil {
			return "", false
		}
		// file:///C:/app.db: a Windows drive.
		if len(v) >= 4 && v[0] == '/' && v[2] == ':' && v[3] == '/' && ('a' <= v[1]|0x20 && v[1]|0x20 <= 'z') {
			v = v[1:]
		}
	}
	if v == "" || v == ":memory:" || strings.ContainsRune(v, 0) {
		return "", false
	}
	return v, true
}
