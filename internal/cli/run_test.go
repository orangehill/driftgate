package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/secret"
)

func TestInitDetects(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "artisan"), "#!/usr/bin/env php\n")
	write(t, filepath.Join(root, "composer.lock"), `{"packages":[{"name":"laravel/framework","version":"v5.5.50"}]}`)
	write(t, filepath.Join(root, "docs", "openapi.yaml"), "openapi: 3.0.0\n")
	cfg := filepath.Join(root, "driftgate.yaml")
	if code, out, errOut := runCLI(t, "init", "--config", cfg); code != 0 || !strings.Contains(out, "found laravel, openapi") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(cfg)
	text := string(raw)
	for _, want := range []string{`command: ["php", "artisan", "route:list"]`, "Laravel 5 prints a table", "file: docs/openapi.yaml", "base_url: http://localhost:8000"} {
		if !strings.Contains(text, want) {
			t.Errorf("driftgate.yaml lacks %q:\n%s", want, text)
		}
	}
	// The written file must load.
	if code, _, errOut := runCLI(t, "coverage", "--config", cfg); code != ExitConfig || !strings.Contains(errOut, "discover --write") {
		t.Fatalf("coverage before discover: %d %s", code, errOut)
	}

	next := t.TempDir()
	write(t, filepath.Join(next, "next.config.mjs"), "export default {}\n")
	if code, out, _ := runCLI(t, "init", "--config", filepath.Join(next, "driftgate.yaml")); code != 0 || !strings.Contains(out, "found nextjs") || strings.Contains(out, "not been checked") {
		t.Fatalf("init in a Next.js project: %s", out)
	}
}

const demoRoutes = `[
 {"method":"GET","path":"/api/news"},
 {"method":"GET","path":"/api/news/{id}"},
 {"method":"GET","path":"/api/me","auth":true},
 {"method":"POST","path":"/api/faq","body":{"q":"?"}},
 {"method":"POST","path":"/admin/reset"}]`

// TestRun goes from an empty catalog to a baseline and then to comparisons,
// on the demo API.
func TestRun(t *testing.T) {
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "routes-src.json"), demoRoutes)
	write(t, cfg, fmt.Sprintf(`version: 1
project: demo
target:
  base_url: %s
identities:
  admin: { env: DG_DEMO_ADMIN }
state:
  api:
    restore: ["curl", "-sf", "-X", "POST", "%s/admin/reset"]
discover:
  - adapter: command
    command: ["cat", "routes-src.json"]
generate:
  identity: admin
templates: [ok, anonymous]
params:
  - name: id
    value: "12"
storage:
  public: ["get-api-news*", "post-api-faq"]
`, srv.URL, srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "exclusions.yaml"), "version: 1\nroutes:\n  - route: POST /admin/reset\n    reason: Test hook of the demo, used as the restore command.\n")
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
body:
  - request: post-api-faq
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Time of creation.
`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DG_DEMO_ADMIN", demo.AdminToken)
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "baseline local made from two equal captures") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "driftgate", "baselines", "local", "responses", "get-api-me-anonymous.json")); err != nil {
		t.Fatalf("baseline lacks the anonymous variant: %v", err)
	}
	if code, out, errOut = runCLI(t, "run", "--config", cfg); code != ExitClean || !strings.Contains(out, "No drift") {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	api.SetVersion("v2")
	if code, out, _ = runCLI(t, "run", "--config", cfg); code != ExitDrift || !strings.Contains(out, "x-powered-by") {
		t.Fatalf("run on v2: %d\n%s", code, out)
	}
	api.SetVersion("v1")
	// A new route: run stops at discover.
	write(t, filepath.Join(root, "routes-src.json"), strings.Replace(demoRoutes, `{"method":"GET","path":"/api/news"},`, `{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/slow"},`, 1))
	if code, out, _ = runCLI(t, "run", "--config", cfg); code != ExitDrift || !strings.Contains(out, "route-added   GET /api/slow") {
		t.Fatalf("run with a new route: %d\n%s", code, out)
	}
}

