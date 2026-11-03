package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

// sqlServerTestDSN is the server of DRIFTGATE_TEST_SQLSERVER, a sqlserver://
// URL of a login that may create databases (sa in CI).
func sqlServerTestDSN(t testing.TB) string {
	v := os.Getenv("DRIFTGATE_TEST_SQLSERVER")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_SQLSERVER not set")
	}
	return v
}

// sqlServerTestDB opens a database of the test server ("" is master).
func sqlServerTestDB(t testing.TB, dsn, database string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if database == "" {
		database = "master"
	}
	q.Set("database", database)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sqlServerTestName is a database name of a test's own.
func sqlServerTestName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "dgt_ss_" + hex.EncodeToString(b)
}

// dropSQLServerDatabase drops a database of a test, if it exists.
func dropSQLServerDatabase(master *sql.DB, name string) {
	master.Exec("IF DB_ID(" + sqlServerString(name) + ") IS NOT NULL BEGIN ALTER DATABASE " + qms(name) +
		" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + qms(name) + " END")
}

// newSQLServerTest makes a database of its own for a test (dropped at its
// end, with <working>_p) and a component with it as the working database;
// the pristine database is <working>_p. create, when not empty, is the
// rest of CREATE DATABASE (e.g. COLLATE …).
func newSQLServerTest(t testing.TB, o Options, create ...string) (*SQLServer, *sql.DB) {
	t.Helper()
	dsn := sqlServerTestDSN(t)
	work := sqlServerTestName()
	master := sqlServerTestDB(t, dsn, "")
	if _, err := master.Exec("CREATE DATABASE " + qms(work) + " " + strings.Join(create, " ")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropSQLServerDatabase(master, work)
		dropSQLServerDatabase(master, work+"_p")
	})
	o.Name, o.DSN, o.Working, o.Pristine = "db", dsn, work, work+"_p"
	s, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, sqlServerTestDB(t, dsn, work)
}

