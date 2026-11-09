//go:build unix

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// The SQLite tests need no server: the database is a file of the project,
// made by the test. The test reaches it through the "sqlite" driver the
// state package registers (only that package imports a driver). The demo
// API keeps one connection to the file open, as an application does, in
// another process than driftgate's.

// sqliteSchema is demo.Schema in SQLite's words, and a table the demo
// does not use, big enough (8 MB) that a restore of it cannot stay in
// SQLite's page cache until the commit: in rollback mode it writes the
// file, and the rollback journal, before.
var sqliteSchema = []string{
	"CREATE TABLE news (id INTEGER PRIMARY KEY, views INT NOT NULL)",
	"INSERT INTO news VALUES (11, 41), (12, 9)",
	"CREATE TABLE faq (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at DATETIME NOT NULL)",
	"CREATE TABLE faq_log (id INTEGER PRIMARY KEY AUTOINCREMENT, faq_id INT NOT NULL)",
	"CREATE TABLE big (id INTEGER PRIMARY KEY, b BLOB NOT NULL)",
	"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000) INSERT INTO big SELECT i, randomblob(4096) FROM n",
}

// sqliteURI is a file: URI of path with params.
func sqliteURI(path string, params ...string) string {
	uri := "file://" + strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	if len(params) > 0 {
		uri += "?" + strings.Join(params, "&")
	}
	return uri
}

