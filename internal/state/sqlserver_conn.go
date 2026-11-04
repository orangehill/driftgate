package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/lazy"
)

// QuoteSQLServer quotes an identifier for SQL Server: [name], with ] doubled.
func QuoteSQLServer(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

func qms(name string) string { return QuoteSQLServer(name) }

// qs2 quotes a schema-qualified name.
func qs2(schema, name string) string { return qms(schema) + "." + qms(name) }

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
// A URL without encrypt gets encrypt=true, as appconf.Convert gives a
// hand-written one. encrypt=disable (no TLS even for the password) is
// refused, and with encrypt=false a server that answers it cannot encrypt
// is refused before the sign-in (sqlServerTLSGuard). No error carries the
// DSN or the password.
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
		return none, errors.New("dsn: encrypt=disable would send the password without TLS; use encrypt=true (or false, which encrypts only the sign-in and refuses a server that cannot encrypt)")
	}
	if p := strings.ToLower(q.Get(msdsn.Protocol)); p != "" && p != "tcp" {
		return none, errors.New("dsn: only the tcp protocol is supported")
	}
	if !q.Has(msdsn.PacketSize) {
		// Fewer packets for the rows a fingerprint reads than the driver's
		// 4096 bytes; the server lowers it where it allows less (16383
		// bytes when the whole connection is encrypted).
		q.Set(msdsn.PacketSize, "32767")
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
// Server Browser over UDP for a named instance without a port. Unless the
// connection is TLS from its first byte (encrypt=strict), the server's
// PRELOGIN answer is checked first (sqlServerTLSGuard).
type sqlServerDialer struct {
	host   string
	dial   dbx.DialFunc
	strict bool
}

func (d sqlServerDialer) HostName() string { return d.host }

func (d sqlServerDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("SQL Server: %s connections are not made (only TCP to host:port)", network)
	}
	var c net.Conn
	var err error
	if d.dial == nil {
		var nd net.Dialer
		c, err = nd.DialContext(ctx, network, addr)
	} else {
		c, err = d.dial(ctx, network, addr)
	}
	if err != nil || d.strict {
		return c, err
	}
	return &sqlServerTLSGuard{Conn: c}, nil
}

// The TDS values sqlServerTLSGuard reads: a packet's header, the type of
// the server's reply, and the PRELOGIN options.
const (
	tdsHeader          = 8
	tdsReply           = 0x04
	tdsOptEncryption   = 0x01
	tdsOptTraceID      = 0x05
	tdsOptTerminator   = 0xff
	tdsEncryptNotSup   = 0x02
	tdsPreloginMaxSize = 1 << 16
)

// errSQLServerNoTLS refuses a server that answers it cannot encrypt.
var errSQLServerNoTLS = errors.New("SQL Server: the server answered that it does not support encryption (PRELOGIN ENCRYPT_NOT_SUP); DriftGate never signs in without TLS, so the password is not sent")

// sqlServerTLSGuard keeps the password from going out without TLS. With
// encrypt=false go-mssqldb encrypts only the sign-in, and when the
// server's PRELOGIN answer says ENCRYPT_NOT_SUP it skips TLS altogether
// and sends the LOGIN7 packet, password and all, in clear: an active
// attacker in between, or a hostile server, would get it. The guard reads
// the server's first message (the PRELOGIN answer, which comes before the
// driver sends anything else) whole, as the driver frames it, and fails
// the connection when an ENCRYPTION option says ENCRYPT_NOT_SUP or the
// answer cannot be read as one; otherwise it hands the bytes to the driver
// unchanged and passes everything after them through. Only the driver's
// goroutine reads a connection.
type sqlServerTLSGuard struct {
	net.Conn
	checked bool
	pending []byte
}

func (g *sqlServerTLSGuard) Read(p []byte) (int, error) {
	if !g.checked {
		msg, err := readSQLServerPrelogin(g.Conn)
		if err != nil {
			g.Conn.Close()
			return 0, err
		}
		g.checked, g.pending = true, msg
	}
	if len(g.pending) > 0 {
		n := copy(p, g.pending)
		g.pending = g.pending[n:]
		return n, nil
	}
	return g.Conn.Read(p)
}

