package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
)

const demoCatalog = `{"version":1,"requests":[
 {"id":"news","method":"GET","path":"/api/news","storage":"public"},
 {"id":"news-missing","method":"GET","path":"/api/news/{id}","params":{"id":"999"},"storage":"public"},
 {"id":"me","method":"GET","path":"/api/me","identity":"admin","storage":"private"},
 {"id":"me-anonymous","method":"GET","path":"/api/me","storage":"public"},
 {"id":"slow","method":"GET","path":"/api/slow","storage":"public","observe":true},
 {"id":"news-item","method":"GET","path":"/api/news/12","mutates":true,"storage":"public"},
 {"id":"login","method":"POST","path":"/api/auth/login","body":{"email":"admin@demo.test","password":"x"},"mutates":true,"storage":"private"},
 {"id":"faq-create","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"}]}`

const demoAllowlist = `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
  - name: set-cookie
    format: 'session=[x0]{40}; Path=/; HttpOnly'
    reason: New session on every login.
body:
  - request: login
    path: $.token
    format: '[0-9a-f]+\.[0-9a-f]+\.[0-9a-f]+'
    reason: New token on every login.
  - request: faq-create
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Time of creation.
`

type demoProject struct {
	cfg string
	api *demo.Server
	url string
}

func newDemoProject(t *testing.T, extra string) *demoProject {
	t.Helper()
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf(`version: 1
project: demo
mode: freeze
level: values
target:
  base_url: %s
  concurrency: 4
identities:
  admin: { env: DG_DEMO_ADMIN }
state:
  db:
    restore: ["curl", "-sf", "-X", "POST", "%s/admin/reset"]
guards:
  - name: always
    command: ["true"]
%s`, srv.URL, srv.URL, extra))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), demoCatalog)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), demoAllowlist)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DG_DEMO_ADMIN", demo.AdminToken)
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	return &demoProject{cfg: cfg, api: api, url: srv.URL}
}

func compareJSON(t *testing.T, cfg, tag, against string) (int, *report.Report) {
	t.Helper()
	code, out, errOut := runCLI(t, "compare", tag, "--against", against, "--config", cfg, "--format", "json")
	var r report.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("compare output: %v\n%s\n%s", err, out, errOut)
	}
	return code, &r
}

func TestCaptureTwiceIsIdentical(t *testing.T) {
	p := newDemoProject(t, "")
	for _, tag := range []string{"c1", "c2"} {
		if code, out, errOut := runCLI(t, "capture", tag, "--config", p.cfg); code != 0 {
			t.Fatalf("capture %s: %d\n%s\n%s", tag, code, out, errOut)
		}
	}
	code, r := compareJSON(t, p.cfg, "c2", "c1")
	if code != ExitClean {
		t.Fatalf("two captures differ: %+v", r.Results)
	}
	if r.Observed != 1 || r.Compared != 7 {
		t.Fatalf("counts: compared %d observed %d", r.Compared, r.Observed)
	}
}

func TestCaptureFindsPlantedDrift(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "capture", "v1", "--config", p.cfg); code != 0 {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "capture", "v2", "--config", p.cfg); code != 0 {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	code, r := compareJSON(t, p.cfg, "v2", "v1")
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	found := map[string]bool{}
	for _, res := range r.Results {
		for _, d := range res.Drifts {
			found[d.Request+" "+d.Kind+" "+d.Where] = true
		}
	}
	for _, want := range []string{
		"news type $.entries[0].id",
		"news value $.entries[0].published_at",
		"news key-extra $.entries[0].meta",
		"news header-extra header:x-powered-by",
		"news-item type $.entry.id",
	} {
		if !found[want] {
			t.Errorf("missing %q", want)
		}
	}
}

