package config

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLayout: baselines: local keeps everything in .driftgate/, git is the
// layout of earlier releases; DRIFTGATE_STORE moves the store in both.
func TestLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(EnvStore, "")
	root := t.TempDir()
	base := Config{Version: Version, Project: "shop", Mode: Freeze, Level: "values"}
	local := base
	local.Baselines = BaselinesLocal
	p, err := New(local, root)
	if err != nil {
		t.Fatal(err)
	}
	dg := filepath.Join(root, ".driftgate")
	store := filepath.Join(dg, "store")
	want := Layout{Mode: BaselinesLocal, Root: root, Config: filepath.Join(dg, "driftgate.yaml"), Dir: dg,
		Catalog: filepath.Join(dg, "catalog.json"), Allowlist: filepath.Join(dg, "allowlist.yaml"), Baselines: filepath.Join(dg, "baselines"),
		Store: store, Captures: filepath.Join(store, "captures"), Private: filepath.Join(store, "private"), Pristine: filepath.Join(store, "pristine"),
		Lock: filepath.Join(store, ".lock"), Key: filepath.Join(dg, "key"), Ignored: dg}
	if p.Layout != want {
		t.Errorf("local:\n%+v\nwant\n%+v", p.Layout, want)
	}
	if p.Path("routes.json") != filepath.Join(dg, "routes.json") {
		t.Errorf("Path: %s", p.Path("routes.json"))
	}

	// No baselines: is git: the layout of earlier releases.
	p, err = New(base, root)
	if err != nil {
		t.Fatal(err)
	}
	store = filepath.Join(home, ".driftgate", "shop")
	want = Layout{Mode: BaselinesGit, Root: root, Config: filepath.Join(root, "driftgate.yaml"), Dir: filepath.Join(root, "driftgate"),
		Catalog: filepath.Join(root, "driftgate", "catalog.json"), Allowlist: filepath.Join(root, "driftgate", "allowlist.yaml"), Baselines: filepath.Join(root, "driftgate", "baselines"),
		Store: store, Captures: filepath.Join(store, "captures"), Private: filepath.Join(store, "private"), Pristine: filepath.Join(store, "pristine"),
		Lock: filepath.Join(store, ".lock")}
	if p.Layout != want {
		t.Errorf("git:\n%+v\nwant\n%+v", p.Layout, want)
	}

	env := filepath.Join(t.TempDir(), "env")
	t.Setenv(EnvStore, env)
	for _, c := range []Config{base, local} {
		if p, err = New(c, root); err != nil || p.Layout.Store != env || p.Layout.Private != filepath.Join(env, "private") {
			t.Errorf("%s with DRIFTGATE_STORE: %+v %v", c.Baselines, p.Layout, err)
		}
	}
	bad := base
	bad.Baselines = "cloud"
	if _, err := New(bad, root); err == nil || !strings.Contains(err.Error(), "baselines must be local or git") {
		t.Errorf("baselines: cloud: %v", err)
	}
}

// TestLoadMode: the place of driftgate.yaml is the mode. A file of an earlier
// release at the root without baselines: is git and stays as it is; baselines: that
// disagrees with the place is refused.
func TestLoadMode(t *testing.T) {
	t.Setenv(EnvStore, "")
	const gitFile = "version: 1\nproject: shop\n"
	root := t.TempDir()
	writeFile(t, filepath.Join(root, FileName), gitFile)
	p, err := Load(filepath.Join(root, FileName))
	if err != nil || p.Layout.Mode != BaselinesGit || p.Layout.Dir != filepath.Join(root, "driftgate") || p.Layout.Key != "" {
		t.Fatalf("git configuration: %+v %v", p, err)
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, FileName)); strings.Contains(string(raw), "baselines") {
		t.Errorf("Save added baselines: to a git configuration:\n%s", raw)
	}

	local := t.TempDir()
	writeFile(t, filepath.Join(local, LocalDir, FileName), gitFile)
	p, err = Load(filepath.Join(local, LocalDir, FileName))
	if err != nil || p.Layout.Mode != BaselinesLocal || p.Root != local || p.Layout.Store != filepath.Join(local, LocalDir, "store") {
		t.Fatalf(".driftgate/driftgate.yaml: %+v %v", p, err)
	}

	for path, cfg := range map[string]string{
		filepath.Join(t.TempDir(), FileName):           gitFile + "baselines: local\n",
		filepath.Join(t.TempDir(), LocalDir, FileName): gitFile + "baselines: git\n",
	} {
		writeFile(t, path, cfg)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "driftgate baselines") {
			t.Errorf("%s with %q: %v", path, cfg, err)
		}
	}
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, gitFile+"baselines: cloud\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "baselines must be local or git") {
		t.Errorf("baselines: cloud: %v", err)
	}
	for path, cfg := range map[string]string{
		filepath.Join(t.TempDir(), FileName):           gitFile + "baselines: git\n",
		filepath.Join(t.TempDir(), LocalDir, FileName): gitFile + "baselines: local\n",
	} {
		writeFile(t, path, cfg)
		if _, err := Load(path); err != nil {
			t.Errorf("%s with %q: %v", path, cfg, err)
		}
	}
}

