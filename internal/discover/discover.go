// Package discover finds the routes of an application: from Laravel's route
// list, an OpenAPI specification, the files of a Next.js or Nuxt project, or
// any command that prints them as JSON.
package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/secret"
)

// Adapter is one entry under discover: in driftgate.yaml.
type Adapter struct {
	Adapter  string   `yaml:"adapter"`             // laravel, openapi, nextjs, nuxt, spring, symfony, rails, django, command
	Command  []string `yaml:"command,omitempty"`   // laravel: route:list; command: prints routes as JSON
	PHP      []string `yaml:"php,omitempty"`       // laravel: runs PHP in the application, for validation rules
	Auth     []string `yaml:"auth,omitempty"`      // laravel: the application's own middleware that signs a user in, by class
	File     string   `yaml:"file,omitempty"`      // openapi: specification file (JSON or YAML)
	URL      string   `yaml:"url,omitempty"`       // openapi: specification the running application serves (a path on the target or a full URL)
	Identity string   `yaml:"identity,omitempty"`  // openapi url, spring: read as this identity (some applications describe only what the caller may use)
	Target   string   `yaml:"target,omitempty"`    // the service these routes belong to, with several targets
	Dir      string   `yaml:"dir,omitempty"`       // folder of the service: commands run there, files are found there (default: the project root)
	BasePath string   `yaml:"base_path,omitempty"` // prefix of every path (Next.js basePath, an API prefix)
}

// Validate checks that the adapter has what it needs.
func (a Adapter) Validate() error {
	switch a.Adapter {
	case "laravel", "command":
		if len(a.Command) == 0 {
			return fmt.Errorf("adapter %s needs command", a.Adapter)
		}
	case "openapi":
		if (a.File == "") == (a.URL == "") {
			return fmt.Errorf("adapter openapi needs file or url (one of them)")
		}
	case "nextjs", "nuxt", "spring", "symfony", "rails", "django":
	default:
		return fmt.Errorf("unknown adapter %q (laravel, openapi, nextjs, nuxt, spring, symfony, rails, django, command)", a.Adapter)
	}
	// Only the laravel adapter reads the middleware of a route.
	if len(a.Auth) > 0 && a.Adapter != "laravel" {
		return fmt.Errorf("auth is for adapter laravel (adapter %s)", a.Adapter)
	}
	for _, n := range a.Auth {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("auth has an empty middleware name")
		}
	}
	if a.BasePath != "" && (!strings.HasPrefix(a.BasePath, "/") || strings.HasSuffix(a.BasePath, "/")) {
		return fmt.Errorf("base_path must start with / and not end with one, got %q", a.BasePath)
	}
	return nil
}

// Fetch reads a URL from the running application (only allowed hosts), as
// the identity when one is given.
type Fetch func(ctx context.Context, url, identity, target string) ([]byte, error)

// Run runs every adapter from the project root and returns the inventory.
func Run(ctx context.Context, root string, adapters []Adapter, fetch Fetch, log io.Writer) (*routes.Inventory, error) {
	if log == nil {
		log = io.Discard
	}
	inv := &routes.Inventory{Version: routes.Version}
	for _, a := range adapters {
		var found []routes.Route
		var err error
		switch a.Adapter {
		case "laravel":
			found, err = laravel(ctx, resolve(root, a.Dir), a, log)
		case "command":
			found, err = command(ctx, resolve(root, a.Dir), a, log)
		case "openapi":
			var raw []byte
			if a.URL != "" {
				if fetch == nil {
					return nil, fmt.Errorf("adapter openapi: no target to read %s from", a.URL)
				}
				raw, err = fetch(ctx, a.URL, a.Identity, a.Target)
			} else {
				raw, err = os.ReadFile(resolve(resolve(root, a.Dir), a.File))
			}
			if err == nil {
				found, err = openAPI(raw)
			}
		case "nextjs":
			found, err = nextJS(resolve(root, a.Dir))
		case "nuxt":
			found, err = nuxt(resolve(root, a.Dir))
		case "spring":
			found, err = spring(ctx, a, fetch)
		case "symfony":
			found, err = symfony(ctx, resolve(root, a.Dir), a, log)
		case "rails":
			found, err = rails(ctx, resolve(root, a.Dir), a, log)
		case "django":
			found, err = django(ctx, resolve(root, a.Dir), a, log)
		}
		if err != nil {
			return nil, fmt.Errorf("adapter %s: %w", a.Adapter, err)
		}
		for i := range found {
			found[i].Source = a.Adapter
			found[i].Target = a.Target
			found[i].File = fromRoot(root, a.Dir, found[i].File)
			if a.BasePath != "" {
				found[i].Path = strings.TrimSuffix(a.BasePath+found[i].Path, "/")
				if found[i].Path == "" {
					found[i].Path = "/"
				}
			}
			if err := catalog.CheckPath(found[i].Path); err != nil {
				return nil, fmt.Errorf("adapter %s: route %w", a.Adapter, err)
			}
		}
		inv.Routes = append(inv.Routes, found...)
	}
	merge(inv)
	inv.Sort()
	return inv, nil
}

