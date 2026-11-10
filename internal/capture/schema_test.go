package capture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
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
// same; a --read-only capture without prepare: is told to add it, and
// with it (state: restores what it writes) is checked as any capture; a
// seed takes the change into the pristine copy.
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
		if !changedOutside(err, "db/table:users (changed)") || !strings.Contains(err.Error(), "Fix: add the command that runs the migrations to prepare:") {
			t.Fatalf("read-only capture: %v", err)
		}
		// With state:, a read-only capture takes prepare: and its check.
		_, err = Run(ctx, outsideProject(t, root, srv.URL, "SELECT 1"), sqliteCatalog(), Options{Tag: "ro-prepare", ReadOnly: true})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:users; the restore undid it, and prepare did not make it again") {
			t.Fatalf("read-only capture whose prepare does not make it again: %v", err)
		}
		if _, err := Run(ctx, outsideProject(t, root, srv.URL, slug), sqliteCatalog(), Options{Tag: "ro-made", ReadOnly: true}); err != nil {
			t.Fatalf("read-only capture whose prepare makes it again: %v", err)
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

// schemaRecordOf is the path of the schemaRecord of component db.
func schemaRecordOf(t *testing.T, p *config.Project) string {
	t.Helper()
	server, err := serverOf(p, p.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := pristinePath(p, "db", server)
	if err != nil {
		t.Fatal(err)
	}
	return schemaPath(path)
}

// TestChangedOutsideRecord: a run whose prepare fails or is cancelled
// after its restore undid a migration leaves the record of what prepare
// must make again, and the next run checks it:
//   - the working database as the restore left it (the pristine schema),
//     or with the migration again (the mirror case): prepare must make it;
//   - another definition of a recorded item since: the run stops before
//     the restore, the record kept;
//   - a change since that the record does not name is added to it;
//   - a seed removes the record; a passing check removes it too.
func TestChangedOutsideRecord(t *testing.T) {
	const slug, posts = "ALTER TABLE users ADD COLUMN slug TEXT", "CREATE TABLE posts (id INTEGER PRIMARY KEY)"
	for _, c := range []struct {
		name    string
		stop    func(t *testing.T, root, url string) // the run that leaves the record
		since   []string                             // changes after it
		prepare []string
		want    string // the stop; "": the capture runs
	}{
		{"prepare failed, made again", failingPrepare, nil, []string{slug}, ""},
		{"prepare failed, not made again", failingPrepare, nil, []string{"SELECT 1"}, "db/table:users; the restore undid it, and prepare did not make it again"},
		{"cancelled, not made again", cancelledPrepare, nil, []string{"SELECT 1"}, "db/table:users; the restore undid it, and prepare did not make it again"},
		{"the migration again (mirror), made again", failingPrepare, []string{slug}, []string{slug}, ""},
		{"the migration again (mirror), not made again", failingPrepare, []string{slug}, []string{"SELECT 1"}, "db/table:users; the restore undid it"},
		{"changed again", failingPrepare, []string{"ALTER TABLE users ADD COLUMN other TEXT"}, []string{slug}, "changed again since an earlier run whose restore it outlived: db/table:users; nothing was restored"},
		{"a new change, both made", failingPrepare, []string{posts}, []string{slug, posts}, ""},
		{"a new change, one made", failingPrepare, []string{posts}, []string{slug}, "db/table:posts; the restore undid it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			ctx := context.Background()
			if err := Seed(ctx, outsideProject(t, root, srv.URL), SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			app.exec(t, slug)
			p := outsideProject(t, root, srv.URL, c.prepare...)
			c.stop(t, root, srv.URL)
			record := schemaRecordOf(t, p)
			if _, err := os.Stat(record); err != nil {
				t.Fatalf("no record after the stopped run: %v", err)
			}
			app.exec(t, c.since...)
			_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			_, kept := os.Stat(record)
			if c.want == "" {
				if err != nil || kept == nil {
					t.Fatalf("capture: %v; record kept: %v", err, kept == nil)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "seeding now, or deleting "+record+", accepts that it was undone") || kept != nil {
				t.Fatalf("capture: %v; record kept: %v", err, kept == nil)
			}
		})
	}

	t.Run("seed removes it", func(t *testing.T) {
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		root := t.TempDir()
		app, srv := outsideApp(t, root)
		p := outsideProject(t, root, srv.URL, "SELECT 1")
		ctx := context.Background()
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		app.exec(t, slug)
		failingPrepare(t, root, srv.URL)
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(schemaRecordOf(t, p)); err == nil {
			t.Fatal("the record is left after a seed")
		}
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "seeded"}); err != nil {
			t.Fatalf("capture after the seed: %v", err)
		}
	})
}

// failingPrepare runs a capture of outsideApp whose prepare fails after
// the restore.
func failingPrepare(t *testing.T, root, url string) {
	t.Helper()
	q := outsideProject(t, root, url)
	q.Prepare = []config.Command{{Run: []string{"false"}}}
	if _, err := Run(context.Background(), q, sqliteCatalog(), Options{Tag: "failed"}); err == nil || !strings.Contains(err.Error(), "prepare:") {
		t.Fatalf("capture with a failing prepare: %v", err)
	}
}

// cancelledPrepare runs a capture of outsideApp that is cancelled in
// prepare, after the restore.
func cancelledPrepare(t *testing.T, root, url string) {
	t.Helper()
	q := outsideProject(t, root, url)
	q.Prepare = []config.Command{{Run: []string{"sleep", "30"}}}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	if _, err := Run(ctx, q, sqliteCatalog(), Options{Tag: "cancelled"}); err == nil {
		t.Fatal("a cancelled capture passed")
	}
}

// TestChangedOutsideMigrateState: verify --migrate-state restores without
// prepare: with prepare: configured, a migration since the seed asks for
// a seed.
func TestChangedOutsideMigrateState(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p := outsideProject(t, root, srv.URL, "SELECT 1")
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	app.exec(t, "ALTER TABLE users ADD COLUMN slug TEXT")
	_, err := MigrateState(ctx, p, &snapshot.Meta{}, io.Discard, io.Discard)
	if !changedOutside(err, "db/table:users (changed)") || !strings.Contains(err.Error(), "Fix: run driftgate seed to make the working database the new pristine copy") {
		t.Fatalf("migrate-state: %v", err)
	}
}

// TestChangedOutsideTwoComponents: every component is checked before any
// is restored: a migration in the second stops the run before the rows
// written to the first are restored.
func TestChangedOutsideTwoComponents(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	other := newSQLiteApp(t, filepath.Join(root, "other.sqlite"), "wal")
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State: map[string]config.Component{
			"a": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}},
			"b": {Driver: config.SQLite, Working: "other.sqlite", Exclude: []string{"sessions"}}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	app.exec(t, "INSERT INTO users (email) VALUES ('kept@x')")
	other.exec(t, "ALTER TABLE users ADD COLUMN slug TEXT")
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "now"}); !changedOutside(err, "b/table:users (changed)") {
		t.Fatalf("capture: %v", err)
	}
	if n := app.count(t, "users WHERE email = 'kept@x'"); n != 1 {
		t.Errorf("component a was restored before b was checked")
	}
}

