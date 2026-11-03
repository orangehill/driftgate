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
	"slices"
	"strings"
	"testing"

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
				if d := Diff(sqliteFingerprint(t, s), pristine); len(d) > 0 {
					t.Fatalf("%s: the restore left %v different", c.name, d)
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

			// Another file put in place of the working one.
			other.Close()
			app.Close()
			repl := s.o.Working + ".new"
			r := sqliteApp(t, repl)
			mustExec(t, r, "CREATE TABLE only (id INTEGER PRIMARY KEY)")
			r.Close()
			if err := os.Rename(repl, s.o.Working); err != nil {
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
			if python, err := exec.LookPath("python3"); err == nil {
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

// TestSQLiteURI: a name with ?, #, % or a space stays one file name.
func TestSQLiteURI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b#c")
	os.MkdirAll(dir, 0o755)
	work := filepath.Join(dir, "x?mode=ro%41.sqlite")
	mustExec(t, sqliteApp(t, work), "CREATE TABLE a (id INTEGER PRIMARY KEY)")
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "x?mode=ro%41.sqlite" {
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
	for table, key := range map[string]string{"users": "id=1", "tags": "name=go", "log": "rowid=7"} {
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
	if n := count(t, sqliteApp(t, s.o.Pristine), "SELECT count(*) FROM users WHERE email = 'q'"); n != 0 {
		t.Fatal("a query wrote to the pristine file")
	}
	os.Remove(s.o.Pristine)
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
