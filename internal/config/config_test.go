package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStateValidation(t *testing.T) {
	base := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values"}
	cases := []struct {
		comp Component
		err  string
	}{
		{Component{Restore: []string{"true"}}, ""},
		{Component{Driver: "mysql", DSN: Secret{Env: "X"}, Working: "app"}, ""},
		{Component{Driver: "mariadb", DSN: Secret{Op: "op://v/i/f"}, Working: "app", Pristine: "app_clean"}, ""},
		{Component{}, "needs a driver"},
		{Component{Driver: "oracle", DSN: Secret{Env: "X"}, Working: "app"}, "unknown driver"},
		{Component{Driver: "postgres", Working: "app"}, "needs dsn"},
		{Component{Driver: "postgres", DSN: Secret{Env: "X"}}, "needs working"},
		{Component{Driver: "mysql", DSN: Secret{Env: "X"}, Working: "app", Pristine: "app"}, "must differ"},
		{Component{Driver: "mariadb", DSN: Secret{Env: "X"}, Working: "App", Pristine: "app"}, "must differ"},
		{Component{Driver: "postgres", DSN: Secret{Env: "X"}, Working: "App", Pristine: "app"}, ""},
		{Component{Driver: "mysql", DSN: Secret{Env: "X"}, Working: "app", Restore: []string{"x"}}, "without a driver"},
		{Component{Restore: []string{"x"}, Working: "app"}, "need a driver"},
		{Component{Driver: "mysql", DSN: Secret{Env: "X"}, Working: "app", Exclude: []string{"sess*"}, RowTracking: []string{"sessions"}}, "is excluded"},
		{Component{Driver: "mysql", DSN: Secret{Env: "X"}, Working: "app", Exclude: []string{"Sessions"}, RowTracking: []string{"sessions"}}, ""}, // as written
		{Component{Driver: "postgres", DSN: Secret{Env: "X"}, Working: "app", Exclude: []string{"Sessions"}, RowTracking: []string{"sessions"}}, ""},
	}
	for i, c := range cases {
		cfg := base
		cfg.State = map[string]Component{"db": c.comp}
		p, err := New(cfg, t.TempDir())
		switch {
		case c.err == "" && err != nil:
			t.Errorf("case %d: %v", i, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("case %d: error %v, want %q", i, err, c.err)
		case c.err == "" && c.comp.Driver != "":
			got := p.State["db"]
			if got.Driver != MySQL && got.Driver != Postgres {
				t.Errorf("case %d: driver %q not normalized", i, got.Driver)
			}
			if got.Pristine == "" {
				t.Errorf("case %d: no default pristine", i)
			}
		}
	}
	cfg := base
	cfg.Prepare = []Command{{}}
	if _, err := New(cfg, t.TempDir()); err == nil {
		t.Error("empty prepare command accepted")
	}
}

func TestBisectValidation(t *testing.T) {
	base := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values"}
	cases := []struct {
		b   Bisect
		err string
	}{
		{Bisect{}, ""},
		{Bisect{Checkout: []string{"make", "restart"}, Ready: []string{"curl", "-sf", "http://localhost/"}, Timeout: Duration{time.Minute}}, ""},
		{Bisect{Ready: []string{"true"}}, "needs checkout"},
		{Bisect{Finish: []string{"true"}}, "needs checkout"},
		{Bisect{Checkout: []string{"true"}, ReadyTimeout: Duration{-time.Second}}, "must not be negative"},
	}
	for i, c := range cases {
		cfg := base
		cfg.Bisect = c.b
		_, err := New(cfg, t.TempDir())
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("case %d: error %v, want %q", i, err, c.err)
		}
	}
}

// TestStateName: a component's name names a file of the local store.
func TestStateName(t *testing.T) {
	for _, name := range []string{"../../x", "a/b", ".db", ""} {
		cfg := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values",
			State: map[string]Component{name: {Restore: []string{"true"}}}}
		if _, err := New(cfg, t.TempDir()); err == nil {
			t.Errorf("state name %q accepted", name)
		}
	}
}

// TestNamesAreFileNamesEverywhere: state names that differ only in case
// share files on macOS and Windows, and a tag or baseline named like a
// Windows device cannot be a folder there.
func TestNamesAreFileNamesEverywhere(t *testing.T) {
	cfg := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values",
		State: map[string]Component{"DB": {Restore: []string{"true"}}, "db": {Restore: []string{"true"}}}}
	if _, err := New(cfg, t.TempDir()); err == nil || !strings.Contains(err.Error(), `"DB" and "db" differ only in case`) {
		t.Errorf("state names by case: %v", err)
	}
	for _, name := range []string{"con", "aux", "NUL.x"} {
		cfg := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values", State: map[string]Component{name: {Restore: []string{"true"}}}}
		if _, err := New(cfg, t.TempDir()); err == nil {
			t.Errorf("state name %q accepted", name)
		}
	}
	p, err := New(Config{Version: Version, Project: "p", Mode: Freeze, Level: "values"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvStore, t.TempDir())
	for _, name := range []string{"con", "PRN", "lpt1.old", "x."} {
		if _, err := p.Capture(name); err == nil {
			t.Errorf("tag %q accepted", name)
		}
		if _, err := p.Baseline(name); err == nil {
			t.Errorf("baseline %q accepted", name)
		}
	}
	if _, err := p.Capture("v1.2.3"); err != nil {
		t.Error(err)
	}
}

