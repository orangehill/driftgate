package discover

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/routes"
)

func keys(rs []routes.Route) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Key())
	}
	return out
}

func find(t *testing.T, rs []routes.Route, key string) routes.Route {
	t.Helper()
	for _, r := range rs {
		if r.Key() == key {
			return r
		}
	}
	t.Fatalf("route %s not found in %v", key, keys(rs))
	return routes.Route{}
}

func run(t *testing.T, adapters ...Adapter) []routes.Route {
	t.Helper()
	inv, err := Run(context.Background(), ".", adapters, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inv.Routes
}

func TestLaravelJSON(t *testing.T) {
	rs := run(t, Adapter{Adapter: "laravel", Command: []string{"cat", "testdata/laravel13.json"}})
	r := find(t, rs, "POST /api/faq")
	if !r.Auth || r.Name != "faq.store" || r.Source != "laravel" {
		t.Errorf("POST /api/faq: %+v", r)
	}
	find(t, rs, "PATCH|PUT /api/faq/{id}")
	find(t, rs, "GET /api/news/{slug?}")
	if r := find(t, rs, "GET /api/faq"); r.Auth {
		t.Error("GET /api/faq marked as needing auth")
	}
}

func TestLaravelTable(t *testing.T) {
	rs := run(t, Adapter{Adapter: "laravel", Command: []string{"cat", "testdata/laravel55.txt"}})
	if got := strings.Join(keys(rs), "; "); got != "GET /; POST /api/faq; PATCH|PUT /api/faq/{id}; GET /api/news; GET /api/news/{id}; GET /api/search/{term?}" {
		t.Fatalf("routes: %s", got)
	}
	r := find(t, rs, "POST /api/faq")
	if !r.Auth || strings.Join(r.Middleware, ",") != "api,jwt.auth" {
		t.Errorf("continued middleware: %+v", r)
	}
}

func TestSampleFromRules(t *testing.T) {
	rules := map[string][]string{
		"question":     {"required", "string", "min:12", "max:200"},
		"email":        {"required", "email"},
		"category":     {"required", `in:"general","billing"`},
		"priority":     {"required", "integer", "between:3,5"},
		"published_at": {"required", "date_format:Y-m-d H:i:s"},
		"tags":         {"required", "array"},
		"tags.*":       {"required", "string"},
		"address.city": {"required", "string"},
		"password":     {"required", "string", "min:8", "confirmed"},
		"note":         {"nullable", "string"},
		"avatar":       {"required", "image"},
		"code":         {"required", "string", "size:4"},
	}
	b, _ := json.Marshal(sampleFromRules(rules))
	want := `{"address":{"city":"driftgate"},"category":"general","code":"drif","email":"driftgate@example.test","password":"driftgate","password_confirmation":"driftgate","priority":3,"published_at":"2026-01-01 00:00:00","question":"driftgate-dr","tags":["driftgate"]}`
	if string(b) != want {
		t.Fatalf("body\n got %s\nwant %s", b, want)
	}
}

func TestOpenAPI(t *testing.T) {
	rs := run(t, Adapter{Adapter: "openapi", File: "testdata/openapi.yaml"})
	if got := strings.Join(keys(rs), "; "); got != "POST /v1/faq; POST /v1/login; GET /v1/news; GET /v1/news/{id}; DELETE /v1/news/{id}" {
		t.Fatalf("routes: %s", got)
	}
	if r := find(t, rs, "GET /v1/news"); r.Auth {
		t.Error("security: [] should mean no auth")
	}
	if r := find(t, rs, "DELETE /v1/news/{id}"); !r.Auth {
		t.Error("global security not applied")
	}
	faq := find(t, rs, "POST /v1/faq")
	if string(faq.Body) != `{"author":{"id":1},"kind":"general","question":"driftgate","tags":["driftgate@example.test"]}` {
		t.Errorf("schema body %s", faq.Body)
	}
	if login := find(t, rs, "POST /v1/login"); string(login.Body) != `{"count":3,"email":"a@example.test","password":"secret","since":"2026-01-02"}` {
		t.Errorf("example body %s", login.Body)
	}
}

func TestNextJS(t *testing.T) {
	rs := run(t, Adapter{Adapter: "nextjs", Dir: "testdata/next", BasePath: "/web"})
	want := "GET /web; GET /web/about; DELETE|PATCH /web/api/users; GET|POST /web/api/users/{id}; GET /web/blog/{slug}; GET /web/docs/{path*?}; * /web/api/legacy; GET /web/old; GET /web/index; GET /web/sitemap.xml; GET /web/robots.txt"
	got := keys(rs)
	for _, w := range strings.Split(want, "; ") {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %s in %v", w, got)
		}
	}
	if len(got) != 11 { // pages/404 is an error page, not a route
		t.Errorf("routes: %v", got)
	}
}

