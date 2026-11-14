package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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
		// Only in requests whose status changed: after the groups the
		// default view lists.
		"[3] 6 drifts in 3 requests: $.data[*].id changed type int → string\n      GET /api/news/{id}  App\\Http\\Controllers\\NewsController@show  app/Http/Controllers/NewsController.php:42\n",
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
		`3. 6 drifts in 3 requests: $.data\[\*\].id changed type int → string`,
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

// TestResentCounted: requests sent again after a restore are counted in a
// line of their own in every format, so the count shows even when the
// list of the requests' notes is cut after ten.
func TestResentCounted(t *testing.T) {
	var results []Result
	for i := range 12 {
		results = append(results, Result{ID: fmt.Sprintf("r%02d", i), Outcome: Pass, Resent: true,
			Notes: []string{"sent again in the capture: the first answer was 500, after the restore of db ended 12 database sessions of the application"}})
	}
	results = append(results, Result{ID: "other", Outcome: Pass})
	r := Build("cur", "baseline:main", results)
	if len(r.Resent) != 12 {
		t.Fatalf("resent %v", r.Resent)
	}
	want := "12 requests were sent again after a restore"
	for _, f := range Formats {
		var b bytes.Buffer
		if err := r.Write(&b, f, 20); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		switch f {
		case "json":
			if !strings.Contains(out, `"resent": [`) || !strings.Contains(out, `"resent": true`) {
				t.Errorf("json: %s", out)
			}
		case "junit":
			if !strings.Contains(out, `<property name="resent" value="12">`) || !strings.Contains(out, want) {
				t.Errorf("junit: %s", out)
			}
		default:
			if !strings.Contains(out, want) {
				t.Errorf("%s: %s", f, out)
			}
		}
	}
	if (&Report{}).ResentNote() != "" || !strings.Contains(Build("c", "b", results[:1]).ResentNote(), "1 request was sent again") {
		t.Error("ResentNote")
	}
}

// TestShortPairAroundTheDifference: two long values are cut alike around
// the first character where they differ, so a change in the middle of a
// long text shows in the drift line and in the group's explanation, a
// quoted one inside its quotes; short values and a difference at the start
// are shown as before.
func TestShortPairAroundTheDifference(t *testing.T) {
	head, tail := strings.Repeat("a", 150), strings.Repeat("z", 150)
	b, c := `"`+head+"OLD-WORD"+tail+`"`, `"`+head+"NEW-WORD"+tail+`"`
	sb, sc := ShortPair(b, c, 80)
	for _, s := range []string{sb, sc} {
		if !strings.HasPrefix(s, `"…`) || !strings.HasSuffix(s, `…"`) || len([]rune(s)) > 80 {
			t.Errorf("window %q (%d characters)", s, len([]rune(s)))
		}
	}
	if !strings.Contains(sb, "OLD-WORD") || !strings.Contains(sc, "NEW-WORD") {
		t.Errorf("the difference is not shown: %q → %q", sb, sc)
	}
	if x, y := ShortPair("short", "other", 80); x != "short" || y != "other" {
		t.Errorf("short values changed: %q %q", x, y)
	}
	// A difference at the start: the beginnings, as before.
	if x, _ := ShortPair("X"+head, "Y"+head, 80); x != "X"+head[:78]+"…" {
		t.Errorf("start: %q", x)
	}
	// One value a prefix of the other: the window ends where the shorter does.
	if x, y := ShortPair(head+head, head+head+"MORE", 80); !strings.HasSuffix(x, "a") || !strings.HasPrefix(x, "…") || !strings.Contains(y, "MORE") {
		t.Errorf("prefix: %q → %q", x, y)
	}
	// A value that only starts with a quote is not a quoted string.
	if x, _ := ShortPair(`"`+head+"OLD"+tail, `"`+head+"NEW"+tail, 80); !strings.HasPrefix(x, "…a") || !strings.HasSuffix(x, "z…") {
		t.Errorf("unclosed quote: %q", x)
	}
	// Multibyte text is cut on characters.
	if x, _ := ShortPair(strings.Repeat("é", 200)+"1", strings.Repeat("é", 200)+"2", 40); !utf8.ValidString(x) || !strings.HasSuffix(x, "1") {
		t.Errorf("runes: %q", x)
	}

	d := compare.Drift{Request: "r", Kind: compare.KindValue, Where: "$.body", Baseline: b, Current: c}
	r := Build("capture:c", "baseline:local", []Result{{ID: "r", Outcome: Drift, Drifts: []compare.Drift{d}}})
	var out bytes.Buffer
	r.Text(&out, DefaultDrifts)
	if lines := strings.Split(out.String(), "\n"); !strings.Contains(lines[1], "OLD-WORD") || !strings.Contains(lines[1], "NEW-WORD") {
		t.Errorf("drift line:\n%s", out.String())
	}
	if e := r.Groups[0].Explanation; !strings.Contains(e, "OLD-WORD") || !strings.Contains(e, "NEW-WORD") {
		t.Errorf("explanation %q", e)
	}
}

