package report

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
)

func drifted(id string, digests ...string) Result {
	res := Result{ID: id, Outcome: Drift}
	for i, d := range digests {
		res.Drifts = append(res.Drifts, compare.Drift{Request: id, Kind: compare.KindHeaderExtra, Where: "header:x-" + string(rune('a'+i)), Current: `["1"]`, Digest: d})
	}
	return res
}

func unmetList(r *Report) string {
	var out []string
	for _, u := range r.Unmet {
		out = append(out, u.Ref+" "+u.Request+" "+u.Why)
	}
	return strings.Join(out, "; ")
}

// TestExpect: a drift is expected only when an expectation names its
// digest; a request only when every drift is. The expectations whose
// drifts did not occur are listed, after matching.
func TestExpect(t *testing.T) {
	private := drifted("me", "ref:5")
	private.Note = NotePrivateMissing
	r := Build("capture:c", "baseline:local", []Result{
		drifted("a", "sha256:1"), drifted("b", "sha256:2"), drifted("c", ""), private,
		{ID: "d", Outcome: Pass}, {ID: "e", Outcome: Skipped, Note: NoteNoBaseline},
		drifted("f", "sha256:6", "sha256:7"),
	})
	r.Expect([]Expectation{
		{Ref: "T-1", Reason: "header a", Request: "a", Drifts: []string{"sha256:1"}},
		{Ref: "T-1", Reason: "header a", Request: "b", Drifts: []string{"sha256:9"}}, // another value
		{Ref: "T-2", Reason: "x", Request: "c", Drifts: []string{"sha256:3"}},
		{Ref: "T-2", Reason: "x", Request: "me", Drifts: []string{"ref:5"}},
		{Ref: "T-2", Reason: "x", Request: "d", Drifts: []string{"sha256:4"}},
		{Ref: "T-2", Reason: "x", Request: "e", Drifts: []string{"sha256:4"}},
		{Ref: "T-2", Reason: "x", Request: "f", Drifts: []string{"sha256:6", "sha256:8"}}, // one of two drifts happened
		{Ref: "T-3", Reason: "x", Request: "gone", Drifts: []string{"sha256:4"}},
		{Ref: "T-3", Reason: "x", Request: "filtered", Drifts: []string{"sha256:4"}},
	}, func(id string) bool { return id != "gone" })
	if r.Expected != 1 || r.Unexpected() != 4 || strings.Join(r.Results[0].Expected, ",") != "T-1" || r.Results[1].Expected != nil {
		t.Fatalf("expected %d, unexpected %d: %+v", r.Expected, r.Unexpected(), r.Results)
	}
	// f's first drift is expected, its second is not: f fails, with the
	// drift that is not expected.
	if f := r.Results[6]; f.Expected != nil || f.Drifts[0].Expected != "T-2" || f.Drifts[1].Expected != "" {
		t.Fatalf("f: %+v", f)
	}
	if got := unmetList(r); got != "T-1 b other drifts; T-2 c other drifts; T-2 d no drift; T-2 f partly met: 1 of 2 drifts; T-3 gone not in the catalog" {
		t.Fatalf("unmet: %s", got)
	}

	// Two tickets change one request: each expects its own drift, and
	// together they expect the request. Neither is unmet.
	r = Build("capture:c", "baseline:local", []Result{drifted("a", "sha256:1", "sha256:2")})
	r.Expect([]Expectation{
		{Ref: "T-1", Reason: "x", Request: "a", Drifts: []string{"sha256:1"}},
		{Ref: "T-2", Reason: "y", Request: "a", Drifts: []string{"sha256:2"}},
		{Ref: "T-3", Reason: "z", Request: "a", Drifts: []string{"sha256:1"}}, // covered by T-1 too
	}, func(string) bool { return true })
	if r.Unexpected() != 0 || strings.Join(r.Results[0].Expected, ",") != "T-1,T-2" || len(r.Unmet) != 0 || len(r.ExpectedRefs) != 2 {
		t.Fatalf("two refs: %+v %s %+v", r.Results[0], unmetList(r), r.ExpectedRefs)
	}
	// One of them alone does not expect the request.
	r.Expect([]Expectation{{Ref: "T-1", Reason: "x", Request: "a", Drifts: []string{"sha256:1"}}}, func(string) bool { return true })
	if r.Unexpected() != 1 || r.Results[0].Expected != nil {
		t.Fatalf("one ref: %+v", r.Results[0])
	}

	// Every drift expected: the comparison passes, and every format says so.
	r = Build("capture:c", "baseline:local", []Result{drifted("a", "sha256:1"), {ID: "d", Outcome: Pass}})
	r.Expect([]Expectation{{Ref: "T-1", Reason: "header a", Request: "a", Drifts: []string{"sha256:1"}}}, func(string) bool { return true })
	var text bytes.Buffer
	r.Text(&text, DefaultDrifts)
	if r.Unexpected() != 0 || !strings.Contains(text.String(), "No unexpected drift ✓") || strings.Contains(text.String(), "No drift ✓") {
		t.Errorf("all expected:\n%s", text.String())
	}
	if md := r.Markdown(DefaultDrifts); !strings.Contains(md, "✓ no unexpected drift in 2 requests (1 drifted as expected)") {
		t.Errorf("markdown:\n%s", md)
	}
	var junit bytes.Buffer
	if err := r.JUnit(&junit, DefaultDrifts); err != nil {
		t.Fatal(err)
	}
	if j := junit.String(); strings.Contains(j, "<failure") || !strings.Contains(j, "<system-out>expected drift (T-1), 1 drift:") || !strings.Contains(j, `name="expected"`) {
		t.Errorf("junit:\n%s", j)
	}
	var js bytes.Buffer
	if err := r.JSON(&js); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || back.Expected != 1 || back.Results[0].Expected[0] != "T-1" || back.Results[0].Drifts[0].Expected != "T-1" {
		t.Errorf("json: %v\n%s", err, js.String())
	}
	// An expectation met in part is in the request's test case, failing or not.
	partly := Build("capture:c", "baseline:local", []Result{drifted("a", "sha256:1"), drifted("b", "sha256:1"), {ID: "d", Outcome: Pass}})
	partly.Expect([]Expectation{{Ref: "T-2", Reason: "r", Request: "a", Drifts: []string{"sha256:1", "sha256:2"}}, {Ref: "T-3", Reason: "r", Request: "d", Drifts: []string{"sha256:1"}}}, func(string) bool { return true })
	junit.Reset()
	if err := partly.JUnit(&junit, DefaultDrifts); err != nil {
		t.Fatal(err)
	}
	if j := junit.String(); !strings.Contains(j, "&#xA;expectation T-2 not met: partly met: 1 of 2 drifts</system-out>") || !strings.Contains(j, "<system-out>expectation T-3 not met: no drift</system-out>") || strings.Count(j, "not met: ") != 2 {
		t.Errorf("junit unmet:\n%s", j)
	}
	// Marking again replaces the marks: without expectations it fails.
	r.Expect(nil, func(string) bool { return true })
	if r.Expected != 0 || r.Results[0].Expected != nil || r.Results[0].Drifts[0].Expected != "" || r.Unexpected() != 1 {
		t.Errorf("marks kept: %+v", r)
	}
}

