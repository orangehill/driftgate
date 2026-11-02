package discover

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
)

// Native adapters for frameworks that list their own routes.

// spring reads Spring Boot's /actuator/mappings (MVC and WebFlux).
func spring(ctx context.Context, a Adapter, fetch Fetch) ([]routes.Route, error) {
	u := a.URL
	if u == "" {
		u = "/actuator/mappings"
	}
	if fetch == nil {
		return nil, fmt.Errorf("no target to read %s from", u)
	}
	raw, err := fetch(ctx, u, a.Identity, a.Target)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Contexts map[string]struct {
			Mappings map[string]json.RawMessage `json:"mappings"` // also servletFilters and servlets, which are lists
		} `json:"contexts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Contexts) == 0 {
		return nil, fmt.Errorf("%s is not Spring Boot's actuator mappings: %v", u, err)
	}
	type mapping struct {
		Handler string `json:"handler"`
		Details *struct {
			Conditions *struct {
				Methods  []string `json:"methods"`
				Patterns []string `json:"patterns"`
			} `json:"requestMappingConditions"`
		} `json:"details"`
	}
	names := make([]string, 0, len(doc.Contexts))
	for n := range doc.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []routes.Route
	for _, name := range names {
		c := doc.Contexts[name]
		for _, kind := range []string{"dispatcherServlets", "dispatcherHandlers"} {
			var lists map[string][]mapping
			if raw, ok := c.Mappings[kind]; ok {
				if err := json.Unmarshal(raw, &lists); err != nil {
					return nil, fmt.Errorf("%s: %s: %w", u, kind, err)
				}
			}
			keys := make([]string, 0, len(lists))
			for k := range lists {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				list := lists[k]
				for _, m := range list {
					if m.Details == nil || m.Details.Conditions == nil {
						continue // static resources and functional routes without conditions
					}
					methods := methodList(m.Details.Conditions.Methods)
					if len(methods) == 0 {
						methods = []string{"*"}
					}
					for _, p := range m.Details.Conditions.Patterns {
						out = append(out, routes.Route{Methods: methods, Path: springPath(p), Action: m.Handler})
					}
				}
			}
		}
	}
	return joinHandlers(out), nil
}

// joinHandlers makes one route of routes with the same methods and path
// (Spring's /error has an HTML and a JSON handler), naming every handler,
// so the result does not depend on the order the framework lists them in.
func joinHandlers(rs []routes.Route) []routes.Route {
	var out []routes.Route
	actions := map[int][]string{}
	index := map[string]int{}
	for _, r := range rs {
		k := r.Domain + " " + r.Key()
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, r)
		}
		actions[i] = append(actions[i], r.Action)
	}
	for i := range out {
		sort.Strings(actions[i])
		out[i].Action = strings.Join(actions[i], " | ")
	}
	return out
}

// springPath writes Spring's path patterns the DriftGate way: {*rest} and
// /** take the rest of the path (or nothing), {id:\d{3}} is {id}, and a
// segment of * is one parameter.
func springPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '{' {
			b.WriteByte(p[i])
			continue
		}
		depth, j := 0, i
		for ; j < len(p); j++ {
			if p[j] == '{' {
				depth++
			} else if p[j] == '}' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		inner := p[i+1 : min(j, len(p))]
		name, _, _ := strings.Cut(inner, ":")
		if strings.HasPrefix(name, "*") {
			b.WriteString("{" + name[1:] + "*?}")
		} else {
			b.WriteString("{" + name + "}")
		}
		i = j
	}
	p = strings.ReplaceAll(b.String(), "/**", "/{rest*?}")
	segs := strings.Split(p, "/")
	n := 0
	for k, s := range segs {
		if s == "*" {
			n++
			segs[k] = fmt.Sprintf("{segment%d}", n)
		}
	}
	return "/" + strings.TrimPrefix(strings.Join(segs, "/"), "/")
}

// symfony reads `bin/console debug:router --format=json`.
func symfony(ctx context.Context, root string, a Adapter, log io.Writer) ([]routes.Route, error) {
	cmd := a.Command
	if len(cmd) == 0 {
		cmd = []string{"php", "bin/console", "debug:router", "--format=json"}
	}
	out, err := runCommand(ctx, root, cmd, nil, log)
	if err != nil {
		return nil, err
	}
	var list map[string]struct {
		Path   string `json:"path"`
		Host   string `json:"host"`
		Method string `json:"method"`
		// Objects, or the text "NO CUSTOM" / "NONE" when empty.
		Defaults     any `json:"defaults"`
		Requirements any `json:"requirements"`
	}
	js := jsonFrom(out, '{')
	if js == nil {
		return nil, fmt.Errorf("debug:router printed no JSON")
	}
	if err := json.NewDecoder(bytes.NewReader(js)).Decode(&list); err != nil {
		return nil, fmt.Errorf("debug:router JSON: %w", err)
	}
	names := make([]string, 0, len(list))
	for n := range list {
		names = append(names, n)
	}
	sort.Strings(names)
	psr4 := loadPSR4(root)
	var rs []routes.Route
	for _, name := range names {
		r := list[name]
		defaults, _ := r.Defaults.(map[string]any)
		reqs, _ := r.Requirements.(map[string]any)
		path := symfonyPath(r.Path, defaults, reqs)
		methods := []string{"*"}
		if r.Method != "" && r.Method != "ANY" {
			methods = methodList([]string{r.Method})
		}
		host := r.Host
		if host == "ANY" {
			host = ""
		}
		action, _ := defaults["_controller"].(string)
		rs = append(rs, routes.Route{Methods: methods, Path: path, Domain: host, Name: name, Action: action, File: psr4.file(root, action)})
	}
	return rs, nil
}

// psr4Map is the PSR-4 autoload map of composer.json: namespace prefix →
// folders.
type psr4Map map[string][]string

func loadPSR4(root string) psr4Map {
	raw, err := os.ReadFile(filepath.Join(root, "composer.json"))
	if err != nil {
		return nil
	}
	var c struct {
		Autoload struct {
			PSR4 map[string]any `json:"psr-4"`
		} `json:"autoload"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	m := psr4Map{}
	for prefix, dirs := range c.Autoload.PSR4 {
		switch d := dirs.(type) {
		case string:
			m[prefix] = []string{d}
		case []any:
			for _, x := range d {
				if s, ok := x.(string); ok {
					m[prefix] = append(m[prefix], s)
				}
			}
		}
	}
	return m
}

