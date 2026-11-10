package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/secret"
)

// closedPort is a local port nothing listens on: a database or an
// application setup cannot reach, without touching anything on this
// machine.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return port
}

// setupDir is a new project folder with files (a git work tree, the
// working folder of the test), a home and a key of the test's own, and
// no terminal unless input says what is typed.
func setupDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	gitIn(t, root, "init", "-q")
	for name, content := range files {
		write(t, filepath.Join(root, name), content)
	}
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DRIFTGATE_STORE", "")
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	for _, v := range []string{"DATABASE_URL", "DB_URL", "SPRING_DATASOURCE_URL", "SPRING_PROFILES_ACTIVE", "ASPNETCORE_ENVIRONMENT", "DOTNET_ENVIRONMENT"} {
		t.Setenv(v, "")
	}
	input(t, "")
	return root
}

// input is what the user types at setup's questions; "" is no terminal.
func input(t *testing.T, typed string) {
	t.Helper()
	old := setupInput
	t.Cleanup(func() { setupInput = old })
	setupInput = func() io.Reader {
		if typed == "" {
			return nil
		}
		return strings.NewReader(typed)
	}
}

// loadState is the state: db of the project's configuration.
func loadState(t *testing.T, root string) (*config.Project, config.Component) {
	t.Helper()
	p, err := config.Load(filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p, p.State["db"]
}

// noSecret fails when a password of the fixtures shows in the output or
// in any file setup wrote.
func noSecret(t *testing.T, root string, outputs ...string) {
	t.Helper()
	for _, o := range outputs {
		if strings.Contains(o, "S3cr") {
			t.Errorf("a password in the output:\n%s", o)
		}
	}
	filepath.WalkDir(filepath.Join(root, ".driftgate"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "S3cr") {
				t.Errorf("a password in %s", path)
			}
		}
		return nil
	})
}

