package appconf

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectedDrivers: a name of SQLite or SQL Server gives that driver
// once appconf reads its settings, and notes it as another database
// before; the other drivers are not affected.
func TestDetectedDrivers(t *testing.T) {
	for _, c := range []struct {
		name, driver, other string
		ready               bool
	}{
		{"pdo_sqlite", SQLite, "SQLite", sqliteReady},
		{"django.db.backends.sqlite3", SQLite, "SQLite", sqliteReady},
		{"org.sqlite.JDBC", SQLite, "SQLite", sqliteReady},
		{"sqlsrv", SQLServer, "SQL Server", sqlServerReady},
		{"mssql", SQLServer, "SQL Server", sqlServerReady},
		{"com.microsoft.sqlserver.jdbc.SQLServerDriver", SQLServer, "SQL Server", sqlServerReady},
		{"pgsql", Postgres, "", true},
		{"mysql2", MySQL, "", true},
	} {
		d, o := driverName(c.name)
		want, wantOther := c.driver, ""
		if !c.ready {
			want, wantOther = "", c.other
		}
		if d != want || o != wantOther {
			t.Errorf("%s: %q %q, want %q %q", c.name, d, o, want, wantOther)
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
// against working). Until the reading lands, the setting is refused.
func TestSQLiteSettingNamesFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("DATABASE_URL=sqlite:///database/app.sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, db, err := Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL"}.Read(root, SQLite, nil)
	switch {
	case !sqliteReady && err == nil:
		t.Error("an SQLite setting read before its reading landed")
	case sqliteReady && (err != nil || db != "database/app.sqlite"):
		t.Errorf("database %q, %v", db, err)
	}
}
