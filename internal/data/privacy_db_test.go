package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
)

func eachMySQL(t *testing.T, fn func(t *testing.T, s *server)) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { fn(t, newServer(t, MySQL, dsn)) })
	}
}

func postgresServer(t *testing.T) *server {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	return newServer(t, Postgres, dsn)
}

func (s *server) query(t *testing.T, db, q string) string {
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

// TestMySQLStandInsFollowCollations: values a case- and accent-insensitive
// PAD SPACE collation counts as equal (José and Jose, abc and "abc ") still
// join after anonymize, and values it counts as different stay different
// (a unique column of names with accents), under the server's collations.
func TestMySQLStandInsFollowCollations(t *testing.T) {
	eachMySQL(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src, dst = "dgcoll_src", "dgcoll_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		var def string
		s.admin.QueryRow("SELECT @@collation_server").Scan(&def)
		colls := []string{"utf8mb4_unicode_ci", "utf8mb4_general_ci"}
		if strings.HasPrefix(def, "utf8mb4_") { // MariaDB 10.3's is latin1_swedish_ci
			colls = append(colls, def)
		}
		for _, coll := range colls {
			s.create(t, src)
			s.drop(dst)
			stmts := []string{
				"CREATE TABLE users (name varchar(50) PRIMARY KEY, nick varchar(20) NOT NULL UNIQUE) COLLATE " + coll,
				"CREATE TABLE orders (id int PRIMARY KEY, user_name varchar(50) NOT NULL, FOREIGN KEY (user_name) REFERENCES users(name)) COLLATE " + coll,
				"INSERT INTO users VALUES ('José Ruiz', 'x1'), ('abc', 'x2'), ('Ana Petrović', 'x3'), ('Strauß', 'x4')",
			}
			// Each reference spelled otherwise, where the collation counts it
			// as equal (general_ci has no ß = ss, a NO PAD collation counts
			// trailing spaces).
			for i, p := range [][2]string{{"Jose Ruiz", "José Ruiz"}, {"abc ", "abc"}, {"ANA PETROVIC", "Ana Petrović"}, {"José Ruiz", "José Ruiz"}, {"STRAUSS", "Strauß"}} {
				if s.query(t, "", "SELECT _utf8mb4"+lit(p[0])+" COLLATE "+coll+" = "+lit(p[1])) == "1" {
					stmts = append(stmts, fmt.Sprintf("INSERT INTO orders VALUES (%d, %s)", i, lit(p[0])))
				}
			}
			// Nicknames that differ in a letter with an accent: different under
			// the collation, and so must their stand-ins be.
			var nicks []string
			n := 0
			for _, a := range "bcdfghjklmnp" {
				for _, v := range "àèìòù" {
					for _, b := range "rstvz" {
						n++
						nicks = append(nicks, fmt.Sprintf("('n%d', 'k%c%c%c')", n, a, v, b))
					}
				}
			}
			stmts = append(stmts, "INSERT INTO users VALUES "+strings.Join(nicks, ", "))
			s.exec(t, src, stmts...)
			rules, err := ParseRules([]byte("version: 1\ntables:\n  users:\n    columns: {name: universal, nick: universal}\n  orders:\n    columns: {id: keep, user_name: universal}\n"))
			if err != nil {
				t.Fatal(err)
			}
			target := s.db(dst)
			var dump bytes.Buffer
			if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey}); err != nil {
				t.Fatalf("%s: %v", coll, err)
			}
			join := "SELECT count(*) FROM orders o JOIN users u ON u.name = o.user_name"
			if a, b := s.query(t, src, join), s.query(t, dst, join); a != b {
				t.Errorf("%s: %s rows join in the source, %s after anonymize", coll, a, b)
			}
			distinct := "SELECT count(DISTINCT nick) FROM users"
			if a, b := s.query(t, src, distinct), s.query(t, dst, distinct); a != b {
				t.Errorf("%s: %s different nicknames in the source, %s after anonymize", coll, a, b)
			}
			if v := s.query(t, dst, "SELECT count(*) FROM users WHERE name IN ('José Ruiz', 'abc', 'Ana Petrović')"); v != "0" {
				t.Errorf("%s: %s originals in the target", coll, v)
			}
			rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Dump: bytes.NewReader(dump.Bytes()), Rules: rules, Key: testKey})
			if err != nil || !rep.Clean() {
				t.Errorf("%s: verify %v %+v", coll, err, rep)
			}
		}
	})
}

