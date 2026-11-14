package report

import (
	"fmt"
	"sort"
	"strings"
)

// NotCovered is what a comparison does not cover, next to its result: a
// green result proves only the requests the catalog has, as they answered
// in the set compared against. It is information and never changes the
// exit code; driftgate coverage lists the routes.
type NotCovered struct {
	// Catalog counts the compared requests (the catalog's, or those --only
	// selected: Only) by what they do.
	Catalog CatalogCounts `json:"catalog"`
	Only    bool          `json:"only,omitempty"`
	// Against is the set compared against, whose answers say which
	// requests failed (baseline:local, capture:old).
	Against string `json:"against,omitempty"`
	// RoutesChecked: a route inventory (routes.json) was there and every
	// request was compared, so NoRequest is known.
	RoutesChecked bool `json:"routes_checked"`
	// Route methods no request meant to succeed reaches, apart from those
	// whose requests failed in the set compared against.
	NoRequest *Gap `json:"no_request,omitempty"`
	// Route methods whose body is validated (a sample from its rules) that
	// no request sends an invalid body: their validation errors are not
	// compared.
	NoInvalidBody *Gap `json:"no_invalid_body,omitempty"`
	// Requests meant to succeed whose answer in the set compared against is
	// an error: the comparison keeps the error green.
	FailedInBaseline *Gap `json:"failed_in_baseline,omitempty"`
	// Requests measured and never compared (observe: true).
	Observed *Gap `json:"observed,omitempty"`
	// Every request the comparison skipped (not compared), by what it is
	// and why: none is left unnamed.
	Skipped []SkippedGap `json:"skipped,omitempty"`
	// Failed requests acknowledged under known_failures: in exclusions.yaml.
	KnownFailures *Gap `json:"known_failures,omitempty"`
	// Requests that changed tracked rows whose values were not compared,
	// and why (a side recorded none).
	RowValues *SkippedGap `json:"row_values,omitempty"`
}

// CatalogCounts are requests by what they do; Multipart are among the
// reads and mutating ones.
type CatalogCounts struct {
	Requests  int `json:"requests"`
	Reads     int `json:"reads"`
	Mutating  int `json:"mutating"`
	Commands  int `json:"commands"`
	Multipart int `json:"multipart"`
}

// Gap is how many requests or routes one class of gap has, and the first
// of them (request IDs or routes).
type Gap struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

// What a skipped request is, for SkippedGap.What.
const (
	SkippedRead     = "read"
	SkippedMutating = "mutating"
	SkippedCommand  = "command"
)

// SkippedGap is the skipped requests of one kind with one reason (the
// result's note: no baseline, not sent by a read-only capture …).
type SkippedGap struct {
	What   string `json:"what"` // read, mutating or command
	Reason string `json:"reason"`
	Gap
}

// gapNames is how many names a Gap keeps; text shows fewer.
const gapNames = 10

// NewGap is the gap of names, nil when there are none.
func NewGap(names []string) *Gap {
	if len(names) == 0 {
		return nil
	}
	return &Gap{Count: len(names), Names: append([]string(nil), names[:min(len(names), gapNames)]...)}
}

// Empty reports that nothing is left uncovered.
func (n *NotCovered) Empty() bool {
	return n == nil || n.NoRequest == nil && n.NoInvalidBody == nil && n.FailedInBaseline == nil && n.Observed == nil && len(n.Skipped) == 0 && n.KnownFailures == nil && n.RowValues == nil
}

// CatalogLine counts the compared requests by what they do: a zero says
// what the gate never sends (no multipart request: no upload is checked).
// It is said with gaps, and when the catalog sends no upload or no
// command step; empty otherwise.
func (n *NotCovered) CatalogLine() string {
	if n == nil || n.Empty() && n.Catalog.Multipart > 0 && n.Catalog.Commands > 0 {
		return ""
	}
	c := n.Catalog
	what := "catalog"
	if n.Only {
		what = "compared (--only)"
	}
	return fmt.Sprintf("%s: %s: %d %s, %d mutating, %s; %d of them multipart",
		what, plural(c.Requests, "request"), c.Reads, word(c.Reads, "read", "reads"), c.Mutating, plural(c.Commands, "command step"), c.Multipart)
}

