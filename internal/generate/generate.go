// Package generate proposes catalog entries for the routes of the inventory:
// parameter values from the configuration, the pristine database or earlier
// responses, and one entry per template (ok, not-found, json…). Entries
// written by hand or imported are kept as they are; generated entries are
// replaced on every run, in the same order every time.
package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// SequencePrefix marks sequences that generate made.
const SequencePrefix = "gen-after-"

// Query runs a read-only query of a parameter on a state component.
type Query func(ctx context.Context, state, sql string) ([]string, error)

// Options of one run.
type Options struct {
	Project    *config.Project
	Inventory  *routes.Inventory
	Exclusions *routes.Exclusions
	Existing   *catalog.Catalog
	Query      Query
	All        bool // also routes that entries written by hand already reach
}

// Skip is a request that could not be generated.
type Skip struct {
	Route    string
	Template string
	Reason   string
}

// Result of a run.
type Result struct {
	Catalog   *catalog.Catalog
	Generated int
	Skipped   []Skip
}

// IsGenerated reports whether an entry came from generate.
func IsGenerated(r catalog.Request) bool { return r.Source != nil && r.Source.Template != "" }

// Run builds the new catalog.
func Run(ctx context.Context, o Options) (*Result, error) {
	p := o.Project
	manual := &catalog.Catalog{Version: catalog.Version}
	for _, r := range o.Existing.Requests {
		if !IsGenerated(r) {
			manual.Requests = append(manual.Requests, r)
		}
	}
	for _, s := range o.Existing.Sequences {
		if !strings.HasPrefix(s.ID, SequencePrefix) {
			manual.Sequences = append(manual.Sequences, s)
		}
	}
	templates := p.Templates
	if len(templates) == 0 {
		templates = []string{"ok"}
	}
	g := &generator{o: o, res: &Result{}, ids: map[string]bool{}, values: map[string][]string{}}
	for _, r := range manual.Requests {
		g.ids[r.ID] = true
	}
	// Each method of a route is generated unless it is excluded or (without
	// --all) a hand-written entry already reaches it.
	byHand := reached(o.Inventory, manual)
	g.skipUnit = func(route routes.Route, unit string) bool {
		return o.Exclusions.ExcludedUnit(route, unit) != nil || (!o.All && byHand[reachKey(route, unit)])
	}
	g.assignBases(o.Inventory.Routes)
	var generated []catalog.Request
	for _, route := range o.Inventory.Routes {
		reqs, err := g.route(ctx, route, templates)
		if err != nil {
			return nil, err
		}
		generated = append(generated, reqs...)
	}
	out := &catalog.Catalog{Version: catalog.Version, Requests: append(manual.Requests, generated...), Sequences: manual.Sequences}
	g.res.Generated = len(generated)
	out.Sequences = append(out.Sequences, g.sequences(out)...)
	g.res.Catalog = out
	return g.res, nil
}

// reached is the route methods the entries written by hand reach, by
// reachKey.
func reached(inv *routes.Inventory, cat *catalog.Catalog) map[string]bool {
	m := routes.NewMatcher(inv)
	out := map[string]bool{}
	for _, r := range cat.Requests {
		if route := m.Match(r); route != nil {
			out[reachKey(*route, route.UnitOf(r.Method))] = true
		}
	}
	return out
}

func reachKey(route routes.Route, unit string) string {
	return route.Target + "\x00" + route.Domain + "\x00" + route.Key() + "\x00" + unit
}

type generator struct {
	o        Options
	res      *Result
	ids      map[string]bool
	values   map[string][]string // query results, by state and SQL
	bases    map[string]string   // ID base of each route and method
	skipUnit func(routes.Route, string) bool
	from     []catalog.Request // generated requests that take a value from a response
}

func (g *generator) skip(route routes.Route, tpl, format string, a ...any) {
	g.res.Skipped = append(g.res.Skipped, Skip{Route: route.Label(), Template: tpl, Reason: fmt.Sprintf(format, a...)})
}

// methods lists the methods a route is generated with (see Route.Units).
func methods(route routes.Route) []string { return route.Units() }

// observed reports whether observe: in the configuration names the request,
// by route ("GET /blog/{slug}", "/blog/*") or by request ID.
func observed(p *config.Project, target, method, path, id string) bool {
	for _, o := range p.Observe {
		if o.Target != "" && o.Target != target {
			continue
		}
		if glob.Match(o.Match, method+" "+path) || glob.Match(o.Match, path) || glob.Match(o.Match, id) {
			return true
		}
	}
	return false
}

