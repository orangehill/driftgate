package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// TestPristinePerServer: two checkouts share the store but each uses its
// own database server; neither takes the other's pristine fingerprint, and
// the file of an earlier release is taken over once by the first that looks for one.
func TestPristinePerServer(t *testing.T) {
	store := t.TempDir()
	t.Setenv(config.EnvStore, store)
	project := func(env string) *config.Project {
		p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			State: map[string]config.Component{"db": {Driver: config.MySQL, DSN: config.Secret{Env: env}, Working: "app"}}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("DG_A_DSN", "mysql://root:a@127.0.0.1:33100/")
	t.Setenv("DG_B_DSN", "mysql://root:b@127.0.0.1:33200/")
	a, b := project("DG_A_DSN"), project("DG_B_DSN")
	ctx := context.Background()

	// A store of an earlier release: one pristine file, made by one of them.
	legacy := filepath.Join(store, "pristine", "db.json")
	if err := snapshot.WriteJSON(legacy, pristineFile{Version: 1, Component: "db", Driver: config.MySQL, Working: "app", Pristine: "app_pristine"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Seeded(ctx, a); err != nil || !ok {
		t.Fatalf("a takes over the file of an earlier release: %v %v", ok, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the file of an earlier release is still there: %v", err)
	}
	if ok, err := Seeded(ctx, b); err != nil || ok {
		t.Errorf("b, on another server, counts as seeded: %v %v", ok, err)
	}
	pa, _ := pristinePath(a, "db", "tcp(127.0.0.1:33100)")
	pb, _ := pristinePath(b, "db", "tcp(127.0.0.1:33200)")
	if pa == pb {
		t.Errorf("one pristine file for two servers: %s", pa)
	}
	// The same server under another user is the same databases.
	t.Setenv("DG_B_DSN", "mysql://other:x@127.0.0.1:33100/")
	if ok, err := Seeded(ctx, b); err != nil || !ok {
		t.Errorf("b on a's server: %v %v", ok, err)
	}
	// Another working database is another pristine copy.
	a.State["db"] = config.Component{Driver: config.MySQL, DSN: config.Secret{Env: "DG_A_DSN"}, Working: "app2", Pristine: "app2_pristine"}
	if ok, err := Seeded(ctx, a); err != nil || ok {
		t.Errorf("another working database counts as seeded: %v %v", ok, err)
	}
}

// TestSeedKeepsAdoption: a seed that fails before it replaces the pristine
// copy keeps the record of it, so a pristine database of an earlier release (no mark,
// adopted only because the record names it) is still adopted by the next
// seed.
func TestSeedKeepsAdoption(t *testing.T) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	ctx := context.Background()
	t.Setenv(config.EnvStore, t.TempDir())
	t.Setenv("DG_ADOPT_DSN", dsn)
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": {Driver: config.MySQL, DSN: config.Secret{Env: "DG_ADOPT_DSN"}, Working: "dgt_adopt", Pristine: "dgt_adopt_pristine"}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := dbx.OpenMySQL(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	exec := func(stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := root.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec("DROP DATABASE IF EXISTS dgt_adopt", "DROP DATABASE IF EXISTS dgt_adopt_pristine",
		"CREATE DATABASE dgt_adopt_pristine", "CREATE TABLE dgt_adopt_pristine.news (id INT PRIMARY KEY)")
	t.Cleanup(func() {
		root.Exec("DROP DATABASE IF EXISTS dgt_adopt")
		root.Exec("DROP DATABASE IF EXISTS dgt_adopt_pristine")
	})
	server, err := dbdsn.Server(config.MySQL, dsn)
	if err != nil {
		t.Fatal(err)
	}
	path, err := pristinePath(p, "db", server)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteJSON(path, pristineFile{Version: 1, Component: "db", Driver: config.MySQL, Server: server, Working: "dgt_adopt", Pristine: "dgt_adopt_pristine"}); err != nil {
		t.Fatal(err)
	}
	// No working database yet: the seed fails, and the record stays.
	if err := Seed(ctx, p, SeedOptions{}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("seed without a working database: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record is gone: %v", err)
	}
	exec("CREATE DATABASE dgt_adopt", "CREATE TABLE dgt_adopt.news (id INT PRIMARY KEY)", "INSERT INTO dgt_adopt.news VALUES (1)")
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatalf("seed over the adopted copy: %v", err)
	}
	pf, err := readPristine(p, "db", server)
	if err != nil || pf.Format != state.FingerprintFormat || len(pf.Fingerprint) != 1 {
		t.Fatalf("record after the seed: %+v %v", pf, err)
	}
}

// TestWorkingFollowsTheSetting: a DSN read from the project's .env that
// names another database than working stops with an error of the
// configuration before anything is restored.
func TestWorkingFollowsTheSetting(t *testing.T) {
	t.Setenv(config.EnvStore, t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_DATABASE=shop_wt42\nDB_USERNAME=u\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": {Driver: config.MySQL, DSN: config.Secret{Dotenv: ".env"}, Working: "shop"}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Seeded(context.Background(), p); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "names the database shop_wt42, but working is shop") {
		t.Errorf("Seeded: %v", err)
	}
}

// TestSeedWhileWriting: the application writes to the working database
// after the seed copied it and before it recorded the copy. The record is
// of the pristine copy, so a restore brings the working database back to
// it; a record of the working database would hold the application's row
// and fail every restore of the table until the next seed.
func TestSeedWhileWriting(t *testing.T) {
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
		t.Run(server, func(t *testing.T) { testSeedWhileWriting(t, driver, dsn) })
	}
}

