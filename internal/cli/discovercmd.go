package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/discover"
	"github.com/orangehill/driftgate/internal/generate"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/har"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/routes"
)

func discoverCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	write := fs.Bool("write", false, "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("discover")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if len(p.Discover) == 0 {
		return 0, configErr("no adapters under discover in %s", config.FileName)
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	code, _, err := discoverRoutes(ctx, p, sel, *write, out, errOut)
	return code, err
}

// discoverRoutes runs the adapters of the selected services, prints the
// routes found and how they differ from the inventory, and with write
// adopts them. It returns the exit code and the routes found, with those of
// the services not looked at as the inventory has them.
func discoverRoutes(ctx context.Context, p *config.Project, sel selection, write bool, out, errOut io.Writer) (int, *routes.Inventory, error) {
	var adapters []discover.Adapter
	for _, a := range p.Discover {
		if sel.has(p.Canonical(a.Target)) {
			adapters = append(adapters, a)
		}
	}
	inv, err := discover.Run(ctx, p.Root, adapters, fetcher(p), errOut)
	if err != nil {
		return 0, nil, envErr(err)
	}
	for i := range inv.Routes {
		inv.Routes[i].Target = p.Canonical(inv.Routes[i].Target)
	}
	inv.Sort()
	path := p.Path("routes.json")
	all, err := routes.LoadInventory(path)
	if err != nil {
		return 0, nil, configErr("%v", err)
	}
	for i := range all.Routes {
		all.Routes[i].Target = p.Canonical(all.Routes[i].Target)
	}
	old := sel.routes(all)
	// What is written keeps the routes of the services not looked at.
	whole := inv
	if sel != nil {
		whole = &routes.Inventory{Routes: append([]routes.Route{}, inv.Routes...)}
		for _, r := range all.Routes {
			if !sel.has(r.Target) {
				whole.Routes = append(whole.Routes, r)
			}
		}
		whole.Sort()
	}
	bySource := map[string]int{}
	for _, r := range inv.Routes {
		bySource[r.Source]++
	}
	fmt.Fprintln(out, routeCount(len(inv.Routes), p.Discover, bySource))
	changes := routes.DiffInventories(old, inv)
	for _, c := range changes {
		line := fmt.Sprintf("  %-13s %s", c.Kind, report.Printable(c.Route))
		if c.What != "" {
			line += "  (" + c.What + ")"
		}
		fmt.Fprintln(out, line)
	}
	switch {
	case write:
		if err := whole.Save(path); err != nil {
			return 0, nil, err
		}
		fmt.Fprintf(out, "wrote %s\n", path)
		return ExitClean, whole, nil
	case len(changes) > 0:
		fmt.Fprintf(out, "%d changes against %s; review them and run driftgate discover --write\n", len(changes), path)
		return ExitDrift, whole, nil
	}
	fmt.Fprintln(out, "routes unchanged ✓")
	return ExitClean, whole, nil
}

// fetcher reads a specification from the running application: a path is
// taken from target.base_url, a full URL must be on an allowed host.
func fetcher(p *config.Project) discover.Fetch {
	return func(ctx context.Context, raw, identity, target string) ([]byte, error) {
		t, err := p.TargetFor(target)
		if err != nil {
			return nil, err
		}
		token := ""
		if identity != "" {
			tokens, err := capture.LoadTokens(ctx, p)
			if err != nil {
				return nil, err
			}
			if token = tokens[identity]; token == "" {
				return nil, fmt.Errorf("%s: no token for identity %s", raw, identity)
			}
		}
		base, path := t.BaseURL, raw
		if u, err := url.Parse(raw); err == nil && u.IsAbs() {
			base = u.Scheme + "://" + u.Host
			path = u.RequestURI()
		}
		if base == "" {
			return nil, fmt.Errorf("%s: target.base_url is not set", raw)
		}
		allow, err := t.Allow(false)
		if err != nil {
			return nil, err
		}
		client, err := httpx.New(httpx.Options{BaseURL: base, UserAgent: t.UserAgent, Timeout: t.Timeout.Duration, FollowRedirects: true, Allow: allow})
		if err != nil {
			return nil, err
		}
		res := client.Do(ctx, catalog.Request{ID: "openapi", Method: "GET", Path: path,
			Headers: [][2]string{{"accept", "application/json, application/yaml;q=0.9, */*;q=0.5"}}}, token)
		switch {
		case res.Record.Error != "":
			return nil, fmt.Errorf("%s: %s (is the application running?)", raw, res.Record.Error)
		case res.Record.Status != 200:
			return nil, fmt.Errorf("%s: status %d", raw, res.Record.Status)
		}
		return res.Body, nil
	}
}

