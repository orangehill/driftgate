package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/lazy"
)

// detected is what init recognises in a project folder.
type detected struct {
	kinds       []string // laravel, nextjs, spring, …
	unchecked   []string // recognised, but not yet checked on a real project
	baseURL     string
	block       string      // discover: section for driftgate.yaml
	excluded    [][2]string // routes of the framework itself: route, reason
	excludedFor [][3]string // with several services: service, route, reason
	targets     string      // targets: section, with several services
	services    []string    // the services of targets:
	defaults    []string    // those of them whose base_url is only the framework's default port
	found       []string    // every service found, also those left out
	allow       string      // allowlist body entries
	headers     string      // allowlist header entries
	observe     string      // observe: section for driftgate.yaml
}

func exists(root string, names ...string) string {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			return n
		}
	}
	return ""
}

// contains reports whether one of the files mentions the text.
func contains(root, text string, files ...string) bool {
	for _, f := range files {
		matches, _ := filepath.Glob(filepath.Join(root, f))
		for _, m := range matches {
			if raw, err := os.ReadFile(m); err == nil && strings.Contains(string(raw), text) {
				return true
			}
		}
	}
	return false
}

var (
	majorVersion = lazy.Regexp(`^v?(\d+)`)
	gemRails     = lazy.Regexp(`(?m)^\s*gem\s+["']rails["']`)
)

func read(root, name string) string {
	raw, _ := os.ReadFile(filepath.Join(root, name))
	return string(raw)
}

// composerPackages lists the packages a PHP project requires itself
// (composer.json require and require-dev) and installs (composer.lock), not
// the packages a dependency merely mentions or suggests.
func composerPackages(root string) map[string]bool {
	out := map[string]bool{}
	var manifest struct {
		Require    map[string]any `json:"require"`
		RequireDev map[string]any `json:"require-dev"`
	}
	if json.Unmarshal([]byte(read(root, "composer.json")), &manifest) == nil {
		for n := range manifest.Require {
			out[n] = true
		}
		for n := range manifest.RequireDev {
			out[n] = true
		}
	}
	var lock struct {
		Packages    []struct{ Name string } `json:"packages"`
		PackagesDev []struct{ Name string } `json:"packages-dev"`
	}
	if json.Unmarshal([]byte(read(root, "composer.lock")), &lock) == nil {
		for _, p := range append(lock.Packages, lock.PackagesDev...) {
			out[p.Name] = true
		}
	}
	return out
}

func requires(root, pkg string) bool { return composerPackages(root)[pkg] }

