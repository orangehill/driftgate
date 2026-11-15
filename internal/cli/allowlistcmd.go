package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// lintResult is the output of allowlist check.
type lintResult struct {
	Baseline string              `json:"baseline,omitempty"` // the baseline read; empty when there is none
	Rules    int                 `json:"rules"`
	Findings []allowlist.Finding `json:"findings"`
	Notes    []string            `json:"notes,omitempty"`
}

// allowlistCheck lints the allowlist without capturing anything: the
// rules by themselves (allowlist.Lint), against the catalog (a rule for no
// request) and against the bodies of a baseline (a rule that reaches
// nothing, any_length over values). Exit 1 on findings.
func allowlistCheck(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("allowlist check", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	name := fs.String("baseline", "local", "")
	format := fs.String("format", "text", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("allowlist check")
	}
	if *format != "text" && *format != "json" {
		return 0, configErr("--format must be text or json")
	}
	p, cat, allow, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	res := &lintResult{Rules: len(allow.Headers) + len(allow.Body) + len(allow.Arrays), Findings: allow.Lint()}
	set, err := p.Baseline(*name)
	if err != nil {
		return 0, failed(err)
	}
	switch {
	case set.Exists():
		res.Baseline = *name
	case flagGiven(fs, "baseline"):
		return 0, configErr("no baseline named %q", *name)
	default:
		set = nil
		res.Notes = append(res.Notes, fmt.Sprintf("no baseline %s yet: rules were not checked against response bodies", *name))
	}
	found, notes, err := lintPlaces(p, cat, allow, set)
	if err != nil {
		return 0, err
	}
	res.Findings = append(res.Findings, found...)
	res.Notes = append(res.Notes, notes...)
	sortFindings(res.Findings)
	if *format == "json" {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", " ")
		if err := enc.Encode(res); err != nil {
			return 0, err
		}
	} else {
		for _, f := range res.Findings {
			fmt.Fprintln(out, report.Printable(f.String()))
		}
		for _, n := range res.Notes {
			fmt.Fprintln(out, "note: "+report.Printable(n))
		}
		against := "no baseline"
		if res.Baseline != "" {
			against = "baseline " + res.Baseline
		}
		if len(res.Findings) == 0 {
			fmt.Fprintf(out, "%d rules checked against the catalog and %s: no findings ✓\n", res.Rules, against)
		} else {
			fmt.Fprintf(out, "%d rules checked against the catalog and %s: %d findings\n", res.Rules, against, len(res.Findings))
		}
	}
	if len(res.Findings) > 0 {
		return ExitDrift, nil
	}
	return ExitClean, nil
}

func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
	return given
}

// sortFindings orders findings by rule: header rules first, then body
// rules, each in the order of the file.
func sortFindings(list []allowlist.Finding) {
	rank := map[string]int{"header": 0, "body": 1, "array": 2}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if rank[a.Kind] < rank[b.Kind] || rank[a.Kind] == rank[b.Kind] && a.Rule <= b.Rule {
				break
			}
			list[j-1], list[j] = b, a
		}
	}
}

// ruleUse is what the catalog and the baseline say about one rule.
type ruleUse struct {
	requests int    // compared requests it applies to
	place    bool   // it reaches something in a baseline response
	values   string // an any_length rule without format: a value path it reaches
	seen     int    // its requests whose baseline response was looked at
	unread   int    // its requests whose private body could not be read here
}

