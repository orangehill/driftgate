package state

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

// TestMySQLObjectCollation: triggers and views keep the collation of the
// connection that made them through a seed and a restore (their string
// literals compare with it), and the fingerprint sees it change.
func TestMySQLObjectCollation(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_coll", "dgt_coll_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work+" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db,
				"CREATE TABLE news (id INT PRIMARY KEY, title VARCHAR(20))",
				"CREATE TABLE log (note VARCHAR(20))",
				"SET NAMES utf8mb4 COLLATE utf8mb4_bin",
				"CREATE VIEW same AS SELECT 'a' = 'A' AS same",
				"CREATE TRIGGER news_log AFTER INSERT ON news FOR EACH ROW INSERT INTO log VALUES (IF('a' = 'A', 'ci', 'bin'))",
				"SET NAMES utf8mb4 COLLATE utf8mb4_general_ci",
			)
			collations := func(db string) []string {
				t.Helper()
				var v, tr string
				root.QueryRow("SELECT COLLATION_CONNECTION FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'same'", db).Scan(&v)
				root.QueryRow("SELECT COLLATION_CONNECTION FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND TRIGGER_NAME = 'news_log'", db).Scan(&tr)
				return []string{v, tr}
			}
			want := []string{"utf8mb4_bin", "utf8mb4_bin"}
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			if got := collations(pristine); !reflect.DeepEqual(got, want) {
				t.Fatalf("pristine collations %v, want %v", got, want)
			}
			fp0 := mustFingerprint(t, m)

			// The same objects made with the session's collation differ.
			mustExec(t, db, "DROP VIEW same", "DROP TRIGGER news_log",
				"CREATE VIEW same AS SELECT 'a' = 'A' AS same",
				"CREATE TRIGGER news_log AFTER INSERT ON news FOR EACH ROW INSERT INTO log VALUES (IF('a' = 'A', 'ci', 'bin'))")
			changed := Diff(fp0, mustFingerprint(t, m))
			if !reflect.DeepEqual(changed, []string{"trigger:news_log", "view:same"}) {
				t.Fatalf("changed %v", changed)
			}
			if err := m.Restore(ctx, changed); err != nil {
				t.Fatal(err)
			}
			if got := collations(work); !reflect.DeepEqual(got, want) {
				t.Fatalf("restored collations %v, want %v", got, want)
			}
			if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
				t.Fatalf("after the restore %v", d)
			}
			var same int
			var note string
			mustExec(t, db, "INSERT INTO news VALUES (1, 'x')")
			db.QueryRow("SELECT same FROM same").Scan(&same)
			db.QueryRow("SELECT note FROM log").Scan(&note)
			if same != 0 || note != "bin" {
				t.Fatalf("restored view says %d, trigger %q: not utf8mb4_bin", same, note)
			}
		})
	}
}

