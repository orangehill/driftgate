package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/secret"
)

// gitRepo runs git in a test repository, with no user or system
// configuration, so signing or hooks of the machine do not interfere.
type gitRepo struct {
	t   *testing.T
	dir string
}

func (g gitRepo) run(args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files (an empty content removes the file) and commits them.
func (g gitRepo) commit(msg string, files map[string]string) string {
	g.t.Helper()
	for name, content := range files {
		path := filepath.Join(g.dir, name)
		if content == "" {
			os.Remove(path)
			continue
		}
		write(g.t, path, content)
	}
	g.run("add", "-A")
	g.run("commit", "-q", "-m", msg)
	return g.run("rev-parse", "HEAD")
}

const bisectCatalog = `{"version":1,"requests":[
 {"id":"news","method":"GET","path":"/api/news","storage":"public"},
 {"id":"me","method":"GET","path":"/api/me","storage":"public"},
 {"id":"status","method":"GET","path":"/api/status","storage":"public"}]}`

// bisectApp serves the application of whichever checkout the control file
// names: its responses are the files under app/ there, and its version
// header comes from app/version. The checkout hook writes the control file,
// as a real hook would restart the application from the worktree.
func bisectApp(t *testing.T, control string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dir, err := os.ReadFile(control)
		if err != nil || len(dir) == 0 {
			http.Error(w, "no application", http.StatusServiceUnavailable)
			return
		}
		app := filepath.Join(string(dir), "app")
		if r.URL.Path == "/ready" {
			if _, err := os.Stat(app); err != nil {
				http.Error(w, "starting", http.StatusServiceUnavailable)
			}
			return
		}
		body, err := os.ReadFile(filepath.Join(app, strings.TrimPrefix(r.URL.Path, "/api/")+".json"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		version, _ := os.ReadFile(filepath.Join(app, "version"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-App-Version", strings.TrimSpace(string(version)))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// bisectEnv makes an empty repository, shop, in a temporary folder, with git
// and the store set up for a test.
func bisectEnv(t *testing.T) (string, gitRepo) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Demo Developer")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "dev@example.com")
	}
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	// Bisect's worktrees go to a temporary folder of this test only, so a
	// check for what is left there does not see another test run's.
	t.Setenv("TMPDIR", t.TempDir())
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	root, _ := filepath.EvalSymlinks(t.TempDir())
	g := gitRepo{t, filepath.Join(root, "shop")}
	os.MkdirAll(g.dir, 0o755)
	g.run("init", "-q", "-b", "main")
	return root, g
}

// TestBisect is the done criterion of M5b: three deliberate changes in
// different commits, with neutral commits between them and a commit that
// cannot be built; bisect names the exact commit of each drift group and
// leaves the user's checkout as it was.
func TestBisect(t *testing.T) {
	root, g := bisectEnv(t)
	dir := g.dir
	control := filepath.Join(root, "control")
	hookLog := filepath.Join(root, "hook.log")
	write(t, control, dir)
	srv := bisectApp(t, control)

	cfg := filepath.Join(dir, "driftgate.yaml")
	files := map[string]string{
		"driftgate.yaml": fmt.Sprintf(`version: 1
project: shop
mode: evolve
target:
  base_url: %s
bisect:
  # A real hook builds and restarts the application from the worktree.
  checkout: ["sh", "-c", "test -f driftgate.yaml && echo \"$DRIFTGATE_BISECT_COMMIT $DRIFTGATE_BISECT_PROJECT\" >> %s && test ! -e \"$DRIFTGATE_BISECT_DIR/app/broken\" && printf '%%s' \"$DRIFTGATE_BISECT_DIR\" > %s"]
  ready: ["curl", "-sf", "%s/ready"]
  finish: ["sh", "-c", "printf '%%s' \"$DRIFTGATE_BISECT_REPO\" > %s"]
  timeout: 30s
  ready_timeout: 10s
`, srv.URL, hookLog, control, srv.URL, control),
		"driftgate/catalog.json":   bisectCatalog,
		"driftgate/allowlist.yaml": initAllowlist,
		"app/news.json":            `{"entries":[{"id":1,"title":"Opening"},{"id":2,"title":"Prices"}]}`,
		"app/me.json":              `{"user":{"id":7,"name":"Ana"}}`,
		"app/status.json":          `{"ok":true}`,
		"app/version":              "1.0\n",
		"README.md":                "shop\n",
	}
	for name, content := range files {
		write(t, filepath.Join(dir, name), content)
	}
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	good := g.commit("Shop with its first baseline", nil)
	g.commit("Describe the shop", map[string]string{"README.md": "shop\n\nsells things\n"})
	newsIDs := g.commit("News ids as strings", map[string]string{"app/news.json": `{"entries":[{"id":"1","title":"Opening"},{"id":"2","title":"Prices"}]}`})
	g.commit("Mention the news", map[string]string{"README.md": "shop\n\nsells things, has news\n"})
	version := g.commit("Version 2.0", map[string]string{"app/version": "2.0\n"})
	g.commit("Notes for release", map[string]string{"NOTES.md": "2.0\n"})
	broken := g.commit("Half of a refactoring", map[string]string{"app/broken": "x\n"})
	g.commit("Finish the refactoring", map[string]string{"app/broken": ""})
	role := g.commit("Users have a role", map[string]string{"app/me.json": `{"user":{"id":7,"name":"Ana","role":"admin"}}`})
	g.commit("Tidy the readme", map[string]string{"README.md": "shop\n"})

	// The bad state is the checkout itself; it drifts in three groups.
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitDrift {
		t.Fatalf("run at the bad commit: %d\n%s\n%s", code, out, errOut)
	}
	// Work in progress the bisect must not touch.
	write(t, filepath.Join(dir, "README.md"), "shop, being edited\n")
	write(t, filepath.Join(dir, "draft.txt"), "untracked\n")
	status, head, branch := g.run("status", "--porcelain"), g.run("rev-parse", "HEAD"), g.run("symbolic-ref", "HEAD")

	code, out, errOut := runCLI(t, "bisect", good[:10], "HEAD", "--config", cfg, "--format", "json")
	if code != ExitClean {
		t.Fatalf("bisect: %d\n%s\n%s", code, out, errOut)
	}
	var res bisectResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("bisect: %d commits, tested %v, skipped %v, %.2fs\n%s", res.Commits, res.Tested, res.Skipped, res.Seconds, errOut)
	if len(res.Groups) != 3 {
		t.Fatalf("groups: %+v", res.Groups)
	}
	for _, gr := range res.Groups {
		var want string
		switch {
		case strings.Contains(gr.Signature, "x-app-version"):
			want = version
		case strings.Contains(gr.Signature, "entries"):
			want = newsIDs
		case strings.Contains(gr.Signature, "role"):
			want = role
		default:
			t.Fatalf("unexpected group %+v", gr)
		}
		if gr.Status != "found" || gr.Commit == nil || gr.Commit.Hash != want || len(gr.Drifts) == 0 || len(gr.Unsteady) > 0 {
			t.Errorf("group [%d] %s: %+v, want commit %.7s", gr.N, gr.Signature, gr, want)
		}
	}
	if len(res.Skipped) != 1 || !strings.HasPrefix(broken, res.Skipped[0].Commit) || !strings.Contains(res.Skipped[0].Reason, "checkout") {
		t.Errorf("skipped %+v, want %.7s", res.Skipped, broken)
	}
	if res.Commits != 9 || len(res.Tested) > 10 {
		t.Errorf("%d commits, tested %v", res.Commits, res.Tested)
	}

	// The user's checkout, branch and baseline are as they were; the
	// worktree and the temporary captures are gone; finish ran.
	if s := g.run("status", "--porcelain"); s != status {
		t.Errorf("working tree changed:\n%s\nwas\n%s", s, status)
	}
	if h, b := g.run("rev-parse", "HEAD"), g.run("symbolic-ref", "HEAD"); h != head || b != branch {
		t.Errorf("HEAD %s on %s, was %s on %s", h, b, head, branch)
	}
	if wts := g.run("worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Errorf("worktrees left:\n%s", wts)
	}
	if d := g.run("diff", "HEAD", "--stat", "--", "driftgate"); d != "" {
		t.Errorf("baseline changed: %s", d)
	}
	if code, out, errOut := runCLI(t, "verify", "--config", cfg); code != ExitClean {
		t.Errorf("verify: %d\n%s\n%s", code, out, errOut)
	}
	if left, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", "bisect-*")); len(left) > 0 {
		t.Errorf("captures left: %v", left)
	}
	if c, _ := os.ReadFile(control); string(c) != dir {
		t.Errorf("finish did not run: the application is on %s", c)
	}
	// The hook ran from the project folder, got the commit, and the
	// project folder inside the worktree, never the user's checkout.
	log, _ := os.ReadFile(hookLog)
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		commit, project, _ := strings.Cut(line, " ")
		if len(commit) != 40 || project == dir || filepath.Base(project) != "shop" {
			t.Errorf("hook got %q", line)
		}
	}
	if !strings.Contains(string(log), broken+" ") {
		t.Errorf("the broken commit was not tried:\n%s", log)
	}

	// One group, in text.
	n := 0
	for _, gr := range res.Groups {
		if gr.Commit.Hash == role {
			n = gr.N
		}
	}
	code, out, errOut = runCLI(t, "bisect", good, "HEAD", "--config", cfg, "--group", fmt.Sprint(n))
	if code != ExitClean || !strings.Contains(out, role[:7]+"  Users have a role") || strings.Contains(out, version[:7]+"  ") {
		t.Errorf("bisect --group %d: %d\n%s\n%s", n, code, out, errOut)
	}

	// A good commit that already drifts is refused, as is a bad one that
	// does not.
	code, _, errOut = runCLI(t, "bisect", newsIDs, "HEAD", "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, "already at the good commit") {
		t.Errorf("good commit with drift: %d %s", code, errOut)
	}
	code, _, errOut = runCLI(t, "bisect", good, newsIDs, "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, "does not drift at the bad commit") {
		t.Errorf("bad commit without drift: %d %s", code, errOut)
	}
	code, _, errOut = runCLI(t, "bisect", "HEAD", good, "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, "not an ancestor") {
		t.Errorf("reversed: %d %s", code, errOut)
	}
	if s := g.run("status", "--porcelain"); s != status {
		t.Errorf("working tree changed by a refused bisect:\n%s", s)
	}
	if wts := g.run("worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Errorf("worktrees left by a refused bisect:\n%s", wts)
	}

	// Without a comparison, --only names the requests and the groups are
	// those found at the bad commit.
	_, last := lastCompare(t)
	os.Remove(last)
	code, out, errOut = runCLI(t, "bisect", good, "HEAD", "--only", "me", "--config", cfg, "--format", "json")
	res = bisectResult{}
	if err := json.Unmarshal([]byte(out), &res); code != ExitClean || err != nil || len(res.Groups) != 2 {
		t.Fatalf("bisect --only me: %d %v\n%s\n%s", code, err, out, errOut)
	}
	for _, gr := range res.Groups {
		if want := map[bool]string{true: version, false: role}[strings.Contains(gr.Signature, "x-app-version")]; gr.Commit == nil || gr.Commit.Hash != want {
			t.Errorf("--only me: group %s at %+v, want %.7s", gr.Signature, gr.Commit, want)
		}
	}
}

// TestBisectInterrupted stops a bisect with Ctrl-C while the checkout hook
// runs: the hook is stopped, and the worktree and the capture are removed.
func TestBisectInterrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no interrupt signal to send")
	}
	root, g := bisectEnv(t)
	control := filepath.Join(root, "control")
	started := filepath.Join(root, "started")
	write(t, control, g.dir)
	srv := bisectApp(t, control)
	cfg := filepath.Join(g.dir, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf(`version: 1
project: shop
target:
  base_url: %s
bisect:
  checkout: ["sh", "-c", "touch %s && exec sleep 30"]
`, srv.URL, started))
	write(t, filepath.Join(g.dir, "driftgate", "catalog.json"), bisectCatalog)
	write(t, filepath.Join(g.dir, "driftgate", "allowlist.yaml"), initAllowlist)
	write(t, filepath.Join(g.dir, "app", "status.json"), `{"ok":true}`)
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	good := g.commit("Start", nil)
	g.commit("Change", map[string]string{"app/status.json": `{"ok":false}`})

	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(started); err == nil {
				p, _ := os.FindProcess(os.Getpid())
				p.Signal(os.Interrupt)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	began := time.Now()
	code, _, errOut := runCLI(t, "bisect", good, "HEAD", "--only", "status", "--config", cfg)
	if code != ExitEnv || !strings.Contains(errOut, "interrupted") {
		t.Errorf("interrupted bisect: %d %s", code, errOut)
	}
	if d := time.Since(began); d > 15*time.Second {
		t.Errorf("the hook was not stopped: %s", d)
	}
	if wts := g.run("worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Errorf("worktrees left:\n%s", wts)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "driftgate-bisect-*", "shop")); len(left) > 0 {
		t.Errorf("worktree folders left: %v", left)
	}
}

func TestBisectNeedsCheckout(t *testing.T) {
	p := newDemoProject(t, "")
	code, _, errOut := runCLI(t, "bisect", "HEAD~1", "HEAD", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "bisect.checkout") {
		t.Errorf("%d %s", code, errOut)
	}
}

// TestBisectRefusesSQLite: the application of a bisect runs from the
// worktree with that worktree's SQLite file, which DriftGate does not
// restore.
func TestBisectRefusesSQLite(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, `version: 1
project: lite
target:
  base_url: http://127.0.0.1:1
state:
  db:
    driver: sqlite
    working: database/database.sqlite
bisect:
  checkout: ["true"]
`)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), demoCatalog)
	code, _, errOut := runCLI(t, "bisect", "HEAD~1", "HEAD", "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, "bisect does not support SQLite state") || !strings.Contains(errOut, "database/database.sqlite") {
		t.Errorf("%d %s", code, errOut)
	}
}
