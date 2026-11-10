package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestBaselinesRebase: a capture becomes the baseline's content with one
// line in the acceptance log; the expectations it holds are closed, the
// others kept; in mode freeze only with --allow-freeze.
func TestBaselinesRebase(t *testing.T) {
	p, base := expectProject(t)
	rebase := func(args ...string) (int, string, string) {
		return runCLI(t, append([]string{"baselines", "rebase", "--config", p.cfg}, args...)...)
	}
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "expect", "me-anonymous", "--ref", "T-2", "--reason", "y", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "capture", "main", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	// news drifts otherwise in main than T-1 expects.
	editRecord(t, "main", "news", func(r *snapshot.Record) { setHeader(r, "x-powered-by", "demo 2") })
	if code, out, errOut := runCLI(t, "capture", "part", "--only", "news*", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	log, _ := os.ReadFile(filepath.Join(base, baseline.LogFile))

	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"local", "--from", "main", "--reason", "merged"}, ExitConfig, "mode is freeze"},
		{[]string{"local", "--from", "main", "--allow-freeze"}, ExitConfig, "needs --reason"},
		{[]string{"local", "--reason", "x", "--allow-freeze"}, ExitConfig, "usage: driftgate baselines rebase"},
		{[]string{"nope", "--from", "main", "--reason", "x", "--allow-freeze"}, ExitConfig, `no baseline named "nope"`},
		{[]string{"local", "--from", "nope", "--reason", "x", "--allow-freeze"}, ExitConfig, `no capture named "nope"`},
		{[]string{"local", "--from", "part", "--reason", "x", "--allow-freeze"}, ExitConfig, "capture part has no response to 5 requests"},
	} {
		if code, _, errOut := rebase(tc.args...); code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: %d %s", tc.args, code, errOut)
		}
	}
	// The private bodies of the capture must be on this machine.
	if code, _, errOut := runCLI(t, "capture", "nobody", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d %s", code, errOut)
	}
	after, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "private", "*.enc"))
	moved := map[string][]byte{}
	for _, f := range after {
		raw, _ := os.ReadFile(f)
		moved[f] = raw
		os.Remove(f)
	}
	if code, _, errOut := rebase("local", "--from", "nobody", "--reason", "x", "--allow-freeze"); code != ExitEnv || !strings.Contains(errOut, "not on this machine") {
		t.Fatalf("private bodies missing: %d %s", code, errOut)
	}
	for f, raw := range moved {
		write(t, f, string(raw))
	}
	// What goes into the baseline is scanned as verify scans it.
	setMode(t, p.cfg, "evolve")
	if code, _, errOut := runCLI(t, "capture", "leak", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d %s", code, errOut)
	}
	editRecord(t, "leak", "news", func(r *snapshot.Record) { setHeader(r, "x-contact", "jane.doe@example.com") })
	if code, _, errOut := rebase("local", "--from", "leak", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "news: ") || !strings.Contains(errOut, "header:x-contact") {
		t.Fatalf("leak: %d %s", code, errOut)
	}
	if now, _ := os.ReadFile(filepath.Join(base, baseline.LogFile)); string(now) != string(log) {
		t.Fatal("a refused rebase wrote the log")
	}
	setMode(t, p.cfg, "freeze")

	code, out, errOut := rebase("local", "--from", "main", "--reason", "main merged v2", "--ref", "M-1", "--allow-freeze")
	if code != ExitClean || !strings.Contains(out, "rebased baseline local on capture main: 7 requests changed") ||
		!strings.Contains(out, "closed the expectations of T-2 the baseline now holds: me-anonymous") || !strings.Contains(out, "1 expectation kept") ||
		!strings.Contains(out, "warning: capture main was made once") {
		t.Fatalf("rebase: %d\n%s\n%s", code, out, errOut)
	}
	entries, err := baseline.Log(base)
	if err != nil || len(entries) != 1 {
		t.Fatalf("log: %v %+v", err, entries)
	}
	if a := entries[0]; a.Kind != "rebase" || a.Capture != "main" || a.Ref != "M-1" || a.Reason != "main merged v2" || len(a.Requests) != 7 || len(a.Signatures) == 0 {
		t.Fatalf("log line: %+v", a)
	}
	exps, _, err := baseline.Expectations(base)
	if err != nil || len(exps) != 1 || exps[0].Ref != "T-1" {
		t.Fatalf("expectations: %v %+v", err, exps)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "OK     baseline local") {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	// The baseline is the capture now: comparing them finds nothing, and
	// the capture is still there.
	if code, r := compareJSON(t, p.cfg, "main", "local"); code != ExitClean || r.Drifted != 0 || len(r.Unmet) != 1 || r.Unmet[0].Why != "no drift" {
		t.Fatalf("main against the rebased baseline: %d %+v", code, r)
	}
	// The API as it is: only the news header differs from main, which no
	// expectation names (T-1 expected v2 against v1).
	code, out, _ = runCLI(t, "run", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "DRIFT  news") || strings.Contains(out, "DRIFT  me") || !strings.Contains(out, "T-1 news (other drifts)") {
		t.Fatalf("run after rebase: %d\n%s", code, out)
	}

	// Data that changed since the baseline (a new seed) stops a capture
	// checked against it; one made with --skip-fingerprint-check records
	// the data it ran on, which a rebase makes the baseline's.
	if code, out, errOut := runCLI(t, "capture", "seeded", "--skip-fingerprint-check", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := rebase("local", "--from", "seeded", "--reason", "new seed", "--allow-freeze"); code != ExitClean {
		t.Fatalf("rebase: %d\n%s\n%s", code, out, errOut)
	}
	set := &snapshot.Set{Dir: base}
	if meta, err := set.Meta(); err != nil || meta.Unreliable || meta.Tag != "seeded" {
		t.Fatalf("meta: %v %+v", err, meta)
	}
	if code, out, _ := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run after the second rebase: %d\n%s", code, out)
	}
	// Two equal captures: no warning.
	if code, out, errOut := runCLI(t, "capture", "twice", "--repeat", "2", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := rebase("local", "--from", "twice", "--reason", "checked", "--allow-freeze"); code != ExitClean || strings.Contains(out, "warning: capture") {
		t.Fatalf("rebase: %d\n%s\n%s", code, out, errOut)
	}
	// The checks of a baseline made by run: an error of a request meant to
	// succeed and a response that came only after a retry are said; the
	// rebase is done and exits 0, as accept does.
	if code, out, errOut := runCLI(t, "capture", "broken", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	editRecord(t, "broken", "news", func(r *snapshot.Record) { r.Status = 500 })
	editRecord(t, "broken", "me-anonymous", func(r *snapshot.Record) {
		r.Retry = &snapshot.Retry{Sends: 2, Status: 503, MS: 5, Recovered: true}
	})
	code, out, errOut = rebase("local", "--from", "broken", "--reason", "checked", "--allow-freeze")
	// Mode freeze accepts nothing: the title says to rebase instead, with
	// every argument rebase needs.
	if code != ExitClean || !strings.Contains(out, failedRequestsTitle(config.Freeze)) ||
		!strings.Contains(out, `driftgate baselines rebase <baseline> --from <capture> --reason "…" --allow-freeze), baseline local:`) ||
		strings.Contains(out, "accept the new answer") || !strings.Contains(out, "news") ||
		!strings.Contains(out, "warning: baseline local holds 1 response that came only after a retry: me-anonymous") {
		t.Fatalf("rebase of a broken capture: %d\n%s\n%s", code, out, errOut)
	}
}

// TestRunWarnsOfLeftOverExpectations: a new baseline made where an
// earlier one of its name left expectations says so.
func TestRunWarnsOfLeftOverExpectations(t *testing.T) {
	p, base := expectProject(t)
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	os.RemoveAll(base)
	code, out, _ := runCLI(t, "run", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "warning: "+filepath.Join("driftgate", "baselines", "local.expectations")+"/ holds expectations of an earlier baseline local") {
		t.Fatalf("run: %d\n%s", code, out)
	}
}

// TestRunIgnoresLeftOverWrite: a folder of expectations that holds only a
// file a killed write left holds no expectations: a new baseline made
// there gives no warning.
func TestRunIgnoresLeftOverWrite(t *testing.T) {
	p, base := expectProject(t)
	os.RemoveAll(base)
	dir := baseline.ExpectDir(base)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshot.TempPrefix+"123"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "run", "--config", p.cfg)
	if code != ExitClean || strings.Contains(out, "holds expectations of an earlier baseline") {
		t.Fatalf("run: %d\n%s", code, out)
	}
}
