package appconf

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The settings of a SQL Server database, in every form its clients write
// them: an ADO.NET connection string (Microsoft.Data.SqlClient,
// System.Data.SqlClient), an ODBC string, a JDBC URL (and Prisma's, which
// is written the same way), a go-mssqldb URL, the URLs of Laravel and
// Node, and the separate values of Laravel, Django, Rails and compose.
//
// DriftGate signs in with a SQL Server login only: Windows and Microsoft
// Entra ID accounts, Kerberos, and the client features DriftGate's driver
// does not have (Always Encrypted, LocalDB, named pipes, failover
// partners, read-only routing, attached files) are refused by name.
//
// The TLS DriftGate connects with is never weaker than the application's
// client: Encrypt, TrustServerCertificate, HostNameInCertificate and the
// certificates of the setting are passed on, and when the setting leaves
// Encrypt to the client, the client's own default is taken (a client
// DriftGate cannot tell encrypts and verifies, as current ones do). A
// value DriftGate cannot map is an error.

// sqlServerReady turns on the reading of SQL Server settings: driverName,
// csprojDriver and the readers then give the driver SQLServer, and the
// functions below read the setting.
const sqlServerReady = true

// sqlServerTLS is what a SQL Server setting says besides the address and
// the credentials: its TLS, the default of its client, and what Find
// notes about it.
type sqlServerTLS struct {
	read bool // the setting was read as SQL Server's (else parts.raw holds its keys)
	// encrypt is true, false or strict ("" leaves it to the client); a
	// setting without TLS at all (Prisma's DANGER_PLAINTEXT, go-mssqldb's
	// disable) is false: DriftGate still encrypts the login.
	encrypt string
	// trust is TrustServerCertificate: true, false, or "" (false).
	trust string
	// hostInCert is HostNameInCertificate; ca a CA certificate file the
	// server's certificate is verified with; serverCert the server's own
	// certificate, compared byte for byte; tlsMin the lowest TLS version.
	hostInCert, ca, serverCert, tlsMin string
	// client is the Encrypt default of the application's client: true,
	// false, or "" when DriftGate cannot tell (it then encrypts and
	// verifies).
	client string
	// compose: the setting is a compose service's, whose certificate is
	// the container's own, self-signed one: trusted on a loopback address
	// of this machine only.
	compose bool
	note    string
}

// Errors of SQL Server settings. None quotes a value.
const (
	errSQLServerWindows    = constError("signs in with a Windows account (Integrated Security, Trusted_Connection, integratedSecurity); DriftGate signs in with a SQL Server login only: set a user and password")
	errSQLServerDomain     = constError("names a Windows account (DOMAIN\\user); DriftGate signs in with a SQL Server login only")
	errSQLServerEntra      = constError("signs in with Microsoft Entra ID (Authentication=ActiveDirectory…); DriftGate signs in with a SQL Server login only, and restores no Azure SQL Database")
	errSQLServerAuth       = constError("names an Authentication DriftGate does not know (the value is not shown); DriftGate signs in with a SQL Server login only (SqlPassword)")
	errSQLServerKerberos   = constError("signs in with Kerberos or NTLM (authenticationScheme); DriftGate signs in with a SQL Server login only")
	errSQLServerAE         = constError("uses Always Encrypted (Column Encryption Setting); DriftGate cannot read encrypted columns")
	errSQLServerReadOnly   = constError("asks for a read-only replica (ApplicationIntent=ReadOnly); DriftGate restores the primary database: remove it")
	errSQLServerFailover   = constError("names a Failover Partner; DriftGate connects to one server only: remove it")
	errSQLServerAttach     = constError("attaches a database file (AttachDbFilename, User Instance); DriftGate restores a database of a running server: attach it there and name it with Database=")
	errSQLServerPipe       = constError("reaches the server through a named pipe or shared memory (np:, lpc:, Network Library); DriftGate connects over TCP: name the host and port (host,1433)")
	errSQLServerLocalDB    = constError("names LocalDB ((localdb)\\…), which takes no TCP connection; run SQL Server (e.g. the mcr.microsoft.com/mssql/server container) and name its host and port")
	errSQLServerAdmin      = constError("names the dedicated admin connection (admin:); DriftGate connects as an application does: remove admin:")
	errSQLServerTrustStore = constError("verifies the server with a Java trust store (trustStore, trustManagerClass), which DriftGate cannot read; give the CA certificate as a PEM file instead, or remove it")
	errSQLServerODBCDSN    = constError("names an ODBC data source (DSN=), whose settings DriftGate cannot read; write the server, port and credentials in the setting")
	errSQLServerServer     = constError("the server is not host, host,port or tcp:host,port")
	errSQLServerPort       = constError("the port is not a number from 1 to 65535")
	errSQLServerPorts      = constError("names two different ports (one with the host, one alone); keep one")
	errSQLServerAmbiguous  = constError("a sqlserver:// URL with a path and no port: go-mssqldb takes the path for a named instance, Rails for the database; add the port (host:1433) and the database parameter")
	errSQLServerSocket     = constError("names a socket; SQL Server is reached over TCP: name the host and port")
	errSQLServerNoUser     = constError("names no user; DriftGate signs in with a SQL Server login (user and password)")
	// errSQLServerInstance refuses a named instance without a port: the SQL
	// Server Browser would choose the port, on a UDP port DriftGate does
	// not reach.
	errSQLServerInstance = constError("names an instance without its port; add the port the instance listens on (host\\instance,port)")
	errSQLServerCertPair = constError("names the server's own certificate (ServerCertificate) with HostNameInCertificate or a CA certificate; the two ways of verifying do not go together: keep one")
)