// loadInventory reads the route inventory with the exclusions.
func loadInventory(p *config.Project) (*routes.Inventory, *routes.Exclusions, error) {
	path := p.Path("routes.json")
	inv, err := routes.LoadInventory(path)
	if err != nil {
		return nil, nil, configErr("%v", err)
	}
	return withExclusions(p, inv, path)
}

// withExclusions checks that inv (named by source in the error) has routes,
// reads the exclusions and gives routes and exclusions their service's
// canonical name.
func withExclusions(p *config.Project, inv *routes.Inventory, source string) (*routes.Inventory, *routes.Exclusions, error) {
	if len(inv.Routes) == 0 {
		return nil, nil, configErr("%s has no routes; run driftgate discover --write first", source)
	}
	excl, err := routes.LoadExclusions(p.Path("exclusions.yaml"))
	if err != nil {
		return nil, nil, configErr("%v", err)
	}
	for i, x := range excl.Routes {
		if x.Target != "" {
			if _, err := p.TargetFor(x.Target); err != nil {
				return nil, nil, configErr("exclusions.yaml: %s: %v", x.Route, err)
			}
		}
		excl.Routes[i].Target = p.Canonical(x.Target)
	}
	for i := range inv.Routes {
		inv.Routes[i].Target = p.Canonical(inv.Routes[i].Target)
	}
	return inv, excl, nil
}

// routeCount says how many routes discover found, and how many each
// adapter gave: "13 routes (13 from laravel)", never "laravel 13", which
// reads as a version.
func routeCount(total int, adapters []discover.Adapter, bySource map[string]int) string {
	var counts []string
	seen := map[string]bool{}
	for _, a := range adapters {
		if n, ok := bySource[a.Adapter]; ok && !seen[a.Adapter] {
			seen[a.Adapter] = true
			counts = append(counts, fmt.Sprintf("%d from %s", n, a.Adapter))
		}
	}
	if len(counts) == 0 {
		return fmt.Sprintf("%d routes", total)
	}
	return fmt.Sprintf("%d routes (%s)", total, strings.Join(counts, ", "))
}

func generateCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	write := fs.Bool("write", false, "")
	check := fs.Bool("check", false, "")
	all := fs.Bool("all", false, "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("generate")
	}
	if *write && *check {
		return 0, configErr("--write and --check exclude each other")
	}
	p, whole, _, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	return generateCatalog(ctx, p, sel, whole, generateOptions{write: *write, check: *check, all: *all}, out, errOut)
}

// generateOptions are the flags of generate.
type generateOptions struct {
	write bool
	check bool
	all   bool
}

