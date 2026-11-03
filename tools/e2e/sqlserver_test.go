//go:build unix

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// The SQL Server tests take the server from DRIFTGATE_TEST_SQLSERVER, a
// sqlserver:// URL of a login that may create databases (sa in CI); their
// databases are named dge2e_ss_*. They reach it through the drivers the
// state package registers: the demo API through "mssql", which takes its
// ? parameters, as an application keeps a pool of connections open.

// sqlServerSchema is demo.Schema in SQL Server's words, with sessions,
// which the application writes on every request and the configuration
// excludes, and big, a table over the size a restore copies whole (6000
// rows of 1 KB), whose rows a restore compares by key.
var sqlServerSchema = []string{
	"CREATE TABLE news (id int PRIMARY KEY, views int NOT NULL)",
	"INSERT INTO news VALUES (11, 41), (12, 9)",
	"CREATE TABLE faq (id int IDENTITY PRIMARY KEY, created_at datetime2(0) NOT NULL)",
	"CREATE TABLE faq_log (id int IDENTITY PRIMARY KEY, faq_id int NOT NULL)",
	"CREATE TABLE sessions (id int IDENTITY PRIMARY KEY, what nvarchar(100) NOT NULL)",
	"CREATE TABLE big (id int PRIMARY KEY, b varbinary(max) NOT NULL)",
	`WITH n AS (SELECT TOP (6000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects a CROSS JOIN sys.all_objects b)
	 INSERT INTO big SELECT i, CRYPT_GEN_RANDOM(1024) FROM n`,
}

// sqlServerE2E is the test server's DSN.
func sqlServerE2E(t *testing.T) string {
	t.Helper()
	v := os.Getenv("DRIFTGATE_TEST_SQLSERVER")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_SQLSERVER not set")
	}
	return v
}

