package appconf

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/dbdsn"
)

// lookup is an environment of a test.
func lookup(m map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// short writes a database as a test compares it.
func short(d Database) string {
	s := fmt.Sprintf("%s %s:%s/%s %s <- %s %s %s", d.Driver, d.Host, d.Port, d.Name, d.User, d.Ref.Source, d.Ref.File, d.Ref.Key)
	if d.Ref.Host != "" {
		s += " @" + d.Ref.Host
	}
	return s
}

// unixSecrets makes the user secrets of the test be read from
// $HOME/.microsoft/usersecrets, also on Windows.
func unixSecrets(t *testing.T) {
	g := goos
	goos = "linux"
	t.Cleanup(func() { goos = g })
}

func TestFind(t *testing.T) {
	unixSecrets(t)
	home, _ := filepath.Abs("testdata/usersecrets/home")
	tests := []struct {
		dir   string
		env   map[string]string
		dbs   []string
		urls  []string
		notes []string
		// the DSN of each database's Ref, with the component's driver
		dsns []string
	}{
		{
			dir:  "laravel55",
			dbs:  []string{"mysql 127.0.0.1:3306/shop shop <- dotenv .env "},
			urls: []string{"http://shop.test .env APP_URL"},
			dsns: []string{"mysql://shop:S3cr%23et%20with%20spaces@127.0.0.1:3306/"},
		},
		{
			dir: "sail",
			dbs: []string{
				"mysql 127.0.0.1:33060/shop sail <- dotenv .env  @compose:mysql",
				"mysql 127.0.0.1:33060/shop root <- compose docker-compose.yml mysql",
			},
			urls: []string{"http://localhost .env APP_URL", "http://localhost:8088 docker-compose.yml services.laravel.test.ports"},
			dsns: []string{"mysql://sail:S3cr-sail@127.0.0.1:33060/", "mysql://root:S3cr-sail@127.0.0.1:33060/"},
		},
		{
			dir: "symfony",
			dbs: []string{
				"mysql 127.0.0.1:3307/app app <- dotenv .env.local DATABASE_URL",
				"postgres 127.0.0.1:5432/app app <- dotenv .env DATABASE_URL",
			},
			dsns: []string{"mysql://app:S3cr%40symfony@127.0.0.1:3307/", "postgres://app:S3cr-symfony-default@127.0.0.1:5432/"},
		},
		{
			dir:  "magento",
			dbs:  []string{"mysql 127.0.0.1:3308/magento magento <- php app/etc/env.php db.connection.default"},
			dsns: []string{"mysql://magento:S3cr%27magento@127.0.0.1:3308/"},
		},
		{
			dir: "dotnet",
			dbs: []string{
				"mysql 127.0.0.1:3306/shop_dev shop <- appsettings src/Shop.Api/Shop.Api.csproj ConnectionStrings:Default",
				"mysql reports.internal:/reports reader <- appsettings src/Shop.Api/Shop.Api.csproj ConnectionStrings:Reports",
			},
			urls: []string{"http://localhost:5080 src/Shop.Api/Properties/launchSettings.json applicationUrl"},
			dsns: []string{"mysql://shop:S3cr-dotnet-dev@127.0.0.1:3306/?ssl-mode=DISABLED", "mysql://reader:S3cr;dotnet%27reports@reports.internal:3306/"},
		},
		{
			dir: "dotnet",
			env: map[string]string{"HOME": home, "ConnectionStrings__Default": "Server=10.0.0.5;Database=shop;Uid=envuser;Pwd=S3cr-env"},
			dbs: []string{
				"mysql 127.0.0.1:3306/shop_dev shop <- appsettings src/Shop.Api/Shop.Api.csproj ConnectionStrings:Default",
				"mysql 10.0.0.5:/shop envuser <- env  ConnectionStrings__Default",
				"mysql reports.internal:/reports reader <- appsettings src/Shop.Api/Shop.Api.csproj ConnectionStrings:Reports",
			},
			urls: []string{"http://localhost:5080 src/Shop.Api/Properties/launchSettings.json applicationUrl"},
			dsns: []string{"mysql://shop:S3cr-usersecrets@127.0.0.1:3306/", "mysql://envuser:S3cr-env@10.0.0.5:3306/", "mysql://reader:S3cr;dotnet%27reports@reports.internal:3306/"},
		},
		{
			dir:   "dotnet-pg",
			dbs:   []string{"postgres localhost:5432/app app <- appsettings Api/Api.csproj ConnectionStrings:AppDb"},
			notes: []string{"Legacy/Legacy.csproj: ConnectionStrings:DefaultConnection names SQL Server; DriftGate restores MySQL, MariaDB and PostgreSQL"},
			dsns:  []string{"postgres://app:S3cr-npgsql@localhost:5432/"},
		},
		{
			dir: "spring",
			// application-dev.yml is no active profile's: not offered.
			dbs:  []string{"mysql localhost:3306/shop shop <- properties src/main/resources/application.properties "},
			urls: []string{"http://localhost:8081 src/main/resources/application.properties server.port"},
			dsns: []string{"mysql://shop:S3cr-spring@localhost:3306/?ssl-mode=REQUIRED"},
		},
		{
			dir:  "spring",
			env:  map[string]string{"DB_USER": "fromenv", "DB_PASSWORD": "S3cr-env", "SPRING_PROFILES_ACTIVE": "dev"},
			dbs:  []string{"mysql 127.0.0.1:3310/shop_dev fromenv <- properties src/main/resources/application.properties ", "mysql 127.0.0.1:3310/shop_dev fromenv <- properties src/main/resources/application-dev.yml "},
			urls: []string{"http://localhost:8081 src/main/resources/application.properties server.port"},
			dsns: []string{"mysql://fromenv:S3cr-spring-dev@127.0.0.1:3310/", "mysql://fromenv:S3cr-spring-dev@127.0.0.1:3310/"},
		},
		{
			dir:  "quarkus",
			dbs:  []string{"postgres localhost:5433/quarkus_dev quarkus <- properties src/main/resources/application.properties "},
			urls: []string{"http://localhost:8090 src/main/resources/application.properties quarkus.http.port"},
			dsns: []string{"postgres://quarkus:S3cr-quarkus@localhost:5433/"},
		},
		{
			dir:  "micronaut",
			dbs:  []string{"postgres localhost:5432/shop shop <- properties src/main/resources/application.yml "},
			dsns: []string{"postgres://shop:S3cr-micronaut@localhost:5432/"},
		},
		{
			dir:  "rails",
			dbs:  []string{"postgres localhost:/shop_development rails <- yaml config/database.yml development"},
			dsns: []string{"postgres://rails:S3cr-rails@localhost:5432/"},
		},
		{
			dir:  "rails",
			env:  map[string]string{"DB_HOST": "db.local", "DB_USER": "u"},
			dbs:  []string{"postgres db.local:/shop_development u <- yaml config/database.yml development"},
			dsns: []string{"postgres://u:S3cr-rails@db.local:5432/"},
		},
		{
			dir:  "rails-multi",
			dbs:  []string{"mysql 127.0.0.1:/app_development root <- yaml config/database.yml development.primary"},
			dsns: []string{"mysql://root:S3cr-rails-multi@127.0.0.1:3306/"},
		},
		{
			dir:  "django",
			dbs:  []string{"postgres localhost:5432/shop shop <- python shop/settings.py default"},
			dsns: []string{"postgres://shop:S3cr-django@localhost:5432/"},
		},
		{
			dir:   "django-env",
			notes: []string{"app/settings.py: default.NAME is computed by Python code; DriftGate does not run it (put the DSN in an environment variable and use dsn: { env: NAME })"},
		},
		{
			dir:  "prisma",
			dbs:  []string{"postgres localhost:5432/mydb johndoe <- dotenv .env POSTGRES_PRISMA_URL"},
			urls: []string{"http://localhost:3000 .env NEXTAUTH_URL"},
			dsns: []string{"postgres://johndoe:S3cr-prisma@localhost:5432/?sslmode=disable"},
		},
		{
			dir: "gosvc",
			dbs: []string{
				"mysql 127.0.0.1:3306/svc svc <- dotenv .env DATABASE_URL",
				"postgres localhost:5434/svc svc <- yaml config.yaml ",
			},
			dsns: []string{"svc:S3cr-go-env@tcp(127.0.0.1:3306)/svc?parseTime=true", "postgres://svc:S3cr-go-yaml@localhost:5434/"},
		},
		{
			dir:  "compose",
			dbs:  []string{"postgres 127.0.0.1:55432/shop_dev shop <- compose compose.yaml db"},
			urls: []string{"http://localhost:8089 compose.yaml services.web.ports"},
			dsns: []string{"postgres://shop:S3cr-compose@127.0.0.1:55432/"},
		},
		{
			dir:  "compose",
			env:  map[string]string{"PG_PORT": "6543"},
			dbs:  []string{"postgres 127.0.0.1:6543/shop_dev shop <- compose compose.yaml db"},
			urls: []string{"http://localhost:8089 compose.yaml services.web.ports"},
			dsns: []string{"postgres://shop:S3cr-compose@127.0.0.1:6543/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			root := filepath.Join("testdata", tt.dir)
			found := Find(root, lookup(tt.env))
			var dbs, urls, notes []string
			for _, d := range found.Databases {
				dbs = append(dbs, short(d))
			}
			for _, u := range found.AppURLs {
				urls = append(urls, u.URL+" "+u.File+" "+u.Key)
			}
			for _, n := range found.Notes {
				notes = append(notes, n.File+": "+n.Text)
			}
			for _, c := range []struct {
				name      string
				got, want []string
			}{{"databases", dbs, tt.dbs}, {"urls", urls, tt.urls}, {"notes", notes, tt.notes}} {
				if !reflect.DeepEqual(c.got, c.want) {
					t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
				}
			}
			if len(found.Databases) != len(tt.dsns) {
				return
			}
			for i, d := range found.Databases {
				dsn, err := d.Ref.DSN(root, d.Driver, lookup(tt.env))
				if err != nil {
					t.Errorf("%s: %v", short(d), err)
					continue
				}
				if dsn != tt.dsns[i] {
					t.Errorf("%s: DSN\n got %s\nwant %s", short(d), dsn, tt.dsns[i])
				}
				// The drivers of DriftGate take it.
				if _, err := dbdsn.Server(d.Driver, dsn); err != nil {
					t.Errorf("%s: dbdsn: %v", short(d), err)
				}
			}
		})
	}
}

// secrets are the passwords of the samples: none may appear in what Find
// returns or in any error.
func secrets(t *testing.T) []string {
	t.Helper()
	return []string{"S3cr", "S3cr%", "3cr-", "with spaces"}
}

func TestNothingFoundHoldsAPassword(t *testing.T) {
	unixSecrets(t)
	home, _ := filepath.Abs("testdata/usersecrets/home")
	env := lookup(map[string]string{"HOME": home, "SPRING_DATASOURCE_URL": "jdbc:mysql://h:3306/db?password=S3cr-spring-env", "SPRING_DATASOURCE_USERNAME": "u", "SPRING_DATASOURCE_PASSWORD": "S3cr-x", "DATABASE_URL": "mysql://u:S3cr-env@h/db"})
	dirs, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		found := Find(filepath.Join("testdata", d.Name()), env)
		out := fmt.Sprintf("%#v %+v %v", found, found, found)
		for _, s := range secrets(t) {
			if strings.Contains(out, s) {
				t.Errorf("%s: Find returned %q:\n%s", d.Name(), s, out)
			}
		}
	}
}

