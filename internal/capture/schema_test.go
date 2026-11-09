package capture

import (
	"context"
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
	"github.com/orangehill/driftgate/internal/snapshot"
)

// outsideApp is an sqliteApp with a migrations table (Laravel's), and
// POST /exec, which runs the statements of its form (sql) after a read of
// the schema: what a prepare: of the migrate command does.
func outsideApp(t *testing.T, root string) (*sqliteApp, *httptest.Server) {
	t.Helper()
	app := newSQLiteApp(t, filepath.Join(root, "app.sqlite"), "wal")
	for _, s := range []string{
		"CREATE TABLE migrations (id INTEGER PRIMARY KEY AUTOINCREMENT, migration TEXT NOT NULL, batch INTEGER NOT NULL)",
		"INSERT INTO migrations (migration, batch) VALUES ('create_users_table', 1)",
		"CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE TABLE cache (key TEXT PRIMARY KEY, value TEXT)",
		"CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT)",
	} {
		if _, err := app.db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", app)
	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		defer app.mu.Unlock()
		app.db.Exec("SELECT count(*) FROM sqlite_schema") // the schema a restore changed
		r.ParseForm()
		for _, s := range r.Form["sql"] {
			if _, err := app.db.Exec(s); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return app, srv
}

// exec runs statements on the application's connection.
func (a *sqliteApp) exec(t *testing.T, stmts ...string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.db.Exec("SELECT count(*) FROM sqlite_schema") // the schema a restore changed
	for _, s := range stmts {
		if _, err := a.db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// outsideProject is the project of outsideApp, with prepare: running
// the given statements through POST /exec (none: no prepare:).
func outsideProject(t *testing.T, root, url string, prepare ...string) *config.Project {
	t.Helper()
	cfg := config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target: config.Target{BaseURL: url},
		State: map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"},
			RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}}}}
	if len(prepare) > 0 {
		curl, err := exec.LookPath("curl")
		if err != nil {
			t.Skip("curl not found")
		}
		run := []string{curl, "-sf", "-X", "POST"}
		for _, s := range prepare {
			run = append(run, "--data-urlencode", "sql="+s)
		}
		cfg.Prepare = []config.Command{{Run: append(run, url+"/exec")}}
	}
	p, err := config.New(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// schemaOf is the schema of the application's database.
func schemaOf(t *testing.T, app *sqliteApp) string {
	t.Helper()
	app.mu.Lock()
	defer app.mu.Unlock()
	var out []string
	rows, err := app.db.Query("SELECT coalesce(sql, '') FROM sqlite_schema ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// changedOutside reports whether err is the stop of a schema changed
// outside DriftGate that names items.
func changedOutside(err error, items string) bool {
	return errors.Is(err, ErrEnv) && strings.Contains(err.Error(), "the schema of the working database changed since the pristine copy was made: "+items+";")
}

// TestChangedOutside: a capture starts with a restore from the pristine
// copy. A migration run on the working database since the seed (a column,
// a table, an index, a view, a pragma, on restore_only and restore_empty
// tables too) would be undone by it, and the capture would pass on the old
// schema: the capture stops before the restore, names what changed and how
// to go on, and leaves the working database as it was. Rows the
// application wrote, in any table, and a migration rolled back stop
// nothing.
func TestChangedOutside(t *testing.T) {
	for _, c := range []struct {
		name   string
		change []string
		want   string // "": the capture runs
	}{
		{"rows the application wrote", []string{"INSERT INTO users (email) VALUES ('new@x')", "UPDATE users SET email = 'z@x' WHERE id = 1", "DELETE FROM notes",
			"INSERT INTO migrations (migration, batch) VALUES ('a row', 9)", "INSERT INTO cache VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')"}, ""},
		{"a migration rolled back", []string{"ALTER TABLE users ADD COLUMN slug TEXT", "INSERT INTO migrations (migration, batch) VALUES ('tried', 2)",
			"ALTER TABLE users DROP COLUMN slug", "DELETE FROM migrations WHERE migration = 'tried'"}, ""},
		{"a migration", []string{"ALTER TABLE users ADD COLUMN slug TEXT", "INSERT INTO migrations (migration, batch) VALUES ('add_slug', 2)"}, "db/table:users (changed)"},
		{"a table", []string{"CREATE TABLE posts (id INTEGER PRIMARY KEY)"}, "db/table:posts (new)"},
		{"a dropped table", []string{"DROP TABLE notes"}, "db/table:notes (missing)"},
		{"an index", []string{"CREATE INDEX notes_body ON notes (body)"}, "db/table:notes (changed)"},
		{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "db/view:emails (new)"},
		{"a pragma", []string{"PRAGMA user_version = 7"}, "db/pragma:user_version (changed)"},
		{"a restore_empty column", []string{"ALTER TABLE cache ADD COLUMN extra INT"}, "db/table:cache (changed)"},
		{"a restore_only index", []string{"CREATE INDEX tokens_token ON tokens (token)"}, "db/table:tokens (changed)"},
		{"a restore_only table dropped", []string{"DROP TABLE tokens"}, "db/table:tokens (missing)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			p := outsideProject(t, root, srv.URL)
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "base"}); err != nil {
				t.Fatal(err)
			}
			app.exec(t, c.change...)
			before := schemaOf(t, app)
			_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.want == "" {
				if err != nil {
					t.Fatalf("capture after %s: %v", c.name, err)
				}
				return
			}
			if !changedOutside(err, c.want) || !strings.Contains(err.Error(), "nothing was restored") ||
				!strings.Contains(err.Error(), "add the command that runs the migrations to prepare: in "+configPath(p)) ||
				!strings.Contains(err.Error(), "run driftgate seed") {
				t.Fatalf("capture after %s: %v", c.name, err)
			}
			if after := schemaOf(t, app); after != before {
				t.Errorf("the capture changed the working database:\n%s\nwas\n%s", after, before)
			}
		})
	}
}

// TestChangedOutsideWithPrepare: with prepare:, the restore and prepare
// run; a prepare that makes the change again lets the capture go on (on
// the new schema), one that does not stops it after the restore, and its
// message never says to add prepare:. Of two migrations, the one prepare
// does not make again is named. A restore_only table dropped or made, and
// made so again by prepare, goes on. The throughput probe checks the
// same; a --read-only capture, which takes no prepare:, offers the seed
// only; a seed takes the change into the pristine copy.
func TestChangedOutsideWithPrepare(t *testing.T) {
	const slug, title = "ALTER TABLE users ADD COLUMN slug TEXT", "ALTER TABLE notes ADD COLUMN title TEXT"
	for _, c := range []struct {
		name            string
		change, prepare []string
		stop            string // "": the capture runs
	}{
		{"made again", []string{slug}, []string{slug}, ""},
		{"not made again", []string{slug}, []string{"SELECT 1"}, "db/table:users"},
		{"two migrations, both made again", []string{slug, title}, []string{slug, title}, ""},
		{"two migrations, one made again", []string{slug, title}, []string{slug}, "db/table:notes"},
		{"restore_only dropped, dropped again", []string{"DROP TABLE tokens"}, []string{"DROP TABLE IF EXISTS tokens"}, ""},
		{"restore_only dropped, not again", []string{"DROP TABLE tokens"}, []string{"SELECT 1"}, "db/table:tokens"},
		{"restore_empty made, made again", []string{"CREATE TABLE cache_new (k TEXT)"}, []string{"CREATE TABLE IF NOT EXISTS cache_new (k TEXT)"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			p := outsideProject(t, root, srv.URL, c.prepare...)
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			app.exec(t, c.change...)
			_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.stop == "" {
				if err != nil {
					t.Fatalf("capture: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), ": "+c.stop+"; the restore undid it, and prepare did not make it again") ||
				strings.Contains(err.Error(), "add the command") {
				t.Fatalf("capture: %v", err)
			}
		})
	}

	t.Run("probe, read-only, seed", func(t *testing.T) {
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		root := t.TempDir()
		app, srv := outsideApp(t, root)
		p := outsideProject(t, root, srv.URL)
		ctx := context.Background()
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		app.exec(t, slug)
		_, err := Probe(ctx, outsideProject(t, root, srv.URL, "SELECT 1"), sqliteCatalog(), ProbeOptions{Services: []string{p.Canonical("")}, Requests: 2, Max: 2})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:users; the restore undid it, and prepare did not make it again") {
			t.Fatalf("probe: %v", err)
		}
		app.exec(t, slug)
		_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "ro", ReadOnly: true})
		if !changedOutside(err, "db/table:users (changed)") || !strings.Contains(err.Error(), "Fix (a --read-only capture takes no prepare:): run driftgate seed") {
			t.Fatalf("read-only capture: %v", err)
		}
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "seeded"}); err != nil {
			t.Fatalf("capture after the seed: %v", err)
		}
		if !strings.Contains(schemaOf(t, app), "slug") {
			t.Errorf("the seed did not keep the column")
		}
	})
}

