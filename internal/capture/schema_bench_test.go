package capture

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// benchTables is the number of tables of benchSchema.
const benchTables = 80

// benchSchema is a schema an application may have, for driver: 80 tables
// with two indexes and 20 rows each, five views, five triggers and, on
// PostgreSQL, a sequence.
func benchSchema(driver string) []string {
	var out []string
	switch driver {
	case config.Postgres:
		out = append(out, "CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'", "CREATE SEQUENCE numbers")
	}
	for i := range benchTables {
		switch driver {
		case config.MySQL:
			out = append(out, fmt.Sprintf("CREATE TABLE t%d (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50), n INT, at DATETIME, KEY t%d_n (n), KEY t%d_name (name))", i, i, i))
		case config.Postgres:
			out = append(out, fmt.Sprintf("CREATE TABLE t%d (id serial PRIMARY KEY, name text, n int, at timestamp)", i),
				fmt.Sprintf("CREATE INDEX t%d_n ON t%d (n)", i, i), fmt.Sprintf("CREATE INDEX t%d_name ON t%d (name)", i, i))
		default:
			out = append(out, fmt.Sprintf("CREATE TABLE t%d (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, n INT, at TEXT)", i),
				fmt.Sprintf("CREATE INDEX t%d_n ON t%d (n)", i, i), fmt.Sprintf("CREATE INDEX t%d_name ON t%d (name)", i, i))
		}
		rows := make([]string, 20)
		for r := range rows {
			rows[r] = fmt.Sprintf("('row %d', %d, '2024-01-01 00:00:00')", r, r)
		}
		out = append(out, fmt.Sprintf("INSERT INTO t%d (name, n, at) VALUES %s", i, strings.Join(rows, ", ")))
	}
	for i := range 5 {
		out = append(out, fmt.Sprintf("CREATE VIEW v%d AS SELECT id, name FROM t%d", i, i))
		switch driver {
		case config.MySQL:
			out = append(out, fmt.Sprintf("CREATE TRIGGER tr%d BEFORE INSERT ON t%d FOR EACH ROW SET NEW.n = COALESCE(NEW.n, 0)", i, i))
		case config.Postgres:
			out = append(out, fmt.Sprintf("CREATE TRIGGER tr%d BEFORE INSERT ON t%d FOR EACH ROW EXECUTE FUNCTION touch()", i, i))
		default:
			out = append(out, fmt.Sprintf("CREATE TRIGGER tr%d AFTER INSERT ON t%d BEGIN SELECT 1; END", i, i))
		}
	}
	return out
}

// benchProject makes the working database of benchSchema for driver (dsn:
// a server, or "" for SQLite), seeds it, and returns the project and what
// runs a statement on the working database, on a new connection to a
// server (a PostgreSQL restore ends the sessions).
func benchProject(b *testing.B, driver, dsn string) (*config.Project, func(string) error) {
	b.Helper()
	b.Setenv(config.EnvStore, filepath.Join(b.TempDir(), "store"))
	root := b.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) }))
	b.Cleanup(srv.Close)
	sc := config.Component{Driver: driver, Working: "app.sqlite"}
	var db *sql.DB
	var err error
	open := func(string, string, dbx.DialFunc) (*sql.DB, error) { return db, nil }
	if driver == config.SQLite {
		db, err = sql.Open("sqlite", sqliteTestURI(filepath.Join(root, "app.sqlite"), "_pragma=busy_timeout(10000)"))
	} else {
		b.Setenv("DG_BENCH_DSN", dsn)
		sc = config.Component{Driver: driver, DSN: config.Secret{Env: "DG_BENCH_DSN"}, Working: "dgt_bench_schema", Pristine: "dgt_bench_schema_pristine"}
		suffix := ""
		open = dbx.OpenMySQL
		if driver == config.Postgres {
			open, suffix = dbx.OpenPostgres, " WITH (FORCE)"
		}
		admin, err := open(dsn, "", nil)
		if err != nil {
			b.Fatal(err)
		}
		drop := func() {
			admin.Exec("DROP DATABASE IF EXISTS " + sc.Working + suffix)
			admin.Exec("DROP DATABASE IF EXISTS " + sc.Pristine + suffix)
		}
		drop()
		b.Cleanup(func() { drop(); admin.Close() })
		if _, err := admin.Exec("CREATE DATABASE " + sc.Working); err != nil {
			b.Fatal(err)
		}
		db, err = open(dsn, sc.Working, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	for _, s := range benchSchema(driver) {
		if _, err := db.Exec(s); err != nil {
			b.Fatalf("%s: %v", s, err)
		}
	}
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Evolve, Level: "values",
		Target: config.Target{BaseURL: srv.URL}, State: map[string]config.Component{"db": sc}}, root)
	if err != nil {
		b.Fatal(err)
	}
	if err := Seed(context.Background(), p, SeedOptions{}); err != nil {
		b.Fatal(err)
	}
	exec := func(stmt string) error {
		c, err := open(dsn, sc.Working, nil)
		if err != nil {
			return err
		}
		if driver != config.SQLite {
			defer c.Close()
		}
		_, err = c.Exec(stmt)
		return err
	}
	return p, exec
}

// BenchmarkCaptureSchema is a capture of one read-only request after the
// application wrote a row since the last one, on benchSchema: its first
// restore, which on fix/schema-check also compares the working schema with
// the pristine copy's. It runs the same on development, for the cost of
// the schema check: SQLite always, MySQL/MariaDB on the first server of
// DRIFTGATE_TEST_MYSQL, PostgreSQL on DRIFTGATE_TEST_POSTGRES. Put
// tools/latencyproxy in front of a server for the cost of round trips.
func BenchmarkCaptureSchema(b *testing.B) {
	servers := map[string]string{config.SQLite: ""}
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers[config.MySQL], _, _ = strings.Cut(v, ",")
	}
	if v := os.Getenv("DRIFTGATE_TEST_POSTGRES"); v != "" {
		servers[config.Postgres] = v
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}}}
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			p, exec := benchProject(b, driver, dsn)
			ctx := context.Background()
			for i := range b.N {
				b.StopTimer()
				if err := exec(fmt.Sprintf("INSERT INTO t%d (name, n, at) VALUES ('new', 1, '2024-01-02 00:00:00')", i%benchTables)); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := Run(ctx, p, cat, Options{Tag: "bench", Force: true}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