// generateCatalog generates requests for the routes of the selected
// services, prints how they differ from the catalog (whole, every service)
// and with write adopts them. With check, a difference is exit 1.
func generateCatalog(ctx context.Context, p *config.Project, sel selection, whole *catalog.Catalog, o generateOptions, out, errOut io.Writer) (int, error) {
	cat := sel.requests(whole)
	inv, excl, err := loadInventory(p)
	if err != nil {
		return 0, err
	}
	inv = sel.routes(inv)
	opts := generate.Options{Project: p, Inventory: inv, Exclusions: excl, Existing: cat, All: o.all}
	for _, pr := range p.Params {
		if pr.SQL != "" {
			q, closeQ, err := capture.Queries(ctx, p, errOut)
			if err != nil {
				return 0, failed(err)
			}
			defer closeQ()
			opts.Query = q
			break
		}
	}
	res, err := generate.Run(ctx, opts)
	if errors.Is(err, generate.ErrSetting) {
		return 0, configErr("%s: %v", rel(p.Root, p.Layout.Config), err)
	}
	if err != nil {
		return 0, envErr(err)
	}
	if err := res.Catalog.Validate(); err != nil {
		return 0, configErr("generated catalog: %v", err)
	}
	added, removed, changed := diffCatalogs(cat, res.Catalog)
	twins := ""
	if n := countTemplate(res.Catalog, routes.JSONErrors); n > 0 {
		twins = fmt.Sprintf(" (%d of them %s)", n, routes.JSONErrors)
	}
	fmt.Fprintf(out, "%d generated requests%s; against %s: %d added, %d removed, %d changed\n",
		res.Generated, twins, p.Layout.Catalog, len(added), len(removed), len(changed))
	list := func(label string, ids []string) {
		for i, id := range ids {
			if i == 30 {
				fmt.Fprintf(out, "  … and %d more\n", len(ids)-30)
				break
			}
			fmt.Fprintf(out, "  %-8s %s\n", label, id)
		}
	}
	list("added", added)
	list("removed", removed)
	list("changed", changed)
	if len(res.Skipped) > 0 {
		printSkipped(out, res.Skipped, rel(p.Root, p.Layout.Config))
	}
	for _, n := range res.Notes {
		fmt.Fprintf(out, "\nnote: %s\n", n)
	}
	printReplaced(out, res.Replaced)
	jsonErrorsNote(p, out, o.check)
	literalIDNote(out, res.Catalog, "")
	for _, u := range res.Unused {
		fmt.Fprintf(out, "note: %s in %s names no generated request (an entry written by hand says mutates and body in the catalog)\n", u, rel(p.Root, p.Layout.Config))
	}
	differs := len(added)+len(removed)+len(changed) > 0
	switch {
	case o.write:
		next := res.Catalog
		if sel != nil {
			// The other services' requests stay as they are.
			rest := sel.others(whole)
			next.Requests = append(next.Requests, rest.Requests...)
			next.Sequences = append(next.Sequences, rest.Sequences...)
		}
		if err := next.Save(p.Layout.Catalog); err != nil {
			return 0, err
		}
		fmt.Fprintf(out, "wrote %s\n", p.Layout.Catalog)
	case o.check && differs:
		return ExitDrift, nil
	}
	return ExitClean, nil
}

// printReplaced names the route methods that entries written by hand
// reach, whose generated requests they take the place of: a request the
// generator would make there (an error variant, the JSON twin) is not in
// the catalog, and generate --all adds them.
func printReplaced(out io.Writer, replaced []generate.Replaced) {
	if len(replaced) == 0 {
		return
	}
	fmt.Fprintf(out, "\nnote: entries written by hand take the place of the generated requests of %d route %s (generate --all makes those too):\n",
		len(replaced), plural(len(replaced), "method", "methods"))
	for _, r := range replaced {
		instead := "its generated requests"
		if len(r.Generated) > 0 {
			instead = strings.Join(r.Generated, ", ")
		}
		fmt.Fprintf(out, "  %s: %s instead of %s\n", report.Printable(r.Route), strings.Join(r.ByHand, ", "), instead)
	}
}