// TestChangedOutsidePreparedOtherwise: a migration by hand (slug) that
// prepare makes otherwise (slug and another column of the same table)
// stops the run after prepare; the next run, on what that prepare made,
// stops the same way, not as a definition changed again since.
func TestChangedOutsidePreparedOtherwise(t *testing.T) {
	const slug, other = "ALTER TABLE users ADD COLUMN slug TEXT", "ALTER TABLE users ADD COLUMN other TEXT"
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p := outsideProject(t, root, srv.URL, slug, other)
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	app.exec(t, slug)
	for _, run := range []string{"first", "second"} {
		_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: run})
		if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "db/table:users; the restore undid it, and prepare did not make it again, or made it otherwise") {
			t.Fatalf("%s run: %v", run, err)
		}
	}
}

// TestChangedOutsidePreparedOtherwiseThenStopped: after a check that
// found prepare made the change otherwise, a run whose prepare makes it
// so again and then fails leaves that schema; the next run repeats the
// stop after prepare, not "changed again".
func TestChangedOutsidePreparedOtherwiseThenStopped(t *testing.T) {
	const slug, other = "ALTER TABLE users ADD COLUMN slug TEXT", "ALTER TABLE users ADD COLUMN other TEXT"
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	app, srv := outsideApp(t, root)
	p := outsideProject(t, root, srv.URL, slug, other)
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	app.exec(t, slug)
	const stop = "db/table:users; the restore undid it, and prepare did not make it again, or made it otherwise"
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "first"}); !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), stop) {
		t.Fatalf("first run: %v", err)
	}
	failing := outsideProject(t, root, srv.URL, slug, other)
	failing.Prepare = append(failing.Prepare, config.Command{Run: []string{"false"}})
	if _, err := Run(ctx, failing, sqliteCatalog(), Options{Tag: "second"}); err == nil || !strings.Contains(err.Error(), "prepare:") {
		t.Fatalf("second run, its prepare failing: %v", err)
	}
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "third"}); !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), stop) {
		t.Fatalf("third run: %v", err)
	}
}

