package state

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
)

// sqliteSchema is an application's database: a table with AUTOINCREMENT,
// a DATETIME column and an index, one with a generated column and a
// foreign key that cascades, one WITHOUT ROWID, one whose rowids are
// hidden (with gaps), an audit table its triggers write, a session table,
// a view and a full-text index.
var sqliteSchema = []string{
	`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL UNIQUE, created_at DATETIME, score REAL, avatar BLOB)`,
	`CREATE INDEX users_created ON users (created_at)`,
	`CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users (id) ON DELETE CASCADE, title TEXT, words INTEGER GENERATED ALWAYS AS (length(title)) VIRTUAL)`,
	`CREATE TABLE tags (name TEXT PRIMARY KEY, n INT, extra) WITHOUT ROWID`,
	`CREATE TABLE log (msg TEXT)`,
	`CREATE TABLE audit (what TEXT)`,
	`CREATE TABLE sessions (id TEXT PRIMARY KEY, payload TEXT)`,
	`CREATE VIEW user_posts AS SELECT u.email, p.title FROM users u JOIN posts p ON p.user_id = u.id`,
	`CREATE TRIGGER users_audit AFTER INSERT ON users BEGIN INSERT INTO audit VALUES ('user ' || NEW.email); END`,
	`CREATE TRIGGER users_gone AFTER DELETE ON users BEGIN INSERT INTO audit VALUES ('deleted ' || OLD.email); END`,
	`CREATE TRIGGER sessions_audit AFTER INSERT ON sessions BEGIN INSERT INTO audit VALUES ('session'); END`,
	`CREATE VIRTUAL TABLE docs USING fts5 (body)`,
	`INSERT INTO users (email, created_at, score, avatar) VALUES ('a@x', '2024-01-02 03:04:05', 1.5, x'00ff'), ('b@x', '2024-02-03 04:05:06', NULL, NULL)`,
	`INSERT INTO posts (user_id, title) VALUES (1, 'hello'), (1, 'world'), (2, 'again')`,
	`INSERT INTO tags VALUES ('go', 1, 1), ('sql', 2, 'ab')`,
	`INSERT INTO log (rowid, msg) VALUES (3, 'three'), (7, 'seven'), (20, 'twenty')`,
	`INSERT INTO sessions VALUES ('s1', 'x')`,
	`INSERT INTO docs VALUES ('the quick brown fox'), ('jumps over')`,
	`DELETE FROM audit`,
	`PRAGMA user_version = 3`,
}