func testSeedWhileWriting(t *testing.T, driver, dsn string) {
	ctx := context.Background()
	t.Setenv(config.EnvStore, t.TempDir())
	t.Setenv("DG_SEEDW_DSN", dsn)
	work, pristine := "dgt_seedw", "dgt_seedw_pristine"
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": {Driver: driver, DSN: config.Secret{Env: "DG_SEEDW_DSN"}, Working: work, Pristine: pristine, Exclude: []string{"sessions"}}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	open, drop, users := dbx.OpenMySQL, "DROP DATABASE IF EXISTS ", "CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(50))"
	if driver == config.Postgres {
		open, users = dbx.OpenPostgres, "CREATE TABLE users (id serial PRIMARY KEY, email text)"
	}
	admin, err := open(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	dropAll := func() {
		suffix := ""
		if driver == config.Postgres {
			suffix = " WITH (FORCE)"
		}
		for _, db := range []string{work, pristine} {
			if _, err := admin.Exec(drop + db + suffix); err != nil {
				t.Error(err)
			}
		}
	}
	dropAll()
	t.Cleanup(dropAll)
	if _, err := admin.Exec("CREATE DATABASE " + work); err != nil {
		t.Fatal(err)
	}
	// The application's own connection, to the working database.
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
	count := func() int {
		t.Helper()
		app, err := open(dsn, work, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer app.Close()
		var n int
		if err := app.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec(users, "CREATE TABLE sessions (id VARCHAR(40) PRIMARY KEY)", "INSERT INTO users (email) VALUES ('a@x'), ('b@x')")

	// The table is copied (MySQL) or the database (PostgreSQL), and then
	// the application writes.
	var once sync.Once
	chaos.At = func(point string) {
		if point == "state.copy" {
			once.Do(func() { exec("INSERT INTO users (email) VALUES ('written-during-seed@x')") })
		}
	}
	t.Cleanup(func() { chaos.At = nil })
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	chaos.At = nil
	if n := count(); n != 3 {
		t.Fatalf("%d users after the seed, want the 2 copied and the one written during it", n)
	}

	// A request writes; the restore after it brings back the pristine copy.
	for i := range 2 {
		exec("INSERT INTO users (email) VALUES ('request@x')", "INSERT INTO sessions VALUES ('s"+fmt.Sprint(i)+"')")
		tr, err := newTracker(ctx, p, nil, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		err = tr.restore(ctx)
		tr.close()
		if err != nil {
			t.Fatalf("restore %d after a seed the application wrote during: %v", i, err)
		}
		if n := count(); n != 2 {
			t.Fatalf("%d users after restore %d, want the 2 of the pristine copy", n, i)
		}
	}
}
