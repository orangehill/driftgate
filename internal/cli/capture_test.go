package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	// A failing guard stops before any request that changes data.
	p = newDemoProject(t, "")
	raw, _ = os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), `["true"]`, `["false"]`, 1))
	if code, out, _ := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitEnv || strings.Contains(out, "faq-create") {
		t.Fatalf("failing guard: exit %d\n%s", code, out)
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
// at a failing guard, after the read-only requests) is not compared as one
// whose other requests are gone (drift, exit 1), but refused as incomplete
// (exit 3).
func TestInterruptedCaptureIsIncomplete(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), `["true"]`, `["false"]`, 1))
	if code, _, errOut := runCLI(t, "capture", "c2", "--config", p.cfg); code != ExitEnv {
		t.Fatalf("failing guard: exit %d %s", code, errOut)
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
		!strings.Contains(out, `"mutates": false`) || !strings.Contains(out, "--restore once") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	write(t, catalogPath, `{"version":1,"requests":[{"id":"news","method":"GET","path":"/api/news","storage":"public"},
		{"id":"login","method":"POST","path":"/api/auth/login","body":{"user":"admin"},"mutates":false,"storage":"private"}]}`)
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--force"); code != ExitClean || strings.Contains(out, "only by their method") {
		t.Fatalf("with mutates: exit %d\n%s\n%s", code, out, errOut)
	}
}