// errSQLServerValue names a SQL Server setting, by DriftGate's name for it,
// whose value it does not know.
func errSQLServerValue(name, values string) error {
	return fmt.Errorf("the SQL Server setting %s has a value DriftGate does not know (the value is not shown); set it to %s", name, values)
}

// sqlServerBool reads a yes/no value as SQL Server's clients write it.
func sqlServerBool(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "t", "on":
		return "true", true
	case "false", "no", "0", "f", "off":
		return "false", true
	}
	return "", false
}

// set records one key of a SQL Server setting (fieldKey), as the
// connection strings, JDBC, ODBC, Prisma and go-mssqldb name it; a key it
// does not know is left out. A key DriftGate refuses is an error.
func (s *sqlServerTLS) set(k, v string) error {
	v = strings.TrimSpace(v)
	switch k {
	case "encrypt":
		switch strings.ToLower(v) {
		case "true", "yes", "mandatory", "1", "t":
			s.encrypt = "true"
		case "false", "no", "optional", "0", "f",
			"disable", "danger_plaintext": // go-mssqldb, Prisma: no TLS at all; DriftGate encrypts the login
			s.encrypt = "false"
		case "strict":
			s.encrypt = "strict"
		default:
			return errSQLServerValue("Encrypt", "true, false or strict")
		}
	case "trustservercertificate":
		b, ok := sqlServerBool(v)
		if !ok {
			return errSQLServerValue("TrustServerCertificate", "true or false")
		}
		s.trust = b
	case "hostnameincertificate":
		s.hostInCert = v
	case "servercertificate":
		s.serverCert = v
	case "certificate", "trustservercertificateca": // go-mssqldb, Prisma: the CA
		s.ca = v
	case "tlsmin":
		switch v {
		case "", "1.0", "1.1", "1.2", "1.3":
			s.tlsMin = v
		default:
			return errSQLServerValue("tlsmin", "1.0, 1.1, 1.2 or 1.3")
		}
	case "sslprotocol": // JDBC: TLS, TLSv1, TLSv1.1, TLSv1.2 or TLSv1.3
		switch strings.ToLower(v) {
		case "tlsv1.3":
			s.tlsMin = "1.3"
		case "", "tls", "tlsv1", "tlsv1.1", "tlsv1.2": // DriftGate's driver takes TLS 1.2 at least
		default:
			return errSQLServerValue("sslProtocol", "TLS, TLSv1.2 or TLSv1.3")
		}
	case "integratedsecurity", "trustedconnection":
		if b, ok := sqlServerBool(v); !ok || b == "true" { // also SSPI
			return errSQLServerWindows
		}
	case "authentication":
		switch a := fieldKey(v); {
		case a == "" || a == "sqlpassword" || a == "notspecified":
		case strings.HasPrefix(a, "activedirectory"):
			return errSQLServerEntra
		default:
			return errSQLServerAuth
		}
	case "authenticationscheme":
		if a := fieldKey(v); a != "" && a != "nativeauthentication" {
			return errSQLServerKerberos
		}
	case "columnencryptionsetting", "columnencryption":
		if b, ok := sqlServerBool(v); strings.EqualFold(v, "enabled") || ok && b == "true" {
			return errSQLServerAE
		}
	case "enclaveattestationurl":
		if v != "" {
			return errSQLServerAE
		}
	case "applicationintent":
		if fieldKey(v) == "readonly" {
			return errSQLServerReadOnly
		}
	case "failoverpartner":
		if v != "" {
			return errSQLServerFailover
		}
	case "attachdbfilename", "extendedproperties", "initialfilename":
		if v != "" {
			return errSQLServerAttach
		}
	case "userinstance":
		if b, _ := sqlServerBool(v); b == "true" {
			return errSQLServerAttach
		}
	case "contextconnection":
		if b, _ := sqlServerBool(v); b == "true" {
			return errSQLServerPipe
		}
	case "networklibrary", "network", "net":
		if l := strings.ToLower(v); l != "" && l != "dbmssocn" {
			return errSQLServerPipe
		}
	case "protocol": // go-mssqldb
		switch strings.ToLower(v) {
		case "", "tcp":
		case "admin":
			return errSQLServerAdmin
		default:
			return errSQLServerPipe
		}
	case "truststore", "truststorepassword", "truststoretype", "trustmanagerclass", "trustmanagerconstructorarg":
		if v != "" {
			return errSQLServerTrustStore
		}
	case "dsn", "odbcdsn":
		if v != "" {
			return errSQLServerODBCDSN
		}
	}
	return nil
}

// setAll records the keys of f, in a fixed order so the same setting
// always gives the same error.
func (s *sqlServerTLS) setAll(f fields) error {
	for _, k := range sortedKeys(f) {
		if err := s.set(k, f[k]); err != nil {
			return err
		}
	}
	return nil
}

