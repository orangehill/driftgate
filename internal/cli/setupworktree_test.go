package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/secret"
)

// worktree commits the files of the main worktree's project (not
// .driftgate/ nor .env, which git does not carry) and adds a worktree of
// it, with files of its own (its .env); the test then works there.
func worktree(t *testing.T, main string, files map[string]string) string {
	t.Helper()
	gitIn(t, main, "add", "-A")
	gitIn(t, main, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "app")
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, main, "worktree", "add", "-q", wt, "-b", "change")
	for name, content := range files {
		write(t, filepath.Join(wt, name), content)
	}
	t.Chdir(wt)
	return wt
}

// TestSetupAdoptsMainWorktree: in a worktree, setup takes the main
// worktree's project with its baseline and key, captures no baseline of
// its own, and run then compares the worktree's code against the main
// worktree's baseline. A second setup keeps everything; init points to
// setup.
func TestSetupAdoptsMainWorktree(t *testing.T) {
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	defer srv.Close()
	main := setupDemo(t, srv.URL)
	t.Setenv(secret.EnvKey, "")
	if code, out, errOut := runCLI(t, "setup", "--yes"); code != ExitClean {
		t.Fatalf("setup in the main worktree: %d\n%s\n%s", code, out, errOut)
	}
	write(t, filepath.Join(main, ".gitignore"), mustRead(t, filepath.Join(main, ".gitignore"))+".env\n")
	wt := worktree(t, main, map[string]string{".env": "APP_URL=" + srv.URL + "\n"})
	if code, _, errOut := runCLI(t, "init"); code != ExitConfig || !strings.Contains(errOut, "run driftgate setup here") {
		t.Errorf("init in the worktree: %d\n%s", code, errOut)
	}
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitClean || !strings.Contains(out, "taking the project from the main worktree") || strings.Contains(out, "== capture ==") ||
		!strings.Contains(out, "the main worktree's (") || !strings.Contains(out, "driftgate run compares this worktree's code against it") {
		t.Fatalf("setup in the worktree: %d\n%s\n%s", code, out, errOut)
	}
	baseline := func(root string) map[string]string {
		return fileSums(t, filepath.Join(root, ".driftgate", "baselines", "local"))
	}
	if got, want := baseline(wt), baseline(main); len(got) == 0 || len(got) != len(want) {
		t.Fatalf("baseline %v, want the main worktree's %v", got, want)
	} else {
		for name, sum := range want {
			if got[name] != sum {
				t.Errorf("%s differs from the main worktree's", name)
			}
		}
	}
	before := fileSums(t, filepath.Join(wt, ".driftgate"))
	if code, out, errOut := runCLI(t, "setup", "--yes"); code != ExitClean || !strings.Contains(out, "baseline local is there already") {
		t.Fatalf("second setup: %d\n%s\n%s", code, out, errOut)
	}
	for name, sum := range before {
		if !strings.HasPrefix(name, "store/") && fileSums(t, filepath.Join(wt, ".driftgate"))[name] != sum {
			t.Errorf("%s changed", name)
		}
	}
	api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "run"); code != ExitDrift || !strings.Contains(out, "x-powered-by") {
		t.Errorf("run of the changed code: %d\n%s\n%s", code, out, errOut)
	}
}

// TestSetupWorktreeDatabase: a worktree whose .env names a database of its
// own gets that database as working (pristine: with it), and its own
// address as base_url, in its configuration, comments kept, before its
// own pristine copy; one that
// names the main worktree's database stops setup before anything is
// written, so the main worktree's database is never restored from here.
func TestSetupWorktreeDatabase(t *testing.T) {
	db := closedPort(t)
	env := func(name string) string {
		return "APP_URL=http://127.0.0.1:" + closedPort(t) + "\nDB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=" + db + "\nDB_DATABASE=" + name + "\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n"
	}
	main := setupDir(t, map[string]string{".gitignore": ".env\n.driftgate/\n", ".env": env("shop"), "app.php": "<?php\n"})
	write(t, filepath.Join(main, ".driftgate", "driftgate.yaml"), `# shop
version: 1
project: shop
baselines: local
target:
  base_url: http://127.0.0.1:1
state:
  db:
    driver: mysql
    dsn: {dotenv: .env}   # the password stays there
    working: shop         # the database the application uses
    pristine: 'shop_pristine'
`)
	write(t, filepath.Join(main, ".driftgate", "baselines", "local", "acceptances.jsonl"), "")

	wt := worktree(t, main, map[string]string{".env": env("shop")})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "name the main worktree's database shop") {
		t.Fatalf("setup on the main worktree's database: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(wt, ".driftgate", "driftgate.yaml")); err == nil {
		t.Error("the project was taken although its database is the main worktree's")
	}

	app := "http://127.0.0.1:" + closedPort(t)
	write(t, filepath.Join(wt, ".env"), strings.Replace(env("shop_wt"), "APP_URL=", "APP_URL="+app+"\n#", 1))
	code, out, errOut = runCLI(t, "setup", "--yes")
	// The pristine copy of shop_wt is made, and fails: nothing listens there.
	if code != ExitEnv || !strings.Contains(out, "state db: working shop_wt, pristine shop_wt_pristine") || !strings.Contains(errOut, "seed of db failed") {
		t.Fatalf("setup on its own database: %d\n%s\n%s", code, out, errOut)
	}
	raw := mustRead(t, filepath.Join(wt, ".driftgate", "driftgate.yaml"))
	if !strings.Contains(raw, "    working: shop_wt         # the database the application uses\n    pristine: shop_wt_pristine\n") ||
		!strings.Contains(raw, "dsn: {dotenv: .env}   # the password stays there") || !strings.Contains(raw, "  base_url: "+app+"\n") {
		t.Errorf("configuration:\n%s", raw)
	}
	if !strings.Contains(mustRead(t, filepath.Join(main, ".driftgate", "driftgate.yaml")), "working: shop ") {
		t.Error("the main worktree's configuration changed")
	}
	noSecret(t, wt, out, errOut)
}
