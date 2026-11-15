package data

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orangehill/driftgate/internal/dbx"
)

// Integration tests on real servers: DRIFTGATE_TEST_MYSQL lists mysql://
// URLs (MySQL and MariaDB, comma-separated), DRIFTGATE_TEST_POSTGRES one
// postgres:// URL. DRIFTGATE_TEST_DATA_USERS sets how many users the seed
// has (default 60), for measuring throughput.

func testUsers() int {
	if n, err := strconv.Atoi(os.Getenv("DRIFTGATE_TEST_DATA_USERS")); err == nil && n > 0 {
		return n
	}
	return 60
}

var (
	firstNames = []string{"Petar", "Đorđe", "Jovana", "Šćepan", "Željko", "Čedomir", "Ćirilo", "Јован", "Љубица", "Ђурђа", "Milica", "Ana-Marija"}
	lastNames  = []string{"Petrović", "Šćepanović", "Marković", "Žižić", "Đukić", "Јовановић", "Њeгош", "O'Brien", "Čolić", "Ilić"}
)

// person is one seeded user; every field but id is personal.
type person struct {
	id                                      int
	name, email, phone, bio, settings, html string
}

func people(n int) []person {
	var out []person
	for i := 1; i <= n; i++ {
		out = append(out, newPerson(i))
	}
	return out
}

func newPerson(i int) person {
	first, last := firstNames[i%len(firstNames)], lastNames[(i/len(firstNames))%len(lastNames)]
	name := fmt.Sprintf("%s %s", first, last)
	email := fmt.Sprintf("%s.user%d@primer-mejl.example", strings.ToLower(strings.ReplaceAll(first, "-", "")), i)
	phone := fmt.Sprintf("+381 6%d %03d %04d", i%10, i%1000, (i*7919)%10000)
	return person{
		id: i, name: name, email: email, phone: phone,
		bio:      fmt.Sprintf("Hi, I am %s. Write to me at %s or call %s. I love the mountains.", first, email, phone),
		settings: fmt.Sprintf(`{"phone": "%s", "contacts": [{"email": "backup%d@rezerva-mejl.example", "label": "rezerva"}], "theme": "dark"}`, phone, i),
		html:     fmt.Sprintf(`<p>Author: <a href="mailto:%s" title="%s">write</a></p><p>Contact phone: %s</p>`, email, email, phone),
	}
}

// invoiceSequence is the sequence of seedStatements on MariaDB, "" on
// other servers. A Galera node creates a sequence with MariaDB's default
// cache only with an increment of 0 (auto_increment_increment, 1235).
func invoiceSequence(s *server, mariadb bool) string {
	if !mariadb {
		return ""
	}
	def := "CREATE SEQUENCE invoice_numbers START WITH 100"
	if isGalera(s.admin) {
		def += " INCREMENT BY 0"
	}
	return def
}

// isGalera reports whether the server is a Galera node (wsrep_on).
func isGalera(db *sql.DB) bool {
	var wsrep int
	return db.QueryRow("SELECT @@wsrep_on").Scan(&wsrep) == nil && wsrep != 0
}

