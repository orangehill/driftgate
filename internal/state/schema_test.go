package state

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// schemaCase is a change of the working database: what SchemaFingerprint
// must find different from the pristine copy (item), or nothing ("").
type schemaCase struct {
	name string
	do   []string
	item string
}

// testSchemaFingerprint: right after a seed the working schema is the
// pristine copy's; each case is applied in turn, and a case that changes
// the schema is seeded again before the next.
func testSchemaFingerprint(t *testing.T, c Component, exec func(t *testing.T, stmts ...string), cases []schemaCase) {
	t.Helper()
	ctx := context.Background()
	s, ok := c.(Schemer)
	if !ok {
		t.Fatalf("%T is no Schemer", c)
	}
	diff := func() []string {
		t.Helper()
		p, err := s.SchemaFingerprint(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		w, err := s.SchemaFingerprint(ctx, false)
		if err != nil || w == nil {
			t.Fatalf("working: %v %v", w, err)
		}
		if len(p) == 0 {
			t.Fatal("the pristine schema has no items")
		}
		return Diff(w, p)
	}
	if err := c.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if d := diff(); len(d) != 0 {
		t.Fatalf("right after the seed: %v", d)
	}
	for _, k := range cases {
		exec(t, k.do...)
		d := diff()
		var want []string
		if k.item != "" {
			want = []string{k.item}
		}
		if !slices.Equal(d, want) {
			t.Errorf("%s: %v, want %v", k.name, d, want)
		}
		if len(d) > 0 {
			if err := c.Seed(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSchemaFingerprintSQLite(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "app.sqlite")
	db := sqliteApp(t, work)
	mustExec(t, db, "PRAGMA journal_mode = wal",
		"CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT)", "CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)",
		"CREATE TABLE cache (k TEXT PRIMARY KEY, v TEXT)", "CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, token TEXT)",
		"CREATE TABLE sessions (id TEXT)", "INSERT INTO users (email) VALUES ('a@x')")
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "pristine.sqlite"),
		Exclude: []string{"sessions"}, RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testSchemaFingerprint(t, s, func(t *testing.T, stmts ...string) { mustExec(t, db, stmts...) }, []schemaCase{
		{"rows", []string{"INSERT INTO users (email) VALUES ('b@x')", "UPDATE users SET email = 'z'", "DELETE FROM notes",
			"INSERT INTO cache VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')", "INSERT INTO sessions VALUES ('s')"}, ""},
		{"a migration rolled back", []string{"CREATE TABLE tmp (id INT)", "DROP TABLE tmp"}, ""},
		{"a column", []string{"ALTER TABLE users ADD COLUMN slug TEXT"}, "table:users"},
		{"a table", []string{"CREATE TABLE posts (id INT)"}, "table:posts"},
		{"a dropped table", []string{"DROP TABLE notes"}, "table:notes"},
		{"an index", []string{"CREATE INDEX users_email ON users (email)"}, "table:users"},
		{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "view:emails"},
		{"a trigger", []string{"CREATE TRIGGER t AFTER INSERT ON users BEGIN SELECT 1; END"}, "trigger:t"},
		{"a pragma", []string{"PRAGMA user_version = 7"}, "pragma:user_version"},
		{"a restore_only column", []string{"ALTER TABLE tokens ADD COLUMN extra INT"}, "table:tokens"},
		{"a restore_empty index", []string{"CREATE INDEX cache_v ON cache (v)"}, "table:cache"},
		{"a restore_only table dropped", []string{"DROP TABLE tokens"}, "table:tokens"},
	})
}

func TestSchemaFingerprintMySQL(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "schema", nil, []string{
				"CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, email VARCHAR(50))", "CREATE TABLE notes (id INT PRIMARY KEY, body TEXT)",
				"CREATE TABLE cache (k VARCHAR(50) PRIMARY KEY, v TEXT)", "CREATE TABLE tokens (id INT AUTO_INCREMENT PRIMARY KEY, token VARCHAR(50))",
				"CREATE TABLE sessions (id VARCHAR(50))", "INSERT INTO users (email) VALUES ('a@x')"})
			cases := []schemaCase{
				{"rows and AUTO_INCREMENT", []string{"INSERT INTO users (email) VALUES ('b@x')", "UPDATE users SET email = 'z'", "DELETE FROM notes",
					"INSERT INTO cache VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')", "ALTER TABLE users AUTO_INCREMENT = 100"}, ""},
				{"a migration rolled back", []string{"CREATE TABLE tmp (id INT)", "DROP TABLE tmp"}, ""},
				{"a column", []string{"ALTER TABLE users ADD COLUMN slug VARCHAR(50)"}, "table:users"},
				{"a table", []string{"CREATE TABLE posts (id INT)"}, "table:posts"},
				{"a dropped table", []string{"DROP TABLE notes"}, "table:notes"},
				{"an index", []string{"CREATE INDEX users_email ON users (email)"}, "table:users"},
				{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "view:emails"},
				{"a trigger", []string{"CREATE TRIGGER t BEFORE INSERT ON users FOR EACH ROW SET NEW.email = lower(NEW.email)"}, "trigger:t"},
				{"a restore_only column", []string{"ALTER TABLE tokens ADD COLUMN extra INT"}, "table:tokens"},
				{"a restore_empty index", []string{"CREATE INDEX cache_k ON cache (k, v(10))"}, "table:cache"},
				{"a restore_only table dropped", []string{"DROP TABLE tokens"}, "table:tokens"},
			}
			if f.mariadb {
				mustExec(t, f.rootWork, "CREATE SEQUENCE numbers")
				cases = append(cases,
					schemaCase{"a sequence that moved", []string{"SELECT NEXT VALUE FOR numbers", "SELECT SETVAL(numbers, 50)"}, ""},
					schemaCase{"ALTER SEQUENCE", []string{"ALTER SEQUENCE numbers INCREMENT BY 5"}, "sequence:numbers"})
			}
			m := f.open(t, f.dsn, Options{Exclude: []string{"sessions"}, RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}})
			testSchemaFingerprint(t, m, func(t *testing.T, stmts ...string) { mustExec(t, f.rootWork, stmts...) }, cases)
		})
	}
}