// TestFrameworkCoverage: on a Next.js and a Nuxt project, a new page or API
// route fails discover and then coverage.
func TestFrameworkCoverage(t *testing.T) {
	for _, c := range []struct{ adapter, fixture, params, newFile string }{
		{"nextjs", "next", `
  - name: slug
    value: hello
  - name: id
    value: "1"
  - name: path
    value: intro`, "src/app/pricing/page.tsx"},
		{"nuxt", "nuxt", `
  - name: id
    value: "1"
  - name: slug
    value: about
  - name: group
    value: admin
  - name: _
    value: a/b`, "server/api/orders.get.ts"},
	} {
		t.Run(c.adapter, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(filepath.Join(root, "app"), os.DirFS(filepath.Join("..", "discover", "testdata", c.fixture))); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(root, "driftgate.yaml")
			write(t, cfg, fmt.Sprintf("version: 1\nproject: web\ntarget:\n  base_url: http://localhost:3000\ndiscover:\n  - adapter: %s\n    dir: app\nparams:%s\n", c.adapter, c.params))
			write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
			write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\n")
			for _, args := range [][]string{{"discover", "--write"}, {"generate", "--write"}} {
				if code, out, errOut := runCLI(t, append(args, "--config", cfg)...); code != 0 {
					t.Fatalf("%v: %d\n%s\n%s", args, code, out, errOut)
				}
			}
			if code, out, _ := runCLI(t, "coverage", "--config", cfg); code != ExitClean {
				t.Fatalf("coverage: %d\n%s", code, out)
			}
			write(t, filepath.Join(root, "app", c.newFile), "export default function X() {}\n")
			if code, out, _ := runCLI(t, "discover", "--config", cfg); code != ExitDrift || !strings.Contains(out, "route-added") {
				t.Fatalf("discover after a new file: %d\n%s", code, out)
			}
			runCLI(t, "discover", "--write", "--config", cfg)
			if code, out, _ := runCLI(t, "coverage", "--config", cfg); code != ExitDrift {
				t.Fatalf("coverage after a new route: %d\n%s", code, out)
			}
		})
	}
}

func TestInitRecognisesFrameworks(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  []string
	}{
		{map[string]string{"composer.json": `{"require":{"magento/product-community-edition":"2.4.7"}}`}, []string{"url: /rest/default/schema?services=all", "base_url: http://localhost\n", "identity:"}},
		{map[string]string{"composer.json": `{"require":{"symfony/framework-bundle":"7.1","api-platform/core":"4"}}`}, []string{"url: /api/docs.jsonopenapi", "adapter: symfony"}},
		{map[string]string{"composer.json": `{"require":{"symfony/framework-bundle":"7.1","nelmio/api-doc-bundle":"5"}}`,
			"composer.lock": `{"packages":[{"name":"nelmio/api-doc-bundle","require-dev":{"api-platform/core":"^4"},"suggest":{"api-platform/core":"for API Platform"}}]}`}, []string{"url: /api/doc.json", "adapter: symfony"}},
		{map[string]string{"pom.xml": "<artifactId>spring-boot-starter-web</artifactId><artifactId>springdoc-openapi-starter-webmvc-ui</artifactId>"}, []string{"url: /v3/api-docs", "base_url: http://localhost:8080"}},
		{map[string]string{"build.gradle.kts": `implementation("org.springframework.boot:spring-boot-starter-web")`}, []string{"/actuator/mappings"}},
		{map[string]string{"pom.xml": "<groupId>io.quarkus</groupId><artifactId>quarkus-smallrye-openapi</artifactId>"}, []string{"url: /q/openapi?format=json"}},
		{map[string]string{"Api/Api.csproj": `<Project Sdk="Microsoft.NET.Sdk.Web"><PackageReference Include="Swashbuckle.AspNetCore" /></Project>`}, []string{"url: /swagger/v1/swagger.json"}},
		{map[string]string{"pyproject.toml": "dependencies = [\"fastapi>=0.110\"]"}, []string{"url: /openapi.json"}},
		{map[string]string{"manage.py": "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'app.settings')\nfrom django.core.management import execute_from_command_line\n", "requirements.txt": "drf-spectacular==0.27"}, []string{"url: /api/schema/"}},
		{map[string]string{"Gemfile": "gem \"rails\"", "config/routes.rb": ""}, []string{"bin/rails routes"}},
		{map[string]string{"pom.xml": "<groupId>io.micronaut</groupId>", "target/classes/META-INF/swagger/demo-0.0.yml": "openapi: 3.0.1\n"}, []string{"file: target/classes/META-INF/swagger/demo-0.0.yml"}},
		{map[string]string{"package.json": `{"dependencies":{"@nestjs/core":"10","@nestjs/swagger":"7"}}`}, []string{"url: /api-json"}},
	}
	for _, c := range cases {
		root := t.TempDir()
		for name, content := range c.files {
			write(t, filepath.Join(root, name), content)
		}
		cfg := filepath.Join(root, "driftgate.yaml")
		code, out, errOut := runCLI(t, "init", "--config", cfg)
		if code != 0 {
			t.Fatalf("%v: %d %s", c.files, code, errOut)
		}
		raw, _ := os.ReadFile(cfg)
		for _, w := range c.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%v: driftgate.yaml lacks %q\n%s", c.files, w, raw)
			}
		}
		if strings.Contains(out, "not been checked") {
			t.Errorf("%v: every variant here has been checked: %s", c.files, out)
		}

		if code, _, errOut := runCLI(t, "seed", "--config", cfg); code != ExitConfig || strings.Contains(errOut, "yaml") {
			t.Errorf("%v: the written configuration does not load: %s", c.files, errOut)
		}
		if _, err := allowlist.Load(filepath.Join(root, "driftgate", "allowlist.yaml")); err != nil {
			t.Errorf("%v: the written allowlist does not load: %v", c.files, err)
		}
		if _, err := routes.LoadExclusions(filepath.Join(root, "driftgate", "exclusions.yaml")); err != nil {
			t.Errorf("%v: the written exclusions do not load: %v", c.files, err)
		}
	}
}

