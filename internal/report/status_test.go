package report

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// redirected drifts like a page that now redirects to the login form: the
// status, its location, a header it no longer sends and the HTML of the
// "Redirecting to …" page.
func redirected(id, to string) Result {
	return Result{ID: id, Outcome: Drift, Drifts: []compare.Drift{
		drift(id, compare.KindStatus, "", "200", "302"),
		drift(id, compare.KindHeaderExtra, "header:location", "", `["`+to+`"]`),
		drift(id, compare.KindHeaderMissing, "header:vary", `["Accept-Encoding"]`, ""),
		drift(id, compare.KindElementExtra, "html>head>meta[0]", "", "meta"),
		drift(id, compare.KindElementExtra, "html>head>meta[1]", "", "meta"),
		drift(id, compare.KindText, "html>head>title", `"Home"`, `"Redirecting to `+to+`"`),
		drift(id, compare.KindText, "html>body", `"<h1> <p>"`, `"Redirecting to <a> ."`),
		drift(id, compare.KindElementMissing, "html>body>h1", "h1", ""),
		drift(id, compare.KindElementExtra, "html>body>a", "", "a"),
	}}
}

// failed drifts like an API that now fails: the status, its content type
// and the error body in place of the data.
func failed(id string) Result {
	return Result{ID: id, Outcome: Drift, Drifts: []compare.Drift{
		drift(id, compare.KindStatus, "", "200", "500"),
		drift(id, compare.KindHeaderValue, "header:content-type", `["application/json"]`, `["application/problem+json"]`),
		drift(id, compare.KindHeaderMissing, "header:x-total", `["3"]`, ""),
		drift(id, compare.KindKeyMissing, "$.data", "data", ""),
		drift(id, compare.KindKeyExtra, "$.message", "", "message"),
		drift(id, compare.KindKeyExtra, "$.exception", "", "exception"),
		drift(id, compare.KindValue, "$.status", `"ok"`, `"error"`),
		drift(id, compare.KindEncoding, "$", "layout", "layout"),
	}}
}

// unchanged drifts in values with the same status: its output must stay
// as it was before status changes were summarized.
func unchanged() []Result {
	var results []Result
	for _, id := range []string{"news-0", "news-1"} {
		results = append(results, Result{ID: id, Outcome: Drift, Drifts: []compare.Drift{
			drift(id, compare.KindType, "$.data[0].id", "int", "string"),
			drift(id, compare.KindHeaderMissing, "header:x-total", `["3"]`, ""),
			drift(id, compare.KindHeaderValue, "header:content-type", `["application/json"]`, `["text/html"]`),
			drift(id, compare.KindValue, "$.title", `"Notes"`, `"Notes `+id+`"`),
		}})
	}
	return append(results,
		Result{ID: "ok", Outcome: Pass},
		Result{ID: "new", Outcome: Skipped, Note: "no baseline"},
		Result{ID: "up", Outcome: Observed, Note: "200, 2 bytes → 200, 3 bytes"},
	)
}

func goldenReport(results []Result) *Report {
	r := Build("run", "baseline:local", results)
	r.Level, r.Mode, r.Created = "values", "evolve", "2026-10-30T10:00:00Z"
	return r
}

// TestStatusGolden writes every format of four comparisons and checks them
// against testdata: (a) a page that now redirects, (b) an API that now
// fails, (c) drifts with the same status, whose output is the one from
// before status changes were summarized, and (d) several requests with the
// same status change. With --max-drifts (*.all.*) the formats that list
// drifts write them as before, every one. go test -run TestStatusGolden
// -update rewrites them.
func TestStatusGolden(t *testing.T) {
	for name, results := range map[string][]Result{
		"redirect":  {redirected("home", "http://localhost:8080"), {ID: "ok", Outcome: Pass}},
		"error":     {failed("api-news"), {ID: "ok", Outcome: Pass}},
		"unchanged": unchanged(),
		"grouped": {
			redirected("home", "/login"), redirected("about", "/login"), redirected("contact", "/login"),
			redirected("admin", "http://localhost:8080/"), failed("api-news"), failed("api-faq"),
			unchanged()[0],
		},
	} {
		for _, f := range Formats {
			var b bytes.Buffer
			if err := goldenReport(results).Write(&b, f, DefaultDrifts); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, name+"."+f, b.Bytes())
			if f == "json" || name == "unchanged" {
				continue
			}
			b.Reset()
			if err := goldenReport(results).Write(&b, f, 20); err != nil {
				t.Fatal(err)
			}
			checkGolden(t, name+".all."+f, b.Bytes())
		}
	}
}

// TestStatusSummary checks the summary in the JSON report, also of a report
// saved before it existed, and which groups the default view leaves out.
func TestStatusSummary(t *testing.T) {
	r := goldenReport([]Result{
		redirected("home", "/login"), redirected("about", "/login"), failed("api-news"),
		{ID: "gone", Outcome: Drift, Drifts: []compare.Drift{drift("gone", compare.KindStatus, "", "200", "error: connection refused")}},
		unchanged()[0],
	})
	home := r.Results[0]
	if *home.StatusChange != (StatusChange{From: "200", To: "302", Location: "/login"}) || home.Collapsed != 7 || len(home.Drifts) != 9 {
		t.Errorf("home: %+v, %d of %d drifts collapsed", home.StatusChange, home.Collapsed, len(home.Drifts))
	}
	if s := r.Results[2].StatusChange; s.Location != "" || r.Results[2].Collapsed != 6 {
		t.Errorf("a 500 has no location: %+v", s)
	}
	if r.Results[4].StatusChange != nil || r.Results[4].Collapsed != 0 {
		t.Errorf("same status: %+v", r.Results[4])
	}
	if len(r.StatusChanges) != 3 || len(r.StatusChanges[0].Requests) != 2 || r.StatusChanges[0].To != "302" {
		t.Errorf("status changes: %+v", r.StatusChanges)
	}
	// Groups touched by a request with the same status stay in view.
	for _, g := range r.Groups {
		hidden := r.hiddenGroups()[g.N]
		if want := g.Signature != "header-missing header:x-total" && g.Signature != "status " &&
			g.Signature != "header-extra header:location" && g.Signature != "header-value header:content-type" &&
			!strings.HasPrefix(g.Signature, "type ") && g.Signature != "value $.title"; hidden != want {
			t.Errorf("group %s hidden %v", g.Signature, hidden)
		}
	}
	// A report saved without the summary gets it when it is read.
	path := filepath.Join(t.TempDir(), "last.json")
	old := *r
	old.StatusChanges = nil
	old.Results = append([]Result{}, r.Results...)
	for i := range old.Results {
		old.Results[i].StatusChange, old.Results[i].Collapsed = nil, 0
	}
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var want, got bytes.Buffer
	r.Text(&want, DefaultDrifts)
	back.Text(&got, DefaultDrifts)
	if want.String() != got.String() {
		t.Errorf("loaded report reads differently:\n%s\nwant\n%s", got.String(), want.String())
	}
	if !strings.Contains(got.String(), "status 200 → error: connection refused\n") {
		t.Errorf("failed request:\n%s", got.String())
	}
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from testdata (go test -run TestStatusGolden -update rewrites it):\n%s", name, got)
	}
}
