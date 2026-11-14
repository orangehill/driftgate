package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// expectProject is acceptProject in mode freeze: every request drifts in
// the last comparison, and the baseline must stay as it is.
func expectProject(t *testing.T) (*demoProject, string) {
	t.Helper()
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	p.api.SetVersion("v2")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitDrift {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	return p, filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
}

func result(r *report.Report, id string) report.Result {
	for _, res := range r.Results {
		if res.ID == id {
			return res
		}
	}
	return report.Result{}
}

// editRecord changes a record of a capture in place.
func editRecord(t *testing.T, tag, id string, change func(*snapshot.Record)) {
	t.Helper()
	path := filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", tag, "responses", id+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec snapshot.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	change(&rec)
	out, _ := json.Marshal(rec)
	write(t, path, string(out))
}

func setHeader(rec *snapshot.Record, name, value string) {
	for i, h := range rec.Headers {
		if h[0] == name {
			rec.Headers[i][1] = value
			return
		}
	}
	rec.Headers = append(rec.Headers, [2]string{name, value})
}

// TestExpect: in mode freeze, drifts recorded with expect pass compare
// only while they are exactly the recorded ones; the baseline stays as it
// is, and every format shows what was expected.
func TestExpect(t *testing.T) {
	p, base := expectProject(t)
	tag := runTagFor(t, p.cfg)
	manifest, _ := os.ReadFile(filepath.Join(base, baseline.ManifestFile))
	expect := func(args ...string) (int, string, string) {
		return runCLI(t, append([]string{"expect", "--config", p.cfg}, args...)...)
	}

	for _, args := range [][]string{{"news", "--reason", "x"}, {"news", "--ref", "T-1"}, {"news", "--ref", " ", "--reason", "x"}} {
		if code, _, errOut := expect(args...); code != ExitConfig || !strings.Contains(errOut, "expect needs") {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"nope"}, {"zz*"}, {"group:99"}, {"--drop", "T-1", "news"}, {"news", "--baseline", "local"}} {
		if code, _, errOut := expect(append(args, "--ref", "T-1", "--reason", "x")...); code != ExitConfig {
			t.Fatalf("%v: %d %s", args, code, errOut)
		}
	}
	// A group expects its drifts only: the requests still drift otherwise.
	r, _ := lastCompare(t)
	header := groupNumber(t, r, "header-extra header:x-powered-by")
	if code, out, errOut := expect(header, "--ref", "T-1", "--reason", "x"); code != ExitClean || !strings.Contains(out, "expected 7 drifts in 7 requests") {
		t.Fatalf("group: %d\n%s\n%s", code, out, errOut)
	}
	// Five requests drift only in that header and are expected; news and
	// news-item drift in their bodies too and still fail.
	if r, _ := lastCompare(t); r.Expected != 5 || r.Unexpected() != 2 || result(r, "news").Expected != nil || result(r, "news").Drifts[0].Expected != "T-1" || result(r, "news").Drifts[1].Expected != "" {
		t.Fatalf("group: expected %d, unexpected %d: %+v", r.Expected, r.Unexpected(), result(r, "news"))
	}
	if code, out, errOut := expect("--drop", "T-1", "--reason", "the header is not the ticket's"); code != ExitClean {
		t.Fatalf("drop: %d\n%s\n%s", code, out, errOut)
	}

	code, out, errOut := expect("news", "me", "--ref", "API-7", "--reason", "IDs are numbers in v2")
	if code != ExitClean || !regexp.MustCompile(`expected \d+ drifts in 2 requests of baseline local for API-7: me, news`).MatchString(out) || !strings.Contains(out, filepath.Join("local.expectations", "API-7.jsonl")) {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if now, _ := os.ReadFile(filepath.Join(base, baseline.ManifestFile)); string(now) != string(manifest) {
		t.Fatal("expect changed the baseline")
	}
	exps, _, err := baseline.Expectations(base)
	if err != nil || len(exps) != 2 || exps[0].ID != "me" || !strings.HasPrefix(exps[0].Drifts[0], "ref:") || !strings.HasPrefix(exps[1].Drifts[0], "sha256:") || exps[1].Ref != "API-7" || len(exps[1].Signatures) == 0 {
		t.Fatalf("expectations: %v %+v", err, exps)
	}
	// The last comparison shows them at once.
	if code, out, _ := runCLI(t, "report", "--config", p.cfg, "--exit-code"); code != ExitDrift || !strings.Contains(out, "API-7 (IDs are numbers in v2): 2 requests: news, me") || strings.Contains(out, "DRIFT  news\n") || !strings.Contains(out, "DRIFT  me-anonymous") {
		t.Fatalf("report: %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "review", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "expected (driftgate expect), by ref:\n  API-7  IDs are numbers in v2\n    requests: news, me") ||
		!regexp.MustCompile(`expect:   driftgate expect group:\d+ --ref <ticket>`).MatchString(out) || strings.Contains(out, "accept:") {
		t.Fatalf("review:\n%s", out)
	}
	if code, _, errOut := expect("news", "--ref", "API-8", "--reason", "x"); code != ExitConfig || !strings.Contains(errOut, "news is expected already (API-7)") {
		t.Fatalf("twice: %d %s", code, errOut)
	}
	// The same ref again replaces its expectation of the request.
	if code, out, _ := expect("news", "--ref", "API-7", "--reason", "IDs are numbers in v2"); code != ExitClean || !strings.Contains(out, "the earlier expectation of API-7 for news names these drifts already") {
		t.Fatalf("again: %d\n%s", code, out)
	}
	if again, _, _ := baseline.Expectations(base); len(again) != 2 {
		t.Fatalf("superseded: %+v", again)
	}

	// The rest, and compare passes: nothing drifted unexpectedly.
	if code, out, errOut := expect("*", "--ref", "API-9", "--reason", "v2 adds a header"); code != ExitClean {
		t.Fatalf("expect *: %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = runCLI(t, "compare", tag, "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "No unexpected drift ✓") || !strings.Contains(out, "API-7 (IDs are numbers in v2): 2 requests: news, me") {
		t.Fatalf("compare: %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "run", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "No unexpected drift ✓") {
		t.Fatalf("run: %d\n%s", code, out)
	}
	for format, want := range map[string]string{"markdown": "match their expectations exactly", "junit": "<system-out>expected drift (API-7)", "json": `"expected": "API-7"`} {
		if code, out, _ := runCLI(t, "report", "--config", p.cfg, "--format", format, "--exit-code"); code != ExitClean || !strings.Contains(out, want) || strings.Contains(out, "<failure") {
			t.Fatalf("%s: %d\n%s", format, code, out)
		}
	}
	if code, out, _ := runCLI(t, "review", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "every drift is expected") {
		t.Fatalf("review: %d\n%s", code, out)
	}

	// A private response's drifts are named by a keyed hash of their
	// values, the same with --show-private.
	_, shown, _ := runCLI(t, "compare", tag, "--config", p.cfg, "--format", "json", "--show-private")
	var withValues report.Report
	if err := json.Unmarshal([]byte(shown), &withValues); err != nil {
		t.Fatal(err)
	}
	var digests []string
	for _, d := range result(&withValues, "me").Drifts {
		digests = append(digests, d.Digest)
	}
	slices.Sort(digests)
	if strings.Join(digests, " ") != strings.Join(exps[0].Drifts, " ") || len(result(&withValues, "me").Expected) != 1 {
		t.Fatalf("--show-private: %v, want %v", digests, exps[0].Drifts)
	}

	// The same kind of drift in the same place with another value is not
	// the expected one; nor is one drift more.
	if code, out, errOut := runCLI(t, "capture", "other", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	editRecord(t, "other", "news", func(r *snapshot.Record) { setHeader(r, "x-powered-by", "demo 2") })
	editRecord(t, "other", "faq-create", func(r *snapshot.Record) { setHeader(r, "x-extra", "1") })
	code, r = compareJSON(t, p.cfg, "other", "local")
	if code != ExitDrift || r.Expected != r.Drifted-2 || result(r, "news").Expected != nil || result(r, "faq-create").Expected != nil || result(r, "news-item").Expected[0] != "API-9" {
		t.Fatalf("other values: %d %+v", code, r.Results)
	}
	unmet := map[string]string{}
	for _, u := range r.Unmet {
		unmet[u.Request] = u.Ref + " " + u.Why
	}
	if !strings.HasPrefix(unmet["news"], "API-7 partly met: ") || len(unmet) != 1 {
		t.Fatalf("unmet: %+v", r.Unmet)
	}

	// The API back as the baseline has it: nothing drifts, every
	// expectation is unmet, which is a warning only.
	p.api.SetVersion("v1")
	code, out, _ = runCLI(t, "run", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "No drift ✓") || !strings.Contains(out, "warning: 7 expectations not met") || !strings.Contains(out, "API-7 me (no drift)") {
		t.Fatalf("v1: %d\n%s", code, out)
	}
	p.api.SetVersion("v2")

	// Dropped by ref, its requests drift again.
	if code, _, errOut := expect("--drop", "API-7"); code != ExitConfig || !strings.Contains(errOut, "--reason") {
		t.Fatalf("drop without a reason: %d %s", code, errOut)
	}
	if code, out, errOut := expect("--drop", "API-7", "--reason", "done"); code != ExitClean || !strings.Contains(out, "dropped the expectations of API-7 from baseline local: me, news") {
		t.Fatalf("drop: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := expect("--drop", "API-7", "--reason", "x", "--baseline", "local"); code != ExitConfig || !strings.Contains(errOut, "has no expectation with ref API-7 in force (it has API-9") {
		t.Fatalf("drop twice: %d %s", code, errOut)
	}
	code, out, _ = runCLI(t, "run", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "DRIFT  news\n") || !strings.Contains(out, "API-9 (v2 adds a header): 5 requests: news-missing, me-anonymous, news-item") {
		t.Fatalf("after drop: %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "5 expectations in force (driftgate expect), of 3 refs in local.expectations/") {
		t.Fatalf("verify: %d\n%s", code, out)
	}

	// An expectations file changed by hand is refused as a changed
	// baseline is: its expectations would pass drifts on its word.
	file := filepath.Join(baseline.ExpectDir(base), "API-9.jsonl")
	good, _ := os.ReadFile(file)
	write(t, file, strings.Replace(string(good), `"ref":"API-9"`, `"ref":"API-0"`, 1))
	if code, _, errOut := runCLI(t, "compare", tag, "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "changed outside driftgate expect") {
		t.Fatalf("edited: %d %s", code, errOut)
	}
	if code, out, _ := runCLI(t, "verify", "--config", p.cfg); code != ExitDrift || !strings.Contains(out, "FAIL   baseline local: local.expectations/API-9.jsonl changed outside driftgate expect") {
		t.Fatalf("verify edited: %d\n%s", code, out)
	}
	if code, _, errOut := expect("--drop", "API-9", "--reason", "x"); code != ExitEnv {
		t.Fatalf("drop from an edited file: %d %s", code, errOut)
	}
	os.Remove(file)
	if code, _, _ := runCLI(t, "compare", tag, "--config", p.cfg); code != ExitDrift {
		t.Fatal("without the file its drifts fail")
	}
}

// TestExpectEvolve: expectations work in mode evolve too, and a capture
// compared with a capture has none.
func TestExpectEvolve(t *testing.T) {
	p, _ := acceptProject(t)
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "capture", "c2", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %s", out)
	}
	code, r := compareJSON(t, p.cfg, "c2", runTagFor(t, p.cfg))
	if code != ExitClean || r.Expected != 0 || len(r.Unmet) != 0 {
		t.Fatalf("capture against capture: %d %+v", code, r)
	}
	if code, _, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "expect takes drifts of a comparison against a baseline") {
		t.Fatalf("expect from a capture comparison: %d %s", code, errOut)
	}
}

// perfBaseline is perfProject with capture a made the baseline local.
func perfBaseline(tb testing.TB, n, scale int) (string, string) {
	tb.Helper()
	cfg := perfProject(tb, n, scale)
	a := filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", "a")
	base := filepath.Join(filepath.Dir(cfg), "driftgate", "baselines", "local")
	if err := copyTree(a, base); err != nil {
		tb.Fatal(err)
	}
	return cfg, base
}

func compareBaseline(tb testing.TB, cfg string) *report.Report {
	tb.Helper()
	var out, errOut bytes.Buffer
	if code := Main([]string{"compare", "b", "--config", cfg, "--format", "json"}, &out, &errOut); code != ExitDrift && code != ExitClean {
		tb.Fatalf("compare: exit %d\n%s", code, errOut.String())
	}
	var r report.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		tb.Fatal(err)
	}
	return &r
}

// BenchmarkCompareExpected compares a capture of 3,000 requests (80%
// private) with a baseline, without expectations and with one for every
// drifted request (in 30 refs).
func BenchmarkCompareExpected(b *testing.B) {
	cfg, base := perfBaseline(b, 3000, 16)
	b.Run("none", func(b *testing.B) {
		for b.Loop() {
			compareBaseline(b, cfg)
		}
	})
	r := compareBaseline(b, cfg)
	byRef := map[string][]baseline.Expected{}
	n := 0
	for _, res := range r.Results {
		if report.Expectable(res) && res.ID != "get-r-2999" {
			e := baseline.Expected{ID: res.ID}
			for _, d := range res.Drifts {
				e.Drifts = append(e.Drifts, d.Digest)
			}
			ref := fmt.Sprintf("T-%d", n%30)
			byRef[ref] = append(byRef[ref], e)
			n++
		}
	}
	for ref, list := range byRef {
		if err := baseline.AppendExpectation(base, baseline.ExpectLine{Op: baseline.OpExpect, Ref: ref, Reason: "r", Requests: list}); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("expected", func(b *testing.B) {
		for b.Loop() {
			if r := compareBaseline(b, cfg); r.Unexpected() != 1 {
				b.Fatalf("unexpected %d of %d", r.Unexpected(), r.Drifted)
			}
		}
	})
}

// TestDigestCoversWitnessNotSideEffects: two drifts that read the same
// but differ in what the description leaves out get other digests; a
// side effect, which names what changed and not its values, gets none.
func TestDigestCoversWitnessNotSideEffects(t *testing.T) {
	digest := digester(nil)
	req := catalog.Request{ID: "a"}
	d := compare.Drift{Request: "a", Kind: compare.KindKeyExtra, Where: "$.is_admin", Current: `"is_admin"`, Witness: "1"}
	other := d
	other.Witness = "2"
	if a, b := digest(req, false, d), digest(req, false, other); a == "" || a == b {
		t.Fatalf("witness not digested: %s %s", a, b)
	}
	side := compare.Drift{Request: "a", Kind: compare.KindSideEffect, Where: "side-effects:db/table:users", Baseline: "unchanged", Current: "changed"}
	if got := digest(req, false, side); got != "" {
		t.Fatalf("side effect digested: %s", got)
	}
	if report.Expectable(report.Result{ID: "a", Outcome: report.Drift, Drifts: []compare.Drift{{Digest: "sha256:1"}, side}}) {
		t.Fatal("a request with a side effect is expectable")
	}
}

// TestExpectImport: a ref's file from another checkout is taken after it
// is checked; one of another baseline, or one that disagrees, is not.
func TestExpectImport(t *testing.T) {
	p, base := expectProject(t)
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	src := filepath.Join(baseline.ExpectDir(base), "T-1.jsonl")
	file := filepath.Join(t.TempDir(), "T-1.jsonl")
	raw, _ := os.ReadFile(src)
	write(t, file, string(raw))
	os.RemoveAll(baseline.ExpectDir(base))
	imp := func(args ...string) (int, string, string) {
		return runCLI(t, append([]string{"expect", "--config", p.cfg, "--import", file}, args...)...)
	}
	if code, out, errOut := imp(); code != ExitClean || !strings.Contains(out, "imported the expectations of T-1 into baseline local") {
		t.Fatalf("import: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := imp(); code != ExitClean || !strings.Contains(out, "has the expectations of T-1 already") {
		t.Fatalf("again: %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "report", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "T-1 (x): 1 request: news") {
		t.Fatalf("report after import:\n%s", out)
	}
	// Another baseline of the project: the file names local.
	if err := copyTree(base, filepath.Join(filepath.Dir(base), "release")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := imp("--baseline", "release"); code != ExitConfig || !strings.Contains(errOut, `of baseline "local", not release`) {
		t.Fatalf("other baseline: %d %s", code, errOut)
	}
	// --drop works on the baseline it names.
	if code, _, errOut := runCLI(t, "expect", "--config", p.cfg, "--drop", "T-1", "--reason", "x", "--baseline", "release"); code != ExitConfig || !strings.Contains(errOut, "baseline release has no expectation with ref T-1 in force (it has none in force") {
		t.Fatalf("drop from release: %d %s", code, errOut)
	}
	// A file that disagrees with the ref's file here.
	if code, out, errOut := runCLI(t, "expect", "--config", p.cfg, "--drop", "T-1", "--reason", "x"); code != ExitClean {
		t.Fatalf("drop: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := imp(); code != ExitConfig || !strings.Contains(errOut, "has more of ref T-1") {
		t.Fatalf("shorter file: %d %s", code, errOut)
	}
	// Two files of the ref that went apart: the message says to delete
	// this one, as a drop would only add a line.
	dropped := filepath.Join(t.TempDir(), "T-1.jsonl")
	raw2, _ := os.ReadFile(src)
	write(t, dropped, string(raw2))
	os.Remove(src)
	if code, _, errOut := imp(); code != ExitClean {
		t.Fatalf("import: %d %s", code, errOut)
	}
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "z", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := runCLI(t, "expect", "--config", p.cfg, "--import", dropped); code != ExitConfig || !strings.Contains(errOut, "delete this baseline's file of the ref") {
		t.Fatalf("forked: %d %s", code, errOut)
	}
	write(t, file, strings.Replace(string(raw), `"reason":"x"`, `"reason":"y"`, 1))
	if code, _, errOut := imp(); code != ExitConfig || !strings.Contains(errOut, "changed outside driftgate expect") {
		t.Fatalf("edited file: %d %s", code, errOut)
	}
}

// TestAcceptClosesExpectations: accepting a request takes its expected
// drifts into the baseline, and closes the expectations of them.
func TestAcceptClosesExpectations(t *testing.T) {
	p, base := acceptProject(t)
	for _, ref := range []string{"T-1", "T-2"} {
		id := map[string]string{"T-1": "news", "T-2": "me"}[ref]
		if code, out, errOut := runCLI(t, "expect", id, "--ref", ref, "--reason", "x", "--config", p.cfg); code != ExitClean {
			t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
		}
	}
	code, out, errOut := runCLI(t, "accept", "news", "--reason", "IDs are numbers", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "closed the expectations of T-1 the baseline now holds: news") || strings.Contains(out, "T-2") {
		t.Fatalf("accept: %d\n%s\n%s", code, out, errOut)
	}
	if exps, _, err := baseline.Expectations(base); err != nil || len(exps) != 1 || exps[0].Ref != "T-2" {
		t.Fatalf("expectations: %v %+v", err, exps)
	}
}

// TestRebaseClosesGoneRequests: an expectation of a request the catalog
// no longer has is closed by a rebase.
func TestRebaseClosesGoneRequests(t *testing.T) {
	p, base := expectProject(t)
	if code, out, errOut := runCLI(t, "expect", "faq-create", "--ref", "T-3", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	cat := filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json")
	raw, _ := os.ReadFile(cat)
	write(t, cat, strings.Replace(string(raw), `,
 {"id":"faq-create","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"}`, "", 1))
	if code, out, errOut := runCLI(t, "capture", "main", "--repeat", "2", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := runCLI(t, "baselines", "rebase", "local", "--from", "main", "--reason", "faq gone", "--allow-freeze", "--config", p.cfg)
	if code != ExitClean || !strings.Contains(out, "(1 new, 1 no longer in the catalog)") && !strings.Contains(out, "1 no longer in the catalog") || !strings.Contains(out, "closed the expectations of T-3 the baseline now holds: faq-create") {
		t.Fatalf("rebase: %d\n%s\n%s", code, out, errOut)
	}
	if exps, _, _ := baseline.Expectations(base); len(exps) != 0 {
		t.Fatalf("expectations: %+v", exps)
	}
}

// TestBisectSkipsExpected: bisect does not chase a drift a ticket
// intends, unless asked to.
func TestBisectSkipsExpected(t *testing.T) {
	p, _ := expectProject(t)
	r, _ := lastCompare(t)
	header := groupNumber(t, r, "header-extra header:x-powered-by")
	if code, out, errOut := runCLI(t, "expect", header, "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	proj, cat, _, err := loadProject(p.cfg)
	if err != nil {
		t.Fatal(err)
	}
	chased := func(withExpected bool) string {
		ch, err := chooseChase(proj, cat, nil, "", "", withExpected)
		if err != nil {
			t.Fatal(err)
		}
		var sigs []string
		for _, g := range ch.groups {
			sigs = append(sigs, g.Signature)
		}
		return strings.Join(sigs, "; ")
	}
	if got := chased(false); strings.Contains(got, "x-powered-by") || got == "" {
		t.Fatalf("without --expected: %s", got)
	}
	if got := chased(true); !strings.Contains(got, "x-powered-by") {
		t.Fatalf("with --expected: %s", got)
	}
	// Only expected drifts left: nothing to bisect.
	if code, out, errOut := runCLI(t, "expect", "*", "--ref", "T-2", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := chooseChase(proj, cat, nil, "", "", false); err == nil || !strings.Contains(err.Error(), "nothing to bisect") {
		t.Fatalf("all expected: %v", err)
	}
	// news is expected by both tickets together.
	if r, _ := lastCompare(t); strings.Join(result(r, "news").Expected, ",") != "T-1,T-2" || r.Unexpected() != 0 {
		t.Fatalf("two refs: %+v", result(r, "news"))
	}
}

// TestExpectNotInCatalog: an expectation of a request the catalog no
// longer has is listed as unmet.
func TestExpectNotInCatalog(t *testing.T) {
	p, _ := expectProject(t)
	if code, out, errOut := runCLI(t, "expect", "faq-create", "--ref", "T-3", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	cat := filepath.Join(filepath.Dir(p.cfg), "driftgate", "catalog.json")
	raw, _ := os.ReadFile(cat)
	write(t, cat, strings.Replace(string(raw), `,
 {"id":"faq-create","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"}`, "", 1))
	code, r := compareJSON(t, p.cfg, runTagFor(t, p.cfg), "local")
	if code != ExitDrift || len(r.Unmet) != 1 || r.Unmet[0].Request != "faq-create" || r.Unmet[0].Why != report.UnmetNotCatalog {
		t.Fatalf("%d %+v", code, r.Unmet)
	}
}

// TestExpectOtherOrigin: an expectation recorded on one address holds for
// the same drift captured from another (a worktree on another port); a
// URL of another host still differs.
func TestExpectOtherOrigin(t *testing.T) {
	p, _ := expectProject(t)
	tag := runTagFor(t, p.cfg)
	editRecord(t, tag, "news", func(r *snapshot.Record) { setHeader(r, "link", p.url+"/next") })
	if code, _, _ := runCLI(t, "compare", tag, "--config", p.cfg); code != ExitDrift {
		t.Fatal("compare")
	}
	if code, out, errOut := runCLI(t, "expect", "news", "--ref", "T-1", "--reason", "x", "--config", p.cfg); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	other := httptest.NewServer(demo.New("v2"))
	t.Cleanup(other.Close)
	raw, _ := os.ReadFile(p.cfg)
	write(t, p.cfg, strings.ReplaceAll(string(raw), p.url, other.URL))
	if code, out, errOut := runCLI(t, "capture", "moved", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	editRecord(t, "moved", "news", func(r *snapshot.Record) { setHeader(r, "link", other.URL+"/next") })
	if _, r := compareJSON(t, p.cfg, "moved", "local"); len(result(r, "news").Expected) != 1 {
		t.Fatalf("another address: %+v", result(r, "news"))
	}
	editRecord(t, "moved", "news", func(r *snapshot.Record) { setHeader(r, "link", "http://elsewhere.test/next") })
	if _, r := compareJSON(t, p.cfg, "moved", "local"); result(r, "news").Expected != nil {
		t.Fatalf("another host: %+v", result(r, "news"))
	}
}

// TestExpectSameRefAddsOtherRefKeeps: expecting a second group of a
// request with the same ref adds to its expectation; another ref does not
// take the drifts a ref expects already.
func TestExpectSameRefAddsOtherRefKeeps(t *testing.T) {
	p, base := expectProject(t)
	expect := func(args ...string) (int, string, string) {
		return runCLI(t, append([]string{"expect", "--config", p.cfg}, args...)...)
	}
	newsOf := func(ref string) []string {
		exps, _, err := baseline.Expectations(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range exps {
			if e.ID == "news" && e.Ref == ref {
				return e.Drifts
			}
		}
		return nil
	}
	r, _ := lastCompare(t)
	header := groupNumber(t, r, "header-extra header:x-powered-by")
	ids := groupNumber(t, r, "type $.entries[*].id string→int")
	if code, out, errOut := expect(header, "--ref", "T-1", "--reason", "x"); code != ExitClean {
		t.Fatalf("expect: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := expect(ids, "--ref", "T-1", "--reason", "x")
	if code != ExitClean || !strings.Contains(out, "this adds 2 drifts to the earlier expectation of T-1 for news") {
		t.Fatalf("second group: %d\n%s\n%s", code, out, errOut)
	}
	if got := newsOf("T-1"); len(got) != 3 {
		t.Fatalf("T-1 of news: %v", got)
	}
	// T-2 takes news whole: the three drifts of T-1 stay T-1's.
	news := result(r, "news")
	code, out, errOut = expect("news", "--ref", "T-2", "--reason", "y")
	if code != ExitClean || !strings.Contains(out, "left out 3 drifts that T-1 expects already") {
		t.Fatalf("T-2: %d\n%s\n%s", code, out, errOut)
	}
	if got := newsOf("T-2"); len(got) != len(news.Drifts)-3 {
		t.Fatalf("T-2 of news: %d of %d", len(got), len(news.Drifts))
	}
	if r, _ := lastCompare(t); strings.Join(result(r, "news").Expected, ",") != "T-1,T-2" {
		t.Fatalf("news: %+v", result(r, "news"))
	}
	// One of the two refs that expect news together takes it again.
	if code, out, errOut := expect("news", "--ref", "T-1", "--reason", "x"); code != ExitClean || !strings.Contains(out, "the earlier expectation of T-1 for news names these drifts already") {
		t.Fatalf("T-1 again: %d\n%s\n%s", code, out, errOut)
	}
	// Everything of news expected: nothing left for a third ref.
	if code, _, errOut := expect(ids, "--ref", "T-3", "--reason", "z"); code != ExitConfig || !strings.Contains(errOut, "nothing to expect: the selected drifts are expected") {
		t.Fatalf("T-3: %d %s", code, errOut)
	}
}

// TestCompareChecksBodiesOnce: compare checks the baseline's public bodies
// as it reads them, and the ones it does not read before it uses a result:
// a body changed by hand stops it either way.
func TestCompareChecksBodiesOnce(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	tag := runTagFor(t, p.cfg)
	var rec snapshot.Record
	raw, _ := os.ReadFile(filepath.Join(base, "responses", "news.json"))
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(base, "bodies", rec.Body.SHA256)
	good, _ := os.ReadFile(body)
	p.api.SetVersion("v2") // news drifts: its body is read
	if code, out, errOut := runCLI(t, "capture", "v2", "--config", p.cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	write(t, body, strings.Replace(string(good), "a", "b", 1))
	for _, args := range [][]string{{"v2"}, {tag}, {"v2", "--only", "me*"}} {
		code, _, errOut := runCLI(t, append([]string{"compare", "--config", p.cfg}, args...)...)
		if code != ExitEnv || !strings.Contains(errOut, "baseline local changed outside driftgate accept") {
			t.Errorf("%v: %d %s", args, code, errOut)
		}
	}
	write(t, body, string(good))
	if code, _, errOut := runCLI(t, "compare", tag, "--config", p.cfg); code != ExitClean {
		t.Fatalf("restored: %d %s", code, errOut)
	}
	// A record changed by hand (still valid JSON), read by the comparison
	// or not.
	record := filepath.Join(base, "responses", "news.json")
	write(t, record, string(raw)+" ")
	for _, args := range [][]string{{tag}, {tag, "--only", "me*"}} {
		code, _, errOut := runCLI(t, append([]string{"compare", "--config", p.cfg}, args...)...)
		if code != ExitEnv || !strings.Contains(errOut, "baseline local changed outside driftgate accept (responses/news.json: changed") {
			t.Errorf("record %v: %d %s", args, code, errOut)
		}
	}
}

// TestCompareTamperedRecords: a baseline record changed by hand in a way
// that fails before the comparison (no JSON, another version, a private
// body by plain hash or a ref, a body that is not there) stops compare as
// any changed file does, with the baseline on either side.
func TestCompareTamperedRecords(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	base := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	tag := runTagFor(t, p.cfg)
	path := filepath.Join(base, "responses", "news.json")
	raw, _ := os.ReadFile(path)
	var rec snapshot.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	enc := func(r snapshot.Record) string { b, _ := json.Marshal(r); return string(b) }
	for name, content := range map[string]string{
		"no JSON":    "{" + string(raw),
		"version":    enc(func(r snapshot.Record) snapshot.Record { r.Version = 99; return r }(rec)),
		"plain hash": enc(func(r snapshot.Record) snapshot.Record { r.Body.Storage = "private"; return r }(rec)),
		"private ref": enc(func(r snapshot.Record) snapshot.Record {
			r.Body.Storage, r.Body.Ref, r.Body.SHA256 = "private", "deadbeef", ""
			return r
		}(rec)),
		"missing body": enc(func(r snapshot.Record) snapshot.Record { r.Body.SHA256 = strings.Repeat("a", 64); return r }(rec)),
	} {
		write(t, path, content)
		for _, args := range [][]string{{tag}, {"baseline:local", "--against", tag}} {
			code, out, errOut := runCLI(t, append([]string{"compare", "--config", p.cfg}, args...)...)
			if code != ExitEnv || out != "" || !strings.Contains(errOut, "baseline local changed outside driftgate accept (responses/news.json: changed)") {
				t.Errorf("%s %v: %d %q %s", name, args, code, out, errOut)
			}
		}
	}
}
