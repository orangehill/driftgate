package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
)

// The not-covered summary and the one change almost every drifted request
// has are in every format; with nothing uncovered and uploads and command
// steps in the catalog, nothing is said.
func TestNotCoveredInEveryFormat(t *testing.T) {
	var ids []string
	for i := range 12 {
		ids = append(ids, fmt.Sprintf("get-r-%d", i))
	}
	r := Build("capture:c", "baseline:local", []Result{drifted("a", "d"), drifted("b", "d"), drifted("c", "d"), drifted("d", "d"), drifted("e", "d"), {ID: "ok", Outcome: Pass}})
	r.NotCovered = &NotCovered{Catalog: CatalogCounts{Requests: 19, Reads: 18, Mutating: 1}, RoutesChecked: true, Against: "baseline:local",
		NoRequest: NewGap([]string{"GET /api/extra"}), NoInvalidBody: NewGap([]string{"POST /api/posts"}), FailedInBaseline: NewGap([]string{"get-tree"}), Observed: NewGap(ids),
		Skipped: []SkippedGap{{What: SkippedMutating, ReasonGap: ReasonGap{Reason: "no baseline", Gap: *NewGap([]string{"post-import"})}},
			{What: SkippedRead, ReasonGap: ReasonGap{Reason: NoteNotSent, Gap: *NewGap([]string{"get-in-seq"})}}, {What: SkippedCommand, ReasonGap: ReasonGap{Reason: "no baseline", Gap: *NewGap([]string{"schema"})}}},
		KnownFailures: NewGap([]string{"get-old"}),
		RowValues:     &ReasonGap{Reason: "the baseline recorded no row values", Gap: *NewGap([]string{"post-follow"})}}
	g, drifted, elsewhere, ok := r.FindDominant()
	if !ok || g.Requests != 5 || drifted != 5 || elsewhere != 0 {
		t.Fatalf("dominant %v %d %d %v", g, drifted, elsewhere, ok)
	}
	r.Dominant = &Dominant{Group: 1, Signature: g.Signature, Explanation: "header x-a is new", Requests: 5, Drifted: 5}
	if len(r.NotCovered.Observed.Names) != gapNames {
		t.Fatalf("names kept: %v", r.NotCovered.Observed.Names)
	}
	catalogLine := "catalog: 19 requests: 18 reads, 1 mutating, 0 command steps; 0 of them multipart"
	gaps := []string{"1 route method without a request meant to succeed: GET /api/extra",
		"1 route method whose validation error no request checks (templates empty-body and json-errors): POST /api/posts", "1 request meant to succeed that failed in baseline:local: get-tree",
		"12 requests observed (measured, not compared): get-r-0, get-r-1, get-r-2, get-r-3, get-r-4 and 7 more", "1 request that changes data skipped (no baseline): post-import", "1 read skipped (" + NoteNotSent + "): get-in-seq", "1 command step skipped (no baseline): schema",
		"1 known failure (exclusions.yaml): get-old", "1 request that changes data whose row values were not compared (the baseline recorded no row values): post-follow"}
	dominant := "one change in 5 of 5 drifted requests: header x-a is new; set aside, 0 of 5 drift in something else; fix it, or take it with driftgate accept group:1"
	mdGaps := []string{"> - 1 route method without a request meant to succeed: GET /api/extra", "> - 12 requests observed (measured, not compared)"}
	want := map[string][]string{
		"text":     append([]string{catalogLine, notCoveredTitle, dominant}, gaps...),
		"json":     {`"not_covered": {`, `"multipart": 0`, `"routes_checked": true`, `"count": 12`, `"get-r-9"`, `"no_invalid_body": {`, `"what": "read"`, `"reason": "not sent: capture --read-only"`, `"dominant": {`, `"elsewhere": 0`, `"get-tree"`, `"row_values": {`},
		"markdown": append([]string{"catalog: 19 requests", "driftgate coverage lists the routes", "one change in 5 of 5 drifted requests"}, mdGaps...),
		"junit":    append([]string{catalogLine, "driftgate coverage lists the routes", dominant}, gaps...),
	}
	for _, f := range []string{"text", "json", "markdown", "junit"} {
		var b bytes.Buffer
		if err := r.Write(&b, f, DefaultDrifts); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "get-tree (") || strings.Contains(b.String(), "get-tree: 5") {
			t.Errorf("%s mixes a status into a name", f)
		}
		for _, w := range want[f] {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s without %q:\n%s", f, w, b.String())
			}
		}
	}

	// Nothing uncovered, with uploads and command steps: nothing said.
	r.NotCovered = &NotCovered{Catalog: CatalogCounts{Requests: 6, Reads: 4, Commands: 2, Multipart: 1}, RoutesChecked: true}
	r.Dominant = nil
	for _, f := range []string{"text", "markdown", "junit"} {
		var b bytes.Buffer
		if err := r.Write(&b, f, DefaultDrifts); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), "catalog: ") || strings.Contains(strings.ToLower(b.String()), "not covered") || f == "text" && strings.HasSuffix(b.String(), "\n\n") {
			t.Errorf("%s:\n%q", f, b.String())
		}
	}
	// No upload: the catalog line says so, also without gaps.
	r.NotCovered = &NotCovered{Catalog: CatalogCounts{Requests: 6, Reads: 4, Commands: 2}, Only: true}
	var b bytes.Buffer
	r.Text(&b, DefaultDrifts)
	if !strings.Contains(b.String(), "compared (--only): 6 requests: 4 reads, 0 mutating, 2 command steps; 0 of them multipart") {
		t.Errorf("text:\n%s", b.String())
	}
}