// TestStoreDir: DRIFTGATE_STORE comes before store: (the configuration is in
// the repository), store: may not be inside the project, and a store folder
// DriftGate makes is open to its owner only.
// storeDir is the store of a project at root with store: as given, made
// when the layout is computed (the environment is read then).
func storeDir(t *testing.T, root, store string) (string, error) {
	t.Helper()
	p, err := New(Config{Version: Version, Project: "p", Mode: Freeze, Level: "values", Store: store}, root)
	if err != nil {
		t.Fatal(err)
	}
	return p.StoreDir()
}

func TestStoreDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvStore, "")
	if dir, err := storeDir(t, root, filepath.Join(root, "driftgate", "store")); err == nil || !strings.Contains(err.Error(), "inside the repository") || !errors.Is(err, ErrInvalid) {
		t.Errorf("store inside the project: %s, %v", dir, err)
	}
	if _, err := storeDir(t, root, root); err == nil {
		t.Error("the project root as the store accepted")
	}
	outside := filepath.Join(t.TempDir(), "a", "store")
	if dir, err := storeDir(t, root, outside); err != nil || dir != outside {
		t.Errorf("store outside the project: %s, %v", dir, err)
	}
	// Windows keeps no permission bits of a folder.
	if fi, err := os.Stat(outside); err != nil || (fi.Mode().Perm() != 0o700 && runtime.GOOS != "windows") {
		t.Errorf("store folder: %v, %v", fi, err)
	}
	env := filepath.Join(t.TempDir(), "env")
	t.Setenv(EnvStore, env)
	if dir, err := storeDir(t, root, filepath.Join(root, "store")); err != nil || dir != env {
		t.Errorf("DRIFTGATE_STORE: %s, %v", dir, err)
	}
}

// TestStoreInRepository: store: (and uploads.dir) is relative to the project folder,
// not to where driftgate runs, and is refused anywhere in the git
// repository, also beside a configuration in a subfolder.
func TestStoreInRepository(t *testing.T) {
	t.Setenv(EnvStore, "")
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // run from elsewhere
	for _, store := range []string{".dg-store", "../dg-store", "../../dg-store", filepath.Join(repo, "dg-store")} {
		if dir, err := storeDir(t, root, store); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("store %s: %s, %v", store, dir, err)
		}
	}
	p, err := New(Config{Version: Version, Project: "p", Mode: Freeze, Level: "values", Store: "../../../dg-store"}, root)
	if err != nil {
		t.Fatal(err)
	}
	// The same for the folder of real files to upload.
	t.Setenv(EnvUploads, "")
	p.Uploads.Dir = "../../uploads"
	if dir, err := p.UploadsDir(); err == nil || !errors.Is(err, ErrInvalid) {
		t.Errorf("uploads in the repository: %s, %v", dir, err)
	}
	if dir, err := p.StoreDir(); err != nil || dir != filepath.Join(filepath.Dir(repo), "dg-store") {
		t.Errorf("store beside the repository: %s, %v", dir, err)
	}
	// A git worktree has a .git file.
	wt := t.TempDir()
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644)
	if _, err := storeDir(t, filepath.Join(wt, "sub"), "../store"); err == nil {
		t.Error("store inside a worktree accepted")
	}
	// ~/ is the home folder, wherever the configuration is.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows' home folder
	if dir, err := storeDir(t, filepath.Join(wt, "sub"), "~/.driftgate/p"); err != nil || dir != filepath.Join(home, ".driftgate", "p") {
		t.Errorf("~/ store: %s, %v", dir, err)
	}
}

// TestLoadRefuses: a negative timeout (http.Client would wait forever), a
// retry out of its range and a second YAML document (ignored, with every
// setting in it).
func TestLoadRefuses(t *testing.T) {
	base := "version: 1\nproject: p\n"
	for cfg, want := range map[string]string{
		base + "target:\n  timeout: -5s\n":             "target.timeout must be positive",
		base + "targets:\n  api:\n    timeout: -1ms\n": "targets.api.timeout must be positive",
		base + "---\nstore: /tmp/elsewhere\n":          "only one YAML document",
		base + "---\n":                                 "",
		base + "target:\n  timeout: 0s\n":              "",
		base + "target:\n  retry: 4\n":                 "target.retry must be from 0 (no retry) to 3",
		base + "targets:\n  api:\n    retry: -1\n":     "targets.api.retry must be from 0",
		base + "target:\n  retry: 0\n":                 "",
	} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%q: %v, want %q", cfg, err, want)
		}
	}
}

// TestHomeRelative: ~/ everywhere, and ~\ where the separator is \
// (Windows), for the tokens file and the uploads folder.
func TestHomeRelative(t *testing.T) {
	if rest, ok := HomeRelative("~/a/b"); !ok || rest != "a/b" {
		t.Errorf("~/a/b: %q %v", rest, ok)
	}
	defer func(s rune) { separator = s }(separator)
	separator = '/'
	if _, ok := HomeRelative(`~\a`); ok {
		t.Error(`~\a taken from the home folder where \ is not a separator`)
	}
	separator = '\\'
	if rest, ok := HomeRelative(`~\a\b`); !ok || rest != `a\b` {
		t.Errorf(`~\a\b: %q %v`, rest, ok)
	}
	home, _ := os.UserHomeDir()
	p := &Project{Root: t.TempDir(), Config: Config{Uploads: Uploads{Dir: `~\dg-uploads`}}}
	t.Setenv(EnvUploads, "")
	if dir, err := p.UploadsDir(); err != nil || !strings.HasPrefix(dir, home) {
		t.Errorf("uploads ~\\: %s %v", dir, err)
	}
}
