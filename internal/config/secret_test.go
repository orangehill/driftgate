package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
)

func TestDSNSources(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "DB_CONNECTION=mysql\nDB_HOST=mysql\nDB_PORT=3306\nDB_DATABASE=shop\nDB_USERNAME=shop\nDB_PASSWORD=\"pw-dotenv\"\nREPORTS_URL=postgresql://u:pw-url@db:5432/shop?schema=public\n")
	write("src/Api/Api.csproj", "<Project/>")
	write("src/Api/appsettings.json", `{
  // comment
  "ConnectionStrings": { "Default": "Server=db;Database=shop;Uid=u;Pwd=pw-ado", },
}`)
	write("compose.yaml", "services:\n  db:\n    image: mariadb:11\n    ports: ['3307:3306']\n    environment:\n      MARIADB_ROOT_PASSWORD: pw-compose\n")
	write("driftgate.yaml", `version: 1
project: shop
state:
  db:
    driver: mariadb
    dsn: { dotenv: .env, host: "127.0.0.1:3307" }
    working: shop
  url:
    driver: postgres
    dsn: { dotenv: .env, key: REPORTS_URL }
    working: shop
  ado:
    driver: mysql
    dsn: { appsettings: src/Api/Api.csproj, key: "ConnectionStrings:Default" }
    working: shop
  compose:
    driver: mysql
    dsn: { compose: compose.yaml, key: db }
    working: shop
  envado:
    driver: mysql
    dsn: { env: DG_TEST_ADO_DSN }
    working: shop
`)
	t.Setenv("DG_TEST_ADO_DSN", "Server=h;Database=shop;Uid=u;Pwd=pw-env")
	t.Setenv("ASPNETCORE_ENVIRONMENT", "Development")
	p, err := Load(filepath.Join(root, "driftgate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"db":      "mysql://shop:pw-dotenv@127.0.0.1:3307/",
		"url":     "postgres://u:pw-url@db:5432/",
		"ado":     "mysql://u:pw-ado@db:3306/",
		"compose": "mysql://root:pw-compose@127.0.0.1:3307/",
		"envado":  "mysql://u:pw-env@h:3306/",
	}
	for name, dsn := range want {
		c := p.State[name]
		got, err := c.DSN.Read(context.Background(), p.Root, c.Driver)
		if err != nil || got != dsn {
			t.Errorf("%s: %s, %v; want %s", name, got, err, dsn)
		}
	}
	// A wrong driver is an error that names the file, not the value.
	_, err = p.State["db"].DSN.Read(context.Background(), p.Root, Postgres)
	if err == nil || !strings.Contains(err.Error(), ".env names a mysql database, the driver is postgres") || strings.Contains(err.Error(), "pw-") {
		t.Errorf("driver: %v", err)
	}
	// The configuration, saved, still holds no secret.
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "driftgate.yaml"))
	if strings.Contains(string(raw), "pw-") || !strings.Contains(string(raw), "dotenv: .env") {
		t.Errorf("saved:\n%s", raw)
	}
}

func TestDSNValidation(t *testing.T) {
	base := Config{Version: Version, Project: "p", Mode: Freeze, Level: "values"}
	for _, c := range []struct {
		dsn Secret
		err string
	}{
		{Secret{Dotenv: ".env"}, ""},
		{Secret{Dotenv: ".env", Key: "DATABASE_URL"}, ""},
		{Secret{Properties: "src/main/resources/application.properties"}, ""},
		{Secret{YAML: "config/database.yml", Key: "development"}, ""},
		{Secret{PHP: "app/etc/env.php"}, ""},
		{Secret{Python: "shop/settings.py"}, ""},
		{Secret{AppSettings: `src\Api\Api.csproj`, Key: "ConnectionStrings:Default"}, ""},
		{Secret{Compose: "compose.yaml", Key: "db"}, ""},
		{Secret{Key: "X"}, "dsn: needs one of env, op, dotenv"},
		{Secret{Env: "X", Dotenv: ".env"}, "dsn: takes one source, not env and dotenv"},
		{Secret{Dotenv: ".env", YAML: "a.yml"}, "dsn: takes one source, not dotenv and yaml"},
		{Secret{Env: "X", Key: "Y"}, "dsn: key and host are for a file of the project (dotenv, appsettings,"},
		{Secret{Op: "op://v/i/f", Host: "h"}, "dsn: key and host are for a file of the project"},
		{Secret{Dotenv: "/etc/app/.env"}, "dsn: /etc/app/.env must be relative to the project root"},
		{Secret{Dotenv: `C:\app\.env`}, `must be relative to the project root`},
		{Secret{Dotenv: "../other/.env"}, "dsn: ../other/.env must be inside the project"},
		{Secret{AppSettings: "appsettings.json"}, "dsn: appsettings needs key"},
		{Secret{Compose: "compose.yaml"}, "dsn: compose needs key (the name of the database service)"},
		{Secret{Dotenv: ".env", Host: "user:pw@h"}, "dsn: host must be host, host:port or the path of a socket"},
	} {
		cfg := base
		cfg.State = map[string]Component{"db": {Driver: MySQL, DSN: c.dsn, Working: "app"}}
		_, err := New(cfg, t.TempDir())
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: %v, want %q", c.dsn, err, c.err)
		}
		cfg = base
		cfg.Data = Data{Driver: MySQL, Source: DataDB{DSN: c.dsn, Database: "a"}, Target: DataDB{Database: "b"}}
		_, err = New(cfg, t.TempDir())
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), strings.Replace(c.err, "dsn:", "data.source.dsn:", 1))) {
			t.Errorf("data %+v: %v, want %q", c.dsn, err, c.err)
		}
	}
}

func TestSecretFor(t *testing.T) {
	for _, src := range appconf.Sources {
		r := appconf.Ref{Source: src, File: "f", Key: "k", Host: "127.0.0.1:1"}
		if got := SecretFor(r).ref(); got != r {
			t.Errorf("%s: %+v", src, got)
		}
	}
	if s := SecretFor(appconf.Ref{Source: appconf.Env, Key: "ConnectionStrings__Default"}); s != (Secret{Env: "ConnectionStrings__Default"}) {
		t.Errorf("env: %+v", s)
	}
}

func TestReadDSNChecksWorking(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DB_CONNECTION=mysql\nDB_HOST=127.0.0.1\nDB_DATABASE=shop_wt42\nDB_USERNAME=u\nDB_PASSWORD=pw-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Component{Driver: MySQL, DSN: Secret{Dotenv: ".env"}, Working: "shop"}
	_, err := c.ReadDSN(context.Background(), root)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), ".env names the database shop_wt42, but working is shop") || strings.Contains(err.Error(), "pw-") {
		t.Errorf("other database: %v", err)
	}
	c.Working = "shop_wt42"
	if dsn, err := c.ReadDSN(context.Background(), root); err != nil || dsn != "mysql://u:pw-secret@127.0.0.1:3306/" {
		t.Errorf("same database: %s, %v", dsn, err)
	}
	// An environment DSN names the server; its database is not checked.
	t.Setenv("DG_TEST_WORKING_DSN", "mysql://u:p@127.0.0.1:3306/other")
	c = Component{Driver: MySQL, DSN: Secret{Env: "DG_TEST_WORKING_DSN"}, Working: "shop"}
	if _, err := c.ReadDSN(context.Background(), root); err != nil {
		t.Errorf("env: %v", err)
	}
}
