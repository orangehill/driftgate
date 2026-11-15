// Package routes is the inventory of an application's routes
// (driftgate/routes.json), the routes excluded from checking
// (driftgate/exclusions.yaml), and coverage: which routes no catalog request
// reaches.
package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const Version = 1

// Route is one route of the application. A path names its parameters in
// braces: {id} is required, {id?} optional, {slug*} takes the rest of the
// path (one or more segments) and {slug*?} the rest or nothing.
type Route struct {
	Methods    []string          `json:"methods"` // "*" means any method
	Path       string            `json:"path"`
	Target     string            `json:"target,omitempty"` // the service, with several targets
	Domain     string            `json:"domain,omitempty"`
	Name       string            `json:"name,omitempty"`
	Action     string            `json:"action,omitempty"`
	File       string            `json:"file,omitempty"` // source file of the action, relative to the project root, with :line when known
	Middleware []string          `json:"middleware,omitempty"`
	Auth       bool              `json:"auth,omitempty"`  // needs an authenticated identity
	Body       json.RawMessage   `json:"body,omitempty"`  // sample request body
	Query      map[string]string `json:"query,omitempty"` // required query parameters with sample values
	BodySource string            `json:"body_source,omitempty"`
	BodyType   string            `json:"body_type,omitempty"` // media type of the body when not application/json
	Files      []FileField       `json:"files,omitempty"`     // file fields of a multipart/form-data body
	Source     string            `json:"source"`              // adapter that found it
	// ErrorsByAccept: the route's framework answers an error as a page or
	// a redirect for a browser and as JSON for an API client (Laravel,
	// Symfony, Rails, Spring Boot); from the configuration, not stored.
	ErrorsByAccept bool `json:"-"`
}

// Key identifies a route within its service: its methods and path.
func (r Route) Key() string { return strings.Join(r.Methods, "|") + " " + r.Path }

// Label names a route in reports: with several services, the service first.
func (r Route) Label() string {
	if r.Target != "" {
		return r.Target + ": " + r.Key()
	}
	return r.Key()
}

// Param is a parameter of a route path.
type Param struct {
	Name     string
	Optional bool
	CatchAll bool
}

var paramRE = lazy.Regexp(`\{([^}]+)\}`)

// Params lists the parameters of the path in order.
func (r Route) Params() []Param {
	var out []Param
	for _, m := range paramRE().FindAllStringSubmatch(r.Path, -1) {
		p := Param{Name: m[1]}
		if strings.HasSuffix(p.Name, "?") {
			p.Optional, p.Name = true, strings.TrimSuffix(p.Name, "?")
		}
		if strings.HasSuffix(p.Name, "*") {
			p.CatchAll, p.Name = true, strings.TrimSuffix(p.Name, "*")
		}
		out = append(out, p)
	}
	return out
}

var patterns sync.Map // path → *regexp.Regexp

// Pattern returns a regular expression that matches the request paths of the
// route. A segment may hold several parameters (/files/{name}.{ext}); an
// optional parameter that fills a whole segment makes the segment optional,
// one inside a segment (user-{group?}) only its value.
func (r Route) Pattern() *regexp.Regexp {
	if re, ok := patterns.Load(r.Path); ok {
		return re.(*regexp.Regexp)
	}
	if r.Path == "/" {
		re := regexp.MustCompile(`^/$`)
		patterns.Store(r.Path, re)
		return re
	}
	var b strings.Builder
	b.WriteString("^")
	for _, seg := range strings.Split(strings.TrimPrefix(r.Path, "/"), "/") {
		ms := paramRE().FindAllStringSubmatchIndex(seg, -1)
		if len(ms) == 1 && ms[0][0] == 0 && ms[0][1] == len(seg) && strings.HasSuffix(seg[ms[0][2]:ms[0][3]], "?") {
			b.WriteString("(?:/" + paramValue(seg[ms[0][2]:ms[0][3]]) + ")?") // the whole segment may be missing
			continue
		}
		b.WriteString("/")
		last := 0
		for _, m := range ms {
			b.WriteString(regexp.QuoteMeta(seg[last:m[0]]))
			name := seg[m[2]:m[3]]
			if strings.HasSuffix(name, "?") {
				b.WriteString("(?:" + paramValue(name) + ")?")
			} else {
				b.WriteString(paramValue(name))
			}
			last = m[1]
		}
		b.WriteString(regexp.QuoteMeta(seg[last:]))
	}
	b.WriteString("/?$")
	re := regexp.MustCompile(b.String())
	patterns.Store(r.Path, re)
	return re
}