// sqliteApp opens a database file the way an application does: with its
// own connection, kept open (the file is made if missing).
func sqliteApp(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sqliteOpen(path, []string{"mode=rwc", "_pragma=busy_timeout(10000)"})
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// sqliteFixture makes the working file in a journal mode (WAL: the
// application's commits stay in the -wal file, no checkpoint), seeds the
// pristine file and returns the component and the application's
// connection.
func sqliteFixture(t *testing.T, mode string, o Options) (*SQLite, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	o.Working, o.Pristine = filepath.Join(dir, "app.sqlite"), filepath.Join(dir, "store", "pristine.sqlite")
	os.MkdirAll(filepath.Dir(o.Pristine), 0o755)
	if o.Name == "" {
		o.Name = "db"
	}
	if o.Exclude == nil {
		o.Exclude = []string{"audit", "sessions"}
	}
	app := sqliteApp(t, o.Working)
	mustExec(t, app, "PRAGMA journal_mode = "+mode, "PRAGMA wal_autocheckpoint = 0")
	mustExec(t, app, sqliteSchema...)
	s, err := NewSQLite(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, app
}

func sqliteFingerprint(t testing.TB, s *SQLite) Fingerprint {
	t.Helper()
	f, err := s.Fingerprint(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sqliteScratch is the fingerprint of the working file read from scratch,
// by a component that keeps no row hash or fingerprint from before.
func sqliteScratch(t testing.TB, s *SQLite) Fingerprint {
	t.Helper()
	fresh, err := NewSQLite(s.o)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	return sqliteFingerprint(t, fresh)
}

func sqlitePristineFP(t testing.TB, s *SQLite) Fingerprint {
	t.Helper()
	f, err := s.pristineFingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func count(t testing.TB, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestSQLiteFingerprintAndRestore: every kind of change the application
// can make shows as exactly the items it changed, in WAL and in rollback
// mode, and only the tables whose pages changed are read again; a restore
// of those items brings the whole file back to the pristine fingerprint,
// without firing a trigger or cascading a delete.
func TestSQLiteFingerprintAndRestore(t *testing.T) {
	cases := []struct {
		name  string
		stmts []string
		want  []string // nil: no change; "table:docs_*": some shadow tables of docs
	}{
		{"insert", []string{"INSERT INTO tags VALUES ('new', 3, NULL)"}, []string{"table:tags"}},
		{"update", []string{"UPDATE posts SET title = 'HELLO' WHERE id = 1"}, []string{"table:posts"}},
		{"delete", []string{"DELETE FROM log WHERE msg = 'seven'"}, []string{"table:log"}},
		{"same value", []string{"UPDATE tags SET n = 1 WHERE name = 'go'"}, nil},
		{"integer to text", []string{"UPDATE tags SET extra = '1' WHERE name = 'go'"}, []string{"table:tags"}},
		{"integer to real", []string{"UPDATE tags SET extra = 1.0 WHERE name = 'go'"}, []string{"table:tags"}},
		{"last bit of a real", []string{"UPDATE users SET score = 1.5000000000000002 WHERE id = 1"}, []string{"table:users"}},
		{"text to blob", []string{"UPDATE tags SET extra = CAST('ab' AS BLOB) WHERE name = 'sql'"}, []string{"table:tags"}},
		{"null to empty", []string{"UPDATE users SET avatar = '' WHERE id = 2"}, []string{"table:users"}},
		{"datetime text", []string{"UPDATE users SET created_at = '2024-01-02T03:04:05' WHERE id = 1"}, []string{"table:users"}},
		{"generated column", []string{"UPDATE posts SET title = 'hello!' WHERE id = 1"}, []string{"table:posts"}},
		{"autoincrement", []string{"INSERT INTO users (email) VALUES ('c@x')", "DELETE FROM users WHERE email = 'c@x'"}, []string{"table:users"}},
		{"hidden rowid", []string{"DELETE FROM log WHERE rowid = 20", "INSERT INTO log (rowid, msg) VALUES (21, 'twenty')"}, []string{"table:log"}},
		{"new index", []string{"CREATE INDEX posts_title ON posts (title)"}, []string{"table:posts"}},
		{"dropped index", []string{"DROP INDEX users_created"}, []string{"table:users"}},
		{"new column", []string{"ALTER TABLE tags ADD COLUMN note TEXT DEFAULT 'x'"}, []string{"table:tags"}},
		{"new table", []string{"CREATE TABLE extra (id INTEGER PRIMARY KEY)", "INSERT INTO extra VALUES (1)"}, []string{"table:extra"}},
		{"dropped table", []string{"DROP TABLE log"}, []string{"table:log"}},
		{"table to view", []string{"DROP TABLE log", "CREATE VIEW log AS SELECT 'x' AS msg"}, []string{"table:log", "view:log"}},
		{"view", []string{"DROP VIEW user_posts", "CREATE VIEW user_posts AS SELECT email FROM users"}, []string{"view:user_posts"}},
		{"dropped trigger", []string{"DROP TRIGGER users_audit"}, []string{"trigger:users_audit"}},
		{"new trigger", []string{"CREATE TRIGGER posts_audit AFTER UPDATE ON posts BEGIN INSERT INTO audit VALUES ('post'); END"}, []string{"trigger:posts_audit"}},
		{"user_version", []string{"PRAGMA user_version = 4"}, []string{"pragma:user_version"}},
		{"application_id", []string{"PRAGMA application_id = 7"}, []string{"pragma:application_id"}},
		{"full-text index", []string{"INSERT INTO docs VALUES ('lazy dog')"}, []string{"table:docs_*"}},
		{"dropped virtual table", []string{"DROP TABLE docs"}, []string{"table:docs", "table:docs_*"}},
		{"overflow pages", []string{"UPDATE users SET avatar = randomblob(20000) WHERE id = 1"}, []string{"table:users"}},
		{"statistics", []string{"ANALYZE"}, nil},
		{"excluded table", []string{"INSERT INTO sessions VALUES ('s2', 'y')"}, nil},
	}
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, app := sqliteFixture(t, mode, Options{})
			pristine := sqlitePristineFP(t, s)
			if d := Diff(sqliteFingerprint(t, s), pristine); len(d) > 0 {
				t.Fatalf("after the seed the working file differs from the pristine one in %v", d)
			}
			for _, c := range cases {
				before := sqliteFingerprint(t, s)
				mustExec(t, app, c.stmts...)
				after := sqliteFingerprint(t, s)
				got := Diff(before, after)
				if !sqliteItemsMatch(got, c.want) {
					t.Errorf("%s: changed %v, want %v", c.name, got, c.want)
				}
				for _, table := range s.scanned {
					if !sqliteMatchesSome("table:"+table, c.want) {
						t.Errorf("%s: read the rows of %s again, whose pages did not change", c.name, table)
					}
				}
				audit := count(t, app, "SELECT count(*) FROM audit")
				posts := count(t, app, "SELECT count(*) FROM posts")
				if err := s.Restore(ctx, Diff(after, pristine)); err != nil {
					t.Fatalf("%s: restore: %v", c.name, err)
				}
				restored := sqliteFingerprint(t, s)
				if d := Diff(restored, pristine); len(d) > 0 {
					t.Fatalf("%s: the restore left %v different", c.name, d)
				}
				if d := Diff(restored, sqliteScratch(t, s)); len(d) > 0 {
					t.Fatalf("%s: the fingerprint after the restore differs from one from scratch in %v", c.name, d)
				}
				if n := count(t, app, "SELECT count(*) FROM audit"); n != audit {
					t.Errorf("%s: the restore fired triggers: audit has %d rows, had %d", c.name, n, audit)
				}
				if c.name != "dropped table" && c.name != "table to view" {
					if n := count(t, app, "SELECT count(*) FROM posts"); n != posts {
						t.Errorf("%s: the restore cascaded a delete: posts has %d rows, had %d", c.name, n, posts)
					}
				}
				var ok string
				if err := app.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
					t.Fatalf("%s: integrity_check: %s %v", c.name, ok, err)
				}
			}
			var jm string
			app.QueryRow("PRAGMA journal_mode").Scan(&jm)
			if jm != mode {
				t.Errorf("journal mode is %s, was %s", jm, mode)
			}
		})
	}
}

// sqliteItemsMatch compares changed items with the wanted ones, where
// "table:x_*" stands for one or more items with that prefix.
func sqliteItemsMatch(got, want []string) bool {
	rest := slices.Clone(got)
	for _, w := range want {
		if p, ok := strings.CutSuffix(w, "*"); ok {
			n := len(rest)
			rest = slices.DeleteFunc(rest, func(g string) bool { return strings.HasPrefix(g, p) })
			if len(rest) == n {
				return false
			}
			continue
		}
		i := slices.Index(rest, w)
		if i < 0 {
			return false
		}
		rest = slices.Delete(rest, i, i+1)
	}
	return len(rest) == 0
}

func sqliteMatchesSome(item string, want []string) bool {
	for _, w := range want {
		if p, ok := strings.CutSuffix(w, "*"); ok && strings.HasPrefix(item, p) || item == w {
			return true
		}
	}
	return false
}

// TestSQLiteReuse: a fingerprint with nothing committed since is the last
// one again; a commit of another connection (also one left in the -wal
// file), a change of the schema, DriftGate's own restore and a file put
// in place of the working one are all seen.
func TestSQLiteReuse(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, app := sqliteFixture(t, mode, Options{})
			f0 := sqliteFingerprint(t, s)
			f1 := sqliteFingerprint(t, s)
			if !s.Reused() || len(Diff(f0, f1)) > 0 {
				t.Fatalf("a quiet file was read again (reused %v, diff %v)", s.Reused(), Diff(f0, f1))
			}
			part, err := s.Fingerprint(ctx, []string{"table:users", "view:user_posts"})
			if err != nil || !s.Reused() || len(part) != 2 || part["table:users"] != f0["table:users"] {
				t.Fatalf("a reused subset: %v %v %v", part, s.Reused(), err)
			}

			// A second application connection commits; the first one
			// stays open.
			other := sqliteApp(t, s.o.Working)
			mustExec(t, other, "PRAGMA wal_autocheckpoint = 0", "UPDATE posts SET title = 'x' WHERE id = 3")
			f2 := sqliteFingerprint(t, s)
			if s.Reused() || !slices.Equal(Diff(f1, f2), []string{"table:posts"}) {
				t.Fatalf("a commit of another connection: reused %v, diff %v", s.Reused(), Diff(f1, f2))
			}
			if !slices.Equal(s.scanned, []string{"posts"}) {
				t.Errorf("read the rows of %v, want only posts", s.scanned)
			}
			// A write to an excluded table: read again, nothing changed,
			// no table's rows read.
			mustExec(t, app, "INSERT INTO sessions VALUES ('s9', 'z')")
			f3 := sqliteFingerprint(t, s)
			if s.Reused() || len(Diff(f2, f3)) > 0 || len(s.scanned) > 0 {
				t.Fatalf("a write to an excluded table: reused %v, diff %v, read %v", s.Reused(), Diff(f2, f3), s.scanned)
			}
			sqliteFingerprint(t, s)
			if !s.Reused() {
				t.Fatal("not reused after a quiet moment")
			}

			// DriftGate's own restore commits through the connection
			// whose data_version does not move for it.
			if err := s.Restore(ctx, []string{"table:posts"}); err != nil {
				t.Fatal(err)
			}
			f4 := sqliteFingerprint(t, s)
			if s.Reused() || f4["table:posts"] != f0["table:posts"] {
				t.Fatalf("after a restore: reused %v, posts %s, want %s", s.Reused(), f4["table:posts"], f0["table:posts"])
			}

			// The schema changes in a way that leaves the pages as they
			// are.
			mustExec(t, app, "CREATE INDEX posts_user ON posts (user_id)")
			if f := sqliteFingerprint(t, s); s.Reused() || f["table:posts"] == f4["table:posts"] {
				t.Fatal("a new index was not seen")
			}
			// A new column with a default leaves the pages too, but not
			// the rows: they are read again.
			mustExec(t, app, "ALTER TABLE tags ADD COLUMN note TEXT DEFAULT 'x'")
			if sqliteFingerprint(t, s); !slices.Equal(s.scanned, []string{"tags"}) {
				t.Fatalf("after a new column read %v, want tags", s.scanned)
			}

			// Another file put in place of the working one: renamed over
			// it, or on Windows, which renames over or deletes no file
			// that is open, a copy written over it in rollback mode (in
			// WAL mode DriftGate's -wal and -shm files would stay with
			// the new file).
			other.Close()
			app.Close()
			repl := s.o.Working + ".new"
			r := sqliteApp(t, repl)
			mustExec(t, r, "CREATE TABLE only (id INTEGER PRIMARY KEY)")
			r.Close()
			if runtime.GOOS == "windows" {
				if mode == "wal" {
					return
				}
				raw, err := os.ReadFile(repl)
				if err == nil {
					err = os.WriteFile(s.o.Working, raw, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(repl, s.o.Working); err != nil {
				t.Fatal(err)
			}
			os.Remove(s.o.Working + "-wal")
			os.Remove(s.o.Working + "-shm")
			f5 := sqliteFingerprint(t, s)
			if s.Reused() || f5["table:only"] == "" || f5["table:users"] != "" {
				t.Fatalf("the replaced file was not read: %v", f5)
			}
		})
	}
}

// TestSQLiteRestoreAtomic: a restore that stops before its commit leaves
// the working file as it was, triggers included, and the next restore
// works.
func TestSQLiteRestoreAtomic(t *testing.T) {
	ctx := context.Background()
	s, app := sqliteFixture(t, "wal", Options{})
	pristine := sqlitePristineFP(t, s)
	mustExec(t, app, "UPDATE users SET email = 'changed' WHERE id = 1", "DROP TABLE log", "DROP INDEX users_created")
	changed := sqliteFingerprint(t, s)
	items := Diff(changed, pristine)

	defer func() { chaos.At = nil }()
	chaos.At = func(p string) {
		if p == "state.sqlite.restore" {
			panic("killed")
		}
	}
	func() {
		defer func() {
			if r := recover(); r != "killed" {
				t.Fatalf("the restore did not reach its chaos point: %v", r)
			}
		}()
		s.Restore(ctx, items)
	}()
	chaos.At = nil
	if d := Diff(sqliteFingerprint(t, s), changed); len(d) > 0 {
		t.Fatalf("a restore stopped before its commit changed %v", d)
	}
	if n := count(t, app, "SELECT count(*) FROM sqlite_schema WHERE type = 'trigger'"); n != 3 {
		t.Fatalf("%d triggers after the stopped restore, want 3", n)
	}
	if err := s.Restore(ctx, items); err != nil {
		t.Fatal(err)
	}
	if d := Diff(sqliteFingerprint(t, s), pristine); len(d) > 0 {
		t.Fatalf("the restore after left %v different", d)
	}

	// An error inside the transaction rolls back too: a pristine table
	// that cannot be created, its name being an index's now.
	mustExec(t, app, "UPDATE users SET email = 'again' WHERE id = 1", "DROP TABLE log", "CREATE INDEX log ON posts (title)")
	before := sqliteFingerprint(t, s)
	err := s.Restore(ctx, []string{"table:users", "table:log"})
	if err == nil || !strings.Contains(err.Error(), "table log") {
		t.Fatalf("restoring a table over an index's name: %v", err)
	}
	if d := Diff(sqliteFingerprint(t, s), before); len(d) > 0 {
		t.Fatalf("a failed restore changed %v", d)
	}
	if n := count(t, app, "SELECT count(*) FROM sqlite_schema WHERE type = 'trigger'"); n != 3 {
		t.Fatalf("%d triggers after the failed restore, want 3", n)
	}
}

// sqlitePython is a Python 3 with its own SQLite, for the tests of locking
// between processes: python3, or python where that is Python 3 (Windows).
// "" when there is none; DRIFTGATE_TEST_PYTHON=1 (CI) makes that a
// failure, so the tests never pass there without the other process.
func sqlitePython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if out, err := exec.Command(path, "-I", "-c", "import sqlite3, sys; print(sys.version_info[0])").Output(); err == nil && strings.TrimSpace(string(out)) == "3" {
			return path
		}
	}
	if os.Getenv("DRIFTGATE_TEST_PYTHON") != "" {
		t.Fatal("DRIFTGATE_TEST_PYTHON is set, but there is no Python 3 with sqlite3")
	}
	return ""
}

