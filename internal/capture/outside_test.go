package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/discover"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// outsideApp is an sqliteApp with a migrations table (Laravel's), and
// POST /migrate, which runs a migration that adds a column, once: what a
// prepare: of the migrate command does.
func outsideApp(t *testing.T, root string) (*sqliteApp, *httptest.Server) {
	t.Helper()
	app := newSQLiteApp(t, filepath.Join(root, "app.sqlite"), "wal")
	for _, s := range []string{
		"CREATE TABLE migrations (id INTEGER PRIMARY KEY AUTOINCREMENT, migration TEXT NOT NULL, batch INTEGER NOT NULL)",
		"INSERT INTO migrations (migration, batch) VALUES ('create_users_table', 1)",
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)",
	} {
		if _, err := app.db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", app)
	mux.HandleFunc("POST /migrate", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()
		var n int
		app.db.QueryRow("SELECT count(*) FROM migrations WHERE migration = 'add_slug'").Scan(&n)
		if n == 0 {
			for _, s := range migrateSlug {
				if _, err := app.db.Exec(s); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return app, srv
}

// migrateSlug is a migration: a column, and its row in migrations.
var migrateSlug = []string{
	"ALTER TABLE users ADD COLUMN slug TEXT",
	"INSERT INTO migrations (migration, batch) VALUES ('add_slug', 2)",
}

// TestChangedOutside: a capture starts with a restore from the pristine
// copy. A migration run on the working database since the seed (a column,
// a table, an index, a view, rows of the migrations table) would be undone
// by it, and the capture would pass on the old schema: the capture stops
// before the restore, names what changed and how to go on, and leaves the
// working database as it was. Rows the application wrote, and a migration
// rolled back (the same rows, a counter moved), stop nothing.
func TestChangedOutside(t *testing.T) {
	for _, c := range []struct {
		name   string
		change []string
		want   string // "": the capture runs
	}{
		{"rows the application wrote", []string{"INSERT INTO users (email) VALUES ('new@x')", "UPDATE users SET email = 'z@x' WHERE id = 1", "DELETE FROM notes"}, ""},
		{"a migration rolled back", []string{"INSERT INTO migrations (migration, batch) VALUES ('tried', 2)", "DELETE FROM migrations WHERE migration = 'tried'"}, ""},
		{"a migration", migrateSlug, "db/table:migrations (migrations ran), db/table:users (its definition changed)"},
		{"a column alone", []string{"ALTER TABLE users ADD COLUMN slug TEXT"}, "db/table:users (its definition changed)"},
		{"a data migration", []string{"UPDATE users SET email = 'z@x' WHERE id = 1", "INSERT INTO migrations (migration, batch) VALUES ('fix_emails', 2)"}, "db/table:migrations (migrations ran)"},
		{"a table", []string{"CREATE TABLE posts (id INTEGER PRIMARY KEY)"}, "db/table:posts (made since the pristine copy)"},
		{"a dropped table", []string{"DROP TABLE notes"}, "db/table:notes (dropped since the pristine copy)"},
		{"an index", []string{"CREATE INDEX notes_body ON notes (body)"}, "db/table:notes (its definition changed)"},
		{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "db/view:emails (made since the pristine copy)"},
		{"a pragma", []string{"PRAGMA user_version = 7"}, "db/pragma:user_version (its definition changed)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				Target:   config.Target{BaseURL: srv.URL},
				State:    map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}},
				Discover: []discover.Adapter{{Adapter: "laravel", Command: []string{"php", "artisan", "route:list", "--json"}}}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "base"}); err != nil {
				t.Fatal(err)
			}
			base, err := p.Capture("base")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range c.change {
				if _, err := app.db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			before := schemaOf(t, app)
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now", Baseline: base})
			if c.want == "" {
				if err != nil {
					t.Fatalf("capture after %s: %v", c.name, err)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "the working database was changed outside DriftGate since the pristine copy was made: "+c.want+". ") ||
				!strings.Contains(err.Error(), "so nothing was restored") ||
				!strings.Contains(err.Error(), `add prepare: [["php", "artisan", "migrate", "--force"]] in `+configPath(p)+", so every capture") ||
				!strings.Contains(err.Error(), "run driftgate seed to make the pristine copy anew") ||
				!strings.Contains(err.Error(), "driftgate compare <tag> --skip-fingerprint-check, and once its drifts are right, driftgate baselines rebase base --from <tag> --allow-freeze --reason") {
				t.Fatalf("capture after %s: %v", c.name, err)
			}
			if after := schemaOf(t, app); after != before {
				t.Errorf("the capture changed the working database:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

// schemaOf is the schema of the application's database and its migrations.
func schemaOf(t *testing.T, app *sqliteApp) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	var out []string
	for _, q := range []string{"SELECT coalesce(sql, '') FROM sqlite_schema ORDER BY name", "SELECT migration FROM migrations ORDER BY id"} {
		rows, err := app.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		rows.Close()
	}
	return strings.Join(out, "\n")
}

// TestChangedOutsideWithPrepare: with prepare:, the restore and prepare
// run; a prepare that runs the migration again lets the capture go on
// (the capture runs on the new schema), one that does not stops it after
// the restore, and says so. A --read-only capture, which takes no
// prepare:, offers the seed only.
func TestChangedOutsideWithPrepare(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not found")
	}
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	project := func(prepare ...string) *config.Project {
		t.Helper()
		cfg := config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
			Target: config.Target{BaseURL: srv.URL},
			State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}}}
		if len(prepare) > 0 {
			cfg.Prepare = []config.Command{{Run: prepare}}
		}
		p, err := config.New(cfg, root)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctx := context.Background()
	if err := Seed(ctx, project(), SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	migrate := func() {
		t.Helper()
		// A read first: the application's connection then reads the
		// schema a restore changed (SQLite checks a column an ALTER
		// adds against the schema the connection read last).
		if _, err := app.db.Exec("SELECT count(*) FROM sqlite_schema"); err != nil {
			t.Fatal(err)
		}
		for _, s := range migrateSlug {
			if _, err := app.db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	migrate()
	// prepare: runs the migration again after the restore.
	p := project(curl, "-sf", "-X", "POST", srv.URL+"/migrate")
	sum, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "migrated"})
	if err != nil {
		t.Fatalf("capture with a prepare that migrates: %v", err)
	}
	if got := strings.Join(sum.Prepared, ","); got != "db/table:migrations,db/table:users" {
		t.Errorf("prepared: %q", got)
	}
	// The capture's last restore left the old schema; the migration again.
	migrate()
	_, err = Run(ctx, project("true"), sqliteCatalog(), Options{Tag: "other"})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:migrations (migrations ran), db/table:users (its definition changed). The restore at the start of the capture undid that, and prepare did not make it again") ||
		!strings.Contains(err.Error(), "Fix: add the command that runs the migrations to prepare: in ") {
		t.Fatalf("capture with a prepare that does not migrate: %v", err)
	}
	if strings.Contains(schemaOf(t, app), "slug") {
		t.Errorf("the restore left the column")
	}
	migrate()
	_, err = Run(ctx, project(), sqliteCatalog(), Options{Tag: "ro", ReadOnly: true})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "so nothing was restored. Fix (a --read-only capture takes no prepare:): run driftgate seed") ||
		!strings.Contains(err.Error(), "driftgate baselines rebase local --from <tag> --reason") {
		t.Fatalf("read-only capture: %v", err)
	}
	// The throughput probe restores too, before the first baseline.
	_, err = Probe(ctx, project(), sqliteCatalog(), ProbeOptions{Services: []string{p.Canonical("")}, Requests: 2, Max: 2})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:migrations (migrations ran)") {
		t.Fatalf("probe: %v", err)
	}
	// A seed takes the migration into the pristine copy: nothing to stop.
	if err := Seed(ctx, project(), SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, project(), sqliteCatalog(), Options{Tag: "seeded"}); err != nil {
		t.Fatalf("capture after the seed: %v", err)
	}
	set, err := p.Capture("seeded")
	if err != nil {
		t.Fatal(err)
	}
	if meta, err := set.Meta(); err != nil || meta.Unreliable || meta.State == nil {
		t.Fatalf("meta: %+v %v", meta, err)
	}
}

// TestMigrationTable: the tables migration tools record their migrations
// in, in any schema and letter case, and nothing else.
func TestMigrationTable(t *testing.T) {
	for item, want := range map[string]bool{
		"table:migrations": true, "table:public.schema_migrations": true, "table:dbo.__EFMigrationsHistory": true,
		"table:SequelizeMeta": true, "table:flyway_schema_history": true, "table:doctrine_migration_versions": true,
		"table:users": false, "view:migrations": false, "table:migrations_old": false, "sequence:migrations_id_seq": false,
	} {
		if got := migrationTable(item); got != want {
			t.Errorf("%s: %v, want %v", item, got, want)
		}
	}
}

// TestMigrateCommand: the migrate command of the framework discover:
// names, with the PHP command of a Laravel in Docker; none for several
// frameworks with other commands, or none known.
func TestMigrateCommand(t *testing.T) {
	for _, c := range []struct {
		adapters []discover.Adapter
		want     string
	}{
		{[]discover.Adapter{{Adapter: "laravel"}}, `["php", "artisan", "migrate", "--force"]`},
		{[]discover.Adapter{{Adapter: "laravel", PHP: []string{"docker", "compose", "exec", "-T", "php", "php"}}}, `["docker", "compose", "exec", "-T", "php", "php", "artisan", "migrate", "--force"]`},
		{[]discover.Adapter{{Adapter: "rails"}}, `["bin/rails", "db:migrate"]`},
		{[]discover.Adapter{{Adapter: "django"}, {Adapter: "openapi"}}, `["python", "manage.py", "migrate", "--noinput"]`},
		{[]discover.Adapter{{Adapter: "laravel"}, {Adapter: "rails"}}, ""},
		{[]discover.Adapter{{Adapter: "nextjs"}}, ""},
		{nil, ""},
	} {
		if got := migrateCommand(&config.Project{Config: config.Config{Discover: c.adapters}}); got != c.want {
			t.Errorf("%+v: %s, want %s", c.adapters, got, c.want)
		}
	}
}

// TestChangedOutsideOlderRecord: a record of the pristine copy made by an
// earlier release has no definitions, and may be of an older fingerprint
// format; the definitions are read from the copy, and a column is found as
// with a new record. Rows written stop nothing.
func TestChangedOutsideOlderRecord(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	server, err := serverOf(p, p.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := pristinePath(p, "db", server)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &record) != nil || record["definitions"] == nil {
		t.Fatalf("record of the seed: %s %v", raw, err)
	}
	delete(record, "definitions")
	delete(record, "format") // fingerprint format 1: views and triggers hashed another way
	if err := snapshot.WriteJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec("INSERT INTO users (email) VALUES ('new@x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "rows"}); err != nil {
		t.Fatalf("capture after rows were written: %v", err)
	}
	if _, err := app.db.Exec("ALTER TABLE users ADD COLUMN slug TEXT"); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "column"}); !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:users (its definition changed)") {
		t.Fatalf("capture after a column: %v", err)
	}
}

