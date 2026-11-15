package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// setsFixture is a project with a baseline local and the captures l55 and
// l56, written as captures write them: l56 changes the type of one public
// value and one private value, and answers a request the baseline does not
// have. The store and the key are set in the environment.
func setsFixture(t *testing.T) (project, cfg string) {
	t.Helper()
	project = t.TempDir()
	cfg = filepath.Join(project, "driftgate.yaml")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	const origin = "http://localhost:8000"
	write(t, cfg, "version: 1\nproject: demo\nmode: freeze\nlevel: values\ntarget:\n  base_url: "+origin+"\n")
	write(t, filepath.Join(project, "driftgate", "catalog.json"), `{"version":1,"requests":[
 {"id":"pub-news","method":"GET","path":"/public-api/news","headers":[["Accept","application/json"],["X-B","2"]],"storage":"public"},
 {"id":"api-me","method":"GET","path":"/api/auth/me","storage":"private"},
 {"id":"mut-login","method":"POST","path":"/api/auth/login","body":{"email":"a@b.c","n":1.0},"mutates":true,"storage":"private"},
 {"id":"pub-new","method":"GET","path":"/public-api/new","storage":"public"}]}`)
	write(t, filepath.Join(project, "driftgate", "allowlist.yaml"), `version: 1
headers:
  - name: date
    format: '\d+'
    reason: time
body:
  - request: mut-login
    path: $.token
    format: '[a-z]+\.[a-z]+'
    reason: new JWT
`)
	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		id, body string
		private  bool
	}
	sets := []struct {
		tag     string
		records []record
	}{
		{"l55", []record{{"pub-news", `{"entries":[{"id":"5","date":"2024-03-01 10:00:00"}]}`, false},
			{"api-me", `{"user":{"id":1,"email":"a@b.c"}}`, true}, {"mut-login", `{"token":"aa.bb"}`, true}}},
		{"l56", []record{{"pub-news", `{"entries":[{"id":5,"date":"2024-03-01 10:00:00"}]}`, false},
			{"api-me", `{"user":{"id":"1","email":"a@b.c"}}`, true}, {"mut-login", `{"token":"ccc.dddd"}`, true}, {"pub-new", `{}`, false}}},
	}
	fill := func(set *snapshot.Set, tag string, records []record) {
		for _, r := range records {
			rec := &snapshot.Record{ID: r.id, Status: 200, Headers: [][2]string{{"content-type", "application/json"}, {"date", "1" + tag[2:]}},
				Body: snapshot.Body{Storage: snapshot.Public}}
			if r.private {
				rec.Body.Storage = snapshot.Private
			}
			if err := set.Write(rec, []byte(r.body)); err != nil {
				t.Fatal(err)
			}
		}
		meta := snapshot.Meta{Tag: tag, Source: "capture", Created: time.Now().UTC().Format(time.RFC3339), Origins: map[string]string{"": origin}}
		if err := set.WriteMeta(meta); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range sets {
		set, err := p.Capture(s.tag)
		if err != nil {
			t.Fatal(err)
		}
		fill(set, s.tag, s.records)
	}
	base, err := p.Baseline("local")
	if err != nil {
		t.Fatal(err)
	}
	fill(base, "l55", sets[0].records)
	if err := writeManifest(base.Dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Layout.Baselines, ".gitattributes"), []byte(lineEndings), 0o644); err != nil {
		t.Fatal(err)
	}
	return project, cfg
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut syncBuffer
	code := Main(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// syncBuffer is a bytes.Buffer that several goroutines may write: a
// command's own lines and the note of the signal handler (signalContext)
// can reach one writer at the same time. A terminal's file is safe for
// that; a bytes.Buffer is not.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestCompareSets compares two captures with a baseline: drifts of public
// and private bodies, allowlist rules used and unused, selections that
// compare nothing, and private bodies without the key.
func TestCompareSets(t *testing.T) {
	project, cfg := setsFixture(t)
	// No private value may reach the repository.
	filepath.Walk(project, func(p string, info os.FileInfo, _ error) error {
		if raw, err := os.ReadFile(p); err == nil && bytes.Contains(raw, []byte(`"email":"a@b.c"}}`)) {
			t.Errorf("private body in repository: %s", p)
		}
		return nil
	})
	// The baseline keeps none of its private bodies, not even encrypted: they
	// are in the local store, named by their ref.
	if _, err := os.Stat(filepath.Join(project, "driftgate", "baselines", "local", "private")); !os.IsNotExist(err) {
		t.Fatalf("private/ in the baseline: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "private", "*.enc")); len(files) < 2 {
		t.Fatalf("private bodies in the local store: %d", len(files))
	}

	code, out, _ := runCLI(t, "compare", "l55", "--config", cfg)
	if code != ExitClean {
		t.Fatalf("baseline against itself: %d\n%s", code, out)
	}

	code, out, _ = runCLI(t, "compare", "l56", "--config", cfg, "--format", "json")
	if code != ExitDrift {
		t.Fatalf("l56: exit %d\n%s", code, out)
	}
	var r report.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, res := range r.Results {
		got[res.ID] = res.Outcome
		for _, d := range res.Drifts {
			got[res.ID+" "+d.Kind+" "+d.Where] = "x"
		}
	}
	for _, want := range []string{"pub-news type $.entries[0].id", "api-me type $.user.id"} {
		if got[want] == "" {
			t.Errorf("missing drift %s; report %s", want, out)
		}
	}
	if got["mut-login"] != report.Pass || got["pub-new"] != report.Skipped {
		t.Errorf("outcomes: %v", got)
	}
	// Both rules applied to a change (the date header, the token).
	if len(r.UnusedRules) != 0 {
		t.Errorf("unused rules: %+v", r.UnusedRules)
	}
	// Against itself nothing changed: both rules are listed, as a warning.
	code, out, _ = runCLI(t, "compare", "l55", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "warning: 2 rules of the allowlist applied to no change") ||
		!strings.Contains(out, "body rule 1 ($.token, request mut-login): nothing it applies to changed") {
		t.Errorf("unused rules, text: %d\n%s", code, out)
	}
	// Of some requests only: the token rule applies to none of them, and
	// says nothing about them.
	_, out, _ = runCLI(t, "compare", "l56", "--config", cfg, "--only", "pub-*", "--format", "json")
	var some report.Report
	if err := json.Unmarshal([]byte(out), &some); err != nil {
		t.Fatal(err)
	}
	if len(some.UnusedRules) != 0 {
		t.Errorf("--only: %+v", some.UnusedRules)
	}

	// Nothing compared is never green: a typo in --only, or requests the
	// baseline does not have (renamed IDs, a baseline of another catalog).
	for only, want := range map[string]string{"pub-nwes": "no request was selected", "pub-new": "the baseline has none of the selected requests"} {
		code, out, _ = runCLI(t, "compare", "l56", "--config", cfg, "--only", only)
		if code != ExitEnv || !strings.Contains(out, want) || strings.Contains(out, "No drift") {
			t.Errorf("--only %s: exit %d\n%s", only, code, out)
		}
		if code, _, _ := runCLI(t, "report", "--config", cfg, "--exit-code"); code != ExitEnv {
			t.Errorf("--only %s: report --exit-code %d", only, code)
		}
	}

	// Without the key, private bodies cannot be read: environment error.
	t.Setenv(secret.EnvKey, "")
	t.Setenv("HOME", t.TempDir()) // no Keychain entry for this project
	if code, _, errOut := runCLI(t, "compare", "l56", "--config", cfg); code != ExitEnv {
		t.Fatalf("without key: exit %d %s", code, errOut)
	}
}

// TestCompareReadsNoState: compare, report and review read captures
// only, so a state: whose DSN cannot be read on this machine (a variable
// not set, a file of the project not there, a command's variables) does
// not stop them; only the YAML itself is checked.
func TestCompareReadsNoState(t *testing.T) {
	_, cfg := setsFixture(t)
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DG_TEST_NO_DSN", "")
	write(t, cfg, string(raw)+`state:
  db:
    driver: mysql
    dsn:
      env: DG_TEST_NO_DSN
    working: app
  pg:
    driver: postgres
    dsn:
      dotenv: missing/.env
      key: DATABASE_URL
    working: app
  cache:
    restore: ["sh", "-c", "redis-cli -u \"$DG_TEST_NO_DSN\" flushall"]
`)
	if code, out, errOut := runCLI(t, "compare", "l56", "--against", "capture:l55", "--config", cfg); code != ExitDrift {
		t.Fatalf("compare: exit %d\n%s%s", code, out, errOut)
	}
	for _, args := range [][]string{{"report", "--config", cfg}, {"review", "--config", cfg}} {
		if code, out, errOut := runCLI(t, args...); code == ExitConfig || code == ExitEnv {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}, {"compare"}, {"import", "x"}, {"compare", "a", "--format", "xml"}} {
		if code, _, _ := runCLI(t, args...); code != ExitConfig {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// TestCompareNamesAreConfiguration: an invalid name after baseline: or
// capture: is a configuration error.
func TestCompareNamesAreConfiguration(t *testing.T) {
	p := newDemoProject(t, "")
	for _, name := range []string{"baseline:../x", "capture:a/b"} {
		if code, _, errOut := runCLI(t, "compare", name, "--against", "baseline:local", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "invalid") {
			t.Errorf("%s: exit %d: %s", name, code, errOut)
		}
	}
}
