package cli

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqliteDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// TestClean: clean removes the pristine copy seed made and its record,
// asks first (and without a terminal needs --yes), never touches the
// working database, refuses a name the configuration does not give a
// database, and a copy seed did not make.
func TestClean(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("DRIFTGATE_STORE", store)
	working := filepath.Join(root, "database", "app.sqlite")
	sqliteDB(t, working, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)", "INSERT INTO users (email) VALUES ('a@example.com')")
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, "version: 1\nproject: shop\ntarget:\n  base_url: http://127.0.0.1:1\nstate:\n  db:\n    driver: sqlite\n    working: database/app.sqlite\n  cache:\n    restore: [\"true\"]\n")
	if code, out, errOut := runCLI(t, "seed", "--config", cfg); code != ExitClean {
		t.Fatalf("seed: %d\n%s\n%s", code, out, errOut)
	}
	made := func() []string {
		files, _ := filepath.Glob(filepath.Join(store, "pristine", "db-*"))
		return files
	}
	if len(made()) != 2 { // the copy and its record
		t.Fatalf("seed made %v", made())
	}
	old := cleanInput
	t.Cleanup(func() { cleanInput = old })

	cleanInput = func() io.Reader { return nil }
	if code, out, errOut := runCLI(t, "clean", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "--yes") ||
		!strings.Contains(out, "state db: file ") || len(made()) != 2 {
		t.Fatalf("no terminal: %d\n%s\n%s", code, out, errOut)
	}
	cleanInput = func() io.Reader { return strings.NewReader("n\n") }
	if code, out, _ := runCLI(t, "clean", "--config", cfg); code != ExitClean || !strings.Contains(out, "nothing removed") || len(made()) != 2 {
		t.Fatalf("answered no: %d\n%s", code, out)
	}
	for _, name := range []string{"cache", "nosuch"} {
		if code, _, errOut := runCLI(t, "clean", name, "--config", cfg, "--yes"); code != ExitConfig || !strings.Contains(errOut, "not a database component") {
			t.Errorf("%s: %d %s", name, code, errOut)
		}
	}
	cleanInput = func() io.Reader { return strings.NewReader("y\n") }
	if code, out, errOut := runCLI(t, "clean", "db", "--config", cfg); code != ExitClean || !strings.Contains(out, "removed state db") || len(made()) != 0 {
		t.Fatalf("clean: %d\n%s\n%s\n%v", code, out, errOut, made())
	}
	if _, err := os.Stat(working); err != nil {
		t.Fatalf("working database: %v", err)
	}
	// Again: nothing there, nothing removed.
	if code, out, _ := runCLI(t, "clean", "--config", cfg, "--yes"); code != ExitClean || !strings.Contains(out, "no pristine copy") {
		t.Fatalf("again: %d\n%s", code, out)
	}

	// A file named as the pristine copy that seed did not make is kept.
	other := filepath.Join(root, "database", "other.sqlite")
	sqliteDB(t, other, "CREATE TABLE orders (id INTEGER PRIMARY KEY)")
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, strings.Replace(string(raw), "working: database/app.sqlite\n", "working: database/app.sqlite\n    pristine: database/other.sqlite\n", 1))
	if code, _, errOut := runCLI(t, "clean", "--config", cfg, "--yes"); code == ExitClean || !strings.Contains(errOut, "not removed") {
		t.Fatalf("not made by seed: %d %s", code, errOut)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a file seed did not make was removed: %v", err)
	}
}