func requiresPrefix(root, prefix string) bool {
	for n := range composerPackages(root) {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// dependsOn reports a package named on a line that is not a comment, as a
// whole word (fastapi, not fastapi-users in a comment).
func dependsOn(root, pkg string, files ...string) bool {
	re := regexp.MustCompile(`(?m)^[^#\n]*["'\s=\[]` + regexp.QuoteMeta(pkg) + `([^\w-]|$)|^` + regexp.QuoteMeta(pkg) + `([^\w-]|$)`)
	for _, f := range files {
		matches, _ := filepath.Glob(filepath.Join(root, f))
		for _, m := range matches {
			if raw, err := os.ReadFile(m); err == nil && re.Match(raw) {
				return true
			}
		}
	}
	return false
}

// laravelMajor reads the major version of laravel/framework from
// composer.lock; 0 when unknown.
func laravelMajor(root string) int {
	raw, err := os.ReadFile(filepath.Join(root, "composer.lock"))
	if err != nil {
		return 0
	}
	var lock struct {
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	}
	if json.Unmarshal(raw, &lock) != nil {
		return 0
	}
	for _, p := range lock.Packages {
		if p.Name == "laravel/framework" {
			if m := majorVersion().FindStringSubmatch(p.Version); m != nil {
				n, _ := strconv.Atoi(m[1])
				return n
			}
		}
	}
	return 0
}

// detector recognises one kind of project and writes its discover entry.
type detector struct {
	kind    string
	checked func(root string) bool // whether the entry init writes was checked on a real project
	baseURL string
	match   func(root string) bool
	entry   func(root string) string
	exclude [][2]string // routes the framework adds for itself
	allow   string      // allowlist body entries for values the framework changes by itself
	headers string      // allowlist header entries, the same for headers
	observe string      // observe: entries for routes whose answer changes by itself
}

func openapiURL(url, note string) func(string) string {
	return func(string) string {
		s := "  - adapter: openapi\n    url: " + url
		if note != "" {
			s += "   # " + note
		}
		return s + "\n"
	}
}

func comment(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("  # " + l + "\n")
	}
	return b.String()
}

var (
	jvm    = []string{"pom.xml", "build.gradle", "build.gradle.kts"}
	python = []string{"pyproject.toml", "requirements*.txt", "Pipfile", "setup.py"}
	dotnet = []string{"*.csproj", "*/*.csproj"}
	node   = []string{"package.json"}
)

func always(string) bool { return true }

// detectors is built on first use: the Next.js and Nuxt rules hold patterns
// made at run time (assetURL).
var detectors = sync.OnceValue(func() []detector {
	return []detector{
		{kind: "laravel", checked: always, baseURL: "http://localhost:8000",
			match: func(r string) bool {
				return exists(r, "artisan") != "" && requires(r, "laravel/framework")
			},
			entry: func(r string) string {
				list, note := `["php", "artisan", "route:list", "--json"]`, ""
				if major := laravelMajor(r); major > 0 && major < 8 {
					list = `["php", "artisan", "route:list"]`
					note = fmt.Sprintf("   # Laravel %d prints a table; DriftGate reads it", major)
				}
				return fmt.Sprintf("  - adapter: laravel\n    command: %s%s\n    php: [\"php\"]                 # reads validation rules for request bodies\n", list, note) +
					comment(`in Docker: command: ["docker", "compose", "exec", "-T", "php", "php", "artisan", "route:list", "--json"]`,
						`           php: ["docker", "compose", "exec", "-T", "php", "php"]`)
			},
			exclude: [][2]string{
				{"/_debugbar/*", "Laravel Debugbar: a development tool."},
				{"/_ignition/*", "Ignition error pages: a development tool."},
				{"/_dusk/*", "Laravel Dusk routes: a testing tool that logs in as any user."},
				{"/telescope", "Laravel Telescope: a development tool."},
				{"/telescope/*", "Laravel Telescope: a development tool."},
				{"/horizon", "Laravel Horizon dashboard: an operations tool, not the API."},
				{"/horizon/*", "Laravel Horizon dashboard: an operations tool, not the API."},
				{"/storage/*", "Files of the local disk served by the framework."},
			},
			headers: laravelHeaders},
		{kind: "magento", checked: always, baseURL: "http://localhost",
			match: func(r string) bool {
				return requiresPrefix(r, "magento/product-") || requires(r, "magento/magento2-base")
			},
			entry: func(r string) string {
				return openapiURL("/rest/default/schema?services=all", "the default store view; /rest/all would also write to every store view")(r) +
					comment("without identity: only anonymous services are listed; add identity: <an admin identity> for all",
						"base_url: Magento keeps its URL in the database (web/unsecure/base_url); set the store's URL")
			}},
		// Frameworks with a native adapter and an OpenAPI document get both: the
		// document first (it gives request bodies), the native adapter for routes
		// the document leaves out.
		{kind: "symfony", checked: always, baseURL: "http://localhost:8000",
			match: func(r string) bool {
				return requires(r, "symfony/framework-bundle") && !requires(r, "magento/magento2-base") && !requires(r, "laravel/framework")
			},
			entry: func(r string) string {
				doc := ""
				switch {
				case requires(r, "api-platform/core") || requires(r, "api-platform/symfony"):
					doc = openapiURL("/api/docs.jsonopenapi", "API Platform")(r)
				case requires(r, "nelmio/api-doc-bundle"):
					doc = openapiURL("/api/doc.json", "NelmioApiDocBundle; the path is the one in its routes config")(r)
				}
				return doc + "  - adapter: symfony            # bin/console debug:router --format=json\n"
			},
			exclude: [][2]string{
				{"/_profiler", "Symfony profiler: a development tool."},
				{"/_profiler/*", "Symfony profiler: a development tool."},
				{"/_wdt/*", "Symfony web debug toolbar: a development tool."},
				{"/_error/*", "Symfony error page previews: a development tool."},
			}},
		{kind: "spring", checked: always, baseURL: "http://localhost:8080",
			match: func(r string) bool { return contains(r, "spring-boot", jvm...) },
			entry: func(r string) string {
				if contains(r, "springdoc", jvm...) {
					return openapiURL("/v3/api-docs", "springdoc-openapi")(r)
				}
				return "  - adapter: spring             # /actuator/mappings; needs spring-boot-starter-actuator and\n" +
					"                                 # management.endpoints.web.exposure.include=mappings\n"
			},
			exclude: [][2]string{
				{"/actuator", "Spring Boot actuator: operations endpoints, not the API."},
				{"/actuator/*", "Spring Boot actuator: operations endpoints, not the API."},
				{"/error", "Spring Boot's error page."},
			},
			allow: `  - request: '*'
    path: $.timestamp
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}(Z|[+-]\d{2}:\d{2})'
    reason: Spring Boot's error body carries the time of the error. If the API has its own timestamp field, this rule allows it too; narrow request then.
`},
		{kind: "quarkus", checked: func(r string) bool { return contains(r, "smallrye-openapi", jvm...) }, baseURL: "http://localhost:8080",
			match: func(r string) bool { return contains(r, "io.quarkus", jvm...) },
			entry: func(r string) string {
				if contains(r, "smallrye-openapi", jvm...) {
					return openapiURL("/q/openapi?format=json", "quarkus-smallrye-openapi")(r)
				}
				return comment("add quarkus-smallrye-openapi (then url: /q/openapi?format=json)")
			}},
		{kind: "micronaut", checked: func(r string) bool { return micronautSpec(r) != "" }, baseURL: "http://localhost:8080",
			match: func(r string) bool { return contains(r, "io.micronaut", jvm...) },
			entry: func(r string) string {
				if spec := micronautSpec(r); spec != "" {
					return "  - adapter: openapi\n    file: " + spec + "   # written by micronaut-openapi at build time\n"
				}
				return comment("micronaut-openapi writes the document at build time; build the project, run init again,",
					"or add: - adapter: openapi", "           file: target/classes/META-INF/swagger/<name>.yml   (Gradle: build/classes/java/main/META-INF/swagger/…)")
			}},
		{kind: "aspnet", checked: always, baseURL: "http://localhost:5000",
			match: func(r string) bool { return contains(r, "Microsoft.NET.Sdk.Web", dotnet...) },
			entry: func(r string) string {
				if contains(r, "Swashbuckle", dotnet...) {
					return openapiURL("/swagger/v1/swagger.json", "Swashbuckle")(r)
				}
				return openapiURL("/openapi/v1.json", "Microsoft.AspNetCore.OpenApi (.NET 9 and later)")(r)
			}},
		{kind: "fastapi", checked: always, baseURL: "http://localhost:8000",
			match: func(r string) bool { return dependsOn(r, "fastapi", python...) },
			entry: openapiURL("/openapi.json", "")},
		{kind: "django", checked: always, baseURL: "http://localhost:8000",
			match: func(r string) bool { return contains(r, "django", "manage.py") },
			entry: func(r string) string {
				doc := ""
				if dependsOn(r, "drf-spectacular", python...) || dependsOn(r, "drf_spectacular", python...) {
					doc = openapiURL("/api/schema/", "drf-spectacular; the path is the one in your urls.py")(r)
				}
				return doc + "  - adapter: django             # manage.py shell (the .venv Python if there is one), with a script that reads urlpatterns\n"
			},
			exclude: [][2]string{
				{"/admin", "Django admin site: HTML for staff, not the API."},
				{"/admin/*", "Django admin site: HTML for staff, not the API."},
				{"*{format}*", "Django REST framework format suffixes (.json, .api): the same views as the routes without them."},
			}},
		{kind: "rails", checked: always, baseURL: "http://localhost:3000",
			match: func(r string) bool {
				return gemRails().MatchString(read(r, "Gemfile")) && exists(r, "config/routes.rb") != ""
			},
			entry: func(r string) string {
				doc := ""
				if regexp.MustCompile(`(?m)^\s*gem\s+["']rswag`).MatchString(read(r, "Gemfile")) {
					doc = openapiURL("/api-docs/v1/swagger.yaml", "rswag")(r)
				}
				return doc + "  - adapter: rails              # bin/rails routes\n"
			},
			exclude: [][2]string{
				{"/rails/*", "Rails engines (Active Storage, Action Mailbox, conductor), not the API."},
			},
			headers: `  - name: x-request-id
    format: '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'
    reason: Rails gives every request its own id.
  - name: x-runtime
    format: '\d+(\.\d+)?'
    reason: Rails reports how long the request took.
  - name: server-timing
    format: '[0-9a-z ,.;=_]+'
    reason: Rails reports timings of the request (development).
`,
			observe: "  - match: \"GET /up\"\n    reason: Rails health check; it reports the current time.\n",
		},
		{kind: "nestjs", checked: always, baseURL: "http://localhost:3000",
			match: func(r string) bool { return contains(r, `"@nestjs/core"`, node...) },
			entry: func(r string) string {
				if contains(r, `"@nestjs/swagger"`, node...) {
					return openapiURL("/api-json", "@nestjs/swagger: the path given to SwaggerModule.setup, plus -json")(r)
				}
				return comment("add @nestjs/swagger (then url: /<setup path>-json)")
			}},
		{kind: "nextjs", checked: always, baseURL: "http://localhost:3000",
			match: func(r string) bool {
				return exists(r, "next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs") != ""
			},
			entry:   func(string) string { return "  - adapter: nextjs\n" },
			allow:   nextAllow(),
			headers: nextHeaders},
		{kind: "nuxt", checked: always, baseURL: "http://localhost:3000",
			match: func(r string) bool { return exists(r, "nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs") != "" },
			entry: func(string) string { return "  - adapter: nuxt\n" },
			allow: nuxtAllow()},
	}
})

// nextAssets is the format of the Next.js asset URLs.
func nextAssets() string {
	return assetURL(`/_next/static/`, `/`, "_next") + `|` + assetURL(`static/chunks/`, `/`, "_next", "static") + `|` +
		assetURL(`%2F_next%2Fstatic%2F`, `%2F`, "_next")
}

// nextAllow holds the values Next.js changes with every build or request.
// Pages are compared by their structure; these rules leave
// out the build id and the hashed asset names, which change with every next
// build, and nonces.
func nextAllow() string {
	return `  - path: html>body>script#__NEXT_DATA__$.buildId
    format: '[\w.-]+'
    reason: Next.js build id (pages router); new with every next build.
  - path: self.__next_f$.0.b
    format: '[\w.-]+'
    reason: Next.js build id in the app router payload (RSC); new with every next build.
  - path: html>*@nonce
    format: '[A-Za-z0-9+/=_-]+'
    reason: Nonce of the Content-Security-Policy; new with every request.
  - path: self.__next_f$*.nonce
    format: '[A-Za-z0-9+/=_-]+|\$undefined'
    reason: Nonce of the Content-Security-Policy in the app router payload; new with every request.
  - path: html*
    format: '` + nextAssets() + `'
    parts: true
    reason: Next.js asset URLs carry the build id or a hash of the file, so they change with the code; the rest of the page is still compared. A part ends at the file name and never holds . or .. or another /_next/static/.
  - path: self.__next_f$*
    format: '` + nextAssets() + `'
    parts: true
    reason: The same asset URLs in the app router payload (RSC).
`
}

const nextHeaders = `  - name: etag
    format: '(W/)?"[0-9a-z]+"'
    reason: Next.js computes the ETag from the body, which is compared itself.
`

// Nuxt writes its build id into an inline script (window.__NUXT__.config)
// and hashes into the names of the files under /_nuxt/.
func nuxtAllow() string {
	return `  - path: html>body>script*
    format: 'buildId:"[\w-]+"'
    parts: true
    reason: Nuxt build id in window.__NUXT__.config; new with every nuxt build.
  - path: html*
    format: '` + assetURL(`/_nuxt/`, `/`, "_nuxt") + `'
    parts: true
    reason: Nuxt asset URLs carry a hash of the file, so they change with the code; the rest of the page is still compared. A part ends at the file name and never holds . or .. or another /_nuxt/.
  - path: html>*@nonce
    format: '[A-Za-z0-9+/=_-]+'
    reason: Nonce of the Content-Security-Policy (nuxt-security); new with every request.
`
}

// assetURL is a format for the asset URLs under prefix: directories (letters,
// digits, _, ~ and -; no dot, so never . or .., and none of the names in
// not, so a part cannot run on into a second asset URL) joined by sep, then a
// file name that does not start with a dot.
func assetURL(prefix, sep string, not ...string) string {
	return prefix + `(?:` + segmentExcept(not) + sep + `)*[\w~-][\w.~-]*`
}

// segmentExcept is a regular expression (RE2 has no lookahead) for a
// non-empty run of [\w~-] that is none of the words: a walk down the trie of
// the words, leaving it at any character the trie does not go on with.
func segmentExcept(words []string) string {
	const class = `0-9A-Za-z_~` // and -, written last
	type trie struct {
		next map[byte]*trie
		end  bool
	}
	root := &trie{next: map[byte]*trie{}, end: true} // the empty run is excluded too
	for _, w := range words {
		t := root
		for i := 0; i < len(w); i++ {
			if t.next[w[i]] == nil {
				t.next[w[i]] = &trie{next: map[byte]*trie{}}
			}
			t = t.next[w[i]]
		}
		t.end = true
	}
	var walk func(t *trie) string
	walk = func(t *trie) string {
		if len(t.next) == 0 {
			// Every character goes on; at least one must when this is a word.
			if t.end {
				return `[\w~-]+`
			}
			return `[\w~-]*`
		}
		var chars []byte
		for c := range t.next {
			chars = append(chars, c)
		}
		sort.Slice(chars, func(i, j int) bool { return chars[i] < chars[j] })
		alts := []string{`[` + without(class, chars) + `][\w~-]*`}
		for _, c := range chars {
			alts = append(alts, regexp.QuoteMeta(string(c))+walk(t.next[c]))
		}
		re := `(?:` + strings.Join(alts, `|`) + `)`
		if !t.end {
			re += `?`
		}
		return re
	}
	return walk(root)
}

// without writes the character class (ranges like 0-9A-Za-z_~, then -)
// without the given characters.
func without(class string, drop []byte) string {
	dropped := map[byte]bool{}
	for _, c := range drop {
		dropped[c] = true
	}
	var set []byte
	for i := 0; i < len(class); i++ {
		if i+2 < len(class) && class[i+1] == '-' {
			for c := class[i]; c <= class[i+2]; c++ {
				set = append(set, c)
			}
			i += 2
			continue
		}
		set = append(set, class[i])
	}
	var b strings.Builder
	for i := 0; i < len(set); {
		if dropped[set[i]] {
			i++
			continue
		}
		j := i
		for j+1 < len(set) && set[j+1] == set[j]+1 && !dropped[set[j+1]] {
			j++
		}
		if j-i >= 2 {
			b.WriteString(string(set[i]) + "-" + string(set[j]))
		} else {
			b.Write(set[i : j+1])
		}
		i = j + 1
	}
	if !dropped['-'] {
		b.WriteByte('-')
	}
	return b.String()
}

// micronautSpec finds the document micronaut-openapi wrote at build time.
func micronautSpec(root string) string {
	for _, dir := range []string{"target/classes/META-INF/swagger", "build/classes/java/main/META-INF/swagger", "build/tmp/kapt3/classes/main/META-INF/swagger"} {
		matches, _ := filepath.Glob(filepath.Join(root, dir, "*.yml"))
		if len(matches) == 0 {
			matches, _ = filepath.Glob(filepath.Join(root, dir, "*.yaml"))
		}
		if len(matches) > 0 {
			rel, _ := filepath.Rel(root, matches[0])
			return filepath.ToSlash(rel)
		}
	}
	return ""
}

// specFile finds an OpenAPI or Swagger file in the usual places.
func specFile(root string) string {
	for _, dir := range []string{"", "docs", "api", "openapi", "spec"} {
		for _, name := range []string{"openapi.yaml", "openapi.yml", "openapi.json", "swagger.yaml", "swagger.yml", "swagger.json"} {
			if exists(root, filepath.Join(dir, name)) != "" {
				return filepath.ToSlash(filepath.Join(dir, name))
			}
		}
	}
	return ""
}

// detectDir recognises the kind of project in one folder.
func detectDir(root string) detected {
	var d detected
	var b strings.Builder
	for _, det := range detectors() {
		if !det.match(root) {
			continue
		}
		d.kinds = append(d.kinds, det.kind)
		if det.checked == nil || !det.checked(root) {
			d.unchecked = append(d.unchecked, det.kind)
		}
		if d.baseURL == "" {
			d.baseURL = det.baseURL
		}
		b.WriteString(det.entry(root))
		d.excluded = append(d.excluded, det.exclude...)
		d.allow += det.allow
		d.headers += det.headers
		d.observe += det.observe
	}
	if spec := specFile(root); spec != "" {
		d.kinds = append(d.kinds, "openapi")
		fmt.Fprintf(&b, "  - adapter: openapi\n    file: %s\n", spec)
	}
	if len(d.kinds) > 0 {
		d.block = "discover:\n" + b.String() + "\ntemplates: " + templatesFor(d.kinds) + "\n"
	}
	if d.observe != "" {
		d.block += "\nobserve:\n" + d.observe
	}
	return d
}