// TestFind: commands find the project from any folder inside it, up to
// the top of the git work tree; a project with a configuration in both
// places is refused.
func TestFind(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	root := filepath.Join(repo, "api")
	sub := filepath.Join(root, "app", "Http")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(sub); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("no configuration: %v", err)
	}
	// Above the work tree is not this project.
	writeFile(t, filepath.Join(filepath.Dir(repo), FileName), "version: 1\nproject: other\n")
	t.Cleanup(func() { os.Remove(filepath.Join(filepath.Dir(repo), FileName)) })
	if _, err := Find(sub); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("configuration above the work tree: %v", err)
	}

	local := filepath.Join(root, LocalDir, FileName)
	writeFile(t, local, "version: 1\nproject: api\n")
	if path, err := Find(sub); err != nil || path != local {
		t.Fatalf("local: %s %v", path, err)
	}
	t.Chdir(sub)
	if p, err := Load(""); err != nil || p.Root != root || p.Layout.Mode != BaselinesLocal {
		t.Fatalf("Load from a subfolder: %+v %v", p, err)
	}

	writeFile(t, filepath.Join(root, FileName), "version: 1\nproject: api\n")
	for _, f := range []func() error{
		func() error { _, err := Find(sub); return err },
		func() error { _, err := Load(""); return err },
		func() error { _, err := Load(filepath.Join(root, FileName)); return err },
		func() error { _, err := Load(local); return err },
	} {
		if err := f(); err == nil || !strings.Contains(err.Error(), "has both") || !strings.Contains(err.Error(), "git rm -r driftgate.yaml driftgate/") || !strings.Contains(err.Error(), "mv .driftgate "+filepath.Join(filepath.Dir(repo), "api.driftgate-aside")) {
			t.Errorf("both configurations: %v", err)
		}
	}
	// The place it suggests for .driftgate/ is outside the work tree, where
	// no git add takes the key in.
	if r, _ := filepath.Rel(repo, aside(root)); !strings.HasPrefix(r, "..") {
		t.Errorf("aside %s is inside the work tree", aside(root))
	}
	os.Remove(local)
	if path, err := Find(sub); err != nil || path != filepath.Join(root, FileName) {
		t.Fatalf("git: %s %v", path, err)
	}
}

