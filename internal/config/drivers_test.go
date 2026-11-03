package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
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
		{comp: Component{Driver: "sqlite", Working: "db.sqlite", Exclude: []string{"sessions", "job_batches", "cache_*"}}, driver: SQLite}, // tables the file may not have
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

// TestReadDSNSQLite: an SQLite component needs no DSN; one of the
// environment must name the working file, and one that names no file is
// refused without showing it.
func TestReadDSNSQLite(t *testing.T) {
	root := t.TempDir()
	c := Component{Driver: SQLite, Working: "db.sqlite"}
	if dsn, err := c.ReadDSN(context.Background(), root); err != nil || dsn != "" {
		t.Errorf("no dsn: %q, %v", dsn, err)
	}
	c.DSN = Secret{Env: "DG_TEST_SQLITE_DSN"}
	for _, v := range []string{"file:db.sqlite", "file:db.sqlite?mode=rw&_pragma=busy_timeout(5000)", "sqlite:///db.sqlite", "./db.sqlite",
		"file://" + filepath.ToSlash(filepath.Join(root, "db.sqlite")), filepath.Join(root, "db.sqlite")} {
		t.Setenv("DG_TEST_SQLITE_DSN", v)
		if dsn, err := c.ReadDSN(context.Background(), root); err != nil || dsn != v {
			t.Errorf("%s: %q, %v", v, dsn, err)
		}
	}
	for _, v := range []string{"file:other.sqlite", "sqlite:////srv/db.sqlite", "mysql://root:s3cr3t@db:3306/", "Data Source=db.sqlite;Password=s3cr3t", ":memory:", "file://remote/db.sqlite"} {
		t.Setenv("DG_TEST_SQLITE_DSN", v)
		_, err := c.ReadDSN(context.Background(), root)
		if err == nil || !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "s3cr3t") || strings.HasPrefix(err.Error(), "dsn:") {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// TestSQLiteDSNAsFound: an SQLite DSN of the environment is read as
// appconf reads the setting (one resolver): Find's file is working, and
// ReadDSN takes it; Prisma's file: URL relative to schema.prisma, Rails'
// sqlite3:, SQLAlchemy's sqlite:///.
func TestSQLiteDSNAsFound(t *testing.T) {
	project := func(prisma bool) string {
		root := t.TempDir()
		files := map[string]string{"prisma/dev.db": "", "storage/dev.sqlite3": "", "dev.db": ""}
		if prisma {
			files["prisma/schema.prisma"] = "datasource db {\n provider = \"sqlite\"\n url = env(\"DATABASE_URL\")\n}\n"
		}
		for name, content := range files {
			os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755)
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	withPrisma, without := project(true), project(false)
	for _, c := range []struct{ root, v, want string }{
		{withPrisma, "file:./dev.db", "prisma/dev.db"},
		{withPrisma, "sqlite:///dev.db", "prisma/dev.db"},
		{without, "file:./dev.db", "dev.db"},
		{without, "sqlite3:storage/dev.sqlite3", "storage/dev.sqlite3"},
		{without, "spatialite:storage/dev.sqlite3", "storage/dev.sqlite3"},
		{without, "file:storage/dev%2Esqlite3?mode=rw", "storage/dev.sqlite3"},
	} {
		t.Setenv("DATABASE_URL", c.v)
		var found []string
		for _, d := range appconf.Find(c.root, os.LookupEnv).Databases {
			if d.Ref.Source != appconf.Env {
				continue
			}
			found = append(found, d.Path)
			comp := Component{Driver: SQLite, Working: d.Path, DSN: SecretFor(d.Ref)}
			if _, err := comp.ReadDSN(context.Background(), c.root); err != nil {
				t.Errorf("%s: ReadDSN of working %s: %v", c.v, d.Path, err)
			}
		}
		if len(found) != 1 || found[0] != c.want {
			t.Errorf("%s: found %q, want %s", c.v, found, c.want)
		}
	}
	// Another variable than schema.prisma's is taken from the root.
	comp := Component{Driver: SQLite, Working: "dev.db", DSN: Secret{Env: "DG_TEST_SQLITE_DSN"}}
	t.Setenv("DG_TEST_SQLITE_DSN", "file:./dev.db")
	if _, err := comp.ReadDSN(context.Background(), withPrisma); err != nil {
		t.Errorf("another variable: %v", err)
	}
}

// TestCheckNamed: the database a setting names must be working; SQL
// Server's in any letter case, MySQL's and PostgreSQL's exactly, SQLite's
// the same file (by path as the system opens it, or past a link when both
// exist): working through link/.. is the file past the link, not beside it.
func TestCheckNamed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "database"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "app.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(filepath.Join(root, "database", "app.sqlite"), filepath.Join(root, "link.sqlite")) == nil
	if err := os.MkdirAll(filepath.Join(root, "database", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	up := os.Symlink(filepath.Join(root, "database", "sub"), filepath.Join(root, "sub")) == nil
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
		{SQLite, "sub/../app.sqlite", "database/app.sqlite", up},
		{SQLite, "sub/../app.sqlite", "app.sqlite", !up},
		{SQLite, "sub/../missing.sqlite", "database/missing.sqlite", up},
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