// TestSetupStacks: on a project of each main stack, setup --yes writes a
// configuration whose database is a reference to the project's own
// setting, never its value, and its address; it then makes the pristine
// copy, which fails here, as nothing listens on the database's port.
func TestSetupStacks(t *testing.T) {
	db, app := closedPort(t), closedPort(t)
	laravel := map[string]string{
		"artisan":       "",
		"composer.json": `{"require":{"laravel/framework":"^11.0"}}`,
		".env": "APP_URL=http://localhost\nAPP_PORT=" + app + "\nFORWARD_DB_PORT=" + db +
			"\nDB_CONNECTION=mysql\nDB_HOST=mysql\nDB_PORT=3306\nDB_DATABASE=shop\nDB_USERNAME=sail\nDB_PASSWORD=S3cr-sail\n",
		"docker-compose.yml": `services:
  laravel.test:
    image: sail-8.3/app
    ports: ['${APP_PORT:-80}:80']
  mysql:
    image: 'mysql/mysql-server:8.0'
    ports: ['${FORWARD_DB_PORT:-3306}:3306']
    environment:
      MYSQL_ROOT_PASSWORD: '${DB_PASSWORD}'
      MYSQL_DATABASE: '${DB_DATABASE}'
      MYSQL_USER: '${DB_USERNAME}'
      MYSQL_PASSWORD: '${DB_PASSWORD}'
`,
	}
	tests := []struct {
		name  string
		files map[string]string
		args  []string
		dsn   config.Secret
		want  config.Component
		url   string
		out   []string
	}{
		{name: "laravel-sail", files: laravel,
			dsn:  config.Secret{Compose: "docker-compose.yml", Key: "mysql"},
			want: config.Component{Driver: "mysql", Working: "shop", Pristine: "shop_pristine"},
			url:  "http://localhost:" + app,
			out:  []string{"found laravel", "user root (docker-compose.yml, service mysql)", "of 2 found (the application's setting names it, and its user can create the pristine copy)"}},
		{name: "aspnet", files: map[string]string{
			"Shop.Api.csproj":                `<Project Sdk="Microsoft.NET.Sdk.Web"><ItemGroup><PackageReference Include="Pomelo.EntityFrameworkCore.MySql" Version="8.0.2" /></ItemGroup></Project>`,
			"appsettings.json":               `{"ConnectionStrings": {"Default": "Server=127.0.0.1;Port=` + db + `;Database=shop;Uid=shop;Pwd=S3cr-dotnet;"}}`,
			"Properties/launchSettings.json": `{"profiles": {"http": {"commandName": "Project", "applicationUrl": "http://localhost:` + app + `"}}}`,
		},
			dsn:  config.Secret{AppSettings: "Shop.Api.csproj", Key: "ConnectionStrings:Default"},
			want: config.Component{Driver: "mysql", Working: "shop", Pristine: "shop_pristine"},
			url:  "http://localhost:" + app,
			out:  []string{"GRANT ALL PRIVILEGES ON `shop_pristine`.* TO 'shop'@'%';"}},
		{name: "spring", files: map[string]string{
			"pom.xml": "<artifactId>spring-boot-starter-web</artifactId>",
			"src/main/resources/application.properties": "server.port=" + app + "\nspring.datasource.url=jdbc:mysql://127.0.0.1:" + db +
				"/shop\nspring.datasource.username=shop\nspring.datasource.password=S3cr-spring\n",
		},
			dsn:  config.Secret{Properties: "src/main/resources/application.properties"},
			want: config.Component{Driver: "mysql", Working: "shop", Pristine: "shop_pristine"},
			url:  "http://localhost:" + app,
			out:  []string{"found spring"}},
		{name: "rails", files: map[string]string{
			"Gemfile":          "source 'https://rubygems.org'\ngem 'rails', '~> 7.1'\n",
			"config/routes.rb": "Rails.application.routes.draw do\nend\n",
			"config/database.yml": "default: &default\n  adapter: postgresql\n  host: 127.0.0.1\n  port: " + db +
				"\n  username: shop\n  password: S3cr-rails\ndevelopment:\n  <<: *default\n  database: shop_development\n",
		},
			// Rails settings give no address: --url.
			args: []string{"--url", "http://127.0.0.1:" + app},
			dsn:  config.Secret{YAML: "config/database.yml", Key: "development"},
			want: config.Component{Driver: "postgres", Working: "shop_development", Pristine: "shop_development_pristine"},
			url:  "http://127.0.0.1:" + app,
			out:  []string{"found rails", `ALTER ROLE "shop" CREATEDB;`}},
		{name: "node-prisma", files: map[string]string{
			"package.json":         `{"dependencies": {"next": "14.2.0", "@prisma/client": "5.0.0"}}`,
			"next.config.js":       "module.exports = {}\n",
			"prisma/schema.prisma": "datasource db {\n  provider = \"postgresql\"\n  url      = env(\"POSTGRES_PRISMA_URL\")\n}\n",
			".env":                 "POSTGRES_PRISMA_URL=\"postgresql://shop:S3cr-prisma@127.0.0.1:" + db + "/shop?schema=public\"\nNEXTAUTH_URL=http://localhost:" + app + "\n",
		},
			dsn:  config.Secret{Dotenv: ".env", Key: "POSTGRES_PRISMA_URL"},
			want: config.Component{Driver: "postgres", Working: "shop", Pristine: "shop_pristine"},
			url:  "http://localhost:" + app,
			out:  []string{"found nextjs"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := setupDir(t, tc.files)
			code, out, errOut := runCLI(t, append([]string{"setup", "--yes"}, tc.args...)...)
			// The pristine copy is made, and fails: nothing listens there.
			if code != ExitEnv || !strings.Contains(errOut, "seed") || !strings.Contains(out, "== pristine copy ==") {
				t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
			}
			p, c := loadState(t, root)
			if c.DSN != tc.dsn {
				t.Errorf("dsn: %+v, want %+v", c.DSN, tc.dsn)
			}
			c.DSN = config.Secret{}
			if fmt.Sprint(c) != fmt.Sprint(tc.want) {
				t.Errorf("state db: %+v, want %+v", c, tc.want)
			}
			if p.Target.BaseURL != tc.url {
				t.Errorf("base_url %s, want %s", p.Target.BaseURL, tc.url)
			}
			for _, w := range tc.out {
				if !strings.Contains(out+errOut, w) {
					t.Errorf("output without %q:\n%s\n%s", w, out, errOut)
				}
			}
			noSecret(t, root, out, errOut)
		})
	}
}