// queryString writes the required query parameters of a route: a value under
// params: with the same name wins over the sample from the specification.
func (g *generator) queryString(route routes.Route) string {
	if len(route.Query) == 0 {
		return ""
	}
	v := url.Values{}
	for name, sample := range route.Query {
		value := sample
		if pr := g.param(route, name); pr != nil {
			if pr.Value != "" {
				value = pr.Value
			} else if len(pr.Values) > 0 {
				value = pr.Values[0]
			}
		}
		v.Set(name, value)
	}
	return "?" + v.Encode() // Encode sorts by name
}

// binding is the value of each parameter of one request.
type binding struct {
	params map[string]string       // in catalog params
	from   map[string]catalog.From // from an earlier response
	inline map[string]string       // written into the path (rest-of-path parameters)
	omit   map[string]bool         // optional parameters without a value
	label  string                  // suffix of the ID when a parameter repeats (foreach)
}

func (g *generator) param(route routes.Route, name string) *config.Param {
	for i, pr := range g.o.Project.Params {
		if pr.Name == name && (pr.Target == "" || pr.Target == route.Target) && (pr.Route == "" || glob.Match(pr.Route, route.Path)) {
			return &g.o.Project.Params[i]
		}
	}
	return nil
}

func (g *generator) query(ctx context.Context, pr *config.Param) ([]string, error) {
	st := pr.State
	if st == "" {
		for name, c := range g.o.Project.State {
			if c.Driver != "" {
				if st != "" {
					return nil, fmt.Errorf("params %s: more than one database; set state", pr.Name)
				}
				st = name
			}
		}
		if st == "" {
			return nil, fmt.Errorf("params %s: sql needs a database under state", pr.Name)
		}
	}
	key := st + "\x00" + pr.SQL
	if v, ok := g.values[key]; ok {
		return v, nil
	}
	if g.o.Query == nil {
		return nil, fmt.Errorf("params %s: no database connection for sql", pr.Name)
	}
	v, err := g.o.Query(ctx, st, pr.SQL)
	if err != nil {
		return nil, fmt.Errorf("params %s: %w", pr.Name, err)
	}
	g.values[key] = v
	return v, nil
}

// bindings lists the parameter values of a route: one binding, or one per
// combination of values when a parameter has foreach. It returns why a
// required parameter has no value.
func (g *generator) bindings(ctx context.Context, route routes.Route) ([]binding, string, error) {
	out := []binding{{params: map[string]string{}, from: map[string]catalog.From{}, inline: map[string]string{}, omit: map[string]bool{}}}
	var missing []string
	for _, prm := range route.Params() {
		if pr := g.param(route, prm.Name); pr == nil && !prm.Optional {
			missing = append(missing, "{"+prm.Name+"}")
		}
	}
	if len(missing) > 0 {
		return nil, "no value for " + strings.Join(missing, ", "), nil
	}
	for _, prm := range route.Params() {
		pr := g.param(route, prm.Name)
		if pr == nil {
			for i := range out {
				out[i].omit[prm.Name] = true // optional, no value
			}
			continue
		}
		if pr.From != nil {
			for i := range out {
				out[i].from[prm.Name] = catalog.From{Request: pr.From.Request, Path: pr.From.Path}
			}
			continue
		}
		var values []string
		switch {
		case pr.Value != "":
			values = []string{pr.Value}
		case len(pr.Values) > 0:
			values = pr.Values
		default:
			v, err := g.query(ctx, pr)
			if err != nil {
				return nil, "", err
			}
			values = v
		}
		if len(values) == 0 {
			if prm.Optional {
				for i := range out {
					out[i].omit[prm.Name] = true
				}
				continue
			}
			return nil, fmt.Sprintf("the query for {%s} returned no rows", prm.Name), nil
		}
		if !pr.Foreach {
			values = values[:1]
		}
		var next []binding
		for _, b := range out {
			for _, v := range values {
				nb := b.clone()
				if prm.CatchAll {
					nb.inline[prm.Name] = v
				} else {
					nb.params[prm.Name] = v
				}
				if pr.Foreach {
					nb.label += "-" + slug(v)
				}
				next = append(next, nb)
			}
		}
		out = next
	}
	return out, "", nil
}

