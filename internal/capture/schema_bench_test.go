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
	"github.com/orangehill/driftgate/internal/state"
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
func benchProject(b testing.TB, driver, dsn string) (*config.Project, func(string) error) {
	b.Helper()
	b.Setenv(config.EnvStore, filepath.Join(b.TempDir(), "store"))
	root := b.TempDir()
	// A POST writes a row, as a request that changes data does.
	var exec func(string) error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if err := exec("INSERT INTO t0 (name, n, at) VALUES ('post', 1, '2024-01-03 00:00:00')"); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		w.Write([]byte(`{"ok":true}`))
	}))
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
	exec = func(stmt string) error {
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

// benchServers are the servers of the benchmarks by driver: SQLite always
// (no server), MySQL/MariaDB on the first server of DRIFTGATE_TEST_MYSQL,
// PostgreSQL on DRIFTGATE_TEST_POSTGRES.
func benchServers() map[string]string {
	servers := map[string]string{config.SQLite: ""}
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		servers[config.MySQL], _, _ = strings.Cut(v, ",")
	}
	if v := os.Getenv("DRIFTGATE_TEST_POSTGRES"); v != "" {
		servers[config.Postgres] = v
	}
	return servers
}

// BenchmarkCaptureSchema is a capture of one read-only request after the
// application wrote a row since the last one, on benchSchema: its first
// restore, which on fix/schema-check also compares the working schema with
// the pristine copy's. It runs the same on development, for the cost of
// the schema check, on benchServers. Put
// tools/latencyproxy in front of a server for the cost of round trips.
func BenchmarkCaptureSchema(b *testing.B) {
	servers := benchServers()
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

// BenchmarkCaptureWrite is a capture of requests that write a row, on
// benchSchema unchanged since the last capture, as in local work: the
// first restore copies nothing. plain: a read-only request and one that
// writes, whose final restore copies what it wrote (PostgreSQL: the whole
// database). prepare: two that write, with --restore each and prepare:,
// whose reset before the second copies it. On benchServers.
func BenchmarkCaptureWrite(b *testing.B) {
	servers := benchServers()
	get := catalog.Request{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}
	post := func(id string) catalog.Request {
		return catalog.Request{ID: id, Method: "POST", Path: "/", Storage: snapshot.Public}
	}
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		for _, c := range []struct {
			name    string
			reqs    []catalog.Request
			prepare bool
		}{{"plain", []catalog.Request{get, post("post")}, false}, {"prepare", []catalog.Request{post("post1"), post("post2")}, true}} {
			b.Run(driver+"/"+c.name, func(b *testing.B) {
				p, _ := benchProject(b, driver, dsn)
				opt := Options{Tag: "bench", Force: true}
				if c.prepare {
					p.Prepare = []config.Command{{Run: []string{"true"}}}
					opt.Restore = "each"
				}
				cat := &catalog.Catalog{Version: 1, Requests: c.reqs}
				ctx := context.Background()
				for range b.N {
					if _, err := Run(ctx, p, cat, opt); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkSchemaFingerprint is one SchemaFingerprint of benchSchema, of the
// pristine copy and of the working database: what checkSchema reads per
// component before the first restore (both) and checkPrepared after
// prepare (the working one). On benchServers.
func BenchmarkSchemaFingerprint(b *testing.B) {
	servers := benchServers()
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			p, _ := benchProject(b, driver, dsn)
			comps, err := openComponents(context.Background(), p, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer comps[0].Close()
			s := comps[0].(state.Schemer)
			for _, side := range []struct {
				name     string
				pristine bool
			}{{"pristine", true}, {"working", false}} {
				b.Run(side.name, func(b *testing.B) {
					for range b.N {
						if f, err := s.SchemaFingerprint(context.Background(), side.pristine); err != nil || len(f) < benchTables {
							b.Fatal(len(f), err)
						}
					}
				})
			}
		})
	}
}

// BenchmarkCheckSchema is the schema check before a capture's first
// restore on benchSchema, unchanged since the seed: cached, with the
// pristine schema the seed recorded (one read, of the working database);
// live, without it (both read). On benchServers.
func BenchmarkCheckSchema(b *testing.B) {
	servers := benchServers()
	for _, driver := range []string{config.SQLite, config.MySQL, config.Postgres} {
		dsn, ok := servers[driver]
		if !ok {
			continue
		}
		b.Run(driver, func(b *testing.B) {
			p, _ := benchProject(b, driver, dsn)
			tr, err := newTracker(context.Background(), p, nil, nil, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer tr.close()
			cached := tr.schemas
			for _, mode := range []string{"cached", "live"} {
				b.Run(mode, func(b *testing.B) {
					tr.schemas = cached
					if mode == "live" {
						tr.schemas = states{}
					}
					for range b.N {
						if err := tr.checkSchema(context.Background()); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