// readSQLServerPrelogin reads the server's PRELOGIN answer: its packets,
// up to one whose status is not 0 (as the driver ends a message), and
// checks it (checkSQLServerPrelogin). It returns the packets as they came.
func readSQLServerPrelogin(r io.Reader) ([]byte, error) {
	var raw, body []byte
	for {
		h := make([]byte, tdsHeader)
		if _, err := io.ReadFull(r, h); err != nil {
			return nil, err
		}
		size := int(binary.BigEndian.Uint16(h[2:4]))
		if size < tdsHeader || len(raw)+size > tdsPreloginMaxSize {
			return nil, errors.New("SQL Server: the server's PRELOGIN answer is not a valid TDS packet")
		}
		pkt := make([]byte, size)
		copy(pkt, h)
		if _, err := io.ReadFull(r, pkt[tdsHeader:]); err != nil {
			return nil, err
		}
		if len(raw) == 0 && h[0] != tdsReply {
			return nil, errors.New("SQL Server: the server did not answer PRELOGIN")
		}
		raw, body = append(raw, pkt...), append(body, pkt[tdsHeader:]...)
		if h[1] != 0 {
			break
		}
	}
	if err := checkSQLServerPrelogin(body); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkSQLServerPrelogin reads the options of a PRELOGIN answer as the
// driver does (a 5-byte entry per option: token, offset, length; 0xff
// ends them; TRACEID carries no data) and fails when one cannot be read,
// when there is no ENCRYPTION option, or when any says ENCRYPT_NOT_SUP:
// the driver takes the last one, the guard refuses on any.
func checkSQLServerPrelogin(b []byte) error {
	invalid := errors.New("SQL Server: the server's PRELOGIN answer cannot be read")
	found := false
	for off := 0; ; off += 5 {
		if off >= len(b) {
			return invalid
		}
		token := b[off]
		if token == tdsOptTerminator {
			break
		}
		if off+5 >= len(b) {
			return invalid
		}
		if token == tdsOptTraceID {
			continue
		}
		at, n := int(binary.BigEndian.Uint16(b[off+1:])), int(binary.BigEndian.Uint16(b[off+3:]))
		if at+n > len(b) || at >= len(b) {
			return invalid
		}
		if token == tdsOptEncryption {
			if n < 1 {
				return invalid
			}
			if b[at] == tdsEncryptNotSup {
				return errSQLServerNoTLS
			}
			found = true
		}
	}
	if !found {
		return invalid
	}
	return nil
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
	c.Dialer = sqlServerDialer{host: cfg.Host, dial: s.o.Dial, strict: cfg.Encryption == msdsn.EncryptionStrict}
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

// AllowNegativeSerial lets crypto/x509 parse a certificate whose serial
// number is negative, as the one SQL Server 2017 makes for itself: Go 1.23
// and later refuse it by default, also with TrustServerCertificate, and no
// connection to such a server would work. The certificate is malformed
// (RFC 5280), not weaker: the chain and the host name are verified as
// before. It turns on GODEBUG x509negativeserial=1 for the whole process,
// from the first SQL Server component on (NewSQLServer), so a project
// without SQL Server never has it; the runtime applies a GODEBUG set at run
// time (os.Setenv). A value of x509negativeserial the user set in GODEBUG
// is kept, as are the other settings there. The processes DriftGate starts
// (seed commands, hooks) inherit it. NewSQLServer calls it; code that
// connects to SQL Server without a component (tests) calls it first.
func AllowNegativeSerial() { allowNegativeSerial() }

var allowNegativeSerial = sync.OnceFunc(func() {
	v := os.Getenv("GODEBUG")
	for _, kv := range strings.Split(v, ",") {
		if k, _, _ := strings.Cut(strings.TrimSpace(kv), "="); k == "x509negativeserial" {
			return
		}
	}
	if v != "" {
		v += ","
	}
	os.Setenv("GODEBUG", v+"x509negativeserial=1")
})

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

// sqlServerAt names the statement of a batch of several that a server
// error (mssql.Error) came from: the line of the batch SQL Server reports
// it on, and the procedure or trigger it was raised in, if any; "" for
// any other error. The batches it serves hold names and numbers (where an
// identity column or sequence stands), never a row's values.
func sqlServerAt(batch string, err error) string {
	var e mssql.Error
	if !errors.As(err, &e) {
		var p *mssql.Error
		if !errors.As(err, &p) || p == nil {
			return ""
		}
		e = *p
	}
	at := ""
	if e.ProcName != "" {
		at = " in " + e.ProcName
	}
	lines := strings.Split(batch, "\n")
	if e.LineNo < 1 || int(e.LineNo) > len(lines) {
		if at == "" {
			return ""
		}
		return " (raised" + at + ")"
	}
	l := strings.TrimSpace(lines[e.LineNo-1])
	if r := []rune(l); len(r) > 200 {
		l = string(r[:200]) + "…"
	}
	if at != "" {
		return fmt.Sprintf(" (raised%s, called at line %d of the batch: %s)", at, e.LineNo, l)
	}
	return fmt.Sprintf(" (at line %d of the batch: %s)", e.LineNo, l)
}

// sqlServerOthersWait bounds how long a seed, restore, fingerprint or read
// of rows tries again while another database of the server is in
// transition (retryOthers).
var sqlServerOthersWait = time.Minute

// ssDatabaseNamed is the database a message of SQL Server names.
var ssDatabaseNamed = lazy.Regexp(`Database '((?:[^']|'')+)'`)

// othersTransition reports, for an error 927 ("Database '…' cannot be
// opened. It is in the middle of a restore") or 952 ("is in transition"),
// the database it names when that is not one of this component's
// (working, pristine, <pristine>_dgnew, <pristine>_dgx). SQL Server 2019
// and earlier fail a statement of one database so while another one is
// restored, created or renamed: even a three-part name of its own
// database (an item restore's INSERT … SELECT FROM [pristine].…) or a
// connection to it, when the other's name comes up in the lookup.
//
// Every 927 and 952 the error carries (each message of a server error,
// each error wrapped or joined) must name another database: one that
// names this component's, or names none, is not retried.
func (s *SQLServer) othersTransition(err error) (string, bool) {
	if n := sqlServerNumber(err); n != 927 && n != 952 {
		return "", false
	}
	other := ""
	for _, e := range sqlServerErrors(err) {
		if e.Number != 927 && e.Number != 952 {
			continue
		}
		// The name in the message of the 927 or 952 itself, not another
		// one of the error or of the text it is wrapped in.
		m := ssDatabaseNamed().FindStringSubmatch(e.Message)
		if m == nil {
			return "", false
		}
		name := strings.ReplaceAll(m[1], "''", "'")
		for _, own := range []string{s.o.Working, s.o.Pristine, s.o.Pristine + "_dgnew", s.excludedDB()} {
			if strings.EqualFold(name, own) {
				return "", false
			}
		}
		if other == "" {
			other = name
		}
	}
	return other, other != ""
}

// sqlServerErrors are the messages of every server error in err: each
// one's own and every message it carries (mssql.Error.All), in wrapped and
// joined errors alike.
func sqlServerErrors(err error) []mssql.Error {
	var out []mssql.Error
	var walk func(error)
	walk = func(err error) {
		switch e := err.(type) {
		case nil:
			return
		case mssql.Error:
			out = append(append(out, e), e.All...)
		case *mssql.Error:
			if e != nil {
				out = append(append(out, *e), e.All...)
			}
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				walk(x)
			}
		}
	}
	walk(err)
	return out
}

// retryOthers runs f again while it fails because another database of
// the server is in transition (othersTransition), up to
// sqlServerOthersWait: each of DriftGate's operations leaves the
// databases as a stop at any point does, and the next run completes it.
func (s *SQLServer) retryOthers(ctx context.Context, f func() error) error {
	deadline := time.Now().Add(sqlServerOthersWait)
	wait := 20 * time.Millisecond
	told := false
	for {
		err := f()
		name, ok := s.othersTransition(err)
		if !ok || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		if !told {
			fmt.Fprintf(s.o.Log, "state %s: database %s of the same server is being restored or changed, which stopped a statement on this one; trying again\n", s.o.Name, name)
			told = true
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// RetryInTransition runs f again while it fails with SQL Server's error
// 927 or 952 (a database in the middle of a restore, or in transition),
// up to 30 seconds: for the queries of tests and test applications on a
// server where other databases are restored meanwhile, which SQL Server
// 2019 and earlier let stop a statement of any database (retryOthers).
func RetryInTransition(f func() error) error {
	deadline := time.Now().Add(30 * time.Second)
	wait := 20 * time.Millisecond
	for {
		err := f()
		if n := sqlServerNumber(err); n != 927 && n != 952 || time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
		wait = min(2*wait, time.Second)
	}
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