// TestExpect: the merge of an earlier run's record with the working and
// pristine schemas, without a database.
func TestExpect(t *testing.T) {
	p := state.Fingerprint{"table:a": "p", "table:c": "pc"}
	r := schemaRecord{Items: []string{"table:a"}, Working: state.Fingerprint{"table:a": "w"}, Prepared: state.Fingerprint{"table:a": "s"}}
	for _, c := range []struct {
		name  string
		r     schemaRecord
		w     state.Fingerprint
		items []string
		want  state.Fingerprint
		again bool
	}{
		{"no record, no difference", schemaRecord{}, state.Fingerprint{"table:a": "p", "table:c": "pc"}, nil, state.Fingerprint{}, false},
		{"no record, a difference", schemaRecord{}, state.Fingerprint{"table:a": "w", "table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"undone: as the pristine copy", r, state.Fingerprint{"table:a": "p", "table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"mirror: as recorded", r, state.Fingerprint{"table:a": "w", "table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"as the earlier prepare made it", r, state.Fingerprint{"table:a": "s", "table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"missing", r, state.Fingerprint{"table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"unreadable", r, nil, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, false},
		{"changed again", r, state.Fingerprint{"table:a": "x", "table:c": "pc"}, []string{"table:a"}, state.Fingerprint{"table:a": "w"}, true},
		{"a new difference besides", r, state.Fingerprint{"table:a": "p", "table:c": "pc", "table:n": "n"}, []string{"table:a", "table:n"}, state.Fingerprint{"table:a": "w", "table:n": "n"}, false},
		{"one dropped besides", r, state.Fingerprint{"table:a": "p"}, []string{"table:a", "table:c"}, state.Fingerprint{"table:a": "w"}, false},
	} {
		rec, again, _ := expect(c.r, c.w, p)
		if !slices.Equal(rec.Items, c.items) || !maps.Equal(rec.Working, c.want) || (len(again) > 0) != c.again {
			t.Errorf("%s: %+v, again %v", c.name, rec, again)
		}
	}
}

// TestChangedOutsideMissingPrepared: with prepare:, a trigger the working
// database lacks (what a restore stopped while it made it again leaves)
// is made again by the restore, which prepare does not undo: the run
// stops, and keeps stopping, with the two ways on. Deleting the record
// (the restore had stopped) goes on, as does a seed (the trigger was
// dropped on purpose).
func TestChangedOutsideMissingPrepared(t *testing.T) {
	for _, way := range []string{"delete the record", "seed"} {
		t.Run(way, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app, srv := outsideApp(t, root)
			app.exec(t, "CREATE TRIGGER notes_log AFTER INSERT ON notes BEGIN SELECT 1; END")
			p := outsideProject(t, root, srv.URL, "SELECT 1")
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			app.exec(t, "DROP TRIGGER notes_log")
			record := schemaRecordOf(t, p)
			if !filepath.IsAbs(record) {
				t.Fatalf("record path %s is not absolute", record)
			}
			for _, run := range []string{"first", "again"} {
				_, err := Run(ctx, p, sqliteCatalog(), Options{Tag: run})
				if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "the working database lacked db/trigger:notes_log before the restore, which made them again from the pristine copy. If an earlier run was stopped while it restored, delete "+record+" to go on; if they were dropped on purpose, add the migration that drops them to prepare:, or run driftgate seed") {
					t.Fatalf("%s run: %v", run, err)
				}
			}
			if way == "seed" {
				app.exec(t, "DROP TRIGGER notes_log") // dropped on purpose
				if err := Seed(ctx, p, SeedOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "on"}); err != nil {
				t.Fatalf("capture after the record was deleted: %v", err)
			}
		})
	}
}

// TestChangedOutsideSchemaCache: the schema the seed records spares the
// read of the pristine copy only when the working schema equals it. A
// cache that differs from the pristine copy, or one of another
// SchemaFormat, never stops a run on its own: the pristine copy is read.
// A record without it (an earlier release) reads the pristine copy too.
func TestChangedOutsideSchemaCache(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(record map[string]any)
		change string
		want   string // "": the capture runs
	}{
		{"stale cache, no change", func(r map[string]any) { r["schema"].(map[string]any)["table:users"] = "stale" }, "", ""},
		{"another format, no change", func(r map[string]any) { r["schema_format"] = 99 }, "", ""},
		{"stale cache, a migration", func(r map[string]any) { r["schema"].(map[string]any)["table:users"] = "stale" }, "ALTER TABLE users ADD COLUMN slug TEXT", "db/table:users (changed)"},
		{"no cache, a migration", func(r map[string]any) { delete(r, "schema"); delete(r, "schema_format") }, "ALTER TABLE users ADD COLUMN slug TEXT", "db/table:users (changed)"},
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
			if err != nil || json.Unmarshal(raw, &record) != nil || record["schema"] == nil || record["schema_format"] == nil {
				t.Fatalf("record of the seed: %s %v", raw, err)
			}
			c.edit(record)
			if err := snapshot.WriteJSON(path, record); err != nil {
				t.Fatal(err)
			}
			if c.change != "" {
				app.exec(t, c.change)
			}
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "now"})
			if c.want == "" && err != nil || c.want != "" && !changedOutside(err, c.want) {
				t.Fatalf("capture: %v", err)
			}
		})
	}
}

