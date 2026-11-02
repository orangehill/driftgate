//go:build unix

package e2e

import (
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// setupRoutes is the OpenAPI description of the demo's reading routes.
const setupRoutes = `{"openapi": "3.0.0", "info": {"title": "shop", "version": "1"}, "paths": {
 "/api/news": {"get": {"responses": {"200": {"description": "ok"}}}},
 "/api/contact": {"get": {"responses": {"200": {"description": "ok"}}}}}}`

// setupFromZero runs driftgate setup --yes in a project folder that holds
// only the application's own files, then run and setup again: the first
// makes everything up to the baseline, run compares with it, and the
// second setup keeps what is there.
func setupFromZero(t *testing.T, files map[string]string, want ...string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		write(t, filepath.Join(root, name), content)
	}
	env := []string{"DRIFTGATE_STORE="}
	r := dg(t, root, env, "setup", "--yes")
	if r.code != 0 || !strings.Contains(r.out, "baseline local made") {
		t.Fatalf("setup: %v", r)
	}
	for _, w := range want {
		if !strings.Contains(r.out, w) {
			t.Errorf("setup output without %q: %v", w, r)
		}
	}
	if r := dg(t, root, env, "run"); r.code != 0 {
		t.Fatalf("run after setup: %v", r)
	}
	if r := dg(t, root, env, "setup", "--yes"); r.code != 0 || !strings.Contains(r.out, "the pristine copy is there already; kept") || strings.Contains(r.out, "== capture ==") {
		t.Fatalf("second setup: %v", r)
	}
	return root
}

// TestSetupMySQL: a project whose database is MariaDB/MySQL, named by the
// DB_* keys of its .env (as Laravel's): setup takes it from there, makes
// its pristine copy and the first baseline.
func TestSetupMySQL(t *testing.T) {
	dsn, db := mysqlDatabase(t, "setup")
	srv := httptest.NewServer(demo.NewSQL("v1", db))
	defer srv.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	env := "APP_URL=" + srv.URL + "\nDB_CONNECTION=mysql\nDB_HOST=" + u.Hostname() + "\nDB_PORT=" + u.Port() +
		"\nDB_DATABASE=dge2e_setup\nDB_USERNAME=" + u.User.Username() + "\nDB_PASSWORD=" + pass + "\n"
	root := setupFromZero(t, map[string]string{".env": env, "openapi.json": setupRoutes},
		"database:   mysql dge2e_setup on "+u.Hostname()+":"+u.Port(), "app:        "+srv.URL)
	raw, _ := os.ReadFile(filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if !strings.Contains(string(raw), "dsn: {dotenv: .env}") || strings.Contains(string(raw), "DB_PASSWORD") {
		t.Errorf("configuration:\n%s", raw)
	}
}

// TestSetupPostgres: a project whose PostgreSQL runs as a compose
// service: setup takes the service's published port and user (a
// superuser), makes the pristine copy and the first baseline.
func TestSetupPostgres(t *testing.T) {
	dsn, _ := postgresDatabase(t, "setup")
	srv := httptest.NewServer(demo.New("v1"))
	defer srv.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	compose := `services:
  db:
    image: postgres:17
    ports: ["` + u.Hostname() + `:` + u.Port() + `:5432"]
    environment:
      POSTGRES_USER: ` + u.User.Username() + `
      POSTGRES_PASSWORD: ` + pass + `
      POSTGRES_DB: dge2e_setup
`
	root := setupFromZero(t, map[string]string{"compose.yaml": compose, ".env": "APP_URL=" + srv.URL + "\n", "openapi.json": setupRoutes},
		"database:   postgres dge2e_setup on "+u.Hostname()+":"+u.Port())
	raw, _ := os.ReadFile(filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if !strings.Contains(string(raw), "dsn: {compose: compose.yaml, key: db}") {
		t.Errorf("configuration:\n%s", raw)
	}
}

// TestSetupWorktreeMySQL: a worktree added with git worktree add takes the
// main worktree's project, works on the database its own .env names (a
// pristine copy of its own; the main worktree's databases untouched), and
// run compares its changed code with the main worktree's baseline.
func TestSetupWorktreeMySQL(t *testing.T) {
	dsn, mainDB := mysqlDatabase(t, "setupmain")
	_, wtDB := mysqlDatabase(t, "setupwt")
	mainSrv := httptest.NewServer(demo.NewSQL("v1", mainDB))
	defer mainSrv.Close()
	wtAPI := demo.NewSQL("v1", wtDB)
	wtSrv := httptest.NewServer(wtAPI)
	defer wtSrv.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	env := func(app, db string) string {
		return "APP_URL=" + app + "\nDB_CONNECTION=mysql\nDB_HOST=" + u.Hostname() + "\nDB_PORT=" + u.Port() +
			"\nDB_DATABASE=" + db + "\nDB_USERNAME=" + u.User.Username() + "\nDB_PASSWORD=" + pass + "\n"
	}
	main := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", main, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, filepath.Join(main, "openapi.json"), setupRoutes)
	write(t, filepath.Join(main, ".gitignore"), ".env\n")
	write(t, filepath.Join(main, ".env"), env(mainSrv.URL, "dge2e_setupmain"))
	envs := []string{"DRIFTGATE_STORE="}
	if r := dg(t, main, envs, "setup", "--yes"); r.code != 0 || !strings.Contains(r.out, "baseline local made") {
		t.Fatalf("setup in the main worktree: %v", r)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "app")
	wt := filepath.Join(t.TempDir(), "wt")
	git("worktree", "add", "-q", wt, "-b", "change")
	write(t, filepath.Join(wt, ".env"), env(wtSrv.URL, "dge2e_setupwt"))

	r := dg(t, wt, envs, "setup", "--yes")
	if r.code != 0 || !strings.Contains(r.out, "taking the project from the main worktree") || !strings.Contains(r.out, "working dge2e_setupwt") ||
		strings.Contains(r.out, "== capture ==") {
		t.Fatalf("setup in the worktree: %v", r)
	}
	admin := openMySQL(t, dsn, "")
	for _, db := range []string{"dge2e_setupmain_pristine", "dge2e_setupwt_pristine"} {
		var n int
		if err := admin.QueryRow("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", db).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d %v", db, n, err)
		}
	}
	if r := dg(t, wt, envs, "run"); r.code != 0 {
		t.Fatalf("run of the unchanged code: %v", r)
	}
	wtAPI.SetVersion("v2")
	if r := dg(t, wt, envs, "run"); r.code != 1 || !strings.Contains(r.out, "x-powered-by") {
		t.Fatalf("run of the changed code: %v", r)
	}
	if r := dg(t, wt, envs, "setup", "--yes"); r.code != 0 || !strings.Contains(r.out, "the pristine copy is there already; kept") {
		t.Fatalf("second setup: %v", r)
	}
	if r := dg(t, main, envs, "run"); r.code != 0 {
		t.Errorf("the main worktree after the worktree's runs: %v", r)
	}
}