// TestPostgresSchemaItems: the fingerprint sees functions, enum and domain
// types, schemas, materialized views, row security and policies change,
// and a restore brings them back.
func TestPostgresSchemaItems(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work, pristine := "dgt_items", "dgt_items_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
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
	db, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	mustExec(t, db,
		"CREATE TABLE news (id int PRIMARY KEY, title text)",
		"CREATE TABLE plain (id int PRIMARY KEY)",
		"INSERT INTO news VALUES (1, 'a')",
		"CREATE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 1'",
		"CREATE TYPE mood AS ENUM ('sad', 'happy')",
		"CREATE DOMAIN positive AS int CHECK (VALUE > 0)",
		"CREATE SCHEMA tenant",
		"CREATE MATERIALIZED VIEW news_count AS SELECT count(*) AS n FROM news",
		"CREATE VIEW plain_ids AS SELECT id FROM plain",
		"CREATE SEQUENCE counter",
		"CREATE TYPE pair AS (a int)",
	)
	before, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // the seed ends the sessions on the working database
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if db, err = p.open(work); err != nil {
		t.Fatal(err)
	}
	fp0, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(before, fp0); len(d) > 0 {
		t.Fatalf("the seed changed %v", d)
	}
	for _, item := range []string{"function:public.add1(i integer)", "type:public.mood", "type:public.positive", "type:public.pair", "schema:tenant", "matview:public.news_count"} {
		if fp0[item] == "" {
			t.Fatalf("no item %s in %v", item, fp0)
		}
	}
	cases := []struct {
		write string
		want  []string
	}{
		{"CREATE OR REPLACE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 2'", []string{"function:public.add1(i integer)"}},
		{"ALTER TYPE mood ADD VALUE 'ok' BEFORE 'happy'", []string{"type:public.mood"}},
		{"ALTER DOMAIN positive ADD CONSTRAINT small CHECK (VALUE < 100)", []string{"type:public.positive"}},
		{"CREATE SCHEMA tenant2", []string{"schema:tenant2"}},
		{"INSERT INTO news VALUES (2, 'b'); REFRESH MATERIALIZED VIEW news_count", []string{"matview:public.news_count", "table:public.news"}},
		{"ALTER TABLE news ENABLE ROW LEVEL SECURITY", []string{"table:public.news"}},
		{"CREATE POLICY mine ON news USING (id > 0)", []string{"table:public.news"}},
		{`ALTER TABLE news ALTER COLUMN title TYPE text COLLATE "C"`, []string{"table:public.news"}},
		{"CREATE RULE keep AS ON DELETE TO plain DO INSTEAD NOTHING", []string{"table:public.plain"}},
		{"CREATE RULE keep AS ON DELETE TO plain_ids DO INSTEAD NOTHING", []string{"view:public.plain_ids"}},
		{"GRANT SELECT ON news TO PUBLIC", []string{"table:public.news"}},
		{"GRANT SELECT (title) ON news TO PUBLIC", []string{"table:public.news"}},
		{"GRANT SELECT ON plain_ids TO PUBLIC", []string{"view:public.plain_ids"}},
		{"COMMENT ON COLUMN news.title IS 'Title'", []string{"table:public.news"}},
		{"COMMENT ON TABLE plain IS 'Plain'", []string{"table:public.plain"}},
		{"ALTER TABLE plain SET UNLOGGED", []string{"table:public.plain"}},
		{"ALTER TABLE news REPLICA IDENTITY FULL", []string{"table:public.news"}},
		{"ALTER SEQUENCE counter INCREMENT BY 5", []string{"sequence:public.counter"}},
		{"ALTER SEQUENCE counter MAXVALUE 1000", []string{"sequence:public.counter"}},
		{"SELECT setval('counter', 10, false)", []string{"sequence:public.counter"}},
		{"ALTER TYPE pair ADD ATTRIBUTE b text", []string{"type:public.pair"}},
	}
	for _, c := range cases {
		mustExec(t, db, c.write)
		got, err := p.Fingerprint(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d := Diff(fp0, got); !reflect.DeepEqual(d, c.want) {
			t.Errorf("%s: changed %v, want %v", c.write, d, c.want)
		}
		db.Close()
		if err := p.Restore(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if db, err = p.open(work); err != nil {
			t.Fatal(err)
		}
		if got, _ := p.Fingerprint(ctx, nil); len(Diff(fp0, got)) > 0 {
			t.Fatalf("%s: after the restore %v", c.write, Diff(fp0, got))
		}
	}
	// The fingerprint of some items has only those.
	if got, _ := p.Fingerprint(ctx, []string{"table:public.plain", "type:public.mood"}); len(got) != 2 {
		t.Fatalf("fingerprint of two items: %v", got)
	}
}

// TestMySQLFingerprintFormat1: format 1 hashes a view and a trigger as
// earlier releases did, without the collation they were made with; every
// other item as format 2 does.
func TestMySQLFingerprintFormat1(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work := "dgt_fmt1"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "CREATE DATABASE "+work+" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
			db := mysqlAdmin(t, dsn, work)
			db.SetMaxOpenConns(1) // SET NAMES holds for the next statements
			create := func(collation string) {
				t.Helper()
				mustExec(t, db, "DROP VIEW IF EXISTS same", "DROP TRIGGER IF EXISTS news_log",
					"SET NAMES utf8mb4 COLLATE "+collation,
					"CREATE VIEW same AS SELECT 'a' = 'A' AS same",
					"CREATE TRIGGER news_log AFTER INSERT ON news FOR EACH ROW INSERT INTO log VALUES ('x')")
			}
			mustExec(t, db, "CREATE TABLE news (id INT PRIMARY KEY)", "CREATE TABLE log (note VARCHAR(20))")
			create("utf8mb4_bin")
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: work + "_pristine"})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			v1, err := m.FingerprintAs(ctx, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			v2 := mustFingerprint(t, m)
			if d := Diff(v1, v2); !reflect.DeepEqual(d, []string{"trigger:news_log", "view:same"}) {
				t.Fatalf("formats differ in %v", d)
			}
			c, err := root.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			def, err := showCreate(ctx, c, "VIEW", work, "same")
			c.Close()
			if err != nil {
				t.Fatal(err)
			}
			if want := hash([]byte(localView(def, work))); v1["view:same"] != want {
				t.Errorf("format 1 view hash %s, want %s", v1["view:same"], want)
			}

			// Made with another collation: format 2 sees it, format 1 did not.
			create("utf8mb4_general_ci")
			items := []string{"view:same", "trigger:news_log"}
			again, err := m.FingerprintAs(ctx, 1, items)
			if err != nil {
				t.Fatal(err)
			}
			if d := Diff(v1.Subset(items), again); len(d) > 0 || len(again) != 2 {
				t.Errorf("format 1 changed with the collation: %v", d)
			}
			if d := Diff(v2, mustFingerprint(t, m)); len(d) != 2 {
				t.Errorf("format 2 changed in %v", d)
			}
		})
	}
}

