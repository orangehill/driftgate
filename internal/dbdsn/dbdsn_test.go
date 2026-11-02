package dbdsn

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestHosts: every host the driver connects to, as the driver reads the
// DSN; a host= parameter is not a local socket.
func TestHosts(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGSERVICE", "")
	for _, c := range []struct {
		driver, dsn string
		want        []string
	}{
		{Postgres, "postgres://u:p@127.0.0.1:5432/postgres?host=203.0.113.10", []string{"203.0.113.10"}},
		{Postgres, "postgres://u:p@localhost/postgres?host=db.prod.example.com&port=5432", []string{"db.prod.example.com"}},
		{Postgres, "postgres:///postgres?host=203.0.113.10", []string{"203.0.113.10"}},
		{Postgres, "postgres://u:p@127.0.0.1:5432,db.example.com:5432/db?sslmode=disable", []string{"127.0.0.1", "db.example.com"}},
		{Postgres, "postgres://u:p@/db?host=/var/run/postgresql&sslmode=disable", []string{""}},
		{MySQL, "mysql://u:p@db.example.com:3306/", []string{"db.example.com"}},
		{MySQL, "mysql://u:p@/", []string{"localhost"}},
		{MySQL, "u:p@tcp(203.0.113.9:3306)/", []string{"203.0.113.9"}},
		{MySQL, "u:p@unix(/tmp/mysql.sock)/", []string{""}},
	} {
		got, err := Hosts(c.driver, c.dsn)
		if err != nil {
			t.Errorf("%s: %v", c.dsn, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.dsn, got, c.want)
		}
	}
	t.Setenv("PGHOST", "203.0.113.11")
	if got, err := Hosts(Postgres, "postgres://u:p@/db"); err != nil || !reflect.DeepEqual(got, []string{"203.0.113.11"}) {
		t.Errorf("PGHOST: %q, %v", got, err)
	}
}

// TestErrorsHideTheDSN: no error shows the DSN or a part of its password,
// also when the password holds % or @ that are not encoded.
func TestErrorsHideTheDSN(t *testing.T) {
	for _, c := range []struct{ driver, dsn string }{
		{MySQL, "mysql://root:s3cr%t@pw@127.0.0.1:3306/"},
		{MySQL, "mysql://root:s3cr%zzpw@127.0.0.1:3306/"},
		{MySQL, "root:s3cr%tpw@tcp(127.0.0.1:3306/"},
		{MySQL, "root:s3crpw@tcp(127.0.0.1:3306)/?timeout=s3crpw"},
		{Postgres, "postgres://app:s3cr%t@pw@127.0.0.1:5432/postgres"},
		{Postgres, "postgres://app:s3cr%zzpw@127.0.0.1:5432/postgres"},
		{Postgres, "postgres://app:s3cr@pw@127.0.0.1:5432/postgres?port=s3crpw"},
		{Postgres, "postgres://app:s3crpw@127.0.0.1:5432/postgres?sslmode=s3crpw"},
	} {
		_, err := Hosts(c.driver, c.dsn)
		if err == nil {
			t.Errorf("%s: accepted", c.dsn)
			continue
		}
		if strings.Contains(err.Error(), "s3cr") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: the error shows the DSN: %v", c.dsn, err)
		}
	}
	if _, err := PostgresURL("postgres://app:s3cr%zzpw@h/db"); err == nil || strings.Contains(err.Error(), "zz") {
		t.Errorf("PostgresURL: %v", err)
	}
}

// TestServer: the server of a DSN, without its credentials or database.
func TestServer(t *testing.T) {
	for _, c := range []struct{ driver, dsn, want string }{
		{MySQL, "mysql://root:secret@127.0.0.1:33061/app", "tcp(127.0.0.1:33061)"},
		{MySQL, "mysql://other:pw@127.0.0.1:33061/", "tcp(127.0.0.1:33061)"},
		{MySQL, "mariadb://root@db", "tcp(db:3306)"},
		{MySQL, "root:pw@tcp(localhost:3307)/", "tcp(localhost:3307)"},
		{MySQL, "root:pw@unix(/tmp/mysql.sock)/", "unix(/tmp/mysql.sock)"},
		{Postgres, "postgres://u:p@db:5433/app", "db:5433"},
		{Postgres, "postgres://u:p@a:5432,b:5433/app?sslmode=disable", "a:5432,b:5433"},
	} {
		got, err := Server(c.driver, c.dsn)
		if err != nil || got != c.want || strings.Contains(got, "secret") {
			t.Errorf("%s: %q, %v; want %q", c.dsn, got, err, c.want)
		}
	}
}

