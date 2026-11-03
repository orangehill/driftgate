package dbdsn

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// The SQL Server DSN is go-mssqldb's URL form, the one appconf builds:
// sqlserver://user:pass@host:port?encrypt=…. This is a parser of its own,
// written by hand: only internal/state may import the driver (a CI check
// guards it), and dbdsn needs no more than the address. It reads the URL
// as go-mssqldb v1.11.2 does (msdsn.splitConnectionStringURL and its tcp
// protocol parser): the host of the URL, an empty one, . and (local) being
// localhost; the port of the URL or of the port parameter, 1433 without
// one; the path a named instance; parameter names in any case, none twice.
// What would make the driver reach another host or another port than the
// address read here is refused: a named instance without a port (the SQL
// Server Browser picks the port), a failover partner, a protocol other
// than tcp. The other parameters are left to the driver, except the TLS
// ones whose values are checked here, so no error of the driver quotes
// them.

// errSQLServerForm refuses a SQL Server DSN that is not a sqlserver:// URL.
var errSQLServerForm = errors.New("dsn: a SQL Server DSN is a sqlserver:// URL (sqlserver://user:pass@host:1433?encrypt=true; the DSN is not shown, as it holds the password)")

// sqlServerAddr reads the host and port a sqlserver:// URL connects to.
// No error carries the DSN or a part of it.
func sqlServerAddr(dsn string) (host, port string, err error) {
	// The driver takes the URL form by this prefix only, in lower case;
	// any other text it reads as an ADO.NET connection string.
	if !strings.HasPrefix(dsn, "sqlserver://") {
		return "", "", errSQLServerForm
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Opaque != "" || u.Fragment != "" {
		return "", "", errURL
	}
	if strings.Contains(u.RawQuery, ";") {
		// net/url drops a key=value pair holding a ; and the driver with
		// it: encrypt=true;x=y would connect without encryption.
		return "", "", errors.New("dsn: the query of the sqlserver:// URL holds a ; (percent-encode it as %3B; the DSN is not shown, as it holds the password)")
	}
	q := map[string]string{}
	for k, vs := range u.Query() {
		lk := strings.ToLower(k)
		if _, dup := q[lk]; dup || len(vs) > 1 {
			return "", "", errors.New("dsn: the sqlserver:// URL gives a parameter twice (names are not case-sensitive; the DSN is not shown, as it holds the password)")
		}
		q[lk] = vs[0]
	}
	if _, ok := q["server"]; ok {
		return "", "", errors.New("dsn: the sqlserver:// URL takes the server as its host, not as the server parameter")
	}
	if v, ok := q["failoverpartner"]; ok && v != "" {
		return "", "", errors.New("dsn: the failoverpartner parameter of the sqlserver:// URL is not supported: DriftGate connects to one server only")
	}
	if v, ok := q["protocol"]; ok && !strings.EqualFold(v, "tcp") {
		return "", "", errors.New("dsn: the protocol parameter of the sqlserver:// URL must be tcp")
	}
	if v, ok := q["encrypt"]; ok {
		switch strings.ToLower(v) {
		case "true", "false", "strict", "disable", "mandatory", "optional", "yes", "no", "1", "0", "t", "f":
		default:
			return "", "", errors.New("dsn: encrypt must be true, false, strict or disable")
		}
	}
	if v, ok := q["trustservercertificate"]; ok {
		switch strings.ToLower(v) {
		case "true", "false", "yes", "no", "1", "0", "t", "f":
		default:
			return "", "", errors.New("dsn: TrustServerCertificate must be true or false")
		}
	}
	host = u.Hostname()
	switch strings.ToLower(host) {
	case "", ".", "(local)":
		host = "localhost"
	}
	port = u.Port()
	if p, ok := q["port"]; ok {
		if port != "" {
			return "", "", errors.New("dsn: the sqlserver:// URL gives the port twice (in the host and as the port parameter)")
		}
		port = p
	}
	if instance := strings.TrimPrefix(u.Path, "/"); instance != "" && port == "" {
		return "", "", errors.New("dsn: the sqlserver:// URL names an instance without a port; the SQL Server Browser would pick the port: add the instance's port (host:port)")
	}
	if port == "" {
		port = "1433"
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", "", errors.New("dsn: the port of the sqlserver:// URL is not a number from 1 to 65535")
	}
	return host, strconv.FormatUint(n, 10), nil // 01433 is 1433
}

// sqlServerHosts lists every host the SQL Server driver may connect to
// with the DSN (Hosts): the one host of the URL, as SQL Server takes TCP
// only. No error carries the DSN.
func sqlServerHosts(dsn string) ([]string, error) {
	host, _, err := sqlServerAddr(dsn)
	if err != nil {
		return nil, err
	}
	return []string{host}, nil
}

// sqlServerServer names the server a SQL Server DSN connects to, without
// the user or password (Server): host:port. No error carries the DSN.
func sqlServerServer(dsn string) (string, error) {
	host, port, err := sqlServerAddr(dsn)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}