// TestPostgresFingerprintFormat1: format 1 has no functions, types,
// schemas or materialized views, and a table's, view's or sequence's hash
// without what format 2 adds to it (row security, policies, rules,
// privileges, comments, collations, UNLOGGED, replica identity, sequence
// parameters), as format 1 made it.
func TestPostgresFingerprintFormat1(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work := "dgt_fmt1"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: work + "_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	drop := func() { p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)") }
	drop()
	t.Cleanup(drop)
	mustExec(t, p.admin, "CREATE DATABASE "+work)
	db, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "CREATE TABLE news (id int PRIMARY KEY)", "INSERT INTO news VALUES (1)")
	// What format 2 adds is no part of format 1: the same hashes, before
	// and after, but for the new column (and view and sequence).
	mustExec(t, db, "ALTER TABLE news ADD COLUMN title text", "CREATE VIEW ids AS SELECT id FROM news", "CREATE SEQUENCE counter")
	plain, err := p.FingerprintAs(ctx, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "DROP VIEW ids", "DROP SEQUENCE counter", "ALTER TABLE news DROP COLUMN title")
	mustExec(t, db, "CREATE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 1'", "CREATE TYPE mood AS ENUM ('sad')",
		"CREATE SCHEMA tenant", "CREATE MATERIALIZED VIEW news_count AS SELECT count(*) AS n FROM news",
		"ALTER TABLE news ENABLE ROW LEVEL SECURITY", "CREATE POLICY mine ON news USING (id > 0)",
		`ALTER TABLE news ADD COLUMN title text COLLATE "C"`, "COMMENT ON COLUMN news.title IS 'Title'", "GRANT SELECT (title) ON news TO PUBLIC",
		"CREATE RULE keep AS ON DELETE TO news DO INSTEAD NOTHING", "GRANT SELECT ON news TO PUBLIC", "COMMENT ON TABLE news IS 'News'",
		"ALTER TABLE news SET UNLOGGED", "ALTER TABLE news REPLICA IDENTITY FULL",
		"CREATE VIEW ids AS SELECT id FROM news", "CREATE RULE keep AS ON DELETE TO ids DO INSTEAD NOTHING", "GRANT SELECT ON ids TO PUBLIC",
		"CREATE SEQUENCE counter INCREMENT BY 5", "SELECT setval('counter', 10, false)", "CREATE TYPE pair AS (a int)")
	v1, err := p.FingerprintAs(ctx, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(plain, v1); len(d) > 0 {
		t.Errorf("format 1 changed: %v", d)
	}
	v2, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(v1, v2); !reflect.DeepEqual(d, []string{"function:public.add1(i integer)", "matview:public.news_count", "schema:tenant", "sequence:public.counter", "table:public.news", "type:public.mood", "type:public.pair", "view:public.ids"}) {
		t.Errorf("formats differ in %v", d)
	}
}
