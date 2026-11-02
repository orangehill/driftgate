package appconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/dbdsn"
)

// writeTree writes files (by slash path) under a new folder.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// dsnOf reads r in root for driver, failing the test on an error.
func dsnOf(t *testing.T, root string, r Ref, driver string, env map[string]string) string {
	t.Helper()
	dsn, err := r.DSN(root, driver, lookup(env))
	if err != nil {
		t.Fatalf("%+v: %v", r, err)
	}
	return dsn
}

func TestTLSKept(t *testing.T) {
	for _, tt := range []struct{ in, driver, out, err string }{
		{"Server=db.prod;Database=app;Uid=u;Pwd=p;SslMode=Required", MySQL, "mysql://u:p@db.prod:3306/?ssl-mode=REQUIRED", ""},
		{"Server=db.prod;Uid=u;Pwd=p;SslMode=VerifyFull;SslCa=/etc/ca.pem", MySQL, "mysql://u:p@db.prod:3306/?ssl-ca=%2Fetc%2Fca.pem&ssl-mode=VERIFY_IDENTITY", ""},
		{"Server=db.prod;Uid=u;Pwd=p;SslCa=/etc/ca.pem", MySQL, "mysql://u:p@db.prod:3306/?ssl-ca=%2Fetc%2Fca.pem&ssl-mode=VERIFY_CA", ""},
		{"Host=db.prod;Database=app;Username=u;Password=p;SSL Mode=VerifyFull", Postgres, "postgres://u:p@db.prod:5432/?sslmode=verify-full", ""},
		{"Host=db.prod;Username=u;Password=p;SSL Mode=Require;Root Certificate=/ca.pem", Postgres, "postgres://u:p@db.prod:5432/?sslmode=require&sslrootcert=%2Fca.pem", ""},
		{"jdbc:mysql://db.prod:3306/app?user=u&password=p&useSSL=true&requireSSL=true", MySQL, "mysql://u:p@db.prod:3306/?ssl-mode=REQUIRED", ""},
		{"jdbc:mysql://db.prod:3306/app?user=u&password=p&useSSL=true&verifyServerCertificate=true", MySQL, "mysql://u:p@db.prod:3306/?ssl-mode=VERIFY_CA", ""},
		{"jdbc:mysql://db.prod/app?user=u&password=p&sslMode=VERIFY_IDENTITY", MySQL, "mysql://u:p@db.prod:3306/?ssl-mode=VERIFY_IDENTITY", ""},
		{"jdbc:postgresql://db.prod/app?user=u&password=p&ssl=true", Postgres, "postgres://u:p@db.prod:5432/?sslmode=verify-full", ""},
		{"jdbc:postgresql://db.prod/app?user=u&password=p&ssl=true&sslmode=require", Postgres, "postgres://u:p@db.prod:5432/?sslmode=require", ""},
		{"jdbc:postgresql://db.prod/app?user=u&password=p&ApplicationName=x&connectTimeout=5&targetServerType=primary&loggerLevel=OFF", Postgres, "postgres://u:p@db.prod:5432/?application_name=x&connect_timeout=5&target_session_attrs=primary", ""},
		{"Server=db.prod;Uid=u;Pwd=p;SslMode=Sometimes", MySQL, "", "the TLS setting sslmode has a value DriftGate does not know (the value is not shown); set it to one of disable, prefer, require, verify-ca, verify-full"},
		{"jdbc:mysql://db.prod/app?user=u&password=p&useSSL=maybe", MySQL, "", "the TLS setting usessl has a value"},
		{"Server=db.prod;Uid=u;Pwd=p;SslMode=Required;SslCert=/c.pem;SslKey=/k.pem", MySQL, "", "a client certificate (sslcert, sslkey) is not supported for MySQL"},
	} {
		got, err := Convert(tt.in, tt.driver)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: error %v, want %s", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.out {
			t.Errorf("%s:\n got %s, %v\nwant %s", tt.in, got, err, tt.out)
			continue
		}
		if strings.Contains(got, "ca=") || strings.Contains(got, "rootcert=") {
			continue // the drivers read the file
		}
		if _, err := dbdsn.Server(tt.driver, got); err != nil {
			t.Errorf("%s: dbdsn: %v", got, err)
		}
	}
	// The TLS of the MySQL driver's own form stays when the host is replaced.
	p, err := fromValue("u:p@tcp(mysql:3306)/app?tls=true")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.setHost("127.0.0.1:3307"); err != nil {
		t.Fatal(err)
	}
	if got, err := p.dsn(MySQL, "x"); err != nil || got != "mysql://u:p@127.0.0.1:3307/?ssl-mode=VERIFY_IDENTITY" {
		t.Errorf("driver form with host: %s, %v", got, err)
	}
}

