package data

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// The subset runs on the seed of the anonymize test, with comments (a
// self-reference and a nullable reference) and likes (no foreign keys: a
// declared reference and a polymorphic one).

func subsetSeed(driver string, ps []person) []string {
	st := []string{
		"CREATE TABLE comments (id INT PRIMARY KEY, post_id INT NOT NULL, parent_id INT NULL, user_id INT NULL, body TEXT NOT NULL, " +
			"CONSTRAINT comments_post FOREIGN KEY (post_id) REFERENCES posts(id), CONSTRAINT comments_parent FOREIGN KEY (parent_id) REFERENCES comments(id), " +
			"CONSTRAINT comments_user FOREIGN KEY (user_id) REFERENCES users(id))",
		"CREATE TABLE likes (id INT PRIMARY KEY, likeable_type VARCHAR(50) NOT NULL, likeable_id INT NOT NULL, user_id INT NOT NULL)",
	}
	n := len(ps)
	var comments, likes []string
	for i := 1; i <= n; i++ {
		// far is a user no kept row needs: the nullable reference to it is nulled.
		p, other, far := ps[i-1], ps[i%n], ps[(i+n/2)%n]
		body := lit(fmt.Sprintf("Odgovor: javite se na %s ili %s", far.email, far.phone))
		prev := "NULL"
		if i > 1 {
			prev = fmt.Sprint(3*(i-1) - 2) // a comment on the previous post
		}
		comments = append(comments,
			fmt.Sprintf("(%d, %d, NULL, %d, %s)", 3*i-2, i, p.id, body),
			fmt.Sprintf("(%d, %d, %d, %d, %s)", 3*i-1, i, 3*i-2, far.id, body),
			fmt.Sprintf("(%d, %d, %s, NULL, %s)", 3*i, i, prev, body))
		likes = append(likes,
			fmt.Sprintf("(%d, 'post', %d, %d)", 2*i-1, i, p.id),
			fmt.Sprintf("(%d, 'comment', %d, %d)", 2*i, 3*i-1, other.id))
	}
	st = append(st, "INSERT INTO comments VALUES "+strings.Join(comments, ", "), "INSERT INTO likes VALUES "+strings.Join(likes, ", "))
	return st
}

const subsetRules = `  comments:
    columns: { id: keep, post_id: keep, parent_id: keep, user_id: keep, body: free-text }
  likes:
    columns: { id: keep, likeable_type: keep, likeable_id: keep, user_id: keep }
subset:
  tables:
    posts: { rows: 10 }
    comments: { follow: [posts], per_parent: 2 }
    likes: { follow: [posts, comments] }
    post_log: { follow: [posts] }
    tags: all
  references:
    - { from: "post_log(post_id)", to: "posts(id)" }
    - { from: "likes(user_id)", to: "users(id)" }
    - { from: "likes(likeable_id)", to: "posts(id)", when: { likeable_type: post } }
    - { from: "likes(likeable_id)", to: "comments(id)", when: { likeable_type: comment } }
    - { from: "comments(user_id)", nullable: null }
`

func TestMySQLSubset(t *testing.T) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testSubset(t, newServer(t, MySQL, dsn)) })
	}
}

func TestPostgresSubset(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	testSubset(t, newServer(t, Postgres, dsn))
}

