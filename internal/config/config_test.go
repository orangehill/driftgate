package config

import (
	"errors"
	"fmt"
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

// TestDataDriver: data.driver mariadb becomes mysql, the only MySQL driver
// name data opens.
func TestDataDriver(t *testing.T) {
	cfg := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values",
		Data: Data{Driver: "mariadb", Source: DataDB{DSN: Secret{Env: "X"}, Database: "a"}, Target: DataDB{Database: "b"}}}
	p, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Data.Driver != MySQL {
		t.Errorf("data.driver %q not normalized", p.Data.Driver)
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
		base + "target:\n  connect_to: 203.0.113.10\n": "target.connect_to needs base_url",
		base + "target:\n  base_url: https://shop.example\n  connect_to: 203.0.113.10:8443\n":                      "",
		base + "targets:\n  api:\n    base_url: https://shop.example\n    connect_to: https://x\n":                 "targets.api: connect_to",
		base + "requests:\n  - match: get-news\n":                                                                  "requests 1: needs mutates or body",
		base + "requests:\n  - mutates: true\n":                                                                    "requests 1: needs match",
		base + "requests:\n  - match: \"GET /news\"\n    target: web\n    mutates: true\n":                         "requests 1: ",
		base + "requests:\n  - match: \"POST /search\"\n    mutates: false\n    body: { q: news, tags: [a, b] }\n": "",
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

// TestRequestBody: a body is written as JSON from the YAML as written:
// numbers keep their digits, a date stays its text, null is JSON's null;
// what JSON would read as another value, and keys that are no strings,
// are refused.
func TestRequestBody(t *testing.T) {
	base := "version: 1\nproject: p\nrequests:\n  - match: post-x\n    body: "
	for body, want := range map[string]string{
		`{n: 12345678901234567890, f: 1.0, d: 2024-01-02, s: "007", b: true, l: [a, ~]}`: `{"n":12345678901234567890,"f":1.0,"d":"2024-01-02","s":"007","b":true,"l":["a",null]}`,
		"null":                   "null",
		"007":                    "error: 007 is no JSON number",
		"0x1F":                   "error: 0x1F is no JSON number",
		"{a: {1: x}}":            `error: the key "1" is not a string`,
		"{a: !!binary aGk=}":     "error: a value tagged !!binary has no JSON form",
		"{n: 1e400, s: '1e400'}": `{"n":1e400,"s":"1e400"}`,
		"{<<: {a: 1}, b: 2}":     "error: merge keys (<<) are not supported in a body",
		"~":                      "null",
	} {
		path := filepath.Join(t.TempDir(), FileName)
		if err := os.WriteFile(path, []byte(base+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := Load(path)
		if e, ok := strings.CutPrefix(want, "error: "); ok {
			if err == nil || !strings.Contains(err.Error(), e) {
				t.Errorf("%s: %v, want %q", body, err, e)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if got, err := p.Requests[0].JSONBody(); err != nil || string(got) != want {
			t.Errorf("%s: %s %v, want %s", body, got, err, want)
		}
	}
	// body: with nothing after it is no body.
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte("version: 1\nproject: p\nrequests:\n  - match: x\n    mutates: true\n    body:\n"), 0o644)
	if p, err := Load(path); err != nil || p.Requests[0].Body != nil {
		t.Errorf("an empty body: %v", err)
	}
	// Aliases that repeat a value into gigabytes are refused.
	bomb := "version: 1\nproject: p\nrequests:\n  - match: x\n    body:\n      a: &a [\"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\", \"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\"]\n"
	prev := "a"
	for _, k := range []string{"b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s"} {
		bomb += fmt.Sprintf("      %s: &%s [*%s, *%s, *%s, *%s]\n", k, k, prev, prev, prev, prev)
		prev = k
	}
	os.WriteFile(path, []byte(bomb), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "larger than 10 MB") {
		t.Errorf("aliases repeated into a large body: %v", err)
	}
	os.WriteFile(path, []byte("version: 1\nproject: p\nrequests:\n  - match: x\n    mutate: true\n"), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field mutate not found") {
		t.Errorf("an unknown key of an entry: %v", err)
	}
}

// TestUploadsRelativeToProject: in the local layout, where driftgate.yaml
// is in .driftgate/, a relative uploads.dir is relative to the project
// folder, not to the folder of driftgate.yaml.
func TestUploadsRelativeToProject(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "shop")
	if err := os.MkdirAll(filepath.Join(root, ".driftgate"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".driftgate", FileName)
	if err := os.WriteFile(cfg, []byte("version: 1\nproject: shop\ntarget:\n  base_url: http://localhost:8000\nuploads:\n  dir: ../shop-uploads\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvUploads, "")
	p, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.UploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(base)
	if g, _ := filepath.EvalSymlinks(filepath.Dir(got)); g != want || filepath.Base(got) != "shop-uploads" {
		t.Errorf("uploads.dir ../shop-uploads in the local layout is %s, want %s", got, filepath.Join(base, "shop-uploads"))
	}
}
