package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
)

// Expectation is an expectation in force as a comparison matches it: the
// drifts of one request it names, each as a digest (compare.Drift.Digest)
// of the drift with its values and what its description leaves out. A
// drift is expected only when an expectation names exactly that drift; a
// request is expected only when every one of its drifts is.
type Expectation struct {
	Ref     string
	Reason  string
	Request string
	Drifts  []string
}

// ExpectedRef lists the requests whose drifts the expectations of a ref
// helped to expect fully.
type ExpectedRef struct {
	Ref      string   `json:"ref"`
	Reason   string   `json:"reason"`
	Requests []string `json:"requests"`
}

// UnmetExpectation is an expectation whose drifts the comparison did not
// have, all or some of them.
type UnmetExpectation struct {
	Ref     string `json:"ref"`
	Request string `json:"request"`
	Why     string `json:"why"`
}

// Why an expectation is unmet.
const (
	UnmetNoDrift    = "no drift"     // the request did not drift: the change is not there yet, or accepted or rebased
	UnmetOther      = "other drifts" // none of its drifts, but others
	UnmetPartly     = "partly met"   // some of its drifts did not occur
	UnmetNotCatalog = "not in the catalog"
)

// Expect marks the drifts that an expectation in force names exactly
// (Drift.Expected), and the drifted requests every drift of which is
// expected (Result.Expected), which then do not fail the comparison. In
// a second pass it lists the expectations whose drifts did not occur:
// a drift another ref expected counts as occurred for every ref that
// names it. known reports whether a request is in the catalog; an
// expectation of a request that was not compared because of --only or
// --target is not listed. Earlier marks are replaced.
func (r *Report) Expect(exps []Expectation, known func(id string) bool) {
	r.Expected, r.ExpectedRefs, r.Unmet = 0, nil, nil
	accepted := make(map[string]bool, len(r.Accepted))
	for _, id := range r.Accepted {
		accepted[id] = true
	}
	// The digests each request's expectations name, with the first ref.
	byRequest := make(map[string]map[string]string, len(exps))
	for _, e := range exps {
		m := byRequest[e.Request]
		if m == nil {
			m = map[string]string{}
			byRequest[e.Request] = m
		}
		for _, d := range e.Drifts {
			if _, ok := m[d]; !ok {
				m[d] = e.Ref
			}
		}
	}
	at := make(map[string]int, len(r.Results))
	reasons := map[string]string{}
	for _, e := range exps {
		if _, ok := reasons[e.Ref]; !ok {
			reasons[e.Ref] = e.Reason
		}
	}
	refRequests := map[string][]string{}
	for i := range r.Results {
		res := &r.Results[i]
		at[res.ID] = i
		res.Expected = nil
		for j := range res.Drifts {
			res.Drifts[j].Expected = ""
		}
		names := byRequest[res.ID]
		if names == nil || res.Outcome != Drift || accepted[res.ID] || res.Note == NotePrivateMissing {
			continue
		}
		all := len(res.Drifts) > 0
		refs := map[string]bool{}
		for j, d := range res.Drifts {
			ref, ok := names[d.Digest]
			if d.Digest == "" || !ok {
				all = false
				continue
			}
			res.Drifts[j].Expected = ref
			refs[ref] = true
		}
		if !all {
			continue
		}
		for ref := range refs {
			res.Expected = append(res.Expected, ref)
		}
		sort.Strings(res.Expected)
		r.Expected++
		for _, ref := range res.Expected {
			refRequests[ref] = append(refRequests[ref], res.ID)
		}
	}
	refs := make([]string, 0, len(refRequests))
	for ref := range refRequests {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		r.ExpectedRefs = append(r.ExpectedRefs, ExpectedRef{Ref: ref, Reason: reasons[ref], Requests: refRequests[ref]})
	}

	// Which expectations did not happen, all or in part.
	for _, e := range exps {
		i, ok := at[e.Request]
		if !ok {
			if !known(e.Request) {
				r.Unmet = append(r.Unmet, UnmetExpectation{Ref: e.Ref, Request: e.Request, Why: UnmetNotCatalog})
			}
			continue
		}
		res := r.Results[i]
		switch {
		case accepted[res.ID] || res.Outcome == Skipped || res.Outcome == Observed || res.Note == NotePrivateMissing:
			continue // nothing to tell: accepted since, or not compared (whole) here
		case res.Outcome == Pass:
			r.Unmet = append(r.Unmet, UnmetExpectation{Ref: e.Ref, Request: e.Request, Why: UnmetNoDrift})
			continue
		}
		occurred := make(map[string]bool, len(res.Drifts))
		for _, d := range res.Drifts {
			occurred[d.Digest] = true
		}
		n := 0
		for _, d := range e.Drifts {
			if occurred[d] {
				n++
			}
		}
		switch {
		case n == 0:
			r.Unmet = append(r.Unmet, UnmetExpectation{Ref: e.Ref, Request: e.Request, Why: UnmetOther})
		case n < len(e.Drifts):
			r.Unmet = append(r.Unmet, UnmetExpectation{Ref: e.Ref, Request: e.Request, Why: fmt.Sprintf("%s: %d of %d drifts", UnmetPartly, n, len(e.Drifts))})
		}
	}
	// An expected drift passes: one that came after a retry is counted so.
	r.retried()
}