func TestNuxt(t *testing.T) {
	rs := run(t, Adapter{Adapter: "nuxt", Dir: "testdata/nuxt"})
	got := strings.Join(keys(rs), "; ")
	want := "GET /; GET /login; GET /user-{group?}; GET /users/{id}; GET /{slug*}; * /api/users/{id}; GET /api/users; POST /api/users; * /sitemap.xml; * /api/proxy/{_*}"
	for _, w := range strings.Split(want, "; ") {
		if !strings.Contains("; "+got+";", "; "+w+";") {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if len(rs) != 10 {
		t.Errorf("routes: %s", got)
	}
}

// TestLaravelApp runs the adapter and the validation-rules script against a
// real Laravel application (DRIFTGATE_TEST_LARAVEL: its folder).
func TestLaravelApp(t *testing.T) {
	dir := os.Getenv("DRIFTGATE_TEST_LARAVEL")
	if dir == "" {
		t.Skip("DRIFTGATE_TEST_LARAVEL not set")
	}
	inv, err := Run(context.Background(), dir, []Adapter{{Adapter: "laravel", Command: []string{"php", "artisan", "route:list", "--json"}, PHP: []string{"php"}}}, nil, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	r := find(t, inv.Routes, "POST /api/faq")
	if r.BodySource != "validation rules" || !strings.Contains(string(r.Body), `"password_confirmation"`) {
		t.Fatalf("POST /api/faq body (%s): %s", r.BodySource, r.Body)
	}
	if r := find(t, inv.Routes, "PATCH|PUT /api/faq/{id}"); r.BodySource != "validation rules" {
		t.Fatalf("PUT body source %q", r.BodySource)
	}
	if r := find(t, inv.Routes, "GET /api/ping"); len(r.Body) != 0 {
		t.Fatal("invokable controller without a form request got a body")
	}
}

func TestLaravelJSONAfterNotice(t *testing.T) {
	rs := run(t, Adapter{Adapter: "laravel", Command: []string{"cat", "testdata/laravel-noisy.json"}})
	find(t, rs, "POST /api/faq")
}

func TestSymfony(t *testing.T) {
	rs := run(t, Adapter{Adapter: "symfony", Command: []string{"cat", "testdata/symfony.json"}})
	for _, k := range []string{"GET /api/items", "POST /api/items", "GET /api/items/{id}", "* /api/search/{term?}"} { // captured in prod: no _error preview route
		find(t, rs, k)
	}
	if r := find(t, rs, "GET /api/items/{id}"); r.Name != "items_show" || !strings.Contains(r.Action, "ItemsController::show") {
		t.Errorf("name and action: %+v", r)
	}
}

func TestDjango(t *testing.T) {
	rs := run(t, Adapter{Adapter: "django", Command: []string{"cat", "testdata/django.txt"}})
	if r := find(t, rs, "GET|POST /api/items/"); r.Name != "item-list" {
		t.Errorf("view set list: %+v", r)
	}
	find(t, rs, "GET /api/items/{pk}/")
	find(t, rs, "* /health/")
	find(t, rs, "GET|POST /api/items.{format}")
	if r := find(t, rs, "* /admin/auth/user/{object_id*}/change/"); r.Source != "django" {
		t.Errorf("admin path converter: %+v", r)
	}
}

func TestRailsPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/items(.:format)":             "/items",
		"/items/:id(.:format)":         "/items/{id}",
		"/files/*path(.:format)":       "/files/{path*}",
		"(/:locale)/news(.:format)":    "/news /{locale}/news",
		"/posts(/page/:page)":          "/posts /posts/page/{page}",
		"/a(/:x/:y)":                   "/a /a/{x}/{y}",
		"/f(/*rest)":                   "/f /f/{rest*}",
		"/posts/:post_id/comments/:id": "/posts/{post_id}/comments/{id}",
	} {
		if got := strings.Join(railsPaths(in), " "); got != want {
			t.Errorf("railsPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRailsEngines(t *testing.T) {
	rs := run(t, Adapter{Adapter: "rails", Command: []string{"cat", "testdata/rails-engine.txt"}})
	got := strings.Join(keys(rs), "; ")
	for _, w := range []string{"GET /blog/articles", "GET /blog", "GET /", "GET|POST /both", "* /sidekiq", "GET|PROPFIND /dav"} {
		if !strings.Contains("; "+got+";", "; "+w+";") {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	for _, r := range rs {
		if r.Path == "/both" && r.Name != "" {
			t.Errorf("unnamed row took a name: %+v", r)
		}
		if strings.Contains(r.Action, "0x0000") {
			t.Errorf("memory address in action: %s", r.Action)
		}
		if r.Action == "Blorgh::Engine" {
			t.Errorf("engine mount kept although its routes are listed: %+v", r)
		}
	}
}

func TestSpringPath(t *testing.T) {
	for in, want := range map[string]string{
		"/items/{id:\\d+}": "/items/{id}",
		"/files/{*path}":   "/files/{path*?}",
		"/a/{id:\\d{3}}":   "/a/{id}",
		"/x/*/y":           "/x/{segment1}/y",
		"/static/**":       "/static/{rest*?}",
		"items/{id}/notes": "/items/{id}/notes",
	} {
		if got := springPath(in); got != want {
			t.Errorf("springPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpringActuator(t *testing.T) {
	raw, err := os.ReadFile("testdata/spring-mappings.json")
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(ctx context.Context, url, identity, target string) ([]byte, error) { return raw, nil }
	var first string
	for i := 0; i < 20; i++ { // map order must not matter
		inv, err := Run(context.Background(), ".", []Adapter{{Adapter: "spring"}}, fetch, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(inv)
		if i == 0 {
			first = string(b)
			find(t, inv.Routes, "GET /items/{id}")
			find(t, inv.Routes, "PATCH|PUT /items/{id}/notes")
			if r := find(t, inv.Routes, "* /error"); !strings.Contains(r.Action, "#error(HttpServletRequest) | ") || !strings.Contains(r.Action, "errorHtml(HttpServletRequest, HttpServletResponse)") {
				t.Errorf("both /error handlers: %q", r.Action)
			}
		} else if string(b) != first {
			t.Fatal("the inventory depends on map order")
		}
	}
}

func TestRails(t *testing.T) {
	rs := run(t, Adapter{Adapter: "rails", Command: []string{"cat", "testdata/rails-routes.txt"}})
	if r := find(t, rs, "PATCH|PUT /items/{id}"); r.Action != "items#update" || r.Name != "item" {
		t.Errorf("update: %+v", r)
	}
	find(t, rs, "GET /items")
	find(t, rs, "POST /items")
	find(t, rs, "DELETE /items/{id}")
	if r := find(t, rs, "GET /up"); r.Name != "rails_health_check" {
		t.Errorf("health check: %+v", r)
	}
	for _, r := range rs {
		if strings.Contains(r.Path, "(") || strings.Contains(r.Path, ":") {
			t.Errorf("unconverted path %s", r.Path)
		}
	}
}

func TestOpenAPIRequiredQuery(t *testing.T) {
	swagger2 := []byte(`{"swagger":"2.0","basePath":"/rest/default","paths":{"/V1/products-render-info":{"get":{"parameters":[
	 {"name":"storeId","in":"query","required":true,"type":"integer"},
	 {"name":"currencyCode","in":"query","required":true,"type":"string"},
	 {"name":"searchCriteria","in":"query","required":false,"type":"string"}]}},
	 "/V1/customers":{"post":{"parameters":[{"name":"body","in":"body","schema":{"type":"object","properties":{"email":{"type":"string"},"website_url":{"type":"string"}}}}]}}}}`)
	rs, err := openAPI(swagger2)
	if err != nil {
		t.Fatal(err)
	}
	r := find(t, rs, "GET /rest/default/V1/products-render-info")
	if !reflect.DeepEqual(r.Query, map[string]string{"storeId": "1", "currencyCode": "driftgate"}) {
		t.Errorf("query %v", r.Query)
	}
	if c := find(t, rs, "POST /rest/default/V1/customers"); string(c.Body) != `{"email":"driftgate@example.test","website_url":"https://example.test/driftgate"}` {
		t.Errorf("named samples: %s", c.Body)
	}
	oas3 := []byte(`openapi: 3.0.0
paths:
  /search:
    parameters:
      - {name: lang, in: query, required: true, schema: {type: string, enum: [sr, en]}}
    get:
      parameters:
        - {name: q, in: query, required: true, example: knjiga}
`)
	rs, err = openAPI(oas3)
	if err != nil {
		t.Fatal(err)
	}
	if r := find(t, rs, "GET /search"); !reflect.DeepEqual(r.Query, map[string]string{"lang": "sr", "q": "knjiga"}) {
		t.Errorf("query %v", r.Query)
	}
}

func TestOpenAPIFromPHP(t *testing.T) {
	// API Platform: JSON with \/ escapes, bodies as application/ld+json and
	// merge-patch+json, a read-only id, a decimal written as text.
	spec := []byte(`{"openapi":"3.1.0","paths":{"\/api\/items":{"post":{"requestBody":{"content":{"application\/ld+json":{"schema":{"$ref":"#\/components\/schemas\/Item"}}}}}},
	"\/api\/items\/{id}":{"patch":{"requestBody":{"content":{"application\/merge-patch+json":{"schema":{"$ref":"#\/components\/schemas\/Item"}}}}}}},
	"components":{"schemas":{"Item":{"type":"object","properties":{"id":{"type":"integer","readOnly":true},"name":{"type":"string"},"price":{"type":"string","format":"decimal"}}}}}}`)
	rs, err := openAPI(spec)
	if err != nil {
		t.Fatal(err)
	}
	post := find(t, rs, "POST /api/items")
	if post.BodyType != "application/ld+json" || string(post.Body) != `{"name":"driftgate","price":"1.50"}` {
		t.Errorf("post: %s %s", post.BodyType, post.Body)
	}
	if patch := find(t, rs, "PATCH /api/items/{id}"); patch.BodyType != "application/merge-patch+json" {
		t.Errorf("patch: %s", patch.BodyType)
	}
}

func TestMergeLendsBodiesInAnyOrder(t *testing.T) {
	for _, first := range []string{"command", "openapi"} {
		bare := routes.Route{Methods: []string{"POST"}, Path: "/items", Source: "command"}
		withBody := routes.Route{Methods: []string{"POST"}, Path: "/items", Source: "openapi", Body: []byte(`{"a":1}`)}
		inv := &routes.Inventory{Routes: []routes.Route{bare, withBody}}
		if first == "openapi" {
			inv.Routes = []routes.Route{withBody, bare}
		}
		merge(inv)
		if len(inv.Routes) != 1 || string(inv.Routes[0].Body) != `{"a":1}` {
			t.Errorf("%s first: %+v", first, inv.Routes)
		}
	}
}

// TestSourceFiles checks that each adapter that can tell where an action is
// defined fills File, relative to the project root.
func TestSourceFiles(t *testing.T) {
	t.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	check := func(rs []routes.Route, key, want string) {
		t.Helper()
		if got := find(t, rs, key).File; got != want {
			t.Errorf("%s: file %q, want %q", key, got, want)
		}
	}
	if _, err := exec.LookPath("php"); err == nil {
		rs := run(t, Adapter{Adapter: "laravel", Dir: "testdata/laravel-app", Command: []string{"cat", "../laravel13.json"}, PHP: []string{"php"}})
		check(rs, "GET /api/faq", "testdata/laravel-app/app/Http/Controllers/FaqController.php:6")
		check(rs, "GET /api/ping", "testdata/laravel-app/app/Http/Controllers/PingController.php:6")
		check(rs, "GET /api/news/{slug?}", "testdata/laravel-app/routes/api.php:5") // a closure
		if r := find(t, rs, "GET /api/faq"); len(r.Body) != 0 || r.BodySource != "" {
			t.Errorf("a route without a form request got a body: %+v", r)
		}
		// A closure that takes a form request gets a body from its rules,
		// as a controller method does.
		list := `[{"domain":null,"method":"POST","uri":"api\/news","name":null,"action":"Closure","middleware":["api"],"path":"routes\/api.php:8"}]`
		rs = run(t, Adapter{Adapter: "laravel", Dir: "testdata/laravel-app", Command: []string{"echo", list}, PHP: []string{"php"}})
		if r := find(t, rs, "POST /api/news"); r.BodySource != "validation rules" || !strings.Contains(string(r.Body), `"title"`) {
			t.Errorf("a closure with a form request: %+v (body %s)", r, r.Body)
		}
	} else {
		t.Log("php not found: Laravel script skipped")
	}
	// Without the PHP script, newer route:list versions name the file of a closure.
	rs := run(t, Adapter{Adapter: "laravel", Command: []string{"cat", "testdata/laravel13.json"}})
	check(rs, "GET /", "routes/web.php:5")
	check(rs, "GET /api/faq", "")

	rs = run(t, Adapter{Adapter: "rails", Dir: "testdata/rails-app", Command: []string{"cat", "../rails-routes.txt"}})
	check(rs, "PATCH|PUT /items/{id}", "testdata/rails-app/app/controllers/items_controller.rb:6")
	check(rs, "GET /items", "testdata/rails-app/app/controllers/items_controller.rb:2")
	check(rs, "POST /items", "testdata/rails-app/app/controllers/items_controller.rb") // no def create
	check(rs, "GET /up", "")                                                           // Rails' own controller

	rs = run(t, Adapter{Adapter: "symfony", Dir: "testdata/symfony-app", Command: []string{"cat", "../symfony.json"}})
	check(rs, "GET /api/items/{id}", "testdata/symfony-app/src/Controller/ItemsController.php:10")
	check(rs, "GET /api/items", "testdata/symfony-app/src/Controller/ItemsController.php:6")

	rs = run(t, Adapter{Adapter: "nextjs", Dir: "testdata/next"})
	check(rs, "GET /blog/{slug}", "testdata/next/src/app/blog/[slug]/page.tsx")
	check(rs, "* /api/legacy", "testdata/next/pages/api/legacy/index.js")
	rs = run(t, Adapter{Adapter: "nuxt", Dir: "testdata/nuxt"})
	check(rs, "GET /users/{id}", "testdata/nuxt/app/pages/users/[id].vue")
	check(rs, "POST /api/users", "testdata/nuxt/server/api/users/index.post.ts")

	if _, err := exec.LookPath("python3"); err == nil {
		rs = run(t, Adapter{Adapter: "django", Dir: "testdata/django-app", Command: []string{"sh", "-c", "(echo 'import urls'; cat) | python3 -"}})
		check(rs, "* /health/", "testdata/django-app/items/views.py:11") // through the decorator
		check(rs, "GET /api/items/{pk}/", "testdata/django-app/items/views.py:16")
		check(rs, "* /len/", "") // not the project's code
	} else {
		t.Log("python3 not found: Django script skipped")
	}

	// A file is lent like a body when another adapter finds the same route.
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"GET"}, Path: "/a", Source: "openapi"},
		{Methods: []string{"GET"}, Path: "/a", Source: "laravel", File: "app/A.php:3"},
	}}
	merge(inv)
	if len(inv.Routes) != 1 || inv.Routes[0].File != "app/A.php:3" {
		t.Errorf("merge: %+v", inv.Routes)
	}
}

// TestFromRoot: a route's file is a path inside the project, or nothing.
func TestFromRoot(t *testing.T) {
	for _, c := range [][4]string{
		{"/proj", "", "src/server.js:10", "src/server.js:10"},
		{"/proj", "api", "app/Http/X.php:4", "api/app/Http/X.php:4"},
		{"/proj", "", "/home/someone/proj/src/server.js:10", ""},
		{"/proj", "api", "/abs/x.js", ""},
		{"/proj", "", "../../etc/x.js", ""},
		{"/proj", "", "a/../../x.js", ""},
		{"/proj", "../other", "x.js", ""},
	} {
		if got := fromRoot(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("fromRoot(%q, %q, %q) = %q, want %q", c[0], c[1], c[2], got, c[3])
		}
	}
}

// TestRoutePathsStartWithSlash: a route path becomes a catalog path, which
// is appended to the base URL; without a leading "/" it could name another
// host (a base path of "@evil.example").
func TestRoutePathsStartWithSlash(t *testing.T) {
	dir := t.TempDir()
	for name, spec := range map[string]string{
		"swagger": "swagger: \"2.0\"\nbasePath: \"@127.0.0.1:8906\"\npaths:\n  /steal:\n    get: {}\n",
		"servers": "{\"openapi\": \"3.0.0\", \"servers\": [{\"url\": \"@evil.example\"}], \"paths\": {\"/steal\": {\"get\": {}}}}",
	} {
		file := dir + "/" + name
		os.WriteFile(file, []byte(spec), 0o644)
		if _, err := Run(context.Background(), ".", []Adapter{{Adapter: "openapi", File: file}}, nil, nil); err == nil || !strings.Contains(err.Error(), "start with /") {
			t.Errorf("%s: %v", name, err)
		}
	}
	out := `__DRIFTGATE_ROUTES__[{"methods": ["GET"], "path": "@127.0.0.1:8906/steal"}]`
	if _, err := Run(context.Background(), ".", []Adapter{{Adapter: "django", Command: []string{"echo", out}}}, nil, nil); err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Errorf("django: %v", err)
	}
	if _, err := Run(context.Background(), ".", []Adapter{{Adapter: "django", Command: []string{"echo", `__DRIFTGATE_ROUTES__[{"methods": ["GET"], "path": "/x"}]`}, BasePath: "evil.example"}}, nil, nil); err == nil {
		t.Error("base_path without a leading / accepted")
	}
}

// TestOpenAPIAliasBomb: nested YAML aliases expand to billions of nodes; a
// specification with aliases is refused at once.
func TestOpenAPIAliasBomb(t *testing.T) {
	spec := "openapi: 3.0.0\na0: &a0 [\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\"]\n"
	for i := 1; i <= 9; i++ {
		prev := "*a" + strconv.Itoa(i-1)
		spec += "a" + strconv.Itoa(i) + ": &a" + strconv.Itoa(i) + " [" + strings.Repeat(prev+",", 9) + prev + "]\n"
	}
	spec += "paths: {}\n"
	done := make(chan error, 1)
	go func() { _, err := openAPI([]byte(spec)); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "alias") {
			t.Fatalf("aliases accepted: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the aliases were expanded")
	}
	if _, err := openAPI(make([]byte, maxSpec+1)); err == nil {
		t.Error("a specification over the size limit was read")
	}
}

// TestLaravelAuthMiddleware: route:list names middleware by class (guest is
// RedirectIfAuthenticated, which is not authentication), and discover[].auth
// names the application's own by alias or class, never by a part of a name.
func TestLaravelAuthMiddleware(t *testing.T) {
	src := Adapter{Adapter: "laravel", Command: []string{"cat", "testdata/laravel-auth.json"}}
	rs := run(t, src)
	if r := find(t, rs, "POST /login"); r.Auth {
		t.Error("guest (RedirectIfAuthenticated) marked as needing auth")
	}
	if !find(t, rs, "GET /api/me").Auth || find(t, rs, "GET /api/profile").Auth {
		t.Errorf("without auth: %+v", rs)
	}
	if got := strings.Join(SuspectAuth(rs), ", "); got != `App\Http\Middleware\GetUserFromToken, App\Http\Middleware\VerifyJwtToken` {
		t.Errorf("suspects: %s", got)
	}

	src.Auth = []string{"getuserfromtoken", "adminpanel", "App\\Http\\Middleware\\VerifyJwtToken"}
	rs = run(t, src)
	for _, k := range []string{"GET /api/profile", "GET /api/admin/stats", "GET /api/report", "GET /api/me"} {
		if !find(t, rs, k).Auth {
			t.Errorf("%s not marked as needing auth", k)
		}
	}
	if find(t, rs, "POST /login").Auth || len(SuspectAuth(rs)) != 0 {
		t.Errorf("with auth: %+v, suspects %v", rs, SuspectAuth(rs))
	}

	// A part of a name matches nothing: api is a group of every route.
	src.Auth = []string{"Token", "Http", "ap"}
	for _, r := range run(t, src) {
		if r.Auth != (r.Path == "/api/me") {
			t.Errorf("%s: auth %v with a part of a name", r.Key(), r.Auth)
		}
	}
	if err := (Adapter{Adapter: "openapi", File: "x.json", Auth: []string{"jwt"}}).Validate(); err == nil || !strings.Contains(err.Error(), "auth is for adapter laravel") {
		t.Errorf("auth on openapi: %v", err)
	}
}
