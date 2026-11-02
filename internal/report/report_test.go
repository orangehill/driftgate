package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

func drift(req, kind, where, base, cur string) compare.Drift {
	return compare.Drift{Request: req, Kind: kind, Where: where, Baseline: base, Current: cur}
}

// sample has one group of each common kind, a skipped and an observed request.
func sample() *Report {
	var results []Result
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("news-%d", i)
		ds := []compare.Drift{
			drift(id, compare.KindType, "$.data[0].id", "int", "string"),
			drift(id, compare.KindType, "$.data[1].id", "int", "string"),
			drift(id, compare.KindStatus, "", "200", "500"),
			drift(id, compare.KindHeaderMissing, "header:x-total", `["3"]`, ""),
			drift(id, compare.KindValue, "$.title", `"Notes"`, fmt.Sprintf(`"Notes %d"`, i)),
		}
		results = append(results, Result{ID: id, Outcome: Drift, Drifts: ds})
	}
	results = append(results,
		Result{ID: "faq", Outcome: Drift, Drifts: []compare.Drift{drift("faq", compare.KindHeaderValue, "header:cache-control", `["no-cache"]`, `["max-age=60"]`)}},
		Result{ID: "ok", Outcome: Pass},
		Result{ID: "new", Outcome: Skipped, Note: "no baseline"},
		Result{ID: "up", Outcome: Observed, Note: "200, 2 bytes → 200, 3 bytes"},
	)
	r := Build("run", "baseline:local", results)
	r.Level, r.Mode, r.Created = "values", "evolve", "2026-10-29T10:00:00Z"
	return r
}

func group(t *testing.T, r *Report, sig string) Group {
	t.Helper()
	for _, g := range r.Groups {
		if g.Signature == sig {
			return g
		}
	}
	t.Fatalf("no group %s", sig)
	return Group{}
}

func TestExplanations(t *testing.T) {
	r := sample()
	for sig, want := range map[string]string{
		"type $.data[*].id int→string":      "6 drifts in 3 requests: $.data[*].id changed type int → string",
		"status ":                           "3 drifts in 3 requests: status changed (200 → 500)",
		"header-missing header:x-total":     "3 drifts in 3 requests: header x-total is missing",
		"value $.title":                     `3 drifts in 3 requests: $.title changed value (for example "Notes" → "Notes 0")`,
		"header-value header:cache-control": `1 drift in 1 request: header cache-control changed value (["no-cache"] → ["max-age=60"])`,
	} {
		if got := group(t, r, sig).Explanation; got != want {
			t.Errorf("%s:\n got %s\nwant %s", sig, got, want)
		}
	}
	for _, c := range []struct {
		d    compare.Drift
		want string
	}{
		{drift("a", compare.KindKeyExtra, "$.items[3].slug", "", "slug"), "$.items[*].slug is new"},
		{drift("a", compare.KindArrayLength, "$.items", "10", "12"), "$.items changed length (10 → 12)"},
		{drift("a", compare.KindEncoding, "$", "layout", "layout"), "the same values with a different layout of whitespace"},
		{drift("a", compare.KindSideEffect, "side-effects:db/table:faq", "unchanged", "changed"), "side effect on db/table:faq (unchanged → changed)"},
	} {
		if got := what(c.d, true); got != c.want {
			t.Errorf("%s: got %q, want %q", c.d.Kind, got, c.want)
		}
	}
	// A long value is clipped in the explanation.
	long := what(drift("a", compare.KindValue, "$.body", `"`+strings.Repeat("x", 500)+`"`, `"y"`), true)
	if len([]rune(long)) > 120 {
		t.Errorf("long value not clipped: %s", long)
	}
}