// TestSetupChoosesDatabase: of several settings, --yes takes the most
// specific one (.env.local over .env) and says so; at a terminal the user
// picks one from a numbered list.
func TestSetupChoosesDatabase(t *testing.T) {
	db := closedPort(t)
	files := map[string]string{
		".env":       "APP_URL=http://127.0.0.1:" + closedPort(t) + "\nDATABASE_URL=mysql://shop:S3cr-a@127.0.0.1:" + db + "/shop\n",
		".env.local": "DATABASE_URL=mysql://shop:S3cr-b@127.0.0.1:" + db + "/shop_local\n",
	}
	t.Run("yes", func(t *testing.T) {
		// Two .env files that name different databases: which one the
		// application reads depends on the framework and its environment.
		root := setupDir(t, files)
		code, out, errOut := runCLI(t, "setup", "--yes")
		if code != ExitConfig || !strings.Contains(errOut, "the settings name different databases") {
			t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
			t.Error("setup wrote files with settings that disagree")
		}
		noSecret(t, root, out, errOut)
	})
	t.Run("terminal", func(t *testing.T) {
		root := setupDir(t, files)
		input(t, "2\nn\n")
		code, out, errOut := runCLI(t, "setup")
		if code != ExitConfig || !strings.Contains(errOut, "2. mysql shop on") || !strings.Contains(errOut, "no pristine copy was made") {
			t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
		}
		if _, c := loadState(t, root); c.DSN.Dotenv != ".env" || c.Working != "shop" {
			t.Errorf("state db: %+v", c)
		}
	})
}

// TestSetupAsksWhatItCannotTell: a setting that does not say which server
// it is stops setup --yes with the flag to pass, and nothing is written;
// at a terminal the answer is taken, and --driver gives it too.
func TestSetupAsksWhatItCannotTell(t *testing.T) {
	db := closedPort(t)
	files := map[string]string{
		"src/Shop.Api/Shop.Api.csproj":  `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`,
		"src/Shop.Api/appsettings.json": `{"ConnectionStrings": {"Default": "Host=127.0.0.1;Port=` + db + `;Database=shop;Username=shop;Password=S3cr-x"}}`,
	}
	url := "http://127.0.0.1:" + closedPort(t)
	root := setupDir(t, files)
	code, out, errOut := runCLI(t, "setup", "--yes", "--url", url)
	if code != ExitConfig || !strings.Contains(errOut, "pass --driver with mysql, postgres, sqlite or sqlserver") {
		t.Fatalf("setup --yes: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
		t.Error("setup wrote files before it knew the driver")
	}
	input(t, "oracle\npostgres\nn\n")
	if code, out, errOut = runCLI(t, "setup", "--url", url); code != ExitConfig || !strings.Contains(errOut, "mysql, postgres, sqlite or sqlserver") {
		t.Fatalf("setup at a terminal: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); c.Driver != "postgres" || c.DSN.AppSettings != "src/Shop.Api/Shop.Api.csproj" {
		t.Errorf("state db: %+v", c)
	}
	noSecret(t, root, out, errOut)

	// No address in the settings: --yes stops with --url.
	setupDir(t, map[string]string{".env": "DATABASE_URL=mysql://shop:S3cr@127.0.0.1:" + db + "/shop\n"})
	if code, out, errOut := runCLI(t, "setup", "--yes"); code != ExitConfig || !strings.Contains(errOut, "pass --url <address>") {
		t.Fatalf("setup without an address: %d\n%s\n%s", code, out, errOut)
	}
}

// TestSetupEnvironmentOnly: a database only this shell's environment sets
// (Spring's SPRING_DATASOURCE_*) cannot be referenced: the configuration
// takes the DSN from DG_DB_DSN, and setup stops until it is set.
func TestSetupEnvironmentOnly(t *testing.T) {
	root := setupDir(t, map[string]string{"pom.xml": "<artifactId>spring-boot-starter-web</artifactId>"})
	t.Setenv("SPRING_DATASOURCE_URL", "jdbc:mysql://127.0.0.1:"+closedPort(t)+"/shop")
	code, out, errOut := runCLI(t, "setup", "--yes", "--url", "http://127.0.0.1:"+closedPort(t))
	if code != ExitConfig || !strings.Contains(errOut, "environment variable DG_DB_DSN is not set") || !strings.Contains(errOut, "driftgate setup --yes") ||
		!strings.Contains(out, "SPRING_DATASOURCE_URL") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); c.DSN != (config.Secret{Env: "DG_DB_DSN"}) || c.Working != "shop" {
		t.Errorf("state db: %+v", c)
	}
}

// TestSetupPristineAsked: the pristine copy creates a database, so setup
// asks first (default no). No answer — no terminal — stops it, as does a
// no; a yes makes it (here it fails: nothing listens on the port, which is
// no reason to grant rights, so no GRANT is proposed).
func TestSetupPristineAsked(t *testing.T) {
	files := map[string]string{".env": "APP_URL=http://127.0.0.1:" + closedPort(t) +
		"\nDB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=" + closedPort(t) + "\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n"}
	setupDir(t, files)
	code, out, errOut := runCLI(t, "setup")
	if code != ExitConfig || !strings.Contains(errOut, "no terminal to ask it on: Make the pristine copy now? It creates the database shop_pristine") ||
		!strings.Contains(errOut, "pass --yes to make it") || strings.Contains(out, "seed:") {
		t.Fatalf("without a terminal: %d\n%s\n%s", code, out, errOut)
	}
	input(t, "\n")
	if code, out, errOut = runCLI(t, "setup"); code != ExitConfig || !strings.Contains(errOut, "driftgate seed --skip-commands, then driftgate setup") {
		t.Fatalf("no: %d\n%s\n%s", code, out, errOut)
	}
	input(t, "y\n")
	if code, out, errOut = runCLI(t, "setup"); code != ExitEnv || !strings.Contains(errOut, "seed of db failed") || strings.Contains(errOut, "GRANT") || strings.Contains(out, "check by hand:\n  - the pristine copy needs") {
		t.Fatalf("yes: %d\n%s\n%s", code, out, errOut)
	}
}

// setupDemo is a project on the demo API without a database: its address
// in .env, its routes in openapi.json.
func setupDemo(t *testing.T, url string) string {
	t.Helper()
	return setupDir(t, map[string]string{
		".env": "APP_URL=" + url + "\n",
		"openapi.json": `{"openapi": "3.0.0", "info": {"title": "shop", "version": "1"}, "paths": {
 "/api/news": {"get": {"responses": {"200": {"description": "ok"}}}},
 "/api/news/{id}": {"get": {"parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}}], "responses": {"200": {"description": "ok"}}}},
 "/api/contact": {"get": {"responses": {"200": {"description": "ok"}}}}}}`,
	})
}

// TestSetupAppNotAnswering: setup stops before the capture when the
// application does not answer, with how to go on; the files it wrote stay.
func TestSetupAppNotAnswering(t *testing.T) {
	url := "http://127.0.0.1:" + closedPort(t)
	root := setupDemo(t, url)
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitEnv || !strings.Contains(errOut, "the application does not answer at "+url) || !strings.Contains(errOut, "then run driftgate setup --yes: it goes on from here") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate", "driftgate.yaml")); err != nil {
		t.Error(err)
	}
}

