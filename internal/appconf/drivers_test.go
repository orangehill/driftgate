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
// %kernel.project_dir%, the project's folder.
func TestSQLiteSymfonyProjectDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DATABASE_URL=\"sqlite:///%kernel.project_dir%/var/data.db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL"}
	if _, db, err := r.Read(root, SQLite, nil); err != nil || db != "var/data.db" {
		t.Errorf("database %q, %v", db, err)
	}
}
