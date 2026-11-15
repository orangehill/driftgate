//go:build unix

package e2e

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/state"
)

// The databases of these tests are named dge2e_*: no other test package
// uses them, so the packages may run at the same time on one server.

// mysqlServer is the first server of DRIFTGATE_TEST_MYSQL.
func mysqlServer(t *testing.T) string {
	t.Helper()
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	return strings.Split(v, ",")[0]
}

func openMySQL(t *testing.T, dsn, db string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mysql.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net, cfg.Addr, cfg.DBName = "tcp", u.Host, db
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := sql.OpenDB(conn)
	t.Cleanup(func() { d.Close() })
	return d
}

func sqlExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		// On SQL Server 2019 and earlier another test's RESTORE may stop a
		// statement (927, 952); MySQL and PostgreSQL never return those.
		if err := state.RetryInTransition(func() error { _, err := db.Exec(s); return err }); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stateCatalog reads and changes data of the demo API.
const stateCatalog = `{"version":1,"requests":[
 {"id":"news","method":"GET","path":"/api/news","storage":"public"},
 {"id":"news-item","method":"GET","path":"/api/news/12","storage":"public"},
 {"id":"faq-a","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"},
 {"id":"faq-b","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"}]}`

const allowCreated = `
body:
  - request: faq-*
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Time of creation.
`

// dbProject is a project whose state is a database; db is its working
// database.
type dbProject struct {
	*project
	db *sql.DB
}

// mysqlDatabase makes the database dge2e_<name> on the first MySQL server,
// with the demo's tables; it is removed with its pristine copy at the end.
func mysqlDatabase(t *testing.T, name string) (dsn string, db *sql.DB) {
	t.Helper()
	dsn = mysqlServer(t)
	work := "dge2e_" + name
	admin := openMySQL(t, dsn, "")
	drop := func() {
		admin.Exec("DROP DATABASE IF EXISTS " + work)
		admin.Exec("DROP DATABASE IF EXISTS " + work + "_pristine")
	}
	drop()
	t.Cleanup(drop)
	sqlExec(t, admin, "CREATE DATABASE "+work)
	db = openMySQL(t, dsn, work)
	sqlExec(t, db, demo.Schema...)
	return dsn, db
}

// postgresDatabase makes the database dge2e_<name> on DRIFTGATE_TEST_POSTGRES
// with a table audit of one row; it is removed with its copies at the end.
func postgresDatabase(t *testing.T, name string) (dsn string, db *sql.DB) {
	t.Helper()
	dsn = os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	work := "dge2e_" + name
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	drop := func() {
		for _, db := range []string{work, work + "_pristine", work + "__driftgate_new", work + "_pristine__driftgate_new"} {
			admin.Exec("DROP DATABASE IF EXISTS " + db + " WITH (FORCE)")
		}
	}
	drop()
	t.Cleanup(drop)
	sqlExec(t, admin, "CREATE DATABASE "+work)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + work
	if db, err = sql.Open("pgx", u.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxIdleConns(0) // a seed or restore ends the sessions on the database
	sqlExec(t, db, "CREATE TABLE audit (id serial PRIMARY KEY, what text NOT NULL)", "INSERT INTO audit (what) VALUES ('seeded')")
	return dsn, db
}

// mysqlState and postgresState configure the databases as state components.
func mysqlState(name string) string {
	return fmt.Sprintf("  db:\n    driver: mysql\n    dsn: { env: DG_E2E_MYSQL }\n    working: dge2e_%s\n    pristine: dge2e_%s_pristine\n", name, name)
}

func postgresState(name string) string {
	return fmt.Sprintf("  pg:\n    driver: postgres\n    dsn: { env: DG_E2E_POSTGRES }\n    working: dge2e_%s\n    pristine: dge2e_%s_pristine\n", name, name)
}

// mysqlProject is a project on the demo API with its data in a MySQL or
// MariaDB database, the project's state.
func mysqlProject(t *testing.T, name string) *dbProject {
	t.Helper()
	dsn, db := mysqlDatabase(t, name)
	api := demo.NewSQL("v1", db)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p := newProject(t, srv.URL, "state:\n"+mysqlState(name), stateCatalog, "")
	write(t, filepath.Join(p.root, "driftgate", "allowlist.yaml"), allowDate+allowCreated)
	p.api = api
	p.env = append(p.env, "DG_E2E_MYSQL="+dsn)
	return &dbProject{project: p, db: db}
}

// postgresProject is a project of reading requests on the demo API (data
// in memory) whose state is a PostgreSQL database the test writes to.
func postgresProject(t *testing.T, name string) *dbProject {
	t.Helper()
	dsn, db := postgresDatabase(t, name)
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p := newProject(t, srv.URL, "state:\n"+postgresState(name), readOnly(8), "")
	p.api = api
	p.env = append(p.env, "DG_E2E_POSTGRES="+dsn)
	return &dbProject{project: p, db: db}
}