// openSQLServer opens database db of the server of dsn with the driver
// driver ("sqlserver", or "mssql" for ? parameters).
func openSQLServer(t *testing.T, driver, dsn, db string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", db)
	u.RawQuery = q.Encode()
	d, err := sql.Open(driver, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// ssProject is a project on the demo API with its data in the SQL Server
// database dge2e_ss_<name>; master is the server's master database, as
// the test login.
type ssProject struct {
	*dbProject
	work   string
	master *sql.DB
}

// sqlServerProject makes the database dge2e_ss_<name> with the demo's
// tables, and a project on the demo API with it as the state db; it is
// removed at the end with what a seed and a restore of it leave.
func sqlServerProject(t *testing.T, name string) *ssProject {
	t.Helper()
	dsn := sqlServerE2E(t)
	work := "dge2e_ss_" + name
	master := openSQLServer(t, "sqlserver", dsn, "master")
	drop := func() {
		rows, err := master.Query(`SELECT DISTINCT f.physical_device_name FROM msdb.dbo.backupset s
			JOIN msdb.dbo.backupmediafamily f ON f.media_set_id = s.media_set_id WHERE s.database_name LIKE @p1`, work+"%")
		if err == nil {
			var files []string
			for rows.Next() {
				var f string
				if rows.Scan(&f) == nil {
					files = append(files, f)
				}
			}
			rows.Close()
			for _, f := range files {
				master.Exec("EXEC master.sys.xp_delete_file 0, N'" + strings.ReplaceAll(f, "'", "''") + "'")
			}
		}
		for _, db := range []string{work, work + "_pristine", work + "_pristine_dgnew", work + "_pristine_dgx"} {
			// An OFFLINE database is brought online first: DROP of an
			// offline one leaves its files behind.
			master.Exec("IF DB_ID(N'" + db + "') IS NOT NULL BEGIN ALTER DATABASE [" + db + "] SET ONLINE; ALTER DATABASE [" + db +
				"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [" + db + "] END")
		}
	}
	drop()
	t.Cleanup(drop)
	sqlExec(t, master, "CREATE DATABASE ["+work+"]")
	db := openSQLServer(t, "mssql", dsn, work)
	sqlExec(t, db, sqlServerSchema...)
	api := demo.NewSQL("v1", db)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	state := fmt.Sprintf("state:\n  db:\n    driver: sqlserver\n    dsn: { env: DG_E2E_SQLSERVER }\n    working: %s\n    pristine: %s_pristine\n    exclude: [sessions]\n    row_tracking: [faq]\n", work, work)
	p := newProject(t, srv.URL, state, stateCatalog, "")
	write(t, p.root+"/driftgate/allowlist.yaml", allowDate+allowCreated)
	p.api = api
	p.env = append(p.env, "DG_E2E_SQLSERVER="+dsn)
	return &ssProject{dbProject: &dbProject{project: p, db: db}, work: work, master: master}
}

// data is the content of the demo's tables, their identity values and
// the SHA-256 of big's rows, one line each; sessions is left out (it is
// excluded: count it with sessions).
func (p *ssProject) data(t *testing.T) string {
	t.Helper()
	db := openSQLServer(t, "sqlserver", sqlServerE2E(t), p.work)
	defer db.Close()
	var b strings.Builder
	big := sha256.New()
	for _, q := range []string{
		"SELECT 'news', id, CAST(views AS nvarchar(20)) FROM news ORDER BY id",
		"SELECT 'faq', id, CONVERT(nvarchar(30), created_at, 126) FROM faq ORDER BY id",
		"SELECT 'faq_log', id, CAST(faq_id AS nvarchar(20)) FROM faq_log ORDER BY id",
		"SELECT 'ident', 0, CONCAT(IDENT_CURRENT('faq'), '/', IDENT_CURRENT('faq_log'))",
		"SELECT 'big', id, CONVERT(nvarchar(64), HASHBYTES('SHA2_256', b), 2) FROM big ORDER BY id",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var what, v string
			var id int
			if err := rows.Scan(&what, &id, &v); err != nil {
				t.Fatal(err)
			}
			if what == "big" {
				fmt.Fprintf(big, "%d %s\n", id, v)
				continue
			}
			fmt.Fprintf(&b, "%s %d %s\n", what, id, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	fmt.Fprintf(&b, "big %x\n", big.Sum(nil))
	return b.String()
}

// sessions counts the rows of the excluded table.
func (p *ssProject) sessions(t *testing.T) int {
	t.Helper()
	var n int
	if err := p.master.QueryRow("SELECT COUNT(*) FROM [" + p.work + "].dbo.sessions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// status is a database's state, user access and read-only flag ("" when
// there is none).
func (p *ssProject) status(t *testing.T, db string) string {
	t.Helper()
	var state, access string
	var ro bool
	err := p.master.QueryRow("SELECT state_desc, user_access_desc, is_read_only FROM sys.databases WHERE name = @p1", db).Scan(&state, &access, &ro)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s %s read-only=%v", state, access, ro)
}

const (
	ssWritable = "ONLINE MULTI_USER read-only=false"
	ssPristine = "ONLINE MULTI_USER read-only=true"
)

// usable fails the test unless the working database is online for the
// application, the pristine copy is online and read-only, and nothing a
// seed or a restore makes for a while is left.
func (p *ssProject) usable(t *testing.T) {
	t.Helper()
	if s := p.status(t, p.work); s != ssWritable {
		t.Errorf("working database: %s, want %s", s, ssWritable)
	}
	if s := p.status(t, p.work+"_pristine"); s != ssPristine {
		t.Errorf("pristine database: %s, want %s", s, ssPristine)
	}
	for _, db := range []string{p.work + "_pristine_dgnew", p.work + "_pristine_dgx"} {
		if s := p.status(t, db); s != "" {
			t.Errorf("%s left: %s", db, s)
		}
	}
}

// TestEndToEndSQLServer is TestEndToEnd on SQL Server: the demo API's data
// in a database it keeps a pool of connections open to; seed, the first
// run that makes the baseline, runs on the same code, planted drift found
// by run and compare, accepted, and the baseline verified. Every run
// leaves the data as seeded, and the excluded table as the application
// wrote it.
func TestEndToEndSQLServer(t *testing.T) {
	p := sqlServerProject(t, "e2e")
	seeded := p.data(t)
	if r := p.must(t, 0, "seed"); !strings.Contains(r.out, "pristine copy of db: ") {
		t.Fatalf("seed: %v", r)
	}
	p.usable(t)
	if r := p.must(t, 0, "run"); !strings.Contains(r.out, "baseline local made") {
		t.Fatalf("first run: %v", r)
	}
	p.must(t, 0, "run")
	p.must(t, 0, "verify")
	if got := p.data(t); got != seeded {
		t.Errorf("the data after the runs:\n%s\nwant as seeded:\n%s", got, seeded)
	}

	p.api.SetVersion("v2")
	r := p.must(t, 1, "run")
	for _, want := range []string{"news", "x-powered-by", "faq_log"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("run on v2 does not report %s:\n%s", want, r.out)
		}
	}
	p.must(t, 0, "capture", "v2")
	p.must(t, 1, "compare", "v2")
	p.must(t, 0, "accept", "*", "--reason", "v2 is intended")
	sqlExec(t, p.db, "INSERT INTO sessions (what) VALUES ('written by the application')")
	p.must(t, 0, "run")
	if r := p.must(t, 0, "verify"); !strings.Contains(r.out, "1 acceptances") {
		t.Errorf("verify: %v", r)
	}
	if got := p.data(t); got != seeded {
		t.Errorf("the data after the runs on v2:\n%s\nwant as seeded:\n%s", got, seeded)
	}
	if n := p.sessions(t); n != 1 {
		t.Errorf("%d session rows, want the 1 the application wrote (excluded)", n)
	}
	p.usable(t)
	p.clean(t)
}

// ssWrite is what the application writes before a capture: rows of the
// demo's tables, a row of big, and a row of the excluded table.
var ssWrite = []string{
	"INSERT INTO faq (created_at) VALUES ('2024-05-01 10:00:00'), ('2024-05-01 10:00:01')",
	"INSERT INTO faq_log (faq_id) VALUES (1)",
	"UPDATE news SET views = 1000",
	"UPDATE big SET b = 0x00 WHERE id = 1",
	"INSERT INTO sessions (what) VALUES ('written by the application')",
}

// TestCrashRestoreSQLServer: a capture killed (SIGKILL) inside its first
// restore. Restored table by table, at the point before the commit: the
// server rolls the transaction back, and the application finds every
// table as it left it. Restored as a whole database (the application made
// a view), after the excluded table was saved, after the database was
// taken offline, and after it was restored before the excluded rows came
// back: the next capture brings the database online and as pristine, with
// the excluded rows the application wrote. Every time, that capture equals
// one before, the working database is online and writable, and the
// pristine copy online and read-only.
func TestCrashRestoreSQLServer(t *testing.T) {
	for i, tc := range []struct {
		point string
		view  bool // the application makes a view: the whole database is restored
		// after are the states of the working database the kill may leave.
		after []string
	}{
		{"state.sqlserver.restore", false, []string{ssWritable}},
		{"state.sqlserver.restore.saved", true, []string{ssWritable}},
		{"state.sqlserver.restore.offline", true, []string{"OFFLINE MULTI_USER read-only=false"}},
		{"state.sqlserver.restore.restored", true, []string{ssWritable}},
	} {
		t.Run(tc.point, func(t *testing.T) {
			if i > 0 {
				full(t)
			}
			p := sqlServerProject(t, fmt.Sprintf("restore%d", i))
			if tc.view {
				// A restore of the whole database ends the application's
				// sessions: an idle connection kept would fail the first
				// request after it (the demo answers it with 0 views). The
				// capture tests cover a pool that connects again.
				p.db.SetMaxIdleConns(0)
			}
			p.must(t, 0, "seed")
			pristine := p.data(t)
			p.must(t, 0, "capture", "c1")
			sessions := p.sessions(t)
			sqlExec(t, p.db, ssWrite...)
			if tc.view {
				sqlExec(t, p.db, "CREATE VIEW faq_count AS SELECT COUNT(*) AS n FROM dbo.faq")
			}
			written := p.data(t)
			r := p.run(t, tc.point, "capture", "c2")
			if r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			status := p.status(t, p.work)
			if !slices.Contains(tc.after, status) {
				t.Fatalf("working database after the kill: %s, want one of %v", status, tc.after)
			}
			if status == ssWritable {
				// Before the commit, or before the database went offline,
				// nothing changed; after the restore, it is pristine.
				got := p.data(t)
				want := written
				if tc.point == "state.sqlserver.restore.restored" {
					want = pristine
				}
				if got != want {
					t.Fatalf("the data after the kill:\n%s\nwant:\n%s", got, want)
				}
			}
			p.must(t, 0, "capture", "c2", "--force")
			p.must(t, 0, "compare", "c2", "--against", "c1")
			if got := p.data(t); got != pristine {
				t.Errorf("the data after the capture:\n%s\nwant pristine:\n%s", got, pristine)
			}
			var view sql.NullInt64
			if err := p.master.QueryRow("SELECT OBJECT_ID(N'[" + p.work + "].dbo.faq_count')").Scan(&view); err != nil || view.Valid {
				t.Errorf("the application's view is left (%v)", err)
			}
			if n := p.sessions(t); n < sessions+1 {
				t.Errorf("%d session rows after the capture, want the %d before and the one written (excluded)", n, sessions)
			}
			p.usable(t)
			p.clean(t)
		})
	}
}

// TestCrashSeedSQLServer: a seed of changed data killed at each of its
// points. The working database is untouched, online and writable. Killed
// before the old pristine copy is replaced (after the backup, the restore
// as <pristine>_dgnew, its mark), the old copy and its record stay, and a
// capture restores the old data; killed in the swap (the old copy and its
// record gone, the new one not renamed yet), a capture asks for a seed
// instead of trusting anything. The next seed replaces what the killed
// one left, and captures before and after agree.
func TestCrashSeedSQLServer(t *testing.T) {
	for i, point := range []string{"state.sqlserver.seed.swap", "state.sqlserver.seed.backup", "state.sqlserver.seed.restored", "state.sqlserver.seed.marked"} {
		t.Run(point, func(t *testing.T) {
			if i > 0 {
				full(t)
			}
			p := sqlServerProject(t, fmt.Sprintf("seed%d", i))
			p.must(t, 0, "seed")
			first := p.data(t)
			p.must(t, 0, "capture", "c1")

			sqlExec(t, p.db, "UPDATE news SET views = 77 WHERE id = 11")
			working := p.data(t)
			if r := p.run(t, point, "seed"); r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			if s := p.status(t, p.work); s != ssWritable {
				t.Errorf("working database after the kill: %s", s)
			}
			if got := p.data(t); got != working {
				t.Errorf("the seed changed the working database:\n%s\nwant:\n%s", got, working)
			}
			if point == "state.sqlserver.seed.swap" {
				if r := p.must(t, 3, "capture", "c2"); !strings.Contains(r.err, "driftgate seed") {
					t.Errorf("a capture after a seed killed in the swap: %v", r)
				}
			} else {
				if s := p.status(t, p.work+"_pristine"); s != ssPristine {
					t.Errorf("old pristine database after the kill: %s", s)
				}
				p.must(t, 0, "capture", "c0")
				if got := p.data(t); got != first {
					t.Errorf("a capture after the killed seed restored:\n%s\nwant the old pristine data:\n%s", got, first)
				}
			}
			// The next seed takes the place of the killed one.
			sqlExec(t, p.db, "UPDATE news SET views = 41 WHERE id = 11")
			p.must(t, 0, "seed")
			p.usable(t)
			p.must(t, 0, "capture", "c2", "--force")
			p.must(t, 0, "compare", "c2", "--against", "c1")
			if got := p.data(t); got != first {
				t.Errorf("the data after the capture:\n%s\nwant:\n%s", got, first)
			}
			p.clean(t)
		})
	}
}
