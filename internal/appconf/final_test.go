package appconf

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConnStringBeforeDriverForm(t *testing.T) {
	for _, pw := range []string{"Tr0ub4dor&3@/horse", "x@tcp(evil:1)/y", "x@unix(/s)/y", "a://b@c"} {
		cs := "Server=127.0.0.1;Port=3307;Database=shop;User=root;Password=" + pw
		root := writeTree(t, map[string]string{
			"Api/Api.csproj":       `<Project><ItemGroup><PackageReference Include="MySqlConnector" /></ItemGroup></Project>`,
			"Api/appsettings.json": `{"ConnectionStrings": {"Default": "` + cs + `"}}`,
		})
		found := Find(root, nil)
		if len(found.Databases) != 1 {
			t.Fatalf("%s: %+v", pw, found)
		}
		d := found.Databases[0]
		if d.User != "root" || d.Host != "127.0.0.1" || d.Port != "3307" || d.Name != "shop" {
			t.Errorf("%s: %+v", pw, d)
		}
		dsn, name, err := d.Ref.Read(root, MySQL, nil)
		p, perr := fromValue(dsn)
		if err != nil || name != "shop" || perr != nil || p.password != pw || p.user != "root" {
			t.Errorf("%s: %s %s %v %v", pw, dsn, name, err, perr)
		}
		got, err := Convert(cs, MySQL)
		if err != nil || got != dsn {
			t.Errorf("Convert %s: %s, %v", pw, got, err)
		}
	}
	// The driver's own form stays one.
	if p, err := fromValue("u:p=;x@tcp(h:3306)/db"); err != nil || p.form == "" || p.user != "u" {
		t.Errorf("driver form: %+v, %v", p, err)
	}
}

func TestComposeHostFollowsThePort(t *testing.T) {
	root, _ := filepath.Abs("testdata/sail")
	r := Ref{Source: Dotenv, File: ".env", Host: ComposeHost + "mysql"}
	if got := dsnOf(t, root, r, MySQL, map[string]string{"FORWARD_DB_PORT": "40000"}); got != "mysql://sail:S3cr-sail@127.0.0.1:40000/" {
		t.Errorf("got %s", got)
	}
	r.Host = ComposeHost + "nosuch"
	if _, err := r.DSN(root, MySQL, nil); err == nil || !strings.Contains(err.Error(), "has no service nosuch") {
		t.Errorf("unknown service: %v", err)
	}
	if err := CheckRef(Ref{Source: Dotenv, File: ".env", Host: "compose:a b"}); err == nil {
		t.Error("compose: with a space accepted")
	}
}

func TestSpringOffersActiveProfilesOnly(t *testing.T) {
	const res = "src/main/resources/"
	files := map[string]string{
		res + "application.properties":         "spring.datasource.url=jdbc:mysql://127.0.0.1/app\n#---\nspring.config.activate.on-profile=prod\nspring.datasource.url=jdbc:mysql://prod-db/app\n",
		res + "application-staging.properties": "spring.datasource.url=jdbc:mysql://staging-db/app\n",
	}
	root := writeTree(t, files)
	found := Find(root, nil)
	if len(found.Databases) != 1 || found.Databases[0].Ref.File != res+"application.properties" || found.Databases[0].Host != "127.0.0.1" {
		t.Errorf("no profile active: %+v", found.Databases)
	}
	found = Find(root, lookup(map[string]string{"SPRING_PROFILES_ACTIVE": "prod"}))
	if len(found.Databases) != 1 || found.Databases[0].Host != "prod-db" {
		t.Errorf("prod active: %+v", found.Databases)
	}
	found = Find(root, lookup(map[string]string{"SPRING_PROFILES_ACTIVE": "staging"}))
	if len(found.Databases) != 2 || found.Databases[1].Ref.File != res+"application-staging.properties" || found.Databases[0].Host != "staging-db" {
		t.Errorf("staging active: %+v", found.Databases)
	}
}

func TestDotenvFilesOfTheFramework(t *testing.T) {
	env := func(root string, e map[string]string) []string {
		var out []string
		for _, d := range Find(root, lookup(e)).Databases {
			out = append(out, d.Ref.File+" "+d.Host)
		}
		return out
	}
	laravel := writeTree(t, map[string]string{
		"artisan":     "#!/usr/bin/env php\n",
		".env":        "DB_CONNECTION=mysql\nDB_HOST=10.0.0.1\n",
		".env.local":  "DB_CONNECTION=mysql\nDB_HOST=10.0.0.2\n",
		".env.ci":     "DB_CONNECTION=mysql\nDB_HOST=10.0.0.3\n",
		".env.ci.bad": "",
	})
	if got := strings.Join(env(laravel, nil), ","); got != ".env 10.0.0.1" {
		t.Errorf("Laravel: %s", got)
	}
	if got := strings.Join(env(laravel, map[string]string{"APP_ENV": "ci"}), ","); got != ".env.ci 10.0.0.3" {
		t.Errorf("Laravel APP_ENV: %s", got)
	}
	if got := strings.Join(env(laravel, map[string]string{"APP_ENV": "../x"}), ","); got != ".env 10.0.0.1" {
		t.Errorf("Laravel APP_ENV with a path: %s", got)
	}
	symfony := writeTree(t, map[string]string{
		"bin/console":     "#!/usr/bin/env php\n",
		".env":            "APP_ENV=dev\nDATABASE_URL=mysql://u@10.0.0.1/app\n",
		".env.local":      "DATABASE_URL=mysql://u@10.0.0.2/app\n",
		".env.test":       "DATABASE_URL=mysql://u@10.0.0.4/app\n",
		".env.dev.local":  "DATABASE_URL=mysql://u@10.0.0.5/app\n",
		".env.staging":    "DATABASE_URL=mysql://u@10.0.0.6/app\n",
		"some/other/file": "",
	})
	if got := strings.Join(env(symfony, nil), ","); got != ".env.dev.local 10.0.0.5,.env.local 10.0.0.2,.env 10.0.0.1" {
		t.Errorf("Symfony dev: %s", got)
	}
	if got := strings.Join(env(symfony, map[string]string{"APP_ENV": "test"}), ","); got != ".env.test 10.0.0.4,.env 10.0.0.1" {
		t.Errorf("Symfony test: %s", got)
	}
}