func TestCapturePersonalDataInPublicBody(t *testing.T) {
	p := newDemoProject(t, "")
	cat := strings.Replace(demoCatalog, `"requests":[`, `"requests":[{"id":"contact","method":"GET","path":"/api/contact","storage":"public"},`, 1)
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"), cat)
	code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "contact: email") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(errOut, "mila.milic@example.com") {
		t.Fatal("report shows the full personal value")
	}
	store := os.Getenv("DRIFTGATE_STORE")
	filepath.Walk(store, func(path string, info os.FileInfo, _ error) error {
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "mila.milic") {
			t.Errorf("personal value stored in plain text: %s", path)
		}
		return nil
	})
}

func TestCaptureRefusals(t *testing.T) {
	// Host outside the allow-list: every request fails, exit 3.
	p := newDemoProject(t, "")
	raw, _ := os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), "  concurrency: 4", "  concurrency: 4\n  allow_hosts: [10.0.0.0/8]", 1))
	if code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--only", "news"); code != ExitEnv || !strings.Contains(errOut, "failed") {
		t.Fatalf("outside allow-list: exit %d %s", code, errOut)
	}

	// Requests that change data without a restore: configuration error.
	p = newDemoProject(t, "")
	raw, _ = os.ReadFile(p.cfg)
	cfg := string(raw)
	cfg = cfg[:strings.Index(cfg, "state:")] + cfg[strings.Index(cfg, "guards:"):]
	write(t, p.cfg, cfg)
	if code, _, _ := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitConfig {
		t.Fatalf("no restore: exit %d", code)
	}

	// So does a request whose method changes data although the catalog
	// does not say mutates (a hand-written or imported entry), and outside
	// allow_hosts it needs --allow-remote-mutations.
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"),
		`{"version":1,"requests":[{"id":"gone","method":"delete","path":"/api/news/1","storage":"public"},
		{"id":"make","method":"POST","path":"/api/news","mutates":true,"storage":"public"}]}`)
	// Both messages name the requests, and those that change data only by
	// their method, with what to write for one that only reads.
	named := func(errOut string) bool {
		return strings.Contains(errOut, "(gone, make)") && strings.Contains(errOut, `gone change data only by their method`) &&
			strings.Contains(errOut, `"mutates": false`)
	}
	if code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "change data") || !named(errOut) || !strings.Contains(errOut, "configure state:") {
		t.Fatalf("DELETE without mutates, no restore: exit %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--allow-mutating-without-restore", "--allow-remote"); code != ExitConfig || !strings.Contains(errOut, "--allow-remote-mutations") || !named(errOut) {
		t.Fatalf("DELETE without mutates, remote: exit %d %s", code, errOut)
	}

	// A repeated request to a target outside allow_hosts is a burst: it
	// needs --allow-remote-mutations like a request that changes data.
	p = newDemoProject(t, "")
	raw, _ = os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), "  concurrency: 4", "  concurrency: 4\n  allow_hosts: [10.0.0.0/8]", 1))
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"),
		`{"version":1,"requests":[{"id":"news","method":"GET","path":"/api/news","repeat":5,"storage":"public"}]}`)
	if code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--allow-remote"); code != ExitConfig || !strings.Contains(errOut, "--allow-remote-mutations") ||
		!strings.Contains(errOut, "(news)") || strings.Contains(errOut, "only by their method") {
		t.Fatalf("remote repeat: exit %d %s", code, errOut)
	}

	// A failing guard stops before any request is sent.
	p = newDemoProject(t, "")
	raw, _ = os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), `["true"]`, `["false"]`, 1))
	if code, out, _ := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitEnv || strings.Contains(out, "faq-create") ||
		regexp.MustCompile(`(?m)^\d{3} `).MatchString(out) {
		t.Fatalf("failing guard: exit %d\n%s", code, out)
	}

	// A guard checks what restore and prepare set up (prepare writes the
	// file the guard reads): it runs after them.
	p = newDemoProject(t, "")
	raw, _ = os.ReadFile(p.cfg)
	env := filepath.Join(t.TempDir(), "env")
	conf := strings.Replace(string(raw), `["true"]`, `["grep", "-q", "MAIL_MAILER=log", "`+env+`"]`, 1)
	conf = strings.Replace(conf, "guards:", "prepare:\n  - [\"sh\", \"-c\", \"echo MAIL_MAILER=log > '"+env+"'\"]\nguards:", 1)
	write(t, p.cfg, conf)
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitClean {
		t.Fatalf("a guard of what prepare sets up: exit %d\n%s\n%s", code, out, errOut)
	}

	// Missing token for an identity.
	p = newDemoProject(t, "")
	t.Setenv("DG_DEMO_ADMIN", "")
	if code, _, errOut := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "admin") {
		t.Fatalf("missing token: exit %d %s", code, errOut)
	}
}

