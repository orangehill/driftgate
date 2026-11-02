package cli

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestDataVerifyCommand: data anonymize makes its output only when data
// verify finds nothing in it; an e-mail address planted in a column that is
// kept stops it with exit 1, leaves the previous target as it was (the run
// wrote a staging database, which is dropped) and leaves no dump. Runs on a
// MySQL/MariaDB server from DRIFTGATE_TEST_MYSQL.
func TestDataVerifyCommand(t *testing.T) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mysql.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net, cfg.Addr = "tcp", u.Host
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(conn)
	admin.SetMaxOpenConns(1)
	defer admin.Close()
	exec := func(stmts ...string) {
		t.Helper()
		for _, s := range stmts {
			if _, err := admin.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	exec("DROP DATABASE IF EXISTS dgt_verify_src", "DROP DATABASE IF EXISTS dgt_verify_dst", "CREATE DATABASE dgt_verify_src CHARACTER SET utf8mb4",
		"CREATE TABLE dgt_verify_src.users (id INT PRIMARY KEY, name VARCHAR(100) NOT NULL, email VARCHAR(100) NOT NULL)",
		"INSERT INTO dgt_verify_src.users VALUES (1, 'Dragoljub Stefanović', 'dragoljub@firma.example'), (2, 'Milica Jovanović', 'milica@firma.example')",
		"CREATE TABLE dgt_verify_src.news (id INT PRIMARY KEY, title VARCHAR(200) NOT NULL)",
		"INSERT INTO dgt_verify_src.news VALUES (1, 'Otvoren novi pogon'), (2, 'Rezultati za 2025. godinu')")
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS dgt_verify_src")
		admin.Exec("DROP DATABASE IF EXISTS dgt_verify_dst")
	})
	count := func(q string) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	root := t.TempDir()
	t.Setenv("DG_DATA_DSN", dsn)
	t.Setenv("DRIFTGATE_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	config := filepath.Join(root, "driftgate.yaml")
	os.WriteFile(config, []byte(`version: 1
project: verifytest
target: { base_url: "http://localhost:1" }
data:
  driver: mysql
  source: { dsn: { env: DG_DATA_DSN }, database: dgt_verify_src }
  target: { database: dgt_verify_dst }
`), 0o644)
	os.MkdirAll(filepath.Join(root, "driftgate"), 0o755)
	os.WriteFile(filepath.Join(root, "driftgate", "classify.yaml"), []byte(`version: 1
tables:
  users:
    columns: { id: keep, name: universal, email: email }
  news:
    columns: { id: keep, title: keep }
`), 0o644)
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(append(args, "--config", config), &out, &errOut)
		return code, out.String() + errOut.String()
	}

	dump := filepath.Join(root, "out", "anon.sql")
	if code, out := run("data", "anonymize", "--dump", dump); code != ExitClean || !strings.Contains(out, "verified: no personal data found") {
		t.Fatalf("anonymize: %d\n%s", code, out)
	}
	if code, out := run("data", "verify"); code != ExitClean || !strings.Contains(out, "verified") {
		t.Fatalf("verify the target: %d\n%s", code, out)
	}
	if code, out := run("data", "verify", "--dump", dump); code != ExitClean {
		t.Fatalf("verify the dump: %d\n%s", code, out)
	}
	// The source itself is full of originals.
	if code, out := run("data", "verify", "--target", "dgt_verify_src"); code != ExitDrift || !strings.Contains(out, "leak     users.email: email from users.email in 2 values (rows id=1 id=2)") {
		t.Fatalf("verify the source: %d\n%s", code, out)
	}

	// A user's e-mail address and name in a title that is kept.
	exec("UPDATE dgt_verify_src.news SET title = 'Write to dragoljub@firma.example (Milica Jovanović)' WHERE id = 2")
	code, out := run("data", "anonymize", "--replace", "--dump", dump+"2")
	if code != ExitDrift || !strings.Contains(out, "the output is not made") {
		t.Fatalf("anonymize with a planted address: %d\n%s", code, out)
	}
	for _, want := range []string{
		"detector news.title: email in 1 value (rows id=2) [database dgt_verify_dst]",
		"leak     news.title: email from users.email in 1 value (rows id=2) [dump]",
		"leak     news.title: universal from users.name in 1 value (rows id=2) [dump]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "dragoljub") || strings.Contains(out, "Jovanovi") {
		t.Fatalf("the report carries a value:\n%s", out)
	}
	if n := count("SELECT COUNT(*) FROM dgt_verify_dst.news WHERE title LIKE '%@%'"); n != 0 {
		t.Errorf("the target holds the output that failed verification (%d rows)", n)
	}
	if n := count("SELECT COUNT(*) FROM dgt_verify_dst.news"); n != 2 {
		t.Errorf("the previous target is gone: %d rows", n)
	}
	if n := count("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'dgt\\_verify\\_dst\\_\\_driftgate\\_tmp\\_%'"); n != 0 {
		t.Errorf("%d staging databases left", n)
	}
	entries, _ := os.ReadDir(filepath.Dir(dump))
	for _, e := range entries {
		if e.Name() != "anon.sql" {
			t.Errorf("left in the dump folder: %s", e.Name())
		}
	}
	// The same with JSON output.
	if code, out := run("data", "anonymize", "--no-target", "--dump", dump+"3", "--format", "json"); code != ExitDrift || !strings.Contains(out, `"check": "leak"`) {
		t.Fatalf("json: %d\n%s", code, out)
	}
}
