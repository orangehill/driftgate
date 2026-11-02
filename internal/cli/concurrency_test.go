package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// autoApp is an API whose item requests take a while. hits counts the item
// requests answered since the last reset, which GET /api/hits shows (a read
// that depends on the order, like a view counter). With overlap set, two
// requests in flight at once leave a mark in the state (GET /state), as a
// lock or a per-client counter would.
type autoApp struct {
	mu       sync.Mutex
	hits     int
	inflight int
	overlap  bool
	marked   bool
}

func (a *autoApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/reset":
		a.mu.Lock()
		a.hits, a.marked = 0, false
		a.mu.Unlock()
	case r.URL.Path == "/state":
		a.mu.Lock()
		fmt.Fprintf(w, `{"marked":%v}`, a.marked)
		a.mu.Unlock()
	case r.URL.Path == "/api/hits":
		a.mu.Lock()
		fmt.Fprintf(w, `{"hits":%d}`, a.hits)
		a.mu.Unlock()
	case r.Method == http.MethodPost && r.URL.Path == "/api/note":
		fmt.Fprint(w, `{"ok":true}`)
	case strings.HasPrefix(r.URL.Path, "/api/items/"):
		a.mu.Lock()
		a.inflight++
		if a.overlap && a.inflight > 1 {
			a.marked = true
		}
		a.mu.Unlock()
		time.Sleep(15 * time.Millisecond)
		a.mu.Lock()
		a.inflight--
		a.hits++
		a.mu.Unlock()
		fmt.Fprintf(w, `{"item":%q}`, strings.TrimPrefix(r.URL.Path, "/api/items/"))
	default:
		http.NotFound(w, r)
	}
}

func (a *autoApp) setOverlap(on bool) {
	a.mu.Lock()
	a.overlap = on
	a.mu.Unlock()
}

// newAutoProject writes a project on an autoApp with the given concurrency
// and a catalog of item requests; withHits puts GET /api/hits fourth.
func newAutoProject(t *testing.T, concurrency string, withHits bool) (*autoApp, string, string) {
	t.Helper()
	app := &autoApp{}
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf(`version: 1
project: auto
mode: freeze
level: values
target:
  base_url: %s
  concurrency: %s
state:
  app:
    restore: ["curl", "-sf", "-X", "POST", "%s/reset"]
    fingerprint: ["curl", "-sf", "%s/state"]
`, srv.URL, concurrency, srv.URL, srv.URL))
	var reqs []string
	for i := 1; i <= 16; i++ {
		reqs = append(reqs, fmt.Sprintf(`{"id":"item-%02d","method":"GET","path":"/api/items/%d","storage":"public"}`, i, i))
		if i == 3 && withHits {
			reqs = append(reqs, `{"id":"hits","method":"GET","path":"/api/hits","storage":"public"}`)
		}
	}
	reqs = append(reqs, `{"id":"note","method":"POST","path":"/api/note","body":{"n":1},"mutates":true,"storage":"public"}`)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[`+strings.Join(reqs, ",\n")+`]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
