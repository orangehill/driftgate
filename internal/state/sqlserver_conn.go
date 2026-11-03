package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/orangehill/driftgate/internal/dbx"
)

// QuoteSQLServer quotes an identifier for SQL Server: [name], with ] doubled.
func QuoteSQLServer(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

func qs(name string) string { return QuoteSQLServer(name) }

// qs2 quotes a schema-qualified name.
func qs2(schema, name string) string { return qs(schema) + "." + qs(name) }

// sqlServerString quotes a text for SQL Server: N'text', with ' doubled.
// Statements that take no parameters (BACKUP, RESTORE, ALTER DATABASE)
// get their texts this way; every other query passes them as parameters.
func sqlServerString(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sqlServerParams are the parameters of a sqlserver:// URL DriftGate passes
// to the driver. The driver ignores a parameter it does not know (a
// mistyped "encrypt" would leave the connection unencrypted), so every
// other one is refused.
var sqlServerParams = map[string]bool{
	msdsn.Database: true, msdsn.Encrypt: true, msdsn.TrustServerCertificate: true, msdsn.Certificate: true,
	msdsn.ServerCertificate: true, msdsn.TLSMin: true, msdsn.HostNameInCertificate: true,
	msdsn.ConnectionTimeout: true, msdsn.DialTimeout: true, msdsn.KeepAlive: true, msdsn.PacketSize: true,
	msdsn.AppName: true, msdsn.WorkstationID: true, msdsn.Protocol: true, msdsn.MultiSubnetFailover: true,
}

// sqlServerRefused names what a parameter the driver knows would ask for
// that DriftGate does not do.
var sqlServerRefused = map[string]string{
	"authenticator":          "Windows, Kerberos and Microsoft Entra (Azure AD) sign-in are not supported; use an SQL login",
	"fedauth":                "Windows, Kerberos and Microsoft Entra (Azure AD) sign-in are not supported; use an SQL login",
	"integrated security":    "Windows, Kerberos and Microsoft Entra (Azure AD) sign-in are not supported; use an SQL login",
	"trusted_connection":     "Windows, Kerberos and Microsoft Entra (Azure AD) sign-in are not supported; use an SQL login",
	msdsn.ServerSpn:          "Windows, Kerberos and Microsoft Entra (Azure AD) sign-in are not supported; use an SQL login",
	msdsn.ApplicationIntent:  "ApplicationIntent=ReadOnly connects to a read-only replica, which cannot be restored",
	msdsn.FailoverPartner:    "a failover partner (database mirroring) is not supported",
	msdsn.FailOverPort:       "a failover partner (database mirroring) is not supported",
	msdsn.FailoverPartnerSpn: "a failover partner (database mirroring) is not supported",
	"columnencryption":       "Always Encrypted is not supported",
	msdsn.ChangePassword:     "changing the password at sign-in is not supported",
}

// sqlServerConfig reads a sqlserver:// URL of the go-mssqldb driver (as
// dbdsn and appconf make it). Only an SQL login (user and password) over
// TCP is accepted: Windows, Kerberos and Entra sign-in, named pipes, shared
// memory, a named instance without a port (it would ask the SQL Server
// Browser over UDP), read-only intent and failover partners are refused.
// encrypt=disable (no TLS even for the password) is refused; a URL without
// encrypt gets encrypt=true, which verifies the server's certificate (the
// driver alone would encrypt only the sign-in, without checking the
// certificate). No error carries the DSN or the password.
func sqlServerConfig(dsn string) (msdsn.Config, error) {
	var none msdsn.Config
	if !strings.HasPrefix(strings.ToLower(dsn), "sqlserver://") {
		return none, errors.New("dsn: an SQL Server DSN must be a sqlserver:// URL")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return none, errors.New("dsn: the SQL Server DSN is not a valid URL") // url's error quotes it
	}
	u.Scheme = "sqlserver"
	pw, _ := u.User.Password()
	clean := func(err error) error {
		msg := err.Error()
		if pw != "" && strings.Contains(msg, pw) {
			msg = "invalid value"
		}
		return fmt.Errorf("dsn: the SQL Server DSN cannot be read: %s", msg)
	}
	user := u.User.Username()
	switch {
	case user == "":
		return none, errors.New("dsn: the SQL Server DSN names no user; DriftGate signs in with an SQL login (user and password)")
	case strings.Contains(user, `\`): // the driver signs in with NTLM
		return none, errors.New(`dsn: a Windows login (DOMAIN\user) is not supported; use an SQL login`)
	}
	host, port := u.Host, ""
	if h, p, err := net.SplitHostPort(u.Host); err == nil {
		host, port = h, p
	}
	switch {
	case host == "":
		return none, errors.New("dsn: the SQL Server DSN names no host")
	case strings.HasPrefix(strings.ToLower(host), "np:"), strings.HasPrefix(strings.ToLower(host), "lpc:"), strings.HasPrefix(strings.ToLower(host), "(localdb)"):
		return none, errors.New("dsn: named pipes, shared memory and LocalDB are not supported; connect over TCP (host:port)")
	case strings.Trim(u.Path, "/") != "" && port == "":
		return none, errors.New("dsn: a named instance needs its port (host:port): DriftGate does not ask the SQL Server Browser for it")
	}
	q := u.Query()
	for key := range q {
		k := strings.ToLower(key)
		if why, ok := sqlServerRefused[k]; ok || strings.HasPrefix(k, "krb5") {
			if !ok {
				why = sqlServerRefused["authenticator"]
			}
			return none, fmt.Errorf("dsn: the SQL Server DSN has %s: %s", key, why)
		}
		if !sqlServerParams[k] {
			return none, fmt.Errorf("dsn: the SQL Server DSN has the parameter %q, which DriftGate does not pass to the driver", key)
		}
		if k != key { // the driver reads keys in lower case
			if _, ok := q[k]; ok {
				return none, fmt.Errorf("dsn: the SQL Server DSN has the parameter %q twice", k)
			}
			q[k] = q[key]
			delete(q, key)
		}
	}
	switch strings.ToLower(q.Get(msdsn.Encrypt)) {
	case "":
		q.Set(msdsn.Encrypt, "true")
	case "disable":
		return none, errors.New("dsn: encrypt=disable would send the password without TLS; use encrypt=true (or false, which still encrypts the sign-in)")
	}
	if p := strings.ToLower(q.Get(msdsn.Protocol)); p != "" && p != "tcp" {
		return none, errors.New("dsn: only the tcp protocol is supported")
	}
	q.Del(msdsn.Database) // the component names its databases
	u.RawQuery = q.Encode()
	cfg, err := msdsn.Parse(u.String())
	if err != nil {
		return none, clean(err)
	}
	return cfg, nil
}

// sqlServerSessionSQL prepares every connection DriftGate takes from a
// pool (the driver runs it after the reset of a pooled session, which
// clears what the last user set, IDENTITY_INSERT included): errors end the
// transaction (XACT_ABORT), a lock waited for longer than 30 s is an error
// instead of a hang, and the settings the row encoding relies on are the
// standard ones (CONCAT_NULL_YIELDS_NULL: a NULL column must make its
// part NULL, see sqlServerValue). Reads are READ COMMITTED: never dirty
// reads, and the database's snapshot settings are left as they are.
const sqlServerSessionSQL = `SET NOCOUNT ON; SET XACT_ABORT ON; SET LOCK_TIMEOUT 30000;
SET CONCAT_NULL_YIELDS_NULL ON; SET ANSI_NULLS ON; SET ANSI_PADDING ON; SET ANSI_WARNINGS ON; SET ARITHABORT ON;
SET QUOTED_IDENTIFIER ON; SET NUMERIC_ROUNDABORT OFF; SET TRANSACTION ISOLATION LEVEL READ COMMITTED`

// sqlServerDialer makes every connection of the driver through the
// component's dial function. As a mssql.HostDialer it gets the host name,
// not the addresses the driver would otherwise resolve itself, so
// dbx.AllowDial checks the name and connects to the address it checked
// (no DNS rebinding). Only TCP is dialled: the driver would ask the SQL
// Server Browser over UDP for a named instance without a port.
type sqlServerDialer struct {
	host string
	dial dbx.DialFunc
}

func (d sqlServerDialer) HostName() string { return d.host }

func (d sqlServerDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("SQL Server: %s connections are not made (only TCP to host:port)", network)
	}
	if d.dial == nil {
		var nd net.Dialer
		return nd.DialContext(ctx, network, addr)
	}
	return d.dial(ctx, network, addr)
}

// sqlServerConns is how many connections share a full fingerprint.
var sqlServerConns = 4

// pool returns the connection pool of a database ("" is master), opened
// the first time. Every connection of a pool runs sqlServerSessionSQL; a
// connection that must not go back to the pool (IDENTITY_INSERT left on
// after an error) is ended with discard.
func (s *SQLServer) pool(db string) *sql.DB {
	if db == "" {
		db = "master"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[db]; ok {
		return p
	}
	cfg := s.cfg
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = cfg.TLSConfig.Clone()
	}
	cfg.Database = db
	c := mssql.NewConnectorConfig(cfg)
	c.SessionInitSQL = sqlServerSessionSQL
	c.Dialer = sqlServerDialer{host: cfg.Host, dial: s.o.Dial}
	p := sql.OpenDB(c)
	p.SetMaxOpenConns(2 * sqlServerConns)
	p.SetMaxIdleConns(sqlServerConns + 1)
	s.pools[db] = p
	return p
}

// closePool closes the pool of a database, so DriftGate holds no session
// in it (ALTER DATABASE … SET OFFLINE, MODIFY NAME and DROP wait for, or
// end, every session). The next use opens it again.
func (s *SQLServer) closePool(db string) {
	if db == "" {
		db = "master"
	}
	if db == s.o.Working {
		s.unpin()
	}
	s.mu.Lock()
	p, ok := s.pools[db]
	delete(s.pools, db)
	s.mu.Unlock()
	if ok {
		p.Close()
	}
}

// conn takes a connection to a database ("" is master) from its pool.
func (s *SQLServer) conn(ctx context.Context, db string) (*sql.Conn, error) {
	return s.pool(db).Conn(ctx)
}

// discard ends a connection instead of returning it to the pool: for one
// whose session state is not known after an error (IDENTITY_INSERT is
// session state that survives ROLLBACK).
func discard(c *sql.Conn) {
	c.Raw(func(any) error { return driver.ErrBadConn })
	c.Close()
}

// sqlServerNumber is the number of a server error (mssql.Error), 0 for any
// other error.
func sqlServerNumber(err error) int32 {
	var e mssql.Error
	if errors.As(err, &e) {
		return e.Number
	}
	var p *mssql.Error
	if errors.As(err, &p) && p != nil {
		return p.Number
	}
	return 0
}

// sqlServerFirst reports the first message of a server error: SQL Server
// ends a failed BACKUP or RESTORE with a generic one ("BACKUP DATABASE is
// terminating abnormally", 3013) after the one that says why.
func sqlServerFirst(err error) error {
	var e mssql.Error
	if errors.As(err, &e) && len(e.All) > 1 {
		return e.All[0]
	}
	return err
}