// diffCatalogs lists request IDs added, removed and changed.
func diffCatalogs(old, cur *catalog.Catalog) (added, removed, changed []string) {
	index := func(c *catalog.Catalog) map[string][]byte {
		m := map[string][]byte{}
		for _, r := range c.Requests {
			b, _ := json.Marshal(r)
			m[r.ID] = b
		}
		return m
	}
	o, c := index(old), index(cur)
	for id, b := range c {
		if prev, ok := o[id]; !ok {
			added = append(added, id)
		} else if !bytes.Equal(prev, b) {
			changed = append(changed, id)
		}
	}
	for id := range o {
		if _, ok := c[id]; !ok {
			removed = append(removed, id)
		}
	}
	oldSeq, _ := json.Marshal(old.Sequences)
	curSeq, _ := json.Marshal(cur.Sequences)
	if !bytes.Equal(oldSeq, curSeq) {
		changed = append(changed, "(sequences)")
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return
}

func coverageCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	format := fs.String("format", "text", "")
	baseline := fs.String("baseline", "", "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 || (*format != "text" && *format != "json") {
		return 0, usageErr("coverage")
	}
	p, cat, _, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	inv, excl, err := loadInventory(p)
	if err != nil {
		return 0, err
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	// The default baseline is checked when it exists; one named must exist;
	// none checks the catalog alone.
	var answers map[string]routes.Answer
	if *baseline != "none" {
		name := *baseline
		if name == "" {
			name = "local"
		}
		if answers, err = readAnswers(p, name, nil); err != nil {
			return 0, err
		}
		if answers == nil && *baseline != "" {
			return 0, configErr("baseline %s does not exist (coverage --baseline none checks the catalog alone)", name)
		}
	}
	_, code, err := checkCoverage(sel, cat, inv, excl, answers, *format, out)
	return code, err
}

// checkCoverage prints which routes of the selected services the catalog
// covers, in format text or json; uncovered routes are exit 1. answers are
// a baseline's by request ID (nil without one): a request covers its route
// only when it did not fail there.
func checkCoverage(sel selection, cat *catalog.Catalog, inv *routes.Inventory, excl *routes.Exclusions, answers map[string]routes.Answer, format string, out io.Writer) (*routes.Coverage, int, error) {
	c := coverageOf(sel, cat, inv, excl, answers)
	inv = sel.routes(inv)
	code := ExitClean
	if len(c.Uncovered) > 0 {
		code = ExitDrift
	}
	h := c.Health
	if format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return c, code, enc.Encode(map[string]any{
			"routes": len(inv.Routes), "covered": c.Covered, "excluded": c.Excluded, "uncovered": c.Uncovered,
			"only_error_cases": c.Negative, "unknown_requests": c.Unknown, "stale_exclusions": c.Stale, "routes_without_body": c.NoBody,
			"baseline_checked": answers != nil, "failed_in_baseline": c.Failed, "requests_failed_in_baseline": h.Failed,
			"known_failure_routes": c.Known, "known_failures": h.Known, "stale_known_failures": h.Stale, "broad_known_failures": h.Broad,
			"only_error_answers": c.ErrorOnly, "routes_without_invalid_body": c.NoInvalidBody,
		})
	}
	known := ""
	if len(c.Known) > 0 {
		known = fmt.Sprintf(", %d with known failures", len(c.Known))
	}
	fmt.Fprintf(out, "%d routes: %d covered, %d excluded, %d uncovered%s\n", len(inv.Routes), len(c.Covered), len(c.Excluded), len(c.Uncovered), known)
	apart := map[string]bool{}
	for _, k := range c.Negative {
		apart[k] = true
	}
	for _, f := range c.Failed {
		apart[f.Route] = true
	}
	var none []string
	for _, k := range c.Uncovered {
		if !apart[k] {
			none = append(none, k)
		}
	}
	section(out, "Uncovered routes (add a catalog entry, or an exclusion with a reason)", none)
	section(out, "Routes reached only by error cases (not-found, invalid-param…): give their parameters under params: so the ok request is generated", c.Negative)
	section(out, failedRoutesTitle, failedRouteLines(c.Failed))
	section(out, failedRequestsTitle, failureLines(h.Failed))
	section(out, "Routes reached only by requests that answered an error (written by hand; covered, but nothing shows the route works)", c.ErrorOnly)
	section(out, knownTitle, failureLines(h.Known))
	section(out, staleKnownTitle, h.Stale)
	section(out, fmt.Sprintf("Known failures that acknowledge failures on more than %d routes (a glob that broad also hides new failures: narrow it)", routes.BroadKnown), h.Broad)
	section(out, "Catalog requests that reach no route", c.Unknown)
	section(out, "Exclusions that match no route", c.Stale)
	section(out, "Routes that take a body but have no sample (from validation rules, OpenAPI or HAR)", c.NoBody)
	section(out, noInvalidBodyTitle, c.NoInvalidBody)
	if code == ExitClean {
		fmt.Fprintln(out, "\nevery route is covered or excluded ✓")
	}
	return c, code, nil
}

// noInvalidBodyTitle heads the covered routes whose validation error no
// request checks.
const noInvalidBodyTitle = "Routes whose body is validated but no request sends an invalid one (a changed validation error passes unseen): add " + routes.EmptyBody + " to templates: and run driftgate generate --write"

// coverageOf is the coverage of the selected services' routes by their
// requests; answers are a baseline's by request ID (nil without one).
func coverageOf(sel selection, cat *catalog.Catalog, inv *routes.Inventory, excl *routes.Exclusions, answers map[string]routes.Answer) *routes.Coverage {
	whole := cat
	inv, cat = sel.routes(inv), sel.requests(cat)
	if sel != nil {
		var kept []routes.Exclusion
		for _, x := range excl.Routes {
			if x.Target == "" || sel.has(x.Target) {
				kept = append(kept, x)
			}
		}
		excl.Routes = kept
		// Known failures of the other services' requests are theirs.
		var known []routes.KnownFailure
		for _, k := range excl.KnownFailures {
			if matchesAny(k.Request, cat) || !matchesAny(k.Request, whole) {
				known = append(known, k)
			}
		}
		excl.KnownFailures = known
	}
	c := routes.CheckWith(inv, cat, excl, answers)
	c.Stale = staleExclusions(excl, c.Stale)
	return c
}