func testSubset(t *testing.T, s *server) {
	ctx := context.Background()
	const src, dst, loaded = "dgsub_src", "dgsub_dst", "dgsub_loaded"
	t.Cleanup(func() { s.drop(src, dst, loaded) })
	mariadb := false
	if s.driver == MySQL {
		var version string
		s.admin.QueryRow("SELECT VERSION()").Scan(&version)
		mariadb = strings.Contains(strings.ToLower(version), "mariadb")
	}
	ps := people(testUsers())
	s.create(t, src)
	s.drop(dst, loaded)
	s.exec(t, src, append(seedStatements(s.driver, ps, invoiceSequence(s, mariadb)), subsetSeed(s.driver, ps)...)...)

	rulesText := testRules + subsetRules
	if s.driver == Postgres {
		rulesText = testRulesPostgres + strings.Replace(subsetRules, "    tags: all\n", "    tags: all\n    billing.invoices: { rows: 1, order: first }\n", 1)
	}
	rules, err := ParseRules([]byte(rulesText))
	if err != nil {
		t.Fatal(err)
	}
	target := s.db(dst)
	run := func(target *Database) ([]byte, *Report) {
		t.Helper()
		var dump bytes.Buffer
		rep, err := Anonymize(ctx, Options{Source: s.db(src), Target: target, Replace: true, Dump: &dump, Rules: rules, Key: testKey, Filter: &Subset{}, BatchRows: 7})
		if err != nil {
			t.Fatal(err)
		}
		return dump.Bytes(), rep
	}
	dump1, rep := run(&target)
	dump2, _ := run(nil)
	if !bytes.Equal(dump1, dump2) {
		t.Fatalf("dumps differ:\n%s", firstDifference(dump1, dump2))
	}
	byName := map[string]TableReport{}
	for _, tr := range rep.Tables {
		byName[tr.Name] = tr
		if tr.Sample != nil {
			t.Logf("%-18s %3d of %3d rows (roots %d, followed %d, parents %d, nulled %d)", tr.Name, tr.Rows, tr.Sample.Total, tr.Sample.Roots, tr.Sample.Followed, tr.Sample.Parents, tr.Sample.Nulled)
		}
	}
	if p := byName["posts"]; p.Sample == nil || p.Sample.Roots != 10 || p.Sample.Parents != 1 || p.Rows != 11 || p.Sample.Total != int64(len(ps)) {
		t.Errorf("posts: %+v %+v", p, p.Sample)
	}
	if c := byName["comments"]; c.Sample.Followed != 20 || c.Sample.Parents != 11 || c.Sample.Nulled != 10 {
		t.Errorf("comments: %+v", c.Sample)
	}
	if u := byName["users"]; u.Rows == 0 || u.Rows >= int64(len(ps)) || u.Sample.Parents != u.Rows {
		t.Errorf("users: %d rows, %+v", u.Rows, u.Sample)
	}

	tdb, err := open(target, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer tdb.Close()
	count := func(db *sql.DB, q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// Every reference resolves, declared ones too.
	dangling := map[string]string{
		"posts.user_id":      "SELECT COUNT(*) FROM posts c WHERE NOT EXISTS (SELECT 1 FROM users p WHERE p.id = c.user_id)",
		"posts.author_email": "SELECT COUNT(*) FROM posts c WHERE NOT EXISTS (SELECT 1 FROM users p WHERE p.email = c.author_email)",
		"comments.post_id":   "SELECT COUNT(*) FROM comments c WHERE NOT EXISTS (SELECT 1 FROM posts p WHERE p.id = c.post_id)",
		"comments.parent_id": "SELECT COUNT(*) FROM comments c WHERE c.parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM comments p WHERE p.id = c.parent_id)",
		"comments.user_id":   "SELECT COUNT(*) FROM comments c WHERE c.user_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM users p WHERE p.id = c.user_id)",
		"post_log.post_id":   "SELECT COUNT(*) FROM post_log c WHERE NOT EXISTS (SELECT 1 FROM posts p WHERE p.id = c.post_id)",
		"likes.user_id":      "SELECT COUNT(*) FROM likes c WHERE NOT EXISTS (SELECT 1 FROM users p WHERE p.id = c.user_id)",
		"likes post":         "SELECT COUNT(*) FROM likes c WHERE c.likeable_type = 'post' AND NOT EXISTS (SELECT 1 FROM posts p WHERE p.id = c.likeable_id)",
		"likes comment":      "SELECT COUNT(*) FROM likes c WHERE c.likeable_type = 'comment' AND NOT EXISTS (SELECT 1 FROM comments p WHERE p.id = c.likeable_id)",
	}
	if s.driver == Postgres {
		dangling["invoices.user_id"] = "SELECT COUNT(*) FROM billing.invoices c WHERE c.user_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM users p WHERE p.id = c.user_id)"
	}
	for name, q := range dangling {
		if n := count(tdb, q); n != 0 {
			t.Errorf("%s: %d dangling references", name, n)
		}
	}
	for q, want := range map[string]int{
		"SELECT COUNT(*) FROM tags":                                  3,
		"SELECT COUNT(*) FROM cache":                                 0,
		"SELECT COUNT(*) FROM post_log":                              10,
		"SELECT COUNT(*) FROM likes WHERE likeable_type = 'post'":    10,
		"SELECT COUNT(*) FROM likes WHERE likeable_type = 'comment'": 10,
	} {
		if n := count(tdb, q); n != want {
			t.Errorf("%s: %d, want %d", q, n, want)
		}
	}
	if count(tdb, "SELECT COUNT(*) FROM user_post_count") == 0 {
		t.Error("the views see no rows")
	}
	if s.driver == Postgres && count(tdb, "SELECT COUNT(*) FROM billing.invoices") != 1 {
		t.Error("invoices")
	}

	// The dump loads into an empty database and gives the same data.
	s.create(t, loaded)
	s.exec(t, loaded, splitDump(t, s.driver, string(dump1))...)
	if a, b := contentOf(t, target), contentOf(t, s.db(loaded)); a != b {
		t.Errorf("the loaded dump differs from the target:\n%s", firstDifference([]byte(a), []byte(b)))
	}

	// No original personal value in the target or the dump.
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
	tgtText := strings.ToLower(contentOf(t, target))
	dumpText := strings.ToLower(string(dump1))
	for _, n := range needles {
		if strings.Contains(tgtText, n) {
			t.Errorf("original value (%d characters) in the target", utf8.RuneCountInString(n))
		}
		if strings.Contains(dumpText, n) {
			t.Errorf("original value (%d characters) in the dump", utf8.RuneCountInString(n))
		}
	}
}

// TestSubsetNulledNeedle: a personal value the subset nulls (a reference
// outside the sample, nullable: null) is still the row's own, so it is
// replaced in the row's free text: the subset passes verify and holds no
// original.
func TestSubsetNulledNeedle(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src, dst = "dgsubnull_src", "dgsubnull_dst"
		t.Cleanup(func() { s.drop(src, dst) })
		s.create(t, src)
		s.drop(dst)
		var uq, mentions []string
		for i := 1; i <= 20; i++ {
			nick := fmt.Sprintf("Zvezdana%dQ", i)
			uq = append(uq, fmt.Sprintf("(%d, %s)", i, lit(nick)))
			mentions = append(mentions, fmt.Sprintf("(%d, %s, %s)", i, lit(nick), lit("pozdrav "+nick+" od svih")))
		}
		s.exec(t, src,
			"CREATE TABLE uq (id INT PRIMARY KEY, nick VARCHAR(50) NOT NULL UNIQUE)",
			"CREATE TABLE mentions (id INT PRIMARY KEY, nick VARCHAR(50) NULL, body TEXT NOT NULL)",
			"INSERT INTO uq VALUES "+strings.Join(uq, ", "),
			"INSERT INTO mentions VALUES "+strings.Join(mentions, ", "))
		rules, err := ParseRules([]byte(`version: 1
tables:
  uq:
    columns: { id: keep, nick: universal }
  mentions:
    columns: { id: keep, nick: universal, body: free-text }
subset:
  tables:
    uq: { rows: 3 }
    mentions: all
  references:
    - { from: "mentions(nick)", to: "uq(nick)", nullable: null }
`))
		if err != nil {
			t.Fatal(err)
		}
		target := s.db(dst)
		var dump bytes.Buffer
		rep, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey, Filter: &Subset{}})
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range rep.Tables {
			if tr.Name == "mentions" && (tr.Sample == nil || tr.Sample.Nulled != 17) {
				t.Fatalf("mentions: %+v, want 17 nulled", tr.Sample)
			}
		}
		vrep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Dump: bytes.NewReader(dump.Bytes()), Rules: rules, Key: testKey})
		if err != nil {
			t.Fatal(err)
		}
		if !vrep.Clean() {
			t.Fatalf("verify: %+v", vrep.Findings)
		}
		for _, text := range []string{contentOf(t, target), dump.String()} {
			if strings.Contains(strings.ToLower(text), "zvezdana") {
				t.Fatal("an original nick is left in the subset")
			}
		}
	})
}