body: []
`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	return app, root, cfg
}

func baselineMeta(t *testing.T, root string) (*snapshot.Meta, []byte) {
	t.Helper()
	path := filepath.Join(root, "driftgate", "baselines", "local", "meta.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m snapshot.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return &m, raw
}

// TestConcurrencyAuto: on an application that is safe in parallel the first
// run picks more than one request at once and records it; later runs use
// it without measuring and leave the baseline as it is; a later capture
// whose read-only requests change the state together (and no one alone)
// puts it back to one at a time, recorded, and captures again sequentially.
func TestConcurrencyAuto(t *testing.T) {
	app, root, cfg := newAutoProject(t, "auto", false)
	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "baseline local made from two equal captures") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	meta, raw := baselineMeta(t, root)
	if len(meta.Concurrency) != 1 {
		t.Fatalf("baseline meta has no decision: %s", raw)
	}
	c := meta.Concurrency[0]
	if c.Service != "" || c.Value < 2 || c.Value != c.Tried || len(c.Differed) > 0 || len(c.Probe) != 4 || c.SequentialMS == 0 || c.ParallelMS == 0 ||
		!strings.Contains(c.Reason, "identical to a sequential capture") {
		t.Fatalf("decision: %+v", c)
	}
	for i, pr := range c.Probe {
		if pr.Concurrency != 1<<i || pr.Requests != 16 {
			t.Errorf("probe %d: %+v", i, pr)
		}
	}
	if want := fmt.Sprintf("concurrency: %d — identical", c.Value); !strings.Contains(out, want) {
		t.Errorf("first run does not say %q:\n%s", want, out)
	}

	// Later: the recorded value, no probe, the baseline unchanged.
	code, out, errOut = runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, fmt.Sprintf("concurrency: %d (auto, recorded in baseline local: identical", c.Value)) ||
		strings.Contains(out, "measuring") {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	if _, again := baselineMeta(t, root); !bytes.Equal(raw, again) {
		t.Errorf("a later run rewrote the baseline meta:\n%s\n%s", raw, again)
	}

	// Read-only requests now leave a mark when they overlap.
	app.setOverlap(true)
	code, out, errOut = runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "back to 1 after a capture at") || !strings.Contains(out, "app/app") ||
		!strings.Contains(out, "capturing again one request at a time") {
		t.Fatalf("run with an order effect: %d\n%s\n%s", code, out, errOut)
	}
	meta, _ = baselineMeta(t, root)
	if c := meta.Concurrency[0]; c.Value != 1 || len(c.Differed) == 0 {
		t.Fatalf("fallback not recorded: %+v", c)
	}
	if code, out, errOut = runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "concurrency: 1 (auto, recorded in baseline local: back to 1") ||
		strings.Contains(out, "back to 1 after a capture at 1") {
		t.Fatalf("run after the fallback: %d\n%s\n%s", code, out, errOut)
	}
}

// TestConcurrencyAutoOrderDependent: a read whose response depends on the
// order (a counter) makes auto stay at one at a time, and says which
// request differed.
func TestConcurrencyAutoOrderDependent(t *testing.T) {
	_, root, cfg := newAutoProject(t, "auto", true)
	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	meta, _ := baselineMeta(t, root)
	c := meta.Concurrency[0]
	if c.Value != 1 || c.Tried < 2 || len(c.Differed) != 1 || c.Differed[0] != "hits" {
		t.Fatalf("decision: %+v", c)
	}
	if !strings.Contains(out, fmt.Sprintf("concurrency: 1 — at %d at once, hits differed from a sequential capture", c.Tried)) {
		t.Errorf("output does not name the request:\n%s", out)
	}
	if code, out, _ = runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "concurrency: 1 (auto, recorded in baseline local: at") {
		t.Fatalf("second run: %d\n%s", code, out)
	}
}

// TestConcurrencyDefault: without auto nothing is measured, said or recorded.
func TestConcurrencyDefault(t *testing.T) {
	_, root, cfg := newAutoProject(t, "1", true)
	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean || strings.Contains(out, "concurrency") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	if meta, raw := baselineMeta(t, root); meta.Concurrency != nil || strings.Contains(string(raw), "concurrency") {
		t.Fatalf("meta: %s", raw)
	}
	if code, _, errOut := runCLI(t, "capture", "c1", "--calibrate", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "concurrency: auto") {
		t.Fatalf("--calibrate without auto: %d %s", code, errOut)
	}
}

// TestCalibrateExistingBaseline: capture --calibrate decides auto for a
// baseline made before, records it there with a manifest that still
// verifies.
func TestCalibrateExistingBaseline(t *testing.T) {
	_, root, cfg := newAutoProject(t, "1", false)
	if code, out, errOut := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, strings.Replace(string(raw), "concurrency: 1", "concurrency: auto", 1))
	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "concurrency: 1 (auto, not decided yet") {
		t.Fatalf("run before calibrating: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut = runCLI(t, "capture", "cal", "--calibrate", "--config", cfg); code != ExitClean || !strings.Contains(out, "recorded in baseline local (commit it)") {
		t.Fatalf("calibrate: %d\n%s\n%s", code, out, errOut)
	}
	meta, _ := baselineMeta(t, root)
	if len(meta.Concurrency) != 1 || meta.Concurrency[0].Value < 2 {
		t.Fatalf("decision: %+v", meta.Concurrency)
	}
	if code, out, errOut = runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "(auto, recorded in baseline local") {
		t.Fatalf("run after calibrating: %d\n%s\n%s", code, out, errOut)
	}
}

// TestCalibrateLocal: with baselines: local nothing is committed, so the
// decision is recorded without saying so.
func TestCalibrateLocal(t *testing.T) {
	_, root, cfg := newAutoProject(t, "1", false)
	if err := os.Rename(filepath.Join(root, "driftgate"), filepath.Join(root, ".driftgate")); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, ".driftgate", "driftgate.yaml")
	raw, _ := os.ReadFile(cfg)
	write(t, local, string(raw))
	os.Remove(cfg)
	if code, out, errOut := runCLI(t, "run", "--config", local); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	write(t, local, strings.Replace(string(raw), "concurrency: 1", "concurrency: auto", 1))
	code, out, errOut := runCLI(t, "capture", "cal", "--calibrate", "--config", local)
	if code != ExitClean || !strings.Contains(out, "recorded in baseline local;") || strings.Contains(out, "commit") {
		t.Fatalf("calibrate: %d\n%s\n%s", code, out, errOut)
	}
}

// TestRecordDecisionsIsDeterministic: decisions are kept one per service,
// in name order, whatever order they come in.
func TestRecordDecisionsIsDeterministic(t *testing.T) {
	write := func(order []string) []byte {
		set := &snapshot.Set{Dir: t.TempDir()}
		if err := set.WriteMeta(snapshot.Meta{Tag: "t", Source: "capture", Created: "2026-10-30T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		for _, s := range order {
			if err := recordDecisions(set, false, []snapshot.Concurrency{{Service: s, Value: 1, Reason: "r " + s}}); err != nil {
				t.Fatal(err)
			}
		}
		// A later decision replaces the earlier one of the same service.
		if err := recordDecisions(set, false, []snapshot.Concurrency{{Service: "b", Value: 4, Reason: "again", Differed: []string{"x"}}}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(set.Dir, "meta.json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	a, b := write([]string{"c", "a", "b"}), write([]string{"b", "c", "a"})
	if !bytes.Equal(a, b) {
		t.Fatalf("meta depends on the order:\n%s\n%s", a, b)
	}
	var m snapshot.Meta
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Concurrency) != 3 || m.Concurrency[0].Service != "a" || m.Concurrency[1].Value != 4 || m.Concurrency[2].Service != "c" {
		t.Fatalf("decisions: %+v", m.Concurrency)
	}
}

func TestChooseConcurrency(t *testing.T) {
	ms := func(d ...int) []capture.Throughput {
		var out []capture.Throughput
		for i, v := range d {
			out = append(out, capture.Throughput{Concurrency: 1 << i, Requests: 32, Duration: time.Duration(v) * time.Millisecond})
		}
		return out
	}
	for _, c := range []struct {
		runs []capture.Throughput
		want int
	}{
		{ms(800, 400, 200, 100), 8},
		{ms(800, 400, 105, 100), 4},    // 4 is within 10% of 8
		{ms(800, 790, 795, 800), 1},    // nothing gained
		{ms(800, 1200, 1600, 3000), 1}, // a server that serializes
		{nil, 1},
	} {
		if got := capture.Choose(c.runs); got != c.want {
			t.Errorf("Choose(%v) = %d, want %d", c.runs, got, c.want)
		}
	}
}

func TestConcurrencyConfig(t *testing.T) {
	for _, c := range []struct {
		text string
		want config.Concurrency
		err  bool
	}{
		{"auto", config.ConcurrencyAuto, false},
		{"4", 4, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"many", 0, true},
	} {
		root := t.TempDir()
		path := filepath.Join(root, "driftgate.yaml")
		write(t, path, "version: 1\nproject: p\ntarget:\n  base_url: http://localhost:1\n  concurrency: "+c.text+"\n")
		p, err := config.Load(path)
		switch {
		case c.err && err == nil:
			t.Errorf("%s accepted", c.text)
		case !c.err && err != nil:
			t.Errorf("%s: %v", c.text, err)
		case !c.err && p.Target.Concurrency != c.want:
			t.Errorf("%s: got %d", c.text, p.Target.Concurrency)
		}
	}
}

// TestRecordDecisionsKeepsBaselineWhole: meta.json and the manifest of a
// baseline change together; a failure on the way leaves the baseline as it
// was, which checkBaseline still accepts.
func TestRecordDecisionsKeepsBaselineWhole(t *testing.T) {
	set := &snapshot.Set{Dir: filepath.Join(t.TempDir(), "local")}
	if err := set.WriteMeta(snapshot.Meta{Tag: "t", Source: "capture", Created: "2026-10-30T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(set.Dir); err != nil {
		t.Fatal(err)
	}
	// A file that cannot be read makes writing the manifest (and copying
	// the baseline) fail.
	stray := filepath.Join(set.Dir, "stray")
	if err := os.WriteFile(stray, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := recordDecisions(set, true, []snapshot.Concurrency{{Service: "", Value: 4, Reason: "r"}}); err == nil {
		t.Fatal("recorded with an unreadable file")
	}
	os.Remove(stray)
	proj, err := config.New(config.Config{Version: config.Version, Project: "p", Mode: config.Freeze, Level: "values"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := checkBaseline(proj, set, "local"); err != nil {
		t.Fatalf("the baseline is no longer whole: %v", err)
	}
	if err := recordDecisions(set, true, []snapshot.Concurrency{{Service: "", Value: 4, Reason: "r"}}); err != nil {
		t.Fatal(err)
	}
	if err := checkBaseline(proj, set, "local"); err != nil {
		t.Fatal(err)
	}
	if m, _ := set.Meta(); len(m.Concurrency) != 1 || m.Concurrency[0].Value != 4 {
		t.Fatalf("meta: %+v", m)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(set.Dir), ".tmp-*"))
	if len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
}