func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// seedStatements builds the source database: references (also on a personal
// column), a generated column, a trigger, views over views, JSON, HTML and
// free text with embedded personal data, serialized PHP, a table without a
// primary key; on MariaDB (sequence, from invoiceSequence) a sequence.
func seedStatements(driver string, ps []person, sequence string) []string {
	var st []string
	if driver == MySQL {
		st = append(st,
			"CREATE TABLE users (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100) NOT NULL, email VARCHAR(191) NOT NULL UNIQUE, password VARCHAR(255) NOT NULL, phone VARCHAR(30) NULL, bio TEXT NULL, settings JSON NULL, remember_token VARCHAR(100) NULL, created_at DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', avatar BLOB NULL, status ENUM('active','blocked') NOT NULL DEFAULT 'active') AUTO_INCREMENT=5000",
			"CREATE TABLE posts (id INT AUTO_INCREMENT PRIMARY KEY, user_id INT NOT NULL, author_email VARCHAR(191) NOT NULL, title VARCHAR(200) NOT NULL, body MEDIUMTEXT NOT NULL, title_lower VARCHAR(200) GENERATED ALWAYS AS (LOWER(title)) STORED, CONSTRAINT posts_user FOREIGN KEY (user_id) REFERENCES users(id), CONSTRAINT posts_email FOREIGN KEY (author_email) REFERENCES users(email))",
			"CREATE TABLE post_log (id INT AUTO_INCREMENT PRIMARY KEY, post_id INT NOT NULL, action VARCHAR(20) NOT NULL)",
			"CREATE TRIGGER posts_ai AFTER INSERT ON posts FOR EACH ROW BEGIN\n  INSERT INTO post_log (post_id, action) VALUES (NEW.id, 'insert');\nEND",
			"CREATE TABLE cache (`key` VARCHAR(191) PRIMARY KEY, value MEDIUMTEXT NOT NULL, expiration INT NOT NULL)",
			"CREATE TABLE tags (name VARCHAR(50) NOT NULL, note VARCHAR(50) NULL)",
			"CREATE VIEW user_post_count AS SELECT email, COUNT(*) AS n FROM user_posts GROUP BY email",
		)
		// The view of a view is created first on purpose; MySQL needs its base.
		st = append(st[:len(st)-1], "CREATE VIEW user_posts AS SELECT u.email, p.title FROM users u JOIN posts p ON p.user_id = u.id", st[len(st)-1])
		// Routines and an event; a view that calls a function, and a view
		// and a trigger made with another collation than the session's.
		st = append(st,
			"CREATE FUNCTION shout(t VARCHAR(200)) RETURNS VARCHAR(200) DETERMINISTIC RETURN UPPER(t)",
			"CREATE PROCEDURE count_posts(OUT n INT) BEGIN\n  SELECT COUNT(*) INTO n FROM posts;\nEND",
			"CREATE EVENT purge_cache ON SCHEDULE EVERY 1 DAY STARTS '2030-01-01 00:00:00' DISABLE DO DELETE FROM cache WHERE expiration < 0",
			"CREATE VIEW loud_titles AS SELECT shout(title) AS title FROM posts",
			"SET NAMES utf8mb4 COLLATE utf8mb4_bin",
			"CREATE VIEW case_check AS SELECT 'a' = 'A' AS same",
			"CREATE TRIGGER post_log_bi BEFORE INSERT ON post_log FOR EACH ROW SET NEW.action = IF('a' = 'A', NEW.action, CONCAT(NEW.action, ':bin'))",
			"SET NAMES utf8mb4",
		)
		if sequence != "" {
			st = append(st, sequence, "SELECT NEXTVAL(invoice_numbers)", "SELECT NEXTVAL(invoice_numbers)",
				// One that has cycled: its count of cycles is copied too.
				"CREATE SEQUENCE rounds MINVALUE 1 MAXVALUE 2 NOCACHE CYCLE", "SELECT NEXTVAL(rounds)", "SELECT NEXTVAL(rounds)", "SELECT NEXTVAL(rounds)")
		}
	} else {
		st = append(st,
			"CREATE TYPE mood AS ENUM ('ok', 'sad')",
			"CREATE TABLE users (id serial PRIMARY KEY, name text NOT NULL, email varchar(191) NOT NULL UNIQUE, password text NOT NULL, phone text, bio text, settings jsonb, remember_token varchar(100), created_at timestamptz NOT NULL DEFAULT now(), avatar bytea, status mood NOT NULL DEFAULT 'ok')",
			"CREATE TABLE posts (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, user_id int NOT NULL REFERENCES users(id), author_email varchar(191) NOT NULL REFERENCES users(email), title text NOT NULL, body text NOT NULL, title_lower text GENERATED ALWAYS AS (lower(title)) STORED)",
			"CREATE INDEX posts_title ON posts (title)",
			"CREATE TABLE post_log (id bigserial PRIMARY KEY, post_id int NOT NULL, action text NOT NULL CHECK (action <> ''))",
			"CREATE FUNCTION log_post() RETURNS trigger LANGUAGE plpgsql AS $$\nBEGIN\n  INSERT INTO public.post_log (post_id, action) VALUES (NEW.id, 'insert');\n  RETURN NEW;\nEND\n$$",
			"CREATE TRIGGER posts_ai AFTER INSERT ON posts FOR EACH ROW EXECUTE FUNCTION log_post()",
			"CREATE TABLE cache (key text PRIMARY KEY, value text NOT NULL, expiration int NOT NULL)",
			"CREATE TABLE tags (name text NOT NULL, note text)",
			"CREATE VIEW user_posts AS SELECT u.email, p.title FROM users u JOIN posts p ON p.user_id = u.id",
			"CREATE VIEW user_post_count AS SELECT email, count(*) AS n FROM user_posts GROUP BY email",
			"CREATE SCHEMA billing",
			"CREATE TABLE billing.invoices (id int PRIMARY KEY, user_id int REFERENCES public.users(id), iban text)",
			"CREATE SEQUENCE invoice_numbers START 100",
			"SELECT nextval('invoice_numbers')", "SELECT nextval('invoice_numbers')",
		)
	}
	st = append(st, seedRows(driver, ps)...)
	st = append(st,
		"INSERT INTO cache VALUES ('user:1', "+lit(`a:2:{s:5:"email";s:26:"cache.user@firma-x.example";s:4:"name";s:5:"Petar";}`)+", 0)",
		"INSERT INTO tags VALUES ('teme', NULL), ('teme', NULL), ('sport', 'x')",
	)
	if driver == Postgres {
		st = append(st, fmt.Sprintf("SELECT setval('users_id_seq', %d)", len(ps)))
		st = append(st, "INSERT INTO billing.invoices VALUES (1, 1, 'RS35260005601001611379'), (2, 2, NULL)")
	}
	return st
}