// TestSetupToBaseline: from zero to the first baseline, with the key made
// in .driftgate/; a route without a value for its parameter is left for
// the user to check, and does not fail the setup. A second run changes
// nothing: configuration, key and baseline stay byte for byte.
func TestSetupToBaseline(t *testing.T) {
	srv := httptest.NewServer(demo.New("v1"))
	defer srv.Close()
	root := setupDemo(t, srv.URL)
	t.Setenv(secret.EnvKey, "")
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitClean || !strings.Contains(out, "baseline local made") || !strings.Contains(out, "created a key for") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	for _, w := range []string{"framework:  openapi", "app:        " + srv.URL, "routes:     3 found: 2 covered, 0 excluded, 1 uncovered", "uncovered routes (1): driftgate coverage lists them", "baseline:   local in .driftgate/baselines/local", "database:   none",
		"mode:       freeze: every drift fails, and driftgate accept is refused; mode: evolve in .driftgate/driftgate.yaml lets a drift be accepted with a reason"} {
		if !strings.Contains(out, w) {
			t.Errorf("summary without %q:\n%s", w, out)
		}
	}
	before := fileSums(t, filepath.Join(root, ".driftgate"))
	code, out, errOut = runCLI(t, "setup", "--yes")
	if code != ExitClean || !strings.Contains(out, "is there already; setup goes on with it") || !strings.Contains(out, "the key is set up already (from .driftgate/key); kept") ||
		!strings.Contains(out, "baseline local is there already") || strings.Contains(out, "== capture ==") {
		t.Fatalf("second setup: %d\n%s\n%s", code, out, errOut)
	}
	after := fileSums(t, filepath.Join(root, ".driftgate"))
	for name, sum := range before {
		if strings.HasPrefix(name, "store/") {
			continue // the store's lock and the last comparisons
		}
		if after[name] != sum {
			t.Errorf("%s changed", name)
		}
	}
}