// TestChangedOutsideDatabases: TestChangedOutside on MySQL/MariaDB and
// PostgreSQL, whose counters of rows (AUTO_INCREMENT, a sequence) move
// with a migration rolled back.
func TestChangedOutsideDatabases(t *testing.T) {
	var servers []string
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers = strings.Split(v, ",")
	}
	if v := os.Getenv("DRIFTGATE_TEST_POSTGRES"); v != "" {
		servers = append(servers, v)
	}
	if len(servers) == 0 {
		t.Skip("DRIFTGATE_TEST_MYSQL and DRIFTGATE_TEST_POSTGRES not set")
	}
	for _, dsn := range servers {
		driver := config.MySQL
		if strings.HasPrefix(dsn, "postgres") {
			driver = config.Postgres
		}
		server, err := dbdsn.Server(driver, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(server, func(t *testing.T) { testChangedOutsideDatabase(t, driver, dsn) })
	}
}

func testChangedOutsideDatabase(t *testing.T, driver, dsn string) {
	ctx := context.Background()
	t.Setenv(config.EnvStore, t.TempDir())
	t.Setenv("DG_OUTSIDE_DSN", dsn)
	work, pristine := "dgt_outside", "dgt_outside_pristine"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	t.Cleanup(srv.Close)
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State: map[string]config.Component{"db": {Driver: driver, DSN: config.Secret{Env: "DG_OUTSIDE_DSN"}, Working: work, Pristine: pristine,
			RestoreEmpty: []string{"cache"}, RestoreOnly: []string{"tokens"}}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	open, suffix := dbx.OpenMySQL, ""
	create := []string{
		"CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(50))",
		"CREATE TABLE migrations (id INT AUTO_INCREMENT PRIMARY KEY, migration VARCHAR(50))",
		"CREATE TABLE cache (k VARCHAR(50) PRIMARY KEY, v TEXT)",
		"CREATE TABLE tokens (id INT AUTO_INCREMENT PRIMARY KEY, token VARCHAR(50))",
	}
	column := "ALTER TABLE users ADD COLUMN slug VARCHAR(50)"
	if driver == config.Postgres {
		open, suffix = dbx.OpenPostgres, " WITH (FORCE)"
		create = []string{"CREATE TABLE users (id serial PRIMARY KEY, email text)", "CREATE TABLE migrations (id serial PRIMARY KEY, migration text)",
			"CREATE TABLE cache (k text PRIMARY KEY, v text)", "CREATE TABLE tokens (id serial PRIMARY KEY, token text)", "CREATE SEQUENCE numbers"}
		column = "ALTER TABLE users ADD COLUMN slug text"
	}
	admin, err := open(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	dropAll := func() {
		for _, db := range []string{work, pristine} {
			if _, err := admin.Exec("DROP DATABASE IF EXISTS " + db + suffix); err != nil {
				t.Error(err)
			}
		}
	}
	dropAll()
	t.Cleanup(dropAll)
	if _, err := admin.Exec("CREATE DATABASE " + work); err != nil {
		t.Fatal(err)
	}
	exec := func(stmts ...string) {
		t.Helper()
		app, err := open(dsn, work, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		for _, s := range stmts {
			if _, err := app.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec(append(create, "INSERT INTO users (email) VALUES ('a@x')", "INSERT INTO migrations (migration) VALUES ('create_users')")...)
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}}}
	exec("INSERT INTO users (email) VALUES ('b@x')", "INSERT INTO migrations (migration) VALUES ('tried')", "DELETE FROM migrations WHERE migration = 'tried'",
		"INSERT INTO cache (k, v) VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')")
	var version string
	admin.QueryRow("SELECT VERSION()").Scan(&version)
	sequences := driver == config.Postgres || strings.Contains(version, "MariaDB")
	if sequences && driver != config.Postgres {
		exec("CREATE SEQUENCE numbers")
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	next := "SELECT NEXT VALUE FOR numbers"
	if driver == config.Postgres {
		next = "SELECT nextval('numbers')"
	}
	if sequences {
		exec(next) // a sequence that moved: rows
		if _, err := Run(ctx, p, cat, Options{Tag: "moved"}); err != nil {
			t.Fatalf("capture after a sequence moved: %v", err)
		}
		exec(next) // moved again, after a restore of it
	}
	if _, err := Run(ctx, p, cat, Options{Tag: "rows"}); err != nil {
		t.Fatalf("capture after rows and a migration rolled back: %v", err)
	}
	exec(column, "INSERT INTO migrations (migration) VALUES ('add_slug')")
	_, err = Run(ctx, p, cat, Options{Tag: "migrated"})
	items := "db/table:migrations (migrations ran), db/table:users (its definition changed)"
	if driver == config.Postgres {
		items = "db/table:public.migrations (migrations ran), db/table:public.users (its definition changed)"
	}
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), items+". ") {
		t.Fatalf("capture after a migration: %v", err)
	}
	exec("UPDATE users SET slug = 'kept'") // the column is still there
	exec("ALTER TABLE users DROP COLUMN slug", "DELETE FROM migrations WHERE migration = 'add_slug'")

	// restore_empty and restore_only tables are never fingerprinted; a
	// change of their definition stops the capture all the same.
	for table, want := range map[string]string{"cache": "db/table:cache", "tokens": "db/table:tokens"} {
		if driver == config.Postgres {
			want = strings.Replace(want, "table:", "table:public.", 1)
		}
		exec("ALTER TABLE " + table + " ADD COLUMN extra INT")
		_, err = Run(ctx, p, cat, Options{Tag: "ro-" + table})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "since the pristine copy was made: "+want+" (its definition changed). ") {
			t.Fatalf("capture after a column of %s: %v", table, err)
		}
		exec("ALTER TABLE " + table + " DROP COLUMN extra")
	}
	if sequences {
		item := "db/sequence:numbers"
		if driver == config.Postgres {
			item = "db/sequence:public.numbers"
		}
		exec("ALTER SEQUENCE numbers INCREMENT BY 5")
		_, err = Run(ctx, p, cat, Options{Tag: "sequence"})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), item+" (its definition changed)") {
			t.Fatalf("capture after ALTER SEQUENCE: %v", err)
		}
		exec("ALTER SEQUENCE numbers INCREMENT BY 1")
	}
	if _, err := Run(ctx, p, cat, Options{Tag: "back"}); err != nil {
		t.Fatalf("capture with the definitions back: %v", err)
	}
}