// seedRows inserts the users and their posts, 200 to a statement.
func seedRows(driver string, ps []person) []string {
	var st []string
	hash := "$2y$10$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234"
	for start := 0; start < len(ps); start += 200 {
		var users, posts []string
		for _, p := range ps[start:min(start+200, len(ps))] {
			created := "'2024-01-02 03:04:05'"
			if driver == MySQL && p.id == 1 {
				created = "'0000-00-00 00:00:00'"
			}
			avatar := "NULL"
			if p.id%3 == 0 {
				if driver == MySQL {
					avatar = "X'00ff10'"
				} else {
					avatar = `'\x00ff10'`
				}
			}
			users = append(users, fmt.Sprintf("(%d, %s, %s, %s, %s, %s, %s, %s, %s, %s)", p.id, lit(p.name), lit(p.email), lit(hash), lit(p.phone),
				lit(p.bio), lit(p.settings), lit(fmt.Sprintf("tok%040x", p.id*104729)), created, avatar))
			posts = append(posts, fmt.Sprintf("(%d, %s, %s, %s)", p.id, lit(p.email), lit(fmt.Sprintf("Naslov %d", p.id)), lit(p.html)))
		}
		st = append(st, "INSERT INTO users (id, name, email, password, phone, bio, settings, remember_token, created_at, avatar) VALUES "+strings.Join(users, ", "))
		st = append(st, "INSERT INTO posts (user_id, author_email, title, body) VALUES "+strings.Join(posts, ", "))
	}
	return st
}

const testRules = `version: 1
tables:
  users:
    columns:
      id: keep
      name: universal
      email: email
      password: password
      phone: universal
      bio: free-text
      settings:
        kind: personal
        strategy: json
        paths:
          "$.phone": universal
          "$.contacts[*].email": email
      remember_token: token
      created_at: keep
      avatar: null
      status: keep
  posts:
    columns:
      id: keep
      user_id: keep
      author_email: email
      title: keep
      body: html
      title_lower: keep
  post_log:
    columns: { id: keep, post_id: keep, action: keep }
  cache:
    columns:
      key: keep
      value: serialized-php
      expiration: keep
  tags:
    columns: { name: keep, note: keep }
`

const testRulesPostgres = testRules + `  billing.invoices:
    columns: { id: keep, user_id: keep, iban: universal }
`

type server struct {
	driver string
	dsn    string
	admin  *sql.DB
}

func (s *server) db(name string) Database { return Database{Driver: s.driver, DSN: s.dsn, Name: name} }

func (s *server) exec(t testing.TB, db string, stmts ...string) {
	t.Helper()
	d, err := open(s.db(db), db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	if s.driver == MySQL {
		d.Exec("SET SESSION sql_mode = ''")
	}
	for _, q := range stmts {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%.120s: %v", q, err)
		}
	}
}

func (s *server) drop(names ...string) {
	for _, n := range names {
		if s.driver == MySQL {
			s.admin.Exec("DROP DATABASE IF EXISTS " + mysqlDialect{}.quote(n))
		} else {
			s.admin.Exec("DROP DATABASE IF EXISTS " + postgresDialect{}.quote(n) + " WITH (FORCE)")
		}
	}
}