// sqliteProject is a project on the demo API with its data in the SQLite
// file database/app.sqlite of the project in a journal mode (wal or
// delete), the project's state; db is the demo's one connection.
func sqliteProject(t *testing.T, mode, catalog string) *dbProject {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "database", "app.sqlite")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteURI(file, "mode=rwc", "_pragma=busy_timeout(10000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	sqlExec(t, db, "PRAGMA journal_mode = "+mode)
	sqlExec(t, db, sqliteSchema...)
	api := demo.NewSQL("v1", db)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p := newProject(t, srv.URL, "state:\n  db:\n    driver: sqlite\n    working: database/app.sqlite\n    row_tracking: [faq]\n", catalog, "")
	// The project folder is the one the test made the file in.
	for _, f := range []string{"driftgate.yaml", "driftgate"} {
		if err := os.Rename(filepath.Join(p.root, f), filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
	}
	p.root = root
	write(t, filepath.Join(p.root, "driftgate", "allowlist.yaml"), allowDate+allowCreated)
	p.api = api
	return &dbProject{project: p, db: db}
}

// sqliteData is the content of the demo's tables and of the AUTOINCREMENT
// counters, one line per row, and the SHA-256 of big's rows.
func sqliteData(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	big := sha256.New()
	for _, q := range []string{
		"SELECT 'news', id, views FROM news ORDER BY id",
		"SELECT 'faq', id, created_at FROM faq ORDER BY id",
		"SELECT 'faq_log', id, faq_id FROM faq_log ORDER BY id",
		"SELECT 'seq', name, seq FROM sqlite_sequence ORDER BY name",
		"SELECT 'big', id, b FROM big ORDER BY id",
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var a, b2, c any
			if err := rows.Scan(&a, &b2, &c); err != nil {
				t.Fatal(err)
			}
			if a == "big" {
				fmt.Fprintf(big, "%v %x\n", b2, c)
				continue
			}
			fmt.Fprintf(&b, "%v %v %v\n", a, b2, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	fmt.Fprintf(&b, "big %x\n", big.Sum(nil))
	return b.String()
}

// sqliteIntact fails the test unless SQLite finds the file whole.
func sqliteIntact(t *testing.T, db *sql.DB, what string) {
	t.Helper()
	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil || res != "ok" {
		t.Fatalf("%s: integrity_check %q %v", what, res, err)
	}
}

// sqlitePristine is the pristine file of the project's store.
func (p *dbProject) sqlitePristine(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(p.store, "pristine", "db-*.sqlite"))
	if len(files) != 1 {
		t.Fatalf("pristine files %v, want one", files)
	}
	return files[0]
}

// openPristine opens the pristine file read-only.
func openPristine(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteURI(path, "mode=ro"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestEndToEndSQLite is TestEndToEnd on SQLite: the demo API's data in a
// file it keeps open in WAL mode; seed, the first run that makes the
// baseline, runs on the same code, planted drift found by run and
// compare, accepted, and the baseline verified. Every run leaves the
// file as seeded.
func TestEndToEndSQLite(t *testing.T) {
	p := sqliteProject(t, "wal", stateCatalog)
	seeded := sqliteData(t, p.db)
	if r := p.must(t, 0, "seed"); !strings.Contains(r.out, "pristine copy of db: ") {
		t.Fatalf("seed: %v", r)
	}
	if r := p.must(t, 0, "run"); !strings.Contains(r.out, "baseline local made") {
		t.Fatalf("first run: %v", r)
	}
	p.must(t, 0, "run")
	p.must(t, 0, "verify")
	if got := sqliteData(t, p.db); got != seeded {
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
	p.must(t, 0, "run")
	if r := p.must(t, 0, "verify"); !strings.Contains(r.out, "1 acceptances") {
		t.Errorf("verify: %v", r)
	}
	sqliteIntact(t, p.db, "working file")
	if got := sqliteData(t, p.db); got != seeded {
		t.Errorf("the data after the runs on v2:\n%s\nwant as seeded:\n%s", got, seeded)
	}
	p.clean(t)
}

// TestCrashRestoreSQLite: a capture killed (SIGKILL) inside its restore,
// after every table was copied back and before the commit, in WAL and in
// rollback mode. The restore writes three tables in one transaction: the
// application's open connection then finds the file whole and every table
// as the application left it, none half restored (a rollback journal left
// behind is rolled back by the next reader). The next capture restores it
// and equals one before.
func TestCrashRestoreSQLite(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		for _, point := range []string{"state.sqlite.restore#1", "state.sqlite.restore#2"} {
			t.Run(mode+"/"+point, func(t *testing.T) {
				p := sqliteProject(t, mode, stateCatalog)
				p.must(t, 0, "seed")
				pristine := sqliteData(t, p.db)
				p.must(t, 0, "capture", "c1")
				// The application writes four tables; the next capture
				// starts from pristine.
				sqlExec(t, p.db, "INSERT INTO faq (created_at) VALUES ('2024-05-01 10:00:00'), ('2024-05-01 10:00:01')",
					"INSERT INTO faq_log (faq_id) VALUES (1)", "UPDATE news SET views = 1000", "UPDATE big SET b = zeroblob(4096) WHERE id = 1")
				written := sqliteData(t, p.db)
				// The first restore is the one at the start of the capture,
				// of what the application wrote; the second, before the
				// requests that change data, of the view news-item counted.
				old := written
				if point != "state.sqlite.restore#1" {
					old = strings.Replace(pristine, "news 12 9\n", "news 12 10\n", 1)
				}
				r := p.run(t, point, "capture", "c2")
				if r.code != killed {
					t.Fatalf("not killed: %v", r)
				}
				journal := filepath.Join(p.root, "database", "app.sqlite-journal")
				if _, err := os.Stat(journal); mode == "delete" && point == "state.sqlite.restore#1" && err != nil {
					t.Errorf("no rollback journal after a restore of 8 MB was killed: the kill tested nothing (%v)", err)
				}
				sqliteIntact(t, p.db, "working file after the kill")
				if got := sqliteData(t, p.db); got != old {
					t.Fatalf("the restore was killed before its commit, yet the data changed:\n%s\nwant as before:\n%s", got, old)
				}
				p.must(t, 0, "capture", "c2", "--force")
				p.must(t, 0, "compare", "c2", "--against", "c1")
				if got := sqliteData(t, p.db); got != pristine {
					t.Errorf("the data after the capture:\n%s\nwant pristine:\n%s", got, pristine)
				}
				sqliteIntact(t, openPristine(t, p.sqlitePristine(t)), "pristine file")
				// A journal the kill left (hot, or never made hot) is gone
				// with the next commit.
				if _, err := os.Stat(journal); err == nil {
					t.Errorf("app.sqlite-journal left after the next capture")
				}
				p.clean(t)
			})
		}
	}
}

// TestCrashSeedSQLite: a seed killed after the new pristine copy is made
// and the record of the old one removed, before the copy replaces the old
// file. The working file is untouched, the old pristine file is whole and
// as the first seed made it, and without its record a capture asks for a
// seed instead of trusting it; the next seed takes the place of the
// half-done one, and captures before and after agree.
func TestCrashSeedSQLite(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			p := sqliteProject(t, mode, stateCatalog)
			// Killed in the very first seed: no pristine copy at all.
			if r := p.run(t, "state.sqlite.seed", "seed"); r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			if r := p.must(t, 3, "capture", "c0"); !strings.Contains(r.err, "driftgate seed") {
				t.Errorf("a capture without a pristine copy: %v", r)
			}
			p.must(t, 0, "seed")
			pristineFile := p.sqlitePristine(t)
			first := sqliteData(t, openPristine(t, pristineFile))
			p.must(t, 0, "capture", "c1")

			// The application's data moves on; the seed of it is killed.
			sqlExec(t, p.db, "UPDATE news SET views = 77 WHERE id = 11")
			working := sqliteData(t, p.db)
			if r := p.run(t, "state.sqlite.seed", "seed"); r.code != killed {
				t.Fatalf("not killed: %v", r)
			}
			sqliteIntact(t, p.db, "working file after the kill")
			if got := sqliteData(t, p.db); got != working {
				t.Errorf("the seed changed the working file:\n%s\nwant:\n%s", got, working)
			}
			old := openPristine(t, pristineFile)
			sqliteIntact(t, old, "old pristine file")
			if got := sqliteData(t, old); got != first {
				t.Errorf("the old pristine file changed:\n%s\nwant:\n%s", got, first)
			}
			old.Close()
			if r := p.must(t, 3, "capture", "c2"); !strings.Contains(r.err, "driftgate seed") {
				t.Errorf("a capture after a killed seed: %v", r)
			}
			// The next seed replaces the old copy (it carries the mark) and
			// what the killed one left next to it.
			sqlExec(t, p.db, "UPDATE news SET views = 41 WHERE id = 11")
			p.must(t, 0, "seed")
			if left, _ := filepath.Glob(pristineFile + ".driftgate-new*"); len(left) > 0 {
				t.Errorf("left by the killed seed: %v", left)
			}
			sqliteIntact(t, openPristine(t, pristineFile), "new pristine file")
			p.must(t, 0, "capture", "c2")
			p.must(t, 0, "compare", "c2", "--against", "c1")
			p.clean(t)
		})
	}
}

// TestCrashPrepareSQLite: a capture killed (SIGKILL) in prepare, after its
// restore undid a migration run since the seed, leaves the record of what
// prepare must make again: the next capture, whose prepare does not make
// it, stops before any request instead of passing on the old schema.
func TestCrashPrepareSQLite(t *testing.T) {
	p := sqliteProject(t, "wal", stateCatalog)
	p.must(t, 0, "seed")
	sqlExec(t, p.db, "ALTER TABLE news ADD COLUMN title TEXT")
	cfg := filepath.Join(p.root, "driftgate.yaml")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, string(raw)+"prepare:\n  - [\"sh\", \"-c\", \"kill -9 $PPID\"]\n")
	if r := p.run(t, "", "capture", "c1"); r.code != killed {
		t.Fatalf("not killed: %v", r)
	}
	write(t, cfg, string(raw)+"prepare:\n  - [\"true\"]\n")
	if r := p.must(t, 3, "capture", "c1", "--force"); !strings.Contains(r.err, "db/table:news; the restore undid it, and prepare did not make it again") {
		t.Fatalf("capture after the kill: %v", r)
	}
}