func paramValue(name string) string {
	if strings.HasSuffix(strings.TrimSuffix(name, "?"), "*") {
		return `.+`
	}
	return `[^/]+`
}

// literal counts the fixed characters of the path.
func (r Route) literal() int { return len(paramRE().ReplaceAllString(r.Path, "")) }

// specificity ranks routes that match the same path: more fixed characters
// first (/news/latest before /news/{id}), then fewer rest-of-path parameters
// (/docs/{id} before /docs/{all*}), then fewer parameters.
func (r Route) specificity() [3]int {
	catchAll, params := 0, 0
	for _, p := range r.Params() {
		params++
		if p.CatchAll {
			catchAll++
		}
	}
	return [3]int{r.literal(), -catchAll, -params}
}

func moreSpecific(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// HasMethod reports whether the route answers the method.
func (r Route) HasMethod(method string) bool {
	for _, m := range r.Methods {
		if m == "*" || m == method {
			return true
		}
	}
	return false
}

// methodRank orders the routes of one path by their first method in the
// standard order; a route that only deletes comes last.
func methodRank(r Route) int {
	if len(r.Methods) == 1 && r.Methods[0] == "DELETE" {
		return len(standard) + 1
	}
	rank := len(standard)
	for _, m := range r.Methods {
		for i, s := range standard {
			if m == s && i < rank {
				rank = i
			}
		}
	}
	return rank
}

// Inventory is driftgate/routes.json.
type Inventory struct {
	Version int     `json:"version"`
	Routes  []Route `json:"routes"`
}

// Sort orders routes by path, then methods, so the file is deterministic.
func (inv *Inventory) Sort() {
	for i := range inv.Routes {
		sort.Strings(inv.Routes[i].Methods)
		sort.Strings(inv.Routes[i].Middleware)
	}
	// On one path, methods go in the order their requests should run:
	// reading, creating, changing, and deleting last.
	sort.SliceStable(inv.Routes, func(i, j int) bool {
		a, b := inv.Routes[i], inv.Routes[j]
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if ra, rb := methodRank(a), methodRank(b); ra != rb {
			return ra < rb
		}
		if ka, kb := a.Key(), b.Key(); ka != kb {
			return ka < kb
		}
		return a.Domain < b.Domain
	})
}

// LoadInventory reads routes.json; a missing file is an empty inventory.
func LoadInventory(path string) (*Inventory, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Inventory{Version: Version}, nil
	}
	if err != nil {
		return nil, err
	}
	var inv Inventory
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if inv.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d", path, inv.Version)
	}
	for _, r := range inv.Routes {
		if err := catalog.CheckPath(r.Path); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return &inv, nil
}

// Save writes routes.json.
func (inv *Inventory) Save(path string) error {
	inv.Version = Version
	inv.Sort()
	return snapshot.WriteJSON(path, inv)
}

// Change is a difference between two inventories.
type Change struct {
	Kind  string `json:"kind"` // route-added, route-removed, route-changed
	Route string `json:"route"`
	What  string `json:"what,omitempty"`
}

