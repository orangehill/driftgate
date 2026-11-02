package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
)

// DefaultDrifts is the maxDrifts of compare, run and report without
// --max-drifts: up to 20 drifts per request, and a request whose status
// changed leads with the change, keeps what says where it went (location,
// content type, side effects) and summarizes the rest in one line. Any
// other maxDrifts lists the drifts as they are.
const DefaultDrifts = -1

// defaultMax is how many drifts a request lists by default.
const defaultMax = 20

// StatusChange is how the status of a request changed; Location is where
// a redirect now points, when its location header drifted.
type StatusChange struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Location string `json:"location,omitempty"`
}

// StatusGroup is a status change shared by requests: the same cause, read
// once.
type StatusGroup struct {
	StatusChange
	Requests []string `json:"requests"`
}

// text writes the change on one line: "200 → 302, redirects to /login".
func (s StatusChange) text() string {
	t := Printable(short(s.From)) + " → " + Printable(short(s.To))
	if s.Location != "" {
		t += ", redirects to " + Printable(short(s.Location))
	}
	return t
}

// summarize finds the status change of every drifted request, how many of
// its drifts follow from it, and the requests that share a change. Build
// and Load call it, so a report saved before it existed reads the same.
func (r *Report) summarize() {
	r.StatusChanges = nil
	byChange := map[StatusChange]int{}
	for i := range r.Results {
		res := &r.Results[i]
		res.StatusChange, res.Collapsed = nil, 0
		if res.Outcome != Drift {
			continue
		}
		res.StatusChange = statusChange(res.Drifts)
		if res.StatusChange == nil {
			continue
		}
		for _, d := range res.Drifts {
			if collapsible(d) {
				res.Collapsed++
			}
		}
		n, ok := byChange[*res.StatusChange]
		if !ok {
			n = len(r.StatusChanges)
			byChange[*res.StatusChange] = n
			r.StatusChanges = append(r.StatusChanges, StatusGroup{StatusChange: *res.StatusChange})
		}
		r.StatusChanges[n].Requests = append(r.StatusChanges[n].Requests, res.ID)
	}
	sort.SliceStable(r.StatusChanges, func(i, j int) bool {
		return len(r.StatusChanges[i].Requests) > len(r.StatusChanges[j].Requests)
	})
}

// statusChange returns how the status changed, or nil when it did not.
func statusChange(drifts []compare.Drift) *StatusChange {
	var s *StatusChange
	location := ""
	for _, d := range drifts {
		switch {
		case d.Kind == compare.KindStatus:
			s = &StatusChange{From: d.Baseline, To: d.Current}
		case (d.Kind == compare.KindHeaderExtra || d.Kind == compare.KindHeaderValue) && d.Where == "header:location":
			var vs []string
			if json.Unmarshal([]byte(d.Current), &vs) == nil && len(vs) > 0 {
				location = vs[0]
			}
		}
	}
	if s != nil && len(s.To) == 3 && s.To[0] == '3' {
		s.Location = location
	}
	return s
}

// collapsible reports whether a drift of a request whose status changed
// follows from the change and is summarized by default: the body and the
// headers other than location and content type. The status, side effects,
// command steps and repeated requests stay listed.
func collapsible(d compare.Drift) bool {
	switch d.Kind {
	case compare.KindStatus, compare.KindNotCaptured, compare.KindSideEffect, compare.KindExit, compare.KindRepeat:
		return false
	case compare.KindHeaderMissing, compare.KindHeaderExtra, compare.KindHeaderValue:
		h := strings.TrimPrefix(d.Where, "header:")
		return h != "location" && h != "content-type"
	}
	return true
}

func isHeader(d compare.Drift) bool {
	return d.Kind == compare.KindHeaderMissing || d.Kind == compare.KindHeaderExtra || d.Kind == compare.KindHeaderValue
}

// bodyKind names what a body drift is in: html, json, cells or text.
func bodyKind(d compare.Drift) string {
	switch d.Kind {
	case compare.KindElementMissing, compare.KindElementExtra, compare.KindElementOrder,
		compare.KindAttributeMissing, compare.KindAttributeExtra, compare.KindAttributeValue, compare.KindText:
		return "html"
	case compare.KindCell, compare.KindSheetMissing, compare.KindSheetExtra, compare.KindSheetOrder:
		return "cells"
	case compare.KindBody:
		return "text"
	case compare.KindEncoding:
		return "encoding"
	}
	return "json"
}

