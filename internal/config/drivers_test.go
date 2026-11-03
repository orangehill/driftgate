package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSQLiteAndSQLServerValidation: sqlite and sqlserver (also mssql) are
// drivers; SQLite's working and pristine are files inside the project, its
// DSN is optional and its pristine copy has no default; SQL Server's
// pristine must differ from working also in letter case.
func TestSQLiteAndSQLServerValidation(t *testing.T) {
	base := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values"}
	cases := []struct {
		comp     Component
		err      string
		driver   string // normalized
		pristine string // after the defaults
	}{
		{comp: Component{Driver: "sqlite", Working: "database/database.sqlite"}, driver: SQLite},
		{comp: Component{Driver: "sqlite", DSN: Secret{Dotenv: ".env"}, Working: "db.sqlite3", Pristine: "var/pristine.sqlite3"}, driver: SQLite, pristine: "var/pristine.sqlite3"},
		{comp: Component{Driver: "sqlite", Working: `storage\development.sqlite3`}, driver: SQLite},
		{comp: Component{Driver: "sqlite"}, err: "needs working"},
		{comp: Component{Driver: "sqlite", Working: "/srv/app/db.sqlite"}, err: "relative to the project root"},
		{comp: Component{Driver: "sqlite", Working: "../other/db.sqlite"}, err: "inside the project"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Pristine: "/tmp/p.sqlite"}, err: "pristine: "},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Pristine: "./db.sqlite"}, err: "another file than working"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Pristine: "DB.sqlite"}, err: "another file than working"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Pristine: "db.sqlite-wal"}, err: "-wal file"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Pristine: "db.sqlite-journal"}, err: "-journal file"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", AllowRemote: true}, err: "allow_remote"},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Restore: []string{"x"}}, err: "without a driver"},
		{comp: Component{Driver: "sqlite", DSN: Secret{Env: "X", Key: "k"}, Working: "db.sqlite"}, err: "dsn: "},
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Exclude: []string{"sess*"}, RowTracking: []string{"sessions"}}, err: "is excluded"},
		{comp: Component{Driver: "sqlserver", DSN: Secret{Env: "X"}, Working: "Shop"}, driver: SQLServer, pristine: "Shop_pristine"},
		{comp: Component{Driver: "mssql", DSN: Secret{Env: "X"}, Working: "Shop", Pristine: "ShopClean"}, driver: SQLServer, pristine: "ShopClean"},
		{comp: Component{Driver: "sqlserver", Working: "Shop"}, err: "needs dsn"},
		{comp: Component{Driver: "sqlserver", DSN: Secret{Env: "X"}}, err: "needs working"},
		{comp: Component{Driver: "sqlserver", DSN: Secret{Env: "X"}, Working: "Shop", Pristine: "shop"}, err: "also in letter case"},
		{comp: Component{Driver: "mssql", DSN: Secret{Env: "X"}, Working: "Shop", Fingerprint: []string{"x"}}, err: "without a driver"},
		{comp: Component{Driver: "SQLite", Working: "db.sqlite"}, err: "unknown driver"},
	}
	for i, c := range cases {
		cfg := base
		cfg.State = map[string]Component{"db": c.comp}
		p, err := New(cfg, t.TempDir())
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("case %d: error %v, want %q", i, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		if got := p.State["db"]; got.Driver != c.driver || got.Pristine != c.pristine {
			t.Errorf("case %d: driver %q, pristine %q; want %q, %q", i, got.Driver, got.Pristine, c.driver, c.pristine)
		}
	}
}

// TestUnknownDriverNamesAll: the error of an unknown driver lists every
// driver.
func TestUnknownDriverNamesAll(t *testing.T) {
	err := Component{Driver: "oracle", DSN: Secret{Env: "X"}, Working: "app"}.validate()
	if err == nil || !strings.Contains(err.Error(), "mysql, postgres, sqlite, sqlserver") {
		t.Errorf("error %v", err)
	}
}

// TestReadDSNSQLite: an SQLite component needs no DSN; one it has is read
// (env and op name no file, so nothing to compare).
func TestReadDSNSQLite(t *testing.T) {
	c := Component{Driver: SQLite, Working: "db.sqlite"}
	if dsn, err := c.ReadDSN(context.Background(), t.TempDir()); err != nil || dsn != "" {
		t.Errorf("no dsn: %q, %v", dsn, err)
	}
	t.Setenv("DG_TEST_SQLITE_DSN", "file:db.sqlite")
	c.DSN = Secret{Env: "DG_TEST_SQLITE_DSN"}
	if dsn, err := c.ReadDSN(context.Background(), t.TempDir()); err != nil || dsn != "file:db.sqlite" {
		t.Errorf("env dsn: %q, %v", dsn, err)
	}
}

// TestCheckNamed: the database a setting names must be working; SQL
// Server's in any letter case, MySQL's and PostgreSQL's exactly, SQLite's
// the same file (by path, or past a link when both exist).
func TestCheckNamed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "database"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "app.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join(root, "database", "app.sqlite"), filepath.Join(root, "link.sqlite")) == nil
	dsn := Secret{Dotenv: ".env"}
	for _, c := range []struct {
		driver, working, named string
		ok                     bool
	}{
		{MySQL, "shop", "", true},
		{MySQL, "shop", "shop", true},
		{MySQL, "shop", "Shop", false},
		{Postgres, "shop", "Shop", false},
		{SQLServer, "Shop", "shop", true},
		{SQLServer, "Shop", "other", false},
		{SQLite, "database/app.sqlite", "database/app.sqlite", true},
		{SQLite, "database/app.sqlite", "./database/../database/app.sqlite", true},
		{SQLite, "database/app.sqlite", filepath.Join(root, "database", "app.sqlite"), true},
		{SQLite, `database\app.sqlite`, "database/app.sqlite", true},
		{SQLite, "database/app.sqlite", "database/other.sqlite", false},
		{SQLite, "database/missing.sqlite", "database/missing.sqlite", true},
		{SQLite, "link.sqlite", "database/app.sqlite", linked},
	} {
		err := Component{Driver: c.driver, DSN: dsn, Working: c.working}.checkNamed(root, c.named)
		if (err == nil) != c.ok {
			t.Errorf("%s %s / %s: %v, want ok %v", c.driver, c.working, c.named, err, c.ok)
		}
		if err != nil && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "set working: "+c.named)) {
			t.Errorf("%s %s / %s: %v", c.driver, c.working, c.named, err)
		}
	}
}