// fileSums are the SHA-256 of the files under dir, by slash path.
func fileSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out[filepath.ToSlash(rel)] = sum(mustRead(t, path))
		}
		return nil
	})
	return out
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestInitDatabaseFromSettings: init writes the database the project's
// settings give as state:, a reference to them, commented out: a run
// after init must not create a database unasked (setup asks). Uncommented,
// the block is the state.
func TestInitDatabaseFromSettings(t *testing.T) {
	root := setupDir(t, map[string]string{
		".env": "APP_URL=http://shop.test\nDB_CONNECTION=pgsql\nDB_HOST=127.0.0.1\nDB_PORT=5432\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n",
	})
	code, out, errOut := runCLI(t, "init")
	if code != ExitClean || !strings.Contains(out, "state: db postgres shop on 127.0.0.1:5432, user shop (.env), commented out") || !strings.Contains(out, "base_url http://shop.test from .env") {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	if p, c := loadState(t, root); c.Driver != "" || p.Target.BaseURL != "http://shop.test" {
		t.Errorf("state db on after init: %+v, base_url %s", c, p.Target.BaseURL)
	}
	path := filepath.Join(root, ".driftgate", "driftgate.yaml")
	on := regexp.MustCompile(`(?m)^# (state:|  db:|    (driver|dsn|working|pristine):)`).ReplaceAllString(mustRead(t, path), "$1")
	write(t, path, on)
	if _, c := loadState(t, root); c.DSN != (config.Secret{Dotenv: ".env"}) || c.Driver != "postgres" || c.Working != "shop" || c.Pristine != "shop_pristine" {
		t.Errorf("state db uncommented: %+v", c)
	}
	noSecret(t, root, out, errOut)
}

// TestSetupTakesTheApplicationsDatabase: a compose service of another
// database comes after the application's own setting, root user or not:
// --yes takes the database the application uses, and lists the other.
// A compose service of the same server and database comes first.
func TestSetupTakesTheApplicationsDatabase(t *testing.T) {
	app, other := closedPort(t), closedPort(t)
	env := "APP_URL=http://127.0.0.1:" + closedPort(t) + "\nDB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=" + app + "\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr-a\n"
	compose := func(port, db string) string {
		return "services:\n  mysql:\n    image: mysql:8.0\n    ports: ['127.0.0.1:" + port + ":3306']\n    environment:\n      MYSQL_ROOT_PASSWORD: S3cr-root\n      MYSQL_DATABASE: " + db + "\n"
	}
	root := setupDir(t, map[string]string{".env": env, "compose.yaml": compose(other, "other_project")})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if _, c := loadState(t, root); c.Working != "shop" || c.DSN.Dotenv != ".env" {
		t.Errorf("setup --yes chose %+v\nexit %d\n%s\n%s", c, code, out, errOut)
	}
	if raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml")); !strings.Contains(raw, "# also found: mysql other_project on 127.0.0.1:"+other) {
		t.Errorf("the compose service is not listed:\n%s", raw)
	}
	root = setupDir(t, map[string]string{".env": env, "compose.yaml": compose(app, "shop")})
	runCLI(t, "setup", "--yes")
	if _, c := loadState(t, root); c.DSN.Compose != "compose.yaml" || c.Working != "shop" {
		t.Errorf("the compose service of the application's database is not first: %+v", c)
	}
}

// TestSetupSettingsDisagree: two kinds of settings that name different
// databases (the environment and .env) stop --yes, naming both, before
// anything is written; at a terminal setup asks, saying they differ.
func TestSetupSettingsDisagree(t *testing.T) {
	root := setupDir(t, map[string]string{".env": "APP_URL=http://127.0.0.1:" + closedPort(t) +
		"\nDB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=" + closedPort(t) + "\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n"})
	t.Setenv("DATABASE_URL", "mysql://other:S3cr-env@127.0.0.1:"+closedPort(t)+"/other")
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "the settings name different databases") || !strings.Contains(errOut, "mysql shop on") ||
		!strings.Contains(errOut, "mysql other on") {
		t.Fatalf("setup --yes: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
		t.Error("setup wrote files with settings that disagree")
	}
	noSecret(t, root, out, errOut)
	input(t, "2\nn\n")
	if _, _, errOut := runCLI(t, "setup"); !strings.Contains(errOut, "The settings name different databases: which one does the application use?") {
		t.Errorf("at a terminal:\n%s", errOut)
	}
	if _, c := loadState(t, root); c.DSN.Env != "DATABASE_URL" || c.Working != "other" {
		t.Errorf("state db: %+v", c)
	}
}