// fromRoot makes a file an adapter found relative to its folder (dir)
// relative to the project root, slash-separated. A file outside the project
// is left out: an absolute path differs from machine to machine.
func fromRoot(root, dir, file string) string {
	// Only a path inside the project: an absolute one differs per machine,
	// and either could name a file outside the repository.
	name := file
	if i := strings.LastIndexByte(file, ':'); i > 0 {
		if _, err := strconv.Atoi(file[i+1:]); err == nil {
			name = file[:i] // without :line
		}
	}
	name = filepath.ToSlash(name)
	if file == "" || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return ""
	}
	if dir == "" {
		return file
	}
	rel, err := filepath.Rel(root, resolve(root, dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return path.Join(filepath.ToSlash(rel), file)
}

func resolve(root, path string) string {
	if path == "" {
		return root
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// merge drops routes another adapter already found and lends a sample body
// and a source file to a route of the same path and method that has none. Bodies are lent
// first, so it does not matter which adapter is listed first.
func merge(inv *routes.Inventory) {
	all := inv.Routes
	for i := range all {
		if all[i].File == "" {
			for _, other := range all {
				if other.File != "" && other.Target == all[i].Target && other.Path == all[i].Path && other.Domain == all[i].Domain && sameMethod(all[i], other) {
					all[i].File = other.File
					break
				}
			}
		}
		if len(all[i].Body) > 0 {
			continue
		}
		for _, other := range all {
			if len(other.Body) > 0 && other.Target == all[i].Target && other.Path == all[i].Path && other.Domain == all[i].Domain && sameMethod(all[i], other) {
				all[i].Body, all[i].BodySource, all[i].BodyType = other.Body, other.BodySource, other.BodyType
				break
			}
		}
	}
	seen := map[string]bool{}
	var out []routes.Route
	for _, r := range all {
		k := r.Target + " " + r.Domain + " " + r.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	inv.Routes = out
}

func sameMethod(a, b routes.Route) bool {
	for _, m := range a.Methods {
		if m != "*" && b.HasMethod(m) {
			return true
		}
	}
	return false
}

func runCommand(ctx context.Context, root string, argv []string, stdin []byte, log io.Writer) ([]byte, error) {
	cmd := procx.Command(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = secret.Environ()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, log
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := procx.Run(cmd); err != nil {
		return nil, fmt.Errorf("%s: %w", procx.Line(argv), err)
	}
	return out.Bytes(), nil
}

// methodList turns "GET|HEAD" or ["PUT","PATCH"] into sorted methods
// without HEAD (it answers like GET); a route that is only HEAD keeps it.
func methodList(ms []string) []string {
	var out []string
	for _, m := range ms {
		for _, part := range strings.Split(m, "|") {
			part = strings.ToUpper(strings.TrimSpace(part))
			if part != "" && part != "HEAD" {
				out = append(out, part)
			}
		}
	}
	if len(out) == 0 && len(ms) > 0 {
		out = []string{"HEAD"}
	}
	sort.Strings(out)
	return dedupe(out)
}

// needsAuth guesses from Laravel middleware names whether a route needs an
// authenticated user (auth, auth:api, jwt.auth, auth.basic, auth:sanctum).
// route:list resolves aliases to classes, so guest is listed as
// Illuminate\Auth\Middleware\RedirectIfAuthenticated: it is not one.
func needsAuth(middleware []string) bool {
	for _, m := range middleware {
		l := strings.ToLower(m)
		if strings.Contains(l, "auth") && !strings.Contains(l, "guest") && !strings.Contains(l, "redirectifauthenticated") {
			return true
		}
	}
	return false
}

// MiddlewareName splits a middleware as route:list lists it into its name
// without the parameters (App\Http\Middleware\CheckRole:admin gives
// App\Http\Middleware\CheckRole) and the class without the namespace
// (CheckRole).
func MiddlewareName(m string) (full, base string) {
	full, _, _ = strings.Cut(m, ":")
	return full, full[strings.LastIndex(full, `\`)+1:]
}

// named reports whether middleware m is one of names: by its class, with or
// without the namespace, or its name as listed. It never matches a part of a
// name, so api does not match every route of the api group. route:list
// --json lists aliases and groups resolved to classes.
func named(names []string, m string) bool {
	full, base := MiddlewareName(m)
	for _, n := range names {
		if strings.EqualFold(n, m) || strings.EqualFold(n, full) || strings.EqualFold(n, base) {
			return true
		}
	}
	return false
}

// UnmatchedAuth lists the names of auth that match no middleware of rs, the
// routes of that adapter: an alias or a group name, which route:list --json
// lists as classes, or a misspelt class.
func UnmatchedAuth(auth []string, rs []routes.Route) []string {
	var out []string
	for _, n := range auth {
		if !slices.ContainsFunc(rs, func(r routes.Route) bool {
			return slices.ContainsFunc(r.Middleware, func(m string) bool { return named([]string{n}, m) })
		}) {
			out = append(out, n)
		}
	}
	return out
}

// withAuth marks the routes that carry one of the adapter's auth middleware.
func withAuth(rs []routes.Route, auth []string) {
	if len(auth) == 0 {
		return
	}
	for i := range rs {
		rs[i].Auth = rs[i].Auth || slices.ContainsFunc(rs[i].Middleware, func(m string) bool { return named(auth, m) })
	}
}

// authLike matches middleware names that look like they sign a user in
// (VerifyJwtToken, ApiGuard); notAuth those that only carry a token: the
// CSRF check and Passport's CreateFreshApiToken, which sets a cookie for
// the user already signed in.
var (
	authLike = regexp.MustCompile(`(?i)token|jwt|guard|bearer|api.?key`)
	notAuth  = regexp.MustCompile(`(?i)csrf|createfreshapitoken`)
)

// SuspectAuth lists, once each and sorted, the middleware of routes that do
// not need authentication whose name looks like it signs a user in: an
// application's own middleware that needsAuth does not know and the
// adapter's auth does not name.
func SuspectAuth(rs []routes.Route) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if r.Auth || r.Source != "laravel" {
			continue
		}
		for _, m := range r.Middleware {
			full, _ := MiddlewareName(m)
			if !seen[full] && authLike.MatchString(full) && !notAuth.MatchString(full) {
				seen[full] = true
				out = append(out, full)
			}
		}
	}
	sort.Strings(out)
	return out
}

// command runs a command that prints routes as JSON: a list of objects with
// method (or methods), path and optionally name, auth, body and file (the
// source of the handler, relative to the command's folder).
func command(ctx context.Context, root string, a Adapter, log io.Writer) ([]routes.Route, error) {
	out, err := runCommand(ctx, root, a.Command, nil, log)
	if err != nil {
		return nil, err
	}
	return ParseCommandOutput(out)
}

// ParseCommandOutput reads the output of the command adapter: a JSON list of
// routes, each with method or methods and path (a leading / is added).
func ParseCommandOutput(out []byte) ([]routes.Route, error) {
	var list []struct {
		Method  string          `json:"method"`
		Methods []string        `json:"methods"`
		Path    string          `json:"path"`
		Name    string          `json:"name"`
		Auth    bool            `json:"auth"`
		Body    json.RawMessage `json:"body"`
		File    string          `json:"file"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("output must be a JSON list of routes: %w", err)
	}
	var rs []routes.Route
	for _, e := range list {
		ms := e.Methods
		if e.Method != "" {
			ms = append(ms, e.Method)
		}
		if e.Path == "" || len(ms) == 0 {
			return nil, fmt.Errorf("every route needs method and path")
		}
		r := routes.Route{Methods: methodList(ms), Path: "/" + strings.TrimPrefix(e.Path, "/"), Name: e.Name, Auth: e.Auth, File: e.File}
		if len(e.Body) > 0 {
			r.Body, r.BodySource = e.Body, "command"
		}
		rs = append(rs, r)
	}
	return rs, nil
}