// The dominant change counts unexpected drifts only.
func TestFindDominant(t *testing.T) {
	if _, _, _, ok := Build("c", "b", []Result{drifted("a", "d"), drifted("b", "d")}).FindDominant(); ok {
		t.Error("a dominant change of two requests")
	}
	mixed := []Result{drifted("a", "d"), drifted("b", "d"), drifted("c", "d"), drifted("d", "d"), drifted("e", "d"), drifted("f", "d"), drifted("g", "d"),
		drifted("h", "d"), drifted("i", "d"), drifted("j", "x", "y")}
	if _, _, elsewhere, ok := Build("c", "b", mixed).FindDominant(); !ok || elsewhere != 1 {
		t.Errorf("elsewhere %d, %v", elsewhere, ok)
	}
	// Expected requests are left out: four unexpected are too few.
	for i := range 6 {
		mixed[i].Expected = []string{"T-1"}
	}
	if _, _, _, ok := Build("c", "b", mixed).FindDominant(); ok {
		t.Error("a dominant change counted expected requests")
	}
	// An expected drift of a request does not count, its other drift does.
	r := []Result{}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		res := drifted(id, "d", "e")
		res.Drifts[0].Expected = "T-1"
		r = append(r, res)
	}
	g, drifted, _, ok := Build("c", "b", r).FindDominant()
	if !ok || drifted != 5 || g.Example.Where != "header:x-b" || g.Example.Kind != compare.KindHeaderExtra {
		t.Errorf("dominant %+v %d %v", g, drifted, ok)
	}
}

// Where accept and expect refuse the comparison, the line on the one change
// names neither: two captures compared, or a capture made with
// --skip-fingerprint-check.
func TestDominantNoteWithoutAccept(t *testing.T) {
	r := &Report{Dominant: &Dominant{Group: 2, Explanation: "header server changed value", Requests: 6, Drifted: 6}}
	if n := r.DominantNote(); !strings.Contains(n, "driftgate accept group:2") || !strings.Contains(n, "driftgate expect group:2") {
		t.Errorf("against a baseline: %s", n)
	}
	r.Dominant.Captures = true
	if n := r.DominantNote(); strings.Contains(n, "driftgate accept") || strings.Contains(n, "driftgate expect") || !strings.Contains(n, "name it in the pull request") || !strings.Contains(n, "baselines rebase") {
		t.Errorf("two captures: %s", n)
	}
	r.Dominant.Captures, r.Dominant.Unreliable = false, true
	if n := r.DominantNote(); strings.Contains(n, "group:2") || !strings.Contains(n, "capture again without --skip-fingerprint-check") {
		t.Errorf("unreliable capture: %s", n)
	}
}

// TestKindsNote: drifts counted by kind, the most first, then by name.
func TestKindsNote(t *testing.T) {
	r := Build("c", "baseline:local", []Result{
		{ID: "a", Outcome: Drift, Drifts: []compare.Drift{{Kind: "header-extra", Where: "header:x"}, {Kind: "status", Where: "status"}}},
		{ID: "b", Outcome: Drift, Drifts: []compare.Drift{{Kind: "header-extra", Where: "header:x"}, {Kind: "type", Where: "$.a"}}},
		{ID: "c", Outcome: Pass}})
	if got := r.KindsNote(); got != "kinds: header-extra 2, status 1, type 1" {
		t.Errorf("%q", got)
	}
	if n := Build("c", "baseline:local", []Result{{ID: "c", Outcome: Pass}}).KindsNote(); n != "" {
		t.Errorf("without drift: %q", n)
	}
}

// TestRowValuesNote: a stop on another value of a tracked row says how to
// leave out a column that changes by itself between days; an expected
// one, or another side effect, does not.
func TestRowValuesNote(t *testing.T) {
	d := compare.Drift{Request: "follow", Kind: compare.KindSideEffect, Where: "side-effects:db/table:follows[id=1]", Baseline: "inserted", Current: "inserted; other value: seen_on"}
	r := Build("capture:c", "baseline:local", []Result{{ID: "follow", Outcome: Drift, Drifts: []compare.Drift{d}}})
	var b bytes.Buffer
	r.Text(&b, DefaultDrifts)
	if !strings.Contains(b.String(), "goes under ignore: in row_tracking") {
		t.Errorf("no hint:\n%s", b.String())
	}
	d.Expected = "ref"
	if n := Build("capture:c", "baseline:local", []Result{{ID: "follow", Outcome: Drift, Drifts: []compare.Drift{d}}}).RowValuesNote(); n != "" {
		t.Errorf("expected: %s", n)
	}
	d.Expected, d.Current = "", "updated seen_on"
	if n := Build("capture:c", "baseline:local", []Result{{ID: "follow", Outcome: Drift, Drifts: []compare.Drift{d}}}).RowValuesNote(); n != "" {
		t.Errorf("another side effect: %s", n)
	}
}