// sqlServerAddress reads the server of a SQL Server setting as its clients
// write it: [tcp:]host[\instance][,port], with port the setting's separate
// port ("" for none). A named instance takes the port it is given (the
// clients then skip the SQL Server Browser) and is refused without one.
// The local server (., (local), none) is 127.0.0.1, with a note.
func sqlServerAddress(server, port string) (host, outPort, note string, err error) {
	if err := sqlServerTransport(server); err != nil {
		return "", "", "", err
	}
	s := strings.TrimSpace(server)
	if len(s) >= 4 && strings.EqualFold(s[:4], "tcp:") {
		s = strings.TrimSpace(s[4:])
	}
	port = strings.TrimSpace(port)
	if i := strings.LastIndexByte(s, ','); i >= 0 {
		p := strings.TrimSpace(s[i+1:])
		s = strings.TrimSpace(s[:i])
		if port != "" && port != p {
			return "", "", "", errSQLServerPorts
		}
		port = p
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strings.Trim(port, "0123456789") != "" {
			return "", "", "", errSQLServerPort
		}
		port = strconv.Itoa(n) // 01433 is 1433
	}
	h, inst, _ := strings.Cut(s, `\`)
	if inst != "" && port == "" {
		return "", "", "", errSQLServerInstance
	}
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	switch strings.ToLower(h) {
	case "", ".", "(local)":
		what := h
		if what == "" {
			what = "not set"
		}
		note = "the server is " + what + ", the local one, which a Windows client may reach through shared memory; DriftGate connects over TCP to 127.0.0.1"
		h = "127.0.0.1"
	}
	if net.ParseIP(h) == nil && strings.Trim(h, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_") != "" {
		return "", "", "", errSQLServerServer // a host name or an IP address
	}
	return h, port, note, nil
}

// sqlServerTransport refuses a server reached otherwise than over TCP:
// a named pipe, shared memory, the dedicated admin connection, LocalDB.
// The readers check it first, as the other refusals follow from it.
func sqlServerTransport(server string) error {
	ls := strings.ToLower(strings.TrimSpace(server))
	switch {
	case strings.HasPrefix(ls, "np:"), strings.HasPrefix(ls, "lpc:"), strings.HasPrefix(ls, `\\`):
		return errSQLServerPipe
	case strings.HasPrefix(ls, "admin:"):
		return errSQLServerAdmin
	case strings.HasPrefix(ls, "(localdb)"):
		return errSQLServerLocalDB
	}
	return nil
}

// sqlServerServerForm reports a server only SQL Server's clients write:
// tcp:, np:, lpc:, admin:, (localdb), (local), host\instance or host,port.
func sqlServerServerForm(server string) bool {
	ls := strings.ToLower(strings.TrimSpace(server))
	for _, pre := range []string{"tcp:", "np:", "lpc:", "admin:", "(localdb)", "(local)"} {
		if strings.HasPrefix(ls, pre) {
			return true
		}
	}
	if strings.Contains(ls, `\`) {
		return true
	}
	i := strings.LastIndexByte(ls, ',')
	return i >= 0 && i < len(ls)-1 && strings.Trim(strings.TrimSpace(ls[i+1:]), "0123456789") == ""
}

// sqlServerValue reads a DSN only SQL Server's clients write, before
// fromValue tries the other forms: a JDBC or Prisma URL (sqlserver://host;
// databaseName=…, the jdbc: prefix already cut), a go-mssqldb URL, an
// ODBC string. ok is false for any other DSN.
func sqlServerValue(v string) (p parts, ok bool, err error) {
	if len(v) >= len("sqlserver://") && strings.EqualFold(v[:len("sqlserver://")], "sqlserver://") {
		rest := v[len("sqlserver://"):]
		if jdbcForm(rest) {
			p, err = fromSQLServerJDBC(rest)
		} else {
			p, err = fromSQLServerURL(v)
		}
		return p, true, err
	}
	if len(v) >= len("odbc:") && strings.EqualFold(v[:len("odbc:")], "odbc:") { // go-mssqldb's ODBC form
		kv, ok := parseODBC(v[len("odbc:"):])
		if !ok {
			return parts{}, true, errors.New("not an ODBC connection string (the value is not shown)")
		}
		p, err = fromODBC(kv)
		return p, true, err
	}
	if kv, ok := parseODBC(v); ok && odbcSQLServer(kv["driver"]) {
		p, err = fromODBC(kv)
		return p, true, err
	}
	// One that names a SQL Server ODBC driver but is written wrong is not
	// read by other rules, which take its {…} literally.
	if kv, err := parseConnString(v); err == nil && odbcSQLServer(kv["driver"]) {
		return parts{}, true, errors.New("not an ODBC connection string: every pair is key=value, separated by ;, a value with ; or } in {…} (the value is not shown)")
	}
	return parts{}, false, nil
}

// jdbcForm reports a sqlserver:// URL written as JDBC and Prisma write it
// (sqlserver://host[\instance][:port];key=value…), told from a go-mssqldb
// URL (sqlserver://user:pass@host:port/instance?key=value, whose password
// may hold a ;) by what comes before the first ;, a host, an instance and
// a port without @ ? or /, and by what follows, properties whose names
// are words.
func jdbcForm(rest string) bool {
	head, props, ok := strings.Cut(rest, ";")
	if !ok || strings.ContainsAny(head, "@?/") {
		return false
	}
	if strings.Count(head, ":") == 1 { // host:port, not user:password
		_, port, _ := strings.Cut(head, ":")
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return false
		}
	}
	kv, err := parseJDBCProps(props)
	if err != nil {
		return true // a JDBC URL written wrong: its parser says so
	}
	for k := range kv {
		if strings.Trim(k, "abcdefghijklmnopqrstuvwxyz0123456789.") != "" {
			return false
		}
	}
	return true
}

// parseJDBCProps reads the properties of a JDBC or Prisma URL: key=value
// pairs separated by ;, a value in {…} when it holds a ; (}} for a }),
// keys as fieldKey makes them. No error quotes the text.
func parseJDBCProps(s string) (fields, error) {
	out := fields{}
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ';' || isSpace(s[i])) {
			i++
		}
		if i >= len(s) {
			break
		}
		eq := strings.IndexByte(s[i:], '=')
		if semi := strings.IndexByte(s[i:], ';'); eq < 0 || semi >= 0 && semi < eq {
			return nil, errors.New("a property of the URL has no = (the value is not shown)")
		}
		k := fieldKey(strings.TrimSpace(s[i : i+eq]))
		i += eq + 1
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		var v string
		if i < len(s) && s[i] == '{' {
			var b strings.Builder
			i++
			for {
				if i >= len(s) {
					return nil, errors.New("a {…} value of the URL has no closing } (the value is not shown)")
				}
				if s[i] == '}' {
					if i+1 < len(s) && s[i+1] == '}' {
						b.WriteByte('}')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			v = b.String()
			for i < len(s) && isSpace(s[i]) {
				i++
			}
			if i < len(s) && s[i] != ';' {
				return nil, errors.New("text after a {…} value of the URL (the value is not shown)")
			}
		} else {
			j := strings.IndexByte(s[i:], ';')
			if j < 0 {
				j = len(s) - i
			}
			v = strings.TrimSpace(s[i : i+j])
			i += j
		}
		if k == "" {
			return nil, errors.New("a property of the URL has no name (the value is not shown)")
		}
		out[k] = v
	}
	return out, nil
}

// fromSQLServerJDBC reads a JDBC URL of mssql-jdbc, or Prisma's, which is
// written the same way (rest is what follows sqlserver://):
// host[\instance][:port];databaseName=…;user=…;password=…;encrypt=…. Both
// clients encrypt and verify by default (mssql-jdbc 10.2 and later).
func fromSQLServerJDBC(rest string) (parts, error) {
	head, props, _ := strings.Cut(rest, ";")
	kv, err := parseJDBCProps(props)
	if err != nil {
		return parts{}, err
	}
	host, port := strings.TrimSpace(head), ""
	switch {
	case strings.HasPrefix(host, "["): // [::1]:1433
		if end := strings.IndexByte(host, ']'); end > 0 {
			host, port = host[:end+1], strings.TrimPrefix(host[end+1:], ":")
		}
	case strings.Count(host, ":") == 1:
		host, port, _ = strings.Cut(host, ":")
	}
	if host == "" {
		host = kv["servername"]
	}
	if inst, ok := kv["instancename"]; ok && !strings.Contains(host, `\`) && inst != "" {
		host += `\` + inst
	}
	if port == "" {
		port, _ = kv.get([]string{"portnumber", "port"})
	}
	if host == "" {
		host = "localhost" // mssql-jdbc's default serverName
	}
	p := parts{driver: SQLServer}
	p.mssql.read, p.mssql.client = true, "true"
	if err := p.mssql.setAll(kv); err != nil { // what is refused before the address
		return parts{}, err
	}
	if p.host, p.port, p.mssql.note, err = sqlServerAddress(host, port); err != nil {
		return parts{}, err
	}
	p.name, _ = kv.get([]string{"databasename", "database", "initialcatalog"})
	p.user, _ = kv.get([]string{"user", "username", "userid", "uid"})
	p.password, _ = kv.get([]string{"password", "pwd"})
	return p, nil
}

// goMSSQLParams are the parameters of a go-mssqldb URL DriftGate takes,
// besides the ones sqlServerTLS.set reads and refuses: a parameter it
// does not know is an error, so a connection never goes without what the
// application asked for.
var goMSSQLParams = []string{"database", "userid", "password", "port", "appname", "connectiontimeout", "dialtimeout",
	"keepalive", "packetsize", "log", "workstationid", "multisubnetfailover", "disableretry", "guidconversion",
	"notraceid", "timezone", "serverspn", "failoverpartnerspn", "failoverport", "epaenabled"}

// goMSSQLTLSParams are the parameters sqlServerTLS.set reads or refuses
// in a go-mssqldb URL.
var goMSSQLTLSParams = []string{"encrypt", "trustservercertificate", "hostnameincertificate", "servercertificate",
	"certificate", "tlsmin", "authentication", "columnencryption", "applicationintent", "failoverpartner", "protocol"}

// fromSQLServerURL reads a URL whose scheme names SQL Server: a go-mssqldb
// URL (sqlserver://user:pass@host:port/instance?database=…&encrypt=…,
// whose client leaves the data unencrypted by default), Laravel's sqlsrv://
// (pdo_sqlsrv: its ODBC driver decides), Node's mssql:// (tedious: encrypts
// and verifies) and dblib:// (FreeTDS: no encryption by default); their
// path is the database.
func fromSQLServerURL(v string) (parts, error) {
	u, err := url.Parse(v)
	if err != nil || u.Opaque != "" || u.Fragment != "" {
		return parts{}, errURL
	}
	scheme := strings.ToLower(u.Scheme)
	p := parts{driver: SQLServer}
	p.mssql.read = true
	p.user = u.User.Username()
	p.password, _ = u.User.Password()
	q := fields{}
	for k, vs := range u.Query() {
		fk := fieldKey(k)
		if _, dup := q[fk]; dup || len(vs) > 1 {
			return parts{}, fmt.Errorf("the %s:// URL gives a parameter twice (the value is not shown)", scheme)
		}
		q[fk] = vs[0]
	}
	upath := strings.Trim(u.Path, "/")
	port := u.Port()
	host := u.Hostname()
	switch scheme {
	case "sqlserver":
		p.mssql.client = "false"
		for _, k := range sortedKeys(q) {
			if !slices.Contains(goMSSQLParams, k) && !slices.Contains(goMSSQLTLSParams, k) {
				name := "a parameter"
				if printable(k) != "such a" {
					name = "parameter " + k
				}
				return parts{}, fmt.Errorf("%s of the sqlserver:// URL is not supported (the value is not shown)", name)
			}
		}
		if q["port"] != "" {
			if port != "" {
				return parts{}, errors.New("the sqlserver:// URL gives the port twice")
			}
			port = q["port"]
		}
		if p.user == "" {
			p.user = q["userid"]
		}
		if p.password == "" {
			p.password = q["password"]
		}
		if db, ok := q["database"]; ok {
			p.name = db
			if upath != "" { // the instance: taken with a port, refused without
				host += `\` + upath
			}
		} else if upath != "" {
			if port == "" {
				return parts{}, errSQLServerAmbiguous
			}
			p.name = upath
		}
	default:
		switch scheme {
		case "mssql":
			p.mssql.client = "true"
		case "dblib":
			p.mssql.client = "false"
		}
		p.name = upath
		if p.user == "" {
			p.user, _ = q.get([]string{"user", "username", "userid", "uid"})
		}
		if p.password == "" {
			p.password, _ = q.get([]string{"password", "pwd"})
		}
	}
	if host == "" {
		host = "localhost"
	}
	if err := p.mssql.setAll(q); err != nil { // what is refused before the address
		return parts{}, err
	}
	if p.host, p.port, p.mssql.note, err = sqlServerAddress(host, port); err != nil {
		return parts{}, err
	}
	return p, nil
}

// parseODBC reads an ODBC connection string: key=value pairs separated by
// ;, a value in {…} when it holds a ; (}} for a }), keys as fieldKey makes
// them. ok is false for text that is not one.
func parseODBC(s string) (fields, bool) {
	kv, err := parseJDBCProps(s)
	if err != nil || len(kv) == 0 {
		return nil, false
	}
	return kv, true
}

// odbcSQLServer reports an ODBC driver of SQL Server: Microsoft's ODBC
// Driver, its Native Client, the old SQL Server driver, FreeTDS.
func odbcSQLServer(driver string) bool {
	d := strings.ToLower(driver)
	for _, w := range []string{"sql server", "sqlncli", "msodbcsql", "freetds", "tdsodbc"} {
		if strings.Contains(d, w) {
			return true
		}
	}
	return false
}

// odbcClient is the Encrypt default of an ODBC driver: ODBC Driver 18 and
// later encrypt and verify; 17 and earlier, the Native Client, the old SQL
// Server driver and FreeTDS do not; "" for a driver DriftGate cannot tell.
func odbcClient(driver string) string {
	d := strings.ToLower(driver)
	version := func(after string) (int, bool) {
		i := strings.Index(d, after)
		if i < 0 {
			return 0, false
		}
		rest := strings.TrimLeft(d[i+len(after):], " .")
		n := 0
		for n < len(rest) && '0' <= rest[n] && rest[n] <= '9' {
			n++
		}
		v, err := strconv.Atoi(rest[:n])
		return v, err == nil
	}
	for _, w := range []string{"odbc driver", "msodbcsql"} {
		if v, ok := version(w); ok {
			if v >= 18 {
				return "true"
			}
			return "false"
		}
	}
	switch {
	case strings.Contains(d, "native client"), strings.Contains(d, "sqlncli"), strings.Contains(d, "freetds"), strings.Contains(d, "tdsodbc"),
		strings.Trim(d, "{} ") == "sql server":
		return "false"
	}
	return ""
}

// fromODBC reads an ODBC connection string of SQL Server.
func fromODBC(kv fields) (parts, error) {
	p := parts{driver: SQLServer}
	p.mssql.read = true
	p.mssql.client = odbcClient(kv["driver"])
	server, _ := kv.get([]string{"server", "address", "addr"})
	if err := sqlServerTransport(server); err != nil {
		return parts{}, err
	}
	if err := p.mssql.setAll(kv); err != nil {
		return parts{}, err
	}
	var err error
	if p.host, p.port, p.mssql.note, err = sqlServerAddress(server, kv["port"]); err != nil {
		return parts{}, err
	}
	p.name, _ = kv.get([]string{"database", "initialcatalog"})
	p.user, _ = kv.get([]string{"uid", "userid", "user"})
	p.password, _ = kv.get([]string{"pwd", "password"})
	return p, nil
}

// sqlServerMarkers are the keys of an ADO.NET connection string only
// Microsoft.Data.SqlClient and System.Data.SqlClient take.
var sqlServerMarkers = []string{"trustedconnection", "multipleactiveresultsets", "attachdbfilename", "applicationintent",
	"failoverpartner", "columnencryptionsetting", "hostnameincertificate", "multisubnetfailover", "userinstance",
	"enclaveattestationurl", "contextconnection", "transparentnetworkipresolution", "servercertificate"}

// sqlServerConnString reads an ADO.NET connection string of
// Microsoft.Data.SqlClient: one with keys or a server form only SQL Server
// takes (Trusted_Connection, MultipleActiveResultSets, AttachDbFilename,
// tcp:, np:, (localdb), host\instance, host,port). ok is false for one
// that is not; it is read by fromFields, and as SQL Server's when the
// project's packages or the component's driver say so.
func sqlServerConnString(kv fields) (p parts, ok bool, err error) {
	server, _ := kv.get(hostKeys)
	marked := sqlServerServerForm(server)
	for _, k := range sqlServerMarkers {
		if _, ok := kv[k]; ok {
			marked = true
		}
	}
	if !marked {
		return parts{}, false, nil
	}
	p = parts{driver: SQLServer, raw: kv}
	p.mssql.read = true
	if err := sqlServerTransport(server); err != nil {
		return parts{}, true, err
	}
	if err := p.mssql.setAll(kv); err != nil {
		return parts{}, true, err
	}
	if p.host, p.port, p.mssql.note, err = sqlServerAddress(server, kv["port"]); err != nil {
		return parts{}, true, err
	}
	p.name, _ = kv.get([]string{"initialcatalog", "database"})
	p.user, _ = kv.get([]string{"userid", "uid", "user", "username"})
	p.password, _ = kv.get([]string{"password", "pwd"})
	return p, true, nil
}

// sqlServerFields reads a SQL Server setting given as separate values
// (fromFields): Django's HOST and PORT (HOST may be host\instance), Rails'
// host and port, the keys of a connection string fromConnString left to
// fromFields. p holds what fromFields filled in; a setting read from a URL
// keeps the URL's TLS.
func sqlServerFields(p parts, f fields) (parts, error) {
	p.driver, p.raw = SQLServer, f
	if !p.mssql.read {
		p.mssql.read = true
		if err := p.mssql.setAll(f); err != nil {
			return parts{}, err
		}
		switch d := fieldKey(f["adapter"]); {
		case d == "sqlserver": // Rails: TinyTDS on FreeTDS
			p.mssql.client = "false"
		case f["odbcdriver"] != "": // Django's OPTIONS driver
			p.mssql.client = odbcClient(f["odbcdriver"])
		}
		if extra := f["extraparams"]; strings.TrimSpace(extra) != "" { // Django's OPTIONS extra_params
			kv, ok := parseODBC(extra)
			if !ok {
				return parts{}, errors.New("OPTIONS extra_params is not an ODBC connection string (the value is not shown)")
			}
			if err := p.mssql.setAll(kv); err != nil {
				return parts{}, err
			}
		}
	}
	if p.socket != "" {
		return parts{}, errSQLServerSocket
	}
	var err error
	var note string
	if p.host, p.port, note, err = sqlServerAddress(p.host, p.port); err != nil {
		return parts{}, err
	}
	if note != "" {
		p.mssql.note = note
	}
	return p, nil
}

// sqlServerOptions takes Django's OPTIONS of a SQL Server database
// (mssql-django) into f: the ODBC driver, whose default Encrypt
// sqlServerFields follows, the ODBC keys of extra_params and an ODBC data
// source, which is refused. Values set by code are left out.
func sqlServerOptions(m map[string]any, f fields) {
	o, ok := m["OPTIONS"].(map[string]any)
	if !ok {
		return
	}
	for k, key := range map[string]string{"driver": "odbcdriver", "extra_params": "extraparams", "dsn": "odbcdsn"} {
		if v, ok := o[k].(string); ok {
			f[key] = v
		}
	}
}

// laravelSQLServer completes Laravel's sqlsrv setting of a .env file, p
// (DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME, DB_PASSWORD read), with
// what only SQL Server takes and DB_URL. Laravel joins DB_HOST and DB_PORT
// as host,port (DB_HOST may be host\instance); config/database.php
// defaults to localhost and 1433. pdo_sqlsrv's ODBC driver decides Encrypt
// unless config/database.php passes DB_ENCRYPT and
// DB_TRUST_SERVER_CERTIFICATE (its encrypt and trust_server_certificate
// are commented out as Laravel ships it): a value config/database.php is
// not seen to pass is taken only when it is not weaker than verifying. A
// DB_URL (DATABASE_URL) is merged over the values, as Laravel does.
func laravelSQLServer(root, file string, env map[string]string, p parts) (parts, error) {
	p.driver, p.socket = SQLServer, ""
	p.mssql.read = true
	if p.host == "" {
		p.host = "localhost"
	}
	var err error
	if p.host, p.port, p.mssql.note, err = sqlServerAddress(p.host, p.port); err != nil {
		return parts{}, fmt.Errorf("%s: %s: %w", file, dbHost, err)
	}
	if p.port == "" {
		p.port = "1433"
	}
	cfg, known := laravelDatabasePHP(root, file)
	for _, k := range []struct{ env, key string }{{"DB_ENCRYPT", "encrypt"}, {"DB_TRUST_SERVER_CERTIFICATE", "trustservercertificate"}} {
		v, set := env[k.env]
		if !set || strings.TrimSpace(v) == "" {
			continue
		}
		var s sqlServerTLS
		if err := s.set(k.key, v); err != nil {
			return parts{}, fmt.Errorf("%s: %s: %w", file, k.env, err)
		}
		weaker := s.encrypt == "false" || s.trust == "true"
		switch {
		case known && !strings.Contains(cfg, k.env):
			p.mssql.note = join(p.mssql.note, k.env+" is set, but config/database.php does not pass it on: DriftGate leaves it out, as Laravel does")
			continue
		case !known && weaker:
			p.mssql.note = join(p.mssql.note, k.env+" is set, but there is no config/database.php to show Laravel passes it on: DriftGate encrypts and verifies the server's certificate")
			continue
		}
		if err := p.mssql.set(k.key, v); err != nil {
			return parts{}, fmt.Errorf("%s: %s: %w", file, k.env, err)
		}
	}
	for _, k := range laravelURLs {
		u := strings.TrimSpace(env[k])
		if u == "" {
			continue
		}
		up, err := fromValue(u)
		if err != nil {
			return parts{}, fmt.Errorf("%s: %s: %w", file, k, err)
		}
		up.merge(p)
		if up.driver == SQLServer {
			up.mssql = mergeSQLServerTLS(up.mssql, p.mssql)
		}
		return up, nil
	}
	return p, nil
}

// laravelDatabasePHP is the text of config/database.php without its
// comments, for the Laravel project of the .env file; known is false when
// there is none to read.
func laravelDatabasePHP(root, file string) (text string, known bool) {
	raw, err := readFile(root, path.Join(path.Dir(strings.ReplaceAll(file, `\`, "/")), "config/database.php"))
	if err != nil {
		return "", false
	}
	var b strings.Builder
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		l := strings.TrimSpace(line)
		if inBlock {
			if i := strings.Index(l, "*/"); i >= 0 {
				inBlock, l = false, l[i+2:]
			} else {
				continue
			}
		}
		if strings.HasPrefix(l, "/*") {
			if !strings.Contains(l, "*/") {
				inBlock = true
			}
			continue
		}
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		if strings.HasPrefix(l, "#") {
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), true
}

// mergeSQLServerTLS fills what the TLS of a URL, s, leaves out from the
// separate values', t: the URL's settings win, as Laravel merges them.
func mergeSQLServerTLS(s, t sqlServerTLS) sqlServerTLS {
	for _, f := range [][2]*string{{&s.encrypt, &t.encrypt}, {&s.trust, &t.trust}, {&s.hostInCert, &t.hostInCert}, {&s.ca, &t.ca}, {&s.serverCert, &t.serverCert}, {&s.tlsMin, &t.tlsMin}} {
		if *f[0] == "" {
			*f[0] = *f[1]
		}
	}
	if s.client == "" {
		s.client = t.client
	}
	s.note = join(s.note, t.note)
	return s
}

// sqlServerSettings is the TLS of a SQL Server setting: what it was read
// with, or for one fromFields read without knowing its database (an
// ADO.NET connection string without a SQL Server key, the project's
// packages or the component naming SQL Server), its keys.
func (p parts) sqlServerSettings() (sqlServerTLS, error) {
	s := p.mssql
	if !s.read {
		s.read = true
		if err := s.setAll(p.raw); err != nil {
			return sqlServerTLS{}, err
		}
	}
	return s, nil
}

// encryption is what DriftGate connects with: Encrypt (the setting's, else
// its client's default, else true), and whether it trusts the server's
// certificate without verifying it. With Encrypt false the clients encrypt
// the login only, without verifying the certificate (go-mssqldb would
// verify it, and fail on a self-signed one): DriftGate trusts it too,
// unless the setting gives a certificate to verify it with.
func (s sqlServerTLS) encryption(host string) (encrypt string, trust bool) {
	encrypt = s.encrypt
	if encrypt == "" {
		encrypt = s.client
	}
	if encrypt == "" {
		encrypt = "true"
	}
	trust = s.trust == "true"
	if s.compose && s.trust == "" && isLoopback(host) {
		trust = true // the container's own, self-signed certificate, on this machine
	}
	switch {
	case encrypt == "strict": // strict always verifies
		trust = false
	case encrypt == "false" && s.ca == "" && s.serverCert == "":
		trust = true
	}
	return encrypt, trust
}

// isLoopback reports a host that is this machine's loopback.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// params writes the TLS of s as the go-mssqldb URL takes it. Encrypt is
// always written: go-mssqldb without it trusts any certificate. disable
// is never written: it is weaker than any client.
func (s sqlServerTLS) params(host string) (url.Values, error) {
	encrypt, trust := s.encryption(host)
	q := url.Values{}
	q.Set("encrypt", encrypt)
	if s.serverCert != "" && (s.ca != "" || s.hostInCert != "") {
		return nil, errSQLServerCertPair
	}
	if trust {
		q.Set("TrustServerCertificate", "true")
	}
	for k, v := range map[string]string{"hostNameInCertificate": s.hostInCert, "certificate": s.ca, "serverCertificate": s.serverCert, "tlsmin": s.tlsMin} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q, nil
}

// sqlServerDSN is the DSN of a SQL Server setting (parts.dsn): a sqlserver://
// URL of the go-mssqldb driver, without the database, with the TLS of the
// application's client (see params). Also reached for a component whose
// driver is sqlserver and whose setting does not say which database it is.
func (p parts) sqlServerDSN(where string) (string, error) {
	s, err := p.sqlServerSettings()
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	host, port := p.host, p.port
	if !p.mssql.read { // the generic reading: the host as the setting wrote it
		if host, port, _, err = sqlServerAddress(host, port); err != nil {
			return "", fmt.Errorf("%s: %w", where, err)
		}
	}
	switch {
	case p.socket != "":
		return "", fmt.Errorf("%s: %w", where, errSQLServerSocket)
	case p.user == "":
		return "", fmt.Errorf("%s: %w", where, errSQLServerNoUser)
	case strings.Contains(p.user, `\`):
		return "", fmt.Errorf("%s: %w", where, errSQLServerDomain)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = defaultPort(SQLServer)
	}
	q, err := s.params(host)
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	u := url.URL{Scheme: "sqlserver", User: userinfo(p.user, p.password), Host: net.JoinHostPort(host, port), RawQuery: q.Encode()}
	return u.String(), nil
}

// sqlServerNote is what Find notes about a SQL Server setting: how its
// server and its TLS are taken.
func (p parts) sqlServerNote() string {
	s, err := p.sqlServerSettings()
	if err != nil {
		return ""
	}
	note := s.note
	encrypt, trust := s.encryption(p.host)
	switch {
	case s.compose && trust && encrypt == "true":
		note = join(note, "DriftGate trusts the container's own certificate on this machine (encrypt=true, TrustServerCertificate=true)")
	case s.encrypt == "" && s.client == "":
		note = join(note, "the setting leaves Encrypt to the application's client, which DriftGate cannot tell: DriftGate encrypts and verifies the server's certificate; for a self-signed one, set Encrypt and TrustServerCertificate in the setting")
	}
	return note
}

// sqlServerClientOf records the Encrypt default of the client an ASP.NET
// project uses, for a setting read from file (a .csproj, or an
// appsettings*.json next to one project file): Microsoft.Data.SqlClient 4
// and later, and Entity Framework Core 7 and later (which use it), encrypt
// and verify; earlier ones and System.Data.SqlClient do not. A setting
// whose client is known (ODBC) keeps it.
func sqlServerClientOf(root, file string, p parts) parts {
	if p.mssql.client != "" || p.driver != SQLServer && p.driver != "" {
		return p
	}
	f := strings.ReplaceAll(file, `\`, "/")
	if !strings.HasSuffix(f, ".csproj") {
		real, err := localPath(root, f)
		if err != nil {
			return p
		}
		m, _ := filepath.Glob(filepath.Join(filepath.Dir(real), "*.csproj"))
		if len(m) != 1 {
			return p
		}
		f = path.Join(path.Dir(f), filepath.Base(m[0]))
	}
	raw, err := readFile(root, f)
	if err != nil {
		return p
	}
	p.mssql.client = csprojClient(string(raw))
	return p
}

// csprojClient is the Encrypt default of the SQL Server client a project
// file references (see sqlServerClientOf); "" when it cannot tell.
func csprojClient(csproj string) string {
	lc := strings.ToLower(csproj)
	for _, c := range []struct {
		pkg   string
		major int // the first version that encrypts by default
	}{{"microsoft.data.sqlclient", 4}, {"microsoft.entityframeworkcore.sqlserver", 7}} {
		if v, ok := packageMajor(lc, c.pkg); ok {
			if v >= c.major {
				return "true"
			}
			return "false"
		}
	}
	if strings.Contains(lc, `"system.data.sqlclient"`) {
		return "false"
	}
	return ""
}

// packageMajor is the major version of the PackageReference to pkg of a
// project file in lower case (Version="8.0.8" or <Version>); ok is false
// when it has none, or none with a plain version.
func packageMajor(lc, pkg string) (int, bool) {
	i := strings.Index(lc, `include="`+pkg+`"`)
	if i < 0 {
		return 0, false
	}
	elem := lc[i:]
	if end := strings.Index(elem, "</packagereference>"); end >= 0 {
		elem = elem[:end]
	} else if end := strings.Index(elem, "/>"); end >= 0 {
		elem = elem[:end]
	}
	var v string
	if _, rest, ok := strings.Cut(elem, `version="`); ok {
		v, _, _ = strings.Cut(rest, `"`)
	} else if _, rest, ok := strings.Cut(elem, "<version>"); ok {
		v, _, _ = strings.Cut(rest, "<")
	}
	v = strings.TrimLeft(strings.TrimSpace(v), "[(")
	n := 0
	for n < len(v) && '0' <= v[n] && v[n] <= '9' {
		n++
	}
	major, err := strconv.Atoi(v[:n])
	return major, err == nil
}
