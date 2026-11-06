package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/storelock"
)

// acceptProject makes a baseline of the demo API with run, switches the API
// to v2 and runs again, so the last comparison has drifts in every request.
func acceptProject(t *testing.T) (*demoProject, string) {
	t.Helper()
	p := newDemoProject(t, "")
	setMode(t, p.cfg, "evolve")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitDrift {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	return p, filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
}

func setMode(t *testing.T, cfg, mode string) {
	t.Helper()
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, regexp.MustCompile(`(?m)^mode: \w+`).ReplaceAllString(string(raw), "mode: "+mode))
}

func lastCompare(t *testing.T) (*report.Report, string) {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "last-compare-*.json"))
	if len(paths) != 1 {
		t.Fatalf("last comparisons: %v", paths)
	}
	path := paths[0]
	r, err := report.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return r, path
}

func groupNumber(t *testing.T, r *report.Report, sig string) string {
	t.Helper()
	for _, g := range r.Groups {
		if g.Signature == sig {
			return "group:" + strconv.Itoa(g.N)
		}
	}
	t.Fatalf("no group %q in %+v", sig, r.Groups)
	return ""
}

func TestAcceptRefusals(t *testing.T) {
	p, base := acceptProject(t)
	accept := func(args ...string) (int, string) {
		code, _, errOut := runCLI(t, append([]string{"accept", "--config", p.cfg}, args...)...)
		return code, errOut
	}

	setMode(t, p.cfg, "freeze")
	if code, errOut := accept("news", "--reason", "intended"); code != ExitConfig || !strings.Contains(errOut, "mode is freeze") {
		t.Fatalf("freeze: %d %s", code, errOut)
	}
	if _, out, _ := runCLI(t, "review", "--config", p.cfg); !strings.Contains(out, "mode is freeze") || strings.Contains(out, "accept:") {
		t.Fatalf("review in freeze mode:\n%s", out)
	}
	setMode(t, p.cfg, "evolve")

	for _, args := range [][]string{{"news"}, {"news", "--reason", "  "}} {
		if code, errOut := accept(args...); code != ExitConfig || !strings.Contains(errOut, "--reason") {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"--reason", "x"}, {"nope", "--reason", "x"}, {"slow", "--reason", "x"}, {"zz*", "--reason", "x"}, {"group:99", "--reason", "x"}} {
		if code, errOut := accept(args...); code != ExitConfig {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}

	// The header group touches every request, but news drifts elsewhere too.
	r, _ := lastCompare(t)
	header := groupNumber(t, r, "header-extra header:x-powered-by")
	code, errOut := accept(header, "--reason", "x")
	if code != ExitConfig || !regexp.MustCompile(`news: \d+ of \d+ drifts not selected, in group:\d`).MatchString(errOut) {
		t.Fatalf("partial group: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(base, baseline.LogFile)); err == nil {
		t.Fatal("a refused accept wrote the log")
	}

	// A stale comparison: the baseline changed since (an earlier report).
	_, path := lastCompare(t)
	old, _ := os.ReadFile(path)
	if code, errOut := accept("news", "--reason", "x"); code != ExitClean {
		t.Fatalf("accept news: %d %s", code, errOut)
	}
	if code, errOut := accept("news", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "already accepted") {
		t.Fatalf("accept twice: %d %s", code, errOut)
	}
	write(t, path, string(old))
	if code, errOut := accept("me", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "baseline local changed since the comparison") {
		t.Fatalf("stale baseline: %d %s", code, errOut)
	}

	// The capture changed or is gone.
	if code, _, _ := runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg); code != ExitDrift {
		t.Fatal("compare")
	}
	capture := filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", runTagFor(t, p.cfg))
	write(t, filepath.Join(capture, "responses", "me.json"), "{}")
	if code, errOut := accept("me", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "capture "+runTagFor(t, p.cfg)+" changed") {
		t.Fatalf("changed capture: %d %s", code, errOut)
	}
	os.RemoveAll(capture)
	if code, errOut := accept("me", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "capture "+runTagFor(t, p.cfg)+" is gone") {
		t.Fatalf("missing capture: %d %s", code, errOut)
	}

	// A baseline edited by hand: nothing compared with it can be trusted.
	write(t, filepath.Join(base, "responses", "me.json"), "{}")
	if code, errOut := accept("me", "--reason", "x"); code != ExitEnv || !strings.Contains(errOut, "changed outside driftgate accept") {
		t.Fatalf("edited baseline: %d %s", code, errOut)
	}
}

func TestAcceptAndVerify(t *testing.T) {
	p, base := acceptProject(t)

	code, out, _ := runCLI(t, "review", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "example:") || !strings.Contains(out, "requests: ") {
		t.Fatalf("review: %d\n%s", code, out)
	}
	// Every request drifts in the header group, so all groups go together.
	m := regexp.MustCompile(`accept:   driftgate accept ((?:group:\d+ )+)--reason`).FindStringSubmatch(out)
	if m == nil || len(strings.Fields(m[1])) < 2 {
		t.Fatalf("review gives no accept command for linked groups:\n%s", out)
	}
	r, _ := lastCompare(t)
	header := groupNumber(t, r, "header-extra header:x-powered-by")
	code, out, _ = runCLI(t, "review", "--config", p.cfg, "--group", strings.TrimPrefix(header, "group:"))
	if code != ExitClean || strings.Count(out, "header:x-powered-by") < 8 || !strings.Contains(out, "faq-create") {
		t.Fatalf("review --group: %d\n%s", code, out)
	}

	code, out, errOut := runCLI(t, "accept", "news", "--reason", "IDs are numbers now", "--ref", "FE-88", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "accepted 1 requests into baseline local: news") {
		t.Fatalf("accept news: %d\n%s\n%s", code, out, errOut)
	}
	log, err := baseline.Log(base)
	if err != nil || len(log) != 1 {
		t.Fatalf("log: %v %+v", err, log)
	}
	a := log[0]
	if a.Reason != "IDs are numbers now" || a.Ref != "FE-88" || a.Capture != runTagFor(t, p.cfg) || len(a.Requests) != 1 || a.Requests[0] != "news" || a.By == "" || a.At == "" || len(a.Signatures) < 3 {
		t.Fatalf("acceptance: %+v", a)
	}
	// The accepted request now passes against the baseline.
	if code, r := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local"); code != ExitDrift || outcome(r, "news") != report.Pass || outcome(r, "me") != report.Drift {
		t.Fatalf("after accept: %d %+v", code, r.Results)
	}

	// The rest, with the command review gives (groups of what is left).
	code, out, _ = runCLI(t, "review", "--config", p.cfg)
	m = regexp.MustCompile(`accept:   driftgate accept ((?:group:\d+ )+)--reason`).FindStringSubmatch(out)
	if code != ExitClean || m == nil {
		t.Fatalf("review: %s", out)
	}
	args := append(append([]string{"accept"}, strings.Fields(m[1])...), "--reason", "v2 adds a header", "--config", p.cfg)
	if code, out, errOut := runCLI(t, args...); code != ExitClean {
		t.Fatalf("%v: %d\n%s\n%s", args, code, out, errOut)
	}
	if code, out, _ := runCLI(t, "review", "--config", p.cfg); !strings.Contains(out, "nothing to review") {
		t.Fatalf("review after accepting all: %d\n%s", code, out)
	}
	if code, _, errOut := runCLI(t, "accept", "*", "--reason", "x", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "matches no request") {
		t.Fatalf("accept with nothing left: %d %s", code, errOut)
	}
	// Private bodies (me, login) included; and a new run passes.
	if code, r := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local"); code != ExitClean {
		t.Fatalf("after accepting all: %d %+v", code, r.Results)
	}
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run after accept: %d\n%s\n%s", code, out, errOut)
	}
	filepath.Walk(filepath.Dir(base), func(path string, info os.FileInfo, _ error) error {
		if strings.Contains(path, ".tmp-") {
			t.Errorf("left behind: %s", path)
		}
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "pera.peric") {
			t.Errorf("private body in the baseline: %s", path)
		}
		return nil
	})
	if log, _ := baseline.Log(base); len(log) != 2 || log[1].Prev == "" {
		t.Fatalf("log: %+v", log)
	}

	code, out, _ = runCLI(t, "verify", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "OK     baseline local") || !strings.Contains(out, "private bodies") {
		t.Fatalf("verify: %d\n%s", code, out)
	}
}