// TestSQLiteRestoreSeenByOpenConnections: a connection the application
// keeps open (in this process, and in a Python process when there is
// one) reads the restored rows at once, in WAL and in rollback mode.
func TestSQLiteRestoreSeenByOpenConnections(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, app := sqliteFixture(t, mode, Options{})
			emails := func() string {
				var e string
				if err := app.QueryRow("SELECT group_concat(email, ',') FROM (SELECT email FROM users ORDER BY id)").Scan(&e); err != nil {
					t.Fatal(err)
				}
				return e
			}
			mustExec(t, app, "UPDATE users SET email = 'z@x' WHERE id = 1", "INSERT INTO users (email) VALUES ('c@x')")
			if e := emails(); e != "z@x,b@x,c@x" {
				t.Fatalf("before: %s", e)
			}

			var py *exec.Cmd
			var pyIn *os.File
			var pyOut *bufio.Reader
			if python := sqlitePython(t); python != "" {
				script := filepath.Join(t.TempDir(), "holder.py")
				os.WriteFile(script, []byte(`import sqlite3, sys
c = sqlite3.connect(sys.argv[1], isolation_level=None, timeout=10)
def q(): return c.execute("SELECT group_concat(email, ',') FROM (SELECT email FROM users ORDER BY id)").fetchone()[0]
print(q(), flush=True)
sys.stdin.readline()
print(q(), flush=True)
print(c.execute("PRAGMA integrity_check").fetchone()[0], c.execute("PRAGMA journal_mode").fetchone()[0], flush=True)
c.execute("INSERT INTO users (email) VALUES ('py@x')")
print("written", flush=True)
`), 0o644)
				py = exec.Command(python, "-I", script, s.o.Working)
				in, w, _ := os.Pipe()
				py.Stdin, pyIn = in, w
				out, err := py.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				py.Stderr = &stderr
				if err := py.Start(); err != nil {
					t.Fatal(err)
				}
				in.Close()
				defer py.Wait()
				defer pyIn.Close()
				pyOut = bufio.NewReader(out)
				if line, _ := pyOut.ReadString('\n'); strings.TrimSpace(line) != "z@x,b@x,c@x" {
					t.Fatalf("python before: %q %s", line, stderr.String())
				}
			} else {
				t.Log("no python3: the in-process connection only")
			}

			if err := s.Restore(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if e := emails(); e != "a@x,b@x" {
				t.Fatalf("the open connection reads %s after the restore", e)
			}
			if py != nil {
				pyIn.WriteString("\n")
				if line, _ := pyOut.ReadString('\n'); strings.TrimSpace(line) != "a@x,b@x" {
					t.Fatalf("python reads %q after the restore", line)
				}
				if line, _ := pyOut.ReadString('\n'); strings.TrimSpace(line) != "ok "+mode {
					t.Fatalf("python: %q", line)
				}
				if line, _ := pyOut.ReadString('\n'); strings.TrimSpace(line) != "written" {
					t.Fatalf("python could not write after the restore: %q", line)
				}
				pyIn.Close()
				if err := py.Wait(); err != nil {
					t.Fatal(err)
				}
				py = nil
				// The users id the Python insert took follows the pristine
				// sequence, not the working one's.
				if n := count(t, app, "SELECT id FROM users WHERE email = 'py@x'"); n != 3 {
					t.Errorf("the next id after the restore is %d, want 3", n)
				}
			}
		})
	}
}

// TestSQLiteScanLetsWritersIn: in WAL mode an application writer that does
// not wait for locks (busy_timeout 0) commits while a fingerprint reads.
func TestSQLiteScanLetsWritersIn(t *testing.T) {
	ctx := context.Background()
	s, _ := sqliteFixture(t, "wal", Options{})
	writer, err := sqliteOpen(s.o.Working, []string{"mode=rw"})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	lock, _, err := s.barrier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conns, err := s.snapshot(ctx, 2)
	lock.ExecContext(ctx, "ROLLBACK")
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("INSERT INTO tags VALUES ('during', 1, 1)"); err != nil {
		t.Fatalf("a writer without a busy timeout failed during the read: %v", err)
	}
	// The read still sees the moment it started at, on every connection.
	for _, c := range conns {
		var n int
		c.QueryRowContext(ctx, "SELECT count(*) FROM tags").Scan(&n)
		if n != 2 {
			t.Errorf("a read connection sees %d tags, want 2", n)
		}
	}
	s.end(conns)
}