// matchesAny reports whether a glob matches the ID of a request of cat.
func matchesAny(pattern string, cat *catalog.Catalog) bool {
	for _, r := range cat.Requests {
		if glob.Match(pattern, r.ID) {
			return true
		}
	}
	return false
}

func harCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("har import", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	write := fs.Bool("write", false, "")
	var targetName once
	fs.Var(&targetName, "target", "")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("har import")
	}
	p, cat, _, err := loadProject(*cfg)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(pos[0])
	if err != nil {
		return configErr("%v", err)
	}
	target, err := p.TargetFor(targetName.v)
	if err != nil {
		return configErr("%v (pass --target)", err)
	}
	ctx, stop := signalContext(os.Stderr)
	defer stop()
	opts, inv, err := candidateOptions(ctx, p, p.Canonical(targetName.v), out)
	if err != nil {
		return err
	}
	cands, notes, err := har.ReadWith(raw, target.BaseURL, opts)
	if err != nil {
		return configErr("%v", err)
	}
	b, err := har.NewBuilder(target.BaseURL, opts)
	if err != nil {
		return configErr("%v", err)
	}
	pr := newProposer(p, cat, b, inv, p.Canonical(targetName.v), "har", "har")
	var added []catalog.Request
	for i := range cands {
		c := &cands[i]
		prop := pr.propose(c)
		if prop == nil {
			continue
		}
		r := prop.request
		added = append(added, r)
		note := ""
		if c.Auth && r.Identity == "" {
			note = "  (the browser was signed in; set identity)"
		}
		fmt.Fprintf(out, "  %-6s %s → %s%s\n", c.Method, c.Path, r.ID, note)
	}
	for _, n := range notes {
		fmt.Fprintf(out, "  skipped %s: %s\n", n.Request, n.Reason)
	}
	fmt.Fprintf(out, "%d requests in the file, %d new\n", len(cands), len(added))
	if !*write || len(added) == 0 {
		return nil
	}
	cat.Requests = append(cat.Requests, added...)
	if err := cat.Validate(); err != nil {
		return configErr("%v", err)
	}
	if err := cat.Save(p.Layout.Catalog); err != nil {
		return err
	}
	fmt.Fprintf(out, "added to %s\n", p.Layout.Catalog)
	return nil
}

// printSkipped groups the requests generate could not make by reason, with
// the routes each reason holds back, and shows how to give a value.
func printSkipped(out io.Writer, skipped []generate.Skip, config string) {
	type group struct {
		reason   string
		requests int
		routes   []string
	}
	var groups []*group
	byReason := map[string]*group{}
	for _, s := range skipped {
		g := byReason[s.Reason]
		if g == nil {
			g = &group{reason: s.Reason}
			byReason[s.Reason] = g
			groups = append(groups, g)
		}
		g.requests++
		if len(g.routes) == 0 || g.routes[len(g.routes)-1] != s.Route {
			g.routes = append(g.routes, s.Route)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].requests > groups[j].requests })
	fmt.Fprintf(out, "\n%d requests could not be generated:\n", len(skipped))
	example := ""
	for _, g := range groups {
		fmt.Fprintf(out, "  %s — %d %s, routes: %s\n", g.reason, g.requests, plural(g.requests, "request", "requests"), limitList(g.routes, 4))
		if example == "" && strings.HasPrefix(g.reason, "no value for {") {
			example = strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(g.reason, ", ", 2)[0], "no value for {"), "}")
		}
	}
	if example != "" {
		fmt.Fprintf(out, `Give a value in %s, for one route or all with that name:
  params:
    - name: %s
      value: "…"               # or values: [...] with foreach: true, or sql: "SELECT … ORDER BY …"
      route: "/path/{%s}*"     # optional: only these routes
`, config, example, example)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func limitList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:n], "; ") + fmt.Sprintf("; and %d more", len(items)-n)
}