// TestNoBaselineAdviceByMode: run tells how new requests join the baseline
// with the command that works in the comparison's mode: accept in evolve,
// a rebase with --allow-freeze in freeze, which refuses accept.
func TestNoBaselineAdviceByMode(t *testing.T) {
	r := Build("capture:run-1", "baseline:local", []Result{{ID: "new", Outcome: Skipped, Note: NoteNoBaseline}, {ID: "old", Outcome: Pass}})
	r.Baseline = "local"
	for mode, want := range map[string]string{
		"evolve": "1 request without a baseline, not compared: new; new requests join the baseline with driftgate accept",
		"":       "new requests join the baseline with driftgate accept",
		"freeze": `in mode freeze new requests join the baseline with driftgate capture new --repeat 2 --force, then driftgate baselines rebase local --from new --reason "…" --allow-freeze, which takes the whole capture, its drifts too`,
	} {
		r.Mode = mode
		if n := r.NoBaselineNote(); !strings.Contains(n, want) || mode == "freeze" && strings.Contains(n, "accept") {
			t.Errorf("mode %q: %q, want %q", mode, n, want)
		}
	}
	r.Mode, r.NotCovered = "freeze", &NotCovered{Only: true}
	if n := r.NoBaselineNote(); !strings.Contains(n, "a capture of the whole catalog (driftgate capture new --repeat 2 --force, then driftgate baselines rebase local") {
		t.Errorf("--only: %q", n)
	}
	// Next to a drift, even an expected one, no rebase command: it would
	// take the drift into the baseline.
	for _, other := range []Result{
		{ID: "old", Outcome: Drift, Drifts: []compare.Drift{{Request: "old", Kind: compare.KindValue, Where: "$.a"}}},
		{ID: "old", Outcome: Pass, Expected: []string{"T-1"}, Drifts: []compare.Drift{{Request: "old", Kind: compare.KindValue, Where: "$.a", Expected: "T-1"}}},
	} {
		d := Build("capture:run-1", "baseline:local", []Result{{ID: "new", Outcome: Skipped, Note: NoteNoBaseline}, other})
		d.Baseline, d.Mode = "local", "freeze"
		if n := d.NoBaselineNote(); strings.Contains(n, "--from") || !strings.Contains(n, "fix or expect the drifts first, then rebase to add the new requests") {
			t.Errorf("with a drift (%s): %q", other.Outcome, n)
		}
	}
	// Not a capture against a baseline: two captures, or two baselines
	// (compare baseline:a against baseline:b). Freeze never names accept,
	// and there is no capture to give as --from.
	for _, sides := range [][2]string{{"capture:b", "capture:a"}, {"baseline:next", "baseline:local"}} {
		r = Build(sides[0], sides[1], []Result{{ID: "new", Outcome: Skipped, Note: NoteNoBaseline}})
		r.Baseline, _ = strings.CutPrefix(sides[1], "baseline:")
		if !strings.HasPrefix(sides[1], "baseline:") {
			r.Baseline = ""
		}
		r.Mode = "freeze"
		n := r.NoBaselineNote()
		if strings.Contains(n, "join the baseline with driftgate accept") || strings.Contains(n, "--from next") || !strings.Contains(n, "in mode freeze accept is refused") {
			t.Errorf("%s against %s: %q", sides[0], sides[1], n)
		}
		r.Mode = "evolve"
		if n := r.NoBaselineNote(); !strings.Contains(n, "driftgate accept") {
			t.Errorf("evolve, %s against %s: %q", sides[0], sides[1], n)
		}
	}
}

// TestRemovedHeaderValueReads: a cookie no longer set reads as removed, a
// new one as new, not as a value off its allowlist format.
func TestRemovedHeaderValueReads(t *testing.T) {
	d := compare.Drift{Request: "r", Kind: compare.KindFormat, Where: "header:set-cookie", Baseline: "(private)", Current: "(private)",
		Values: [2][]string{{"XSRF-TOKEN=x; path=/", "laravel-session=x; path=/"}, {"XSRF-TOKEN=y; path=/"}}}
	for _, c := range []struct {
		d    compare.Drift
		want string
	}{
		{d, "header:set-cookie has 1 value instead of 2: laravel-session removed"},
		{compare.Drift{Kind: d.Kind, Where: d.Where, Values: [2][]string{d.Values[1], d.Values[0]}}, "header:set-cookie has 2 values instead of 1: laravel-session new"},
		{compare.Drift{Kind: d.Kind, Where: "header:x-trace", Values: [2][]string{{"a"}, {"a", "b", "c"}}}, "header:x-trace has 3 values instead of 1: 2 values new"},
		{compare.Drift{Kind: d.Kind, Where: "header:date", Baseline: `["a"]`, Current: `["b"]`}, "header:date no longer matches its allowlist format"},
	} {
		r := Build("capture:c", "baseline:local", []Result{{ID: "r", Outcome: Drift, Drifts: []compare.Drift{c.d}}})
		if e := r.Groups[0].Explanation; !strings.Contains(e, c.want) {
			t.Errorf("explanation %q, want %q", e, c.want)
		}
	}
}

