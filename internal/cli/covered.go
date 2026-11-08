package cli

import (
	"regexp"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// notCovered is what a comparison of the requests reqs (the catalog cat,
// or those --only selected: only) against the set base, named against,
// leaves uncovered (report.NotCovered). cov is the coverage run checked
// already, or nil: it is then taken from the route inventory, when there
// is one, and the answers base holds (its records are read already); with
// --only, routes are not judged (most would lack a compared request). It
// never fails: what cannot be read is left out.
func notCovered(p *config.Project, sel selection, cat *catalog.Catalog, reqs []catalog.Request, only bool, against string, base *snapshot.Set, results []report.Result, cov *routes.Coverage) *report.NotCovered {
	n := &report.NotCovered{Only: only, Against: against}
	byID := make(map[string]catalog.Request, len(reqs))
	for _, req := range reqs {
		byID[req.ID] = req
		n.Catalog.Requests++
		switch {
		case req.IsCommand():
			n.Catalog.Commands++
		case req.Mutating():
			n.Catalog.Mutating++
		default:
			n.Catalog.Reads++
		}
		if req.Multipart != nil {
			n.Catalog.Multipart++
		}
	}
	compared := cat
	if only {
		compared = &catalog.Catalog{Version: cat.Version, Requests: reqs}
		cov = nil
	}
	var h *routes.Health
	if cov == nil {
		answers := map[string]routes.Answer{}
		for _, req := range compared.Requests {
			if rec, err := base.Record(req.ID); err == nil && rec != nil && rec.Exit == nil {
				answers[req.ID] = answerOf(rec.Status, rec.Error, rec.Repeat, rec.Headers)
			}
		}
		inv, excl, err := loadInventory(p)
		switch {
		case err == nil && !only:
			cov = coverageOf(sel, cat, inv, excl, answers)
		case err == nil:
			h = routes.CheckHealth(compared, answers, excl.KnownFailures)
		default:
			if excl, err := routes.LoadExclusions(p.Path("exclusions.yaml")); err == nil {
				h = routes.CheckHealth(compared, answers, excl.KnownFailures)
			}
		}
	}
	if cov != nil {
		n.RoutesChecked, h = true, cov.Health
		failed := map[string]bool{}
		for _, f := range cov.Failed {
			failed[f.Route] = true
		}
		var none []string
		for _, k := range cov.Uncovered {
			if !failed[k] {
				none = append(none, k)
			}
		}
		n.NoRequest = report.NewGap(none)
		n.NoInvalidBody = report.NewGap(cov.NoInvalidBody)
	}
	ids := func(fs []routes.Failure) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return out
	}
	if h != nil {
		n.FailedInBaseline, n.KnownFailures = report.NewGap(ids(h.Failed)), report.NewGap(ids(h.Known))
	}
	var observed, skipped []string
	for _, res := range results {
		req := byID[res.ID]
		switch {
		case res.Outcome == report.Observed:
			observed = append(observed, res.ID)
		case res.Outcome == report.Skipped && (req.Mutating() || req.IsCommand()):
			skipped = append(skipped, res.ID)
		}
	}
	n.Observed, n.MutatingSkipped = report.NewGap(observed), report.NewGap(skipped)
	return n
}

// answerOf is how a request fared, from its record.
func answerOf(status int, errText, repeat string, headers [][2]string) routes.Answer {
	a := routes.Answer{Status: status, Error: errText, Repeat: repeat}
	for _, h := range headers {
		if h[0] == "location" {
			a.Location = h[1]
			break
		}
	}
	return a
}

// stackHeaders are the headers that name the server stack, not what the
// application answers: of these alone a change on almost every request
// gets an allowlist rule proposed.
var stackHeaders = []string{"server", "x-powered-by", "via"}

// dominant is the one change almost every request with an unexpected
// drift has (report.FindDominant). Of a header of the server stack whose
// new value is the same single value on every request it gets the rule
// that allows that value exactly, and nothing wider; of anything else no
// rule: a change between builds is fixed, accepted or expected. nil
// without one.
func dominant(r *report.Report) *report.Dominant {
	g, drifted, elsewhere, ok := r.FindDominant()
	if !ok {
		return nil
	}
	_, what, _ := strings.Cut(g.Explanation, ": ") // after "N drifts in M requests"
	d := &report.Dominant{Group: g.N, Signature: g.Signature, Explanation: what, Requests: g.Requests, Drifted: drifted, Elsewhere: elsewhere}
	name := strings.TrimPrefix(g.Example.Where, "header:")
	if g.Example.Kind != compare.KindHeaderValue || !slices.Contains(stackHeaders, name) {
		return d
	}
	value := ""
	for _, res := range r.Results {
		for _, x := range res.Drifts {
			if x.Expected != "" || x.Signature() != g.Signature {
				continue
			}
			if len(x.Values[1]) != 1 || value != "" && x.Values[1][0] != value {
				return d // more values than one: no rule
			}
			value = x.Values[1][0]
		}
	}
	if value != "" {
		d.Rule = "- {name: " + yamlQuote(name) + ", format: " + yamlQuote(regexp.QuoteMeta(value)) + ", reason: '…'}"
	}
	return d
}