func TestInit(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	if code, out, errOut := runCLI(t, "init", "--config", cfg, "--project", "shop"); code != 0 || !strings.Contains(out, "CODEOWNERS") {
		t.Fatalf("init: %d %s %s", code, out, errOut)
	}
	if code, _, _ := runCLI(t, "init", "--config", cfg); code != ExitConfig {
		t.Fatal("init over an existing configuration")
	}
	t.Setenv("DRIFTGATE_STORE", t.TempDir())
	// An empty catalog would make an empty baseline that passes everything.
	if code, _, errOut := runCLI(t, "capture", "x", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "no requests") {
		t.Fatalf("capture with an empty catalog: %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "run", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "no requests") {
		t.Fatalf("run with an empty catalog: %d %s", code, errOut)
	}
}

// TestInterruptedCaptureIsIncomplete: a capture that stopped half way (here
// at a guard that passed at the start and fails when checked again, after
// the read-only requests) is not compared as one
// whose other requests are gone (drift, exit 1), but refused as incomplete
// (exit 3).
func TestInterruptedCaptureIsIncomplete(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(p.cfg)
	flag := filepath.Join(t.TempDir(), "checked")
	write(t, p.cfg, strings.Replace(string(raw), `["true"]`, `["sh", "-c", "test ! -e '`+flag+`' && touch '`+flag+`'"]`, 1))
	if code, out, errOut := runCLI(t, "capture", "c2", "--config", p.cfg); code != ExitEnv || !strings.Contains(out, "news") {
		t.Fatalf("failing guard: exit %d %s\n%s", code, errOut, out)
	}
	code, _, errOut := runCLI(t, "compare", "c2", "--against", "c1", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(errOut, "incomplete capture") {
		t.Fatalf("compare with the interrupted capture: exit %d %s", code, errOut)
	}
	// Captured again, it is complete.
	write(t, p.cfg, string(raw))
	if code, out, errOut := runCLI(t, "capture", "c2", "--force", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture again: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, "compare", "c2", "--against", "c1", "--config", p.cfg); code != ExitClean {
		t.Fatalf("compare: exit %d %s", code, errOut)
	}
}

// TestCaseOnlyIDsRefused: two IDs that differ only in case would share one
// record file on macOS and Windows, and the second response would hide a
// change of the first; the catalog is refused before anything is sent.
func TestCaseOnlyIDsRefused(t *testing.T) {
	p := newDemoProject(t, "")
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"),
		`{"version":1,"requests":[{"id":"get-X","method":"GET","path":"/api/news","storage":"public"},{"id":"get-x","method":"GET","path":"/api/faq","storage":"public"}]}`)
	if code, _, errOut := runCLI(t, "capture", "a", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "differ only in case") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

// TestByMethodNote: requests that change data only because of their
// method (no mutates, as in a catalog of an earlier release) are
// named once per command, with what to write; the exit code stays.
func TestByMethodNote(t *testing.T) {
	p := newDemoProject(t, "")
	catalogPath := filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json")
	write(t, catalogPath, `{"version":1,"requests":[{"id":"news","method":"GET","path":"/api/news","storage":"public"},
		{"id":"login","method":"POST","path":"/api/auth/login","body":{"user":"admin"},"storage":"private"}]}`)
	code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--repeat", "2")
	if code != ExitClean || strings.Count(out, "only by their method") != 1 || !strings.Contains(out, "1 request changes data only by their method, as the catalog does not say mutates: login") ||
		!strings.Contains(out, "mutates: false") || !strings.Contains(out, "under requests: in driftgate.yaml") || !strings.Contains(out, "--restore once") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	write(t, catalogPath, `{"version":1,"requests":[{"id":"news","method":"GET","path":"/api/news","storage":"public"},
		{"id":"login","method":"POST","path":"/api/auth/login","body":{"user":"admin"},"mutates":false,"storage":"private"}]}`)
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--force"); code != ExitClean || strings.Contains(out, "only by their method") {
		t.Fatalf("with mutates: exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestLiteralIDWarning: a request that changes data with a fixed id in
// its path is warned of, one line per request; one whose id is a
// parameter, or that only reads, is not.
func TestLiteralIDWarning(t *testing.T) {
	p := newDemoProject(t, "")
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"), `{"version":1,"requests":[
		{"id":"get-user","method":"GET","path":"/api/users/17","storage":"public"},
		{"id":"put-user","method":"PUT","path":"/api/users/17?x=5","body":{},"mutates":true,"storage":"public"},
		{"id":"put-param","method":"PUT","path":"/api/users/{id}","params":{"id":"17"},"body":{},"mutates":true,"storage":"public"},
		{"id":"search","method":"POST","path":"/api/search/2","body":{},"mutates":false,"storage":"public"}]}`)
	code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg)
	if code == ExitConfig || code == ExitEnv || strings.Count(out, "fixed id") != 1 ||
		!strings.Contains(out, "warning: put-user (PUT /api/users/17?x=5) changes data with the fixed id 17 in its path") || !strings.Contains(out, "params_from") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestRepeatedRunsInOtherSeconds: a run of a repeated capture writes at
// least a second after the run before it ended, so a timestamp of the
// second the application wrote (updated_at) differs between the runs and
// the comparison finds it, however short the runs are.
func TestRepeatedRunsInOtherSeconds(t *testing.T) {
	defer func(d time.Duration) { runGap = d }(runGap)
	runGap = time.Second
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		mu.Lock()
		at = append(at, now)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":1,"updated_at":%q}`, now.UTC().Format("2006-01-02 15:04:05"))
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[{"id":"item","method":"GET","path":"/item","storage":"public"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	code, out, errOut := runCLI(t, "capture", "c", "--config", cfg, "--repeat", "2")
	if code == ExitClean || !strings.Contains(out, "updated_at") {
		t.Fatalf("the timestamp is not found volatile: exit %d\n%s\n%s", code, out, errOut)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 2 || at[1].Sub(at[0]) < time.Second {
		t.Fatalf("requests at %v: the second run began within a second", at)
	}
}

// TestMarkWriters: a generated request that changed data is marked under
// requests: by its route, once per route; one written by hand in the
// catalog.
func TestMarkWriters(t *testing.T) {
	cat := &catalog.Catalog{Requests: []catalog.Request{
		{ID: "get-api-news-id", Method: "GET", Path: "/api/news/{id}", Source: &catalog.Source{Route: "GET /api/news/{id}", Template: "ok"}},
		{ID: "get-api-news-id-json", Method: "GET", Path: "/api/news/{id}", Source: &catalog.Source{Route: "GET /api/news/{id}", Template: "json"}},
		{ID: "hand", Method: "GET", Path: "/api/stats"},
	}}
	var out strings.Builder
	markWriters(&out, []string{"get-api-news-id", "get-api-news-id-json", "hand"}, cat)
	want := "mark them as changing data, so they run one at a time on a known state:\n" +
		"  in driftgate.yaml, then driftgate generate --write:\n  requests:\n    - match: \"GET /api/news/{id}\"\n      mutates: true\n" +
		"  in the catalog, written by hand: \"mutates\": true on hand\n"
	if out.String() != want {
		t.Errorf("advice:\n%s\nwant\n%s", out.String(), want)
	}
}

// TestReadOnlyCapture: --read-only sends only what leaves data unchanged,
// also without state:; a comparison of such a capture skips the others,
// lists them as not covered, and calls none of their rules stale.
func TestReadOnlyCapture(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	srv := httptest.NewServer(noDate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1,"token":"abc"}`)
	})))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
		{"id":"news","method":"GET","path":"/news","storage":"public"},
		{"id":"search","method":"POST","path":"/search","body":{},"mutates":false,"storage":"public"},
		{"id":"post-user","method":"POST","path":"/users","body":{},"mutates":true,"storage":"public"},
		{"id":"seq-login","method":"POST","path":"/login","body":{},"mutates":true,"storage":"public"},
		{"id":"seq-me","method":"GET","path":"/me","storage":"public"},
		{"id":"cmd","command":["true"],"storage":"public"},
		{"id":"cmd-ro","command":["true"],"mutates":false,"storage":"public"}],
		"sequences":[{"id":"s","requests":["seq-login","seq-me"]}]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nbody:\n  - request: post-user\n    path: $.token\n    format: '[a-z]+'\n    reason: new token\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))

	if code, out, errOut := runCLI(t, "capture", "full", "--config", cfg, "--allow-mutating-without-restore"); code != ExitClean {
		t.Fatalf("full: exit %d\n%s\n%s", code, out, errOut)
	}
	mu.Lock()
	writes = 0
	mu.Unlock()
	code, out, errOut := runCLI(t, "capture", "ro", "--config", cfg, "--read-only")
	if code != ExitClean || !strings.Contains(out, "--read-only: 4 requests that change data") {
		t.Fatalf("read-only: exit %d\n%s\n%s", code, out, errOut)
	}
	mu.Lock()
	sent := writes
	mu.Unlock()
	if sent != 1 { // the search, marked mutates: false
		t.Fatalf("%d requests other than GET sent", sent)
	}
	code, r := compareJSON(t, cfg, "ro", "capture:full")
	got := map[string]string{}
	for _, res := range r.Results {
		got[res.ID] = res.Outcome + " " + res.Note
	}
	want := map[string]string{"news": "pass ", "search": "pass ", "cmd-ro": "pass ", "post-user": "skipped " + report.NoteNotSent,
		"seq-login": "skipped " + report.NoteNotSent, "seq-me": "skipped " + report.NoteNotSent, "cmd": "skipped " + report.NoteNotSent}
	if code != ExitClean || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("exit %d: %v", code, got)
	}
	// Every request the result did not compare is named under "Not
	// covered", with what it is and why it was skipped, in every format:
	// the read in a sequence too (seq-me), and the command step apart.
	notSent := "skipped (" + report.NoteNotSent + "): "
	var named []string
	for _, s := range r.NotCovered.Skipped {
		named = append(named, fmt.Sprintf("%s/%s:%v", s.What, s.Reason, s.Names))
	}
	if want := []string{"mutating/" + report.NoteNotSent + ":[post-user seq-login]", "read/" + report.NoteNotSent + ":[seq-me]", "command/" + report.NoteNotSent + ":[cmd]"}; !reflect.DeepEqual(named, want) {
		t.Errorf("json skipped %v, want %v", named, want)
	}
	for _, f := range []string{"text", "markdown", "junit"} {
		_, out, _ := runCLI(t, "compare", "ro", "--against", "capture:full", "--config", cfg, "--format", f)
		for _, w := range []string{"2 requests that change data " + notSent + "post-user, seq-login", "1 read " + notSent + "seq-me", "1 command step " + notSent + "cmd"} {
			if !strings.Contains(out, w) {
				t.Errorf("%s lacks %q:\n%s", f, w, out)
			}
		}
	}
	if len(r.UnusedRules) != 0 {
		t.Errorf("a rule of a request not sent is called stale: %+v", r.UnusedRules)
	}
	// What the capture left out is recorded in it: a catalog changed
	// since (post-user now says mutates: false) does not change which
	// requests it did not send.
	catPath := filepath.Join(root, "driftgate", "catalog.json")
	raw, _ := os.ReadFile(catPath)
	write(t, catPath, strings.Replace(string(raw), `"path":"/users","body":{},"mutates":true`, `"path":"/users","body":{},"mutates":false`, 1))
	if code, r := compareJSON(t, cfg, "ro", "capture:full"); code != ExitClean || r.Skipped != 4 {
		t.Fatalf("catalog changed since: exit %d, %d skipped: %+v", code, r.Skipped, r.Results)
	}
	write(t, catPath, string(raw))
	// A capture that sent everything compared with a read-only one: the
	// requests that one left out were never checked, so the result is
	// incomplete (not new requests to accept).
	if code, r := compareJSON(t, cfg, "full", "capture:ro"); code != ExitEnv || len(r.ReadOnlyBaseline) != 4 || len(r.NoBaseline) != 0 {
		t.Errorf("against a read-only capture: exit %d, %+v", code, r.ReadOnlyBaseline)
	}
	code, out, _ = runCLI(t, "compare", "full", "--against", "capture:ro", "--config", cfg)
	if code != ExitEnv || !strings.Contains(out, "incomplete: 4 requests not checked: capture:ro was captured with --read-only") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	// Nor does a read-only capture become a baseline.
	if code, out, errOut := runCLI(t, "run", "--config", cfg, "--allow-mutating-without-restore"); code != ExitClean {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "baselines", "rebase", "local", "--from", "ro", "--reason", "x", "--allow-freeze", "--config", cfg)
	if code != ExitConfig || !strings.Contains(errOut, "made with --read-only") {
		t.Errorf("rebase from a read-only capture: exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestRunReadOnlyNeedsBaseline: the first run makes the baseline from
// every request; --read-only there would leave the requests that change
// data unchecked by every later run.
func TestRunReadOnlyNeedsBaseline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1}`)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\nstate:\n  db:\n    restore: [\"true\"]\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
		{"id":"news","method":"GET","path":"/news","storage":"public"},
		{"id":"post-user","method":"POST","path":"/users","body":{},"mutates":true,"storage":"public"}]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.*'\n    reason: clock\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	code, out, errOut := runCLI(t, "run", "--config", cfg, "--read-only")
	if code != ExitConfig || !strings.Contains(errOut, "makes it from every request") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(root, "driftgate", "baselines", "local")
	if _, err := os.Stat(base); err == nil {
		t.Fatal("a baseline was made")
	}
}

// TestReadOnlyCalibrate: the capture at the measured concurrency of
// --calibrate keeps --read-only: no request that changes data is sent.
func TestReadOnlyCalibrate(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes++
			mu.Unlock()
		} else {
			time.Sleep(15 * time.Millisecond) // so more at once is faster
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1}`)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n  concurrency: auto\nstate:\n  db:\n    restore: [\"true\"]\n", srv.URL))
	var reqs []string
	for i := 0; i < 20; i++ {
		reqs = append(reqs, fmt.Sprintf(`{"id":"g%d","method":"GET","path":"/g%d","storage":"public"}`, i, i))
	}
	reqs = append(reqs, `{"id":"post-user","method":"POST","path":"/users","body":{},"mutates":true,"storage":"public"}`)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[`+strings.Join(reqs, ",")+`]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.*'\n    reason: clock\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	code, out, errOut := runCLI(t, "capture", "ro", "--config", cfg, "--read-only", "--calibrate")
	mu.Lock()
	defer mu.Unlock()
	if writes > 0 || !strings.Contains(out, "capture at the measured concurrency") {
		t.Fatalf("exit %d, %d requests that change data sent\n%s\n%s", code, writes, out, errOut)
	}
}

// TestReadOnlyProjectCommands: --read-only selects requests only, so a
// prepare: (which may write) is refused, and a tokens.command, which runs
// as configured, is warned of.
func TestReadOnlyProjectCommands(t *testing.T) {
	srv := httptest.NewServer(noDate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1}`)
	})))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	base := fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n", srv.URL)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[{"id":"news","method":"GET","path":"/news","storage":"public"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))

	write(t, cfg, base+"prepare:\n  - [\"true\"]\n")
	if code, out, errOut := runCLI(t, "capture", "a", "--config", cfg, "--read-only"); code != ExitConfig || !strings.Contains(errOut, "prepare: runs after every restore and may write") {
		t.Fatalf("prepare: exit %d\n%s\n%s", code, out, errOut)
	}
	write(t, cfg, base+"tokens:\n  command: [\"echo\", \"{}\"]\n")
	if code, out, errOut := runCLI(t, "capture", "b", "--config", cfg, "--read-only", "--repeat", "2"); code != ExitClean || strings.Count(errOut, "tokens.command runs as configured") != 1 {
		t.Fatalf("tokens: exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestRunReadOnlyRefusesPrepareFirst: run --read-only refuses prepare:
// before anything runs, also the seed of a store without a pristine copy.
func TestRunReadOnlyRefusesPrepareFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1}`)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	sqliteDB(t, filepath.Join(root, "app.sqlite"), "CREATE TABLE news (id INTEGER PRIMARY KEY)")
	cfg := filepath.Join(root, "driftgate.yaml")
	conf := fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\nstate:\n  db:\n    driver: sqlite\n    working: app.sqlite\n", srv.URL)
	write(t, cfg, conf)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[{"id":"news","method":"GET","path":"/news","storage":"public"}]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.*'\n    reason: clock\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("baseline: %d\n%s\n%s", code, out, errOut)
	}
	// A new store: run would seed first.
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	seeded := filepath.Join(t.TempDir(), "seeded")
	write(t, cfg, conf+"seed:\n  - [\"touch\", \""+seeded+"\"]\nprepare:\n  - [\"true\"]\n")
	code, out, errOut := runCLI(t, "run", "--config", cfg, "--read-only")
	if code != ExitConfig || !strings.Contains(errOut, "prepare: runs after every restore") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(seeded); err == nil {
		t.Fatal("the seed ran before the refusal")
	}
}

// guardProject is a project whose state is a file (data) with restore and
// fingerprint commands, a guard command and a catalog of requests.
func guardProject(t *testing.T, guard, requests string) (cfg, root string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1}`)
	}))
	t.Cleanup(srv.Close)
	root = t.TempDir()
	write(t, filepath.Join(root, "data"), "")
	cfg = filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n"+
		"state:\n  db:\n    restore: [\"sh\", \"-c\", \": > data\"]\n    fingerprint: [\"cat\", \"data\"]\n"+
		"guards:\n  - name: g\n    command: %s\n", srv.URL, guard))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[`+requests+`]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	return cfg, root
}

