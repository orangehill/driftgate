// Package report turns comparison results into output for people (text, and
// markdown for a pull-request comment) and for machines (JSON, JUnit XML for
// CI test reporters), with drifts grouped by signature and explained.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const Version = 1

// Outcomes of one request.
const (
	Pass     = "pass"
	Drift    = "drift"
	Skipped  = "skipped"
	Observed = "observed" // measured, never compared
)

// NoteNoBaseline is the note of a request the baseline has no response
// for: a new request (accept takes it into the baseline), or a baseline
// made for another catalog.
const NoteNoBaseline = "no baseline"

// NotePrivateMissing is the note of a request skipped because its private
// body is not on the machine that compared: a private body never enters the
// repository, and it is never compared by its ref alone.
const NotePrivateMissing = "private body not on this machine"

// Result is the outcome for one request.
type Result struct {
	ID      string          `json:"id"`
	Outcome string          `json:"outcome"`
	Level   string          `json:"level,omitempty"`
	Note    string          `json:"note,omitempty"`
	Drifts  []compare.Drift `json:"drifts,omitempty"`
	// StatusChange is how the status changed, when it did. Collapsed is
	// how many of the drifts follow from it (the body, headers other than
	// location and content type): text, markdown and JUnit summarize them
	// in one line by default; Drifts has them all.
	StatusChange *StatusChange `json:"status_change,omitempty"`
	Collapsed    int           `json:"collapsed,omitempty"`
	// Notes say how the comparison went where that is not a drift: an
	// array the allowlist pairs by key whose items were paired by position.
	Notes []string `json:"notes,omitempty"`
}

// Group is a set of drifts with the same signature: one cause. Explanation
// says in plain words what changed; Routes are the routes it touches (at
// most 20, of RouteCount), with their action and source file.
type Group struct {
	N           int           `json:"n"`
	Signature   string        `json:"signature"`
	Requests    int           `json:"requests"`
	Drifts      int           `json:"drifts"`
	Example     compare.Drift `json:"example"`
	Explanation string        `json:"explanation"`
	Routes      []Location    `json:"routes,omitempty"`
	RouteCount  int           `json:"route_count,omitempty"`
}

