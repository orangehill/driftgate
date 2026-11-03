package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
)

// laravelSQLiteEnv is the .env of a Laravel 11 application on SQLite, with
// its sessions, cache and jobs in the database.
func laravelSQLiteEnv(appURL string) string {
	return "APP_URL=" + appURL + "\nDB_CONNECTION=sqlite\nSESSION_DRIVER=database\nCACHE_STORE=database\nQUEUE_CONNECTION=database\n"
}

// TestSetupSQLite: setup --yes on a Laravel application on SQLite writes
// state: with the database file as working, no pristine database and no
// grant, and excludes the framework's own tables; before the application
// has made the file, it stops with how to make it.
func TestSetupSQLite(t *testing.T) {
	root := setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv("http://127.0.0.1:" + closedPort(t))})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(out, "state: db sqlite database/database.sqlite (.env)") ||
		!strings.Contains(errOut, "the database file database/database.sqlite does not exist yet") || !strings.Contains(errOut, "php artisan migrate") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	_, c := loadState(t, root)
	want := []string{"sessions", "cache", "cache_locks", "jobs", "job_batches", "failed_jobs"}
	if c.Driver != config.SQLite || c.Working != "database/database.sqlite" || c.Pristine != "" || c.DSN != (config.Secret{Dotenv: ".env"}) || !reflect.DeepEqual(c.Exclude, want) {
		t.Errorf("state db: %+v", c)
	}
	if !strings.Contains(out, "chosen: exclude: sessions, cache") || strings.Contains(out+errOut, "GRANT") {
		t.Errorf("output:\n%s\n%s", out, errOut)
	}

	// At a terminal, the exclusion is a question whose default is yes.
	root = setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv("http://127.0.0.1:" + closedPort(t))})
	input(t, "n\n")
	if code, out, errOut = runCLI(t, "setup"); code != ExitConfig || !strings.Contains(errOut, "Exclude the framework's own tables sessions") {
		t.Fatalf("setup at a terminal: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); len(c.Exclude) != 0 || c.Working != "database/database.sqlite" {
		t.Errorf("state db after no: %+v", c)
	}
}

// TestInitSQLite: init writes the SQLite state: commented out, with the
// framework's own tables proposed for exclude:.
func TestInitSQLite(t *testing.T) {
	root := setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv("http://shop.test")})
	code, out, errOut := runCLI(t, "init")
	if code != ExitClean || !strings.Contains(out, "state: db sqlite database/database.sqlite (.env), commented out") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml"))
	for _, want := range []string{"#     working: database/database.sqlite\n", "#     exclude: [sessions, cache, cache_locks, jobs, job_batches, failed_jobs]", "    # pristine: a file of DriftGate's store"} {
		if !strings.Contains(raw, want) {
			t.Errorf("configuration without %q:\n%s", want, raw)
		}
	}
}