func TestRailsTLSAndDatabaseURL(t *testing.T) {
	root := writeTree(t, map[string]string{"config/database.yml": "development:\n  adapter: postgresql\n  host: db.prod\n  username: u\n  password: p\n  sslmode: verify-full\n"})
	r := Ref{Source: YAML, File: "config/database.yml", Key: "development"}
	if got := dsnOf(t, root, r, Postgres, nil); got != "postgres://u:p@db.prod:5432/?sslmode=verify-full" {
		t.Errorf("sslmode: %s", got)
	}
	// Rails merges DATABASE_URL over the environment's configuration.
	got := dsnOf(t, root, r, Postgres, map[string]string{"DATABASE_URL": "postgres://u2@other-host/other"})
	if got != "postgres://u2:p@other-host:5432/?sslmode=verify-full" {
		t.Errorf("DATABASE_URL: %s", got)
	}
}

func TestDjangoStartproject(t *testing.T) {
	settings := `from pathlib import Path

BASE_DIR = Path(__file__).resolve().parent.parent
SECRET_KEY = 'django-insecure-x'
DEBUG = True
ALLOWED_HOSTS = []

DATABASES = {
    'default': {
        'ENGINE': 'django.db.backends.sqlite3',
        'NAME': BASE_DIR / 'db.sqlite3',
    }
}

DATABASES = {
    'default': {
        'ENGINE': 'django.db.backends.postgresql',
        'NAME': 'shop',
        'USER': 'shop',
        'PASSWORD': 'p',
        'HOST': 'localhost',
    }
}  # the local one

# Password validation
AUTH_PASSWORD_VALIDATORS = [
    {
        'NAME': 'django.contrib.auth.password_validation.UserAttributeSimilarityValidator',
    },
]

LANGUAGE_CODE = 'en-us'
`
	root := writeTree(t, map[string]string{"shop/settings.py": settings})
	if got := dsnOf(t, root, Ref{Source: Python, File: "shop/settings.py"}, Postgres, nil); got != "postgres://shop:p@localhost:5432/" {
		t.Errorf("got %s", got)
	}
}

func TestLiteralEnds(t *testing.T) {
	for _, src := range []string{
		"<?php return ['db' => ['host' => 'h';]];",
		"DATABASES = {'default': {'HOST': 'h')}",
		"<?php return ['a' => ['b' => 'c'];",
		"<?php return [,,,;",
	} {
		done := make(chan error, 1)
		go func() {
			py := strings.HasPrefix(src, "DATABASES")
			var err error
			if py {
				_, err = djangoDatabases(src)
			} else {
				l := &literal{s: src, i: strings.Index(src, "return") + 6}
				_, err = l.value(0)
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: no error", src)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: does not return", src)
		}
	}
}

func TestERBEdges(t *testing.T) {
	root := writeTree(t, map[string]string{"config/database.yml": "development:\n  adapter: mysql2\n  host: h\n  password: a<%>b\n"})
	if _, err := (Ref{Source: YAML, File: "config/database.yml"}).DSN(root, MySQL, nil); err != nil && strings.Contains(err.Error(), "a<") {
		t.Errorf("error quotes a value: %v", err)
	}
	got := erb("<%= ENV.fetch('DB_HOST', 'db.internal') %>|<%= ENV['DB_HOST'] || 'db.internal' %>", lookup(map[string]string{"DB_HOST": ""}))
	if got != "|" {
		t.Errorf("empty variable: %q", got)
	}
}

