package allowlist

import (
	"fmt"
	"strings"

	"github.com/orangehill/driftgate/internal/glob"
)

// Checks of driftgate allowlist check: what a rule does that its author
// probably did not mean. Lint finds those that need only the list; the
// command adds those that need the catalog and a baseline.
const (
	CheckNoRequest  = "no-request"          // the rule applies to no compared request
	CheckNoPlace    = "no-place"            // it reaches nothing in the baseline bodies of its requests
	CheckAnyLength  = "any-length-values"   // any_length without format whose glob reaches values
	CheckRawRequest = "raw-without-request" // a $raw rule for every request
	CheckBroad      = "broad"               // see broad
	CheckShadowed   = "shadowed"            // an earlier rule always decides first
	CheckDuplicate  = "duplicate"           // the same rule again
)

// Finding is one problem of a rule.
type Finding struct {
	RuleRef
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

func (f Finding) String() string { return f.RuleRef.String() + ": " + f.Check + ": " + f.Detail }

// Lint checks the rules of a list by themselves: a $raw rule without
// request, rules that reach far more than a value (broad), and rules that
// never decide because an earlier one always does (shadowed, duplicate).
func (l *List) Lint() []Finding {
	var out []Finding
	for i := range l.Headers {
		r := &l.Headers[i]
		for j := range l.Headers[:i] {
			a := &l.Headers[j]
			if !strings.EqualFold(a.Name, r.Name) || !covers(a.Target, a.Request, r.Target, r.Request) {
				continue
			}
			f := Finding{RuleRef: headerRef(i, r), Check: CheckShadowed, Detail: fmt.Sprintf("header rule %d comes first for every request this one applies to, so this one never decides", j+1)}
			if a.Target == r.Target && a.Request == r.Request && a.Format == r.Format && a.IgnoreCase == r.IgnoreCase {
				f.Check, f.Detail = CheckDuplicate, fmt.Sprintf("the same rule as header rule %d", j+1)
			}
			out = append(out, f)
			break
		}
	}
	for i := range l.Body {
		r := &l.Body[i]
		ref := bodyRef(i, r)
		if r.Path == RawPath && r.Request == "" {
			out = append(out, Finding{RuleRef: ref, Check: CheckRawRequest,
				Detail: "a $raw rule without request lets the whole body of every response that is not JSON change while it matches the format; name the requests whose pages it is about"})
		}
		if why := broad(r); why != "" {
			out = append(out, Finding{RuleRef: ref, Check: CheckBroad, Detail: why})
		}
		if f, ok := l.shadowed(i); ok {
			out = append(out, f)
		}
	}
	// The first array rule that reaches an array pairs its items.
	for i := range l.Arrays {
		r := &l.Arrays[i]
		for j := range l.Arrays[:i] {
			a := &l.Arrays[j]
			if !covers(a.Target, a.Request, r.Target, r.Request) || !pathCovers(a.Path, r.Path) {
				continue
			}
			f := Finding{RuleRef: arrayRef(i, r), Check: CheckShadowed, Detail: fmt.Sprintf("array rule %d comes first for every array this one reaches, so this one never decides", j+1)}
			if a.Target == r.Target && a.Request == r.Request && a.Path == r.Path && a.Key == r.Key && a.AnyOrder == r.AnyOrder {
				f.Check, f.Detail = CheckDuplicate, fmt.Sprintf("the same rule as array rule %d", j+1)
			}
			out = append(out, f)
			break
		}
	}
	return out
}

// roots are where a body path starts: a path that names nothing after its
// root but wildcards reaches the whole body.
var roots = []string{"$", "html", "self.__next_f$", "xlsx:", "csv!"}

// anyText are values of every shape; a format that matches all of them
// lets any value change.
var anyText = []string{"x", "0", "-1.5", "true", "null", "2026-01-01T00:00:00Z", "a b", "é", "<p>x</p>", `{"a":1}`}

// broad says why a body rule reaches far more than a value; "" when it
// does not. A rule with parts is exempt from the path checks: only what its
// format matches may change, wherever that is (an asset hash in a page).
func broad(r *BodyRule) string {
	wild := strings.ContainsAny(r.Path, "*?")
	if !r.Parts && (strings.HasPrefix(r.Path, "*") || strings.HasPrefix(r.Path, "?")) {
		return "the path starts with a wildcard, so it reaches every kind of body (JSON, pages, the RSC payload) at any depth; start it at $ or html and name the place (*.updated_at → $.data[*].updated_at)"
	}
	if !r.Parts && wild {
		for _, root := range roots {
			if rest, ok := strings.CutPrefix(r.Path, root); ok && strings.Trim(rest, "*?.[]>!") == "" {
				return "the path names nothing after " + root + " but wildcards, so it reaches the whole body; name the place"
			}
		}
	}
	if r.re != nil && !r.Parts && (wild || r.Path == RawPath) {
		for _, s := range anyText {
			if !r.re.MatchString(s) {
				return ""
			}
		}
		return fmt.Sprintf("the format %q accepts any value and the path is a glob, so every value it reaches may change unchecked; write the format of the value", r.Format)
	}
	return ""
}

// shadowed reports a body rule that never decides: lookups take the first
// rule with a whole-value format and the first any_length rule, and a rule
// with parts only when no whole-value rule matches (match). The kinds of
// rules (values, cells, the whole body) are looked up apart.
func (l *List) shadowed(i int) (Finding, bool) {
	r := &l.Body[i]
	kind := func(x *BodyRule) int {
		switch {
		case x.Path == RawPath:
			return 2
		case IsCellPath(x.Path):
			return 1
		}
		return 0
	}
	whole := func(x *BodyRule) bool { return x.Format != "" && !x.Parts }
	// The earliest rule that takes the format's part, and the length's.
	format, length := -1, -1
	for j := range l.Body {
		a := &l.Body[j]
		if j == i || kind(a) != kind(r) || !covers(a.Target, a.Request, r.Target, r.Request) || !ruleCovers(a, r) {
			continue
		}
		if format < 0 && whole(a) && (j < i || r.Parts) && r.Format != "" {
			format = j
		}
		if length < 0 && a.AnyLength && j < i && r.AnyLength {
			length = j
		}
	}
	if (r.Format != "" && format < 0) || (r.AnyLength && length < 0) {
		return Finding{}, false
	}
	by := max(format, length)
	a := &l.Body[by]
	f := Finding{RuleRef: bodyRef(i, r), Check: CheckShadowed,
		Detail: fmt.Sprintf("body rule %d decides first everywhere this one applies, so this one never decides", by+1)}
	if a.Target == r.Target && a.Request == r.Request && a.Path == r.Path && a.Format == r.Format && a.AnyLength == r.AnyLength && a.Parts == r.Parts {
		f.Check, f.Detail = CheckDuplicate, fmt.Sprintf("the same rule as body rule %d", by+1)
	}
	return f, true
}

// covers reports whether a rule with target and request a applies to
// every request a rule with target and request b applies to.
func covers(aTarget, aRequest, bTarget, bRequest string) bool {
	return (aTarget == "" || aTarget == bTarget) && (aRequest == "" || aRequest == bRequest || bRequest != "" && pathCovers(aRequest, bRequest))
}

// ruleCovers reports whether body rule a reaches every path rule b
// reaches: two cell rules by worksheet, column and row apart (cellGlob). A
// cell rule reaches no more than its path as a glob does, so a plain glob
// covers it by pathCovers; a cell rule is taken to cover a plain glob
// (xlsx:Users*) only when they are the same.
func ruleCovers(a, b *BodyRule) bool {
	switch {
	case a.cell != nil && b.cell != nil:
		return pathCovers(a.cell.sheet, b.cell.sheet) && pathCovers(a.cell.col, b.cell.col) && pathCovers(a.cell.row, b.cell.row)
	case a.cell != nil:
		return a.Path == b.Path
	}
	return pathCovers(a.Path, b.Path)
}

// pathCovers reports whether glob a matches every name glob b matches. It
// is decided soundly, never guessed: a without ? matches the text of b
// (each * of b then falls inside a * of a, and a's literal characters
// meet b's own); otherwise only when they are equal.
func pathCovers(a, b string) bool {
	return a == b || !strings.Contains(a, "?") && glob.Match(a, b)
}