// Expectable reports whether every drift of a result can be expected: it
// drifted, everything was compared (not without a private body), and
// each drift has a digest (a side effect has none).
func Expectable(res Result) bool {
	if res.Outcome != Drift || res.Note == NotePrivateMissing || len(res.Drifts) == 0 {
		return false
	}
	for _, d := range res.Drifts {
		if d.Digest == "" {
			return false
		}
	}
	return true
}

// Unexpected is the number of drifted requests with a drift no
// expectation names: those make the comparison fail.
func (r *Report) Unexpected() int { return r.Drifted - r.Expected }

// expectedNames writes the refs that expect a request.
func expectedNames(res Result) string { return strings.Join(res.Expected, ", ") }

// failing reports a drifted request that fails: one drift or more is not
// expected.
func failing(res Result) bool { return res.Outcome == Drift && len(res.Expected) == 0 }

// failingLines are the drift lines of a failing request: the drifts no
// expectation names, and one line for those that one does.
func failingLines(res Result, maxDrifts, pad int) []string {
	var open []compare.Drift
	refs := map[string]bool{}
	for _, d := range res.Drifts {
		if d.Expected == "" {
			open = append(open, d)
		} else {
			refs[d.Expected] = true
		}
	}
	if len(open) == len(res.Drifts) {
		return driftLines(res, maxDrifts, pad)
	}
	part := res
	part.Drifts, part.StatusChange = open, statusChange(open)
	names := make([]string, 0, len(refs))
	for ref := range refs {
		names = append(names, ref)
	}
	sort.Strings(names)
	return append(driftLines(part, maxDrifts, pad), fmt.Sprintf("+ %s expected (%s)", plural(len(res.Drifts)-len(open), "drift"), strings.Join(names, ", ")))
}

// ExpectedLines list the expected requests by ref, one line each: the
// ref, its reason, how many requests and which. Empty when nothing was
// expected.
func (r *Report) ExpectedLines() []string {
	var out []string
	for _, e := range r.ExpectedRefs {
		out = append(out, fmt.Sprintf("%s (%s): %s: %s", e.Ref, short(e.Reason), plural(len(e.Requests), "request"), listIDs(e.Requests)))
	}
	return out
}

// expectedGroups are the groups every drift of which is expected.
func (r *Report) expectedGroups() map[int]bool {
	open := map[string]bool{}
	for _, res := range r.Results {
		if res.Outcome != Drift {
			continue
		}
		for _, d := range res.Drifts {
			if d.Expected == "" {
				open[d.Signature()] = true
			}
		}
	}
	out := map[int]bool{}
	if r.Expected == 0 && len(r.ExpectedRefs) == 0 {
		return out
	}
	for _, g := range r.Groups {
		if !open[g.Signature] {
			out[g.N] = true
		}
	}
	return out
}

// driftedText is the number of drifted requests, with the expected ones.
func (r *Report) driftedText() string {
	if r.Expected > 0 {
		return fmt.Sprintf("%d drifted (%d expected)", r.Drifted, r.Expected)
	}
	return fmt.Sprintf("%d drifted", r.Drifted)
}

// UnmetNote warns of the expectations whose drifts the comparison did not
// have, up to ten; empty when there are none.
func (r *Report) UnmetNote() string {
	n := len(r.Unmet)
	if n == 0 {
		return ""
	}
	var list []string
	for i, u := range r.Unmet {
		if i == 10 {
			list = append(list, fmt.Sprintf("and %d more", n-10))
			break
		}
		list = append(list, fmt.Sprintf("%s %s (%s)", u.Ref, u.Request, u.Why))
	}
	return fmt.Sprintf("warning: %s not met in this comparison (driftgate expect --drop <ref> --reason \"…\" removes a stale one): %s",
		plural(n, "expectation"), strings.Join(list, "; "))
}

// expectedSummary is the one-line count of the expected drifts.
func (r *Report) expectedSummary() string {
	return fmt.Sprintf("%s of the %d drifted match their expectations exactly (driftgate expect)", plural(r.Expected, "request"), r.Drifted)
}