// lintPlaces checks every rule against the compared requests of the
// catalog (observed ones are never compared) and the responses the
// baseline holds for them. Private bodies are read when the key is at hand
// and the body is on this machine; a rule that could reach something in a
// body not read, or whose requests have no response in the baseline, is
// not reported as reaching nothing: a note lists it.
func lintPlaces(p *config.Project, cat *catalog.Catalog, allow *allowlist.List, set *snapshot.Set) ([]allowlist.Finding, []string, error) {
	applies := func(target, request string, req catalog.Request) bool {
		return (target == "" || target == req.Target) && (request == "" || glob.Match(request, req.ID))
	}
	headers := make([]ruleUse, len(allow.Headers))
	body := make([]ruleUse, len(allow.Body))
	arrays := make([]ruleUse, len(allow.Arrays))
	keyChecked, keyOK := false, false
	unread := map[string]string{} // request → why its body was not read
	for _, req := range cat.Requests {
		if req.Observe {
			continue
		}
		var hs, bs, as []int
		for i, r := range allow.Headers {
			if applies(r.Target, r.Request, req) {
				headers[i].requests++
				hs = append(hs, i)
			}
		}
		for i := range allow.Body {
			r := &allow.Body[i]
			if applies(r.Target, r.Request, req) {
				body[i].requests++
				if !body[i].place || r.AnyLength && r.Format == "" && body[i].values == "" {
					bs = append(bs, i)
				}
			}
		}
		for i, r := range allow.Arrays {
			if applies(r.Target, r.Request, req) {
				arrays[i].requests++
				if !arrays[i].place {
					as = append(as, i)
				}
			}
		}
		if set == nil || len(hs)+len(bs)+len(as) == 0 {
			continue
		}
		rec, err := set.Record(req.ID)
		if err != nil {
			return nil, nil, envErr(err)
		}
		if rec == nil || rec.Error != "" {
			continue // no response to look at, as compare has none
		}
		for _, i := range hs {
			headers[i].seen++
			for _, h := range rec.Headers {
				if strings.EqualFold(h[0], allow.Headers[i].Name) {
					headers[i].place = true
				}
			}
		}
		if len(bs)+len(as) == 0 {
			continue
		}
		why := ""
		if isPrivate(rec) {
			if !keyChecked {
				_, kerr := p.LoadKey()
				keyChecked, keyOK = true, kerr == nil
			}
			switch {
			case rec.Body.PlainHash():
				why = "a private body named by its plain SHA-256"
			case !keyOK:
				why = "a private body, and the project key is not at hand"
			case missingPrivate(set, rec):
				why = "a private body that is not on this machine"
			}
		}
		if why != "" {
			unread[req.ID] = why
			for _, i := range bs {
				body[i].unread++
			}
			for _, i := range as {
				arrays[i].unread++
			}
			continue
		}
		for _, i := range bs {
			body[i].seen++
		}
		for _, i := range as {
			arrays[i].seen++
		}
		raw, err := set.Body(rec)
		if err != nil {
			return nil, nil, envErr(fmt.Errorf("%s: %w", req.ID, err))
		}
		reachPlaces(allow, req, bs, body, as, arrays, compare.Side{Record: rec, Body: raw})
	}

	var out []allowlist.Finding
	var unchecked, defaults []string
	add := func(ref allowlist.RuleRef, u ruleUse, what string) {
		switch {
		case u.requests == 0:
			out = append(out, allowlist.Finding{RuleRef: ref, Check: allowlist.CheckNoRequest,
				Detail: "it applies to no request of the catalog that is compared (renamed or removed requests? observed ones are never compared)"})
		case set == nil || u.place:
		case u.unread > 0 || u.seen == 0:
			// It may reach something in a body not read here, or its
			// requests have no response in the baseline yet.
			unchecked = append(unchecked, ref.String())
		case isInitDefault(allow, ref):
			// init's default for a part the baseline need not show (a
			// rate limit reached), as unused rules are not warned about.
			defaults = append(defaults, ref.String())
		default:
			out = append(out, allowlist.Finding{RuleRef: ref, Check: allowlist.CheckNoPlace,
				Detail: fmt.Sprintf("the baseline's responses to its %d %s have no %s it reaches (renamed or removed?)", u.seen, plural(u.seen, "request", "requests"), what)})
		}
	}
	for i := range allow.Headers {
		r := &allow.Headers[i]
		add(allowlist.RuleRef{Kind: "header", Rule: i + 1, Name: r.Name, Request: r.Request, Target: r.Target}, headers[i], "header of that name")
	}
	for i := range allow.Body {
		r := &allow.Body[i]
		ref := allowlist.RuleRef{Kind: "body", Rule: i + 1, Path: r.Path, Request: r.Request, Target: r.Target}
		add(ref, body[i], placeWhat(r))
		if v := body[i].values; v != "" {
			out = append(out, allowlist.Finding{RuleRef: ref, Check: allowlist.CheckAnyLength,
				Detail: fmt.Sprintf("any_length lets arrays change length; the path also reaches values such as %s, and those are still compared (no format): narrow the path to the arrays", v)})
		}
	}
	for i := range allow.Arrays {
		r := &allow.Arrays[i]
		add(allowlist.RuleRef{Kind: "array", Rule: i + 1, Path: r.Path, Request: r.Request, Target: r.Target}, arrays[i], "array")
	}
	var notes []string
	if len(unread) > 0 {
		ids := make([]string, 0, len(unread))
		for _, req := range cat.Requests {
			if _, ok := unread[req.ID]; ok {
				ids = append(ids, req.ID+" ("+unread[req.ID]+")")
			}
		}
		notes = append(notes, fmt.Sprintf("%d %s not read from the baseline: %s", len(ids), plural(len(ids), "response body", "response bodies"), listIDs(ids)))
	}
	if len(unchecked) > 0 {
		notes = append(notes, fmt.Sprintf("not checked against the baseline (bodies of their requests not read, or no response yet): %s", strings.Join(unchecked, "; ")))
	}
	if len(defaults) > 0 {
		notes = append(notes, fmt.Sprintf("not checked: rules init wrote for what the baseline need not show, such as a rate limit reached: %s", strings.Join(defaults, "; ")))
	}
	return out, notes, nil
}

// placeWhat names what a body rule reaches, for a finding.
func placeWhat(r *allowlist.BodyRule) string {
	switch {
	case r.Path == allowlist.RawPath:
		return "body that is not JSON"
	case allowlist.IsCellPath(r.Path):
		return "cell"
	case r.Format == "":
		return "array"
	case r.AnyLength:
		return "value or array"
	}
	return "value"
}

// reachPlaces marks the body rules bs and the array rules as that reach a
// place of one response to req.
func reachPlaces(allow *allowlist.List, req catalog.Request, bs []int, use []ruleUse, as []int, arrays []ruleUse, s compare.Side) {
	open := len(bs) + len(as)
	compare.Places(s, allow.ForTarget(req.Target), req.ID, func(kind compare.PlaceKind, path string) bool {
		for _, i := range as {
			if u := &arrays[i]; !u.place && kind == compare.PlaceArray && allow.Arrays[i].Reaches(path) {
				u.place = true
				open--
			}
		}
		for _, i := range bs {
			r, u := &allow.Body[i], &use[i]
			if u.place && !(r.AnyLength && r.Format == "" && u.values == "") {
				continue
			}
			var reach bool
			switch {
			case r.Path == allowlist.RawPath:
				reach = kind == compare.PlaceBody
			case allowlist.IsCellPath(r.Path):
				reach = kind == compare.PlaceCell && r.Reaches(path)
			case kind == compare.PlaceValue:
				reach = r.Reaches(path)
				if reach && r.Format == "" {
					u.values, reach = path, false
				}
			case kind == compare.PlaceArray:
				reach = r.AnyLength && r.Reaches(path)
			}
			u.place = u.place || reach
			if u.place && !(r.AnyLength && r.Format == "" && u.values == "") {
				open--
			}
		}
		return open > 0
	})
}
