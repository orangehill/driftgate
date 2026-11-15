package cli

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// reportPDF is the body of the demo's report: a PDF, which a capture of
// an earlier release stored public, in git.
const reportPDF = "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n"

// TestUpgradeOlderProject: a project as an earlier release left it (a
// catalog with a search sent as POST without mutates, a pristine record
// and a baseline whose state fingerprints have the old format, a PDF
// stored public in the baseline, no .gitattributes) reaches a clean run, each refusal naming
// the command that fixes it.
func TestUpgradeOlderProject(t *testing.T) {
	// The report is a PDF, which a capture keeps private: the project key.
	t.Setenv("DRIFTGATE_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	p := newDBProject(t)
	if _, err := p.db.Exec("CREATE VIEW faq_questions AS SELECT id, created_at FROM faq"); err != nil {
		t.Fatal(err)
	}
	// The application: the demo, a search sent as POST that only reads, and
	// a report as PDF.
	mux := http.NewServeMux()
	mux.Handle("/", p.api)
	mux.HandleFunc("POST /api/faq/search", func(w http.ResponseWriter, r *http.Request) {
		var n int
		p.db.QueryRow("SELECT COUNT(*) FROM faq").Scan(&n)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"count": n})
	})
	mux.HandleFunc("GET /api/report.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte(reportPDF))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	raw, _ := os.ReadFile(p.cfg)
	cfg := strings.Replace(string(raw), "base_url: ", "base_url: "+srv.URL+" #", 1)
	write(t, p.cfg, cfg)

	// The catalog of an earlier release: a search without mutates after a write.
	catPath := filepath.Join(p.root, "driftgate", "catalog.json")
	write(t, catPath, `{"version": 1, "requests": [
		{"id": "news", "method": "GET", "path": "/api/news", "storage": "public"},
		{"id": "faq-create", "method": "POST", "path": "/api/faq", "body": {"q": "?"}, "mutates": true, "storage": "public"},
		{"id": "faq-search", "method": "POST", "path": "/api/faq/search", "body": {"q": "?"}, "storage": "public"},
		{"id": "report", "method": "GET", "path": "/api/report.pdf", "storage": "public"}]}`)
	write(t, filepath.Join(p.root, "driftgate", "allowlist.yaml"), `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
body:
  - request: faq-*
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Time of creation.
`)
	cli := func(want int, args ...string) (string, string) {
		t.Helper()
		code, out, errOut := runCLI(t, append(args, "--config", p.cfg)...)
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s\n%s", args, code, want, out, errOut)
		}
		return out, errOut
	}

	// What an earlier release made: the baseline of a run in which the
	// search only read (as it took a request without mutates); then the
	// files as it wrote them.
	setMutates := func(id string, value *bool) {
		t.Helper()
		cat, err := catalog.Load(catPath)
		if err != nil {
			t.Fatal(err)
		}
		for i := range cat.Requests {
			if cat.Requests[i].ID == id {
				cat.Requests[i].Mutates = value
			}
		}
		if err := cat.Save(catPath); err != nil {
			t.Fatal(err)
		}
	}
	readOnly := false
	setMutates("faq-search", &readOnly)
	cli(ExitClean, "run")
	setMutates("faq-search", nil)
	records, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "pristine", "db-*.json"))
	if len(records) != 1 {
		t.Fatalf("pristine records %v", records)
	}
	setLegacyView(t, records[0], "fingerprint")
	baselines := filepath.Join(p.root, "driftgate", "baselines")
	base := filepath.Join(baselines, "local")
	setLegacyView(t, filepath.Join(base, "meta.json"), "state")
	write(t, filepath.Join(base, "bodies", snapshot.Sum([]byte(reportPDF))), reportPDF)
	rec := &snapshot.Record{Version: snapshot.Version, ID: "report", Status: 200, Headers: [][2]string{{"content-type", "application/pdf"}},
		Body: snapshot.Body{Storage: snapshot.Public, SHA256: snapshot.Sum([]byte(reportPDF)), Size: int64(len(reportPDF))}}
	old, err := (&snapshot.Set{Dir: base}).Record("report")
	if err != nil || old == nil {
		t.Fatalf("report record: %v", err)
	}
	rec.Headers = old.Headers
	if err := snapshot.WriteJSON(filepath.Join(base, "responses", "report.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(baselines, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(base); err != nil {
		t.Fatal(err)
	}

	// The upgrade.
	out, errOut := cli(ExitEnv, "run")
	if !strings.Contains(errOut, "re-seed needed") || !strings.Contains(out, "change") || !strings.Contains(out, "faq-search") {
		t.Fatalf("first run:\n%s\n%s", out, errOut)
	}
	if _, err := os.Stat(filepath.Join(baselines, ".gitattributes")); err != nil {
		t.Fatalf("run wrote no .gitattributes: %v", err)
	}
	cli(ExitClean, "seed")
	if _, errOut := cli(ExitEnv, "run"); !strings.Contains(errOut, "driftgate verify --baseline local --migrate-state") {
		t.Fatalf("run after the seed: %s", errOut)
	}
	cli(ExitClean, "verify", "--baseline", "local", "--migrate-state")
	// The search runs after the write, on the data it left.
	if out, _ := cli(ExitDrift, "run"); !strings.Contains(out, "mutates: false") || !strings.Contains(out[strings.Index(out, "== compare =="):], "faq-search") {
		t.Fatalf("run with the search after the write:\n%s", out)
	}
	// The fix the run names: the search only reads.
	setMutates("faq-search", &readOnly)
	if out, _ := cli(ExitDrift, "verify"); !strings.Contains(out, "report: a public body that cannot be scanned") || !strings.Contains(out, "--migrate-private") {
		t.Fatalf("verify of the PDF in git:\n%s", out)
	}
	cli(ExitClean, "verify", "--baseline", "local", "--migrate-private")
	if out, _ := cli(ExitClean, "run"); strings.Contains(out, "only by their method") {
		t.Fatalf("the last run:\n%s", out)
	}
	cli(ExitClean, "verify")
}

// TestGitLayoutKept: a project of the git layout (driftgate.yaml at the
// root, no baselines: key) keeps its layout: the files in driftgate/, the
// store in ~/.driftgate/<project>, nothing new in the repository. Its
// commands find the configuration from a subfolder too.
func TestGitLayoutKept(t *testing.T) {
	p := newDemoProject(t, "")
	root := filepath.Dir(p.cfg)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DRIFTGATE_STORE", "")
	cfg, _ := os.ReadFile(p.cfg)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "git layout")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "commit driftgate/") {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "driftgate", "baselines", "local", "responses")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(home, ".driftgate", "demo", "captures")); err != nil || len(entries) == 0 {
		t.Fatalf("store: %v %v", entries, err)
	}
	if st := gitIn(t, root, "status", "--porcelain", "--untracked-files=all"); strings.Contains(st, ".driftgate") || strings.Contains(st, ".gitignore") || strings.Contains(st, "driftgate.yaml") {
		t.Fatalf("run changed what git holds besides the baseline:\n%s", st)
	}
	if now, _ := os.ReadFile(p.cfg); string(now) != string(cfg) {
		t.Fatalf("driftgate.yaml changed:\n%s", now)
	}
	sub := filepath.Join(root, "routes")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if code, out, errOut := runCLI(t, "run"); code != ExitClean || !strings.Contains(out, "No drift") {
		t.Fatalf("run from a subfolder: %d\n%s\n%s", code, out, errOut)
	}
}
