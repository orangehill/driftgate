package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// localDemo is the demo project laid out as baselines: local, in a git
// work tree, with its key in .driftgate/key.
func localDemo(t *testing.T) (root string) {
	t.Helper()
	return localDemoProject(t).cfg
}

// localDemoProject is localDemo with its API; cfg is the project folder.
func localDemoProject(t *testing.T) *demoProject {
	t.Helper()
	p := newDemoProject(t, "")
	root := filepath.Dir(p.cfg)
	gitIn(t, root, "init", "-q")
	if err := os.Rename(filepath.Join(root, "driftgate"), filepath.Join(root, ".driftgate")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.cfg, filepath.Join(root, ".driftgate", "driftgate.yaml")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DRIFTGATE_STORE", "")
	t.Setenv(secret.EnvKey, "")
	p.cfg = root
	return p
}

// TestInitLocal: a new project keeps everything in .driftgate/, which
// git ignores; its key is a file there, open to its owner only.
func TestInitLocal(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	t.Chdir(root)
	code, out, errOut := runCLI(t, "init", "--project", "shop")
	if code != ExitClean || strings.Contains(out, "CODEOWNERS") || !strings.Contains(out, "created .driftgate/driftgate.yaml") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if err != nil || !strings.Contains(string(raw), "\nbaselines: local ") {
		t.Fatalf("configuration: %v\n%s", err, raw)
	}
	for _, f := range []string{"catalog.json", "allowlist.yaml"} {
		if _, err := os.Stat(filepath.Join(root, ".driftgate", f)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "driftgate.yaml")); err == nil {
		t.Error("driftgate.yaml at the root")
	}
	if raw, _ := os.ReadFile(filepath.Join(root, ".gitignore")); string(raw) != ".driftgate/\n" {
		t.Errorf(".gitignore: %q", raw)
	}
	if code, _, errOut := runCLI(t, "init"); code != ExitConfig || !strings.Contains(errOut, "already exists") {
		t.Errorf("second init: %d %s", code, errOut)
	}

	t.Setenv(secret.EnvKey, "")
	if code, out, errOut := runCLI(t, "key", "create"); code != ExitClean || !strings.Contains(out, filepath.Join(root, ".driftgate", "key")) {
		t.Fatalf("key create: %d %s %s", code, out, errOut)
	}
	fi, err := os.Stat(filepath.Join(root, ".driftgate", "key"))
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if code, _, errOut := runCLI(t, "key", "create"); code != ExitEnv || !strings.Contains(errOut, "already exists") {
		t.Errorf("second key: %d %s", code, errOut)
	}
	if st := gitIn(t, root, "status", "--porcelain", "--untracked-files=all"); st != "?? .gitignore\n" {
		t.Errorf("git sees more than .gitignore:\n%s", st)
	}
}

// TestInitGit: init --baselines git (or --config at the root) lays the
// project out in the git layout.
func TestInitGit(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if code, out, errOut := runCLI(t, "init", "--baselines", "git"); code != ExitClean || !strings.Contains(out, "CODEOWNERS") || !strings.Contains(out, "/driftgate/baselines/") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	for _, f := range []string{"driftgate.yaml", "driftgate/catalog.json", "driftgate/allowlist.yaml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); err == nil {
		t.Error("init --baselines git wrote .gitignore")
	}
	// Without the key the file is git, and an earlier release reads it.
	if raw, _ := os.ReadFile(filepath.Join(root, "driftgate.yaml")); strings.Contains(string(raw), "baselines") {
		t.Errorf("driftgate.yaml of baselines: git names it:\n%s", raw)
	}
	if p, err := config.Load(filepath.Join(root, "driftgate.yaml")); err != nil || p.Layout.Mode != config.BaselinesGit {
		t.Errorf("Load: %v", err)
	}
	other := t.TempDir()
	if code, _, errOut := runCLI(t, "init", "--config", filepath.Join(other, ".driftgate", "driftgate.yaml"), "--baselines", "git"); code != ExitConfig || !strings.Contains(errOut, "baselines: local keeps") {
		t.Errorf("--config in .driftgate with --baselines git: %d %s", code, errOut)
	}
}

// TestLocalRun: with baselines: local a run, from any folder of the
// project, keeps its captures, private bodies and baseline in .driftgate/
// and nothing in the home folder or in what git sees.
func TestLocalRun(t *testing.T) {
	root := localDemo(t)
	write(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	if code, _, errOut := runCLI(t, "key", "create", "--config", filepath.Join(root, ".driftgate", "driftgate.yaml")); code != ExitClean {
		t.Fatalf("key create: %s", errOut)
	}
	sub := filepath.Join(root, "app", "Http")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	code, out, errOut := runCLI(t, "run")
	if code != ExitClean || strings.Contains(out, "commit") {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	for _, dir := range []string{"baselines/local/responses", "store/captures", "store/private"} {
		if entries, err := os.ReadDir(filepath.Join(root, ".driftgate", dir)); err != nil || len(entries) == 0 {
			t.Errorf("%s: %v %v", dir, entries, err)
		}
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".driftgate")); err == nil {
		t.Error("the store went to the home folder")
	}
	if code, out, errOut := runCLI(t, "run"); code != ExitClean || !strings.Contains(out, "No drift") {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	if st := gitIn(t, root, "status", "--porcelain", "--untracked-files=all"); st != "?? .gitignore\n" {
		t.Errorf("git sees more than .gitignore:\n%s", st)
	}
}

// TestLocalRefusedWhenGitDoesNotIgnore: nothing sensitive is written into
// a .driftgate/ that git would commit.
func TestLocalRefusedWhenGitDoesNotIgnore(t *testing.T) {
	root := localDemo(t)
	t.Setenv(secret.EnvKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Chdir(root)
	code, _, errOut := runCLI(t, "run")
	if code != ExitConfig || !strings.Contains(errOut, "git does not ignore .driftgate/") || !strings.Contains(errOut, filepath.Join(root, ".gitignore")) {
		t.Fatalf("run: %d %s", code, errOut)
	}
	filepath.WalkDir(filepath.Join(root, ".driftgate", "store"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("written into the store: %s", path)
		}
		return nil
	})
	if code, _, errOut := runCLI(t, "key", "create"); code != ExitConfig || !strings.Contains(errOut, "git does not ignore") {
		t.Fatalf("key create: %d %s", code, errOut)
	}
}

// TestLocalCompareRefusedWhenGitDoesNotIgnore: compare writes the last
// comparison into the store, so it too needs git to ignore .driftgate/.
func TestLocalCompareRefusedWhenGitDoesNotIgnore(t *testing.T) {
	root := localDemo(t)
	t.Setenv(secret.EnvKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Chdir(root)
	// Captures made while the project was not in git yet.
	if err := os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-off")); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"c1", "c2"} {
		if code, out, errOut := runCLI(t, "capture", tag); code != ExitClean {
			t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
		}
	}
	if err := os.Rename(filepath.Join(root, ".git-off"), filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "compare", "c2", "--against", "c1")
	if code != ExitConfig || !strings.Contains(errOut, "git does not ignore .driftgate/") {
		t.Fatalf("compare: %d %s", code, errOut)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".driftgate", "store", "last-compare-*")); len(left) > 0 {
		t.Errorf("written: %v", left)
	}
	write(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	if code, out, errOut := runCLI(t, "compare", "c2", "--against", "c1"); code != ExitClean {
		t.Fatalf("compare once git ignores the folder: %d\n%s\n%s", code, out, errOut)
	}
}

// TestLocalLineEndingsSayNoGit: with baselines: local git holds no
// baseline, so the line-ending messages name no commit and no git command.
func TestLocalLineEndingsSayNoGit(t *testing.T) {
	root := localDemo(t)
	write(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	t.Setenv(secret.EnvKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Chdir(root)
	if code, out, errOut := runCLI(t, "run"); code != ExitClean {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	baselines := filepath.Join(root, ".driftgate", "baselines")
	os.Remove(filepath.Join(baselines, ".gitattributes"))
	news := filepath.Join(baselines, "local", "responses", "news.json")
	raw, _ := os.ReadFile(news)
	write(t, news, strings.ReplaceAll(string(raw), "\n", "\r\n"))
	code, out, _ := runCLI(t, "verify")
	if code != ExitDrift || !strings.Contains(out, "wrote ") || !strings.Contains(out, "CRLF") || strings.Contains(out, "commit") || strings.Contains(out, "git rm") || strings.Contains(out, "git checkout") {
		t.Fatalf("verify: %d\n%s", code, out)
	}
}