func (s *server) create(t testing.TB, name string) {
	t.Helper()
	s.drop(name)
	q := "CREATE DATABASE " + mysqlDialect{}.quote(name) + " CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"
	if s.driver == Postgres {
		q = "CREATE DATABASE " + postgresDialect{}.quote(name)
	}
	if _, err := s.admin.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func newServer(t testing.TB, driver, dsn string) *server {
	t.Helper()
	s := &server{driver: driver, dsn: dsn}
	admin, err := open(s.db(""), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if err = admin.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatalf("%s: %v", dsn, err)
	}
	s.admin = admin
	return s
}

func TestMySQLAnonymize(t *testing.T) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testAnonymize(t, newServer(t, MySQL, dsn)) })
	}
}

func TestPostgresAnonymize(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	testAnonymize(t, newServer(t, Postgres, dsn))
}

// TestPostgresPublishOtherSessions: publish ends the other sessions on the
// staging database before it renames it. The target's DSN names a role
// that is not a superuser, which may not end a superuser's session: (a) a
// session that ends within the server's wait (5 s) does not stop publish;
// (b) one that stays stops it after that wait with "being accessed by
// other users" and the permission error (42501), not at once with the
// permission error alone.
func TestPostgresPublishOtherSessions(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	s := newServer(t, Postgres, dsn)
	ctx := context.Background()
	const role, staging, target = "dgdata_pub_app", "dgdata_pub_staging", "dgdata_pub_target"
	drop := func() {
		s.drop(staging, target)
		s.admin.Exec("DROP ROLE IF EXISTS " + role)
	}
	drop()
	t.Cleanup(drop)
	if _, err := s.admin.Exec("CREATE ROLE " + role + " LOGIN PASSWORD 'app' CREATEDB"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "app")
	app := &server{driver: Postgres, dsn: u.String()}
	e := &engine{dl: postgresDialect{}}

	// publish runs while a superuser's session on the staging database
	// runs q; done gets the session's error.
	publish := func(t *testing.T, q string) (took time.Duration, done chan error, err error) {
		t.Helper()
		s.drop(staging, target)
		if _, err := s.admin.Exec("CREATE DATABASE " + staging + " OWNER " + role); err != nil {
			t.Fatal(err)
		}
		d, err := open(s.db(staging), staging)
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		go func() {
			_, err := d.Exec(q)
			d.Close()
			done <- err
		}()
		for i := 0; ; i++ {
			var n int
			s.admin.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query = $2", staging, q).Scan(&n)
			if n > 0 {
				break
			}
			if i == 200 {
				t.Fatalf("no session on %s", staging)
			}
			time.Sleep(10 * time.Millisecond)
		}
		start := time.Now()
		err = e.publish(ctx, app.db(staging), app.db(target), false)
		return time.Since(start), done, err
	}

	t.Run("a session that ends", func(t *testing.T) {
		_, done, err := publish(t, "SELECT pg_sleep(1.5)")
		if err != nil {
			t.Fatalf("publish over a session that ends: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("the session was ended: %v", err)
		}
		var n int
		if err := s.admin.QueryRow("SELECT count(*) FROM pg_database WHERE datname = $1", target).Scan(&n); err != nil || n != 1 {
			t.Fatalf("target after publish: %d, %v", n, err)
		}
	})
	t.Run("a session that stays", func(t *testing.T) {
		took, done, err := publish(t, "SELECT pg_sleep(60)")
		s.admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", staging)
		<-done
		pe := (*pgconn.PgError)(nil)
		if !errors.As(err, &pe) || pe.Code != "55006" || !strings.Contains(err.Error(), "SQLSTATE 42501") {
			t.Fatalf("publish over a superuser's session: %v", err)
		}
		if took > 8*time.Second {
			t.Fatalf("the error took %v", took)
		}
	})
}

func testAnonymize(t *testing.T, s *server) {
	ctx := context.Background()
	const src, dst, loaded = "dgdata_src", "dgdata_dst", "dgdata_loaded"
	t.Cleanup(func() { s.drop(src, dst, loaded) })
	mariadb := false
	if s.driver == MySQL {
		var version string
		s.admin.QueryRow("SELECT VERSION()").Scan(&version)
		mariadb = dbx.IsMariaDB(version)
	}
	ps := people(testUsers())
	s.create(t, src)
	s.drop(dst, loaded)
	s.exec(t, src, seedStatements(s.driver, ps, invoiceSequence(s, mariadb))...)

	rulesText := testRules
	if s.driver == Postgres {
		rulesText = testRulesPostgres
	}
	rules, err := ParseRules([]byte(rulesText))
	if err != nil {
		t.Fatal(err)
	}
	target := s.db(dst)
	run := func(target *Database, replace bool) ([]byte, *Report) {
		t.Helper()
		var dump bytes.Buffer
		rep, err := Anonymize(ctx, Options{Source: s.db(src), Target: target, Replace: replace, Dump: &dump, Rules: rules, Key: testKey, BatchRows: 50})
		if err != nil {
			t.Fatal(err)
		}
		return dump.Bytes(), rep
	}
	start := time.Now()
	dump1, rep := run(&target, false)
	elapsed := time.Since(start)
	t.Logf("%d rows in %s (%.0f rows/s), dump %d bytes", rep.Rows, elapsed.Round(time.Millisecond), float64(rep.Rows)/elapsed.Seconds(), len(dump1))

	// Byte-identical dumps: dump only, and again with a replaced target.
	dump2, _ := run(nil, false)
	if !bytes.Equal(dump1, dump2) {
		t.Fatalf("dumps differ:\n%s", firstDifference(dump1, dump2))
	}
	if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Rules: rules, Key: testKey}); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("an existing target was replaced without asking: %v", err)
	}
	dump3, _ := run(&target, true)
	if !bytes.Equal(dump1, dump3) {
		t.Fatalf("dumps differ after replacing the target:\n%s", firstDifference(dump1, dump3))
	}
	// Another key, another output.
	var other bytes.Buffer
	if _, err := Anonymize(ctx, Options{Source: s.db(src), Dump: &other, Rules: rules, Key: []byte("another key, another key, 32 by")}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other.Bytes(), dump1) {
		t.Fatal("a different key gave the same dump")
	}

	tdb, err := open(target, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()
	sdb, err := open(s.db(src), src)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	count := func(db *sql.DB, q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// Joins still match, also over a personal column, and the views work.
	for _, q := range []string{
		"SELECT COUNT(*) FROM users u JOIN posts p ON p.user_id = u.id",
		"SELECT COUNT(*) FROM users u JOIN posts p ON p.author_email = u.email AND p.user_id = u.id",
		"SELECT COUNT(*) FROM user_post_count",
		"SELECT COUNT(*) FROM post_log",
		"SELECT COUNT(*) FROM tags",
		"SELECT COUNT(DISTINCT email) FROM users",
	} {
		if a, b := count(sdb, q), count(tdb, q); a != b || a == 0 {
			t.Errorf("%s: source %d, target %d", q, a, b)
		}
	}
	if n := count(tdb, "SELECT COUNT(*) FROM cache"); n != 0 {
		t.Errorf("serialized PHP table has %d rows", n)
	}
	if s.driver == MySQL {
		// Routines and events are copied; views and triggers keep their collation.
		for _, q := range []string{
			"SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE()",
			"SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = DATABASE() AND STATUS = 'DISABLED'",
			"SELECT COUNT(*) FROM loud_titles WHERE title = BINARY UPPER(title)",
			"SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = DATABASE() AND COLLATION_CONNECTION = 'utf8mb4_bin'",
			"SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = DATABASE() AND COLLATION_CONNECTION = 'utf8mb4_bin'",
		} {
			if a, b := count(sdb, q), count(tdb, q); a != b || a == 0 {
				t.Errorf("%s: source %d, target %d", q, a, b)
			}
		}
	}
	if n := count(tdb, "SELECT COUNT(*) FROM users WHERE avatar IS NOT NULL"); n != 0 {
		t.Errorf("nulled column has %d values", n)
	}
	want := "$2y$10$UA27VjnZnIYNSwBEzD1bq.divaC6Z62cXSBxtud9Ddo7GSGMwtF02"
	if n := count(tdb, "SELECT COUNT(*) FROM users WHERE password = '"+want+"'"); n != len(ps) {
		t.Errorf("%d of %d passwords are the test password", n, len(ps))
	}
	// The e-mail in the HTML body is the stand-in of the author's address.
	if n := count(tdb, "SELECT COUNT(*) FROM users u JOIN posts p ON p.user_id = u.id WHERE p.body LIKE CONCAT('%mailto:', u.email, '%')"); n != len(ps) {
		t.Errorf("%d of %d bodies carry their author's stand-in address", n, len(ps))
	}
	// The dump loads into an empty database and gives the same data.
	s.create(t, loaded)
	s.exec(t, loaded, splitDump(t, s.driver, string(dump1))...)
	if a, b := contentOf(t, target), contentOf(t, s.db(loaded)); a != b {
		t.Errorf("the loaded dump differs from the target:\n%s", firstDifference([]byte(a), []byte(b)))
	}

	// The trigger did not fire during the copy, and fires afterwards.
	before := count(tdb, "SELECT COUNT(*) FROM post_log")
	if s.driver == MySQL {
		s.exec(t, dst, "INSERT INTO posts (user_id, author_email, title, body) SELECT id, email, 'x', 'y' FROM users ORDER BY id LIMIT 1")
	} else {
		s.exec(t, dst, "INSERT INTO posts (user_id, author_email, title, body) SELECT id, email, 'x', 'y' FROM users ORDER BY id LIMIT 1")
	}
	if after := count(tdb, "SELECT COUNT(*) FROM post_log"); after != before+1 {
		t.Errorf("trigger: %d → %d", before, after)
	}
	if s.driver == MySQL {
		autoInc := func(db *sql.DB) string {
			var name, def string
			if err := db.QueryRow("SHOW CREATE TABLE users").Scan(&name, &def); err != nil {
				t.Fatal(err)
			}
			_, after, _ := strings.Cut(def, "AUTO_INCREMENT=")
			n, _, _ := strings.Cut(after, " ")
			return n
		}
		if a, b := autoInc(sdb), autoInc(tdb); a != b || a == "" {
			t.Errorf("AUTO_INCREMENT %s in the source, %s in the target", a, b)
		}
		if mariadb {
			// MariaDB tells where a sequence continues only past its cache
			// (next_not_cached_value): a gap, as after a server restart.
			if n := count(tdb, "SELECT NEXTVAL(invoice_numbers)"); n != 1100 {
				t.Errorf("sequence continues at %d", n)
			}
			var next, cycles int
			if err := tdb.QueryRow("SELECT next_not_cached_value, cycle_count FROM rounds").Scan(&next, &cycles); err != nil || next != 2 || cycles != 1 {
				t.Errorf("cycled sequence: next %d, cycles %d, %v; want 2, 1", next, cycles, err)
			}
		}
	} else {
		if n := count(tdb, "SELECT nextval('public.invoice_numbers')"); n != 102 {
			t.Errorf("sequence continues at %d", n)
		}
		if n := count(tdb, "SELECT nextval(pg_get_serial_sequence('public.users', 'id'))"); n != len(ps)+1 {
			t.Errorf("serial continues at %d", n)
		}
	}

	// No original personal value anywhere in the target or the dump.
	var needles []string
	err = PersonalValues(ctx, s.db(src), rules, nil, func(v PersonalValue) error {
		if utf8.RuneCountInString(v.Value) >= 6 {
			needles = append(needles, strings.ToLower(v.Value))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needles) < len(ps)*5 {
		t.Fatalf("only %d personal values", len(needles))
	}
	// A large seed (throughput runs) checks an even sample of the values.
	if step := len(needles) / 3000; step > 1 {
		var sample []string
		for i := 0; i < len(needles); i += step {
			sample = append(sample, needles[i])
		}
		needles = sample
	}
	srcText := strings.ToLower(contentOf(t, s.db(src)))
	tgtText := strings.ToLower(contentOf(t, target))
	dumpText := strings.ToLower(string(dump1))
	found := 0
	for _, n := range needles {
		if strings.Contains(srcText, n) {
			found++
		}
		if strings.Contains(tgtText, n) {
			t.Errorf("original value (%d characters) in the target: %q", utf8.RuneCountInString(n), n)
		}
		if strings.Contains(dumpText, n) {
			t.Errorf("original value (%d characters) in the dump", utf8.RuneCountInString(n))
		}
	}
	if found < len(needles)*9/10 {
		t.Fatalf("the search does not work: %d of %d values found in the source", found, len(needles))
	}

	// A new column without a rule stops the run before anything is written.
	s.exec(t, src, "ALTER TABLE users ADD COLUMN nickname VARCHAR(50) NULL")
	_, err = Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: true, Rules: rules, Key: testKey})
	var ce *CheckError
	if !errors.As(err, &ce) || len(ce.Unclassified) != 1 || ce.Unclassified[0] != "users.nickname" {
		t.Fatalf("got %v", err)
	}
	if n := count(tdb, "SELECT COUNT(*) FROM users"); n != len(ps) {
		t.Fatalf("the target changed: %d users", n)
	}

	// classify proposes rules for it and keeps the rest.
	cls, err := Classify(ctx, s.db(src), rules, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(cls.Tables) != 1 || cls.Tables[0].Table != "users" || len(cls.Tables[0].Columns) != 1 || cls.Tables[0].Columns[0].Column != "nickname" {
		t.Fatalf("%+v", cls)
	}
	all, err := Classify(ctx, s.db(src), &Rules{Version: 1, Tables: map[string]*TableRule{}}, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tp := range all.Tables {
		if tp.Action != "" {
			got[tp.Table] = "table:" + tp.Action
		}
		for _, cp := range tp.Columns {
			r := cp.Rule.Kind
			if r == KindPersonal {
				r = cp.Rule.Strategy
			}
			got[tp.Table+"."+cp.Column] = r
		}
	}
	for k, v := range map[string]string{
		"users.email": StrategyEmail, "users.password": StrategyPassword, "users.name": StrategyUniversal, "users.phone": StrategyUniversal,
		"users.bio": StrategyFreeText, "users.settings": StrategyJSON, "users.remember_token": StrategyToken, "posts.body": StrategyHTML,
		"posts.author_email": StrategyEmail, "posts.title": KindKeep, "cache": "table:empty", "users.id": KindKeep,
	} {
		if got[k] != v {
			t.Errorf("classify %s: %s, want %s", k, got[k], v)
		}
	}
}

// contentOf reads every row of every table as text, in key order.
func contentOf(t *testing.T, d Database) string {
	t.Helper()
	ctx := context.Background()
	dl, _ := dialectFor(d.Driver)
	db, err := open(d, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := dl.session(ctx, c); err != nil {
		t.Fatal(err)
	}
	s, err := dl.schema(ctx, c, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, tb := range s.Tables {
		rows, err := queryStrings(ctx, c, dl.selectRows(tb, tb.Columns, ""))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("## " + tb.Name + "\n")
		for _, r := range rows {
			b.WriteString(strings.Join(r, "\t") + "\n")
		}
	}
	return b.String()
}

// splitDump splits a dump into statements the way mysql and psql read it:
// quotes, dollar quotes, comments and (MySQL) DELIMITER lines.
func splitDump(t *testing.T, driver, dump string) []string {
	t.Helper()
	var out []string
	delim := ";"
	var cur strings.Builder
	i := 0
	for i < len(dump) {
		atLineStart := i == 0 || dump[i-1] == '\n'
		if atLineStart && strings.TrimSpace(cur.String()) == "" {
			if strings.HasPrefix(dump[i:], "--") {
				i += strings.IndexByte(dump[i:], '\n') + 1
				continue
			}
			if driver == MySQL && strings.HasPrefix(dump[i:], "DELIMITER ") {
				end := strings.IndexByte(dump[i:], '\n')
				delim = strings.TrimSpace(dump[i+len("DELIMITER ") : i+end])
				i += end + 1
				continue
			}
		}
		if strings.HasPrefix(dump[i:], delim) {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
			i += len(delim)
			continue
		}
		c := dump[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(dump) {
				if driver == MySQL && dump[j] == '\\' && c != '`' {
					j += 2
					continue
				}
				if dump[j] == c {
					if j+1 < len(dump) && dump[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			cur.WriteString(dump[i : j+1])
			i = j + 1
			continue
		case c == '$' && driver == Postgres:
			if end := strings.IndexByte(dump[i+1:], '$'); end >= 0 && !strings.ContainsAny(dump[i+1:i+1+end], " \n;'(),") {
				tag := dump[i : i+end+2]
				end := strings.Index(dump[i+len(tag):], tag)
				if end >= 0 {
					j := i + len(tag) + end + len(tag)
					cur.WriteString(dump[i:j])
					i = j
					continue
				}
			}
		}
		cur.WriteByte(c)
		i++
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func firstDifference(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	from := max(0, i-200)
	return fmt.Sprintf("at byte %d:\n--- first\n%s\n--- second\n%s", i, a[from:min(len(a), i+200)], b[from:min(len(b), i+200)])
}