func TestInitAvoidsFalseFrameworks(t *testing.T) {
	for _, files := range []map[string]string{
		{"requirements.txt": "# not fastapi, we use flask\nflask==3.0\n"},
		{"Gemfile": "gem 'sinatra'\ngem 'rubocop-rails'\n", "config/routes.rb": ""},
		{"manage.py": "from flask_script import Manager\n"},
	} {
		root := t.TempDir()
		for name, content := range files {
			write(t, filepath.Join(root, name), content)
		}
		_, out, _ := runCLI(t, "init", "--config", filepath.Join(root, "driftgate.yaml"))
		if strings.Contains(out, "found ") {
			t.Errorf("%v: %s", files, out)
		}
	}
}

func TestRunWithoutBaselineFails(t *testing.T) {
	p := newDemoProject(t, "")
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "allowlist.yaml"), "version: 1\n")
	code, out, _ := runCLI(t, "run", "--config", p.cfg)
	if code != ExitDrift || !strings.Contains(out, "no baseline was made") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// TestRunSeveralServices: an API and a second service in one project, each
// with its own address and routes; one run checks both, and a change in one
// fails the run with drift on that service only.
func TestRunSeveralServices(t *testing.T) {
	api, web := demo.New("v1"), demo.New("v1")
	apiSrv, webSrv := httptest.NewServer(api), httptest.NewServer(web)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(webSrv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "api-routes.json"), `[{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/news/{id}"}]`)
	write(t, filepath.Join(root, "web", "routes.json"), `[{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/contact"}]`)
	write(t, cfg, fmt.Sprintf(`version: 1
project: shop
targets:
  api:
    base_url: %s
  web:
    base_url: %s
discover:
  - adapter: command
    target: api
    command: ["cat", "api-routes.json"]
  - adapter: command
    target: web
    dir: web
    command: ["cat", "routes.json"]
state:
  api:
    restore: ["curl", "-sf", "-X", "POST", "%s/admin/reset"]
  web:
    restore: ["curl", "-sf", "-X", "POST", "%s/admin/reset"]
params:
  - name: id
    value: "11"
storage:
  default: private
`, apiSrv.URL, webSrv.URL, apiSrv.URL, webSrv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: time\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "baseline local made") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"route-added   api: GET /api/news", "route-added   web: GET /api/contact", "4 routes: 4 covered"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run lacks %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(root, "driftgate", "catalog.json"))
	for _, id := range []string{`"api-get-api-news"`, `"web-get-api-news"`, `"web-get-api-contact"`, `"target": "web"`} {
		if !strings.Contains(string(raw), id) {
			t.Errorf("catalog lacks %s", id)
		}
	}
	if code, out, _ = runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("second run: %d\n%s", code, out)
	}
	web.SetVersion("v2")
	code, out, _ = runCLI(t, "run", "--config", cfg)
	if code != ExitDrift || !strings.Contains(out, "DRIFT  web-get-api-news") || strings.Contains(out, "DRIFT  api-") {
		t.Fatalf("drift in web only: %d\n%s", code, out)
	}
	web.SetVersion("v1")

	// A new route is seen by coverage in the same run, and the run says why it fails.
	write(t, filepath.Join(root, "web", "routes.json"), `[{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/contact"},{"method":"GET","path":"/api/faq"}]`)
	code, out, _ = runCLI(t, "run", "--config", cfg)
	for _, want := range []string{"route-added   web: GET /api/faq", "uncovered", "web: GET /api/faq", "the run fails because:", "the routes changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("new route: output lacks %q", want)
		}
	}
	if code != ExitDrift {
		t.Errorf("new route: exit %d\n%s", code, out)
	}
}