// TestChangedOutsideRestoreOnly: restore_only and restore_empty tables
// (Laravel's cache, as setup proposes it) are restored at every restore
// without a fingerprint: a column added to one stops the capture too,
// rows written to them never do, nor do they with a record of the seed
// made before their definitions were recorded.
func TestChangedOutsideRestoreOnly(t *testing.T) {
	for _, c := range []struct {
		name   string
		change []string
		want   string
	}{
		{"rows", []string{"INSERT INTO cache (key, value) VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')"}, ""},
		{"restore_empty column", []string{"ALTER TABLE cache ADD COLUMN extra INT"}, "db/table:cache (its definition changed)"},
		{"restore_only column", []string{"ALTER TABLE tokens ADD COLUMN extra INT"}, "db/table:tokens (its definition changed)"},
		{"restore_only index", []string{"CREATE INDEX tokens_token ON tokens (token)"}, "db/table:tokens (its definition changed)"},
		{"restore_only dropped", []string{"DROP TABLE tokens"}, "db/table:tokens (dropped since the pristine copy)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			for _, s := range []string{"CREATE TABLE cache (key TEXT PRIMARY KEY, value TEXT)", "CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT)"} {
				if _, err := app.db.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
				Target: config.Target{BaseURL: srv.URL},
				State: map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"},
					RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}}}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "base"}); err != nil {
				t.Fatal(err)
			}
			for _, s := range c.change {
				if _, err := app.db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.want == "" {
				if err != nil {
					t.Fatalf("capture after %s: %v", c.name, err)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "since the pristine copy was made: "+c.want+". ") {
				t.Fatalf("capture after %s: %v", c.name, err)
			}
		})
	}
}

