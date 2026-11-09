package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
)

// Location is a route a group of drifts touches, from the inventory: the
// route, the action that handles it and the action's source file.
type Location struct {
	Route  string `json:"route"`
	Action string `json:"action,omitempty"`
	File   string `json:"file,omitempty"`
}

// maxLocations is how many routes a group keeps; RouteCount has them all.
const maxLocations = 20

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// explain says in plain words what a group of drifts is: how many there are
// and what changed. same reports that every drift of the group has the
// example's values; otherwise the values are only an example.
func explain(g Group, same bool) string {
	return fmt.Sprintf("%s in %s: %s", plural(g.Drifts, "drift"), plural(g.Requests, "request"), what(g.Example, same))
}

func what(d compare.Drift, same bool) string {
	where := compare.AnyItem(d.Where)
	values := func(max int) string {
		if d.Baseline == "" && d.Current == "" {
			return ""
		}
		b, c := DisplayPair(d, max)
		v := b + " → " + c
		if same {
			return " (" + v + ")"
		}
		return " (for example " + v + ")"
	}
	header := strings.TrimPrefix(where, "header:")
	switch d.Kind {
	case compare.KindNotCaptured:
		return "not captured this time"
	case compare.KindStatus:
		return "status changed" + values(40)
	case compare.KindHeaderMissing:
		return "header " + header + " is missing"
	case compare.KindHeaderExtra:
		return "header " + header + " is new"
	case compare.KindHeaderValue:
		return "header " + header + " changed value" + values(40)
	case compare.KindKeyMissing:
		return where + " is missing"
	case compare.KindKeyExtra:
		return where + " is new"
	case compare.KindKeyOrder:
		return "keys of " + where + " are in a different order"
	case compare.KindItemOrder:
		return "items of " + where + " are in a different order"
	case compare.KindArrayLength:
		return where + " changed length" + values(20)
	case compare.KindType:
		return where + " changed type " + d.Baseline + " → " + d.Current
	case compare.KindNumberLiteral:
		return where + " is the same number written differently" + values(40)
	case compare.KindFormat:
		if s := countChange(where, d.Values); s != "" {
			return s
		}
		return where + " no longer matches its allowlist format" + values(40)
	case compare.KindValue:
		return where + " changed value" + values(40)
	case compare.KindEncoding:
		switch {
		case d.Baseline == "layout":
			return "the same values with a different layout of whitespace"
		case strings.HasSuffix(d.Baseline, " bytes"):
			return "the same values written differently (escaping, byte order mark)"
		}
		return where + " is the same text escaped differently"
	case compare.KindElementMissing:
		return "element " + where + " is missing"
	case compare.KindElementExtra:
		return "element " + where + " is new"
	case compare.KindElementOrder:
		return "elements of " + where + " are in a different order"
	case compare.KindAttributeMissing:
		return "attribute " + where + " is missing"
	case compare.KindAttributeExtra:
		return "attribute " + where + " is new"
	case compare.KindAttributeValue:
		return "attribute " + where + " changed value" + values(40)
	case compare.KindText:
		return "text of " + where + " changed" + values(40)
	case compare.KindDoctype:
		switch {
		case d.Current == "":
			return "the page lost its document type declaration (" + d.Baseline + ")"
		case d.Baseline == "":
			return "the page has a document type declaration now (" + d.Current + ")"
		}
		return "the document type declaration changed" + values(60)
	case compare.KindBody:
		if strings.HasPrefix(where, "line ") {
			return where + " of the text changed" + values(40)
		}
		return "the body (not JSON) changed" + values(40)
	case compare.KindSideEffect:
		return "side effect on " + strings.TrimPrefix(where, "side-effects:") + values(40)
	case compare.KindExit:
		return "the command step ended with another exit code" + values(40)
	case compare.KindRepeat:
		return "the statuses of the repeated request changed" + values(40)
	case compare.KindCell:
		return "cell " + where + " changed" + values(40)
	case compare.KindCellFormat:
		return "cell " + where + " changed its format" + values(40)
	case compare.KindCellFormula:
		return "cell " + where + " changed its formula" + values(40)
	case compare.KindSheetMissing:
		if d.Current != "" {
			return "worksheet " + where + " is missing (" + d.Current + ": renamed?)"
		}
		return "worksheet " + where + " is missing"
	case compare.KindSheetExtra:
		return "worksheet " + where + " is new"
	case compare.KindSheetOrder:
		return "worksheets are in a different order" + values(60)
	}
	if where == "" {
		return d.Kind
	}
	return d.Kind + " at " + where
}