// file finds the source of a Symfony controller (Class::method or an
// invokable Class) through the PSR-4 map, with the line of the method; ""
// when the class is not in the project's own code.
func (m psr4Map) file(root, action string) string {
	class, method, ok := strings.Cut(action, "::")
	if !ok {
		method = "__invoke"
	}
	best := ""
	for prefix := range m {
		if strings.HasPrefix(class, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return ""
	}
	for _, dir := range m[best] {
		rel := path.Join(filepath.ToSlash(dir), strings.ReplaceAll(strings.TrimPrefix(class, best), `\`, "/")+".php")
		if f := sourceLine(root, rel, regexp.MustCompile(`function\s+`+regexp.QuoteMeta(method)+`\s*\(`)); f != "" {
			return f
		}
	}
	return ""
}

// sourceLine returns rel with the line of the first match of def in it
// ("app/x.rb:12"), rel alone when nothing matches, or "" when the file does
// not exist.
func sourceLine(root, rel string, def *regexp.Regexp) string {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	if loc := def.FindIndex(raw); loc != nil {
		return fmt.Sprintf("%s:%d", rel, bytes.Count(raw[:loc[0]], []byte("\n"))+1)
	}
	return rel
}

var paramRE = lazy.Regexp(`\{[^}]+\}`)

// symfonyPath marks parameters the Symfony way: a parameter with a default
// (null too) is optional only at the end of the path, where nothing but other
// optional parameters and separators follow it; a requirement of .+ or .*
// takes the rest of the path.
func symfonyPath(path string, defaults, reqs map[string]any) string {
	ms := paramRE().FindAllStringIndex(path, -1)
	optional := make([]bool, len(ms))
	tailOK := true
	for i := len(ms) - 1; i >= 0; i-- {
		after := len(path)
		if i+1 < len(ms) {
			after = ms[i+1][0]
		}
		if strings.Trim(path[ms[i][1]:after], "/.-") != "" {
			tailOK = false // fixed text follows
		}
		name := path[ms[i][0]+1 : ms[i][1]-1]
		_, hasDefault := defaults[name]
		optional[i] = tailOK && hasDefault
		tailOK = tailOK && optional[i]
	}
	var b strings.Builder
	last := 0
	for i, m := range ms {
		b.WriteString(path[last:m[0]])
		name := path[m[0]+1 : m[1]-1]
		suffix := ""
		if req, _ := reqs[name].(string); req == ".+" || req == ".*" {
			suffix = "*"
		}
		if optional[i] {
			suffix += "?"
		}
		b.WriteString("{" + name + suffix + "}")
		last = m[1]
	}
	b.WriteString(path[last:])
	return b.String()
}

// jsonFrom finds JSON that starts a line with the given character, skipping
// notices printed before it.
func jsonFrom(out []byte, open byte) []byte {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) > 0 && trimmed[0] == open {
		return trimmed
	}
	if i := bytes.Index(out, []byte{'\n', open}); i >= 0 {
		return out[i+1:]
	}
	return nil
}

var (
	railsVerb    = lazy.Regexp(`^[A-Z]+(\|[A-Z]+)*$`)
	railsSymbol  = lazy.Regexp(`:(\w+)`)
	railsSplat   = lazy.Regexp(`\*(\w+)`)
	railsSection = lazy.Regexp(`^Routes for (\S+):$`)
	railsAddress = lazy.Regexp(`0x[0-9a-f]+`)
)

// railsPaths turns a Rails route pattern into DriftGate paths. (.:format)
// is dropped; any other optional group gives two paths, without it and with
// it (/posts(/page/:page) → /posts and /posts/page/{page}); :id is {id} and
// *path takes the rest of the path.
func railsPaths(p string) []string {
	p = strings.ReplaceAll(p, "(.:format)", "")
	variants := []string{p}
	for round := 0; round < 3; round++ {
		var next []string
		changed := false
		for _, v := range variants {
			open := strings.LastIndex(v, "(")
			if open < 0 {
				next = append(next, v)
				continue
			}
			close := strings.Index(v[open:], ")")
			if close < 0 {
				next = append(next, v)
				continue
			}
			close += open
			changed = true
			next = append(next, v[:open]+v[close+1:], v[:open]+v[open+1:close]+v[close+1:])
		}
		variants = next
		if !changed {
			break
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range variants {
		v = railsSplat().ReplaceAllString(v, "{${1}*}")
		v = railsSymbol().ReplaceAllString(v, "{${1}}")
		if v == "" {
			v = "/"
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// rails reads the table of `bin/rails routes`. Routes of the same path and
// action (PATCH and PUT of update) become one route with all methods. Routes
// of a mounted engine, listed in its own "Routes for X:" section, get the
// path the engine is mounted at; the mount row itself is then left out.
func rails(ctx context.Context, root string, a Adapter, log io.Writer) ([]routes.Route, error) {
	cmd := a.Command
	if len(cmd) == 0 {
		cmd = []string{"bin/rails", "routes"}
	}
	out, err := runCommand(ctx, root, cmd, nil, log)
	if err != nil {
		return nil, err
	}
	type entry struct {
		methods []string
		path    string
		name    string
		action  string
		mount   bool
	}
	var order []string
	byKey := map[string]*entry{}
	mounts := map[string]string{} // engine → path it is mounted at
	sections := map[string]bool{}
	prefix, lastName, lastRaw := "", "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if m := railsSection().FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			prefix = strings.TrimSuffix(mounts[m[1]], "/")
			sections[m[1]] = true
			lastName, lastRaw = "", ""
			continue
		}
		fields := strings.Fields(line)
		pi := -1
		for i, f := range fields {
			if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "(/") {
				pi = i
				break
			}
		}
		if pi < 0 || (len(fields) > 0 && fields[0] == "Prefix") {
			continue
		}
		verb, name := "", ""
		switch {
		case pi >= 1 && railsVerb().MatchString(fields[pi-1]):
			verb = fields[pi-1]
			if pi >= 2 {
				name = fields[pi-2]
			}
		case pi == 1:
			name = fields[0] // a mounted engine or app has no verb
		}
		raw := fields[pi]
		// A row without a name belongs to the name above only on the same path.
		if name == "" && raw == lastRaw {
			name = lastName
		}
		lastName, lastRaw = name, raw
		action := railsAddress().ReplaceAllString(strings.Join(fields[pi+1:], " "), "0x…")
		if verb == "" && len(fields) > pi+1 {
			mounts[fields[pi+1]] = prefix + railsPaths(raw)[0]
		}
		for _, path := range railsPaths(raw) {
			path = prefix + path
			if prefix != "" && strings.HasSuffix(path, "/") && path != "/" {
				path = strings.TrimSuffix(path, "/")
			}
			key := path + " " + action
			e := byKey[key]
			if e == nil {
				e = &entry{path: path, name: name, action: action, mount: verb == ""}
				byKey[key] = e
				order = append(order, key)
			}
			if verb == "" {
				e.methods = append(e.methods, "*")
			} else {
				e.methods = append(e.methods, strings.Split(verb, "|")...)
			}
		}
	}
	var rs []routes.Route
	for _, k := range order {
		e := byKey[k]
		if e.mount && sections[e.action] {
			continue // its routes are listed in the engine's section
		}
		methods := methodList(e.methods)
		if contains(methods, "*") {
			methods = []string{"*"}
		}
		rs = append(rs, routes.Route{Methods: methods, Path: e.path, Name: e.name, Action: e.action, File: railsFile(root, e.action)})
	}
	return rs, nil
}

var railsAction = lazy.Regexp(`^([a-z0-9_/]+)#([A-Za-z0-9_]+[!?]?)$`)

// railsFile finds the controller of controller#action under
// app/controllers, with the line of the action's def; "" for actions the
// application does not define itself (engines, Rails' own controllers).
func railsFile(root, action string) string {
	m := railsAction().FindStringSubmatch(action)
	if m == nil {
		return ""
	}
	rel := "app/controllers/" + m[1] + "_controller.rb"
	return sourceLine(root, rel, regexp.MustCompile(`(?m)^[ \t]*def[ \t]+`+regexp.QuoteMeta(m[2])+`(?:[^\w!?]|$)`))
}

//go:embed django_routes.py
var djangoScript []byte

// django reads the URL patterns through `python manage.py shell` and the
// script above. Methods come from class-based views and REST framework view
// sets; a function view takes any method.
func django(ctx context.Context, root string, a Adapter, log io.Writer) ([]routes.Route, error) {
	cmd := a.Command
	if len(cmd) == 0 {
		// The project's own environment if it has one, else python3.
		python := "python3"
		for _, venv := range []string{".venv/bin/python", "venv/bin/python", ".venv/Scripts/python.exe"} {
			if _, err := os.Stat(filepath.Join(root, venv)); err == nil {
				python = filepath.Join(root, venv)
				break
			}
		}
		cmd = []string{python, "manage.py", "shell"}
	}
	out, err := runCommand(ctx, root, cmd, djangoScript, log)
	if err != nil {
		return nil, err
	}
	const marker = "__DRIFTGATE_ROUTES__"
	i := bytes.LastIndex(out, []byte(marker))
	if i < 0 {
		return nil, fmt.Errorf("the route script printed no result")
	}
	var list []struct {
		Methods []string `json:"methods"`
		Path    string   `json:"path"`
		Name    string   `json:"name"`
		Action  string   `json:"action"`
		File    string   `json:"file"`
	}
	if err := json.NewDecoder(bytes.NewReader(out[i+len(marker):])).Decode(&list); err != nil {
		return nil, fmt.Errorf("route script: %w", err)
	}
	var rs []routes.Route
	for _, e := range list {
		rs = append(rs, routes.Route{Methods: methodList(e.Methods), Path: e.Path, Name: e.Name, Action: e.Action, File: e.File})
	}
	return rs, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