// TestSetupSQLiteDriverMismatch: --driver sqlite for a setting of a
// database server, or another driver for an SQLite file, stops setup
// before it writes anything.
func TestSetupSQLiteDriverMismatch(t *testing.T) {
	url := "http://127.0.0.1:" + closedPort(t)
	root := setupDir(t, map[string]string{
		"src/Shop.Api/Shop.Api.csproj":  `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`,
		"src/Shop.Api/appsettings.json": `{"ConnectionStrings": {"Default": "Host=127.0.0.1;Port=5432;Database=shop;Username=shop;Password=S3cr-x"}}`,
	})
	code, out, errOut := runCLI(t, "setup", "--yes", "--driver", "sqlite", "--url", url)
	if code != ExitConfig || !strings.Contains(errOut, "--driver sqlite: src/Shop.Api/Shop.Api.csproj ConnectionStrings:Default names a database server") {
		t.Fatalf("--driver sqlite: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
		t.Error("setup wrote files for a driver that does not fit the setting")
	}
	noSecret(t, root, out, errOut)

	root = setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv(url)})
	if code, out, errOut = runCLI(t, "setup", "--yes", "--driver", "mysql"); code != ExitConfig || !strings.Contains(errOut, "--driver mysql: .env names the SQLite file database/database.sqlite") {
		t.Fatalf("--driver mysql: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut = runCLI(t, "setup", "--yes", "--database", "../shop.sqlite"); code != ExitConfig || !strings.Contains(errOut, "--database: an SQLite database is its file") {
		t.Fatalf("--database outside: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
		t.Error("setup wrote files")
	}
}

// TestSetupSQLiteWorktree: a worktree takes the main worktree's SQLite
// project: its file is its own, which must be there (git does not carry
// it); a file of its own that its setting names becomes working.
func TestSetupSQLiteWorktree(t *testing.T) {
	env := func(db string) string {
		return "APP_URL=http://127.0.0.1:" + closedPort(t) + "\nDB_CONNECTION=sqlite\nDB_DATABASE=" + db + "\n"
	}
	main := setupDir(t, map[string]string{".gitignore": ".env\n.driftgate/\n*.sqlite\n", ".env": env(""), "artisan": "", "database/.gitkeep": "",
		"database/database.sqlite": "SQLite format 3\x00"})
	write(t, filepath.Join(main, ".driftgate", "driftgate.yaml"), `version: 1
project: shop
baselines: local
target:
  base_url: http://127.0.0.1:1
state:
  db:
    driver: sqlite
    dsn: {dotenv: .env}
    working: database/database.sqlite   # the file the application uses
`)
	write(t, filepath.Join(main, ".driftgate", "baselines", "local", "acceptances.jsonl"), "")

	wt := worktree(t, main, map[string]string{".env": env("")})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "the database file database/database.sqlite is not in this worktree (git does not carry it): copy the main worktree's") {
		t.Fatalf("setup without the file: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(wt, ".driftgate", "driftgate.yaml")); err == nil {
		t.Error("the project was taken without the worktree's database file")
	}

	write(t, filepath.Join(wt, ".env"), env("database/wt.sqlite"))
	write(t, filepath.Join(wt, "database", "wt.sqlite"), "SQLite format 3\x00")
	code, out, errOut = runCLI(t, "setup", "--yes")
	if !strings.Contains(out, "taking the project from the main worktree") || !strings.Contains(out, "state db: working database/wt.sqlite") ||
		strings.Contains(errOut, "do not say which database") || strings.Contains(errOut, "names the database file") {
		t.Fatalf("setup on its own file: %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(mustRead(t, filepath.Join(wt, ".driftgate", "driftgate.yaml")), "    working: database/wt.sqlite   # the file the application uses\n") {
		t.Errorf("configuration:\n%s", mustRead(t, filepath.Join(wt, ".driftgate", "driftgate.yaml")))
	}
	if !strings.Contains(mustRead(t, filepath.Join(main, ".driftgate", "driftgate.yaml")), "working: database/database.sqlite ") {
		t.Error("the main worktree's configuration changed")
	}

	// The main worktree's own file is refused.
	s := &setup{a: &asker{yes: true}, resume: "driftgate setup"}
	if err := s.adoptSQLite(main, main, "db", "database/database.sqlite", "database/database.sqlite"); err == nil || !strings.Contains(err.Error(), "is the main worktree's database file") {
		t.Errorf("the main worktree's file: %v", err)
	}
}

// TestSQLiteDescribe: an SQLite setting is described by its file; two
// files of one Rails setting do not disagree; state: names the exclusions.
func TestSQLiteDescribe(t *testing.T) {
	ref := appconf.Ref{Source: appconf.YAML, File: "config/database.yml", Key: "development.primary"}
	primary := appconf.Database{Driver: appconf.SQLite, Path: "storage/development.sqlite3", File: ref.File, Key: ref.Key, Ref: ref}
	queue := primary
	queue.Path, queue.Key, queue.Ref.Key = "storage/development_queue.sqlite3", "development.queue", "development.queue"
	if d := describe(primary); d != "sqlite storage/development.sqlite3 (config/database.yml development.primary)" {
		t.Errorf("describe: %s", d)
	}
	if _, _, ok := disagree([]appconf.Database{primary, queue}); ok {
		t.Error("the files of one Rails setting disagree")
	}
	other := primary
	other.Ref = appconf.Ref{Source: appconf.Env, Key: "DATABASE_URL"}
	other.Path = "db/other.sqlite3"
	if _, _, ok := disagree([]appconf.Database{primary, other}); !ok {
		t.Error("two settings of different files agree")
	}
	if d := describe(appconf.Database{Driver: appconf.SQLite, File: ".env", Ref: appconf.Ref{Source: appconf.Dotenv, File: ".env"}}); !strings.Contains(d, "(no database file named)") {
		t.Errorf("describe without a file: %s", d)
	}
	block := stateBlock(primary, true, "solid_queue_jobs")
	if !strings.Contains(block, "    exclude: [solid_queue_jobs]") || strings.Contains(block, "# exclude:") {
		t.Errorf("state block:\n%s", block)
	}
	if block = stateBlock(appconf.Database{Driver: appconf.SQLite, Ref: ref}, true); !strings.Contains(block, "# set working (the database file the application uses") {
		t.Errorf("state block without a file:\n%s", block)
	}
}