// TestWholeRestoreDrift: PostgreSQL restores the whole database, as its
// pristine copy is now. A pristine copy changed behind its record, in an
// item the restore did not find different, stops the run with the
// restore's message, from the fingerprint the run reads anyway; with a
// record of an older fingerprint format it is not compared, nor is a
// component the restore did not copy (the application wrote since). A
// whole copy later in the run (the reset before a request that changes
// data) is compared too; the final restore's is not.
func TestWholeRestoreDrift(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	for _, kind := range []string{"drift", "drift mid-run", "drift mid-run, prepare", "older format", "not restored"} {
		t.Run(kind, func(t *testing.T) {
			older := kind == "older format"
			curl, _ := exec.LookPath("curl")
			ctx := context.Background()
			t.Setenv(config.EnvStore, t.TempDir())
			t.Setenv("DG_DRIFT_DSN", dsn)
			work, pristine := "dgt_drift", "dgt_drift_pristine"
			// A POST writes a row to a; POST /prepare (the prepare: of
			// "drift mid-run, prepare") sets b back to the rows the record
			// has (the restore ends the sessions: a connection of its own
			// each time).
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					c, err := dbx.OpenPostgres(dsn, work, nil)
					if err == nil {
						if r.URL.Path == "/prepare" {
							_, err = c.Exec("DELETE FROM b WHERE id <> 1")
						} else {
							_, err = c.Exec("INSERT INTO a VALUES ($1)", 100+posts.Add(1))
						}
						c.Close()
					}
					if err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				Target: config.Target{BaseURL: srv.URL},
				State:  map[string]config.Component{"db": {Driver: config.Postgres, DSN: config.Secret{Env: "DG_DRIFT_DSN"}, Working: work, Pristine: pristine}}}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			admin, err := dbx.OpenPostgres(dsn, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			drop := func() {
				admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
				admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
			}
			drop()
			t.Cleanup(func() { drop(); admin.Close() })
			exec := func(db string, stmts ...string) {
				t.Helper()
				c, err := dbx.OpenPostgres(dsn, db, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				for _, s := range stmts {
					if _, err := c.Exec(s); err != nil {
						t.Fatalf("%s: %v", s, err)
					}
				}
			}
			if _, err := admin.Exec("CREATE DATABASE " + work); err != nil {
				t.Fatal(err)
			}
			exec(work, "CREATE TABLE a (id int PRIMARY KEY)", "CREATE TABLE b (id int PRIMARY KEY)", "INSERT INTO b VALUES (1)")
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if older {
				server, err := serverOf(p, p.State["db"], dsn)
				if err != nil {
					t.Fatal(err)
				}
				path, err := pristinePath(p, "db", server)
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if raw, err := os.ReadFile(path); err != nil || json.Unmarshal(raw, &record) != nil {
					t.Fatalf("record: %v", err)
				}
				delete(record, "format")
				if err := snapshot.WriteJSON(path, record); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "not restored" {
				// Nothing to restore; the application writes before the
				// fingerprint the run reads after the restore.
				tr, err := newTracker(ctx, p, nil, io.Discard, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				defer tr.close()
				if err := tr.restore(ctx); err != nil {
					t.Fatal(err)
				}
				exec(work, "INSERT INTO a VALUES (1)")
				now, err := tr.fingerprint(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := tr.checkWhole(ctx, now); err != nil {
					t.Fatalf("a component the restore did not copy: %v", err)
				}
				return
			}
			// Another hand changes the pristine copy's b; the application
			// writes a, which the restore finds different.
			if _, err := admin.Exec("ALTER DATABASE " + pristine + " ALLOW_CONNECTIONS true"); err != nil {
				t.Fatal(err)
			}
			exec(pristine, "INSERT INTO b VALUES (2)")
			admin.Exec("ALTER DATABASE " + pristine + " ALLOW_CONNECTIONS false")
			cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}}}
			if strings.HasPrefix(kind, "drift mid-run") {
				// The first restore copies nothing; the reset before the
				// second POST (--restore each) copies the whole database,
				// with the drift, and is compared on the fingerprint it
				// reads after it, or with prepare: before prepare, which
				// here sets b back as the record has it: a check after
				// prepare would find nothing.
				if kind == "drift mid-run, prepare" {
					if curl == "" {
						t.Skip("curl not found")
					}
					p.Prepare = []config.Command{{Run: []string{curl, "-sf", "-X", "POST", srv.URL + "/prepare"}}}
				}
				cat.Requests = append(cat.Requests, catalog.Request{ID: "post1", Method: "POST", Path: "/", Storage: snapshot.Public},
					catalog.Request{ID: "post2", Method: "POST", Path: "/", Storage: snapshot.Public})
			} else {
				exec(work, "INSERT INTO a VALUES (1)")
			}
			opt := Options{Tag: "now"}
			if strings.HasPrefix(kind, "drift mid-run") {
				opt.Restore = "each" // a reset before each request that changes data
			}
			_, err = Run(ctx, p, cat, opt)
			if older {
				if err != nil {
					t.Fatalf("capture with a record of an older format: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "restore of db left 1 items different from the fingerprint driftgate seed recorded of the pristine copy: [table:public.b]") {
				t.Fatalf("capture: %v", err)
			}
		})
	}
}

// fullReads counts the fingerprints of all items a component takes.
type fullReads struct {
	state.Component
	n int
}

func (f *fullReads) Fingerprint(ctx context.Context, items []string) (state.Fingerprint, error) {
	if items == nil {
		f.n++
	}
	return f.Component.Fingerprint(ctx, items)
}

// TestWholeCopyReads: a whole copy of PostgreSQL is read in full only by
// the reset before prepare or the tokens command (wholeFingerprint), once
// in a run: the final restore of a capture, whose requests all ran on
// checked data, reads only the items it restored.
func TestWholeCopyReads(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	p, exec := benchProject(t, config.Postgres, dsn)
	ctx := context.Background()
	tr, err := newTracker(ctx, p, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	if err := tr.restore(ctx); err != nil { // the first: nothing to copy
		t.Fatal(err)
	}
	f := &fullReads{Component: tr.comps[0]}
	tr.comps[0] = f
	write := func() {
		t.Helper()
		if err := exec("INSERT INTO t0 (name, n, at) VALUES ('w', 1, '2024-01-03 00:00:00')"); err != nil {
			t.Fatal(err)
		}
		tr.markDirty(map[string][]string{"db": {"table:public.t0"}})
	}
	for _, c := range []struct {
		name      string
		readWhole bool
		want      int
	}{{"final restore", false, 0}, {"reset before prepare", true, 1}, {"compared already", true, 1}} {
		write()
		if err := tr.restore(ctx); err != nil {
			t.Fatal(err)
		}
		if c.readWhole { // reset, before prepare
			whole, err := tr.wholeFingerprint(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.checkWhole(ctx, whole); err != nil {
				t.Fatal(err)
			}
		}
		if f.n != c.want {
			t.Errorf("%s: %d full reads in all, want %d", c.name, f.n, c.want)
		}
	}
}
