package appconf

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectedDrivers: a name of SQLite or SQL Server gives that driver;
// the other drivers are not affected.
func TestDetectedDrivers(t *testing.T) {
	for _, c := range []struct{ name, driver string }{
		{"pdo_sqlite", SQLite},
		{"django.db.backends.sqlite3", SQLite},
		{"org.sqlite.JDBC", SQLite},
		{"sqlsrv", SQLServer},
		{"mssql", SQLServer},
		{"com.microsoft.sqlserver.jdbc.SQLServerDriver", SQLServer},
		{"pgsql", Postgres},
		{"mysql2", MySQL},
	} {
		if d, o := driverName(c.name); d != c.driver || o != "" {
			t.Errorf("%s: %q %q, want %q", c.name, d, o, c.driver)
		}
	}
	for driver, port := range map[string]string{MySQL: "3306", Postgres: "5432", SQLServer: "1433"} {
		if got := defaultPort(driver); got != port {
			t.Errorf("default port of %s: %s", driver, got)
		}
	}
}

// TestSQLiteSettingNamesFile: Ref.Read returns the file an SQLite setting
// names, relative to the project root, as the database (config checks it
// against working).
func TestSQLiteSettingNamesFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DATABASE_URL=sqlite:///database/app.sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, db, err := Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL"}.Read(root, SQLite, nil)
	if err != nil || db != "database/app.sqlite" {
		t.Errorf("database %q, %v", db, err)
	}
}

// TestSQLiteSymfonyProjectDir: Symfony's DATABASE_URL names the file under
// %kernel.project_dir%, the folder of the Symfony project: the .env
// file's, or the one above config/ for a file under it.
func TestSQLiteSymfonyProjectDir(t *testing.T) {
	root := t.TempDir()
	for f, want := range map[string]string{
		".env":                           "var/data.db",
		"backend/.env":                   "backend/var/data.db",
		"backend/.env.local":             "backend/var/data.db",
		"backend/config/packages/x.yaml": "backend/var/data.db",
		"config/packages/doctrine.yaml":  "var/data.db",
	} {
		p, err := resolveSQLite(root, Ref{Source: Dotenv, File: f, Key: "DATABASE_URL"}, parts{name: "%kernel.project_dir%/var/data.db"})
		if err != nil || p.file != want {
			t.Errorf("%s: %q, %v; want %q", f, p.file, err, want)
		}
	}
	// Symfony joins the folder and the rest: //etc/x.db is etc/x.db in it.
	p, err := resolveSQLite(root, Ref{Source: Dotenv, File: "backend/.env", Key: "DATABASE_URL"}, parts{name: "%kernel.project_dir%//etc/x.db"})
	if err != nil || p.file != "backend/etc/x.db" {
		t.Errorf("double slash: %q, %v", p.file, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DATABASE_URL=\"sqlite:///%kernel.project_dir%/var/data.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, db, err := (Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL"}).Read(root, SQLite, nil); err != nil || db != "var/data.db" {
		t.Errorf("database %q, %v", db, err)
	}
}