func (b binding) clone() binding {
	c := binding{params: map[string]string{}, from: map[string]catalog.From{}, inline: map[string]string{}, omit: map[string]bool{}, label: b.label}
	for k, v := range b.params {
		c.params[k] = v
	}
	for k, v := range b.from {
		c.from[k] = v
	}
	for k, v := range b.inline {
		c.inline[k] = v
	}
	for k, v := range b.omit {
		c.omit[k] = v
	}
	return c
}

var placeholder = lazy.Regexp(`\{([^}]+)\}`)

// path writes the catalog path of a route for a binding: {name} stays for
// catalog params, a rest-of-path value is written in, and an optional
// parameter without a value disappears with its slash.
func (b binding) path(route routes.Route) string {
	segs := strings.Split(route.Path, "/")
	var out []string
	for _, seg := range segs {
		drop := false
		seg = placeholder().ReplaceAllStringFunc(seg, func(m string) string {
			name := strings.TrimRight(m[1:len(m)-1], "?*")
			switch {
			case b.omit[name]:
				drop = true
				return ""
			case b.inline[name] != "":
				return b.inline[name]
			}
			return "{" + name + "}"
		})
		if drop && seg == "" {
			continue
		}
		out = append(out, seg)
	}
	p := strings.Join(out, "/")
	if p == "" {
		return "/"
	}
	return p
}

var nonSlug = lazy.Regexp(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(nonSlug().ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "x"
	}
	if len(s) > 48 {
		// A cut alone would give long sibling paths the same ID; a hash of the
		// whole text keeps each one distinct and stable.
		sum := sha256.Sum256([]byte(s))
		s = strings.TrimRight(s[:39], "-") + "-" + hex.EncodeToString(sum[:4])
	}
	return s
}

// routeBase is the ID the requests of a route and method start with; with
// several services, the service's name first (api-get-items, web-get-root).
func routeBase(route routes.Route, m string) string {
	path := placeholder().ReplaceAllString(route.Path, "$1")
	if route.Path == "/" {
		path = "root"
	}
	return slug(strings.TrimSpace(route.Target + " " + m + " " + path))
}

// assignBases gives every route its ID base. Routes whose bases would be
// equal (/a/{id} and /a/id, or one taken by an entry written by hand) all
// get a hash of their own route added, so no ID depends on which other
// routes exist or on their order.
func (g *generator) assignBases(rs []routes.Route) {
	count := map[string]int{}
	for _, r := range rs {
		for _, m := range methods(r) {
			count[routeBase(r, m)]++
		}
	}
	g.bases = map[string]string{}
	for _, r := range rs {
		for _, m := range methods(r) {
			b := routeBase(r, m)
			if count[b] > 1 || g.ids[b] {
				sum := sha256.Sum256([]byte(r.Target + " " + r.Domain + " " + r.Key()))
				b += "-" + hex.EncodeToString(sum[:4])
			}
			g.bases[r.Target+" "+r.Domain+" "+m+" "+r.Key()] = b
		}
	}
}