// schemaLiteralStatements create schema objects that hold personal data:
// a comment, a default, a view, a trigger (and on PostgreSQL a function and
// a check constraint). The view names a user of the rows.
func schemaLiteralStatements(driver string) []string {
	if driver == MySQL {
		return []string{
			"CREATE TABLE users (id int PRIMARY KEY, email varchar(100) COMMENT 'owner: jelena.jovanovic@firma.example', note varchar(100) DEFAULT 'ask milica.mikic@firma.example', created datetime NOT NULL DEFAULT '0000-00-00 00:00:00')",
			"INSERT INTO users (id, email, created) VALUES (1, 'ana.petrovic@posta.example', '2024-01-01'), (2, 'pera@firma.example', '2024-01-01')",
			"CREATE VIEW vip AS SELECT * FROM users WHERE email = 'ana.petrovic@posta.example'",
			"CREATE TRIGGER trg BEFORE INSERT ON users FOR EACH ROW SET NEW.note = IF(NEW.note = '', 'svetlana.savic@firma.example', NEW.note)",
		}
	}
	return []string{
		"CREATE TABLE users (id int PRIMARY KEY, email varchar(100), note text DEFAULT 'ask milica.mikic@firma.example', phone text CHECK (phone <> '+381 64 765 4321'))",
		"INSERT INTO users (id, email) VALUES (1, 'ana.petrovic@posta.example'), (2, 'pera@firma.example')",
		"CREATE VIEW vip AS SELECT * FROM users WHERE email = 'ana.petrovic@posta.example'",
		"CREATE FUNCTION fill() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.note := 'svetlana.savic@firma.example'; RETURN NEW; END $$",
		"CREATE TRIGGER trg BEFORE INSERT ON users FOR EACH ROW EXECUTE FUNCTION fill()",
	}
}

func TestMySQLSchemaLiterals(t *testing.T) {
	eachMySQL(t, func(t *testing.T, s *server) {
		testSchemaLiterals(t, s, []string{"table users: email", "trigger trg: email", "view vip: email"})
	})
}

func TestPostgresSchemaLiterals(t *testing.T) {
	testSchemaLiterals(t, postgresServer(t), []string{"constraint users.users_phone_check: phone", "function fill: email", "table users: email", "view vip: email"})
}