// TestSQLiteSeed: a seed replaces only a pristine file it made, an empty
// one or an adopted one; it copies excluded tables without rows, keeps
// hidden rowids, makes a rollback-mode file, and one stopped before the
// rename leaves the old pristine file.
func TestSQLiteSeed(t *testing.T) {
	ctx := context.Background()
	s, app := sqliteFixture(t, "wal", Options{})
	p := sqliteApp(t, s.o.Pristine)
	if n := count(t, p, "SELECT count(*) FROM "+pristineTable); n != 1 {
		t.Fatal("no mark in the pristine file")
	}
	if n := count(t, p, "SELECT count(*) FROM sessions"); n != 0 {
		t.Errorf("the excluded table has %d rows in the pristine file", n)
	}
	if n := count(t, p, "SELECT count(*) FROM log WHERE rowid IN (3, 7, 20)"); n != 3 {
		t.Errorf("hidden rowids were not kept")
	}
	var jm string
	p.QueryRow("PRAGMA journal_mode").Scan(&jm)
	if jm != "delete" {
		t.Errorf("the pristine file is in %s mode", jm)
	}
	p.Close()
	if d := Diff(sqliteFingerprint(t, s), sqlitePristineFP(t, s)); len(d) > 0 {
		t.Fatalf("pristine and working differ in %v", d)
	}
	if _, err := os.Stat(s.o.Working + "-wal"); err != nil {
		t.Errorf("the working file's -wal file: %v", err)
	}

	// A marked file is replaced; a -wal file left next to it is removed
	// first, never read as the new file's.
	mustExec(t, app, "INSERT INTO tags VALUES ('seeded', 9, 9)")
	os.WriteFile(s.o.Pristine+"-wal", []byte("stale"), 0o644)
	calls := 0
	s.o.Replacing = func() error { calls++; return nil }
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("Replacing ran %d times", calls)
	}
	if _, err := os.Stat(s.o.Pristine + "-wal"); err == nil {
		t.Error("the stale -wal file is still there")
	}
	if d := Diff(sqliteFingerprint(t, s), sqlitePristineFP(t, s)); len(d) > 0 {
		t.Fatalf("after the second seed pristine and working differ in %v", d)
	}

	// A seed stopped before the rename keeps the old pristine file, and
	// the next one cleans up after it.
	old := sqlitePristineFP(t, s)
	mustExec(t, app, "INSERT INTO tags VALUES ('lost', 1, 1)")
	defer func() { chaos.At = nil }()
	chaos.At = func(p string) {
		if p == "state.sqlite.seed" {
			panic("killed")
		}
	}
	func() {
		defer func() { recover() }()
		s.Seed(ctx)
	}()
	chaos.At = nil
	s.pristine = nil
	if d := Diff(sqlitePristineFP(t, s), old); len(d) > 0 {
		t.Fatalf("a seed stopped before the rename changed the pristine file: %v", d)
	}
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.o.Pristine + sqliteNew); err == nil {
		t.Error("the new file of the stopped seed is still there")
	}

	// Someone's file is never replaced: an SQLite database without the
	// mark, or a file of another kind.
	someone := sqliteApp(t, s.o.Pristine+".someone")
	mustExec(t, someone, "CREATE TABLE orders (id INTEGER PRIMARY KEY)", "INSERT INTO orders VALUES (1)")
	someone.Close()
	os.Rename(s.o.Pristine+".someone", s.o.Pristine)
	err := s.Seed(ctx)
	if err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over an unmarked file: %v", err)
	}
	someone = sqliteApp(t, s.o.Pristine)
	if n := count(t, someone, "SELECT count(*) FROM orders"); n != 1 {
		t.Fatal("the unmarked file was changed")
	}
	someone.Close()
	os.WriteFile(s.o.Pristine, []byte("notes\n"), 0o644)
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over a text file: %v", err)
	}
	if b, _ := os.ReadFile(s.o.Pristine); string(b) != "notes\n" {
		t.Fatal("the text file was changed")
	}
	// An adopted one is replaced; so is an empty file.
	adopt := *s
	adopt.o.AdoptPristine = true
	if err := adopt.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.o.Pristine, nil, 0o644)
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.sqlite")
	e := sqliteApp(t, empty)
	mustExec(t, e, "PRAGMA user_version = 1") // a database file with no objects
	e.Close()
	os.Rename(empty, s.o.Pristine)
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestNewSQLite: the working file must be an SQLite database, and the
// pristine file can be neither it nor a file SQLite keeps next to it (nor
// the other way round).
func TestNewSQLite(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "app.sqlite")
	mustExec(t, sqliteApp(t, work), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	os.WriteFile(filepath.Join(dir, "text.sqlite"), []byte("not a database at all"), 0o644)
	os.WriteFile(filepath.Join(dir, "empty.sqlite"), nil, 0o644)
	os.Symlink(work, filepath.Join(dir, "link.sqlite"))
	pristine := filepath.Join(dir, "pristine.sqlite")
	newWAL := pristine + ".driftgate-new-wal"
	mustExec(t, sqliteApp(t, newWAL), "CREATE TABLE precious (id INTEGER PRIMARY KEY)")
	mustExec(t, sqliteApp(t, filepath.Join(dir, "P.sqlite.driftgate-new-shm")), "CREATE TABLE precious (id INTEGER PRIMARY KEY)")
	for _, c := range []struct {
		working, pristine, want string
	}{
		{filepath.Join(dir, "missing.sqlite"), pristine, "does not exist"},
		{filepath.Join(dir, "text.sqlite"), pristine, "is not an SQLite database"},
		{filepath.Join(dir, "empty.sqlite"), pristine, "is empty"},
		{dir, pristine, "folder"},
		{work, work, "is the working database file"},
		{work, filepath.Join(dir, "link.sqlite"), "is the working database file"},
		{work, work + "-wal", "-wal"},
		{work, work + "-journal", "-journal"},
		{pristine + "-journal", pristine, "next to it"},
		// A seed removes the -wal and -shm files of its new copy: a working
		// file named so would be deleted.
		{newWAL, pristine, "next to it"},
		{filepath.Join(dir, "P.sqlite.driftgate-new-shm"), filepath.Join(dir, "p.sqlite"), "next to it"},
		{work + ".old", work, "named like the pristine file"},
		{"app.sqlite", pristine, "absolute"},
	} {
		s, err := NewSQLite(Options{Name: "db", Working: c.working, Pristine: c.pristine})
		if err == nil {
			s.Close()
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s / %s: %v, want %q", c.working, c.pristine, err, c.want)
		}
	}
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(pristine); err == nil {
		t.Error("opening made the pristine file")
	}
}

// TestSQLiteURI: a name with ?, #, %, & or a space stays one file name.
func TestSQLiteURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b#c")
	os.MkdirAll(dir, 0o755)
	name := "x?mode=ro%41.sqlite"
	if runtime.GOOS == "windows" { // no ? in a file name there
		name = "x&mode=ro%41.sqlite"
	}
	work := filepath.Join(dir, name)
	mustExec(t, sqliteApp(t, work), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != name {
		t.Fatalf("files: %v", ents)
	}
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "p#1.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "p#1.sqlite")); err != nil {
		t.Fatal(err)
	}
	if f := sqliteFingerprint(t, s); f["table:a"] == "" {
		t.Fatalf("fingerprint: %v", f)
	}
}