func outcome(r *report.Report, id string) string {
	for _, res := range r.Results {
		if res.ID == id {
			return res.Outcome
		}
	}
	return ""
}

func TestVerifyFindsHandEdits(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	news := filepath.Join(base, "responses", "news.json")
	orig, _ := os.ReadFile(news)
	appendLog := func() {
		if err := baseline.Append(base, baseline.Acceptance{Reason: "first", Capture: "run"}); err != nil {
			t.Fatal(err)
		}
		if err := baseline.Append(base, baseline.Acceptance{Reason: "second", Capture: "run"}); err != nil {
			t.Fatal(err)
		}
		if err := baseline.Write(base); err != nil {
			t.Fatal(err)
		}
	}
	appendLog()
	logPath := filepath.Join(base, baseline.LogFile)
	origLog, _ := os.ReadFile(logPath)
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local"); code != ExitClean {
		t.Fatalf("intact: %d %s", code, out)
	}

	cases := []struct {
		name         string
		change, undo func()
		want         string
	}{
		{"hand edit", func() { write(t, news, strings.Replace(string(orig), "200", "201", 1)) }, func() { write(t, news, string(orig)) }, "responses/news.json: changed"},
		{"deleted file", func() { os.Remove(news) }, func() { write(t, news, string(orig)) }, "responses/news.json: missing"},
		{"extra file", func() { write(t, filepath.Join(base, "responses", "new.json"), "{}") }, func() { os.Remove(filepath.Join(base, "responses", "new.json")) }, "responses/new.json: not in the manifest"},
		{"edited log line", func() { write(t, logPath, strings.Replace(string(origLog), `"first"`, `"other"`, 1)) }, func() { write(t, logPath, string(origLog)) }, "line 2 does not follow line 1"},
	}
	for _, c := range cases {
		c.change()
		code, out, _ := runCLI(t, "verify", "--config", p.cfg)
		if code != ExitDrift || !strings.Contains(out, c.want) {
			t.Errorf("%s: verify %d\n%s", c.name, code, out)
		}
		for _, cmd := range [][]string{{"compare", runTagFor(t, p.cfg)}, {"capture", "c1"}, {"run"}} {
			code, _, errOut := runCLI(t, append(cmd, "--config", p.cfg)...)
			if code != ExitEnv || !strings.Contains(errOut, "baseline local changed outside driftgate accept") || !strings.Contains(errOut, "driftgate verify") {
				t.Errorf("%s: %v: %d %s", c.name, cmd, code, errOut)
			}
		}
		c.undo()
	}

	// A baseline made before manifests: refused until adopted.
	os.Remove(filepath.Join(base, baseline.ManifestFile))
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitDrift || !strings.Contains(out, "--adopt") {
		t.Fatalf("no manifest: %d %s", code, out)
	}
	if code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "no manifest.json") {
		t.Fatalf("compare without manifest: %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "verify", "--config", p.cfg, "--adopt"); code != ExitConfig {
		t.Fatalf("adopt without --baseline: %d %s", code, errOut)
	}
	if code, out, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--adopt"); code != ExitClean {
		t.Fatalf("adopt: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--adopt"); code != ExitConfig || !strings.Contains(errOut, "already has") {
		t.Fatalf("adopt twice: %d %s", code, errOut)
	}
	if code, out, _ := runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg); code != ExitClean {
		t.Fatalf("compare after adopt: %d %s", code, out)
	}
	// Adoption does not cover a broken log.
	os.Remove(filepath.Join(base, baseline.ManifestFile))
	write(t, logPath, strings.Replace(string(origLog), `"first"`, `"other"`, 1))
	if code, _, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--adopt"); code != ExitConfig || !strings.Contains(errOut, "does not follow") {
		t.Fatalf("adopt with a broken log: %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(base, baseline.ManifestFile)); err == nil {
		t.Fatal("refused adoption left a manifest")
	}
}