func word(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shown is how many names of a gap the text formats show.
const shown = 5

func (g *Gap) list() string {
	names := g.Names[:min(len(g.Names), shown)]
	s := strings.Join(names, ", ")
	if more := g.Count - len(names); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

// NotCoveredLines are the gaps, one line each; none when nothing is left
// uncovered.
func (n *NotCovered) NotCoveredLines() []string {
	if n.Empty() {
		return nil
	}
	var out []string
	add := func(g *Gap, one, many string) {
		if g != nil {
			out = append(out, fmt.Sprintf("%d %s: %s", g.Count, word(g.Count, one, many), Printable(g.list())))
		}
	}
	against := n.Against
	if against == "" {
		against = "the baseline"
	}
	add(n.NoRequest, "route method without a request meant to succeed", "route methods without a request meant to succeed")
	add(n.NoInvalidBody, "route method whose validation error no request checks (templates empty-body and json-errors)", "route methods whose validation errors no request checks (templates empty-body and json-errors)")
	add(n.FailedInBaseline, "request meant to succeed that failed in "+against, "requests meant to succeed that failed in "+against)
	add(n.Observed, "request observed (measured, not compared)", "requests observed (measured, not compared)")
	for _, s := range n.Skipped {
		one, many := "read", "reads"
		switch s.What {
		case SkippedMutating:
			one, many = "request that changes data", "requests that change data"
		case SkippedCommand:
			one, many = "command step", "command steps"
		}
		g := s.Gap
		add(&g, one+" skipped ("+s.Reason+")", many+" skipped ("+s.Reason+")")
	}
	add(n.KnownFailures, "known failure (exclusions.yaml)", "known failures (exclusions.yaml)")
	if g := n.RowValues; g != nil {
		add(&g.Gap, "request that changes data whose row values were not compared ("+g.Reason+")", "requests that change data whose row values were not compared ("+g.Reason+")")
	}
	return out
}

// notCoveredTitle heads the gaps.
const notCoveredTitle = "Not covered by this result (driftgate coverage lists the routes):"

// NotCoveredNote is the gaps as a short markdown list; empty when nothing
// is left uncovered.
func (n *NotCovered) NotCoveredNote() string {
	lines := n.NotCoveredLines()
	if len(lines) == 0 {
		return ""
	}
	return notCoveredTitle + "\n- " + strings.Join(lines, "\n- ")
}

// Dominant is one drift signature that almost every request with an
// unexpected drift has: set aside, how many requests drift in something
// else. Rule is the allowlist rule for it when it is a header of the
// server stack whose new value is one (empty otherwise: a change between
// builds is fixed, accepted or expected, never allowed by a guess).
type Dominant struct {
	Group       int    `json:"group"`
	Signature   string `json:"signature"`
	Explanation string `json:"explanation"`
	Requests    int    `json:"requests"`
	Drifted     int    `json:"drifted"`
	Elsewhere   int    `json:"elsewhere"`
	Rule        string `json:"rule,omitempty"`
	// Captures: the comparison is not of a capture against a baseline (two
	// captures, or a baseline against another); Unreliable: the capture was
	// made with
	// --skip-fingerprint-check. accept and expect take neither.
	Captures   bool `json:"captures,omitempty"`
	Unreliable bool `json:"unreliable,omitempty"`
}

// KindsNote counts the drifts by kind, the most first ("kinds:
// header-extra 622, array-length 188, status 6"); empty without drift.
func (r *Report) KindsNote() string {
	if len(r.Kinds) == 0 {
		return ""
	}
	kinds := make([]string, 0, len(r.Kinds))
	for k := range r.Kinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if a, b := r.Kinds[kinds[i]], r.Kinds[kinds[j]]; a != b {
			return a > b
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = fmt.Sprintf("%s %d", k, r.Kinds[k])
	}
	return "kinds: " + strings.Join(parts, ", ")
}

// DominantNote says it; empty without one.
func (r *Report) DominantNote() string {
	d := r.Dominant
	if d == nil {
		return ""
	}
	s := fmt.Sprintf("one change in %d of %d drifted requests: %s; set aside, %d of %d drift in something else",
		d.Requests, d.Drifted, d.Explanation, d.Elsewhere, d.Drifted)
	if d.Rule != "" {
		return s + "; when the other stack is meant to answer so, add under headers: in allowlist.yaml " + d.Rule
	}
	repeat := "; if it changes from run to run, capture --repeat 2 proposes a rule"
	switch {
	case d.Captures:
		return s + "; fix it, or name it in the pull request (only drifts of a capture compared against a baseline are accepted or expected; baselines rebase records a switch of the server stack)" + repeat
	case d.Unreliable:
		return s + "; fix it, or capture again without --skip-fingerprint-check before driftgate accept or expect: drifts of a capture made with that flag are not taken"
	}
	return s + fmt.Sprintf("; fix it, or take it with driftgate accept group:%d (mode evolve) or driftgate expect group:%d --ref <ticket> (mode freeze)", d.Group, d.Group) + repeat
}

// dominantMin is how many requests must drift before one change is called
// the drift of almost all of them.
const dominantMin = 5

// FindDominant is the group of drifts that nine in ten requests with an
// unexpected drift or more have, when at least dominantMin have one; ok
// false otherwise. Expected drifts do not count: a request counts as
// drifting elsewhere when it has an unexpected drift of another group.
func (r *Report) FindDominant() (g Group, drifted, elsewhere int, ok bool) {
	count := map[string]int{}
	for _, res := range r.Results {
		if res.Outcome != Drift || len(res.Expected) > 0 {
			continue
		}
		seen := map[string]bool{}
		for _, d := range res.Drifts {
			if sig := d.Signature(); d.Expected == "" && !seen[sig] {
				seen[sig] = true
				count[sig]++
			}
		}
		if len(seen) > 0 {
			drifted++
		}
	}
	if drifted < dominantMin {
		return Group{}, 0, 0, false
	}
	best := ""
	for _, x := range r.Groups {
		if count[x.Signature] > count[best] {
			best = x.Signature
		}
	}
	if count[best]*10 < drifted*9 {
		return Group{}, 0, 0, false
	}
	for _, x := range r.Groups {
		if x.Signature == best {
			g = x
		}
	}
	g.Requests = count[best]
	for _, res := range r.Results {
		if res.Outcome != Drift || len(res.Expected) > 0 {
			continue
		}
		for _, d := range res.Drifts {
			if d.Expected == "" && d.Signature() != best {
				elsewhere++
				break
			}
		}
	}
	return g, drifted, elsewhere, true
}