// driftLines lists the drifts of a request: up to maxDrifts, then how many
// more there are; with DefaultDrifts a request whose status changed leads
// with the change and ends with one line for the drifts that follow from
// it. pad is the width of the kind column.
func driftLines(res Result, maxDrifts, pad int) []string {
	drifts := res.Drifts
	var out []string
	var rest []compare.Drift
	if maxDrifts == DefaultDrifts {
		maxDrifts = defaultMax
		if res.StatusChange != nil {
			out = append(out, "status "+res.StatusChange.text())
			var kept []compare.Drift
			for _, d := range drifts {
				switch {
				case d.Kind == compare.KindStatus:
				case collapsible(d):
					rest = append(rest, d)
				default:
					kept = append(kept, d)
				}
			}
			drifts = kept
		}
	}
	for i, d := range drifts {
		if i == maxDrifts {
			out = append(out, fmt.Sprintf("… %d more", len(drifts)-maxDrifts))
			break
		}
		out = append(out, line(d, pad))
	}
	if len(rest) > 0 {
		out = append(out, collapsedLine(rest))
	}
	return out
}

// collapsedLine summarizes the drifts that follow from a status change:
// "body differs: 6 drifts, mostly html; 1 other header differs (vary)".
func collapsedLine(rest []compare.Drift) string {
	var headers []string
	seen := map[string]bool{}
	kinds := map[string]int{}
	body := 0
	for _, d := range rest {
		if isHeader(d) {
			if h := strings.TrimPrefix(d.Where, "header:"); !seen[h] {
				seen[h] = true
				headers = append(headers, Printable(h))
			}
			continue
		}
		body++
		kinds[bodyKind(d)]++
	}
	var parts []string
	if body > 0 {
		top := ""
		for k, n := range kinds {
			if n > kinds[top] || n == kinds[top] && k < top {
				top = k
			}
		}
		if kinds[top] == body {
			parts = append(parts, "body differs: "+plural(body, top+" drift"))
		} else {
			parts = append(parts, fmt.Sprintf("body differs: %s, mostly %s", plural(body, "drift"), top))
		}
	}
	if n := len(headers); n > 0 {
		names := headers
		if n > 3 {
			names = append(names[:3:3], fmt.Sprintf("%d more", n-3))
		}
		verb := "differs"
		if n > 1 {
			verb = "differ"
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", plural(n, "other header"), verb, strings.Join(names, ", ")))
	}
	return strings.Join(parts, "; ") + "; listed with --max-drifts N and in the JSON report"
}

// hiddenGroups returns the groups the default view leaves out: those whose
// every drift follows from a status change and is summarized in its request.
func (r *Report) hiddenGroups() map[int]bool {
	collapsed := map[string]int{}
	for _, res := range r.Results {
		if res.Outcome != Drift || res.StatusChange == nil {
			continue
		}
		for _, d := range res.Drifts {
			if collapsible(d) {
				collapsed[d.Signature()]++
			}
		}
	}
	hidden := map[int]bool{}
	for _, g := range r.Groups {
		if n := collapsed[g.Signature]; n > 0 && n == g.Drifts {
			hidden[g.N] = true
		}
	}
	return hidden
}

// head writes a status change shared by requests:
// "3 requests: 200 → 302 to /login".
func (s StatusGroup) head() string {
	t := Printable(short(s.From)) + " → " + Printable(short(s.To))
	if s.Location != "" {
		t += " to " + Printable(short(s.Location))
	}
	return plural(len(s.Requests), "request") + ": " + t
}

// ids lists the requests of a status change, at most ten, each written by
// id.
func (s StatusGroup) ids(id func(string) string) string {
	const maxIDs = 10
	ids := make([]string, 0, min(len(s.Requests), maxIDs))
	for i, x := range s.Requests {
		if i == maxIDs {
			break
		}
		ids = append(ids, id(x))
	}
	list := strings.Join(ids, ", ")
	if more := len(s.Requests) - maxIDs; more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return list
}