func TestLocateAndText(t *testing.T) {
	r := sample()
	r.Locate(func(id string) *Location {
		switch {
		case strings.HasPrefix(id, "news-"):
			return &Location{Route: "GET /api/news/{id}", Action: `App\Http\Controllers\NewsController@show`, File: "app/Http/Controllers/NewsController.php:42"}
		case id == "faq":
			return &Location{Route: "GET /faq", Action: "app/faq/page.tsx", File: "src/app/faq/page.tsx"}
		}
		return nil
	})
	g := group(t, r, "type $.data[*].id int→string")
	if g.RouteCount != 1 || g.Routes[0].Action != `App\Http\Controllers\NewsController@show` {
		t.Fatalf("locations %+v", g)
	}
	var b bytes.Buffer
	r.Text(&b, 20)
	out := b.String()
	for _, want := range []string{
		"[1] 6 drifts in 3 requests: $.data[*].id changed type int → string\n      GET /api/news/{id}  App\\Http\\Controllers\\NewsController@show  app/Http/Controllers/NewsController.php:42\n",
		"GET /faq  src/app/faq/page.tsx\n", // the action is the file: once
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	// The explanation and routes are in the JSON too.
	var js bytes.Buffer
	if err := r.Write(&js, "json", 20); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || back.Groups[0].Explanation == "" || back.Groups[0].Routes[0].File == "" {
		t.Errorf("json groups: %v %+v", err, back.Groups)
	}
}

func TestJUnit(t *testing.T) {
	r := sample()
	// A control character in a value must not break the XML.
	r.Results[3].Drifts[0].Current = "[\"a\x01<b>&\"]"
	var b bytes.Buffer
	if err := r.Write(&b, "junit", 2); err != nil {
		t.Fatal(err)
	}
	var doc junitSuites
	if err := xml.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, b.String())
	}
	if len(doc.Suites) != 1 {
		t.Fatalf("suites: %d", len(doc.Suites))
	}
	s := doc.Suites[0]
	if doc.Tests != 7 || s.Tests != 7 || s.Failures != 4 || s.Skipped != 2 || len(s.Cases) != 7 {
		t.Errorf("counts: %+v", s)
	}
	cases := map[string]junitCase{}
	for _, c := range s.Cases {
		if c.Classname == "" || c.Name == "" {
			t.Errorf("case without name or classname: %+v", c)
		}
		cases[c.Name] = c
	}
	f := cases["news-0"].Failure
	if f == nil || !strings.HasPrefix(f.Message, "5 drifts: type $.data[0].id  int → string; type $.data[1].id") || !strings.Contains(f.Message, "… 3 more") {
		t.Errorf("failure: %+v", f)
	}
	if f != nil && strings.Count(f.Text, "\n") != 2 {
		t.Errorf("failure text: %q", f.Text)
	}
	if cases["ok"].Failure != nil || cases["ok"].Skipped != nil {
		t.Errorf("passed case: %+v", cases["ok"])
	}
	if c := cases["new"]; c.Skipped == nil || c.Skipped.Message != "skipped: no baseline" {
		t.Errorf("skipped: %+v", c)
	}
	if c := cases["up"]; c.Skipped == nil || !strings.HasPrefix(c.Skipped.Message, "observed: ") {
		t.Errorf("observed: %+v", c)
	}
	if !strings.HasPrefix(b.String(), "<?xml") {
		t.Error("no XML declaration")
	}
}

func TestMarkdown(t *testing.T) {
	r := sample()
	r.Results[0].ID = "news-`0`"
	md := r.Markdown(20)
	for _, want := range []string{
		"### DriftGate: ✗ 4 of 5 compared requests drifted\n",
		"| 5 | 1 | 4 | 1 | 1 |",
		"#### What changed",
		`1. 6 drifts in 3 requests: $.data\[\*\].id changed type int → string`,
		"<details><summary><code>news-`0`</code> — 5 drifts</summary>",
		"<details><summary>2 skipped or observed</summary>",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "Not shown") {
		t.Error("a small report was cut")
	}
	clean := Build("run", "baseline:local", []Result{{ID: "a", Outcome: Pass}})
	if md := clean.Markdown(20); !strings.HasPrefix(md, "### DriftGate: ✓ no drift in 1 requests") {
		t.Errorf("clean verdict: %s", md)
	}
}

func TestMarkdownLimit(t *testing.T) {
	var results []Result
	for i := 0; i < 3000; i++ {
		id := fmt.Sprintf("request-%04d", i)
		var ds []compare.Drift
		for j := 0; j < 30; j++ {
			ds = append(ds, drift(id, compare.KindValue, fmt.Sprintf("$.field%d.value%d", i, j), `"`+strings.Repeat("ž", 200)+`"`, `"x"`))
		}
		results = append(results, Result{ID: id, Outcome: Drift, Drifts: ds})
	}
	r := Build("run", "baseline:local", results)
	md := r.Markdown(20)
	if len(md) >= MarkdownLimit {
		t.Fatalf("markdown is %d bytes, limit %d", len(md), MarkdownLimit)
	}
	if !strings.Contains(md, "_Not shown, to stay under GitHub's comment limit:") || !strings.Contains(md, "drifted requests") || !strings.Contains(md, "groups") {
		t.Errorf("no note on what was cut:\n%s", md[len(md)-400:])
	}
	if strings.Count(md, "<details>") != strings.Count(md, "</details>") {
		t.Error("unbalanced details")
	}
}