// TestMigrationsTableOfTheApplication: in a project whose framework keeps
// its migrations in another table (Rails), an application table named
// migrations is rows like any other; where it is the framework's own
// (Laravel), the message says to exclude or rename it if it is the
// application's.
func TestMigrationsTableOfTheApplication(t *testing.T) {
	for _, c := range []struct {
		adapter, want string
	}{
		{"rails", ""},
		{"laravel", `"…". When db/table:migrations is a table of the application, not of a migration tool, exclude it or rename it`},
	} {
		t.Run(c.adapter, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
				Target:   config.Target{BaseURL: srv.URL},
				State:    map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}},
				Discover: []discover.Adapter{{Adapter: c.adapter, Command: []string{"true"}}}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.Exec("INSERT INTO migrations (migration, batch) VALUES ('a row of the application', 9)"); err != nil {
				t.Fatal(err)
			}
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("%s: %v", c.adapter, err)
			}
		})
	}
}

// TestChangedOutsideMigrateInPrepare: when prepare: runs the migrate
// command already and still does not make the change again (a column from
// another branch, or added by hand), the message does not say to add it to
// prepare:; it says no migration of this checkout makes the change.
func TestChangedOutsideMigrateInPrepare(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	// php: true makes the migrate command ["true", "artisan", "migrate",
	// "--force"], which runs and changes nothing.
	migrate := []string{"true", "artisan", "migrate", "--force"}
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target:   config.Target{BaseURL: srv.URL},
		State:    map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}},
		Prepare:  []config.Command{{Run: migrate}},
		Discover: []discover.Adapter{{Adapter: "laravel", Command: []string{"true"}, PHP: []string{"true"}}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec("ALTER TABLE users ADD COLUMN slug TEXT"); err != nil {
		t.Fatal(err)
	}
	_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "prepare: runs the migrations already, so no migration of this checkout makes it: it comes from another branch, or was made by hand") ||
		strings.Contains(err.Error(), "add prepare:") || !strings.Contains(err.Error(), "run driftgate seed") {
		t.Fatalf("capture: %v", err)
	}
}