func TestErrorsHoldNoValue(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "DB_CONNECTION=pgsql\nDB_HOST=h\nDB_PASSWORD=\"S3cr-unterminated\n")
	write("url.env", "DATABASE_URL=mysql://u:S3cr:@x@h:3306/db\nOTHER=S3cr-other\nBAD=sqlsrv://u:S3cr@h\nPORT=mysql://u:S3cr-p@h:S3cr/db\n")
	write("laravel.env", "DB_CONNECTION=pgsql\nDB_HOST=h\nDB_PORT=S3cr-port\nDB_PASSWORD=S3cr-pw\n")
	write("appsettings.json", `{"ConnectionStrings": {"A": "Server=h;Pwd='S3cr-unterminated", "B": "S3cr-novalue", "C": "Server=h;Pwd=S3cr-pg;Trusted_Connection=yes"}} `)
	write("broken.json", `{"ConnectionStrings": {"A": "S3cr-broken"`)
	write("app.properties", "spring.datasource.url=jdbc:mysql://h/db\nspring.datasource.password=${DB_SECRET_MISSING}\n")
	write("database.yml", "development:\n  adapter: mysql2\n  password: <%= Rails.application.credentials.S3cr %>\n")
	write("bad.yml", "development: [S3cr-bad: {\n")
	write("env.php", "<?php return ['db' => ['connection' => ['default' => ['host' => 'h', 'password' => \"S3cr-$x\"]]]];")
	write("settings.py", "DATABASES = {'default': {'ENGINE': 'S3cr-engine', 'PASSWORD': 'S3cr-py'}}\n")
	write("compose.yaml", "services:\n  db:\n    image: mysql:8\n    environment:\n      MYSQL_ROOT_PASSWORD: S3cr-c\n")
	write("compose2.yaml", "services:\n  db:\n    image: mysql:8\n    environment:\n      MYSQL_ROOT_PASSWORD: ${ROOT_PW:?S3cr-required}\n")
	refs := []Ref{
		{Source: Dotenv, File: ".env"},
		{Source: Dotenv, File: "url.env", Key: "DATABASE_URL"},
		{Source: Dotenv, File: "url.env", Key: "OTHER"},
		{Source: Dotenv, File: "url.env", Key: "BAD"},
		{Source: Dotenv, File: "url.env", Key: "PORT"},
		{Source: Dotenv, File: "laravel.env"},
		{Source: Dotenv, File: "laravel.env", Key: "MISSING"},
		{Source: AppSettings, File: "appsettings.json", Key: "ConnectionStrings:A"},
		{Source: AppSettings, File: "appsettings.json", Key: "ConnectionStrings:B"},
		{Source: AppSettings, File: "appsettings.json", Key: "ConnectionStrings:C"},
		{Source: AppSettings, File: "broken.json", Key: "ConnectionStrings:A"},
		{Source: Properties, File: "app.properties"},
		{Source: YAML, File: "database.yml"},
		{Source: YAML, File: "bad.yml"},
		{Source: PHP, File: "env.php"},
		{Source: Python, File: "settings.py"},
		{Source: Compose, File: "compose.yaml", Key: "db"},
		{Source: Compose, File: "compose2.yaml", Key: "db"},
		{Source: Env, Key: "DATABASE_URL"},
		{Source: Dotenv, File: "../outside.env"},
		{Source: Dotenv, File: "laravel.env", Host: "S3cr@x"},
	}
	env := lookup(map[string]string{"DATABASE_URL": "postgres://u:S3cr%zz@h/db"})
	for _, r := range refs {
		for _, driver := range []string{MySQL, Postgres} {
			dsn, err := r.DSN(root, driver, env)
			if err == nil {
				// An engine DriftGate does not know leaves the driver to the component.
				if !strings.Contains(r.File, "laravel") && !strings.Contains(r.File, "url") && r.File != "settings.py" {
					t.Errorf("%+v %s: no error, DSN %s", r, driver, dsn)
				}
				continue
			}
			for _, s := range []string{"S3cr", "3cr-"} {
				if strings.Contains(strings.ReplaceAll(err.Error(), "S3cr@x", ""), s) {
					t.Errorf("%+v %s: the error holds a value: %v", r, driver, err)
				}
			}
		}
	}
}