func TestSchemaFingerprintPostgres(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	work, pristine := "dgt_schema", "dgt_schema_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine,
		Exclude: []string{"sessions"}, RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	drop := func() {
		p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
		p.admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
	}
	drop()
	t.Cleanup(drop)
	mustExec(t, p.admin, "CREATE DATABASE "+work)
	exec := func(t *testing.T, stmts ...string) {
		t.Helper()
		db, err := p.open(work)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mustExec(t, db, stmts...)
	}
	exec(t, "CREATE TABLE users (id serial PRIMARY KEY, email text)", "CREATE TABLE notes (id int PRIMARY KEY, body text)",
		"CREATE TABLE cache (k text PRIMARY KEY, v text)", "CREATE TABLE tokens (id serial PRIMARY KEY, token text)",
		"CREATE TABLE sessions (id text)", "CREATE SEQUENCE numbers", "INSERT INTO users (email) VALUES ('a@x')")
	testSchemaFingerprint(t, p, exec, []schemaCase{
		{"rows", []string{"INSERT INTO users (email) VALUES ('b@x')", "UPDATE users SET email = 'z'", "DELETE FROM notes",
			"INSERT INTO cache VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')", "INSERT INTO sessions VALUES ('s')"}, ""},
		{"a sequence that moved", []string{"SELECT nextval('numbers')", "SELECT setval('numbers', 40, false)"}, ""},
		{"a migration rolled back", []string{"CREATE TABLE tmp (id int)", "DROP TABLE tmp"}, ""},
		{"a column", []string{"ALTER TABLE users ADD COLUMN slug text"}, "table:public.users"},
		{"a table", []string{"CREATE TABLE posts (id int)"}, "table:public.posts"},
		{"a dropped table", []string{"DROP TABLE notes"}, "table:public.notes"},
		{"an index", []string{"CREATE INDEX users_email ON users (email)"}, "table:public.users"},
		{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "view:public.emails"},
		{"ALTER SEQUENCE", []string{"ALTER SEQUENCE numbers INCREMENT BY 5"}, "sequence:public.numbers"},
		{"a restore_only column", []string{"ALTER TABLE tokens ADD COLUMN extra int"}, "table:public.tokens"},
		{"a restore_empty index", []string{"CREATE INDEX cache_v ON cache (v)"}, "table:public.cache"},
		{"an excluded table's column", []string{"ALTER TABLE sessions ADD COLUMN extra int"}, "table:public.sessions"},
		{"an extension", []string{"CREATE EXTENSION pgcrypto"}, "extension:pgcrypto"},
	})
}