// TestChangedOutsideOlderFormat: with a record of an older fingerprint
// format, an item on one side only that is not a table may be one that
// format does not make at all: the capture asks for a seed, rather than
// call it made outside DriftGate.
func TestChangedOutsideOlderFormat(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	server, err := serverOf(p, p.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := pristinePath(p, "db", server)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	delete(record, "format")
	if err := snapshot.WriteJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec("CREATE VIEW emails AS SELECT email FROM users"); err != nil {
		t.Fatal(err)
	}
	_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "re-seed needed: the record of the pristine copy has an older fingerprint format, which may not make these items at all: [view:emails]") ||
		strings.Contains(err.Error(), "changed outside DriftGate") {
		t.Fatalf("capture: %v", err)
	}
}

// TestChangedOutsideRestoreOnlyPrepare: a restore_only or restore_empty
// table dropped or made outside DriftGate (a migration that renames
// password_resets) is in no fingerprint. With prepare: that drops or makes
// it again, the capture goes on; with one that does not, it stops.
func TestChangedOutsideRestoreOnlyPrepare(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not found")
	}
	for _, c := range []struct {
		name, change, prepare string
		stop                  string // "": the capture runs
	}{
		{"dropped, dropped again", "DROP TABLE tokens", "DROP TABLE IF EXISTS tokens", ""},
		{"dropped, not again", "DROP TABLE tokens", "SELECT 1", "db/table:tokens (dropped since the pristine copy)"},
		{"made, made again", "CREATE TABLE cache_new (k TEXT)", "CREATE TABLE IF NOT EXISTS cache_new (k TEXT)", ""},
		{"made, not again", "CREATE TABLE cache_new (k TEXT)", "SELECT 1", "db/table:cache_new (made since the pristine copy)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, _ := outsideApp(t, root)
			mux := http.NewServeMux()
			mux.Handle("/", app)
			mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
				app.mu.Lock()
				defer app.mu.Unlock()
				app.db.Exec("SELECT count(*) FROM sqlite_schema") // read the schema a restore changed
				if _, err := app.db.Exec(r.FormValue("sql")); err != nil {
					http.Error(w, err.Error(), 500)
				}
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			for _, s := range []string{"CREATE TABLE cache (key TEXT PRIMARY KEY, value TEXT)", "CREATE TABLE tokens (id INTEGER PRIMARY KEY, token TEXT)"} {
				if _, err := app.db.Exec(s); err != nil {
					t.Fatal(err)
				}
			}
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
				Target: config.Target{BaseURL: srv.URL},
				State: map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"},
					RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache*"}}},
				Prepare: []config.Command{{Run: []string{curl, "-sf", "-X", "POST", "--data-urlencode", "sql=" + c.prepare, srv.URL + "/exec"}}}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.Exec(c.change); err != nil {
				t.Fatal(err)
			}
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.stop == "" {
				if err != nil {
					t.Fatalf("capture: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), c.stop+". The restore at the start of the capture undid that, and prepare did not make it again") {
				t.Fatalf("capture: %v", err)
			}
		})
	}
}