// TestSQLiteRows: rows are keyed by their primary key, or by rowid.
func TestSQLiteRows(t *testing.T) {
	ctx := context.Background()
	s, app := sqliteFixture(t, "wal", Options{RowTracking: []string{"users", "tags", "log"}})
	before, err := s.Rows(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for table, key := range map[string]string{"users": "id=1", "tags": "name='go'", "log": "rowid=7"} {
		if _, ok := before[table][key]; !ok {
			t.Errorf("%s: no row %s in %v", table, key, before[table])
		}
	}
	mustExec(t, app, "UPDATE users SET score = 2 WHERE id = 1", "DELETE FROM log WHERE rowid = 7", "INSERT INTO tags VALUES (x'00', 1, 1)")
	after, err := s.Rows(ctx, []string{"table:users", "table:log", "table:tags", "table:posts"})
	if err != nil {
		t.Fatal(err)
	}
	got := RowDiff(before, after)
	want := []RowChange{
		{Table: "log", Key: "rowid=7", Change: "deleted"},
		{Table: "tags", Key: "name=x'00'", Change: "inserted"},
		{Table: "users", Key: "id=1", Change: "updated", Columns: []string{"score"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("row changes %v, want %v", got, want)
	}
}

// TestSQLiteQuery: a query reads the pristine file as SQLite holds it, and
// anything that is not a plain read, or reads the file's pages, is
// refused.
func TestSQLiteQuery(t *testing.T) {
	ctx := context.Background()
	s, app := sqliteFixture(t, "wal", Options{})
	mustExec(t, app, "UPDATE users SET email = 'working@x' WHERE id = 1")
	for q, want := range map[string]string{
		"SELECT email FROM users ORDER BY id":                                     "a@x b@x",
		"SELECT email FROM users ORDER BY id DESC;":                               "b@x a@x",
		"SELECT created_at, id FROM users ORDER BY id":                            "2024-01-02 03:04:05 2024-02-03 04:05:06",
		"WITH t AS (SELECT name FROM tags) SELECT name FROM t ORDER BY name DESC": "sql go",
		"SELECT count(*) FROM sessions ORDER BY 1":                                "0",
	} {
		got, err := s.Query(ctx, q)
		if err != nil || strings.Join(got, " ") != want {
			t.Errorf("%s: %q %v, want %s", q, got, err, want)
		}
	}
	for _, q := range []string{
		"PRAGMA table_info(users)",
		"SELECT * FROM pragma_table_info('users') ORDER BY 1",
		"SELECT pgno FROM sqlite_dbpage ORDER BY 1",
		"SELECT name FROM dbstat ORDER BY 1",
		`SELECT pgno FROM "sqlite_dbpage" ORDER BY 1`,
		"SELECT pgno FROM [sqlite_dbpage] ORDER BY 1",
		`SELECT name FROM "dbstat" ORDER BY 1`,
		"ATTACH DATABASE '/tmp/x.db' AS x",
		"SELECT load_extension('x') ORDER BY 1",
		"SELECT readfile('/etc/passwd') ORDER BY 1",
		"SELECT fts3_tokenizer('simple') ORDER BY 1",
		"VACUUM INTO '/tmp/x.db'",
		"SELECT 1 ORDER BY 1; DELETE FROM users",
		"INSERT INTO users (email) VALUES ('q')",
		"SELECT * FROM sqlite_dbdata ORDER BY 1",
	} {
		if got, err := s.Query(ctx, q); err == nil {
			t.Errorf("%s: accepted (%v)", q, got)
		}
	}
	check := sqliteApp(t, s.o.Pristine)
	if n := count(t, check, "SELECT count(*) FROM users WHERE email = 'q'"); n != 0 {
		t.Fatal("a query wrote to the pristine file")
	}
	check.Close() // Windows deletes no file that is open
	if err := os.Remove(s.o.Pristine); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "SELECT 1 ORDER BY 1"); err == nil || !strings.Contains(err.Error(), "run driftgate seed") {
		t.Errorf("no pristine file: %v", err)
	}
}

// TestSQLiteEncoding: the hash of a table's rows tells apart values that
// read alike: rows whose texts join to the same text, a text and a blob
// of the same bytes, an integer, a real and a text of the same number,
// NULL and an empty text.
func TestSQLiteEncoding(t *testing.T) {
	for _, pair := range [][2][]any{
		{{"ab", "c"}, {"a", "bc"}},
		{{"ab"}, {[]byte("ab")}},
		{{int64(1)}, {1.0}},
		{{int64(1)}, {"1"}},
		{{nil}, {""}},
		{{""}, {[]byte{}}},
		{{"a", nil}, {"a\x00", nil}},
		{{int64(0)}, {nil}},
		{{0.0}, {math.Copysign(0, -1)}},
	} {
		var a, b []byte
		for _, v := range pair[0] {
			a = appendValue(a, v)
		}
		for _, v := range pair[1] {
			b = appendValue(b, v)
		}
		if bytes.Equal(a, b) {
			t.Errorf("%#v and %#v encode alike", pair[0], pair[1])
		}
	}
	dir := t.TempDir()
	sums := map[string]bool{}
	for i, rows := range []string{
		"('ab', 'c'), ('d', 'e')",
		"('a', 'bc'), ('d', 'e')",
		"('ab', 'c'), ('de', NULL)",
		"('ab', 'c'), ('d', 'e'), (NULL, NULL)",
		"('ab', 'c'), ('d', x'65')",
	} {
		path := filepath.Join(dir, fmt.Sprintf("%d.sqlite", i))
		mustExec(t, sqliteApp(t, path), "CREATE TABLE t (a, b)", "INSERT INTO t VALUES "+rows)
		s, err := NewSQLite(Options{Name: "db", Working: path, Pristine: path + ".p"})
		if err != nil {
			t.Fatal(err)
		}
		f := sqliteFingerprint(t, s)
		s.Close()
		if sums[f["table:t"]] {
			t.Errorf("rows %s hash like others", rows)
		}
		sums[f["table:t"]] = true
	}
}

// TestSQLiteMissingModule: a virtual table of a module DriftGate's SQLite
// does not have is fingerprinted by its definition, and a restore that has
// to make it again says what is missing.
func TestSQLiteMissingModule(t *testing.T) {
	ctx := context.Background()
	s, app := sqliteFixture(t, "delete", Options{})
	app.Close()
	app = sqliteApp(t, s.o.Working)
	mustExec(t, app, "PRAGMA writable_schema = ON",
		"INSERT INTO sqlite_schema (type, name, tbl_name, rootpage, sql) VALUES ('table', 'geo', 'geo', 0, 'CREATE VIRTUAL TABLE geo USING nosuchmodule (a)')",
		"PRAGMA writable_schema = OFF")
	app.Close()
	s.Close()
	s, err := NewSQLite(s.o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := sqliteFingerprint(t, s)
	if f["table:geo"] == "" {
		t.Fatalf("no item for the virtual table: %v", f)
	}
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	app = sqliteApp(t, s.o.Working)
	sv := count(t, app, "PRAGMA schema_version")
	mustExec(t, app, "PRAGMA writable_schema = ON", "DELETE FROM sqlite_schema WHERE name = 'geo'",
		fmt.Sprintf("PRAGMA schema_version = %d", sv+1), "PRAGMA writable_schema = OFF")
	app.Close()
	err = s.Restore(ctx, []string{"table:geo"})
	if err == nil || !strings.Contains(err.Error(), "does not have this module") {
		t.Fatalf("restore of a virtual table of a missing module: %v", err)
	}
}

// TestSQLiteRestoreTransfer: with its triggers dropped, a table of the
// shape frameworks make (INTEGER PRIMARY KEY AUTOINCREMENT, indexes) and a
// WITHOUT ROWID table are copied by SQLite's transfer optimization, record
// by record, not value by value.
func TestSQLiteRestoreTransfer(t *testing.T) {
	ctx := context.Background()
	s, _ := sqliteFixture(t, "wal", Options{})
	uri, err := sqliteURI(s.o.Pristine, "mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	c := s.main
	if _, err := c.ExecContext(ctx, "ATTACH DATABASE ? AS "+sqlitePristine, uri); err != nil {
		t.Fatal(err)
	}
	defer c.ExecContext(ctx, "DETACH DATABASE "+sqlitePristine)
	mustExecConn(t, c, "BEGIN IMMEDIATE", "DROP TRIGGER users_audit", "DROP TRIGGER users_gone")
	defer c.ExecContext(ctx, "ROLLBACK")
	pris, err := s.objects(ctx, c, sqlitePristine)
	if err != nil {
		t.Fatal(err)
	}
	for table, transfer := range map[string]bool{"users": true, "tags": true, "posts": false, "log": false} {
		cols, _, err := readColumns(ctx, c, sqlitePristine, table)
		if err != nil {
			t.Fatal(err)
		}
		q := copySQL(pris.tables[table], cols)
		rows, err := c.QueryContext(ctx, "EXPLAIN "+q)
		if err != nil {
			t.Fatal(err)
		}
		records := false
		for rows.Next() {
			var addr int
			var op string
			var rest [6]any
			rows.Scan(&addr, &op, &rest[0], &rest[1], &rest[2], &rest[3], &rest[4], &rest[5])
			records = records || op == "RowData" || op == "RowCell"
		}
		rows.Close()
		if records != transfer {
			t.Errorf("%s: %s copies records: %v, want %v", table, q, records, transfer)
		}
	}
}

func mustExecConn(t *testing.T, c *sql.Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// sqliteSchemaFixture makes a working file with the given statements in a
// journal mode, seeds it and returns the component and the application's
// connection.
func sqliteSchemaFixture(t *testing.T, mode string, exclude []string, stmts ...string) (*SQLite, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	o := Options{Name: "db", Working: filepath.Join(dir, "app.sqlite"), Pristine: filepath.Join(dir, "store", "pristine.sqlite"), Exclude: exclude}
	app := sqliteApp(t, o.Working)
	mustExec(t, app, "PRAGMA journal_mode = "+mode)
	mustExec(t, app, stmts...)
	s, err := NewSQLite(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, app
}

// sqliteRestoreChecked restores the items that differ, checks them the
// way the capture does (only those items again), and then checks that
// the whole file is the pristine one: a restore that passes the first
// check and fails the second would be a false pass.
func sqliteRestoreChecked(t *testing.T, s *SQLite) {
	t.Helper()
	ctx := context.Background()
	pristine := sqlitePristineFP(t, s)
	items := Diff(sqliteFingerprint(t, s), pristine)
	if err := s.Restore(ctx, items); err != nil {
		t.Fatal(err)
	}
	got, err := s.Fingerprint(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(got, pristine.Subset(items)); len(d) > 0 {
		t.Fatalf("the restore of %v left %v different", items, d)
	}
	if d := Diff(sqliteFingerprint(t, s), pristine); len(d) > 0 {
		t.Fatalf("the restore of %v passed its check, but %v differ from the pristine file", items, d)
	}
}

// TestSQLiteNameCase: SQLite names are case-insensitive, and an index or a
// trigger keeps its table's name as its statement wrote it. A trigger on
// USERS is still dropped before users is written and made again after (it
// neither fires nor gets lost), an index on USERS is part of users, and a
// seed does not fire a trigger on SESSIONS while it empties sessions.
func TestSQLiteNameCase(t *testing.T) {
	t.Run("trigger fires", func(t *testing.T) {
		s, app := sqliteSchemaFixture(t, "wal", nil,
			"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)",
			"CREATE TABLE counts (n INTEGER)",
			"INSERT INTO counts VALUES (0)",
			"CREATE TRIGGER users_count AFTER INSERT ON USERS BEGIN UPDATE counts SET n = n + 1; END",
			"INSERT INTO users VALUES (1, 'a')")
		mustExec(t, app, "INSERT INTO users VALUES (2, 'b')", "UPDATE counts SET n = n - 1")
		sqliteRestoreChecked(t, s)
	})
	t.Run("trigger lost", func(t *testing.T) {
		s, app := sqliteSchemaFixture(t, "delete", nil,
			"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)",
			"CREATE TABLE audit2 (m TEXT)",
			"CREATE TRIGGER users_a AFTER INSERT ON Users BEGIN INSERT INTO audit2 VALUES (NEW.email); END")
		mustExec(t, app, "ALTER TABLE users ADD COLUMN extra TEXT")
		sqliteRestoreChecked(t, s)
		if n := count(t, app, "SELECT count(*) FROM sqlite_schema WHERE type = 'trigger'"); n != 1 {
			t.Fatalf("%d triggers after the restore, want 1", n)
		}
	})
	t.Run("renamed in case", func(t *testing.T) {
		s, app := sqliteSchemaFixture(t, "wal", nil,
			"CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, v)", "INSERT INTO users (v) VALUES (1)",
			"CREATE TABLE other (x)", "CREATE TRIGGER t1 AFTER INSERT ON users BEGIN INSERT INTO other VALUES (1); END")
		mustExec(t, app, "ALTER TABLE users RENAME TO tmp_u", "ALTER TABLE tmp_u RENAME TO Users")
		sqliteRestoreChecked(t, s)
		if n := count(t, app, "SELECT count(*) FROM other"); n != 0 {
			t.Fatal("the restore fired the trigger")
		}
	})
	t.Run("index", func(t *testing.T) {
		s, app := sqliteSchemaFixture(t, "wal", nil, "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)")
		before := sqliteFingerprint(t, s)
		mustExec(t, app, "CREATE UNIQUE INDEX ue ON USERS (email)")
		if d := Diff(before, sqliteFingerprint(t, s)); !slices.Equal(d, []string{"table:users"}) {
			t.Fatalf("an index on USERS changed %v", d)
		}
		sqliteRestoreChecked(t, s)
	})
	t.Run("seed", func(t *testing.T) {
		s, _ := sqliteSchemaFixture(t, "wal", []string{"sessions"},
			"CREATE TABLE users (id INTEGER PRIMARY KEY, n INTEGER)",
			"CREATE TABLE sessions (id TEXT PRIMARY KEY)",
			"INSERT INTO users VALUES (1, 0)",
			"INSERT INTO sessions VALUES ('s')",
			"CREATE TRIGGER sess_gone AFTER DELETE ON SESSIONS BEGIN UPDATE users SET n = n + 1; END")
		if d := Diff(sqliteFingerprint(t, s), sqlitePristineFP(t, s)); len(d) > 0 {
			t.Fatalf("the seed fired a trigger of an excluded table: %v differ", d)
		}
	})
}

// TestSQLiteExcludedTableTrigger: a trigger of an excluded table is an
// item: its body may write any table.
func TestSQLiteExcludedTableTrigger(t *testing.T) {
	s, app := sqliteSchemaFixture(t, "wal", []string{"sessions"},
		"CREATE TABLE users (id INTEGER PRIMARY KEY, n INTEGER)",
		"CREATE TABLE sessions (id TEXT PRIMARY KEY)",
		"CREATE TRIGGER st AFTER INSERT ON sessions BEGIN SELECT 1; END")
	before := sqliteFingerprint(t, s)
	mustExec(t, app, "INSERT INTO sessions VALUES ('a')", "DROP TRIGGER st", "CREATE TRIGGER st AFTER INSERT ON sessions BEGIN UPDATE users SET n = n + 1; END")
	if d := Diff(before, sqliteFingerprint(t, s)); !slices.Equal(d, []string{"trigger:st"}) {
		t.Fatalf("changed %v, want trigger:st", d)
	}
	sqliteRestoreChecked(t, s)
	if n := count(t, app, "SELECT count(*) FROM sessions"); n != 1 {
		t.Fatalf("the restore of the trigger changed the excluded table: %d rows", n)
	}
}

// TestSQLiteExcludedAbsent: exclude: may name tables the file does not
// have (Laravel 10 has no job_batches; a glob matching nothing); seed,
// fingerprint and restore take the tables that exist and ignore the rest,
// and a table the application makes later under an excluded name is
// excluded from then on.
func TestSQLiteExcludedAbsent(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			s, app := sqliteSchemaFixture(t, mode, []string{"sessions", "job_batches", "cache_*", "failed_jobs"},
				"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)",
				"CREATE TABLE sessions (id TEXT PRIMARY KEY, payload TEXT)",
				"INSERT INTO users VALUES (1, 'a')",
				"INSERT INTO sessions VALUES ('s1', 'x')")
			before := sqliteFingerprint(t, s)
			for item := range before {
				if strings.Contains(item, "sessions") || strings.Contains(item, "job_batches") || strings.Contains(item, "cache") {
					t.Fatalf("fingerprint has the excluded item %s", item)
				}
			}
			mustExec(t, app, "INSERT INTO users VALUES (2, 'b')", "INSERT INTO sessions VALUES ('s2', 'y')",
				"CREATE TABLE job_batches (id TEXT PRIMARY KEY)", "INSERT INTO job_batches VALUES ('b1')")
			if d := Diff(before, sqliteFingerprint(t, s)); !slices.Equal(d, []string{"table:users"}) {
				t.Fatalf("changed %v, want table:users", d)
			}
			sqliteRestoreChecked(t, s)
			if n := count(t, app, "SELECT count(*) FROM sessions") + count(t, app, "SELECT count(*) FROM job_batches"); n != 3 {
				t.Fatalf("the restore changed the excluded tables: %d rows, want 3", n)
			}
		})
	}
}

// TestSQLiteExcludedLeavesNoTrace: nothing of an excluded table's rows is
// in the pristine file's bytes: not in free pages, not in the samples of
// index keys ANALYZE keeps (sqlite_stat4), and not in the pages of an
// excluded full-text index, whose delete would only add delete markers.
func TestSQLiteExcludedLeavesNoTrace(t *testing.T) {
	for _, analyze := range []string{"ANALYZE", "PRAGMA optimize = 0x10002"} {
		t.Run(analyze, func(t *testing.T) {
			stmts := []string{
				"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)",
				"INSERT INTO users VALUES (1, 'kept-user')",
				"CREATE TABLE sessions (id TEXT PRIMARY KEY, payload TEXT)",
				"CREATE INDEX sessions_payload ON sessions (payload)",
				"CREATE VIRTUAL TABLE Secret_Fts USING fts5 (body)",
				"INSERT INTO secret_fts VALUES ('ftsleakword hunter2leak')",
			}
			for i := range 60 {
				stmts = append(stmts, fmt.Sprintf("INSERT INTO sessions VALUES ('SESSIONTOKEN%03d', 'PAYLOADLEAK%03d')", i, i))
			}
			s, app := sqliteSchemaFixture(t, "wal", []string{"sessions", "secret_fts"}, append(stmts, analyze)...)
			if n := count(t, app, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'sessions'"); n == 0 {
				t.Fatalf("%s kept no statistics of sessions: the test tests nothing", analyze)
			}
			raw, err := os.ReadFile(s.o.Pristine)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range []string{"SESSIONTOKEN", "PAYLOADLEAK", "ftsleakword", "hunter2leak"} {
				if bytes.Contains(raw, []byte(w)) {
					t.Errorf("%s is in the pristine file", w)
				}
			}
			if !bytes.Contains(raw, []byte("kept-user")) {
				t.Error("the pristine file lost a table that is not excluded")
			}
			if got, err := s.Query(context.Background(), "SELECT count(*) FROM sqlite_stat1 WHERE tbl IN ('sessions', 'secret_fts')"); err != nil || len(got) != 1 || got[0] != "0" {
				t.Errorf("statistics of excluded tables in the pristine file: %v %v", got, err)
			}
		})
	}
}

// TestSQLiteBusy: a lock the application holds longer than DriftGate
// waits is reported as what it is, with what to do.
func TestSQLiteBusy(t *testing.T) {
	session, wait := slices.Clone(sqliteSession), sqliteBusyWait
	for i, p := range sqliteSession {
		if strings.HasPrefix(p, "_pragma=busy_timeout(") {
			sqliteSession[i] = "_pragma=busy_timeout(200)"
		}
	}
	sqliteBusyWait = 200 * time.Millisecond
	t.Cleanup(func() { sqliteSession, sqliteBusyWait = session, wait })
	ctx := context.Background()
	s, app := sqliteFixture(t, "delete", Options{})
	mustExec(t, app, "UPDATE users SET email = 'z@x' WHERE id = 1")
	c, err := app.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mustExecConn(t, c, "BEGIN IMMEDIATE")
	defer c.ExecContext(ctx, "ROLLBACK")
	err = s.Restore(ctx, []string{"table:users"})
	if err == nil || !strings.Contains(err.Error(), "another connection held a lock on "+s.o.Working+" for 200ms") || !strings.Contains(err.Error(), "stop the worker") {
		t.Fatalf("restore under a held lock: %v", err)
	}
	if _, err := s.Fingerprint(ctx, nil); err == nil || !strings.Contains(err.Error(), "another connection held a lock") {
		t.Fatalf("fingerprint under a held lock: %v", err)
	}
}

// TestSQLiteKeepsLocks: DriftGate never opens and closes a database file
// itself: that would drop the locks of every connection of the process to
// it. While one component holds the write lock, a second one opening the
// same file (and seeding from it) leaves another process locked out.
func TestSQLiteKeepsLocks(t *testing.T) {
	python := sqlitePython(t)
	if python == "" {
		t.Skip("no python3")
	}
	for _, mode := range []string{"delete", "wal"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, _ := sqliteFixture(t, mode, Options{})
			writer := func() string {
				out, _ := exec.Command(python, "-I", "-c", `import sqlite3, sys
c = sqlite3.connect(sys.argv[1], isolation_level=None, timeout=0)
try:
    c.execute("BEGIN IMMEDIATE"); print("got the write lock"); c.execute("ROLLBACK")
except sqlite3.OperationalError as e:
    print("locked out")`, s.o.Working).CombinedOutput()
				return strings.TrimSpace(string(out))
			}
			if _, err := s.main.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			defer s.main.ExecContext(ctx, "ROLLBACK")
			if got := writer(); got != "locked out" {
				t.Fatalf("before: %s", got)
			}
			o := s.o
			o.Pristine = filepath.Join(t.TempDir(), "other.sqlite")
			other, err := NewSQLite(o)
			if err != nil {
				t.Fatal(err)
			}
			other.checkPristine(ctx)
			other.Close()
			if got := writer(); got != "locked out" {
				t.Fatalf("after a second component opened the file: %s", got)
			}
		})
	}
}

// TestSQLiteOverwrittenInPlace: a copy written over the working file (the
// same file, the same change counter, other rows) is not taken for the
// file the last fingerprint read.
func TestSQLiteOverwrittenInPlace(t *testing.T) {
	s, app := sqliteSchemaFixture(t, "delete", nil, "CREATE TABLE a (id INTEGER PRIMARY KEY, v)", "INSERT INTO a VALUES (1, 1)")
	app.Close()
	bak := s.o.Working + ".bak"
	copyFile(t, s.o.Working, bak)
	a := sqliteApp(t, s.o.Working)
	mustExec(t, a, "UPDATE a SET v = 2")
	a.Close()
	before := sqliteFingerprint(t, s)
	b := sqliteApp(t, bak)
	mustExec(t, b, "UPDATE a SET v = 3") // the copy's change counter now equals the file's
	b.Close()
	data, err := os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.o.Working, data, 0o644); err != nil {
		t.Fatal(err)
	}
	after := sqliteFingerprint(t, s)
	if s.Reused() || !slices.Equal(Diff(before, after), []string{"table:a"}) {
		t.Fatalf("the overwritten file: reused %v, changed %v", s.Reused(), Diff(before, after))
	}
}

// TestSQLiteExcludedRowsGone: the rows of an excluded table do not stay in
// the free pages of the pristine file.
func TestSQLiteExcludedRowsGone(t *testing.T) {
	for _, mode := range []string{"delete", "wal"} {
		t.Run(mode, func(t *testing.T) {
			s, app := sqliteFixture(t, mode, Options{})
			mustExec(t, app, "INSERT INTO sessions VALUES ('s2', 'SECRET-SESSION-TOKEN-0123456789')",
				"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 2000) INSERT INTO sessions SELECT 'n' || i, 'SECRET-SESSION-TOKEN-' || i FROM r")
			if err := s.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(s.o.Pristine)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("SECRET-SESSION-TOKEN")) {
				t.Fatal("rows of the excluded table are in the pristine file")
			}
		})
	}
}