func TestSpringRules(t *testing.T) {
	const res = "src/main/resources/"
	for _, tt := range []struct {
		name  string
		files map[string]string
		file  string
		env   map[string]string
		host  string
	}{
		{"properties beat yml", map[string]string{
			res + "application-dev.properties": "spring.datasource.url=jdbc:mysql://db-a/app\n",
			res + "application-dev.yml":        "spring:\n  datasource:\n    url: jdbc:mysql://db-b/app\n",
		}, res + "application-dev.properties", nil, "db-a"},
		{"profile document", map[string]string{
			res + "application-dev.yml": "spring:\n  datasource:\n    url: jdbc:mysql://localhost/app\n---\nspring:\n  config:\n    activate:\n      on-profile: dev\n  datasource:\n    url: jdbc:mysql://127.0.0.1/app\n---\nspring:\n  config:\n    activate:\n      on-profile: prod\n  datasource:\n    url: jdbc:mysql://prod/app\n",
		}, res + "application-dev.yml", nil, "127.0.0.1"},
		{"active profile of the base file", map[string]string{
			res + "application.properties":     "spring.profiles.active=dev\nspring.datasource.url=jdbc:mysql://localhost/app\n",
			res + "application-dev.properties": "spring.datasource.url=jdbc:mysql://127.0.0.1/app\n",
		}, res + "application.properties", nil, "127.0.0.1"},
		{"SPRING_PROFILES_ACTIVE", map[string]string{
			res + "application.properties":      "spring.profiles.active=dev\nspring.datasource.url=jdbc:mysql://localhost/app\n",
			res + "application-dev.properties":  "spring.datasource.url=jdbc:mysql://127.0.0.1/app\n",
			res + "application-test.properties": "spring.datasource.url=jdbc:mysql://test-db/app\n",
		}, res + "application.properties", map[string]string{"SPRING_PROFILES_ACTIVE": "test"}, "test-db"},
		{"placeholder takes a property", map[string]string{
			res + "application.properties": "db.host=10.1.2.3\nspring.datasource.url=jdbc:mysql://${db.host:localhost}/app\n",
		}, res + "application.properties", nil, "10.1.2.3"},
		{"placeholder default", map[string]string{
			res + "application.properties": "spring.datasource.url=jdbc:mysql://${db.host:localhost}/app\n",
		}, res + "application.properties", nil, "localhost"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := writeTree(t, tt.files)
			p, err := Ref{Source: Properties, File: tt.file}.read(root, lookup(tt.env))
			if err != nil || p.host != tt.host {
				t.Errorf("host %q, %v; want %s", p.host, err, tt.host)
			}
		})
	}
	root := writeTree(t, map[string]string{res + "application.properties": "a=${b}\nb=${a}\nspring.datasource.url=jdbc:mysql://${a}/x\n"})
	if _, err := (Ref{Source: Properties, File: res + "application.properties"}).read(root, nil); err == nil {
		t.Error("placeholders that refer to each other: no error")
	}
}

func TestLaravelRules(t *testing.T) {
	root := writeTree(t, map[string]string{
		"url/.env":    "DB_CONNECTION=mysql\nDB_URL=mysql://u:p@db-url-host:3306/app\nDB_HOST=127.0.0.1\nDB_PASSWORD=other\n",
		"pgsql/.env":  "DB_CONNECTION=pgsql\nDB_HOST=db.internal\nDB_SOCKET=/var/run/mysqld/mysqld.sock\nDB_USERNAME=u\n",
		"twice/.env":  "DB_CONNECTION=mysql\nDB_HOST=a\nAPP_NAME=x\nDB_HOST=b\n",
		"same/.env":   "DB_CONNECTION=mysql\nDB_HOST=a\nDB_HOST=a\n",
		"urltwo/.env": "DATABASE_URL=mysql://u:p@a/x\nDATABASE_URL=mysql://u:p@b/x\n",
	})
	if got := dsnOf(t, filepath.Join(root, "url"), Ref{Source: Dotenv, File: ".env"}, MySQL, nil); got != "mysql://u:p@db-url-host:3306/" {
		t.Errorf("DB_URL: %s", got)
	}
	if got := dsnOf(t, filepath.Join(root, "pgsql"), Ref{Source: Dotenv, File: ".env"}, Postgres, nil); got != "postgres://u@db.internal:5432/" {
		t.Errorf("pgsql socket: %s", got)
	}
	if got := dsnOf(t, filepath.Join(root, "same"), Ref{Source: Dotenv, File: ".env"}, MySQL, nil); got != "mysql://a:3306/" {
		t.Errorf("same value twice: %s", got)
	}
	for _, c := range []struct{ dir, key, err string }{
		{"twice", "", ".env: DB_HOST is set twice to different values (lines 2 and 4); Laravel 5.5 to 5.7 take the first, later versions the last: keep one"},
		{"urltwo", "DATABASE_URL", ".env: DATABASE_URL is set twice to different values (lines 1 and 2)"},
	} {
		_, err := Ref{Source: Dotenv, File: ".env", Key: c.key}.DSN(filepath.Join(root, c.dir), MySQL, nil)
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: %v, want %s", c.dir, err, c.err)
		}
	}
}