// TestShortPairKeepsCharactersWhole: a window does not start or end inside
// a character the eye sees as one: an accent stays on its letter, a
// joined emoji stays whole.
func TestShortPairKeepsCharactersWhole(t *testing.T) {
	for _, unit := range []string{"é", "👨‍👩‍👧", "👍🏽", "❤️"} {
		for pad := range 4 {
			b := strings.Repeat("a", pad) + strings.Repeat(unit, 120) + "OLD" + strings.Repeat(unit, 120)
			c := strings.Repeat("a", pad) + strings.Repeat(unit, 120) + "NEW" + strings.Repeat(unit, 120)
			x, y := ShortPair(b, c, 80)
			for _, w := range []string{x, y} {
				inner := strings.TrimSuffix(strings.TrimPrefix(w, "…"), "…")
				if !strings.HasPrefix(inner, unit) && !strings.HasPrefix(inner, "a") || !strings.HasSuffix(inner, unit) || len([]rune(w)) > 80 {
					t.Errorf("%q, pad %d: window cuts a character or is too long: %q", unit, pad, w)
				}
			}
			if !strings.Contains(x, "OLD") || !strings.Contains(y, "NEW") {
				t.Errorf("%q: difference not shown: %q → %q", unit, x, y)
			}
		}
	}
}

// TestShortPairKeepsEscapesWhole: a window over a quoted JSON value does
// not start or end inside a backslash escape (\", \\, \uXXXX), keeps the
// outer quotes and shows the difference, wherever the escapes fall.
func TestShortPairKeepsEscapesWhole(t *testing.T) {
	cycle := strings.Repeat(`\"\\é\u00e9`, 20)
	var original string
	if err := json.Unmarshal([]byte(`"`+cycle+`"`), &original); err != nil {
		t.Fatal(err)
	}
	// pad moves where the window starts among the escapes, tail where it ends.
	for pad := range 12 {
		for tail := range 12 {
			b := `"` + cycle + strings.Repeat("a", pad) + "OLD" + strings.Repeat("a", tail) + cycle + `"`
			c := `"` + cycle + strings.Repeat("a", pad) + "NEW" + strings.Repeat("a", tail) + cycle + `"`
			x, y := ShortPair(b, c, 80)
			for _, w := range []string{x, y} {
				if !strings.HasPrefix(w, `"…`) || !strings.HasSuffix(w, `…"`) || len([]rune(w)) > 80 {
					t.Errorf("pad %d, tail %d: outer quotes not kept or window too long: %s", pad, tail, w)
				}
				inner := strings.TrimPrefix(strings.TrimPrefix(w, `"`), "…")
				inner = strings.TrimSuffix(strings.TrimSuffix(inner, `"`), "…")
				var text string
				if err := json.Unmarshal([]byte(`"`+inner+`"`), &text); err != nil {
					t.Errorf("pad %d, tail %d: window splits an escape: %s", pad, tail, w)
					continue
				}
				// \u00e9 cut to u00e9 is still valid JSON, but not the text.
				head, _, _ := strings.Cut(text, "a")
				head, _, _ = strings.Cut(head, "OLD")
				head, _, _ = strings.Cut(head, "NEW")
				if !strings.Contains(original, head) {
					t.Errorf("pad %d, tail %d: window starts inside an escape: %s", pad, tail, w)
				}
			}
			if !strings.Contains(x, "OLD") || !strings.Contains(y, "NEW") {
				t.Errorf("pad %d, tail %d: difference not shown: %s → %s", pad, tail, x, y)
			}
		}
	}
}

// TestShortPairCutsAfterAnEscapedBackslash: \\ is a whole escape, so a
// window may end right after it; the backslash before it does not start
// another escape with the next character.
func TestShortPairCutsAfterAnEscapedBackslash(t *testing.T) {
	// The window is the first 78 characters: " X, 74 a, then \\.
	rest := strings.Repeat("a", 74) + `\\` + strings.Repeat("a", 100) + `"`
	if x, _ := ShortPair(`"X`+rest, `"Y`+rest, 80); !strings.HasSuffix(x, `a\\…"`) || len([]rune(x)) != 80 {
		t.Errorf("cut after \\\\: %s", x)
	}
}