// TestInitMonorepo: a Laravel API, a Next.js frontend and a NestJS
// microservice in one repository, with docker-compose ports.
func TestInitMonorepo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shop")
	write(t, filepath.Join(root, "docker-compose.yml"), `services:
  api:
    build: ./backend
    ports: ["8000:80"]
  web:
    build:
      context: frontend
    ports: ["127.0.0.1:3001:3000"]
  payments:
    build: ./services/payments
    ports:
      - published: 4000
        target: 3000
  db:
    image: mariadb:11
    ports: ["3306:3306"]
`)
	write(t, filepath.Join(root, "backend", "artisan"), "")
	write(t, filepath.Join(root, "backend", "composer.json"), `{"require":{"laravel/framework":"^12.0"}}`)
	write(t, filepath.Join(root, "frontend", "next.config.mjs"), "export default {}\n")
	write(t, filepath.Join(root, "services", "payments", "package.json"), `{"dependencies":{"@nestjs/core":"10","@nestjs/swagger":"7"}}`)
	write(t, filepath.Join(root, "node_modules", "x", "next.config.js"), "")
	cfg := filepath.Join(root, "driftgate.yaml")
	code, out, errOut := runCLI(t, "init", "--config", cfg)
	if code != 0 {
		t.Fatalf("init: %d %s", code, errOut)
	}
	for _, want := range []string{"backend (laravel, backend)", "frontend (nextjs, frontend)", "payments (nestjs, services/payments)"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output lacks %q:\n%s", want, out)
		}
	}
	p, err := config.Load(cfg)
	if err != nil {
		raw, _ := os.ReadFile(cfg)
		t.Fatalf("the written configuration does not load: %v\n%s", err, raw)
	}
	want := map[string]string{"backend": "http://localhost:8000", "frontend": "http://localhost:3001", "payments": "http://localhost:4000"}
	if len(p.Targets) != 3 {
		t.Fatalf("targets %v", p.Targets)
	}
	for name, base := range want {
		if p.Targets[name].BaseURL != base {
			t.Errorf("%s: base_url %q, want %q", name, p.Targets[name].BaseURL, base)
		}
	}
	got := map[string]string{}
	for _, a := range p.Discover {
		got[a.Target] = a.Adapter + " " + a.Dir
	}
	if got["backend"] != "laravel backend" || got["frontend"] != "nextjs frontend" || got["payments"] != "openapi services/payments" {
		t.Errorf("adapters %v", got)
	}
	excl, err := routes.LoadExclusions(filepath.Join(root, "driftgate", "exclusions.yaml"))
	if err != nil || len(excl.Routes) == 0 || excl.Routes[0].Target != "backend" {
		t.Errorf("exclusions %+v %v", excl, err)
	}
}