func TestFormats(t *testing.T) {
	r := sample()
	for _, f := range Formats {
		var b bytes.Buffer
		if err := r.Write(&b, f, 20); err != nil || b.Len() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := r.Write(&bytes.Buffer{}, "xml", 20); err == nil {
		t.Error("unknown format accepted")
	}
}

// TestMarkdownNoMentions: values from responses do not mention people or
// link issues in a pull-request comment.
func TestMarkdownNoMentions(t *testing.T) {
	got := mdText("owner @team-a in #12")
	if strings.Contains(got, "@team-a") || strings.Contains(got, " #12") {
		t.Errorf("mdText kept a mention or an issue link: %s", got)
	}
}

// TestTextOutputIsInert: keys and values from responses cannot start a line
// of their own (GitHub reads "::warning::" at the start of a log line as a
// workflow command) or send terminal sequences.
func TestTextOutputIsInert(t *testing.T) {
	rec := &snapshot.Record{Status: 200, Headers: [][2]string{{"content-type", "application/json"}}}
	evil := `\n::warning title=pwn::injected\n\u001b]0;evil\u0007\u009b31m` + "\u202e"
	base := []byte(`{"` + evil + `a": 1, "note": "` + evil + ` a", "k": {"` + evil + `": 1}}`)
	cur := []byte(`{"` + evil + `b": 1, "note": "` + evil + ` b", "k": {"` + evil + `": "x"}}`)
	ds := compare.One("x", compare.Side{Record: rec, Body: base}, compare.Side{Record: rec, Body: cur}, allowlist.Empty().ForTarget(""), compare.LevelValues)
	if len(ds) < 4 {
		t.Fatalf("drifts: %+v", ds)
	}
	r := Build("run", "baseline:local", []Result{{ID: "x", Outcome: Drift, Drifts: ds}})
	var b bytes.Buffer
	r.Text(&b, 20)
	out := b.String()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "::") {
			t.Errorf("a line starts with :: — %q", l)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07\u009b\u202e") {
		t.Errorf("control characters in the output:\n%q", out)
	}
}

// TestSkipPrivateWithNothingCompared: --skip-private accepts a partial
// check, not one that compared nothing.
func TestSkipPrivateWithNothingCompared(t *testing.T) {
	r := &Report{PrivateMissing: []string{"me"}, SkipPrivate: true}
	if !r.Incomplete() {
		t.Error("nothing compared, yet complete")
	}
	r.Compared = 1
	if r.Incomplete() {
		t.Error("a partial check with --skip-private is complete")
	}
}

// TestNothingCompared: a comparison that compared nothing is never green,
// whatever the reason, and the report says why; requests without a
// baseline are listed in every format.
func TestNothingCompared(t *testing.T) {
	cases := map[string][]Result{
		"no request was selected":            nil,
		"the baseline has none":              {{ID: "a", Outcome: Skipped, Note: NoteNoBaseline}, {ID: "b", Outcome: Skipped, Note: NoteNoBaseline}},
		"nothing was compared":               {{ID: "a", Outcome: Observed, Note: "200 → 200"}},
		"every request is private and its b": {{ID: "a", Outcome: Skipped, Note: NotePrivateMissing}, {ID: "b", Outcome: Skipped, Note: NoteNoBaseline}},
	}
	for want, results := range cases {
		r := Build("cur", "baseline:local", results)
		r.SkipPrivate = true
		var text strings.Builder
		r.Text(&text, DefaultDrifts)
		if !r.Incomplete() || !strings.Contains(r.NothingNote(), want) || !strings.Contains(text.String(), want) || strings.Contains(text.String(), "No drift") {
			t.Errorf("%s: incomplete %v, note %q\n%s", want, r.Incomplete(), r.NothingNote(), text.String())
		}
		if md := r.Markdown(DefaultDrifts); !strings.Contains(md, "✗ incomplete, nothing compared") || !strings.Contains(md, want) {
			t.Errorf("%s: markdown\n%s", want, md)
		}
	}
	r := Build("cur", "baseline:local", []Result{{ID: "a", Outcome: Pass}, {ID: "new", Outcome: Skipped, Note: NoteNoBaseline}})
	var js, text strings.Builder
	r.JSON(&js)
	r.Text(&text, DefaultDrifts)
	if r.Incomplete() || !strings.Contains(js.String(), `"no_baseline": [`) || !strings.Contains(text.String(), "1 request without a baseline, not compared: new") ||
		!strings.Contains(r.Markdown(DefaultDrifts), "without a baseline") {
		t.Errorf("one new request:\n%s\n%s", js.String(), text.String())
	}
}