func (g *generator) id(base string) string {
	id := base
	for n := 2; g.ids[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	g.ids[id] = true
	return id
}

func (g *generator) storage(id string) string { return Storage(g.o.Project, id) }

// Storage of a new entry: private unless storage.public names it.
func Storage(p *config.Project, id string) string {
	for _, pat := range p.Storage.Public {
		if glob.Match(pat, id) {
			return snapshot.Public
		}
	}
	if p.Storage.Default != "" {
		return p.Storage.Default
	}
	return snapshot.Private
}

// Slug turns text into a part of a request ID.
func Slug(s string) string { return slug(s) }

func takesBody(m string) bool { return m == "POST" || m == "PUT" || m == "PATCH" }

// route generates the requests of one route, for each of its methods.
func (g *generator) route(ctx context.Context, route routes.Route, templates []string) ([]catalog.Request, error) {
	var out []catalog.Request
	for _, m := range methods(route) {
		if g.skipUnit != nil && g.skipUnit(route, m) {
			continue
		}
		reqs, err := g.routeMethod(ctx, route, m, templates)
		if err != nil {
			return nil, err
		}
		out = append(out, reqs...)
	}
	return out, nil
}

func (g *generator) routeMethod(ctx context.Context, route routes.Route, m string, templates []string) ([]catalog.Request, error) {
	query := g.queryString(route)
	base := g.bases[route.Target+" "+route.Domain+" "+m+" "+route.Key()]
	bindings, missing, err := g.bindings(ctx, route)
	if err != nil {
		return nil, err
	}
	params := route.Params()
	identity := ""
	if route.Auth {
		identity = g.o.Project.Generate.Identity
	}
	body := json.RawMessage(nil)
	if takesBody(m) {
		body = route.Body
		if len(body) == 0 {
			body = json.RawMessage(`{}`)
		}
	}
	var out []catalog.Request
	add := func(tpl, suffix string, b binding, ident string, headers [][2]string, reqBody json.RawMessage) {
		if len(reqBody) > 0 && route.BodyType != "" {
			headers = append(append([][2]string{}, headers...), [2]string{"content-type", route.BodyType})
		}
		r := catalog.Request{
			ID: g.id(base + b.label + suffix), Target: route.Target, Method: m, Path: b.path(route) + query, Identity: ident,
			Headers: headers, Body: reqBody, Mutates: catalog.MutatesFor(m),
			Source: &catalog.Source{Route: route.Key(), Template: tpl},
		}
		r.Observe = observed(g.o.Project, route.Target, m, route.Path, r.ID)
		if len(b.params) > 0 {
			r.Params = b.params
		}
		if len(b.from) > 0 {
			r.ParamsFrom = b.from
			g.from = append(g.from, r)
		}
		r.Storage = g.storage(r.ID)
		out = append(out, r)
	}
	// Values that cannot exist: every required parameter gets one.
	special := func(value string) (binding, bool) {
		b := binding{params: map[string]string{}, inline: map[string]string{}, omit: map[string]bool{}}
		any := false
		for _, prm := range params {
			switch {
			case prm.Optional:
				b.omit[prm.Name] = true
			case prm.CatchAll:
				b.inline[prm.Name], any = value, true
			default:
				b.params[prm.Name], any = value, true
			}
		}
		return b, any
	}
	for _, tpl := range templates {
		if bindings == nil && tpl != "not-found" && tpl != "invalid-param" {
			g.skip(route, tpl, "%s", missing)
			continue
		}
		switch tpl {
		case "ok":
			for _, b := range bindings {
				add(tpl, "", b, identity, nil, body)
			}
		case "not-found":
			if b, ok := special("999999999"); ok {
				add(tpl, "-not-found", b, identity, nil, body)
			}
		case "invalid-param":
			if b, ok := special("driftgate-invalid"); ok {
				add(tpl, "-invalid-param", b, identity, nil, body)
			}
		case "anonymous":
			if route.Auth && identity != "" {
				add(tpl, "-anonymous", bindings[0], "", nil, body)
			}
		case "identities":
			if !route.Auth {
				continue
			}
			names := make([]string, 0, len(g.o.Project.Identities))
			for n := range g.o.Project.Identities {
				if n != identity {
					names = append(names, n)
				}
			}
			sort.Strings(names)
			for _, n := range names {
				add(tpl, "-as-"+slug(n), bindings[0], n, nil, body)
			}
		case "json":
			for _, b := range bindings {
				add(tpl, "-json", b, identity, [][2]string{{"accept", "application/json"}}, body)
			}
		case "empty-body":
			if takesBody(m) && len(route.Body) > 0 {
				add(tpl, "-empty-body", bindings[0], identity, nil, json.RawMessage(`{}`))
			}
		}
	}
	out = withMultipart(route, out)
	if slices.Contains(templates, routes.JSONErrors) {
		out = g.jsonErrors(route, out)
	}
	return out, nil
}

// jsonErrors adds after each request of an error template the same request
// with Accept: application/json (ID suffix -json): frameworks such as
// Laravel, Symfony, Rails and Spring Boot render an error as a page or a
// redirect for a browser and as JSON for an API client, and a change of
// the JSON error body would otherwise go unseen.
func (g *generator) jsonErrors(route routes.Route, reqs []catalog.Request) []catalog.Request {
	out := make([]catalog.Request, 0, 2*len(reqs))
	for _, r := range reqs {
		out = append(out, r)
		if r.Source == nil || !slices.Contains(routes.ErrorTemplates, r.Source.Template) {
			continue
		}
		j := r
		j.ID = g.id(r.ID + "-json")
		j.Headers = [][2]string{{"accept", "application/json"}}
		for _, h := range r.Headers {
			if !strings.EqualFold(h[0], "accept") {
				j.Headers = append(j.Headers, h)
			}
		}
		j.Source = &catalog.Source{Route: r.Source.Route, Template: routes.JSONErrors}
		if r.Multipart != nil {
			mp := *r.Multipart
			j.Multipart = &mp
		}
		j.Storage = g.storage(j.ID)
		j.Observe = observed(g.o.Project, route.Target, r.Method, route.Path, j.ID)
		if len(r.ParamsFrom) > 0 {
			g.from = append(g.from, j)
		}
		out = append(out, j)
	}
	return out
}

// sequences puts every request that takes a value from a response into one
// sequence with the requests that give it, givers first, so a chain
// (create → read → update) runs in order and each request runs once. A giver
// that already belongs to a sequence written by hand cannot be moved; such
// takers are dropped and reported, as are takers of an unknown request and
// cycles.
func (g *generator) sequences(cat *catalog.Catalog) []catalog.Sequence {
	inSeq := map[string]string{}
	for _, s := range cat.Sequences {
		for _, id := range s.Requests {
			inSeq[id] = s.ID
		}
	}
	order := map[string]int{}
	for i, r := range cat.Requests {
		order[r.ID] = i
	}
	givers := map[string][]string{} // taker → its givers
	drop := map[string]bool{}
	note := func(r catalog.Request, format string, a ...any) {
		g.res.Skipped = append(g.res.Skipped, Skip{Route: r.Source.Route, Template: r.Source.Template, Reason: fmt.Sprintf(format, a...)})
		drop[r.ID] = true
	}
	for _, r := range g.from {
		names := make([]string, 0, len(r.ParamsFrom))
		for n := range r.ParamsFrom {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			src := r.ParamsFrom[n].Request
			switch _, ok := order[src]; {
			case !ok:
				note(r, "parameter %s from unknown request %s", n, src)
			case inSeq[src] != "":
				note(r, "%s is in sequence %s; add %s to it by hand", src, inSeq[src], r.ID)
			case src == r.ID:
				note(r, "parameter %s comes from the request itself", n)
			default:
				givers[r.ID] = append(givers[r.ID], src)
			}
		}
	}
	// A taker of a dropped request is dropped too.
	for changed := true; changed; {
		changed = false
		for t, gs := range givers {
			for _, src := range gs {
				if drop[src] && !drop[t] {
					drop[t], changed = true, true
					g.res.Skipped = append(g.res.Skipped, Skip{Reason: fmt.Sprintf("%s takes a value from %s, which was dropped", t, src)})
				}
			}
		}
	}
	// Group connected requests (union-find), then order each group so every
	// giver comes before its takers, ties in catalog order.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	for t, gs := range givers {
		if drop[t] {
			continue
		}
		for _, src := range gs {
			a, b := find(t), find(src)
			if a != b {
				parent[a] = b
			}
		}
	}
	groups := map[string][]string{}
	for id := range parent {
		root := find(id)
		groups[root] = append(groups[root], id)
	}
	var out []catalog.Sequence
	for _, members := range groups {
		sort.Slice(members, func(i, j int) bool { return order[members[i]] < order[members[j]] })
		placed := map[string]bool{}
		var seq []string
		for len(seq) < len(members) {
			progress := false
			for _, id := range members {
				if placed[id] {
					continue
				}
				ready := true
				for _, src := range givers[id] {
					ready = ready && placed[src]
				}
				if ready {
					seq, placed[id], progress = append(seq, id), true, true
					break // restart, so the order stays the catalog order where possible
				}
			}
			if !progress {
				for _, id := range members {
					if !placed[id] {
						drop[id] = true
						g.res.Skipped = append(g.res.Skipped, Skip{Reason: fmt.Sprintf("%s is part of a cycle of values taken from responses", id)})
					}
				}
				break
			}
		}
		if len(seq) > 1 {
			out = append(out, catalog.Sequence{ID: SequencePrefix + seq[0], Requests: seq})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(drop) > 0 {
		kept := cat.Requests[:0]
		for _, r := range cat.Requests {
			if !drop[r.ID] {
				kept = append(kept, r)
			}
		}
		g.res.Generated -= len(cat.Requests) - len(kept)
		cat.Requests = kept
	}
	return out
}