// TestSQLiteRowsKeys: keys tell an integer from a text, a text from a
// blob's hex, and a text that holds a separator; rows with the same key
// are an error.
func TestSQLiteRowsKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := sqliteSchemaFixture(t, "wal", nil,
		"CREATE TABLE k (id PRIMARY KEY, v)",
		"INSERT INTO k VALUES (1, 'int'), ('1', 'text'), (x'31', 'blob'), ('x''31''', 'hex text')",
		"CREATE TABLE c (a, b, PRIMARY KEY (a, b))",
		"INSERT INTO c VALUES ('1,b=2', 3), ('1', '2,b=3')",
		"CREATE TABLE n (id TEXT PRIMARY KEY, v)",
		"INSERT INTO n VALUES (NULL, 1), (NULL, 2)")
	s.o.RowTracking = []string{"k", "c", "n"}
	r, err := s.Rows(ctx, []string{"table:k", "table:c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r["k"]) != 4 || len(r["c"]) != 2 {
		t.Fatalf("keys: %v %v", r["k"], r["c"])
	}
	if _, err := s.Rows(ctx, []string{"table:n"}); err == nil || !strings.Contains(err.Error(), "same primary key") {
		t.Fatalf("two NULL keys: %v", err)
	}
}

// TestSQLiteQueryShapes: a query returns the first column in the query's
// own order, whatever its shape, also for a DATETIME first column (read
// as its text).
func TestSQLiteQueryShapes(t *testing.T) {
	ctx := context.Background()
	s, _ := sqliteFixture(t, "wal", Options{})
	w := sqliteApp(t, s.o.Pristine)
	mustExec(t, w, "CREATE TABLE n (id INTEGER PRIMARY KEY, k TEXT, v INT, d DATETIME)",
		`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 200)
			INSERT INTO n (k, v, d) SELECT printf('k%03d', (i * 7919) % 1000), (i * 31) % 50, printf('2024-01-%02d 10:00:00', 1 + i % 28) FROM r`)
	w.Close()
	direct, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	for _, q := range []string{
		"SELECT k FROM n ORDER BY v DESC, k",
		"SELECT DISTINCT k FROM n ORDER BY k DESC",
		"SELECT k FROM n GROUP BY k ORDER BY count(*) DESC, k",
		"SELECT k FROM n WHERE v < 10 UNION ALL SELECT k FROM n WHERE v > 40 ORDER BY 1 DESC",
		"SELECT k FROM n WHERE v < 10 UNION SELECT k FROM n WHERE v > 40 ORDER BY 1 DESC",
		"SELECT a.k FROM n a JOIN n b ON a.id = b.id + 1 ORDER BY a.v DESC, a.k",
		"SELECT k FROM n ORDER BY v DESC, k LIMIT 10 OFFSET 5",
		"SELECT k, row_number() OVER (ORDER BY v) FROM n ORDER BY v DESC, k",
		"WITH t AS (SELECT k, v FROM n) SELECT k FROM t ORDER BY v DESC, k",
		"SELECT d, k FROM n ORDER BY k DESC",
		"SELECT DISTINCT d FROM n ORDER BY d DESC LIMIT 5",
		"WITH t AS (SELECT d, v FROM n) SELECT d FROM t ORDER BY v, d DESC",
	} {
		got, err := s.Query(ctx, q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			continue
		}
		// The query itself, its times written back in SQLite's format.
		want := sqliteQueryText(t, direct, q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s:\n got %v\nwant %v", q, got, want)
		}
	}
}