// TestGuardThatWrites: a guard that writes to the tracked state (it
// should only check) does not stop the capture, and its write is part of
// the state the read-only requests start from, never taken for theirs.
func TestGuardThatWrites(t *testing.T) {
	cfg, _ := guardProject(t, `["sh", "-c", "echo row >> data"]`,
		`{"id":"news","method":"GET","path":"/news","storage":"public"},{"id":"post","method":"POST","path":"/p","body":{},"mutates":true,"storage":"public"}`)
	code, out, errOut := runCLI(t, "capture", "c", "--config", cfg)
	if code != ExitClean || strings.Contains(out, "read-only requests changed") || strings.Contains(out, "mutates: true") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
}

// TestGuardsChecked: the guards run before the first request, and again
// before the first request that changes data only when read-only requests
// ran in between.
func TestGuardsChecked(t *testing.T) {
	for _, c := range []struct {
		requests string
		runs     int
	}{
		{`{"id":"post","method":"POST","path":"/p","body":{},"mutates":true,"storage":"public"}`, 1},
		{`{"id":"news","method":"GET","path":"/news","storage":"public"},{"id":"post","method":"POST","path":"/p","body":{},"mutates":true,"storage":"public"}`, 2},
	} {
		count := filepath.Join(t.TempDir(), "count")
		cfg, _ := guardProject(t, `["sh", "-c", "echo x >> '`+count+`'"]`, c.requests)
		if code, out, errOut := runCLI(t, "capture", "c", "--config", cfg); code != ExitClean {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		raw, _ := os.ReadFile(count)
		if n := strings.Count(string(raw), "x"); n != c.runs {
			t.Errorf("%s: the guard ran %d times, want %d", c.requests, n, c.runs)
		}
	}
}
