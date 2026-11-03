package cli

import (
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
)

// TestSetupDrivers: setup takes every driver and its alias, grants
// nothing for an SQLite file, and names a SQL Server login with ] quoted.
func TestSetupDrivers(t *testing.T) {
	for v, want := range map[string]string{"mysql": "mysql", "mariadb": "mysql", "postgres": "postgres", "sqlite": "sqlite",
		"sqlserver": "sqlserver", "mssql": "sqlserver", "oracle": "", "SQLite": "", "": ""} {
		if got := driverOf(v); got != want {
			t.Errorf("driverOf(%q) = %q, want %q", v, got, want)
		}
	}
	if g := grant(config.Component{Driver: config.SQLite, Working: "db.sqlite"}, "u"); g != "" {
		t.Errorf("sqlite grant: %q", g)
	}
	if g := grant(config.Component{Driver: config.SQLServer, Working: "Shop", Pristine: "Shop_pristine"}, "a]b"); !strings.HasPrefix(g, "USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [a]]b]; USE [Shop];") {
		t.Errorf("sqlserver grant: %q", g)
	}
	if g := grant(config.Component{Driver: config.MySQL, Working: "shop", Pristine: "shop_pristine"}, "u"); g != "GRANT ALL PRIVILEGES ON `shop_pristine`.* TO 'u'@'%';" {
		t.Errorf("mysql grant: %q", g)
	}
}

// TestNewProjectDrivers: a SQL Server setting is a TCP server on 1433 and
// sa of a compose service can create the pristine copy; an SQLite
// setting's state: names its file and no pristine database.
func TestNewProjectDrivers(t *testing.T) {
	ref := appconf.Ref{Source: appconf.Compose, File: "compose.yaml", Key: "db"}
	mssql := appconf.Database{Driver: appconf.SQLServer, Host: "localhost", Name: "Shop", User: "sa", Ref: ref}
	if s := server(mssql); s != "sqlserver://local:1433/Shop" {
		t.Errorf("server: %s", s)
	}
	if !canCreate(mssql) {
		t.Error("sa of a compose service cannot create the pristine copy")
	}
	if mssql.User = "app"; canCreate(mssql) {
		t.Error("another login of a compose service can create the pristine copy")
	}

	lite := appconf.Database{Driver: appconf.SQLite, Path: "database/database.sqlite", File: ".env", Ref: appconf.Ref{Source: appconf.Dotenv, File: ".env"}}
	if s := server(lite); s != "sqlite:database/database.sqlite" {
		t.Errorf("server: %s", s)
	}
	block := stateBlock(lite, true)
	for _, want := range []string{"    driver: sqlite\n", "    dsn: {dotenv: .env}", "    working: database/database.sqlite\n", "# pristine: a file of DriftGate's store"} {
		if !strings.Contains(block, want) {
			t.Errorf("state block without %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "_pristine") || strings.Contains(block, "# set ") {
		t.Errorf("state block:\n%s", block)
	}
	lite.Ref = appconf.Ref{}
	if block = stateBlock(lite, true); strings.Contains(block, "dsn:") {
		t.Errorf("state block of an SQLite file without a setting has a dsn:\n%s", block)
	}
}
