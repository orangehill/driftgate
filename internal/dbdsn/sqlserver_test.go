package dbdsn

import (
	"reflect"
	"strings"
	"testing"
)

// TestSQLServerHostsAndServer: the host and port of a sqlserver:// URL as
// go-mssqldb reads it.
func TestSQLServerHostsAndServer(t *testing.T) {
	for _, c := range []struct{ dsn, host, server string }{
		{"sqlserver://sa:p@127.0.0.1:14331?encrypt=true&TrustServerCertificate=true", "127.0.0.1", "127.0.0.1:14331"},
		{"sqlserver://sa:p@db.example.com?encrypt=true", "db.example.com", "db.example.com:1433"},
		{"SQLServer://sa:p@db.example.com:1533?encrypt=strict", "db.example.com", "db.example.com:1533"},
		{"sqlserver://sa:p@db.example.com:1533/SHOP?encrypt=true", "db.example.com", "db.example.com:1533"},
		{"sqlserver://sa:p@[::1]:1433?encrypt=true", "::1", "[::1]:1433"},
		{"sqlserver://sa:p@:01433?encrypt=true", "localhost", "localhost:1433"},
		{"sqlserver://sa:p;w%40x@db:1433?Encrypt=true", "db", "db:1433"},
	} {
		hosts, err := Hosts(SQLServer, c.dsn)
		if err != nil || !reflect.DeepEqual(hosts, []string{c.host}) {
			t.Errorf("Hosts(%s): %q, %v", c.dsn, hosts, err)
		}
		if s, err := Server(SQLServer, c.dsn); err != nil || s != c.server {
			t.Errorf("Server(%s): %q, %v", c.dsn, s, err)
		}
	}
}

// TestSQLServerRefused: what would make the driver reach another host or
// port than the one read, or that is not the URL form, is an error that
// holds no part of the DSN.
func TestSQLServerRefused(t *testing.T) {
	for _, c := range []struct{ dsn, err string }{
		{"server=db;user id=sa;password=s3crpw", "a SQL Server DSN is a sqlserver:// URL"},
		{"odbc:server=db;uid=sa;pwd=s3crpw", "a SQL Server DSN is a sqlserver:// URL"},
		{"sqlserver://sa:s3cr%zzpw@db:1433", "not a valid URL"},
		{"sqlserver://sa:s3cr#pw@db:1433", "not a valid URL"},
		{"sqlserver://sa:s3crpw@db/SHOP?encrypt=true", "an instance without a port"},
		{"sqlserver://sa:s3crpw@db:1433?failoverpartner=db2", "failoverpartner"},
		{"sqlserver://sa:s3crpw@db:1433?protocol=np", "protocol"},
		{"sqlserver://sa:s3crpw@db:1433?server=db2", "server parameter"},
		{"sqlserver://sa:s3crpw@db:1433?port=1533", "write sqlserver://user:password@host:1533 and remove port=1533"},
		{"sqlserver://sa:s3crpw@db?port=1533", "write sqlserver://user:password@host:1533 and remove port=1533"},
		{"sqlserver://sa:s3crpw@db?port=s3crpw", "write sqlserver://user:password@host:<port> and remove port=<port>"},
		{"sqlserver://sa:s3crpw@db:1433?encrypt=true&Encrypt=false", "a parameter twice"},
		{"sqlserver://sa:s3crpw@db:1433?encrypt=s3crpw", "encrypt must be"},
		{"sqlserver://sa:s3crpw@db:1433?TrustServerCertificate=s3crpw", "TrustServerCertificate must be"},
		{"sqlserver://sa:s3crpw@db:s3crpw", "not a valid URL"},
		{"sqlserver://sa:s3crpw@db:0", "not a number"},
		{"sqlserver://sa:s3crpw@db:1433?encrypt=true;server=evil", "holds a ;"},
		{"sqlserver://sa:s3crpw@db:1433?Port=01533", "host:1533"},
	} {
		for _, f := range []func() error{
			func() error { _, err := Hosts(SQLServer, c.dsn); return err },
			func() error { _, err := Server(SQLServer, c.dsn); return err },
		} {
			err := f()
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v, want an error with %q", c.dsn, err, c.err)
				continue
			}
			if strings.Contains(err.Error(), "s3cr") {
				t.Errorf("%s: the error holds the DSN: %v", c.dsn, err)
			}
		}
	}
}

// TestSQLServerDriverParity: the host and port read here are the ones
// go-mssqldb v1.11.2's msdsn.Parse gives (port 0 there is its default,
// 1433), for the inputs where the two could part; every row was checked
// against the driver.
func TestSQLServerDriverParity(t *testing.T) {
	for _, c := range []struct{ dsn, server string }{
		{"sqlserver://sa:p@db:1433?encrypt=true", "db:1433"},
		{"sqlserver://sa:p@:1433", "localhost:1433"},
		{"sqlserver://sa:p@.:1433", "localhost:1433"},
		{"sqlserver://sa:p@[::1]:1433", "[::1]:1433"},
		{"sqlserver://sa:p;w@db:01433", "db:1433"},
		{"sqlserver://sa:p@db:1433/inst", "db:1433"},                  // the port wins over the instance
		{"sqlserver://sa:p@db?database=x%3Bserver%3Devil", "db:1433"}, // an encoded ; is the database's
	} {
		if s, err := Server(SQLServer, c.dsn); err != nil || s != c.server {
			t.Errorf("%s: %q, %v; the driver connects to %s", c.dsn, s, err, c.server)
		}
	}
}
