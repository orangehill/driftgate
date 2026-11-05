package capture

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestStalePristineHint: a restore that leaves items different says so
// with a hint when the pristine copy itself no longer matches its record
// (a pristine copy seeded by an earlier release on MySQL 8), and without
// one otherwise.
func TestStalePristineHint(t *testing.T) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(servers, ",") {
		t.Run(dsn[strings.Index(dsn, "@")+1:], func(t *testing.T) {
			ctx := context.Background()
			t.Setenv(config.EnvStore, t.TempDir())
			t.Setenv("DG_STALE_DSN", dsn)
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				State: map[string]config.Component{"db": {Driver: config.MySQL, DSN: config.Secret{Env: "DG_STALE_DSN"}, Working: "dgt_stale", Pristine: "dgt_stale_pristine"}}}, t.TempDir())
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
			exec("DROP DATABASE IF EXISTS dgt_stale", "DROP DATABASE IF EXISTS dgt_stale_pristine", "CREATE DATABASE dgt_stale",
				"CREATE TABLE dgt_stale.news (id INT PRIMARY KEY, title VARCHAR(20))", "INSERT INTO dgt_stale.news VALUES (1, 'a')")
			t.Cleanup(func() {
				root.Exec("DROP DATABASE IF EXISTS dgt_stale")
				root.Exec("DROP DATABASE IF EXISTS dgt_stale_pristine")
			})
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			restore := func() error {
				t.Helper()
				tr, err := newTracker(ctx, p, nil, io.Discard, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				defer tr.close()
				return tr.restore(ctx)
			}
			exec("UPDATE dgt_stale.news SET title = 'b'")
			if err := restore(); err != nil {
				t.Fatalf("restore: %v", err)
			}
			// The pristine copy changed after its record was made.
			exec("UPDATE dgt_stale_pristine.news SET title = 'c'", "UPDATE dgt_stale.news SET title = 'b'")
			err = restore()
			if err == nil || !strings.Contains(err.Error(), "left 1 items different") || !strings.Contains(err.Error(), "re-create the working database") {
				t.Fatalf("restore from a stale pristine copy: %v", err)
			}
		})
	}
}

// TestStaleHintMatches: no hint when the pristine copy matches its record.
func TestStaleHintMatches(t *testing.T) {
	c := pristineComp{f: state.Fingerprint{"table:a": "1", "table:b": "2"}}
	if h := staleHint(context.Background(), c, state.Fingerprint{"table:a": "1", "table:b": "3"}, []string{"table:a"}); h != "" {
		t.Errorf("hint for a matching item: %q", h)
	}
	if h := staleHint(context.Background(), c, state.Fingerprint{"table:a": "9"}, []string{"table:a"}); h == "" {
		t.Error("no hint for a pristine copy that differs from its record")
	}
}

type pristineComp struct {
	state.Component
	f state.Fingerprint
}

func (c pristineComp) PristineFingerprint(_ context.Context, items []string) (state.Fingerprint, error) {
	return c.f.Subset(items), nil
}
