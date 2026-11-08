package cli

import (
	"encoding/xml"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/generate"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestReport compares two captures and writes the last comparison again
// in every format, with the groups explained from routes.json.
func TestReport(t *testing.T) {
	project, cfg := setsFixture(t)
	if code, _, errOut := runCLI(t, "report", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "no comparison yet") {
		t.Fatalf("report before compare: %d %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "report", "--exit-code", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "no comparison yet") {
		t.Fatalf("report --exit-code before compare: %d %s", code, errOut)
	}
	write(t, filepath.Join(project, "driftgate", "routes.json"), `{"version":1,"routes":[
 {"methods":["GET"],"path":"/public-api/news","action":"App\\Http\\Controllers\\NewsController@index","file":"app/Http/Controllers/NewsController.php:14","source":"laravel"},
 {"methods":["GET"],"path":"/api/auth/me","action":"App\\Http\\Controllers\\AuthController@me","source":"laravel"}]}`)

	code, out, _ := runCLI(t, "compare", "l56", "--config", cfg)
	if code != ExitDrift {
		t.Fatalf("compare: %d\n%s", code, out)
	}
	for _, want := range []string{
		"1 drift in 1 request: $.entries[*].id changed type string → int\n      GET /public-api/news  App\\Http\\Controllers\\NewsController@index  app/Http/Controllers/NewsController.php:14\n",
		"1 drift in 1 request: $.user.id changed type int → string\n      GET /api/auth/me  App\\Http\\Controllers\\AuthController@me\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compare text lacks %q:\n%s", want, out)
		}
	}

	reportExits(t, cfg, ExitDrift)

	code, out, _ = runCLI(t, "report", "--config", cfg, "--format", "markdown")
	if code != ExitClean || !strings.HasPrefix(out, "### DriftGate: ✗ 2 of 3 compared requests drifted") || !strings.Contains(out, "`app/Http/Controllers/NewsController.php:14`") {
		t.Errorf("markdown (%d):\n%s", code, out)
	}
	code, out, _ = runCLI(t, "report", "--config", cfg, "--format", "junit")
	var doc junitDoc
	if err := xml.Unmarshal([]byte(out), &doc); code != ExitClean || err != nil || doc.Suite.Failures != 2 || doc.Suite.Skipped != 1 {
		t.Errorf("junit (%d, %v): %+v\n%s", code, err, doc, out)
	}
	// compare writes the new formats directly too.
	if code, out, _ := runCLI(t, "compare", "l56", "--config", cfg, "--format", "junit"); code != ExitDrift || !strings.HasPrefix(out, "<?xml") {
		t.Errorf("compare --format junit: %d\n%s", code, out)
	}
	for _, args := range [][]string{{"report", "--format", "xml"}, {"report", "extra"}} {
		if code, _, _ := runCLI(t, append(args, "--config", cfg)...); code != ExitConfig {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	// No private body reaches any format: only the drift, as text shows it.
	for _, f := range []string{"text", "json", "junit", "markdown"} {
		_, out, _ := runCLI(t, "report", "--config", cfg, "--format", f)
		if strings.Contains(out, "a@b.c") {
			t.Errorf("%s shows a private body:\n%s", f, out)
		}
	}
}

// reportExits checks the exit codes of report after a comparison whose
// verdict was want: 0 by default (a CI step renders several formats after
// run), want with --exit-code, in every format.
func reportExits(t *testing.T, cfg string, want int) {
	t.Helper()
	for _, f := range report.Formats {
		if code, out, errOut := runCLI(t, "report", "--config", cfg, "--format", f); code != ExitClean {
			t.Errorf("report --format %s: exit %d, want 0\n%s\n%s", f, code, out, errOut)
		}
		code, out, errOut := runCLI(t, "report", "--config", cfg, "--format", f, "--exit-code")
		if code != want || out == "" {
			t.Errorf("report --format %s --exit-code: exit %d, want %d\n%s\n%s", f, code, want, out, errOut)
		}
	}
}

type junitDoc struct {
	Suite struct {
		Failures int `xml:"failures,attr"`
		Skipped  int `xml:"skipped,attr"`
	} `xml:"testsuite"`
}

// TestHidePrivateValues: a drift in a private body keeps its kind and place,
// not the values; types, formats, headers and status stay as they are.
func TestHidePrivateValues(t *testing.T) {
	drifts := []compare.Drift{
		{Kind: compare.KindValue, Where: "$.user.email", Baseline: `"pera@example.com"`, Current: `"mika@example.com"`},
		{Kind: compare.KindEncoding, Where: "$.name", Baseline: `"ž"`, Current: `"ž"`},
		{Kind: compare.KindType, Where: `$.users["pera.peric@example.com"].id`, Baseline: "int", Current: "string"},
		{Kind: compare.KindFormat, Where: "$.at", Baseline: `"2024-03-01 10:00:00"`, Current: `"1. 3. 2024."`},
		{Kind: compare.KindText, Where: "html>body>p", Baseline: "Pera Perić", Current: "Mika Mikić"},
		{Kind: compare.KindStatus, Baseline: "200", Current: "500"},
		{Kind: compare.KindHeaderValue, Where: "header:x-total", Baseline: "1", Current: "2"},
		{Kind: compare.KindStatus, Baseline: "200", Current: "error: Get: dial tcp 10.0.0.1:80: i/o timeout"},
	}
	hidePrivateValues(drifts)
	for i, want := range [][2]string{
		{"(private)", "(private)"}, {"(private)", "(private)"}, {"int", "string"},
		{"(private)", "(private)"}, {"(private)", "(private)"}, {"200", "500"}, {"1", "2"},
		{"200", "error (private)"},
	} {
		if got := [2]string{drifts[i].Baseline, drifts[i].Current}; got != want {
			t.Errorf("%s %s: %v, want %v", drifts[i].Kind, drifts[i].Where, got, want)
		}
	}
	if drifts[2].Where != `$.users["(private)"].id` {
		t.Errorf("a key that is data stays in the path: %s", drifts[2].Where)
	}
	if isPrivate(nil) || !isPrivate(&snapshot.Record{Body: snapshot.Body{Storage: snapshot.Private}}) {
		t.Error("isPrivate")
	}
}

// TestReportStatusChange: by default a request whose status changed reads
// as the change and one line for its body; --max-drifts lists every drift.
func TestReportStatusChange(t *testing.T) {
	p := newDemoProject(t, "")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	proj, err := config.Load(p.cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := lastReport(proj)
	if err != nil {
		t.Fatal(err)
	}
	d := func(kind, where, base, cur string) compare.Drift {
		return compare.Drift{Request: "home", Kind: kind, Where: where, Baseline: base, Current: cur}
	}
	r := report.Build("run", "baseline:local", []report.Result{{ID: "home", Outcome: report.Drift, Drifts: []compare.Drift{
		d(compare.KindStatus, "", "200", "302"),
		d(compare.KindHeaderExtra, "header:location", "", `["/login"]`),
		d(compare.KindElementMissing, "html>body>h1", "h1", ""),
		d(compare.KindText, "html>head>title", `"Home"`, `"Redirecting to /login"`),
	}}})
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runCLI(t, "report", "--config", p.cfg)
	for _, want := range []string{
		"DRIFT  home\n         status 200 → 302, redirects to /login\n         header-extra    header:location   → [\"/login\"]\n         body differs: 2 html drifts; listed with --max-drifts N and in the JSON report\n",
		"      1 request: 200 → 302 to /login (home)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runCLI(t, "report", "--config", p.cfg, "--max-drifts", "20")
	if !strings.Contains(out, "element-missing html>body>h1") || strings.Contains(out, "body differs") {
		t.Errorf("--max-drifts does not list every drift:\n%s", out)
	}
}

// TestDriftLineAroundTheDifference: review and review --group show the
// part of two long values where they differ.
func TestDriftLineAroundTheDifference(t *testing.T) {
	head, tail := strings.Repeat("a", 150), strings.Repeat("z", 150)
	d := compare.Drift{Kind: compare.KindValue, Where: "$.body", Baseline: `"` + head + "OLD-WORD" + tail + `"`, Current: `"` + head + "NEW-WORD" + tail + `"`}
	if l := driftLine(d); !strings.Contains(l, "OLD-WORD") || !strings.Contains(l, "NEW-WORD") {
		t.Errorf("drift line %q", l)
	}
}

// TestPrintReplaced: generate and run name the generated requests that
// entries written by hand take the place of.
func TestPrintReplaced(t *testing.T) {
	var b strings.Builder
	printReplaced(&b, nil)
	if b.Len() != 0 {
		t.Errorf("nothing replaced: %q", b.String())
	}
	printReplaced(&b, []generate.Replaced{
		{Route: "GET /api/limited", ByHand: []string{"limited-throttle", "limited-throttle-json"}, Generated: []string{"get-api-limited", "get-api-limited-json"}},
		{Route: "POST /api/import", ByHand: []string{"import-real"}},
	})
	want := "\nnote: entries written by hand take the place of the generated requests of 2 route methods (generate --all makes those too):\n" +
		"  GET /api/limited: limited-throttle, limited-throttle-json instead of get-api-limited, get-api-limited-json\n" +
		"  POST /api/import: import-real instead of its generated requests\n"
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