// TestSetupInitThenRun: init leaves state: commented out, its comments in
// line, so a run after it creates no database unasked. setup asks to turn
// it on (no terminal: it stops), and --yes turns it on in place, its
// comments kept, before the pristine copy.
func TestSetupInitThenRun(t *testing.T) {
	root := setupDir(t, map[string]string{
		".env": "APP_URL=http://127.0.0.1:" + closedPort(t) +
			"\nDB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_PORT=" + closedPort(t) + "\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n",
		"openapi.json": `{"openapi":"3.0.0","info":{"title":"s","version":"1"},"paths":{"/a":{"get":{"responses":{"200":{"description":"ok"}}}}}}`,
	})
	if code, out, errOut := runCLI(t, "init"); code != ExitClean {
		t.Fatalf("init: %d\n%s\n%s", code, out, errOut)
	}
	if raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml")); !strings.Contains(raw, "\n#     # exclude: [sessions, cache, jobs]") {
		t.Errorf("a comment inside the commented-out block is not in line:\n%s", raw)
	}
	if code, out, errOut := runCLI(t, "run"); strings.Contains(out, "== seed ==") {
		t.Errorf("run after init seeds unasked: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut := runCLI(t, "setup")
	if code != ExitConfig || !strings.Contains(errOut, "Turn on state: in .driftgate/driftgate.yaml") || strings.Contains(out, "== discover ==") {
		t.Fatalf("setup without a terminal: %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = runCLI(t, "setup", "--yes")
	if code != ExitEnv || !strings.Contains(out, "turned on state: in .driftgate/driftgate.yaml") || !strings.Contains(errOut, "seed of db failed") || strings.Contains(out, "== discover ==") {
		t.Fatalf("setup --yes: %d\n%s\n%s", code, out, errOut)
	}
	raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if _, c := loadState(t, root); c.DSN.Dotenv != ".env" || c.Working != "shop" ||
		!strings.Contains(raw, "    # exclude: [sessions, cache, jobs]") || !strings.Contains(raw, "# found in the project's settings; turned on by driftgate setup\nstate:") {
		t.Errorf("state db %+v:\n%s", c, raw)
	}
}

// TestSetupStopsWithoutState: a configuration without state: while the
// project's settings name a database stops setup before routes and
// captures, which would run without restoring it.
func TestSetupStopsWithoutState(t *testing.T) {
	root := setupDemo(t, "http://127.0.0.1:"+closedPort(t))
	write(t, filepath.Join(root, ".env"), mustRead(t, filepath.Join(root, ".env"))+"DATABASE_URL=mysql://shop:S3cr@127.0.0.1:"+closedPort(t)+"/shop\n")
	write(t, filepath.Join(root, ".driftgate", "driftgate.yaml"), "version: 1\nproject: shop\nbaselines: local\ntarget:\n  base_url: http://127.0.0.1:1\n")
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "has no state: database: requests would run without restoring it") || strings.Contains(out, "== discover ==") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	noSecret(t, root, out, errOut)
}

// TestSetupGrantOnlyWhenDenied: the GRANT is proposed for the errors of a
// user without the right, not for any failed seed.
func TestSetupGrantOnlyWhenDenied(t *testing.T) {
	for msg, want := range map[string]bool{
		"seed of db failed: Error 1044 (42000): Access denied for user 'shop'@'%' to database 'shop_pristine'": true,
		"seed of db failed: Error 1142 (42000): CREATE command denied":                                         true,
		"seed of db failed: ERROR: permission denied to create database (SQLSTATE 42501)":                      true,
		"seed of db failed: dial tcp 127.0.0.1:5999: connect: connection refused":                              false,
		"seed of db failed: context canceled":                                                                  false,
	} {
		if denied().MatchString(msg) != want {
			t.Errorf("%s: %v", msg, !want)
		}
	}
}

// TestSetupChosenNotAlsoFound: the setting chosen is not listed among the
// others, also when --driver completed it.
func TestSetupChosenNotAlsoFound(t *testing.T) {
	root := setupDir(t, map[string]string{
		"Shop.Api.csproj":  `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`,
		"appsettings.json": `{"ConnectionStrings": {"Default": "Host=127.0.0.1;Port=` + closedPort(t) + `;Database=shop;Username=shop;Password=S3cr-x"}}`,
	})
	runCLI(t, "setup", "--yes", "--driver", "postgres", "--url", "http://127.0.0.1:"+closedPort(t))
	if raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml")); strings.Contains(raw, "# also found:") {
		t.Errorf("the chosen setting is listed as also found:\n%s", raw)
	}
}

// TestSetupPostgresOwnDatabase: a compose PostgreSQL without POSTGRES_DB
// names the server's own database, postgres, which DriftGate creates the
// pristine copy from: never the working database. --yes writes state:
// commented out, with what to set, and goes on.
func TestSetupPostgresOwnDatabase(t *testing.T) {
	root := setupDir(t, map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:17\n    ports: ['127.0.0.1:" + closedPort(t) +
		":5432']\n    environment:\n      POSTGRES_PASSWORD: S3cr-pg\n"})
	code, out, errOut := runCLI(t, "setup", "--yes", "--url", "http://127.0.0.1:"+closedPort(t))
	if _, c := loadState(t, root); c.Working != "" {
		t.Errorf("state db on: %+v", c)
	}
	raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if !strings.Contains(raw, "POSTGRES_DB") || !strings.Contains(raw, "# set working (the database the application uses), then uncomment:") ||
		!strings.Contains(out, "set working: and uncomment it") {
		t.Errorf("exit %d\n%s\n%s\n%s", code, out, errOut, raw)
	}
}

// TestSetupNoSecret: a password in a DSN's query or in libpq's keyword
// form, or a key that cannot be read, never shows.
func TestSetupNoSecret(t *testing.T) {
	for _, env := range []string{
		"DATABASE_URL=postgres://shop@127.0.0.1:5999/shop?password=S3cr-q&sslmode=disable\n",
		"DATABASE_URL=mysql://shop:S3cr-p@127.0.0.1:5999/shop?tls=false\n",
		"DATABASE_URL=\"host=127.0.0.1 port=5999 user=shop password=S3cr-kv dbname=shop\"\n",
	} {
		root := setupDir(t, map[string]string{".env": env + "APP_URL=http://127.0.0.1:" + closedPort(t) + "\n"})
		for range 2 {
			_, out, errOut := runCLI(t, "setup", "--yes")
			noSecret(t, root, out, errOut)
		}
	}
	setupDir(t, map[string]string{".env": "APP_URL=http://127.0.0.1:" + closedPort(t) + "\n"})
	t.Setenv(secret.EnvKey, "S3cr-not-base64!!")
	if _, out, errOut := runCLI(t, "setup", "--yes"); strings.Contains(out+errOut, "S3cr") {
		t.Errorf("the key shows:\n%s\n%s", out, errOut)
	}
}

// TestSetupControlCharacters: a database name with a line break (%0A in a
// URL) would add its own keys to driftgate.yaml: the setting is left out,
// with a note that names the file, never the value; names are quoted.
func TestSetupControlCharacters(t *testing.T) {
	root := setupDir(t, map[string]string{".env": "DATABASE_URL=mysql://shop:S3cr@127.0.0.1:5999/shop%0A    allow_remote:%20true\nAPP_URL=http://127.0.0.1:" + closedPort(t) + "\n"})
	code, out, errOut := runCLI(t, "setup", "--yes")
	raw := mustRead(t, filepath.Join(root, ".driftgate", "driftgate.yaml"))
	if regexp.MustCompile(`(?m)^\s*allow_remote: true`).MatchString(raw) || !strings.Contains(out, `".env": a value of the database setting holds a control character`) {
		t.Errorf("exit %d\n%s\n%s\n%s", code, out, errOut, raw)
	}
	var back struct {
		State map[string]config.Component `yaml:"state"`
	}
	if got := stateBlock(appconfDatabase("shop: #x"), true, appconf.Tables{}); yaml.Unmarshal([]byte(got), &back) != nil || back.State["db"].Working != "shop: #x" {
		t.Errorf("the name is not quoted:\n%s", got)
	}
}

// TestSetupTargets: with targets: (a service in a subfolder) --url is the
// address of the single service; with several services it is refused,
// and a framework's default port is never taken unasked.
func TestSetupTargets(t *testing.T) {
	url := "http://127.0.0.1:" + closedPort(t)
	root := setupDir(t, map[string]string{"src/Shop.Api/Shop.Api.csproj": `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`})
	code, out, errOut := runCLI(t, "setup", "--yes", "--url", url)
	if p, _ := loadState(t, root); len(p.Targets) != 1 || p.Targets[p.TargetNames()[0]].BaseURL != url {
		t.Errorf("targets %+v: exit %d\n%s\n%s", p.Targets, code, out, errOut)
	}
	setupDir(t, map[string]string{"src/Shop.Api/Shop.Api.csproj": `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>`})
	if code, out, errOut := runCLI(t, "setup", "--yes"); code != ExitConfig || !strings.Contains(errOut, "only the framework's default port (http://localhost:5000), which setup does not take unasked: pass --url") {
		t.Errorf("default port: %d\n%s\n%s", code, out, errOut)
	}
	setupDir(t, map[string]string{
		"backend/artisan":          "",
		"backend/composer.json":    `{"require":{"laravel/framework":"^12.0"}}`,
		"frontend/next.config.mjs": "export default {}\n",
	})
	if code, out, errOut := runCLI(t, "setup", "--yes", "--url", url); code != ExitConfig || !strings.Contains(errOut, "--url is the address of a project with one service, and this one has 2") {
		t.Errorf("several services: %d\n%s\n%s", code, out, errOut)
	}
}

func appconfDatabase(name string) appconf.Database {
	return appconf.Database{Driver: "mysql", Name: name, File: ".env", Ref: appconf.Ref{Source: appconf.Dotenv, File: ".env"}}
}

// TestSetupHostNeeded: MySQL's localhost is the local socket for PHP's
// client, TCP for DriftGate's: setup --yes stops with the host: to write,
// at a terminal it asks.
func TestSetupHostNeeded(t *testing.T) {
	db := closedPort(t)
	root := setupDir(t, map[string]string{".env": "APP_URL=http://127.0.0.1:" + closedPort(t) +
		"\nDB_CONNECTION=mysql\nDB_HOST=localhost\nDB_PORT=" + db + "\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=S3cr\n"})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "dsn: {dotenv: .env, host: '127.0.0.1:"+db+"'}") {
		t.Fatalf("setup --yes: %d\n%s\n%s", code, out, errOut)
	}
	input(t, "localhost:x\n127.0.0.1:"+db+"\nn\n")
	code, out, errOut = runCLI(t, "setup")
	if _, c := loadState(t, root); c.DSN.Host != "127.0.0.1:"+db {
		t.Fatalf("state db: %+v\n%d\n%s\n%s", c, code, out, errOut)
	}
	noSecret(t, root, out, errOut)
}