// Report is the result of one comparison.
type Report struct {
	Version  int      `json:"version"`
	Tag      string   `json:"tag"`
	Against  string   `json:"against"`
	Compared int      `json:"compared"`
	Passed   int      `json:"passed"`
	Drifted  int      `json:"drifted"`
	Skipped  int      `json:"skipped"`
	Observed int      `json:"observed"`
	Results  []Result `json:"results"`
	Groups   []Group  `json:"groups"`
	// The status changes shared by requests, the most common first: one
	// cause, whatever drifts follow from it.
	StatusChanges []StatusGroup `json:"status_changes,omitempty"`
	// What review, accept and report need to find the compared sets again.
	Baseline string `json:"baseline,omitempty"` // the baseline compared against, when it is one: its name
	Level    string `json:"level,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Created  string `json:"created,omitempty"`
	// What accept checks to know the comparison still describes the compared
	// sets (manifest of the baseline, records of the capture), and the
	// requests it has accepted from it since.
	BaselineDigest string   `json:"baseline_digest,omitempty"`
	CaptureDigest  string   `json:"capture_digest,omitempty"`
	Accepted       []string `json:"accepted,omitempty"`
	// Requests skipped because their private body is not on this machine;
	// the result is incomplete unless they were skipped on purpose
	// (--skip-private).
	PrivateMissing []string `json:"private_missing,omitempty"`
	SkipPrivate    bool     `json:"skip_private,omitempty"`
	// Requests skipped because the baseline has no response for them.
	NoBaseline []string `json:"no_baseline,omitempty"`
	// Allowlist rules that applied to no change in this comparison: a stale
	// rule (a renamed value, a request that is gone) stays unnoticed
	// otherwise. A warning, never a failure.
	UnusedRules []allowlist.Unused `json:"unused_rules,omitempty"`
	// Response times of the two sides, as information (CompareTimings).
	Timings *Timings `json:"timings,omitempty"`
	// Origins name the services whose two sides were captured from
	// different origins, each taken as the other ("http://localhost:8080 ≙
	// http://localhost:8081").
	Origins []string `json:"origins,omitempty"`
}

// Incomplete reports whether the comparison checked less than it should:
// private requests could not be compared and that was not accepted with
// --skip-private, or nothing at all was compared (--only matched no
// request, the baseline has none of the requests, every request is
// private or observed): a green result would mean nothing was checked.
// compare and run then exit 3.
func (r *Report) Incomplete() bool {
	return r.Compared == 0 || len(r.PrivateMissing) > 0 && !r.SkipPrivate
}

// NothingNote says why nothing was compared; empty when something was.
func (r *Report) NothingNote() string {
	if r.Compared > 0 {
		return ""
	}
	switch {
	case len(r.Results) == 0:
		return "nothing was compared: no request was selected (check --only and --target against the catalog's request IDs)"
	case len(r.NoBaseline) == len(r.Results):
		return "nothing was compared: the baseline has none of the selected requests (a baseline of another catalog, or renamed request IDs?)"
	case len(r.PrivateMissing) > 0 && len(r.PrivateMissing)+len(r.NoBaseline)+r.Observed == len(r.Results):
		return "nothing was compared: every request is private and its body is not on this machine, observed, or without a baseline"
	}
	return "nothing was compared"
}

// NoBaselineNote lists the requests the baseline has no response for;
// empty when there are none.
func (r *Report) NoBaselineNote() string {
	n := len(r.NoBaseline)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%s without a baseline, not compared: %s; new requests join the baseline with driftgate accept", plural(n, "request"), listIDs(r.NoBaseline))
}

// UnusedNote warns of the allowlist rules that applied to no change, up to
// ten of them; empty when every rule did.
func (r *Report) UnusedNote() string {
	n := len(r.UnusedRules)
	if n == 0 {
		return ""
	}
	var list []string
	for i, u := range r.UnusedRules {
		if i == 10 {
			list = append(list, fmt.Sprintf("and %d more", n-10))
			break
		}
		why := "nothing it applies to changed"
		if u.Why == "no request" {
			why = "it applies to no compared request"
		}
		list = append(list, u.RuleRef.String()+": "+why)
	}
	return fmt.Sprintf("warning: %s of the allowlist applied to no change in this comparison (a stale one can be removed): %s",
		plural(n, "rule"), strings.Join(list, "; "))
}

// ResultNotes lists the notes of the requests (Result.Notes), up to ten;
// empty when there are none.
func (r *Report) ResultNotes() string {
	var list []string
	n := 0
	for _, res := range r.Results {
		for _, note := range res.Notes {
			if n++; n <= 10 {
				list = append(list, res.ID+": "+note)
			}
		}
	}
	switch {
	case n == 0:
		return ""
	case n > 10:
		list = append(list, fmt.Sprintf("and %d more", n-10))
	}
	return "note: " + strings.Join(list, "; ")
}

// listIDs writes up to ten request IDs and how many more there are.
func listIDs(ids []string) string {
	if len(ids) > 10 {
		return fmt.Sprintf("%s and %d more", strings.Join(ids[:10], ", "), len(ids)-10)
	}
	return strings.Join(ids, ", ")
}

// PrivateNote says what happened to the private requests that could not be
// compared here; empty when there are none.
func (r *Report) PrivateNote() string {
	n := len(r.PrivateMissing)
	if n == 0 {
		return ""
	}
	ids := listIDs(r.PrivateMissing)
	what := plural(n, "private request")
	if r.SkipPrivate {
		return fmt.Sprintf("%s compared without the body with --skip-private (the private body is not on this machine; status, headers and side effects were compared): %s; this is a partial check", what, ids)
	}
	return fmt.Sprintf("the result is incomplete: %s could not be compared here (the private body is not on this machine): %s; compare them where the bodies are, or use an anonymized database so responses hold no personal data (--skip-private accepts a partial check)", what, ids)
}

// Save writes the report, as the last comparison, to path.
func (r *Report) Save(path string) error { return snapshot.WriteJSON(path, r) }

// Load reads a report Save wrote.
func Load(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.summarize()
	return &r, nil
}

// Build counts results and groups their drifts. Groups are ordered by the
// number of requests they touch, then by the number of drifts, then by
// signature.
func Build(tag, against string, results []Result) *Report {
	r := &Report{Version: Version, Tag: tag, Against: against, Results: results, Groups: []Group{}}
	type acc struct {
		g        Group
		requests map[string]bool
		same     bool // every drift has the example's values
	}
	groups := map[string]*acc{}
	for _, res := range results {
		switch res.Outcome {
		case Pass:
			r.Passed++
		case Drift:
			r.Drifted++
		case Skipped:
			r.Skipped++
		case Observed:
			r.Observed++
		}
		if res.Outcome == Pass || res.Outcome == Drift {
			r.Compared++
		}
		// A private body not on this machine: the request is skipped, or
		// drifts in what could be compared without the body.
		if res.Note == NotePrivateMissing {
			r.PrivateMissing = append(r.PrivateMissing, res.ID)
		}
		if res.Outcome == Skipped && res.Note == NoteNoBaseline {
			r.NoBaseline = append(r.NoBaseline, res.ID)
		}
		for _, d := range res.Drifts {
			sig := d.Signature()
			a := groups[sig]
			if a == nil {
				a = &acc{g: Group{Signature: sig, Example: d}, requests: map[string]bool{}, same: true}
				groups[sig] = a
			}
			a.same = a.same && d.Baseline == a.g.Example.Baseline && d.Current == a.g.Example.Current
			a.g.Drifts++
			a.requests[d.Request] = true
		}
	}
	for _, a := range groups {
		a.g.Requests = len(a.requests)
		a.g.Explanation = explain(a.g, a.same)
		r.Groups = append(r.Groups, a.g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		gi, gj := r.Groups[i], r.Groups[j]
		if gi.Requests != gj.Requests {
			return gi.Requests > gj.Requests
		}
		if gi.Drifts != gj.Drifts {
			return gi.Drifts > gj.Drifts
		}
		return gi.Signature < gj.Signature
	})
	for i := range r.Groups {
		r.Groups[i].N = i + 1
	}
	r.summarize()
	// The groups the default view leaves out (only in requests whose
	// status changed) come last, so the ones it lists are numbered from
	// [1] on, as review and accept group:N read them.
	if hidden := r.hiddenGroups(); len(hidden) > 0 {
		sort.SliceStable(r.Groups, func(i, j int) bool { return !hidden[r.Groups[i].N] && hidden[r.Groups[j].N] })
		for i := range r.Groups {
			r.Groups[i].N = i + 1
		}
	}
	return r
}

// JSON writes the report as indented JSON.
func (r *Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	return enc.Encode(r)
}

// Formats are the output formats of compare and report.
var Formats = []string{"text", "json", "junit", "markdown"}

// ValidFormat reports whether f is one of Formats.
func ValidFormat(f string) bool {
	for _, x := range Formats {
		if x == f {
			return true
		}
	}
	return false
}

// Write writes the report in a format, with up to maxDrifts drifts per
// request where the format lists them (DefaultDrifts: the default view).
func (r *Report) Write(w io.Writer, format string, maxDrifts int) error {
	switch format {
	case "json":
		return r.JSON(w)
	case "junit":
		return r.JUnit(w, maxDrifts)
	case "markdown":
		_, err := io.WriteString(w, r.Markdown(maxDrifts))
		return err
	case "text":
		r.Text(w, maxDrifts)
		return nil
	}
	return fmt.Errorf("unknown format %q (%s)", format, strings.Join(Formats, ", "))
}

// text is the explanation of a group, also for a report written before
// groups were explained.
func (g Group) text() string {
	if g.Explanation != "" {
		return g.Explanation
	}
	return fmt.Sprintf("%s  (%d requests, %d drifts)", g.Signature, g.Requests, g.Drifts)
}

// line writes one drift the way every format shows it: kind, place and the
// two values, each shortened. Private bodies show no more than this.
// pad is the width of the kind column.
func line(d compare.Drift, pad int) string {
	s := d.Kind
	switch {
	case pad > 0:
		s = fmt.Sprintf("%-*s %s", pad, d.Kind, Printable(d.Where))
	case d.Where != "":
		s += " " + Printable(d.Where)
	}
	if d.Baseline != "" || d.Current != "" {
		s += fmt.Sprintf("  %s → %s", Printable(short(d.Baseline)), Printable(short(d.Current)))
	}
	return s
}

// Printable escapes the characters of response-derived text that a terminal
// or a CI log would act on instead of showing: control characters (a line
// break would start a line of its own, where GitHub reads "::warning::" as a
// workflow command; ESC starts terminal sequences), line and paragraph
// separators and the marks that reorder text (bidi).
func Printable(s string) string {
	if strings.IndexFunc(s, unprintable) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unprintable(r) {
			fmt.Fprintf(&b, "\\u%04x", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unprintable(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f, r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c,
		r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == utf8.RuneError:
		return true
	}
	return false
}

func short(s string) string {
	const max = 80
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// Text writes a readable report: every drifted request with up to maxDrifts
// drifts, then the groups and a summary line.
func (r *Report) Text(w io.Writer, maxDrifts int) {
	for _, o := range r.Origins {
		fmt.Fprintln(w, "origins: "+Printable(o)+" (own URLs compared as the same)")
	}
	for _, res := range r.Results {
		if res.Outcome != Drift {
			continue
		}
		fmt.Fprintf(w, "DRIFT  %s\n", res.ID)
		for _, l := range driftLines(res, maxDrifts, 15) {
			fmt.Fprintln(w, "         "+l)
		}
	}
	if len(r.Groups) > 0 {
		// The default view lists the status changes under the status group
		// and leaves out the groups that only follow from them.
		summary := maxDrifts == DefaultDrifts
		hidden := map[int]bool{}
		if summary {
			hidden = r.hiddenGroups()
		}
		fmt.Fprintln(w, "\nGroups:")
		for _, g := range r.Groups {
			if hidden[g.N] {
				continue
			}
			fmt.Fprintf(w, "  [%d] %s\n", g.N, Printable(g.text()))
			if len(g.Routes) > 0 {
				line := "      " + Printable(g.Routes[0].describe("  "))
				if g.RouteCount > 1 {
					line += fmt.Sprintf("  (+%d more routes)", g.RouteCount-1)
				}
				fmt.Fprintln(w, line)
			}
			if summary && g.Example.Kind == compare.KindStatus {
				for _, s := range r.StatusChanges {
					fmt.Fprintf(w, "      %s (%s)\n", s.head(), s.ids(Printable))
				}
			}
		}
		if n := len(hidden); n > 0 {
			fmt.Fprintf(w, "  … %s only in requests whose status changed, summarized there (driftgate review lists them)\n", plural(n, "more group"))
		}
	}
	for _, res := range r.Results {
		switch res.Outcome {
		case Skipped:
			fmt.Fprintf(w, "SKIP   %s (%s)\n", res.ID, Printable(res.Note))
		case Observed:
			fmt.Fprintf(w, "OBSERVE %s (%s)\n", res.ID, Printable(res.Note))
		}
	}
	fmt.Fprintf(w, "\n%s against %s: %d compared, %d passed, %d drifted, %d skipped, %d observed\n", r.Tag, r.Against, r.Compared, r.Passed, r.Drifted, r.Skipped, r.Observed)
	for _, note := range []string{r.NoBaselineNote(), r.PrivateNote(), r.NothingNote(), r.UnusedNote(), r.ResultNotes(), r.TimingsNote()} {
		if note != "" {
			fmt.Fprintln(w, Printable(note))
		}
	}
	if r.Drifted == 0 && r.Compared > 0 && !r.Incomplete() {
		fmt.Fprintln(w, "No drift ✓")
	}
}