func TestLaunchProfile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Api/Api.csproj":               "<Project/>",
		"Api/appsettings.json":         `{"ConnectionStrings": {"Default": "Server=base;Uid=u;Pwd=p", "Other": "Server=o;Uid=u;Pwd=p"}}`,
		"Api/appsettings.Staging.json": `{"ConnectionStrings": {"Default": "Server=staging;Uid=u;Pwd=p"}}`,
		"Api/Properties/launchSettings.json": `{
  "profiles": {
    "IIS Express": {"commandName": "IISExpress", "applicationUrl": "http://localhost:1"},
    "Api": {
      "commandName": "Project",
      "applicationUrl": "https://localhost:7001;http://localhost:5001",
      "environmentVariables": {"ASPNETCORE_ENVIRONMENT": "Staging", "ConnectionStrings__Other": "Server=fromlaunch;Uid=u;Pwd=p"}
    }
  }
}`,
	})
	r := Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:Default"}
	if got := dsnOf(t, root, r, MySQL, nil); got != "mysql://u:p@staging:3306/" {
		t.Errorf("environment of the profile: %s", got)
	}
	r.Key = "ConnectionStrings:Other"
	if got := dsnOf(t, root, r, MySQL, nil); got != "mysql://u:p@fromlaunch:3306/" {
		t.Errorf("connection string of the profile: %s", got)
	}
	var urls []string
	for _, u := range Find(root, nil).AppURLs {
		urls = append(urls, u.URL)
	}
	if strings.Join(urls, " ") != "https://localhost:7001 http://localhost:5001" {
		t.Errorf("urls %q", urls)
	}
}

func TestSymlinkOutside(t *testing.T) {
	outside := writeTree(t, map[string]string{".env": "DB_CONNECTION=mysql\nDB_HOST=h\nDB_PASSWORD=p\n"})
	root := writeTree(t, map[string]string{
		"compose.yaml": "services:\n  db:\n    image: mysql:8\n    env_file: shared/.env\n    ports: ['3307:3306']\n    environment:\n      MYSQL_USER: u\n",
	})
	if err := os.Symlink(outside, filepath.Join(root, "shared")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if err := os.Symlink(filepath.Join(outside, ".env"), filepath.Join(root, ".env")); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Ref{
		{Source: Dotenv, File: "shared/.env", Host: "evil.example:3306"},
		{Source: Dotenv, File: ".env"},
		{Source: Compose, File: "compose.yaml", Key: "db"},
	} {
		_, err := r.DSN(root, MySQL, nil)
		if err == nil || !strings.Contains(err.Error(), "a symbolic link leads outside the project") {
			t.Errorf("%+v: %v", r, err)
		}
	}
	// A link inside the project is followed.
	if err := os.WriteFile(filepath.Join(root, "real.env"), []byte("DB_CONNECTION=mysql\nDB_HOST=h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.env", filepath.Join(root, "link.env")); err != nil {
		t.Fatal(err)
	}
	if got := dsnOf(t, root, Ref{Source: Dotenv, File: "link.env"}, MySQL, nil); got != "mysql://h:3306/" {
		t.Errorf("link inside: %s", got)
	}
}

func FuzzLiteral(f *testing.F) {
	for _, s := range []string{
		"<?php return ['db' => ['connection' => ['default' => ['host' => 'h', 'password' => \"p\\$\"]]]];",
		"<?php return array('a' => array(1, 2), \\PDO::X => true, 'b' => f('x') . 'y');",
		"DATABASES = {'default': {'NAME': r'x', 'PORT': 5432, 'OPTIONS': {'a': [1, (2, 3)]}}}\nX = 1",
		"<?php return ['db' => ['host' => 'h';]];",
		"DATABASES = {'default': {'HOST': 'h')}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			djangoDatabases(s)
			if i := strings.Index(s, "return"); i >= 0 {
				l := &literal{s: s, i: i + 6}
				l.value(0)
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("does not return on %q", s)
		}
	})
}
