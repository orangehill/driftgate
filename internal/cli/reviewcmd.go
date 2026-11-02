package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
)

// reviewCmd shows the drift groups of the last comparison, each with an
// example, the requests it touches and the accept command that takes it.
// It never asks anything, so it works in CI.
func reviewCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	group := fs.Int("group", 0, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("review")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	r, _, err := readLastCompare(p)
	if err != nil {
		return 0, err
	}
	if *group != 0 && (*group < 1 || *group > len(r.Groups)) {
		return 0, configErr("--group %d: the last comparison has groups 1 to %d", *group, len(r.Groups))
	}
	fmt.Fprintf(out, "last comparison: %s against %s, %s (mode %s, level %s)\n", r.Tag, r.Against, r.Created, r.Mode, r.Level)
	why, err := staleComparison(p, r)
	if err != nil {
		return 0, err
	}
	canAccept := why == "" && p.Mode == config.Evolve
	switch {
	case why != "":
		fmt.Fprintf(out, "it is out of date: %s; run driftgate compare (or driftgate run) again before accepting\n", why)
	case p.Mode == config.Freeze:
		fmt.Fprintln(out, "mode is freeze: drifts are not accepted; every one is a change to fix")
	}
	acceptArgs := " --comparison " + comparisonID(r)
	if *cfg != "" {
		acceptArgs += " --config " + *cfg
	}
	acceptLine := func(sel []string) string {
		return fmt.Sprintf(`driftgate accept %s --reason "<why this change is intended>"%s`, strings.Join(sel, " "), acceptArgs)
	}

	done := map[string]bool{}
	for _, id := range r.Accepted {
		done[id] = true
	}
	// Drifts still open, by group.
	open := map[int][]compare.Drift{}
	groupOf := map[string]int{}
	for _, g := range r.Groups {
		groupOf[g.Signature] = g.N
	}
	for _, res := range r.Results {
		if res.Outcome != report.Drift || done[res.ID] {
			continue
		}
		for _, d := range res.Drifts {
			n := groupOf[d.Signature()]
			open[n] = append(open[n], d)
		}
	}
	linked := linkedGroups(r, done)

	if *group != 0 {
		g := r.Groups[*group-1]
		fmt.Fprintf(out, "\n[%d] %s\n", g.N, g.Signature)
		if len(open[g.N]) == 0 {
			fmt.Fprintln(out, "    accepted")
			return ExitClean, nil
		}
		for _, d := range open[g.N] {
			fmt.Fprintf(out, "    %-24s %s\n", d.Request, driftLine(d))
		}
		if canAccept {
			fmt.Fprintf(out, "\naccept: %s\n", acceptLine(groupSelectors(linked[g.N])))
		}
		return ExitClean, nil
	}

	shown := 0
	for _, g := range r.Groups {
		drifts := open[g.N]
		if len(drifts) == 0 {
			continue
		}
		shown++
		var ids []string
		seen := map[string]bool{}
		for _, d := range drifts {
			if !seen[d.Request] {
				seen[d.Request] = true
				ids = append(ids, d.Request)
			}
		}
		fmt.Fprintf(out, "\n[%d] %s  (%d requests, %d drifts)\n", g.N, report.Printable(g.Signature), len(ids), len(drifts))
		fmt.Fprintf(out, "    example:  %s  %s\n", drifts[0].Request, driftLine(drifts[0]))
		const maxIDs = 10
		list := strings.Join(ids, ", ")
		if len(ids) > maxIDs {
			list = fmt.Sprintf("%s … and %d more", strings.Join(ids[:maxIDs], ", "), len(ids)-maxIDs)
		}
		fmt.Fprintf(out, "    requests: %s\n", list)
		if !canAccept {
			continue
		}
		with := linked[g.N]
		if first := with[0]; first != g.N {
			fmt.Fprintf(out, "    accept:   together with group:%d (see there)\n", first)
			continue
		}
		fmt.Fprintf(out, "    accept:   %s\n", acceptLine(groupSelectors(with)))
		if len(with) > 1 {
			fmt.Fprintf(out, "              (these requests also drift in the other groups named, and a request is accepted whole)\n")
		}
	}

	var fresh []string
	for _, res := range r.Results {
		if acceptable(res) && res.Outcome == report.Skipped && !done[res.ID] {
			fresh = append(fresh, res.ID)
		}
	}
	if len(fresh) > 0 {
		fmt.Fprintf(out, "\nrequests without a baseline record: %s\n", strings.Join(fresh, ", "))
		if canAccept {
			fmt.Fprintf(out, "    accept:   %s\n", acceptLine(fresh))
		}
	}
	if len(r.Accepted) > 0 {
		fmt.Fprintf(out, "\naccepted since the comparison: %s\n", strings.Join(r.Accepted, ", "))
	}
	if shown == 0 && len(fresh) == 0 {
		fmt.Fprintln(out, "\nnothing to review")
		return ExitClean, nil
	}
	fmt.Fprintln(out, "\ndriftgate review --group N shows every drift of a group")
	if canAccept {
		fmt.Fprintln(out, `requests can also be accepted by ID or glob: driftgate accept <request|glob> --reason "…"`)
	}
	return ExitClean, nil
}

func driftLine(d compare.Drift) string {
	s := fmt.Sprintf("%-15s %s", d.Kind, report.Printable(d.Where))
	if d.Baseline != "" || d.Current != "" {
		s += fmt.Sprintf("  %s → %s", report.Printable(short(d.Baseline)), report.Printable(short(d.Current)))
	}
	return s
}

func groupSelectors(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprintf("group:%d", n)
	}
	return out
}

// linkedGroups returns, for every group, the groups that must be accepted
// with it: groups are linked when a request not yet accepted drifts in
// both, since a request is accepted with all of its drifts.
func linkedGroups(r *report.Report, done map[string]bool) map[int][]int {
	parent := map[int]int{}
	var find func(int) int
	find = func(n int) int {
		if p, ok := parent[n]; ok && p != n {
			parent[n] = find(p)
			return parent[n]
		}
		parent[n] = n
		return n
	}
	for _, res := range r.Results {
		if res.Outcome != report.Drift || done[res.ID] {
			continue
		}
		gs := driftGroups(r, res.Drifts)
		for _, g := range gs[1:] {
			parent[find(g)] = find(gs[0])
		}
	}
	members := map[int][]int{}
	for _, g := range r.Groups {
		root := find(g.N)
		members[root] = append(members[root], g.N)
	}
	out := map[int][]int{}
	for _, g := range r.Groups {
		ms := members[find(g.N)]
		sort.Ints(ms)
		out[g.N] = ms
	}
	return out
}