func TestSchemaFingerprintSQLServer(t *testing.T) {
	s, db := newSQLServerTest(t, Options{Exclude: []string{"sessions"}, RestoreOnly: []string{"tokens"}, RestoreEmpty: []string{"cache"}})
	mustExec(t, db, "CREATE TABLE users (id int IDENTITY PRIMARY KEY, email nvarchar(50))", "CREATE TABLE notes (id int PRIMARY KEY, body nvarchar(50))",
		"CREATE TABLE cache (k nvarchar(50) PRIMARY KEY, v nvarchar(50))", "CREATE TABLE tokens (id int IDENTITY PRIMARY KEY, token nvarchar(50))",
		"CREATE TABLE sessions (id nvarchar(50))", "CREATE SEQUENCE numbers START WITH 1", "INSERT INTO users (email) VALUES ('a@x')")
	testSchemaFingerprint(t, s, func(t *testing.T, stmts ...string) { mustExec(t, db, stmts...) }, []schemaCase{
		{"rows and identity", []string{"INSERT INTO users (email) VALUES ('b@x')", "UPDATE users SET email = 'z'", "DELETE FROM notes",
			"INSERT INTO cache VALUES ('k', 'v')", "INSERT INTO tokens (token) VALUES ('t')", "INSERT INTO sessions VALUES ('s')"}, ""},
		{"a sequence that moved", []string{"SELECT NEXT VALUE FOR numbers"}, ""},
		{"a migration rolled back", []string{"CREATE TABLE tmp (id int)", "DROP TABLE tmp"}, ""},
		{"a column", []string{"ALTER TABLE users ADD slug nvarchar(50)"}, "table:dbo.users"},
		{"a table", []string{"CREATE TABLE posts (id int)"}, "table:dbo.posts"},
		{"a dropped table", []string{"DROP TABLE notes"}, "table:dbo.notes"},
		{"an index", []string{"CREATE INDEX users_email ON users (email)"}, "table:dbo.users"},
		{"a view", []string{"CREATE VIEW emails AS SELECT email FROM users"}, "view:dbo.emails"},
		{"ALTER SEQUENCE", []string{"ALTER SEQUENCE numbers INCREMENT BY 5"}, "sequence:dbo.numbers"},
		{"a restore_only column", []string{"ALTER TABLE tokens ADD extra int"}, "table:dbo.tokens"},
		{"a restore_empty index", []string{"CREATE INDEX cache_v ON cache (v)"}, "table:dbo.cache"},
	})
}

// TestMySQLCancelDuringCopy: a restore stopped during the copy of a table
// with triggers (an interrupt) makes the dropped triggers and view again
// on a session of its own: the working schema stays the pristine copy's,
// and a migration of the table it was restoring is still found.
func TestMySQLCancelDuringCopy(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "cancelcopy", nil, []string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
				"CREATE TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)",
				"CREATE TRIGGER t2 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id + 1)", "CREATE VIEW v AS SELECT id FROM items"})
			m := f.open(t, f.dsn, Options{})
			ctx := context.Background()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.rootWork, "INSERT INTO items VALUES (1)")
			stop, cancel := context.WithCancel(ctx)
			m.hooks = &mysqlHooks{copyFails: func() error { cancel(); return stop.Err() }}
			if err := m.Restore(stop, []string{"table:items", "view:v"}); !errors.Is(err, context.Canceled) {
				t.Fatalf("restore: %v", err)
			}
			m.hooks = nil
			schema := func() []string {
				t.Helper()
				p, err := m.SchemaFingerprint(ctx, true)
				if err != nil {
					t.Fatal(err)
				}
				w, err := m.SchemaFingerprint(ctx, false)
				if err != nil {
					t.Fatal(err)
				}
				return Diff(w, p)
			}
			if d := schema(); len(d) != 0 {
				t.Fatalf("after the stopped restore: %v", d)
			}
			var order string
			f.root.QueryRow("SELECT GROUP_CONCAT(TRIGGER_NAME ORDER BY ACTION_ORDER) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?", f.work).Scan(&order)
			if order != "t1,t2" {
				t.Errorf("triggers in firing order: %q", order)
			}
			mustExec(t, f.rootWork, "ALTER TABLE items ADD COLUMN slug VARCHAR(20)")
			if d := schema(); !slices.Equal(d, []string{"table:items"}) {
				t.Errorf("a migration after the stopped restore: %v", d)
			}
		})
	}
}

// TestSQLServerSchemaOffline: a working database taken offline after a
// migration is brought online and its schema read: the migration is found.
func TestSQLServerSchemaOffline(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE users (id int IDENTITY PRIMARY KEY, email nvarchar(50))")
	ctx := context.Background()
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "ALTER TABLE users ADD slug nvarchar(50)")
	db.Close()
	master := sqlServerTestDB(t, s.o.DSN, "")
	mustExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" SET OFFLINE WITH ROLLBACK IMMEDIATE")
	p, err := s.SchemaFingerprint(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.SchemaFingerprint(ctx, false)
	if err != nil || w == nil {
		t.Fatalf("working: %v %v", w, err)
	}
	if d := Diff(w, p); !slices.Equal(d, []string{"table:dbo.users"}) {
		t.Errorf("after OFFLINE: %v", d)
	}
}