func TestCheckFile(t *testing.T) {
	for _, tt := range []struct {
		file string
		ok   bool
	}{
		{".env", true},
		{"src/Shop.Api/Shop.Api.csproj", true},
		{`src\Shop.Api\Shop.Api.csproj`, true},
		{"./config/../config/database.yml", true},
		{"", false},
		{"/etc/passwd", false},
		{`C:\Users\me\.env`, false},
		{`C:.env`, false},
		{`\\server\share\.env`, false},
		{"../.env", false},
		{`..\.env`, false},
		{"config/../../.env", false},
		{"a\x00b", false},
	} {
		if err := CheckFile(tt.file); (err == nil) != tt.ok {
			t.Errorf("CheckFile(%q) = %v", tt.file, err)
		}
	}
}

func TestWindowsPaths(t *testing.T) {
	defer func(g string) { goos = g }(goos)
	goos = "windows"
	got := userSecretsPath("aspnet-x-1", lookup(map[string]string{"APPDATA": `C:\Users\me\AppData\Roaming\`, "HOME": "/home/me"}))
	if want := `C:\Users\me\AppData\Roaming\Microsoft\UserSecrets\aspnet-x-1\secrets.json`; got != want {
		t.Errorf("windows: %s, want %s", got, want)
	}
	goos = "linux"
	got = userSecretsPath("aspnet-x-1", lookup(map[string]string{"HOME": "/home/me"}))
	if want := filepath.Join("/home/me", ".microsoft", "usersecrets", "aspnet-x-1", "secrets.json"); got != want {
		t.Errorf("linux: %s, want %s", got, want)
	}
	for _, bad := range []string{"<UserSecretsId>../../etc</UserSecretsId>", `<UserSecretsId>a\b</UserSecretsId>`, "<UserSecretsId>a/b</UserSecretsId>"} {
		if _, err := userSecretsID(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
	// A Ref written with \ (on Windows) reads the same file.
	r := Ref{Source: AppSettings, File: `src\Shop.Api\Shop.Api.csproj`, Key: "connectionstrings:default"}
	dsn, err := r.DSN("testdata/dotnet", MySQL, nil)
	if err != nil || dsn != "mysql://shop:S3cr-dotnet-dev@127.0.0.1:3306/?ssl-mode=DISABLED" {
		t.Errorf("backslashes: %s, %v", dsn, err)
	}
}

func TestDriverMismatch(t *testing.T) {
	r := Ref{Source: Dotenv, File: ".env"}
	if _, err := r.DSN("testdata/laravel55", Postgres, nil); err == nil || !strings.Contains(err.Error(), "names a mysql database, the driver is postgres") {
		t.Errorf("got %v", err)
	}
	r = Ref{Source: Compose, File: "docker-compose.yml", Key: "redis"}
	if _, err := r.DSN("testdata/sail", MySQL, nil); err == nil || !strings.Contains(err.Error(), "runs no MySQL") {
		t.Errorf("got %v", err)
	}
}