// TestMySQLURLParams: the parameters of a mysql:// URL reach the driver's
// configuration; one DriftGate does not know is an error, never dropped (a
// tls=true dropped would connect without TLS).
func TestMySQLURLParams(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, testCA, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query, tls, err string
		verify          bool // a TLS configuration with certificate authorities of its own
	}{
		{"", "", "", false},
		{"?tls=true", "true", "", false},
		{"?tls=skip-verify", "skip-verify", "", false},
		{"?tls=preferred", "preferred", "", false},
		{"?tls=false", "false", "", false},
		{"?ssl-mode=REQUIRED", "skip-verify", "", false},
		{"?ssl-mode=verify_identity", "true", "", false},
		{"?ssl-mode=DISABLED", "false", "", false},
		{"?ssl-ca=" + ca, "", "", true},
		{"?tls=true&ssl-ca=" + ca, "", "", true},
		{"?tls=skip-verify&ssl-ca=" + ca, "", "turns off", false},
		{"?ssl-ca=/nonexistent/ca.pem", "", "ssl-ca", false},
		{"?tls=maybe", "", "tls must be", false},
		{"?ssl-mode=on", "", "ssl-mode must be", false},
		{"?sslmode=require", "", `parameter "sslmode"`, false},
		{"?charset=latin1", "", `parameter "charset"`, false},
		{"?timeout=10", "", "timeout must be a duration", false},
	} {
		cfg, err := MySQLConfig("mysql://u:s3crpw@db.example.com:3306/app" + c.query)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: error %v, want %q", c.query, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if cfg.TLSConfig != c.tls || (cfg.TLS != nil && cfg.TLS.RootCAs != nil) != c.verify {
			t.Errorf("%s: tls %q (own CAs %v), want %q (%v)", c.query, cfg.TLSConfig, cfg.TLS != nil, c.tls, c.verify)
		}
	}
	cfg, err := MySQLConfig("mysql://u:p@db.example.com/?timeout=5s&readTimeout=30s&writeTimeout=1m")
	if err != nil || cfg.Timeout != 5*time.Second || cfg.ReadTimeout != 30*time.Second || cfg.WriteTimeout != time.Minute {
		t.Errorf("timeouts: %+v, %v", cfg, err)
	}
	// A ? or # in a password that is not encoded is not shown.
	for _, dsn := range []string{"mysql://root:s3cr?pw@127.0.0.1:3306/", "mysql://root:s3cr#pw@127.0.0.1:3306/"} {
		if _, err := MySQLConfig(dsn); err == nil || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: %v", dsn, err)
		}
	}
}

// testCA is a self-signed certificate, only read as PEM.
var testCA = []byte(`-----BEGIN CERTIFICATE-----
MIIBVjCB/KADAgECAhRIWGfgfRI99b+qjjCfmCYS6fXL0zAKBggqhkjOPQQDAjAR
MQ8wDQYDVQQDDAZ0ZXN0Y2EwHhcNMjYxMDMwMjI0NDM0WhcNMzYxMDI3MjI0NDM0
WjARMQ8wDQYDVQQDDAZ0ZXN0Y2EwWTATBgcqhkjOPQIBBggqhkjOPQMBBwNCAASM
elrhACRqG+nSBe0GIJw/K3bb77lLJuSbQ3IKiuSdiP9nEoqI/nsmGI6z3J9FSrdC
ZJDfSSDcPqe7YIogQF/KozIwMDAdBgNVHQ4EFgQUrAxzkLwChF7wn1Z8FFjZcTk2
yrcwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNJADBGAiEAnSiEjtvjlsPv
4SqrcOjUq+fKaJORVkKp063O1bsJN1kCIQDunyQpbkzVge4TEaf0cX5DnmUb+JIx
e3FJ3Azzkac45g==
-----END CERTIFICATE-----
`)