// TestDriftPairCompactsCookies: a Set-Cookie drift shows each masked
// cookie as its name and length with the attributes in full, so an
// attribute added at the end shows; a value that is not a list of cookies,
// such as (private), is shown as it is.
func TestDriftPairCompactsCookies(t *testing.T) {
	token := strings.Repeat("xx0x", 80)
	d := drift("r", compare.KindHeaderValue, "header:set-cookie", `["XSRF-TOKEN=`+token+`"]`, `["XSRF-TOKEN=`+token+`; samesite=lax"]`)
	r := Build("capture:c", "baseline:local", []Result{{ID: "r", Outcome: Drift, Drifts: []compare.Drift{d}}})
	var out bytes.Buffer
	r.Text(&out, DefaultDrifts)
	line := strings.Split(out.String(), "\n")[1]
	for _, s := range []string{line, r.Groups[0].Explanation} {
		if !strings.Contains(s, "XSRF-TOKEN=…(320)") || !strings.Contains(s, "samesite=lax") || strings.Contains(s, "xxxx") {
			t.Errorf("set-cookie drift shown as %q", s)
		}
	}
	// Cookies of the same length: compacted alike, so shown as they are.
	if b, c := DriftPair(drift("r", compare.KindHeaderValue, "header:set-cookie", `["a=xx0"]`, `["a=xxx"]`), 80); b != `["a=xx0"]` || c != `["a=xxx"]` {
		t.Errorf("same length: %s → %s", b, c)
	}

	// Two cookies of the same number: the one that did not change is …,
	// one whose masked value changed shape shows it, even at the same
	// length.
	if b, c := DriftPair(drift("r", compare.KindHeaderValue, "header:set-cookie", `["a=xxx","b=xx0","c=xx"]`, `["a=xxxx","b=x0x","c=xx"]`), 80); b != `["a=xxx","b=xx0","…"]` || c != `["a=xxxx","b=x0x","…"]` {
		t.Errorf("two changed cookies: %s → %s", b, c)
	}
	// Laravel: the session cookie gains an attribute; the line names it.
	xsrf, session := "XSRF-TOKEN="+token+"; path=/; samesite=lax", "laravel_session="+token+"; path=/; httponly"
	laravel := drift("r", compare.KindHeaderValue, "header:set-cookie", `["`+xsrf+`","`+session+`"]`, `["`+xsrf+`","`+session+`; samesite=lax"]`)
	if b, c := DriftPair(laravel, shortMax); !strings.Contains(b, "laravel_session=…(320); path=/; httponly") || !strings.Contains(c, "laravel_session=…(320); path=/; httponly; samesite=lax") {
		t.Errorf("laravel: %s → %s", b, c)
	}
	// With expires, the window starts at the name of the cookie that
	// changed: the line shows it and the attribute it gained, and the
	// explanation a session cookie whose masked value changed shape.
	expires := "; expires=Tue, 10 Nov 2026 16:00:00 GMT; path=/"
	xsrf, session = "XSRF-TOKEN="+token+expires, "laravel_session="+token+expires+"; httponly"
	laravel = drift("r", compare.KindHeaderValue, "header:set-cookie", `["`+xsrf+`","`+session+`"]`, `["`+xsrf+`; samesite=lax","`+session+`"]`)
	if b, c := DriftPair(laravel, shortMax); !strings.Contains(b, "XSRF-TOKEN=…(320)") || !strings.HasPrefix(c, "…XSRF-TOKEN=…(320)") || !strings.Contains(c, "samesite=lax") {
		t.Errorf("laravel with expires: %s → %s", b, c)
	}
	laravel = drift("r", compare.KindHeaderValue, "header:set-cookie", `["`+xsrf+`","`+session+`"]`, `["`+xsrf+`","laravel_session=0`+token[1:]+expires+`; httponly"]`)
	if b, c := DriftPair(laravel, 40); !strings.HasPrefix(b, "…laravel_session=x") || !strings.HasPrefix(c, "…laravel_session=0") {
		t.Errorf("laravel, shape only: %s → %s", b, c)
	}

	private := drift("r", compare.KindHeaderValue, "header:set-cookie", "(private)", "(private)")
	r = Build("capture:c", "baseline:local", []Result{{ID: "r", Outcome: Drift, Drifts: []compare.Drift{private}}})
	out.Reset()
	r.Text(&out, DefaultDrifts)
	if line := strings.Split(out.String(), "\n")[1]; !strings.Contains(line, "(private) → (private)") {
		t.Errorf("private line %q", line)
	}
	if e := r.Groups[0].Explanation; !strings.Contains(e, "(private) → (private)") {
		t.Errorf("private explanation %q", e)
	}
}
