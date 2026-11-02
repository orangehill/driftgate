package data

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// eachServer runs a test on every server of DRIFTGATE_TEST_MYSQL and
// DRIFTGATE_TEST_POSTGRES.
func eachServer(t *testing.T, fn func(t *testing.T, s *server)) {
	ran := false
	if v := os.Getenv("DRIFTGATE_TEST_MYSQL"); v != "" {
		for _, dsn := range strings.Split(v, ",") {
			u, _ := url.Parse(dsn)
			ran = true
			t.Run("mysql "+u.Host, func(t *testing.T) { fn(t, newServer(t, MySQL, dsn)) })
		}
	}
	if dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES"); dsn != "" {
		ran = true
		t.Run("postgres", func(t *testing.T) { fn(t, newServer(t, Postgres, dsn)) })
	}
	if !ran {
		t.Skip("neither DRIFTGATE_TEST_MYSQL nor DRIFTGATE_TEST_POSTGRES is set")
	}
}

const targetRules = "version: 1\ntables:\n  users:\n    columns: {id: keep, name: universal}\n"

// seedUsers creates the users table the target tests copy.
func seedUsers(t *testing.T, s *server, db string, n int) {
	t.Helper()
	s.create(t, db)
	st := []string{"CREATE TABLE users (id int PRIMARY KEY, name varchar(50))"}
	for i := 1; i <= n; i++ {
		st = append(st, "INSERT INTO users VALUES ("+strconv.Itoa(i)+", 'Ana Petrović "+strconv.Itoa(i)+"')")
	}
	s.exec(t, db, st...)
}

func queryOne(t *testing.T, s *server, db, q string) string {
	t.Helper()
	d, err := open(s.db(db), db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var v string
	if err := d.QueryRow(q).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// databases lists the databases whose names start with prefix.
func databases(t *testing.T, s *server, prefix string) []string {
	t.Helper()
	q := "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA ORDER BY 1"
	if s.driver == Postgres {
		q = "SELECT datname FROM pg_database ORDER BY 1"
	}
	rows, err := s.admin.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

// aliases are other spellings of a server's DSN: another name of the host,
// the driver's own form (MySQL) or the host= parameter (PostgreSQL).
func aliases(t *testing.T, s *server) []string {
	u, err := url.Parse(s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	other := *u
	other.Host = "localhost:" + u.Port()
	out := []string{other.String()}
	if s.driver == MySQL {
		out = append(out, u.User.Username()+":"+pass+"@tcp(localhost:"+u.Port()+")/")
	} else {
		q := u.Query()
		q.Set("host", u.Hostname())
		q.Set("port", u.Port())
		other = *u
		other.Host, other.RawQuery = "", q.Encode()
		out = append(out, other.String())
	}
	return out
}

// TestTargetIsSource: a target that is the source under another spelling
// of the DSN is refused before anything is written; the source stays.
func TestTargetIsSource(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		const src = "dgtarget_same"
		t.Cleanup(func() { s.drop(src) })
		seedUsers(t, s, src, 3)
		rules, _ := ParseRules([]byte(targetRules))
		for _, dsn := range append(aliases(t, s), s.dsn) {
			target := Database{Driver: s.driver, DSN: dsn, Name: src}
			_, err := Anonymize(context.Background(), Options{Source: s.db(src), Target: &target, Replace: true, Rules: rules, Key: testKey})
			if err == nil || !strings.Contains(err.Error(), "the target is the source") {
				t.Errorf("%s: %v", dsn, err)
			}
			if n := queryOne(t, s, src, "SELECT count(*) FROM users WHERE name LIKE 'Ana Petrovi%'"); n != "3" {
				t.Fatalf("%s: the source changed: %s original rows", dsn, n)
			}
		}
	})
}

// TestTargetNotDriftGates: a database DriftGate did not write is never
// dropped while it holds anything (also only routines or sequences); an
// empty one is replaced; DriftGate's own target is replaced with Replace.
func TestTargetNotDriftGates(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src, dst = "dgtarget_src", "dgtarget_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		seedUsers(t, s, src, 3)
		rules, _ := ParseRules([]byte(targetRules))
		target := s.db(dst)
		run := func(replace bool) error {
			_, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: replace, Rules: rules, Key: testKey})
			return err
		}
		objects := [][]string{{"CREATE TABLE app (id int PRIMARY KEY)"}}
		if s.driver == MySQL {
			objects = append(objects, []string{"CREATE PROCEDURE app_proc() SELECT 1"},
				[]string{"CREATE EVENT app_event ON SCHEDULE EVERY 1 DAY DO SELECT 1"})
		} else {
			objects = append(objects, []string{"CREATE SEQUENCE app_seq"},
				[]string{"CREATE FUNCTION app_fn() RETURNS int LANGUAGE sql AS 'SELECT 1'"},
				[]string{"CREATE TYPE app_mood AS ENUM ('ok')"})
		}
		for _, o := range objects {
			s.create(t, dst)
			s.exec(t, dst, o...)
			for _, replace := range []bool{false, true} {
				if err := run(replace); !errors.Is(err, ErrNotTarget) {
					t.Errorf("%s, replace %v: %v", o[0], replace, err)
				}
				if got := databases(t, s, dst); len(got) != 1 {
					t.Fatalf("%s: databases %v", o[0], got)
				}
			}
		}
		// An empty database is replaced, and is DriftGate's afterwards.
		s.create(t, dst)
		if err := run(false); err != nil {
			t.Fatal(err)
		}
		if err := run(false); !errors.Is(err, ErrTargetExists) {
			t.Fatalf("replaced without asking: %v", err)
		}
		if err := run(true); err != nil {
			t.Fatal(err)
		}
		if n := queryOne(t, s, dst, "SELECT count(*) FROM users"); n != "3" {
			t.Fatalf("%s users in the target", n)
		}
		// The marker is DriftGate's: not a table of the schema.
		schema, err := ReadSchema(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		if len(schema.Tables) != 1 || schema.Tables[0].Name != "users" {
			t.Fatalf("tables of the target: %v", schema.Tables)
		}
	})
}