// TestAcceptAddsRecords: accept takes changed records and one the baseline
// did not have, and the acceptance log names them.
func TestAcceptAddsRecords(t *testing.T) {
	project, cfg := setsFixture(t)
	if code, out, _ := runCLI(t, "verify", "--config", cfg); code != ExitClean {
		t.Fatalf("verify: %d %s", code, out)
	}
	// Mode freeze by default; switch.
	setMode(t, cfg, "evolve")
	if code, _, _ := runCLI(t, "compare", "l56", "--config", cfg); code != ExitDrift {
		t.Fatal("compare")
	}
	if code, out, errOut := runCLI(t, "accept", "pub-*", "api-me", "--reason", "new types", "--config", cfg); code != ExitClean {
		t.Fatalf("accept: %d %s %s", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "compare", "l56", "--config", cfg); code != ExitClean {
		t.Fatalf("compare after accept: %d %s", code, out)
	}
	// pub-new had no baseline record; accepting it added one.
	f, err := os.Open(filepath.Join(project, "driftgate", "baselines", "local", baseline.LogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	var a baseline.Acceptance
	if err := json.Unmarshal(sc.Bytes(), &a); err != nil || strings.Join(a.Requests, ",") != "api-me,pub-new,pub-news" || a.Capture != "l56" {
		t.Fatalf("acceptance: %v %+v", err, a)
	}
}

// TestAcceptAfterReview covers what review found: accept is tied to the
// comparison review showed, takes nothing from a capture made without the
// state check, removes a request that was not captured only when named,
// and an interrupted accept is not mistaken for a missing baseline.
func TestAcceptAfterReview(t *testing.T) {
	// news is private here, so its drifts must be hidden.
	p := newDemoProject(t, "")
	setMode(t, p.cfg, "evolve")
	catalogPath := filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json")
	write(t, catalogPath, strings.Replace(demoCatalog, `"path":"/api/news","storage":"public"`, `"path":"/api/news","storage":"private"`, 1))
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitDrift {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	accept := func(args ...string) (int, string) {
		code, _, errOut := runCLI(t, append([]string{"accept", "--config", p.cfg}, args...)...)
		return code, errOut
	}
	r, _ := lastCompare(t)
	if _, out, _ := runCLI(t, "review", "--config", p.cfg); !strings.Contains(out, "--comparison "+comparisonID(r)) {
		t.Fatalf("review does not tie the accept command to the comparison:\n%s", out)
	}
	if code, errOut := accept("news", "--reason", "x", "--comparison", "000000000000"); code != ExitConfig || !strings.Contains(errOut, "compare ran again") {
		t.Fatalf("another comparison: %d %s", code, errOut)
	}

	// --show-private shows values, but the saved comparison is the same.
	saved := func() string {
		r, _ := lastCompare(t)
		b, _ := json.Marshal(r.Results)
		return string(b)
	}
	runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg)
	plain := saved()
	if !strings.Contains(plain, "(private)") {
		t.Fatal("the demo comparison has no drift in a private body to hide")
	}
	// run names this checkout's capture of driftgate run, as README writes it.
	code, shownOut, errOut := runCLI(t, "compare", "run", "--config", p.cfg, "--show-private")
	if code != ExitDrift || saved() != plain {
		t.Errorf("compare run --show-private: %d %s, or it saved values the plain comparison does not", code, errOut)
	}
	if strings.Contains(shownOut, "(private)") {
		t.Errorf("compare --show-private still hides values:\n%s", shownOut)
	}
	// review --show-private shows them too, in the same groups.
	_, hidden, _ := runCLI(t, "review", "--config", p.cfg)
	_, shown, _ := runCLI(t, "review", "--config", p.cfg, "--show-private")
	if !strings.Contains(hidden, "(private)") || strings.Contains(shown, "(private)") || strings.Count(hidden, "\n[") != strings.Count(shown, "\n[") {
		t.Errorf("review --show-private:\n%s\nplain:\n%s", shown, hidden)
	}
	if saved() != plain {
		t.Error("review --show-private saved values")
	}

	// Part of the catalog captured: a glob must not drop the rest.
	if code, out, errOut := runCLI(t, "capture", runTagFor(t, p.cfg), "--config", p.cfg, "--force", "--only", "news"); code != ExitClean {
		t.Fatalf("capture --only: %d\n%s\n%s", code, out, errOut)
	}
	runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg)
	if code, errOut := accept("*", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "were not captured this time") {
		t.Fatalf("glob over requests not captured: %d %s", code, errOut)
	}

	// A capture without the state check is for diagnosis only.
	if code, out, errOut := runCLI(t, "capture", runTagFor(t, p.cfg), "--config", p.cfg, "--force", "--skip-fingerprint-check"); code != ExitClean {
		t.Fatalf("capture --skip-fingerprint-check: %d\n%s\n%s", code, out, errOut)
	}
	runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg, "--skip-fingerprint-check")
	if code, errOut := accept("news", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "skip-fingerprint-check") {
		t.Fatalf("unreliable capture: %d %s", code, errOut)
	}

	// What an interrupted accept leaves stops run, accept and verify.
	left := filepath.Join(filepath.Dir(base), ".tmp-old-123")
	if err := os.MkdirAll(left, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "interrupted") {
		t.Fatalf("run next to a leftover: %d %s", code, errOut)
	}
	if code, errOut := accept("news", "--reason", "x"); code != ExitEnv {
		t.Fatalf("accept next to a leftover: %d %s", code, errOut)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitDrift || !strings.Contains(out, "interrupted") {
		t.Fatalf("verify next to a leftover: %d %s", code, out)
	}
}

// TestAcceptKeepsOldBaselineWhenSwapFails: when the new baseline cannot be
// moved into place and the old one cannot be moved back, the old one stays
// in .tmp-old-*/baseline, where the next command says it is.
func TestAcceptKeepsOldBaselineWhenSwapFails(t *testing.T) {
	p, base := acceptProject(t)
	manifest, err := os.ReadFile(filepath.Join(base, baseline.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	rename = func(from, to string) error {
		if calls++; calls == 1 {
			return os.Rename(from, to)
		}
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrPermission}
	}
	t.Cleanup(func() { rename = os.Rename })
	if code, _, errOut := runCLI(t, "accept", "news", "--reason", "x", "--config", p.cfg); code == ExitClean || !strings.Contains(errOut, ".tmp-old-") {
		t.Fatalf("accept: exit %d %s", code, errOut)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(base), ".tmp-old-*", "baseline", baseline.ManifestFile))
	if len(left) != 1 {
		t.Fatalf("old baseline not kept: %v", left)
	}
	if raw, _ := os.ReadFile(left[0]); string(raw) != string(manifest) {
		t.Error("the kept baseline is not the old one")
	}
	rename = os.Rename
	if code, _, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, ".tmp-old-*/baseline") {
		t.Errorf("run after the failed accept: exit %d %s", code, errOut)
	}
}

// TestVerifyFindsLeaks: a baseline that holds in git what a capture keeps
// out (an older DriftGate made it, or a hand edit committed with its
// manifest): personal data or a credential in a public body or a header
// fails verify (exit 1), naming the request, the kind and where, never the
// value. A baseline as capture makes it, masked headers included, passes.
func TestVerifyFindsLeaks(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("a baseline as capture makes it: %d\n%s", code, out)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	const email, jwt = "ana.anic@example.com", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NSJ9.c2lnbmF0dXJlLXNpZ25hdHVyZQ"
	body := `{"entries":[{"author":"` + email + `","download":"https://cdn.example.test/f.pdf?token=` + jwt + `"}]}`
	write(t, filepath.Join(base, "bodies", sum(body)), body)
	rec := &snapshot.Record{Version: snapshot.Version, ID: "news", Status: 200,
		Headers: [][2]string{{"content-type", "application/json"}, {"x-debug-user", email}, {"x-auth-token", "a1b2c3d4e5f6a7b8c9d0e1f2"}},
		Body:    snapshot.Body{Storage: snapshot.Public, SHA256: sum(body), Size: int64(len(body))}}
	if err := snapshot.WriteJSON(filepath.Join(base, "responses", "news.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := baseline.Write(base); err != nil { // committed with its manifest
		t.Fatal(err)
	}
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitDrift {
		t.Fatalf("verify: %d\n%s", code, out)
	}
	for _, want := range []string{"FAIL   baseline local: 1 records would publish", "news: ", "email at $.entries[0].author", "credential at $.entries[0].download",
		"email at header:x-debug-user", "credential at header:x-auth-token"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify does not say %q:\n%s", want, out)
		}
	}
	for _, value := range []string{email, jwt, "a1b2c3d4"} {
		if strings.Contains(out, value) {
			t.Errorf("verify prints the value %s:\n%s", value, out)
		}
	}
	if !strings.Contains(out, "driftgate verify --baseline local --migrate-private") {
		t.Errorf("verify names no remedy:\n%s", out)
	}
}

// TestMigratePrivateLeaks: verify --migrate-private brings a baseline of
// an earlier release to what a capture stores now: a public body with personal
// data or a credential, or one that cannot be scanned (a PDF), is stored
// private, header values are masked, the catalog marks the requests
// private, and the acceptance log keeps its lines with one more. It holds
// the store's lock while it writes the store.
func TestMigratePrivateLeaks(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	const email = "ana.anic@example.com"
	body, pdf := `{"author":"`+email+`"}`, "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<<>>\nendobj\n"
	for id, b := range map[string]string{"news": body, "me-anonymous": pdf} {
		ct := "application/json"
		if id != "news" {
			ct = "application/pdf"
		}
		write(t, filepath.Join(base, "bodies", sum(b)), b)
		rec := &snapshot.Record{Version: snapshot.Version, ID: id, Status: 200,
			Headers: [][2]string{{"content-type", ct}, {"x-csrf-token", "a1b2c3d4e5f6a7b8c9d0e1f2"}},
			Body:    snapshot.Body{Storage: snapshot.Public, SHA256: sum(b), Size: int64(len(b))}}
		if err := snapshot.WriteJSON(filepath.Join(base, "responses", id+".json"), rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := baseline.Write(base); err != nil {
		t.Fatal(err)
	}
	logBefore, _ := os.ReadFile(filepath.Join(base, baseline.LogFile))

	// The lock: its file names the holder while the baseline is swapped.
	store := os.Getenv("DRIFTGATE_STORE")
	write(t, filepath.Join(store, storelock.FileName), "0\n")
	holder := -1
	chaos.At = func(point string) {
		if point == "baseline.swap" {
			holder = storelock.Holder(store)
		}
	}
	defer func() { chaos.At = nil }()
	code, out, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--migrate-private")
	if code != ExitClean {
		t.Fatalf("migrate: %d\n%s\n%s", code, out, errOut)
	}
	if holder != os.Getpid() {
		t.Errorf("the store's lock was not held during the migration (holder %d)", holder)
	}
	for _, want := range []string{"stored 2 public bodies of baseline local as private", "masked header values of 2 records", "marked 2 requests storage: private"} {
		if !strings.Contains(out, want) {
			t.Errorf("migrate does not say %q:\n%s", want, out)
		}
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("verify after the migration: %d\n%s", code, out)
	}
	set := &snapshot.Set{Dir: base}
	for _, id := range []string{"news", "me-anonymous"} {
		r, err := set.Record(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Body.Storage != snapshot.Private || r.Body.Ref == "" || r.Body.SHA256 != "" || r.Headers[1][1] == "a1b2c3d4e5f6a7b8c9d0e1f2" {
			t.Errorf("%s: %+v", id, r)
		}
	}
	filepath.Walk(base, func(path string, info os.FileInfo, _ error) error {
		if raw, err := os.ReadFile(path); err == nil && (strings.Contains(string(raw), email) || strings.Contains(string(raw), "%PDF")) {
			t.Errorf("%s still holds what must stay out of git", path)
		}
		return nil
	})
	cat, err := catalog.Load(filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cat.Requests {
		if (r.ID == "news" || r.ID == "me-anonymous") != (r.Storage == snapshot.Private) && r.ID != "me" && r.ID != "login" {
			t.Errorf("catalog: %s is %s", r.ID, r.Storage)
		}
	}
	logAfter, _ := os.ReadFile(filepath.Join(base, baseline.LogFile))
	if !strings.HasPrefix(string(logAfter), string(logBefore)) || strings.Count(string(logAfter), "\n") != strings.Count(string(logBefore), "\n")+1 ||
		!strings.Contains(string(logAfter), leaksReason) {
		t.Errorf("acceptance log:\n%s", logAfter)
	}
	if code, out, errOut := runCLI(t, "verify", "--config", p.cfg, "--baseline", "local", "--migrate-private"); code != ExitConfig || !strings.Contains(errOut, "nothing to migrate") {
		t.Errorf("a second migration: %d\n%s\n%s", code, out, errOut)
	}
}

// TestBaselinesKeepLineEndings: the first run puts a .gitattributes next
// to the baselines that keeps git from changing their line endings
// (core.autocrlf on Windows would break every manifest check); a project's
// own file there stays.
func TestBaselinesKeepLineEndings(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	attrs := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", ".gitattributes")
	if raw, err := os.ReadFile(attrs); err != nil || !strings.Contains(string(raw), "* -text\n") {
		t.Fatalf("%s: %q %v", attrs, raw, err)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean {
		t.Fatalf("verify with the file there: %d\n%s", code, out)
	}
	if err := keepLineEndings(attrs, func(string, []byte, os.FileMode) error { return errors.New("written again") }); err != nil {
		t.Fatal(err)
	}

	// A project of an earlier release may have none: run, verify and accept write it
	// before they check the baseline (accept then has nothing to take).
	setMode(t, p.cfg, "evolve")
	for _, args := range [][]string{{"run"}, {"verify"}, {"accept", "news", "--reason", "x"}} {
		os.Remove(attrs)
		code, out, errOut := runCLI(t, append(args, "--config", p.cfg)...)
		if raw, err := os.ReadFile(attrs); err != nil || !strings.Contains(string(raw), "* -text\n") || !strings.Contains(out, ".gitattributes") {
			t.Errorf("%v: exit %d, %s: %q %v\n%s\n%s", args, code, attrs, raw, err, out, errOut)
		}
	}

	// A baseline git checked out with CRLF says so, with what to do.
	meta := filepath.Join(filepath.Dir(attrs), "local", "meta.json")
	raw, _ := os.ReadFile(meta)
	write(t, meta, strings.ReplaceAll(string(raw), "\n", "\r\n"))
	code, out, _ := runCLI(t, "verify", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "meta.json: changed (only its line endings: CRLF instead of LF)") || !strings.Contains(out, "git rm -r -q --cached") {
		t.Errorf("verify of a CRLF checkout: %d\n%s", code, out)
	}
	if code, _, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "core.autocrlf") {
		t.Errorf("run on a CRLF checkout: %d\n%s", code, errOut)
	}
}

// TestReviewShowPrivateCaptures: review --show-private shows the values of
// private bodies of a comparison of two captures too, and nothing once a
// set changed since the comparison.
func TestReviewShowPrivateCaptures(t *testing.T) {
	p := newDemoProject(t, "")
	catalogPath := filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json")
	write(t, catalogPath, strings.Replace(demoCatalog, `"path":"/api/news","storage":"public"`, `"path":"/api/news","storage":"private"`, 1))
	if code, out, errOut := runCLI(t, "capture", "a", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture a: %d\n%s\n%s", code, out, errOut)
	}
	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "capture", "b", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture b: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "compare", "b", "--against", "a", "--config", p.cfg); code != ExitDrift || !strings.Contains(out, "(private)") ||
		!strings.Contains(out, "values of private bodies are hidden as (private); driftgate review --show-private shows them on this machine") {
		t.Fatalf("compare b against a: %d\n%s", code, out)
	}
	_, hidden, _ := runCLI(t, "review", "--config", p.cfg)
	_, shown, _ := runCLI(t, "review", "--config", p.cfg, "--show-private")
	if !strings.Contains(hidden, "(private)") || strings.Contains(shown, "(private)") || strings.Count(hidden, "\n[") != strings.Count(shown, "\n[") {
		t.Errorf("review --show-private of two captures:\n%s\nplain:\n%s", shown, hidden)
	}
	if code, out, errOut := runCLI(t, "capture", "a", "--force", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture a again: %d\n%s\n%s", code, out, errOut)
	}
	if _, out, _ := runCLI(t, "review", "--config", p.cfg, "--show-private"); !strings.Contains(out, "--show-private shows nothing: capture:a changed since the comparison") || !strings.Contains(out, "(private)") {
		t.Errorf("review --show-private after a set changed:\n%s", out)
	}
}

// TestAcceptAddsRoots: a body accepted from another checkout names its
// folder: the baseline's roots gain it, once.
func TestAcceptAddsRoots(t *testing.T) {
	base, cur := &snapshot.Set{Dir: t.TempDir()}, &snapshot.Set{Dir: t.TempDir()}
	if err := base.WriteMeta(snapshot.Meta{Tag: "local", Roots: []string{"~/Sites/shop"}}); err != nil {
		t.Fatal(err)
	}
	if err := cur.WriteMeta(snapshot.Meta{Tag: "run", Roots: []string{"/tmp/wt/shop", "/private/tmp/wt/shop"}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := addRoots(base, cur); err != nil {
			t.Fatal(err)
		}
	}
	m, err := base.Meta()
	if err != nil || strings.Join(m.Roots, ",") != "~/Sites/shop,/tmp/wt/shop,/private/tmp/wt/shop" || m.Tag != "local" {
		t.Fatalf("baseline meta after accept: %+v %v", m, err)
	}
}