// TestChangedOutsideAfterStoppedRestore: a restore that stopped halfway
// (killed, or failed) leaves a mark with the items it was restoring. The
// next capture completes those without taking them for changes made
// outside DriftGate; any other change still stops it (a migration run
// after a failed restore is no false pass). A mark of a restore of the
// whole database exempts everything once. A seed removes the mark.
func TestChangedOutsideAfterStoppedRestore(t *testing.T) {
	setup := func(t *testing.T) (*sqliteApp, *config.Project, string) {
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		root := t.TempDir()
		app, srv := outsideApp(t, root)
		p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
			Target: config.Target{BaseURL: srv.URL},
			State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}}}, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := Seed(context.Background(), p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		server, err := serverOf(p, p.State["db"], "")
		if err != nil {
			t.Fatal(err)
		}
		record, err := pristinePath(p, "db", server)
		if err != nil {
			t.Fatal(err)
		}
		return app, p, strings.TrimSuffix(record, ".json") + ".restoring"
	}
	exec := func(t *testing.T, app *sqliteApp, stmts ...string) {
		t.Helper()
		app.db.Exec("SELECT count(*) FROM sqlite_schema") // the schema a restore changed
		for _, s := range stmts {
			if _, err := app.db.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	ctx := context.Background()

	t.Run("killed partway", func(t *testing.T) {
		app, p, mark := setup(t)
		// Killed after it dropped notes and before it made it again.
		exec(t, app, "DROP TABLE notes")
		snapshot.WriteJSON(mark, restoringMark{Items: []string{"table:notes"}})
		var out strings.Builder
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "after", Out: &out}); err != nil {
			t.Fatalf("capture after a killed restore: %v", err)
		}
		if !strings.Contains(out.String(), "state db: the last restore stopped halfway; this restore completes its 1 item") {
			t.Errorf("output:\n%s", out.String())
		}
		if _, err := os.Stat(mark); err == nil {
			t.Errorf("the mark is left after a complete restore")
		}
		exec(t, app, "ALTER TABLE users ADD COLUMN slug TEXT")
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "column"}); !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:users (its definition changed)") {
			t.Fatalf("capture after a column, the restore complete: %v", err)
		}
	})

	t.Run("failed, then a migration", func(t *testing.T) {
		app, p, mark := setup(t)
		// A restore that stopped with an error leaves the mark with its
		// items (written as a restore writes it).
		exec(t, app, "INSERT INTO notes (body) VALUES ('written')")
		snapshot.WriteJSON(mark, restoringMark{Items: []string{"table:notes"}})
		// Then the developer migrates another table.
		exec(t, app, "ALTER TABLE users ADD COLUMN slug TEXT", "INSERT INTO migrations (migration, batch) VALUES ('add_slug', 2)")
		var out strings.Builder
		_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "migrated", Out: &out})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:migrations (migrations ran), db/table:users (its definition changed). ") || strings.Contains(err.Error(), "table:notes") {
			t.Fatalf("capture after a failed restore and a migration: %v\n%s", err, out.String())
		}
		if !strings.Contains(schemaOf(t, app), "slug") {
			t.Errorf("the migration was undone")
		}
	})

	t.Run("finished, its check failed, then a migration", func(t *testing.T) {
		app, p, mark := setup(t)
		// A restore that finished but fails its check (the pristine copy
		// changed, the application wrote meanwhile) leaves no mark.
		exec(t, app, "INSERT INTO notes (body) VALUES ('written')")
		server, _ := serverOf(p, p.State["db"], "")
		record, _ := pristinePath(p, "db", server)
		pristine, err := sql.Open("sqlite", sqliteTestURI(sqliteStorePristine(record), "mode=rw"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pristine.Exec("INSERT INTO notes (body) VALUES ('tampered')"); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "failed"}); err == nil || !strings.Contains(err.Error(), "left 1 items different") {
			t.Fatalf("restore on a changed pristine copy: %v", err)
		}
		if _, err := os.Stat(mark); err == nil {
			t.Fatalf("a restore that finished left its mark")
		}
		pristine.Exec("DELETE FROM notes WHERE body = 'tampered'")
		pristine.Close()
		// Then the developer migrates notes, the table the restore wrote.
		exec(t, app, "ALTER TABLE notes ADD COLUMN title TEXT")
		_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "migrated"})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:notes (its definition changed). ") {
			t.Fatalf("capture after a restore whose check failed and a migration: %v", err)
		}
	})

	t.Run("whole database, unreadable mark, seed", func(t *testing.T) {
		app, p, mark := setup(t)
		exec(t, app, "DROP TABLE notes", "CREATE VIEW emails AS SELECT email FROM users")
		snapshot.WriteJSON(mark, restoringMark{Items: []string{"table:notes"}, Whole: true})
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "whole"}); err != nil {
			t.Fatalf("capture after a stopped restore of the whole database: %v", err)
		}
		// A mark that cannot be read exempts nothing.
		exec(t, app, "DROP TABLE notes")
		os.WriteFile(mark, []byte("not json"), 0o600)
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "unread"}); !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:notes (dropped since the pristine copy)") {
			t.Fatalf("capture with an unreadable mark: %v", err)
		}
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(mark); err == nil {
			t.Errorf("the mark is left after a seed")
		}
	})
}