// TestRunOneService: --target checks one service of several and says so.
func TestRunOneService(t *testing.T) {
	api, web := demo.New("v1"), demo.New("v1")
	apiSrv, webSrv := httptest.NewServer(api), httptest.NewServer(web)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(webSrv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "api-routes.json"), `[{"method":"GET","path":"/api/news"}]`)
	write(t, filepath.Join(root, "web-routes.json"), `[{"method":"GET","path":"/api/contact"}]`)
	write(t, cfg, fmt.Sprintf(`version: 1
project: shop
targets:
  api:
    base_url: %s
  web:
    base_url: %s
discover:
  - adapter: command
    target: api
    command: ["cat", "api-routes.json"]
  - adapter: command
    target: web
    command: ["cat", "web-routes.json"]
storage:
  default: private
`, apiSrv.URL, webSrv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: time\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	if code, out, _ := runCLI(t, "run", "--config", cfg, "--target", "api"); code != ExitConfig {
		t.Fatalf("a first run with --target: %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "run", "--config", cfg); code != ExitClean {
		t.Fatalf("baseline: %d\n%s", code, out)
	}
	// web changes and its server is down: a run on api alone neither sees nor needs it.
	web.SetVersion("v2")
	webSrv.Close()
	code, out, errOut := runCLI(t, "run", "--config", cfg, "--target", "api")
	if code != ExitClean || !strings.Contains(out, "PARTIAL CHECK — checked: api; not checked: web") || strings.Contains(out, "web-") {
		t.Fatalf("run --target api: %d\n%s\n%s", code, out, errOut)
	}
	if code, _, _ = runCLI(t, "run", "--config", cfg, "--target", "nope"); code != ExitConfig {
		t.Errorf("unknown service: %d", code)
	}
	write(t, filepath.Join(root, "api-routes.json"), `[{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/faq"}]`)
	if code, out, errOut := runCLI(t, "discover", "--config", cfg, "--target", "api", "--write"); code != ExitClean {
		t.Fatalf("discover --target api --write: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "generate", "--config", cfg, "--target", "api", "--write"); code != ExitClean {
		t.Fatalf("generate --target api --write: %d\n%s\n%s", code, out, errOut)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "driftgate", "routes.json"))
	if !strings.Contains(string(raw), "/api/contact") || !strings.Contains(string(raw), "/api/faq") {
		t.Errorf("routes.json should keep web's routes and add api's new one:\n%s", raw)
	}
	raw, _ = os.ReadFile(filepath.Join(root, "driftgate", "catalog.json"))
	if !strings.Contains(string(raw), `"web-get-api-contact"`) || !strings.Contains(string(raw), `"api-get-api-faq"`) {
		t.Errorf("catalog.json should keep web's requests and add api's new one:\n%s", raw)
	}
}

// TestInitChosenServices: init --service keeps the services named.
func TestInitChosenServices(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shop")
	write(t, filepath.Join(root, "backend", "artisan"), "")
	write(t, filepath.Join(root, "backend", "composer.json"), `{"require":{"laravel/framework":"^12.0"}}`)
	write(t, filepath.Join(root, "frontend", "next.config.mjs"), "export default {}\n")
	cfg := filepath.Join(root, "driftgate.yaml")
	if code, _, _ := runCLI(t, "init", "--config", cfg, "--service", "nope"); code != ExitConfig {
		t.Fatalf("unknown service: %d", code)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Fatal("a failed init wrote the configuration")
	}
	code, out, errOut := runCLI(t, "init", "--config", cfg, "--service", "backend")
	if code != 0 || strings.Contains(out, "frontend") {
		t.Fatalf("init --service backend: %d\n%s\n%s", code, out, errOut)
	}
	p, err := config.Load(cfg)
	if err != nil || len(p.Targets) != 1 || p.Targets["backend"].BaseURL == "" || len(p.Discover) != 1 || p.Discover[0].Dir != "backend" {
		t.Fatalf("config: %+v %v", p, err)
	}
	os.Remove(cfg)
	if _, out, _ = runCLI(t, "init", "--config", cfg); !strings.Contains(out, "init --service <name>") {
		t.Errorf("init without --service should say how to choose:\n%s", out)
	}
}

// TestSeedSkipCommands: seed --skip-commands takes the working state as it
// is, without running seed:.
func TestSeedSkipCommands(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	marker := filepath.Join(root, "seeded")
	write(t, cfg, fmt.Sprintf(`version: 1
project: seedskip
target:
  base_url: http://127.0.0.1:1
state:
  files:
    restore: ["true"]
    fingerprint: ["echo", "same"]
seed:
  - ["touch", %q]
`, marker))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	if code, out, errOut := runCLI(t, "seed", "--config", cfg, "--skip-commands"); code != 0 {
		t.Fatalf("seed --skip-commands: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("seed --skip-commands ran the seed commands")
	}
	if code, out, errOut := runCLI(t, "seed", "--config", cfg); code != 0 {
		t.Fatalf("seed: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("seed did not run the seed commands")
	}
}

// TestRunFirstWithBaselineFolder: a baseline folder without responses/
// stops the first run before it captures anything; an empty one is taken
// for no baseline.
func TestRunFirstWithBaselineFolder(t *testing.T) {
	p := newDemoProject(t, "")
	dir := filepath.Join(filepath.Dir(p.cfg), "driftgate", "baselines", "local")
	write(t, filepath.Join(dir, "README"), "x")
	code, out, errOut := runCLI(t, "run", "--config", p.cfg)
	if code != ExitConfig || !strings.Contains(errOut, "holds no baseline") || strings.Contains(out, "== capture ==") {
		t.Fatalf("folder with a file: %d\n%s\n%s", code, out, errOut)
	}
	os.Remove(filepath.Join(dir, "README"))
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("empty folder: %d\n%s\n%s", code, out, errOut)
	}
}

// runTagFor is the capture tag run writes for the project of cfg.
func runTagFor(t *testing.T, cfg string) string {
	t.Helper()
	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return runTag(p)
}

// TestRunCapturePerCheckout: two checkouts of a project (worktrees) share
// the local store; each run captures into its own folder, as each keeps its
// own last comparison.
func TestRunCapturePerCheckout(t *testing.T) {
	t.Setenv(config.EnvStore, t.TempDir())
	cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values"}
	a, err := config.New(cfg, filepath.Join(t.TempDir(), "main"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.New(cfg, filepath.Join(t.TempDir(), "worktree"))
	if err != nil {
		t.Fatal(err)
	}
	if runTag(a) == runTag(b) || !strings.HasPrefix(runTag(a), "run-") {
		t.Errorf("run tags %s and %s", runTag(a), runTag(b))
	}
	for _, p := range []*config.Project{a, b} {
		if _, err := p.Capture(runTag(p)); err != nil {
			t.Errorf("%s: %v", runTag(p), err)
		}
	}
}

// TestRunAppDown: a run whose every request finds no application stops
// after the first capture and says how to start it, not with a second
// capture's error.
func TestRunAppDown(t *testing.T) {
	setupDemo(t, "http://127.0.0.1:"+closedPort(t))
	if code, out, errOut := runCLI(t, "init"); code != ExitClean {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := runCLI(t, "run")
	if code != ExitEnv || !strings.Contains(errOut, "nothing answered: is the application running at http://127.0.0.1:") ||
		!strings.Contains(errOut, "or set the address in .driftgate/driftgate.yaml") || strings.Contains(errOut, "private body not found") {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
}

// TestMessagesNameTheConfiguration: the hints of generate and capture
// name the project's configuration where it is (.driftgate/), and a DSN
// whose file is missing is a configuration error (exit 2), in run as in
// setup.
func TestMessagesNameTheConfiguration(t *testing.T) {
	root := setupDir(t, map[string]string{"openapi.json": `{"openapi":"3.0.0","info":{"title":"s","version":"1"},"paths":{
 "/a/{id}":{"get":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"integer"}}],"responses":{"200":{"description":"ok"}}}},
 "/b":{"post":{"responses":{"200":{"description":"ok"}}}}}}`})
	if code, out, errOut := runCLI(t, "init", "--project", "shop"); code != ExitClean {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := runCLI(t, "run")
	if !strings.Contains(out, "Give a value in .driftgate/driftgate.yaml, for one route") || code != ExitConfig ||
		!strings.Contains(errOut, "configure state: in .driftgate/driftgate.yaml") {
		t.Errorf("run: %d\n%s\n%s", code, out, errOut)
	}
	cfg := filepath.Join(root, ".driftgate", "driftgate.yaml")
	write(t, cfg, mustRead(t, cfg)+"state:\n  db:\n    driver: mysql\n    dsn: { dotenv: .env }\n    working: shop\n")
	if code, out, errOut := runCLI(t, "run"); code != ExitConfig || !strings.Contains(errOut, "state db: dsn: .env: not found") {
		t.Errorf("run without .env: %d\n%s\n%s", code, out, errOut)
	}
}