// TestExpectLayout: the requests that fail come first, with the drifts
// that fail; expected ones are one line per ref; groups say when all of
// their drifts are expected; markdown keeps its room for what fails.
func TestExpectLayout(t *testing.T) {
	r := Build("capture:c", "baseline:local", []Result{drifted("a", "sha256:1"), drifted("f", "sha256:1", "sha256:7"), {ID: "d", Outcome: Pass}})
	r.Expect([]Expectation{{Ref: "T-1", Reason: "header a", Request: "a", Drifts: []string{"sha256:1"}}, {Ref: "T-1", Reason: "header a", Request: "f", Drifts: []string{"sha256:1"}}}, func(string) bool { return true })
	var text bytes.Buffer
	r.Text(&text, DefaultDrifts)
	out := text.String()
	for _, want := range []string{
		"DRIFT  f\n         header-extra    header:x-b   → [\"1\"]\n         + 1 drift expected (T-1)\n",
		"Expected (driftgate expect), not failing:\n  T-1 (header a): 1 request: a\n",
		"[1] 2 drifts in 2 requests: header x-a is new  (expected)",
		"3 compared, 1 passed, 2 drifted (1 expected),",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DRIFT  a") || strings.Index(out, "DRIFT  f") > strings.Index(out, "Expected (") {
		t.Errorf("order:\n%s", out)
	}
	md := r.Markdown(DefaultDrifts)
	for _, want := range []string{"### DriftGate: ✗ 1 of 3 compared requests drifted unexpectedly (1 more as expected)", "| Compared | Passed | Drifted | Expected |", "| 3 | 1 | 2 | 1 |", "_(expected)_", "<code>f</code>", "<details><summary>1 request of the 2 drifted match their expectations exactly"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "<code>a</code>") || strings.Index(md, "<code>f</code>") > strings.Index(md, "match their expectations") {
		t.Errorf("markdown order:\n%s", md)
	}

	// Too many failing requests for one comment: the expected block is
	// left out, and the comment says so.
	var many []Result
	var exps []Expectation
	for i := 0; i < 3000; i++ {
		id := strings.Repeat("x", 40) + string(rune('A'+i%26)) + strings.Repeat("y", i%7)
		res := drifted(id+"-"+itoa(i), "sha256:1")
		many = append(many, res)
		if i%2 == 0 {
			exps = append(exps, Expectation{Ref: "T-1", Reason: "r", Request: res.ID, Drifts: []string{"sha256:1"}})
		}
	}
	r = Build("capture:c", "baseline:local", many)
	r.Expect(exps, func(string) bool { return true })
	md = r.Markdown(DefaultDrifts)
	if len(md) > MarkdownLimit || !strings.Contains(md, "the requests that drifted as expected") || strings.Contains(md, "match their expectations exactly</summary>") {
		t.Errorf("cut: %d bytes\n%s", len(md), md[len(md)-400:])
	}

	// Groups that fill the comment: an expectation met in part is still
	// named.
	many = nil
	for i := range 3000 {
		res := Result{ID: "r-" + itoa(i), Outcome: Drift, Drifts: []compare.Drift{{Request: "r-" + itoa(i), Kind: compare.KindHeaderExtra, Where: "header:x-" + itoa(i), Current: `["1"]`, Digest: "sha256:1"}}}
		many = append(many, res)
	}
	r = Build("capture:c", "baseline:local", many)
	r.Expect([]Expectation{{Ref: "T-2", Reason: "r", Request: "r-1", Drifts: []string{"sha256:1", "sha256:2"}}}, func(string) bool { return true })
	if md = r.Markdown(DefaultDrifts); len(md) > MarkdownLimit || !strings.Contains(md, "group") || !strings.Contains(md, "T-2 r-1 (partly met") {
		t.Errorf("unmet note cut: %d bytes\n%s", len(md), md[len(md)-600:])
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
