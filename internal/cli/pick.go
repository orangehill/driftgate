package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/report"
)

// pickRules say what accept or expect can take from the last comparison.
type pickRules struct {
	verb string // "accept", "expect"
	// open reports a request a glob or a group takes.
	open func(report.Result) bool
	// refuse says why a request named by its ID cannot be taken; empty
	// when it can.
	refuse func(report.Result) string
	// none is what a glob that takes no request matches no request with.
	none string
}

// picked is what selections took: the drifts of each request (indexes
// into its drifts; every one for a request taken whole by its ID or a
// glob), and the requests named by their ID.
type picked struct {
	drifts    map[string][]int
	whole     map[string]bool
	named     map[string]bool
	groupArgs []string
}

// pick resolves selections (request IDs, globs over IDs, group:N) against
// the last comparison. A group takes the drifts with its signature of the
// open requests; a request that was not captured is taken only by its ID,
// as taking it removes it.
func pick(r *report.Report, args []string, rules pickRules) (*picked, error) {
	byID := map[string]report.Result{}
	for _, res := range r.Results {
		byID[res.ID] = res
	}
	p := &picked{drifts: map[string][]int{}, whole: map[string]bool{}, named: map[string]bool{}}
	groups := map[string]bool{}
	for _, a := range args {
		if n, ok := strings.CutPrefix(a, "group:"); ok {
			num, err := strconv.Atoi(n)
			if err != nil || num < 1 || num > len(r.Groups) {
				return nil, configErr("%s: the last comparison has groups 1 to %d", a, len(r.Groups))
			}
			groups[r.Groups[num-1].Signature] = true
			p.groupArgs = append(p.groupArgs, a)
			continue
		}
		if strings.ContainsAny(a, "*?") {
			matched := 0
			for _, res := range r.Results {
				if glob.Match(a, res.ID) && rules.open(res) {
					p.whole[res.ID] = true
					matched++
				}
			}
			if matched == 0 {
				return nil, configErr("%s matches no request %s in the last comparison", a, rules.none)
			}
			continue
		}
		res, ok := byID[a]
		if !ok {
			return nil, configErr("%s is not in the last comparison", a)
		}
		if why := rules.refuse(res); why != "" {
			return nil, configErr("%s %s", a, why)
		}
		p.whole[a], p.named[a] = true, true
	}
	for _, res := range r.Results {
		switch {
		case p.whole[res.ID]:
			all := make([]int, len(res.Drifts))
			for i := range all {
				all[i] = i
			}
			p.drifts[res.ID] = all
		case len(groups) > 0 && rules.open(res):
			for i, d := range res.Drifts {
				if groups[d.Signature()] {
					p.drifts[res.ID] = append(p.drifts[res.ID], i)
				}
			}
		}
	}
	var gone []string
	for id := range p.drifts {
		for _, d := range byID[id].Drifts {
			if d.Kind == compare.KindNotCaptured && !p.named[id] {
				gone = append(gone, id)
				break
			}
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		what := "accepting them removes them from the baseline"
		if rules.verb == "expect" {
			what = "to expect that they are gone"
		}
		return nil, configErr("%s were not captured this time; %s, so name each by its ID to do that", strings.Join(gone, ", "), what)
	}
	return p, nil
}

// partial lists the requests of which a selection took only some drifts,
// with the groups of the others; accept takes a request whole.
func (p *picked) partial(r *report.Report) []string {
	groupOf := map[string]int{}
	for _, g := range r.Groups {
		groupOf[g.Signature] = g.N
	}
	var out []string
	for _, res := range r.Results {
		got := p.drifts[res.ID]
		if len(got) == 0 || len(got) == len(res.Drifts) {
			continue
		}
		in := map[int]bool{}
		for _, i := range got {
			in[i] = true
		}
		missing := map[int]int{}
		n := 0
		for i, d := range res.Drifts {
			if !in[i] {
				missing[groupOf[d.Signature()]]++
				n++
			}
		}
		list := make([]string, 0, len(missing))
		for g, c := range missing {
			list = append(list, fmt.Sprintf("group:%d (%d)", g, c))
		}
		sort.Strings(list)
		out = append(out, fmt.Sprintf("  %s: %d of %d drifts not selected, in %s", res.ID, n, len(res.Drifts), strings.Join(list, ", ")))
	}
	return out
}

// ids are the requests a selection took, sorted.
func (p *picked) ids() []string {
	out := make([]string, 0, len(p.drifts))
	for id := range p.drifts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