func TestTLSOfLaravelAndPrisma(t *testing.T) {
	root := writeTree(t, map[string]string{".env": "DB_CONNECTION=mysql\nDB_HOST=db\nDB_USERNAME=u\nMYSQL_ATTR_SSL_CA=/etc/ssl/ca.pem\n"})
	if got := dsnOf(t, root, Ref{Source: Dotenv, File: ".env"}, MySQL, nil); got != "mysql://u@db:3306/?ssl-ca=%2Fetc%2Fssl%2Fca.pem&ssl-mode=VERIFY_CA" {
		t.Errorf("MYSQL_ATTR_SSL_CA: %s", got)
	}
	for _, tt := range []struct{ in, driver, out, err string }{
		{"postgresql://u:p@db/app?sslaccept=strict", Postgres, "postgres://u:p@db:5432/?sslmode=verify-full", ""},
		{"postgresql://u:p@db/app?sslmode=require&sslaccept=accept_invalid_certs", Postgres, "postgres://u:p@db:5432/?sslmode=require", ""},
		{"mysql://u:p@db/app?sslcert=/ca.pem&sslaccept=strict", MySQL, "mysql://u:p@db:3306/?ssl-ca=%2Fca.pem&ssl-mode=VERIFY_IDENTITY", ""},
		{"postgresql://u:p@db/app?sslaccept=sometimes", Postgres, "", "the TLS setting sslaccept has a value"},
		{"postgresql://u:p@db/app?sslidentity=/id.p12", Postgres, "", "the TLS setting sslidentity (a client identity) is not supported"},
		{"mysql://u:p@localhost/app?socket=/var/run/mysqld/mysqld.sock", MySQL, "u:p@unix(/var/run/mysqld/mysqld.sock)/", ""},
		{"postgresql://u:p@localhost/app?socket=/tmp", Postgres, "", "the socket parameter of the URL is not the path of a MySQL socket"},
	} {
		p, err := fromValue(tt.in)
		var got string
		if err == nil {
			got, err = p.dsn(tt.driver, "x")
		}
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: %v, want %s", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.out {
			t.Errorf("%s: %s, %v; want %s", tt.in, got, err, tt.out)
		}
	}
}

func TestLocalHostIsNotGuessed(t *testing.T) {
	root := writeTree(t, map[string]string{
		".env":                "DB_CONNECTION=mysql\nDB_HOST=localhost\nDB_DATABASE=shop\nDB_USERNAME=u\n",
		"config/database.yml": "development:\n  adapter: postgresql\n  database: shop\n",
		"pg/.env":             "DB_CONNECTION=pgsql\nDB_HOST=localhost\nDB_USERNAME=u\n",
		"nohost/.env":         "DB_CONNECTION=mysql\nDB_USERNAME=u\n",
		"src/main/resources/application.properties": "spring.datasource.url=jdbc:mysql://localhost/shop\n",
	})
	for _, c := range []struct {
		r     Ref
		host  string
		hint  string
		dsn   string
		named string
	}{
		{Ref{Source: Dotenv, File: ".env"}, "", "add host: 127.0.0.1:3306", "", ""},
		{Ref{Source: Dotenv, File: ".env", Host: "127.0.0.1:3306"}, "", "", "mysql://u@127.0.0.1:3306/", "shop"},
		{Ref{Source: Dotenv, File: ".env", Host: "/var/run/mysqld/mysqld.sock"}, "", "", "u@unix(/var/run/mysqld/mysqld.sock)/", "shop"},
		{Ref{Source: YAML, File: "config/database.yml"}, "", "add host: 127.0.0.1:5432", "", ""},
		{Ref{Source: YAML, File: "config/database.yml", Host: "/var/run/postgresql"}, "", "", "postgres:///?host=%2Fvar%2Frun%2Fpostgresql", "shop"},
		{Ref{Source: Dotenv, File: "pg/.env"}, "", "", "postgres://u@localhost:5432/", ""},                                      // libpq's localhost is TCP
		{Ref{Source: Dotenv, File: "nohost/.env"}, "", "", "mysql://u@127.0.0.1:3306/", ""},                                     // Laravel's default
		{Ref{Source: Properties, File: "src/main/resources/application.properties"}, "", "", "mysql://localhost:3306/", "shop"}, // JDBC is TCP
	} {
		dsn, named, err := c.r.Read(root, "", nil)
		if c.hint != "" {
			if err == nil || !strings.Contains(err.Error(), c.hint) {
				t.Errorf("%+v: %v, want %s", c.r, err, c.hint)
			}
			continue
		}
		if err != nil || dsn != c.dsn || named != c.named {
			t.Errorf("%+v: %s %q %v; want %s %q", c.r, dsn, named, err, c.dsn, c.named)
		}
	}
	var needed []string
	for _, d := range Find(root, nil).Databases {
		needed = append(needed, d.Ref.File+"="+d.HostNeeded)
	}
	if got := strings.Join(needed, " "); got != ".env=127.0.0.1:3306 src/main/resources/application.properties= config/database.yml=127.0.0.1:5432" {
		t.Errorf("Find: %s", got)
	}
}