// DiffInventories lists routes that were added, removed or changed.
func DiffInventories(old, cur *Inventory) []Change {
	index := func(inv *Inventory) map[string]Route {
		m := map[string]Route{}
		for _, r := range inv.Routes {
			m[r.Target+" "+r.Domain+" "+r.Key()] = r
		}
		return m
	}
	o, c := index(old), index(cur)
	var out []Change
	for k, r := range c {
		prev, ok := o[k]
		if !ok {
			out = append(out, Change{Kind: "route-added", Route: r.Label()})
			continue
		}
		// File only says where the code is: it is filled the first time an
		// inventory is written with a version that knows it, and its line
		// moves with every edit above the action. Neither changes the route.
		prev.File, r.File = "", ""
		a, _ := json.Marshal(prev)
		b, _ := json.Marshal(r)
		if !bytes.Equal(a, b) {
			out = append(out, Change{Kind: "route-changed", Route: r.Label(), What: changedFields(prev, r)})
		}
	}
	for k, r := range o {
		if _, ok := c[k]; !ok {
			out = append(out, Change{Kind: "route-removed", Route: r.Label()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Route != out[j].Route {
			return out[i].Route < out[j].Route
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func changedFields(a, b Route) string {
	var f []string
	if a.Name != b.Name {
		f = append(f, "name")
	}
	if a.Action != b.Action {
		f = append(f, "action")
	}
	if strings.Join(a.Middleware, ",") != strings.Join(b.Middleware, ",") {
		f = append(f, "middleware")
	}
	if a.Auth != b.Auth {
		f = append(f, "auth")
	}
	if !bytes.Equal(a.Body, b.Body) {
		f = append(f, "body")
	}
	if fmt.Sprint(a.Query) != fmt.Sprint(b.Query) {
		f = append(f, "query")
	}
	if a.Source != b.Source {
		f = append(f, "source")
	}
	return strings.Join(f, ", ")
}

// Exclusion is a route that is deliberately not checked.
type Exclusion struct {
	Target string `yaml:"target,omitempty"` // only routes of this service; empty: every service
	Route  string `yaml:"route"`            // "METHOD /path" glob; without a method it matches every method
	Reason string `yaml:"reason"`
}

// Exclusions is driftgate/exclusions.yaml.
type Exclusions struct {
	Version int         `yaml:"version"`
	Routes  []Exclusion `yaml:"routes"`
	// KnownFailures acknowledge requests meant to succeed that fail in the
	// baseline: they stay in the catalog and are compared, and coverage
	// lists them instead of failing on them.
	KnownFailures []KnownFailure `yaml:"known_failures,omitempty"`
}

// KnownFailure acknowledges a known error in the baseline.
type KnownFailure struct {
	Request string `yaml:"request"` // request ID or glob
	Reason  string `yaml:"reason"`
	Ref     string `yaml:"ref,omitempty"` // ticket or link
	// Status narrows it to one failure: a status (500), a class (5xx,
	// 4xx), error (no answer) or timeout; empty: any failure.
	Status string `yaml:"status,omitempty"`
}

// onlyDocument reports whether dec, past its first document, holds nothing
// but empty documents: what follows a --- would otherwise be ignored, with
// every setting in it.
func onlyDocument(dec *yaml.Decoder) bool {
	for {
		var n yaml.Node
		if err := dec.Decode(&n); errors.Is(err, io.EOF) {
			return true
		} else if err != nil || len(n.Content) != 1 || n.Content[0].Tag != "!!null" {
			return false
		}
	}
}

// LoadExclusions reads exclusions.yaml; a missing file excludes nothing.
func LoadExclusions(path string) (*Exclusions, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Exclusions{Version: Version}, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var e Exclusions
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !onlyDocument(dec) {
		return nil, fmt.Errorf("%s: only one YAML document is read; remove the --- and what follows, or merge it into the first", path)
	}
	if e.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d", path, e.Version)
	}
	for i, x := range e.Routes {
		if x.Route == "" || strings.TrimSpace(x.Reason) == "" {
			return nil, fmt.Errorf("%s: exclusion %d needs route and reason", path, i+1)
		}
	}
	for i, k := range e.KnownFailures {
		if k.Request == "" || strings.TrimSpace(k.Reason) == "" {
			return nil, fmt.Errorf("%s: known failure %d needs request and reason", path, i+1)
		}
		if !validKnownStatus(k.Status) {
			return nil, fmt.Errorf("%s: known failure %d: status %q is not a status (500), a class (5xx), error or timeout", path, i+1, k.Status)
		}
	}
	return &e, nil
}

var standard = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// Units lists the methods a route is checked with: each one, since they
// usually have separate handlers (GET and POST of a Next.js route), except
// that PUT and PATCH of one route are the same handler (Laravel
// Route::match, resource update) and count as PUT. A route that takes any
// method counts as GET.
func (r Route) Units() []string {
	if r.HasMethod("*") {
		return []string{"GET"}
	}
	var out []string
	for _, m := range standard {
		if r.HasMethod(m) && !(m == "PATCH" && r.HasMethod("PUT")) {
			out = append(out, m)
		}
	}
	for _, m := range r.Methods {
		known := false
		for _, s := range standard {
			known = known || s == m
		}
		if !known {
			out = append(out, m)
		}
	}
	return out
}

// UnitOf is the unit a request with this method checks.
func (r Route) UnitOf(method string) string {
	if r.HasMethod("*") {
		return "GET"
	}
	if method == "PATCH" && r.HasMethod("PUT") {
		return "PUT"
	}
	return method
}

// excludes reports whether the exclusion covers the route, for any method.
func (x Exclusion) excludes(r Route) bool {
	for _, m := range r.Units() {
		if x.excludesUnit(r, m) {
			return true
		}
	}
	return false
}

// excludesUnit reports whether the exclusion covers one method of the route.
func (x Exclusion) excludesUnit(r Route, unit string) bool {
	if x.Target != "" && x.Target != r.Target {
		return false
	}
	method, path, ok := strings.Cut(x.Route, " ")
	if !ok {
		method, path = "", x.Route
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if !glob.Match(path, r.Path) {
		return false
	}
	method = strings.ToUpper(method)
	return method == "" || method == "*" || r.UnitOf(method) == unit
}

// Excluded returns the exclusion that covers the route, if any.
func (e *Exclusions) Excluded(r Route) *Exclusion {
	units := r.Units()
	var first *Exclusion
	for _, u := range units {
		x := e.ExcludedUnit(r, u)
		if x == nil {
			return nil // a method that is still checked
		}
		if first == nil {
			first = x
		}
	}
	return first
}

// ExcludedUnit returns the exclusion that covers one method of the route.
func (e *Exclusions) ExcludedUnit(r Route, unit string) *Exclusion {
	for i := range e.Routes {
		if e.Routes[i].excludesUnit(r, unit) {
			return &e.Routes[i]
		}
	}
	return nil
}

// requestPath is the path a catalog request goes to, without the query and
// with its parameters filled where values are known.
func requestPath(req catalog.Request) string {
	p := req.Path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	for name, value := range req.Params {
		p = strings.ReplaceAll(p, "{"+name+"}", value)
	}
	return p
}

// Match finds the route a request reaches: of the routes whose method and
// path match, the most specific one.
func Match(inv *Inventory, req catalog.Request) *Route {
	path := requestPath(req)
	var best *Route
	for i := range inv.Routes {
		r := &inv.Routes[i]
		if r.Target != req.Target || !r.HasMethod(req.Method) || !r.Pattern().MatchString(path) {
			continue
		}
		if best == nil || moreSpecific(r.specificity(), best.specificity()) {
			best = r
		}
	}
	return best
}

func takesBody(r Route) bool {
	for _, m := range r.Methods {
		if m == "POST" || m == "PUT" || m == "PATCH" {
			return true
		}
	}
	return false
}
