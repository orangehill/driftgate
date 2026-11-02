package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/data"
)

// TestDataCommands runs data classify and data anonymize end to end on a
// MySQL/MariaDB server from DRIFTGATE_TEST_MYSQL, with their exit codes.
func TestDataCommands(t *testing.T) {
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
	exec("DROP DATABASE IF EXISTS dgt_data_src", "DROP DATABASE IF EXISTS dgt_data_dst", "CREATE DATABASE dgt_data_src",
		"CREATE TABLE dgt_data_src.users (id INT PRIMARY KEY, email VARCHAR(100) NOT NULL, bio TEXT NULL, created_at DATETIME NULL)",
		"INSERT INTO dgt_data_src.users VALUES (1, 'pera@firma.example', 'Write to me at pera@firma.example, warm regards from me and the whole family in Belgrade.', NULL), (2, 'mika@firma.example', NULL, NULL)",
		"CREATE TABLE dgt_data_src.sessions (id VARCHAR(40) PRIMARY KEY, payload TEXT NOT NULL)")
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS dgt_data_src")
		admin.Exec("DROP DATABASE IF EXISTS dgt_data_dst")
	})

	root := t.TempDir()
	t.Setenv("DG_DATA_DSN", dsn)
	t.Setenv("DRIFTGATE_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	config := filepath.Join(root, "driftgate.yaml")
	os.WriteFile(config, []byte(`version: 1
project: datatest
target: { base_url: "http://localhost:1" }
data:
  driver: mysql
  source: { dsn: { env: DG_DATA_DSN }, database: dgt_data_src }
  target: { database: dgt_data_dst }
`), 0o644)
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(append(args, "--config", config), &out, &errOut)
		return code, out.String() + errOut.String()
	}

	if code, out := run("data", "classify"); code != ExitDrift || !strings.Contains(out, "+ users.email: personal/email") || !strings.Contains(out, "+ sessions: action empty") {
		t.Fatalf("classify: %d\n%s", code, out)
	}
	if code, out := run("data", "classify", "--write"); code != ExitClean {
		t.Fatalf("classify --write: %d\n%s", code, out)
	}
	rulesFile := filepath.Join(root, "driftgate", "classify.yaml")
	raw, _ := os.ReadFile(rulesFile)
	if strings.Contains(string(raw), "pera") {
		t.Fatalf("a sampled value in classify.yaml:\n%s", raw)
	}
	// Proposals are not rules yet.
	if code, out := run("data", "anonymize"); code != ExitConfig || !strings.Contains(out, "not confirmed") {
		t.Fatalf("anonymize with proposals: %d\n%s", code, out)
	}
	rules, err := data.ParseRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rules.Tables {
		tr.Proposed = false
		for _, c := range tr.Columns {
			c.Proposed = false
		}
	}
	confirmed, _ := rules.Marshal()
	os.WriteFile(rulesFile, confirmed, 0o644)

	dump := filepath.Join(root, "out", "anon.sql")
	code, out := run("data", "anonymize", "--dump", dump)
	if code != ExitClean || !strings.Contains(out, "2 rows") {
		t.Fatalf("anonymize: %d\n%s", code, out)
	}
	if strings.Contains(out, "pera") {
		t.Fatalf("the report carries a value:\n%s", out)
	}
	b, err := os.ReadFile(dump)
	if err != nil || strings.Contains(string(b), "pera@firma.example") || !strings.Contains(string(b), ".invalid") {
		t.Fatalf("dump: %v\n%s", err, b)
	}
	// The target exists now: only --replace writes it again.
	if code, out := run("data", "anonymize", "--no-target", "--dump", dump+"2"); code != ExitClean {
		t.Fatalf("dump only: %d\n%s", code, out)
	}
	if code, out := run("data", "anonymize"); code != ExitConfig || !strings.Contains(out, "--replace") {
		t.Fatalf("existing target: %d\n%s", code, out)
	}
	b2, _ := os.ReadFile(dump + "2")
	if !bytes.Equal(b, b2) {
		t.Fatal("dumps differ")
	}
	// data subset needs a subset: section, and writes a sample.
	if code, out := run("data", "subset", "--replace"); code != ExitConfig || !strings.Contains(out, "no subset: section") {
		t.Fatalf("subset without a section: %d\n%s", code, out)
	}
	os.WriteFile(rulesFile, append(confirmed, "subset:\n  tables:\n    users: { rows: 1 }\n"...), 0o644)
	if code, out := run("data", "subset", "--replace"); code != ExitClean || !strings.Contains(out, "1 rows of 2") || strings.Contains(out, "pera") {
		t.Fatalf("subset: %d\n%s", code, out)
	}
	if code, out := run("data", "subset", "--no-target", "--dump", dump+"3", "--format", "json"); code != ExitClean || !strings.Contains(out, `"total": 2`) {
		t.Fatalf("subset json: %d\n%s", code, out)
	}
	// A new column fails with its name.
	exec("ALTER TABLE dgt_data_src.users ADD COLUMN phone VARCHAR(20) NULL")
	if code, out := run("data", "anonymize", "--replace"); code != ExitConfig || !strings.Contains(out, "users.phone") {
		t.Fatalf("unclassified: %d\n%s", code, out)
	}
}