// sqlServerFull fingerprints the working database the long way: a new
// component, nothing to reuse. Every fingerprint must equal it.
func sqlServerFull(t testing.TB, s *SQLServer) Fingerprint {
	t.Helper()
	o := s.o
	o.Log = nil
	fresh, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.rvMax, fresh.rvPristine = s.rvMax, s.rvPristine
	ctx := context.Background()
	c, err := fresh.conn(ctx, o.Working)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fp, err := fresh.fingerprint(ctx, c, o.Working, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fp.f
}

func mustSQLServerFingerprint(t testing.TB, s *SQLServer) Fingerprint {
	t.Helper()
	f, err := s.Fingerprint(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSQLServerConfig(t *testing.T) {
	const secret = "S3cr;et@{pw}"
	pw := url.QueryEscape(secret)
	for _, tc := range []struct {
		dsn, err string
	}{
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?encrypt=true&TrustServerCertificate=true", ""},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433", ""}, // encrypt=true is added
		{"SQLServer://sa:" + pw + "@db.example:14331/INST?Encrypt=false&trustservercertificate=true&app+name=x", ""},
		{"Server=127.0.0.1;User Id=sa;Password=" + secret, "must be a sqlserver:// URL"},
		{"odbc:server=127.0.0.1;user id=sa;password=" + secret, "must be a sqlserver:// URL"},
		{"sqlserver://127.0.0.1:1433?encrypt=true", "names no user"},
		{"sqlserver://" + url.QueryEscape(`CORP\me`) + ":" + pw + "@127.0.0.1:1433", "Windows login"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?encrypt=disable", "encrypt=disable"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?encrypt=bogus", "invalid encrypt"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?Integrated+Security=true", "SQL login"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?trusted_connection=yes", "SQL login"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?fedauth=ActiveDirectoryPassword", "SQL login"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?krb5-configfile=/etc/krb5.conf", "SQL login"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?ApplicationIntent=ReadOnly", "read-only replica"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?failoverpartner=x", "failover partner"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?columnencryption=true", "Always Encrypted"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?bogusParam=1", `"bogusParam"`},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?encrypt=true&Encrypt=disable", "twice"},
		{"sqlserver://sa:" + pw + "@127.0.0.1:1433?protocol=np", "only the tcp protocol"},
		{"sqlserver://sa:" + pw + "@127.0.0.1/SQLEXPRESS", "named instance needs its port"},
		{"sqlserver://sa:" + pw + "@np:server", "dsn: "}, // not a URL host
		{"sqlserver://sa:" + pw + "@(localdb)", "LocalDB"},
		{"sqlserver://sa:" + pw + "@:1433", "names no host"},
		{"sqlserver://sa:" + secret + "@127.0.0.1:1433", ""}, // unescaped: not a URL, or a wrong host
		{"sqlserver://sa:%zz" + pw + "@127.0.0.1:1433", "not a valid URL"},
	} {
		cfg, err := sqlServerConfig(tc.dsn)
		switch {
		case err != nil && strings.Contains(err.Error(), secret), err != nil && strings.Contains(err.Error(), pw):
			t.Errorf("%s: the error shows the password: %v", tc.dsn, err)
		case tc.err == "" && err != nil && tc.dsn != "sqlserver://sa:"+secret+"@127.0.0.1:1433":
			t.Errorf("%s: %v", tc.dsn, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: error %v, want %q", tc.dsn, err, tc.err)
		case err == nil && cfg.Encryption != 1 && !strings.Contains(strings.ToLower(tc.dsn), "encrypt=false"):
			t.Errorf("%s: encryption %d, want required", tc.dsn, cfg.Encryption)
		case err == nil && !strings.Contains(strings.ToLower(tc.dsn), "trustservercertificate=true") && cfg.TrustServerCertificate:
			t.Errorf("%s: the certificate is not verified", tc.dsn)
		}
	}
	if _, err := NewSQLServer(Options{Name: "db", DSN: "sqlserver://sa:x@127.0.0.1:1433", Working: "App", Pristine: "app"}); err == nil {
		t.Error("a pristine database named as the working one (but for case) is accepted")
	}
}

func TestSQLServerDialerRefusesUDP(t *testing.T) {
	d := sqlServerDialer{host: "127.0.0.1"}
	if _, err := d.DialContext(context.Background(), "udp", "127.0.0.1:1434"); err == nil {
		t.Fatal("dialled the SQL Server Browser over UDP")
	}
	if d.HostName() != "127.0.0.1" {
		t.Fatal("HostName")
	}
}

func TestSQLServerQuote(t *testing.T) {
	if got := QuoteSQLServer("a]b[c"); got != "[a]]b[c]" {
		t.Fatalf("QuoteSQLServer: %s", got)
	}
	if got := sqlServerString("it's"); got != "N'it''s'" {
		t.Fatalf("sqlServerString: %s", got)
	}
}

func TestCheckSQLServerQuery(t *testing.T) {
	for _, q := range []string{
		"SELECT id FROM dbo.users WHERE email = N'a@b.c'",
		"select top (1) [id] from [dbo].[users] order by [id] desc;",
		"WITH x AS (SELECT 1 AS n) SELECT n FROM x",
		"SELECT [update], [delete] FROM t", // quoted names are names
		"SELECT 'DROP TABLE x; -- /*' AS s",
		`SELECT "set" FROM t WHERE a = 'it''s'`,
		"SELECT CAST(0x1F AS int), 1e5, $1.50, @@VERSION",
		"SELECT COUNT(*) FROM t WITH (NOLOCK) WHERE name LIKE N'[a]%'",
		"SELECT v FROM OPENJSON(N'[1,2]')",
		"SELECT next_id FROM t", // not NEXT VALUE
		"SELECT 1.5e-3, .5, 0x1F, t.id FROM dbo.t",
		"SELECT [nämé] FROM t WHERE x = N'ünïcode'",
	} {
		if err := checkSQLServerQuery(q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		"",
		"SELECT 1 DROP TABLE x",
		"SELECT 1; DROP TABLE x",
		"SELECT * INTO t2 FROM t",
		"SELECT NEXT VALUE FOR dbo.seq",
		"SELECT next  value FOR dbo.seq",
		"SELECT 1 -- c",
		"SELECT /* c */ 1",
		"DELETE FROM t",
		"UPDATE t SET a = 1",
		"WITH x AS (SELECT 1 AS n) DELETE FROM t",
		"EXEC xp_cmdshell 'whoami'",
		"SELECT 1 EXEC sp_who",
		"SELECT * FROM OPENROWSET(BULK '/etc/passwd', SINGLE_CLOB) AS x",
		"SELECT * FROM [OPENROWSET](1)",
		"SELECT * FROM OPENDATASOURCE('SQLNCLI', 'Data Source=x').db.dbo.t",
		"SELECT * FROM OPENQUERY(srv, 'SELECT 1')",
		"SELECT * FROM sys.dm_exec_sessions",
		"SELECT * FROM sys.[dm_exec_requests]",
		"SELECT * FROM sys.fn_dblog(NULL, NULL)",
		"SELECT * FROM sys.sql_logins",
		"SELECT master.dbo.xp_regread()",
		"SELECT 1 WAITFOR DELAY '00:00:10'",
		"SELECT 1 DECLARE @x int",
		"SELECT 1 SET NOCOUNT ON",
		"SELECT 1 BEGIN TRAN",
		"SELECT 1 COMMIT",
		"SELECT 1 USE master",
		"SELECT 1 TRUNCATE TABLE t",
		"SELECT 'unterminated",
		"SELECT [unterminated",
		"SELECT [f](1)",
		"INSERT INTO t VALUES (1)",
		"SHUTDOWN",
		"SELECT 1EXEC('SELECT 42')",
		"SELECT 1e1EXEC('SELECT 42')",
		"SELECT 0xEXEC('SELECT 42')",
		"SELECT 1.EXEC('SELECT 42')",
		"SELECT 1 x WHERE 1=1EXEC('SELECT 42')",
		"SELECT 1\u00a0EXEC('SELECT 42')",
		"SELECT 1\u3000EXEC('SELECT 42')",
		"SELECT 1\x0bEXEC('SELECT 42')",
		"SELECT LOGINPROPERTY('sa', 'PasswordHash')",
		"SELECT 1 FROM [x].master.sys.objects",
		"SELECT * FROM otherdb.dbo.t",
		"SELECT * FROM otherdb..t",
		"SELECT * FROM srv.db.dbo.t",
	} {
		if err := checkSQLServerQuery(q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}

func TestSQLServerSum(t *testing.T) {
	var a, b ssSum
	for _, r := range []string{"x", "y", "y", "z"} {
		a.add([]byte(r))
	}
	for _, r := range []string{"y", "z", "x", "y"} {
		b.add([]byte(r))
	}
	if a.String() != b.String() {
		t.Fatal("the order of the rows changes the hash")
	}
	var c ssSum
	for _, r := range []string{"x", "y", "z", "z"} {
		c.add([]byte(r))
	}
	if a.String() == c.String() {
		t.Fatal("another multiset of rows hashes the same")
	}
}

func TestSQLServerDatabaseRefuse(t *testing.T) {
	for _, d := range []sqlServerDatabase{{AG: true}, {CDC: true}, {Replicated: true}, {Ledger: true}} {
		if d.refuse("db") == nil {
			t.Errorf("%+v not refused", d)
		}
	}
	if err := (&sqlServerDatabase{ChangeTracking: true}).refuse("db"); err != nil {
		t.Error(err)
	}
}

func TestSQLServerFirstError(t *testing.T) {
	err := mssql.Error{Number: 3013, Message: "RESTORE DATABASE is terminating abnormally.",
		All: []mssql.Error{{Number: 3154, Message: "The backup set holds a backup of a database other than the existing database."}, {Number: 3013}}}
	if got := sqlServerNumber(sqlServerFirst(err)); got != 3154 {
		t.Fatalf("first error %d", got)
	}
	if sqlServerNumber(errors.New("x")) != 0 {
		t.Fatal("number of a non-server error")
	}
}

func TestSQLServerRestored(t *testing.T) {
	s := &SQLServer{last: &ssLast{sig: &ssSignals{lsn: "1"}, content: map[int64]ssKept{
		1: {item: "table:dbo.a"}, 2: {item: "table:dbo.b"},
	}}}
	s.restored([]string{"table:dbo.a", "sequence:dbo.q"})
	if s.last.sig.lsn != "" || len(s.last.content) != 1 || s.last.content[2].item != "table:dbo.b" {
		t.Fatalf("%+v", s.last)
	}
}

func sqlServerExec(t testing.TB, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}