// TestChangedOutsideOlderRecord: a record of the pristine copy of an older
// fingerprint format (no format) stops nothing for rows written, and a
// column is found as with a new record: the schema is read live on both
// sides, never from the record.
func TestChangedOutsideOlderRecord(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p := outsideProject(t, root, srv.URL)
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
	if err != nil || json.Unmarshal(raw, &record) != nil {
		t.Fatalf("record of the seed: %s %v", raw, err)
	}
	delete(record, "format")
	if err := snapshot.WriteJSON(path, record); err != nil {
		t.Fatal(err)
	}
	app.exec(t, "INSERT INTO users (email) VALUES ('new@x')")
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "rows"}); err != nil && strings.Contains(err.Error(), "schema") {
		t.Fatalf("capture after rows were written: %v", err)
	}
	app.exec(t, "ALTER TABLE users ADD COLUMN slug TEXT")
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "column"}); !changedOutside(err, "db/table:users (changed)") {
		t.Fatalf("capture after a column: %v", err)
	}
}

// TestChangedOutsideDatabases: TestChangedOutside on MySQL/MariaDB and
// PostgreSQL, whose counters (AUTO_INCREMENT, a sequence) move with rows
// and with a migration rolled back: those stop nothing, nor does a
// sequence that only moved; a column, also of a restore_only or
// restore_empty table, and ALTER SEQUENCE do.
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
	open, suffix, prefix := dbx.OpenMySQL, "", ""
	create := []string{
		"CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(50))",
		"CREATE TABLE migrations (id INT AUTO_INCREMENT PRIMARY KEY, migration VARCHAR(50))",
		"CREATE TABLE cache (k VARCHAR(50) PRIMARY KEY, v TEXT)",
		"CREATE TABLE tokens (id INT AUTO_INCREMENT PRIMARY KEY, token VARCHAR(50))",
	}
	column := "ALTER TABLE users ADD COLUMN slug VARCHAR(50)"
	if driver == config.Postgres {
		open, suffix, prefix = dbx.OpenPostgres, " WITH (FORCE)", "public."
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
	var version string
	admin.QueryRow("SELECT VERSION()").Scan(&version)
	sequences := driver == config.Postgres || strings.Contains(version, "MariaDB")
	if sequences && driver != config.Postgres {
		create = append(create, "CREATE SEQUENCE numbers")
	}
	exec(append(create, "INSERT INTO users (email) VALUES ('a@x')", "INSERT INTO migrations (migration) VALUES ('create_users')")...)
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}}}
	exec("INSERT INTO users (email) VALUES ('b@x')", "INSERT INTO migrations (migration) VALUES ('tried')", "DELETE FROM migrations WHERE migration = 'tried'",
		"INSERT INTO cache (k, v) VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')")
	if sequences {
		next := "SELECT NEXT VALUE FOR numbers"
		if driver == config.Postgres {
			next = "SELECT nextval('numbers')"
		}
		exec(next, next) // a sequence that moved: rows
	}
	if _, err := Run(ctx, p, cat, Options{Tag: "rows"}); err != nil {
		t.Fatalf("capture after rows, a sequence that moved and a migration rolled back: %v", err)
	}
	exec(column, "INSERT INTO migrations (migration) VALUES ('add_slug')")
	if _, err = Run(ctx, p, cat, Options{Tag: "migrated"}); !changedOutside(err, "db/table:"+prefix+"users (changed)") {
		t.Fatalf("capture after a migration: %v", err)
	}
	exec("UPDATE users SET slug = 'kept'") // the column is still there
	exec("ALTER TABLE users DROP COLUMN slug")
	for _, table := range []string{"cache", "tokens"} {
		exec("ALTER TABLE " + table + " ADD COLUMN extra INT")
		if _, err = Run(ctx, p, cat, Options{Tag: "ro-" + table}); !changedOutside(err, "db/table:"+prefix+table+" (changed)") {
			t.Fatalf("capture after a column of %s: %v", table, err)
		}
		exec("ALTER TABLE " + table + " DROP COLUMN extra")
	}
	if sequences {
		exec("ALTER SEQUENCE numbers INCREMENT BY 5")
		if _, err = Run(ctx, p, cat, Options{Tag: "sequence"}); !changedOutside(err, "db/sequence:"+prefix+"numbers (changed)") {
			t.Fatalf("capture after ALTER SEQUENCE: %v", err)
		}
		exec("ALTER SEQUENCE numbers INCREMENT BY 1")
	}
	if _, err := Run(ctx, p, cat, Options{Tag: "back"}); err != nil {
		t.Fatalf("capture with the definitions back: %v", err)
	}
}

// TestChangedOutsideMissingOnly: triggers and sequences the working
// database lacks are what a restore stopped while it made them again
// leaves: the message says to make them again from the pristine copy,
// never to seed, which would make their absence pristine.
func TestChangedOutsideMissingOnly(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	app.exec(t, "CREATE TRIGGER notes_log AFTER INSERT ON notes BEGIN SELECT 1; END")
	p := outsideProject(t, root, srv.URL)
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	app.exec(t, "DROP TRIGGER notes_log")
	_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
	if !changedOutside(err, "db/trigger:notes_log (missing)") || !strings.Contains(err.Error(), "make them again in the working database as the pristine copy has them") ||
		strings.Contains(err.Error(), "seed") {
		t.Fatalf("capture: %v", err)
	}
}