// testSchemaLiterals: anonymize stops at personal-looking literals in
// schema objects until classify.yaml confirms them; verify runs the
// detector over the schema of both outputs (confirmed kinds aside) and the
// leak check regardless: a confirmed view that names a user of the rows is
// still a leak.
func testSchemaLiterals(t *testing.T, s *server, want []string) {
	ctx := context.Background()
	const src, dst = "dgschema_src", "dgschema_dst"
	t.Cleanup(func() { s.drop(src, dst) })
	s.create(t, src)
	s.drop(dst)
	s.exec(t, src, schemaLiteralStatements(s.driver)...)
	cols := "{id: keep, email: email, note: free-text, created: keep}"
	if s.driver == Postgres {
		cols = "{id: keep, email: email, note: free-text, phone: universal}"
	}
	base := "version: 1\ntables:\n  users:\n    columns: " + cols + "\n"
	rules, err := ParseRules([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	target := s.db(dst)
	var dump bytes.Buffer
	_, err = Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey})
	var ce *CheckError
	if !errors.As(err, &ce) || strings.Join(ce.Schema, "|") != strings.Join(want, "|") || dump.Len() > 0 {
		t.Fatalf("anonymize: %v (dump %d bytes)", err, dump.Len())
	}
	for _, v := range []string{"jelena", "milica", "svetlana", "ana.petrovic"} {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("a value in the error: %v", err)
		}
	}

	cls, err := Classify(ctx, s.db(src), rules, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var proposed []string
	for _, f := range cls.Schema {
		proposed = append(proposed, f.Object+": "+strings.Join(f.Kinds, ", "))
	}
	if strings.Join(proposed, "|") != strings.Join(want, "|") {
		t.Fatalf("classify proposes %q", proposed)
	}

	// Confirmed: the run goes through; the detector accepts what is
	// confirmed, the leak check still finds the user the view names.
	confirm := "schema:\n"
	for _, w := range want {
		obj, kinds, _ := strings.Cut(w, ": ")
		confirm += fmt.Sprintf("  %q: [%s]\n", obj, kinds)
	}
	confirmed, err := ParseRules([]byte(base + confirm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: confirmed, Key: testKey}); err != nil {
		t.Fatal(err)
	}
	findings := func(r *Rules) []string {
		t.Helper()
		rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Dump: bytes.NewReader(dump.Bytes()), Rules: r, Key: testKey})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range rep.Findings {
			if f.Output != "dump" {
				f.Output = "database"
			}
			out = append(out, f.Check+" "+f.Output+" "+f.Table+" "+f.Column+" "+f.Kind+" "+f.Origin)
		}
		sort.Strings(out)
		return out
	}
	// The view names a user; the default is in the rows that took it.
	leak := []string{"leak database (schema) table users email users.note", "leak database (schema) view vip email users.email",
		"leak dump (schema) table users email users.note", "leak dump (schema) view vip email users.email"}
	if got := findings(confirmed); strings.Join(got, "|") != strings.Join(leak, "|") {
		t.Errorf("confirmed: %q", got)
	}
	// Without the confirmations verify finds every one of them, in both outputs.
	got := findings(rules)
	for _, w := range want {
		obj, kinds, _ := strings.Cut(w, ": ")
		for _, out := range []string{"database", "dump"} {
			k := "detector " + out + " (schema) " + obj + " " + kinds + " "
			if !slicesContains(got, k) {
				t.Errorf("not found: %s in %q", k, got)
			}
		}
	}
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestPostgresOtherTypes: arrays of text, XML and tsvector are sampled by
// classify and scanned by verify; their proposals replace the personal
// parts (an array element by element, XML as HTML) or null the column.
func TestPostgresOtherTypes(t *testing.T) {
	s := postgresServer(t)
	ctx := context.Background()
	const src, dst = "dgother_src", "dgother_dst"
	t.Cleanup(func() { s.drop(src, dst) })
	s.create(t, src)
	s.drop(dst)
	s.exec(t, src,
		"CREATE TABLE contacts (id int PRIMARY KEY, cc text[], mails varchar(100)[] NOT NULL, card xml, search tsvector)",
		"INSERT INTO contacts VALUES (1, ARRAY['marko.markovic@firma.example','+381 64 123 4567', 'bez podataka'], ARRAY['jovan@x.rs', 'Ana \"A\" <ana@y.rs>'], "+
			"'<v kind=\"x\"><email>jelena.jovanovic@firma.example</email><jmbg>0101990710008</jmbg></v>', to_tsvector('simple', 'Dragana Dragić dragana@firma.example'))",
		"INSERT INTO contacts VALUES (2, '{}', '{}', NULL, NULL)",
	)
	empty, _ := ParseRules([]byte("version: 1\n"))
	cls, err := Classify(ctx, s.db(src), empty, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cls.Tables[0].Columns {
		got[c.Column] = c.Rule.Kind + "/" + c.Rule.Strategy
	}
	want := map[string]string{"id": "keep/", "cc": "personal/free-text", "mails": "personal/free-text", "card": "personal/html", "search": "personal/null"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("proposals %v, want %v", got, want)
	}
	run := func(cols string) []VerifyFinding {
		t.Helper()
		rules, err := ParseRules([]byte("version: 1\ntables:\n  contacts:\n    columns: " + cols + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		target := s.db(dst)
		var dump bytes.Buffer
		if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: true, Dump: &dump, Rules: rules, Key: testKey}); err != nil {
			t.Fatal(err)
		}
		rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Dump: bytes.NewReader(dump.Bytes()), Rules: rules, Key: testKey})
		if err != nil {
			t.Fatal(err)
		}
		return rep.Findings
	}
	if fs := run("{id: keep, cc: free-text, mails: email, card: html, search: empty}"); len(fs) != 0 {
		t.Fatalf("%+v", fs)
	}
	if v := s.query(t, dst, "SELECT cardinality(mails)::text || ' ' || (mails[2] LIKE '%.invalid')::text FROM contacts WHERE id = 1"); v != "2 true" {
		t.Errorf("mails: %s", v)
	}
	if v := s.query(t, dst, "SELECT (search = ''::tsvector)::text FROM contacts WHERE id = 1"); v != "true" {
		t.Errorf("search: %s", v)
	}
	kinds := map[string]bool{}
	for _, f := range run("{id: keep, cc: keep, mails: keep, card: keep, search: keep}") {
		kinds[f.Check+" "+f.Column+" "+f.Kind] = true
	}
	for _, k := range []string{"detector cc email", "detector cc phone", "detector mails email", "detector card email", "detector card jmbg", "detector search email"} {
		if !kinds[k] {
			t.Errorf("not found: %s in %v", k, kinds)
		}
	}
}