// TestDataTargetHosts: the target is checked against allow_hosts at every
// host the driver would connect to, also a host= parameter and the second
// of several hosts; nothing is connected to before that.
func TestDataTargetHosts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PGHOST", "")
	t.Setenv("DG_DATA_SRC", "postgres://u:p@127.0.0.1:1/postgres?sslmode=disable")
	t.Setenv("DRIFTGATE_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	config := filepath.Join(root, "driftgate.yaml")
	for _, dsn := range []string{
		"postgres://u:p@/postgres?host=203.0.113.9&sslmode=disable",
		"postgres://u:p@127.0.0.1:5432,203.0.113.9:5432/postgres?sslmode=disable",
		"postgres://u:p@/postgres?host=/tmp,203.0.113.9&sslmode=disable",
	} {
		t.Setenv("DG_DATA_DST", dsn)
		os.WriteFile(config, []byte(`version: 1
project: hosttest
target: { base_url: "http://localhost:1" }
data:
  driver: postgres
  source: { dsn: { env: DG_DATA_SRC }, database: app }
  target: { dsn: { env: DG_DATA_DST }, database: app_copy }
`), 0o644)
		var out bytes.Buffer
		code := Main([]string{"data", "anonymize", "--config", config}, &out, &out)
		if code != ExitConfig || !strings.Contains(out.String(), "database host 203.0.113.9 is not allowed") {
			t.Errorf("%s: %d\n%s", dsn, code, out.String())
		}
	}
}

// TestDataTargetLocalOnly: without allow_hosts the target, which is dropped
// and written again, may only be on this machine, as a database under
// state: (not on a private address, which may well be production).
func TestDataTargetLocalOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		allow string
		host  string
		ok    bool
	}{
		{"", "", true}, // a local socket
		{"", "localhost", true},
		{"", "127.0.0.1", true},
		{"", "::1", true},
		{"", "10.1.2.3", false},
		{"", "192.168.1.5", false},
		{`["10.0.0.0/8"]`, "10.1.2.3", true},
		{`["10.0.0.0/8"]`, "127.0.0.1", false},
	} {
		root := t.TempDir()
		cfg := filepath.Join(root, "driftgate.yaml")
		target := `{ base_url: "http://localhost:1" }`
		if tc.allow != "" {
			target = `{ base_url: "http://localhost:1", allow_hosts: ` + tc.allow + ` }`
		}
		write(t, cfg, "version: 1\nproject: hosttest\ntarget: "+target+"\n")
		p, err := config.Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := allowHost(ctx, p, tc.host); (err == nil) != tc.ok {
			t.Errorf("allow_hosts %q, host %q: %v", tc.allow, tc.host, err)
		}
	}
}