// Locate attaches to every group the routes its drifts touch. find returns
// the location of a request, or nil when no route of the inventory matches
// it. Routes are ordered by the number of drifts they get, then by name.
func (r *Report) Locate(find func(id string) *Location) {
	type acc struct {
		loc    Location
		drifts int
	}
	byGroup := map[string]map[string]*acc{}
	cache := map[string]*Location{}
	for _, res := range r.Results {
		for _, d := range res.Drifts {
			loc, ok := cache[d.Request]
			if !ok {
				loc = find(d.Request)
				cache[d.Request] = loc
			}
			if loc == nil {
				continue
			}
			sig := d.Signature()
			if byGroup[sig] == nil {
				byGroup[sig] = map[string]*acc{}
			}
			k := loc.Route + "\x00" + loc.Action + "\x00" + loc.File
			a := byGroup[sig][k]
			if a == nil {
				a = &acc{loc: *loc}
				byGroup[sig][k] = a
			}
			a.drifts++
		}
	}
	for i := range r.Groups {
		g := &r.Groups[i]
		var list []*acc
		for _, a := range byGroup[g.Signature] {
			list = append(list, a)
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].drifts != list[j].drifts {
				return list[i].drifts > list[j].drifts
			}
			if list[i].loc.Route != list[j].loc.Route {
				return list[i].loc.Route < list[j].loc.Route
			}
			if list[i].loc.Action != list[j].loc.Action {
				return list[i].loc.Action < list[j].loc.Action
			}
			return list[i].loc.File < list[j].loc.File
		})
		g.RouteCount = len(list)
		g.Routes = nil
		for j, a := range list {
			if j == maxLocations {
				break
			}
			g.Routes = append(g.Routes, a.loc)
		}
	}
}

// describe writes a location on one line: route, action and file, without
// repeating the file when the action is the file (Next.js, Nuxt).
func (l Location) describe(sep string) string {
	parts := []string{l.Route}
	if a := l.Action; a != "" && !(l.File == a || strings.HasSuffix(l.File, "/"+a)) {
		parts = append(parts, a)
	}
	if l.File != "" {
		parts = append(parts, l.File)
	}
	return strings.Join(parts, sep)
}

// countChange says that a header with an allowlist format gained or lost
// values (a cookie no longer set), naming the cookies of Set-Cookie: a
// rule lets the values change, not their number. Empty when the number
// did not change.
func countChange(where string, values [2][]string) string {
	b, c := len(values[0]), len(values[1])
	if b == c {
		return ""
	}
	s := fmt.Sprintf("%s has %s instead of %d", where, plural(c, "value"), b)
	if where == "header:set-cookie" {
		gone, added := cookieNames(values[0]), cookieNames(values[1])
		for n := range added {
			if gone[n] > 0 {
				k := min(gone[n], added[n])
				gone[n] -= k
				added[n] -= k
			}
		}
		if names := countedNames(gone); names != "" {
			s += ": " + names + " removed"
		}
		if names := countedNames(added); names != "" {
			if strings.Contains(s, " removed") {
				s += ", "
			} else {
				s += ": "
			}
			s += names + " new"
		}
		return s
	}
	if c < b {
		return s + ": " + plural(b-c, "value") + " removed"
	}
	return s + ": " + plural(c-b, "value") + " new"
}

// cookieNames counts the cookies of Set-Cookie values by name.
func cookieNames(values []string) map[string]int {
	out := map[string]int{}
	for _, v := range values {
		name, _, _ := strings.Cut(v, "=")
		out[strings.TrimSpace(name)]++
	}
	return out
}

func countedNames(m map[string]int) string {
	var names []string
	for n, k := range m {
		for range k {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