// TestPostgresNondeterministicCollation: a collation that is not bytewise
// is refused for a replaced column.
func TestPostgresNondeterministicCollation(t *testing.T) {
	s := postgresServer(t)
	const src = "dgndcoll_src"
	t.Cleanup(func() { s.drop(src) })
	s.create(t, src)
	s.exec(t, src,
		"CREATE COLLATION ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)",
		"CREATE TABLE users (id int PRIMARY KEY, nick text COLLATE ci)",
	)
	rules, _ := ParseRules([]byte("version: 1\ntables:\n  users:\n    columns: {id: keep, nick: universal}\n"))
	_, err := Anonymize(context.Background(), Options{Source: s.db(src), Dump: &bytes.Buffer{}, Rules: rules, Key: testKey})
	var ce *CheckError
	if !errors.As(err, &ce) || len(ce.Invalid) != 1 || !strings.Contains(ce.Invalid[0], "nondeterministic") {
		t.Fatalf("%v", err)
	}
}

func TestMySQLStandInIsNoOtherOriginal(t *testing.T) {
	eachMySQL(t, testStandInIsNoOtherOriginal)
}

func TestPostgresStandInIsNoOtherOriginal(t *testing.T) {
	testStandInIsNoOtherOriginal(t, postgresServer(t))
}

// testStandInIsNoOtherOriginal: phone numbers where one is the stand-in
// another would get: the output holds no original, verify finds nothing,
// and the dump is the same every time.
func testStandInIsNoOtherOriginal(t *testing.T, s *server) {
	ctx := context.Background()
	const src = "dgtaken_src"
	t.Cleanup(func() { s.drop(src) })
	r := newTestReplacer(t, testKey)
	a := "064 111 2222"
	b, err := r.Universal(a)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.Number("381641112222")
	if err != nil {
		t.Fatal(err)
	}
	s.create(t, src)
	s.exec(t, src,
		"CREATE TABLE users (id int PRIMARY KEY, phone varchar(20), mobile bigint)",
		fmt.Sprintf("INSERT INTO users VALUES (1, %s, 381641112222), (2, %s, %s)", lit(a), lit(b), n),
	)
	rules, _ := ParseRules([]byte("version: 1\ntables:\n  users:\n    columns: {id: keep, phone: universal, mobile: universal}\n"))
	dump := func() string {
		var d bytes.Buffer
		if _, err := Anonymize(ctx, Options{Source: s.db(src), Dump: &d, Rules: rules, Key: testKey}); err != nil {
			t.Fatal(err)
		}
		return d.String()
	}
	d := dump()
	for _, orig := range []string{"'" + a + "'", "'" + b + "'", "381641112222", n} {
		if strings.Contains(d, orig) {
			t.Errorf("the original %s is in the output", orig)
		}
	}
	if dump() != d {
		t.Error("dumps differ")
	}
	rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Dump: strings.NewReader(d), Rules: rules, Key: testKey})
	if err != nil || !rep.Clean() {
		t.Fatalf("%v %+v", err, rep)
	}
}
