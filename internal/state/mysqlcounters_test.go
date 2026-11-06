package state

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/dbx"
)

// TestMySQLCountersOffWhy: without the grants the per-table counters need,
// the log says that fingerprints read every table after any write, and why:
// no PROCESS (SHOW ENGINE INNODB STATUS), no SELECT on performance_schema.
func TestMySQLCountersOffWhy(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, user := "dgt_counters_why", "dgt_noprocess"
			root := mysqlAdmin(t, dsn, "")
			var version string
			var userstat sql.NullInt64
			root.QueryRow("SELECT VERSION()").Scan(&version)
			mariadb := dbx.IsMariaDB(version)
			if mariadb {
				root.QueryRow("SELECT @@userstat").Scan(&userstat)
			}
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP USER IF EXISTS '"+user+"'@'%'",
				"CREATE DATABASE "+work, "CREATE TABLE "+work+".a (id INT PRIMARY KEY)", "CREATE USER '"+user+"'@'%' IDENTIFIED BY 'x'",
				"GRANT ALL ON "+work+".* TO '"+user+"'@'%'")
			t.Cleanup(func() {
				root.Exec("DROP DATABASE IF EXISTS " + work)
				root.Exec("DROP USER IF EXISTS '" + user + "'@'%'")
			})
			u.User = url.UserPassword(user, "x")
			mode := func() string {
				t.Helper()
				var log strings.Builder
				m, err := NewMySQL(Options{Name: "db", DSN: u.String(), Working: work, Log: &log})
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close()
				if _, err := m.Fingerprint(ctx, nil); err != nil {
					t.Fatal(err)
				}
				// Said once, and only what holds.
				if !strings.Contains(log.String(), m.Mode()) || strings.Count(log.String(), "state db: fingerprint") != 1 {
					t.Fatalf("log %q, mode %q", log.String(), m.Mode())
				}
				return m.Mode()
			}
			if !mariadb {
				if got := mode(); !strings.Contains(got, "no per-table counters") || !strings.Contains(got, "SELECT on performance_schema") {
					t.Fatalf("without SELECT on performance_schema: %q", got)
				}
				mustExec(t, root, "GRANT SELECT ON performance_schema.* TO '"+user+"'@'%'")
			} else if userstat.Int64 != 1 {
				t.Skip("userstat is OFF on this server: no per-table counters to turn off")
			}
			if got := mode(); !strings.Contains(got, "no per-table counters") || !strings.Contains(got, "PROCESS privilege") {
				t.Fatalf("without PROCESS: %q", got)
			}
			mustExec(t, root, "GRANT PROCESS ON *.* TO '"+user+"'@'%'")
			if got := mode(); !strings.HasPrefix(got, "fingerprint per table") {
				t.Fatalf("with the grants: %q", got)
			}
		})
	}
}
