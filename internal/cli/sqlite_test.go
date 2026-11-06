package cli

import (
	"database/sql"
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
	want := []string{"sessions", "jobs", "job_batches", "failed_jobs"}
	if c.Driver != config.SQLite || c.Working != "database/database.sqlite" || c.Pristine != "" || c.DSN != (config.Secret{Dotenv: ".env"}) || !reflect.DeepEqual(c.Exclude, want) ||
		!reflect.DeepEqual(c.RestoreEmpty, []string{"cache", "cache_locks"}) {
		t.Errorf("state db: %+v", c)
	}
	if raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml")); !strings.Contains(raw, "dsn: {dotenv: .env}   # from .env, read when a command runs; it must name the working file\n") {
		t.Errorf("configuration:\n%s", raw)
	}
	if !strings.Contains(out, "chosen: exclude: sessions, jobs, job_batches, failed_jobs; restore_empty: cache, cache_locks") || strings.Contains(out+errOut, "GRANT") || strings.Contains(out, "password") {
		t.Errorf("output:\n%s\n%s", out, errOut)
	}

	// At a terminal, the exclusion is a question whose default is yes.
	root = setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv("http://127.0.0.1:" + closedPort(t))})
	input(t, "n\n")
	if code, out, errOut = runCLI(t, "setup"); code != ExitConfig || !strings.Contains(errOut, "The framework keeps its own state in this database (") || !strings.Contains(errOut, "empty cache, cache_locks at every restore (a cache; never compared)") {
		t.Fatalf("setup at a terminal: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); len(c.Exclude)+len(c.RestoreEmpty) != 0 || c.Working != "database/database.sqlite" {
		t.Errorf("state db after no: %+v", c)
	}
}