// files fails on the file value failAt (from 1; 0 never).
type files struct{ n, failAt int }

func (f *files) Placeholder(_ context.Context, v FileValue) (string, error) {
	if f.n++; f.n == f.failAt {
		return "", errors.New("disk full")
	}
	return v.Path, nil
}

// cancelAfter cancels the run after some rows, like Ctrl-C.
type cancelAfter struct {
	rows   int
	cancel context.CancelFunc
}

func (c *cancelAfter) Where(*Table) string { return "" }

func (c *cancelAfter) Keep(*Table, []*Column, [][]byte) bool {
	if c.rows--; c.rows == 0 {
		c.cancel()
	}
	return true
}

// dropAfter drops a table of the staging database after some rows, so the
// INSERTs after it fail.
type dropAfter struct {
	rows int
	drop func()
}

func (d *dropAfter) Where(*Table) string { return "" }

func (d *dropAfter) Keep(t *Table, _ []*Column, _ [][]byte) bool {
	if t.Name == "users" {
		if d.rows--; d.rows == 0 {
			d.drop()
		}
	}
	return true
}

// TestTargetFailedRun: a run that fails (an error in the middle of the
// copy, a cancelled context, a failed check) writes no target: an existing
// one stays as it was, and no staging database is left.
func TestTargetFailedRun(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src, dst = "dgtarget_fail_src", "dgtarget_fail_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		seedUsers(t, s, src, 30)
		s.exec(t, src, "CREATE TABLE avatars (id int PRIMARY KEY, path varchar(100))",
			"INSERT INTO avatars VALUES (1, 'a/1.png'), (2, 'a/2.png'), (3, 'a/3.png')")
		s.drop(dst)
		rules, _ := ParseRules([]byte(targetRules + "  avatars:\n    columns: {id: keep, path: file}\n"))
		target := s.db(dst)
		opt := func() Options {
			return Options{Source: s.db(src), Target: &target, Replace: true, Rules: rules, Key: testKey, Files: &files{}, BatchRows: 2}
		}
		left := func(what string) {
			t.Helper()
			if got := databases(t, s, dst+stagingInfix); len(got) > 0 {
				t.Errorf("%s: staging databases left: %v", what, got)
			}
		}

		// No target yet: a failed run makes none.
		o := opt()
		o.Files = &files{failAt: 2}
		if _, err := Anonymize(ctx, o); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("got %v", err)
		}
		if got := databases(t, s, dst); len(got) != 0 {
			t.Fatalf("a failed run made %v", got)
		}

		if _, err := Anonymize(ctx, opt()); err != nil {
			t.Fatal(err)
		}
		before := contentOf(t, target)
		s.exec(t, src, "UPDATE users SET name = 'Changed Name' WHERE id = 1")

		o = opt()
		o.Files = &files{failAt: 2}
		if _, err := Anonymize(ctx, o); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("error in the copy: %v", err)
		}
		left("error in the copy")

		cctx, cancel := context.WithCancel(ctx)
		o = opt()
		o.Filter = &cancelAfter{rows: 5, cancel: cancel}
		if _, err := Anonymize(cctx, o); err == nil { // context canceled, or pgx's bad connection
			t.Fatal("a cancelled run passed")
		}
		cancel()
		left("cancelled")

		// An INSERT that fails, while the rows after it are read.
		o = opt()
		o.Filter = &dropAfter{rows: 5, drop: func() {
			// The writer makes the table on its own goroutine; wait for it.
			for range 200 {
				for _, name := range databases(t, s, dst+stagingInfix) {
					q := "SELECT count(*) FROM information_schema.tables WHERE table_schema = '" + name + "' AND table_name = 'users'"
					if s.driver == Postgres {
						q = "SELECT count(*) FROM information_schema.tables WHERE table_name = 'users'"
					}
					if queryOne(t, s, name, q) == "1" {
						s.exec(t, name, "DROP TABLE users")
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("the staging database got no users table")
		}}
		if _, err := Anonymize(ctx, o); err == nil || !strings.HasPrefix(err.Error(), "target: ") || !strings.Contains(err.Error(), "users") {
			t.Fatalf("a failed INSERT: %v", err)
		}
		left("a failed INSERT")

		o = opt()
		var checked string
		o.Verify = func(ctx context.Context, staging *Database) error {
			checked = queryOne(t, s, staging.Name, "SELECT count(*) FROM users")
			return errors.New("findings")
		}
		if _, err := Anonymize(ctx, o); err == nil || err.Error() != "findings" {
			t.Fatalf("check: %v", err)
		}
		if checked != "30" {
			t.Errorf("the check saw %q users", checked)
		}
		left("check")

		if after := contentOf(t, target); after != before {
			t.Fatalf("a failed run changed the target:\n%s", firstDifference([]byte(before), []byte(after)))
		}
	})
}