// sqliteQueryText runs a query and reads its first column as text, a
// time read back in SQLite's own format.
func sqliteQueryText(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		switch v := vals[0].(type) {
		case time.Time:
			out = append(out, v.Format("2006-01-02 15:04:05"))
		case []byte:
			out = append(out, string(v))
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

// TestSQLiteSeedCleansUp: a seed that fails after the copy leaves no new
// file behind, and the old pristine file as it was.
func TestSQLiteSeedCleansUp(t *testing.T) {
	s, _ := sqliteFixture(t, "wal", Options{})
	old := sqlitePristineFP(t, s)
	s.o.Replacing = func() error { return fmt.Errorf("no") }
	if err := s.Seed(context.Background()); err == nil {
		t.Fatal("the seed did not fail")
	}
	for _, suf := range []string{"", "-journal", "-wal", "-shm"} {
		if _, err := os.Stat(s.o.Pristine + sqliteNew + suf); err == nil {
			t.Errorf("%s is left", sqliteNew+suf)
		}
	}
	s.pristine = nil
	if d := Diff(sqlitePristineFP(t, s), old); len(d) > 0 {
		t.Fatalf("the failed seed changed the pristine file: %v", d)
	}
}

// TestSQLitePageDigest: a fingerprint that keeps row hashes by page
// digest equals one read from scratch, and sees every change, whatever
// moves the pages: tables that grow and shrink, overflow chains, pages one
// table frees and another reuses, root pages auto_vacuum moves when a
// table is dropped, VACUUM, incremental_vacuum, with pages of 512 to 65536
// bytes, in WAL mode without a checkpoint and in rollback mode.
func TestSQLitePageDigest(t *testing.T) {
	steps := []struct {
		name  string
		stmts []string
		want  []string
	}{
		{"grow", []string{"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 300) INSERT INTO a (v) SELECT CASE WHEN i % 7 = 0 THEN randomblob(3000) ELSE 'row ' || i END FROM r"}, []string{"table:a"}},
		{"excluded", []string{"INSERT INTO sessions VALUES (2, randomblob(5000))"}, nil},
		{"overflow chain longer", []string{"UPDATE a SET v = randomblob(9000) WHERE id = 7"}, []string{"table:a"}},
		{"overflow chain same length", []string{"UPDATE a SET v = randomblob(9000) WHERE id = 7"}, []string{"table:a"}},
		{"one byte of an overflow page", []string{"UPDATE a SET v = CAST(substr(v, 1, 8999) || iif(substr(v, 9000) = x'00', x'01', x'00') AS BLOB) WHERE id = 7"}, []string{"table:a"}},
		{"shrink", []string{"DELETE FROM a WHERE id > 40"}, []string{"table:a"}},
		{"freed pages reused by another table", []string{"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 200) INSERT INTO b SELECT 'k' || i, randomblob(200) FROM r"}, []string{"table:b"}},
		{"pages move from one table to the other", []string{"DELETE FROM b WHERE k > 'k5'", "WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 100) INSERT INTO a (v) SELECT randomblob(500) FROM r"}, []string{"table:a", "table:b"}},
		{"dropped table", []string{"DROP TABLE c"}, []string{"table:c"}},
		{"new table", []string{"CREATE TABLE c (id INTEGER PRIMARY KEY, v)", "INSERT INTO c VALUES (1, 'one')"}, []string{"table:c"}},
		{"table dropped before others", []string{"DROP TABLE d"}, []string{"table:d"}},
		{"incremental_vacuum", []string{"PRAGMA incremental_vacuum"}, nil},
		{"vacuum", []string{"DELETE FROM a WHERE id % 2 = 0", "VACUUM"}, []string{"table:a"}},
		{"vacuum alone", []string{"VACUUM"}, nil},
		{"same rows written again", []string{"DELETE FROM c", "INSERT INTO c VALUES (1, 'one')"}, nil},
		{"one value", []string{"UPDATE b SET v = x'00' WHERE k = 'k1'"}, []string{"table:b"}},
	}
	defer func(v float64) { sqliteScanAll = v }(sqliteScanAll)
	for _, mode := range []string{"wal", "delete"} {
		for _, size := range []int{512, 4096, 65536} {
			for i, av := range []string{"none", "full", "incremental"} {
				// Every page of the file read, or only those needed.
				all := (i+size)%2 == 0
				t.Run(fmt.Sprintf("%s/%d/%s/all=%v", mode, size, av, all), func(t *testing.T) {
					sqliteScanAll = 2
					if all {
						sqliteScanAll = 0
					}
					dir := t.TempDir()
					o := Options{Name: "db", Working: filepath.Join(dir, "app.sqlite"), Pristine: filepath.Join(dir, "pristine.sqlite"), Exclude: []string{"sessions"}}
					app := sqliteApp(t, o.Working)
					mustExec(t, app, fmt.Sprintf("PRAGMA page_size = %d", size), "PRAGMA auto_vacuum = "+av,
						"PRAGMA journal_mode = "+mode, "PRAGMA wal_autocheckpoint = 0",
						"CREATE TABLE d (id INTEGER PRIMARY KEY, v)", "INSERT INTO d VALUES (1, randomblob(2000))",
						"CREATE TABLE a (id INTEGER PRIMARY KEY, v)",
						"CREATE TABLE b (k TEXT PRIMARY KEY, v) WITHOUT ROWID",
						"CREATE TABLE sessions (id INTEGER PRIMARY KEY, p)",
						"CREATE TABLE c (id INTEGER PRIMARY KEY, v)", "INSERT INTO c VALUES (1, 'one')",
						"INSERT INTO sessions VALUES (1, 'x')")
					s, err := NewSQLite(o)
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					var got int
					app.QueryRow("PRAGMA page_size").Scan(&got)
					if got != size {
						t.Fatalf("page size %d, want %d", got, size)
					}
					before := sqliteFingerprint(t, s)
					for _, st := range steps {
						mustExec(t, app, st.stmts...)
						after := sqliteFingerprint(t, s)
						if d := Diff(before, after); !slices.Equal(d, st.want) {
							t.Errorf("%s: changed %v, want %v", st.name, d, st.want)
						}
						if d := Diff(after, sqliteScratch(t, s)); len(d) > 0 {
							t.Fatalf("%s: a fingerprint by page digests differs from one read from scratch in %v", st.name, d)
						}
						before = after
					}
				})
			}
		}
	}
}

// TestSQLitePageDigestConcurrentWriter: fingerprints taken while another
// connection keeps committing are each of one moment: every table's hash
// is one of the hashes it has between two commits, the table nobody writes
// keeps its hash, and once the writer stops the fingerprint is that of
// the file as it was left.
func TestSQLitePageDigestConcurrentWriter(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			s, app := sqliteSchemaFixture(t, mode, []string{"sessions"},
				"CREATE TABLE a (id INTEGER PRIMARY KEY, v)",
				"CREATE TABLE b (id INTEGER PRIMARY KEY, v)",
				"CREATE TABLE sessions (id INTEGER PRIMARY KEY, p)",
				"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 400) INSERT INTO a SELECT i, 'x' || i FROM r",
				"WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 400) INSERT INTO b SELECT i, randomblob(300) FROM r",
				"INSERT INTO sessions VALUES (1, 'x')")
			states := []string{"UPDATE a SET v = 'x' || id", "UPDATE a SET v = CASE WHEN id % 50 = 0 THEN CAST(hex(zeroblob(2500)) || id AS BLOB) ELSE 'y' || id END", "UPDATE a SET v = 'z' || id"}
			hashes := map[string]bool{} // of a in each state
			for _, q := range states {
				mustExec(t, app, q)
				hashes[sqliteFingerprint(t, s)["table:a"]] = true
			}
			first := sqliteFingerprint(t, s)
			writer := sqliteApp(t, s.o.Working)
			done := make(chan struct{})
			errs := make(chan error, 1)
			go func() {
				defer close(errs)
				for i := 0; ; i++ {
					select {
					case <-done:
						return
					case <-time.After(time.Millisecond): // a writer that never pauses would starve the barrier
					}
					for _, q := range []string{fmt.Sprintf("INSERT OR REPLACE INTO sessions VALUES (1, %d)", i), states[i%3]} {
						if _, err := writer.Exec(q); err != nil {
							errs <- err
							return
						}
					}
				}
			}()
			seen := map[string]bool{}
			for range 40 {
				f := sqliteFingerprint(t, s)
				if !hashes[f["table:a"]] {
					t.Fatalf("table a has a hash of no moment: %s", f["table:a"])
				}
				seen[f["table:a"]] = true
				time.Sleep(2 * time.Millisecond) // the writer's turn, in rollback mode
				if f["table:b"] != first["table:b"] {
					t.Fatal("table b, which nobody writes, changed")
				}
			}
			close(done)
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			if len(seen) < 2 {
				t.Fatalf("the fingerprints saw %d states of a", len(seen))
			}
			writer.Close()
			if d := Diff(sqliteFingerprint(t, s), sqliteScratch(t, s)); len(d) > 0 {
				t.Fatalf("after the writer stopped the fingerprint differs from one read from scratch in %v", d)
			}
		})
	}
}