// TestInitSQLite: init writes the SQLite state: commented out, with the
// framework's own tables proposed for exclude:, the block commented in line
// and an SQLite example of seed:.
func TestInitSQLite(t *testing.T) {
	root := setupDir(t, map[string]string{"artisan": "", ".env": laravelSQLiteEnv("http://shop.test")})
	code, out, errOut := runCLI(t, "init")
	if code != ExitClean || !strings.Contains(out, "state: db sqlite database/database.sqlite (.env), commented out") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml"))
	for _, want := range []string{"#     working: database/database.sqlite\n", "#     exclude: [sessions, jobs, job_batches, failed_jobs]", "#     restore_empty: [cache, cache_locks]", "\n#     # pristine: a file of DriftGate's store",
		`#   - ["sqlite3", "database/database.sqlite", ".read dump.sql"]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("configuration without %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "mysql") {
		t.Errorf("a MySQL example for an SQLite database:\n%s", raw)
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
	c := config.Component{Driver: config.SQLite, Working: "database/database.sqlite"}
	if err := s.adoptSQLite(nil, main, main, "db", c, appconf.Found{}, appconf.Database{}, false); err == nil || !strings.Contains(err.Error(), "is the main worktree's database file") {
		t.Errorf("the main worktree's file: %v", err)
	}
}

// TestSetupSQLiteWorktreeMainFile: a worktree whose .env names the main
// worktree's file by its absolute path (a copied .env) is refused before
// anything is written, whether or not the configuration reads the
// setting: a false pass is worse than a refusal.
func TestSetupSQLiteWorktreeMainFile(t *testing.T) {
	for _, dsn := range []string{"    dsn: {dotenv: .env}\n", ""} {
		main := setupDir(t, map[string]string{".gitignore": ".env\n.driftgate/\n*.sqlite\n", ".env": "DB_CONNECTION=sqlite\n", "artisan": "",
			"database/.gitkeep": "", "database/database.sqlite": "SQLite format 3\x00"})
		write(t, filepath.Join(main, ".driftgate", "driftgate.yaml"), "version: 1\nproject: shop\nbaselines: local\ntarget:\n  base_url: http://127.0.0.1:1\nstate:\n  db:\n    driver: sqlite\n"+dsn+"    working: database/database.sqlite\n")
		write(t, filepath.Join(main, ".driftgate", "baselines", "local", "acceptances.jsonl"), "")
		env := "DB_CONNECTION=sqlite\nDB_DATABASE=" + filepath.Join(main, "database", "database.sqlite") + "\n"
		wt := worktree(t, main, map[string]string{".env": env, "database/database.sqlite": "SQLite format 3\x00"})
		code, out, errOut := runCLI(t, "setup", "--yes")
		_, taken := os.Stat(filepath.Join(wt, ".driftgate", "driftgate.yaml"))
		if dsn != "" {
			if code != ExitConfig || !strings.Contains(errOut, "names no database file of this worktree") || !strings.Contains(errOut, "outside the project") || taken == nil {
				t.Errorf("with a dsn: %d\n%s\n%s", code, out, errOut)
			}
			continue
		}
		if code != ExitConfig || !strings.Contains(errOut, "this worktree's settings name no database file of this worktree (.env: the SQLite file is outside the project") || taken == nil {
			t.Errorf("without a dsn: %d\n%s\n%s", code, out, errOut)
		}

		// Its own file in the settings, it takes the project.
		write(t, filepath.Join(wt, ".env"), "DB_CONNECTION=sqlite\n")
		if code, out, errOut = runCLI(t, "setup", "--yes"); !strings.Contains(out, "took .driftgate") || strings.Contains(errOut, "names no database file") {
			t.Errorf("without a dsn, own file: %d\n%s\n%s", code, out, errOut)
		}
	}
}

// TestSQLiteAdoptNotes: without a dsn, a worktree setting Find could not
// take is refused unless another setting names working; one that names
// working leaves the note shown.
func TestSQLiteAdoptNotes(t *testing.T) {
	here := t.TempDir()
	write(t, filepath.Join(here, "database", "database.sqlite"), "SQLite format 3\x00")
	s := &setup{a: &asker{yes: true}, resume: "driftgate setup", out: &strings.Builder{}}
	c := config.Component{Driver: config.SQLite, Working: "database/database.sqlite"}
	outside := appconf.Note{File: ".env", Text: "the SQLite file is outside the project; DriftGate restores a file of the project only"}
	if err := s.adoptSQLite(nil, here, t.TempDir(), "db", c, appconf.Found{Notes: []appconf.Note{outside}}, appconf.Database{}, false); err == nil || !strings.Contains(err.Error(), "name no database file of this worktree") {
		t.Errorf("outside only: %v", err)
	}
	found := appconf.Found{Notes: []appconf.Note{outside}, Databases: []appconf.Database{{Driver: appconf.SQLite, Path: "database/database.sqlite"}}}
	if err := s.adoptSQLite(nil, here, t.TempDir(), "db", c, found, appconf.Database{}, false); err != nil {
		t.Errorf("a setting names working: %v", err)
	}
	if out := s.out.(*strings.Builder).String(); !strings.Contains(out, "note: .env: the SQLite file is outside the project") {
		t.Errorf("note not shown: %q", out)
	}
	if err := s.adoptSQLite(nil, here, t.TempDir(), "db", c, appconf.Found{}, appconf.Database{}, false); err != nil {
		t.Errorf("no settings: %v", err)
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
	block := stateBlock(primary, true, appconf.Tables{Exclude: []string{"solid_queue_jobs"}, RestoreEmpty: []string{"solid_cache_entries"}})
	if !strings.Contains(block, "    exclude: [solid_queue_jobs]") || !strings.Contains(block, "    restore_empty: [solid_cache_entries]") || strings.Contains(block, "# exclude:") {
		t.Errorf("state block:\n%s", block)
	}
	if block = stateBlock(appconf.Database{Driver: appconf.SQLite, Ref: ref}, true, appconf.Tables{}); !strings.Contains(block, "# set working (the database file the application uses") {
		t.Errorf("state block without a file:\n%s", block)
	}
}

// TestSeedExample: the commented seed: of a new configuration fits the
// database found, and Laravel's migrations and seeders for Laravel.
func TestSeedExample(t *testing.T) {
	for _, c := range []struct {
		d       appconf.Database
		laravel bool
		want    string
	}{
		{appconf.Database{Driver: appconf.MySQL, Name: "shop"}, false, "mysql shop"},
		{appconf.Database{Driver: appconf.Postgres, Name: "shop"}, false, `"pg_restore"`},
		{appconf.Database{Driver: appconf.SQLServer, Name: "shop"}, false, `RESTORE DATABASE [shop]`},
		{appconf.Database{Driver: appconf.SQLite, File: "db/app.sqlite"}, false, `"sqlite3"`},
		{appconf.Database{Driver: appconf.SQLServer, Name: "shop"}, true, `["php", "artisan", "migrate:fresh", "--seed", "--force"]`},
	} {
		got := seedExample(c.d, c.laravel)
		if !strings.Contains(got, c.want) || !strings.HasPrefix(got, "#   - [") {
			t.Errorf("%+v %t: %s, want %s", c.d, c.laravel, got, c.want)
		}
		// Uncommented, it is a command of seed: as the configuration reads
		// one, and its line hides no part of it (it holds no secret).
		path := filepath.Join(t.TempDir(), config.FileName)
		os.WriteFile(path, []byte("version: 1\nproject: p\nseed:\n"+strings.TrimPrefix(got, "#")+"\n"), 0o644)
		p, err := config.Load(path)
		if err != nil || len(p.Seed) != 1 || len(p.Seed[0].Run) < 2 || p.Seed[0].Dir != "" || p.Seed[0].Stdin != "" || strings.Contains(p.Seed[0].String(), "***") {
			t.Errorf("%+v %t: %s: %+v %v", c.d, c.laravel, got, p, err)
		}
	}
}

// TestSetupSQLiteWorktreeFromPristine: a worktree without the database
// file (git does not carry it) gets one from the main worktree's pristine
// copy, the data its baseline was captured from, without the pristine
// copy's mark; a run there then starts from the baseline's data.
func TestSetupSQLiteWorktreeFromPristine(t *testing.T) {
	main := setupDir(t, map[string]string{".gitignore": ".env\n.driftgate/\n*.sqlite\n", ".env": "APP_URL=http://127.0.0.1:" + closedPort(t) + "\nDB_CONNECTION=sqlite\n",
		"artisan": "", "database/.gitkeep": ""})
	db, err := sql.Open("sqlite", filepath.Join(main, "database", "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)", "INSERT INTO users (email) VALUES ('a@x'), ('b@x')"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	write(t, filepath.Join(main, ".driftgate", "driftgate.yaml"), "version: 1\nproject: shop\nbaselines: local\ntarget:\n  base_url: http://127.0.0.1:1\nstate:\n  db:\n    driver: sqlite\n    working: database/database.sqlite\n")
	write(t, filepath.Join(main, ".driftgate", "baselines", "local", "acceptances.jsonl"), "")
	if code, out, errOut := runCLI(t, "seed"); code != ExitClean {
		t.Fatalf("seed in the main worktree: %d\n%s\n%s", code, out, errOut)
	}
	wt := worktree(t, main, map[string]string{".env": "DB_CONNECTION=sqlite\n"})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if !strings.Contains(out, "state db: database/database.sqlite made from the main worktree's pristine copy") {
		t.Fatalf("setup without the file: %d\n%s\n%s", code, out, errOut)
	}
	copied, err := sql.Open("sqlite", filepath.Join(wt, "database", "database.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var users, marks int
	if err := copied.QueryRow("SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM sqlite_schema WHERE name = 'driftgate_pristine')").Scan(&users, &marks); err != nil || users != 2 || marks != 0 {
		t.Fatalf("the worktree's file: %d users, %d marks (%v)", users, marks, err)
	}
	copied.Close()

	// Gone again in the worktree that has the project: a command says
	// where to get it, and setup makes it again.
	if err := os.Remove(filepath.Join(wt, "database", "database.sqlite")); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := runCLI(t, "seed"); code != ExitConfig || !strings.Contains(errOut, "working: database/database.sqlite: not found in this worktree (git does not carry it): driftgate setup makes it") {
		t.Fatalf("seed without the file: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "setup", "--yes"); !strings.Contains(out, "state db: database/database.sqlite made from the main worktree's pristine copy") {
		t.Fatalf("setup in the worktree's project without the file: %d\n%s\n%s", code, out, errOut)
	}
}
