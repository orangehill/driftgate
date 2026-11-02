package data

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMySQLVerify(t *testing.T) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testVerify(t, newServer(t, MySQL, dsn)) })
	}
}

func TestPostgresVerify(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	testVerify(t, newServer(t, Postgres, dsn))
}

// testVerify: a clean anonymization passes both checks on the database and
// the dump; personal data the rules let through (an e-mail address in a
// column that is kept, in a kept JSON path, a name in the text of another
// table) is found, by place and never by value.
func testVerify(t *testing.T, s *server) {
	ctx := context.Background()
	const src, dst = "dgverify_src", "dgverify_dst"
	t.Cleanup(func() { s.drop(src, dst) })
	mariadb := false
	if s.driver == MySQL {
		var version string
		s.admin.QueryRow("SELECT VERSION()").Scan(&version)
		mariadb = strings.Contains(strings.ToLower(version), "mariadb")
	}
	ps := people(testUsers())
	s.create(t, src)
	s.drop(dst)
	s.exec(t, src, seedStatements(s.driver, ps, mariadb)...)

	// The settings keep $.public as it is.
	rulesText := strings.Replace(testRules, `"$.phone": universal`, `"$.phone": universal
          "$.public": keep`, 1)
	if s.driver == Postgres {
		rulesText += testRulesPostgres[len(testRules):]
	}
	rules, err := ParseRules([]byte(rulesText))
	if err != nil {
		t.Fatal(err)
	}
	target := s.db(dst)
	run := func() *VerifyReport {
		t.Helper()
		var dump bytes.Buffer
		start := time.Now()
		index := &VerifyIndex{}
		rep, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: true, Dump: &dump, Rules: rules, Key: testKey, Index: index})
		if err != nil {
			t.Fatal(err)
		}
		anonymized := time.Since(start)
		start = time.Now()
		v, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Dump: bytes.NewReader(dump.Bytes()), Rules: rules, Key: testKey})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("anonymize: %d rows, dump %d bytes in %s; verify (database and dump): %d values, %d originals in %s",
			rep.Rows, dump.Len(), anonymized.Round(time.Millisecond), v.Values, v.Originals, time.Since(start).Round(time.Millisecond))
		// The index the copy made finds what a read of the source finds.
		vi, err := Verify(ctx, VerifyOptions{Source: s.db(src), Target: &target, Dump: bytes.NewReader(dump.Bytes()), Rules: rules, Key: testKey, Index: index})
		if err != nil {
			t.Fatal(err)
		}
		if vi.Originals != v.Originals || vi.Values != v.Values || fmt.Sprint(vi.Findings) != fmt.Sprint(v.Findings) {
			t.Fatalf("with the index of the copy: %d originals, %d values, %+v\nwith a read of the source: %d originals, %d values, %+v",
				vi.Originals, vi.Values, vi.Findings, v.Originals, v.Values, v.Findings)
		}
		return v
	}

	v := run()
	if !v.Clean() {
		t.Fatalf("a clean anonymization fails verification: %+v", v.Findings)
	}
	if v.Originals < int64(len(ps))*3 {
		t.Fatalf("only %d originals", v.Originals)
	}

	// Plant personal data the rules let through.
	p1, p2, p3 := ps[1], ps[2], ps[7]
	s.exec(t, src,
		// An e-mail address of a user in a column that is kept.
		fmt.Sprintf("UPDATE posts SET title = %s WHERE id = 2", lit("Write to the author at "+p1.email)),
		// An e-mail address under a kept JSON path.
		fmt.Sprintf("UPDATE users SET settings = %s WHERE id = 3", lit(`{"theme": "dark", "public": {"contact": "`+p2.email+`"}}`)),
		// A name in the text of another table: anonymize replaces names in
		// free text only within the same row.
		fmt.Sprintf("UPDATE posts SET body = %s WHERE id = 5", lit("<p>Intervju: "+p3.name+"</p>")),
		// A JMBG nobody classified, which only the detector can find.
		"UPDATE tags SET note = 'JMBG 0101990710008' WHERE name = 'sport'",
	)
	v = run()
	got := map[string]VerifyFinding{}
	var keys []string
	for _, f := range v.Findings {
		k := f.Check + " " + f.Output + " " + f.Table + "." + f.Column + " " + f.Kind
		got[k] = f
		keys = append(keys, k)
		for _, r := range f.Rows {
			if strings.ContainsAny(r, "@<") || strings.Contains(strings.ToLower(r), "jmbg") {
				t.Errorf("a value in the rows of %s: %q", k, r)
			}
		}
	}
	sort.Strings(keys)
	want := map[string]string{} // finding → its first row
	for _, out := range []string{"database " + dst, "dump"} {
		want["detector "+out+" posts.title email"] = "id=2"
		want["leak "+out+" posts.title email"] = "id=2"
		want["detector "+out+" users.settings email"] = "id=3"
		want["leak "+out+" users.settings email"] = "id=3"
		want["leak "+out+" posts.body universal"] = "id=5"
		want["detector "+out+" tags.note jmbg"] = "#1" // no primary key: the position in key order
	}
	// The server computes the generated column from the kept title.
	want["detector database "+dst+" posts.title_lower email"] = "id=2"
	want["leak database "+dst+" posts.title_lower email"] = "id=2"
	for k, row := range want {
		f, ok := got[k]
		if !ok {
			t.Errorf("not found: %s", k)
			continue
		}
		if len(f.Rows) == 0 || f.Rows[0] != row || f.Values != 1 {
			t.Errorf("%s: %d values, rows %v, want %s", k, f.Values, f.Rows, row)
		}
		if f.Check == CheckLeak && !strings.HasSuffix(f.Origin, "email") && !strings.HasSuffix(f.Origin, ".name") {
			t.Errorf("%s: origin %s", k, f.Origin)
		}
	}
	if len(got) != len(want) {
		t.Errorf("findings:\n%s", strings.Join(keys, "\n"))
	}
}