// TestMySQLTriggerOfTheCopy: a trigger whose body names the source
// database writes into the copy, not into the source.
func TestMySQLTriggerOfTheCopy(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		if s.driver != MySQL {
			t.Skip("MySQL and MariaDB")
		}
		const src, dst = "dgtarget_trg_src", "dgtarget_trg_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		seedUsers(t, s, src, 2)
		s.drop(dst)
		s.exec(t, src, "CREATE TABLE audit (id int AUTO_INCREMENT PRIMARY KEY, user_id int)",
			"CREATE TRIGGER users_ai AFTER INSERT ON users FOR EACH ROW INSERT INTO `"+src+"`.`audit` (user_id) VALUES (NEW.id)",
			"CREATE TRIGGER users_au AFTER UPDATE ON users FOR EACH ROW INSERT INTO "+src+".audit (user_id) VALUES (NEW.id)",
			"CREATE VIEW audited AS SELECT u.id FROM `"+src+"`.users u JOIN "+src+".audit a ON a.user_id = u.id")
		rules, _ := ParseRules([]byte(targetRules + "  audit:\n    columns: {id: keep, user_id: keep}\n"))
		target := s.db(dst)
		if _, err := Anonymize(context.Background(), Options{Source: s.db(src), Target: &target, Rules: rules, Key: testKey}); err != nil {
			t.Fatal(err)
		}
		s.exec(t, dst, "INSERT INTO users VALUES (100, 'x')", "UPDATE users SET name = 'y' WHERE id = 1")
		if n := queryOne(t, s, src, "SELECT count(*) FROM audit"); n != "0" {
			t.Errorf("the copy's triggers wrote %s rows into the source", n)
		}
		if n := queryOne(t, s, dst, "SELECT count(*) FROM audited"); n != "2" {
			t.Errorf("the copy's audit has %s rows", n)
		}
	})
}