// TestWriteIgnores: .driftgate/ goes into .gitignore (made when missing)
// and into .dockerignore when there is one or a Docker build that would
// copy the folder; once, with the file's own line endings.
func TestWriteIgnores(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	l.Ignored = filepath.Join(l.Root, LocalDir)
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(l.Root, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return string(raw)
	}
	for range 2 {
		if err := l.WriteIgnores(); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(".gitignore"); got != ".driftgate/\n" {
		t.Errorf(".gitignore: %q", got)
	}
	if _, err := os.Stat(filepath.Join(l.Root, ".dockerignore")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".dockerignore made without Docker: %v", err)
	}

	writeFile(t, filepath.Join(l.Root, ".gitignore"), "/vendor\r\n.env")
	writeFile(t, filepath.Join(l.Root, "compose.yaml"), "services: {}\n")
	for range 2 {
		if err := l.WriteIgnores(); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(".gitignore"); got != "/vendor\r\n.env\r\n.driftgate/\r\n" {
		t.Errorf(".gitignore with CRLF: %q", got)
	}
	if got := read(".dockerignore"); got != ".driftgate/\n" {
		t.Errorf(".dockerignore with compose.yaml: %q", got)
	}

	// An entry in another form counts; a .dockerignore is extended without a
	// Dockerfile too.
	other := Layout{Root: t.TempDir()}
	other.Ignored = filepath.Join(other.Root, LocalDir)
	writeFile(t, filepath.Join(other.Root, ".gitignore"), "node_modules\n/.driftgate\n")
	writeFile(t, filepath.Join(other.Root, ".dockerignore"), ".git\n")
	if err := other.WriteIgnores(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(other.Root, ".gitignore")); string(raw) != "node_modules\n/.driftgate\n" {
		t.Errorf(".gitignore with /.driftgate: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(other.Root, ".dockerignore")); string(raw) != ".git\n.driftgate/\n" {
		t.Errorf(".dockerignore: %q", raw)
	}
	// git mode has nothing of its own to ignore.
	git := Layout{Root: t.TempDir()}
	if err := git.WriteIgnores(); err != nil || exists(filepath.Join(git.Root, ".gitignore")) {
		t.Errorf("git layout wrote .gitignore: %v", err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

// TestCheckIgnored: in a git work tree that does not ignore .driftgate/,
// nothing sensitive is written there (the store, the key); once git ignores
// it, it is. Outside git there is nothing to check.
func TestCheckIgnored(t *testing.T) {
	t.Setenv(EnvStore, "")
	t.Setenv(secret.EnvKey, "")
	local := Config{Version: Version, Project: "shop", Mode: Freeze, Level: "values", Baselines: BaselinesLocal}
	root := t.TempDir()
	gitInit(t, root)
	p, err := New(local, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteStore(); err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "add the line .driftgate/ to") {
		t.Fatalf("store in a folder git does not ignore: %v", err)
	}
	if _, err := p.CreateKey(); err == nil || !strings.Contains(err.Error(), "git does not ignore .driftgate/") {
		t.Fatalf("key in a folder git does not ignore: %v", err)
	}
	if exists(p.Layout.Store) || exists(p.Layout.Key) {
		t.Fatal("written before git ignored the folder")
	}
	// A file git tracks there keeps the refusal, with what to do about it.
	writeFile(t, filepath.Join(root, ".driftgate", "notes"), "x")
	if out, err := exec.Command("git", "-C", root, "add", ".driftgate/notes").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if err := p.Layout.WriteIgnores(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateKey(); err == nil || !strings.Contains(err.Error(), "git rm -r --cached .driftgate/") {
		t.Fatalf("tracked file in the folder: %v", err)
	}
	if out, err := exec.Command("git", "-C", root, "rm", "-q", "--cached", ".driftgate/notes").CombinedOutput(); err != nil {
		t.Fatalf("git rm: %v %s", err, out)
	}
	if dir, err := p.WriteStore(); err != nil || dir != p.Layout.Store {
		t.Fatalf("store once git ignores the folder: %s %v", dir, err)
	}
	where, err := p.CreateKey()
	if err != nil || where != p.Layout.Key {
		t.Fatalf("key: %s %v", where, err)
	}
	if fi, err := os.Stat(p.Layout.Key); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("key file: %v %v", fi, err)
	}
	raw, _ := os.ReadFile(p.Layout.Key)
	if k, err := p.LoadKey(); err != nil || base64.StdEncoding.EncodeToString(k)+"\n" != string(raw) {
		t.Fatalf("LoadKey from the key file: %v", err)
	}
	if _, err := p.CreateKey(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second key: %v", err)
	}

	// No git: nothing to ask.
	plain := t.TempDir()
	if p, err = New(local, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteStore(); err != nil {
		t.Fatalf("outside git: %v", err)
	}
}

// TestIgnoredByFiles: the ignore files settle it without git when root's
// .gitignore or the repository's info/exclude (also of a worktree, in the
// common folder) has the entry and no negation could take it back.
func TestIgnoredByFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for gi, want := range map[string]bool{
		"":                                  false,
		"vendor\n.driftgate/\n":             true,
		"/.driftgate\r\n":                   true,
		".driftgate/\n!.driftgate/key\n":    false,
		".driftgate\n!*.json\n":             false,
		"# .driftgate/\n":                   false,
		".driftgate-old/\n":                 false,
		"/vendor\n.driftgate\n!/vendor/x\n": true,
	} {
		writeFile(t, filepath.Join(root, ".gitignore"), gi)
		if got := ignoredByFiles(root, root, LocalDir); got != want {
			t.Errorf(".gitignore %q: %v, want %v", gi, got, want)
		}
	}
	writeFile(t, filepath.Join(root, ".gitignore"), "vendor\n")
	writeFile(t, filepath.Join(root, ".git", "info", "exclude"), ".driftgate/\n")
	if !ignoredByFiles(root, root, LocalDir) {
		t.Error("info/exclude not read")
	}
	// A project in a subfolder: the files from it up to the top count, an
	// entry by the rules git matches it with there; a negation anywhere
	// leaves it to git.
	sub := filepath.Join(root, "services", "api")
	os.Remove(filepath.Join(root, ".git", "info", "exclude"))
	for top, want := range map[string]bool{
		".driftgate/\n":                            true,  // any depth
		"/.driftgate/\n":                           false, // only at the top
		"services/api/.driftgate/\n":               true,
		"/services/api/.driftgate\n":               true,
		"api/.driftgate/\n":                        false, // a path from the top
		".driftgate/\n!services/api/.driftgate/\n": false,
	} {
		writeFile(t, filepath.Join(root, ".gitignore"), top)
		if got := ignoredByFiles(sub, root, LocalDir); got != want {
			t.Errorf("top .gitignore %q: %v, want %v", top, got, want)
		}
	}
	writeFile(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	writeFile(t, filepath.Join(sub, ".gitignore"), "!*.json\n")
	if ignoredByFiles(sub, root, LocalDir) {
		t.Error("a negation in the project's .gitignore settled nothing")
	}
	writeFile(t, filepath.Join(root, ".git", "info", "exclude"), "services/api/.driftgate/\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "vendor\n")
	os.Remove(filepath.Join(sub, ".gitignore"))
	if !ignoredByFiles(sub, root, LocalDir) {
		t.Error("info/exclude by path from the top not read")
	}

	// A worktree: .git is a file naming its folder in the main repository,
	// whose commondir leads to the common folder with info/exclude.
	main := t.TempDir()
	writeFile(t, filepath.Join(main, ".git", "info", "exclude"), "/.driftgate/\n")
	writeFile(t, filepath.Join(main, ".git", "worktrees", "wt", "commondir"), "../..\n")
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "wt")+"\n")
	if got := excludeFile(wt); got != filepath.Join(main, ".git", "info", "exclude") {
		t.Errorf("excludeFile of a worktree: %s", got)
	}
	if !ignoredByFiles(wt, wt, LocalDir) {
		t.Error("worktree: info/exclude of the common folder not read")
	}
}

// TestCheckIgnoredFallsBack: a negation the files cannot settle is left
// to git, which here says the folder is not ignored.
func TestCheckIgnoredFallsBack(t *testing.T) {
	t.Setenv(EnvStore, "")
	root := t.TempDir()
	gitInit(t, root)
	writeFile(t, filepath.Join(root, ".gitignore"), ".driftgate/\n!.driftgate/\n")
	p, err := New(Config{Version: Version, Project: "shop", Mode: Freeze, Level: "values", Baselines: BaselinesLocal}, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteStore(); err == nil || !strings.Contains(err.Error(), "git does not ignore") {
		t.Fatalf("negated entry: %v", err)
	}
	writeFile(t, filepath.Join(root, ".gitignore"), ".driftgate/\n!.driftgate/notes\n")
	if _, err := p.WriteStore(); err != nil {
		t.Fatalf("git ignores the folder: %v", err)
	}
}

// TestCheckIgnoredEveryPath: a .gitignore that ignores the folder's
// contents but takes the key and the store back passes no check: git must
// ignore the folder, the key, the store and the baselines each.
func TestCheckIgnoredEveryPath(t *testing.T) {
	t.Setenv(EnvStore, "")
	t.Setenv(secret.EnvKey, "")
	root := t.TempDir()
	gitInit(t, root)
	writeFile(t, filepath.Join(root, ".gitignore"), ".driftgate/*\n!.driftgate/key\n!.driftgate/store/\n")
	writeFile(t, filepath.Join(root, LocalDir, FileName), "version: 1\nproject: shop\nbaselines: local\n")
	p, err := Load(filepath.Join(root, LocalDir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateKey(); err == nil || !strings.Contains(err.Error(), ".driftgate/key") {
		t.Errorf("key create: %v", err)
	}
	if _, err := p.WriteStore(); err == nil || !strings.Contains(err.Error(), ".driftgate/store/") {
		t.Errorf("store: %v", err)
	}
	if exists(p.Layout.Key) {
		t.Error("key written")
	}
}

// TestIgnoreLinesAsGitReadsThem: git keeps leading whitespace and trailing
// tabs, and drops only unescaped trailing spaces and the \r of CRLF; a
// line git reads as another name neither settles the check nor counts as
// the entry init writes.
func TestIgnoreLinesAsGitReadsThem(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for gi, want := range map[string]bool{
		" .driftgate/\n":   false,
		".driftgate/\t\n":  false,
		".driftgate/  \n":  true,
		".driftgate/\r\n":  true,
		".driftgate/\\ \n": false,
	} {
		writeFile(t, filepath.Join(root, ".gitignore"), gi)
		if got := ignoredByFiles(root, root, LocalDir); got != want {
			t.Errorf(".gitignore %q: %v, want %v", gi, got, want)
		}
	}
	l := Layout{Root: root, Ignored: filepath.Join(root, LocalDir)}
	writeFile(t, filepath.Join(root, ".gitignore"), " .driftgate/\n")
	if err := l.WriteIgnores(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, ".gitignore")); string(raw) != " .driftgate/\n.driftgate/\n" {
		t.Errorf(".gitignore: %q", raw)
	}
}

// TestWriteIgnoresMonorepo: a Docker build at the top of the work tree
// copies a project in a subfolder too, so its .dockerignore gets the
// project's folder by its path from there.
func TestWriteIgnoresMonorepo(t *testing.T) {
	top := t.TempDir()
	writeFile(t, filepath.Join(top, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(top, "compose.yaml"), "services: {}\n")
	writeFile(t, filepath.Join(top, "services", ".dockerignore"), "node_modules\n")
	root := filepath.Join(top, "services", "api")
	l := Layout{Root: root, Ignored: filepath.Join(root, LocalDir)}
	for range 2 {
		if err := l.WriteIgnores(); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(top, ".dockerignore"):             "services/api/.driftgate/\n",
		filepath.Join(top, "services", ".dockerignore"): "node_modules\napi/.driftgate/\n",
		filepath.Join(root, ".gitignore"):               ".driftgate/\n",
	} {
		if raw, _ := os.ReadFile(path); string(raw) != want {
			t.Errorf("%s: %q, want %q", path, raw, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".dockerignore")); err == nil {
		t.Error(".dockerignore made in the project without Docker files there")
	}
	// Above the work tree is another project.
	if _, err := os.Stat(filepath.Join(filepath.Dir(top), ".dockerignore")); err == nil {
		t.Error(".dockerignore written above the work tree")
	}
}

// TestCheckIgnoredWithStoreElsewhere: with the store outside the project
// (DRIFTGATE_STORE) the key, the configuration and the baselines are
// still in .driftgate/, so a writing command still needs git to ignore it.
func TestCheckIgnoredWithStoreElsewhere(t *testing.T) {
	t.Setenv(EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	gitInit(t, root)
	p, err := New(Config{Version: Version, Project: "shop", Mode: Freeze, Level: "values", Baselines: BaselinesLocal}, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteStore(); err == nil || !strings.Contains(err.Error(), "git does not ignore .driftgate/") {
		t.Fatalf("store elsewhere, .driftgate/ not ignored: %v", err)
	}
	if err := p.Layout.WriteIgnores(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteStore(); err != nil {
		t.Fatal(err)
	}
}