func TestDescribeAndServer(t *testing.T) {
	d := appconf.Database{Driver: appconf.MySQL, Name: "shop", Host: "127.0.0.1", User: "Port=3307;Database=shop;User=root;Password=Tr0ub4dor&3", File: "appsettings.json", Key: "ConnectionStrings:Default", Ref: appconf.Ref{Source: appconf.AppSettings}}
	if got := describe(d); strings.Contains(got, "Tr0ub") || !strings.Contains(got, "user (not shown") {
		t.Errorf("describe: %s", got)
	}
	a := appconf.Database{Driver: appconf.MySQL, Host: "localhost", Name: "shop"}
	b := appconf.Database{Driver: appconf.MySQL, Host: "127.0.0.1", Name: "shop"}
	if server(a) == server(b) {
		t.Error("MySQL localhost (the socket) taken for 127.0.0.1")
	}
	a.Driver, b.Driver = appconf.Postgres, appconf.Postgres
	if server(a) != server(b) {
		t.Error("PostgreSQL localhost (TCP) not taken for 127.0.0.1")
	}
}

// TestSetupSummaryPristine: the summary says that the data as it was
// became the pristine copy, and the mode.
func TestSetupSummaryPristine(t *testing.T) {
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values", Target: config.Target{BaseURL: "http://127.0.0.1:1"}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s := &setup{a: &asker{yes: true}, out: &out, errOut: &out, copied: "the data in the database as it was now: every capture starts from it; after the data changes, driftgate seed --skip-commands takes it as it is again (driftgate seed runs the seed: commands first)"}
	s.summary(p, detected{})
	for _, w := range []string{"pristine:   the data in the database as it was now: every capture starts from it", "mode:       evolve: a drift may be accepted with a reason (driftgate accept)"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("summary without %q:\n%s", w, out.String())
		}
	}
}