// TestSQLitePageDigestRootMoved: tables whose root pages are swapped in
// sqlite_schema have each other's rows, though no page changed.
func TestSQLitePageDigestRootMoved(t *testing.T) {
	s, app := sqliteSchemaFixture(t, "delete", nil,
		"CREATE TABLE x (id INTEGER PRIMARY KEY, v)", "INSERT INTO x VALUES (1, 'x')",
		"CREATE TABLE y (id INTEGER PRIMARY KEY, v)", "INSERT INTO y VALUES (1, 'y')")
	before := sqliteFingerprint(t, s)
	var sv int
	app.QueryRow("PRAGMA schema_version").Scan(&sv)
	mustExec(t, app, "PRAGMA writable_schema = ON",
		"UPDATE sqlite_schema SET rootpage = (SELECT sum(rootpage) FROM sqlite_schema WHERE name IN ('x', 'y')) - rootpage WHERE name IN ('x', 'y')",
		"PRAGMA writable_schema = OFF", fmt.Sprintf("PRAGMA schema_version = %d", sv+1))
	if n := count(t, app, "SELECT count(*) FROM x WHERE v = 'y'"); n != 1 {
		t.Fatalf("the root pages were not swapped (%d)", n)
	}
	after := sqliteFingerprint(t, s)
	if d := Diff(before, after); !slices.Equal(d, []string{"table:x", "table:y"}) {
		t.Fatalf("changed %v, want x and y", d)
	}
}
