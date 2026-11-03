package appconf

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Drivers.
const (
	MySQL     = "mysql" // MySQL and MariaDB
	Postgres  = "postgres"
	SQLite    = "sqlite"
	SQLServer = "sqlserver"
)

// detected is the driver and other (driverName) for a database of a
// DriftGate driver: the driver when appconf reads that database's
// settings (sqliteReady, sqlServerReady), else other, so the setting is
// noted as one DriftGate cannot use.
func detected(driver string) (string, string) {
	switch {
	case driver == SQLite && !sqliteReady:
		return "", "SQLite"
	case driver == SQLServer && !sqlServerReady:
		return "", "SQL Server"
	}
	return driver, ""
}

// defaultPort is the port a driver's server listens on by default.
func defaultPort(driver string) string {
	switch driver {
	case Postgres:
		return "5432"
	case SQLServer:
		return "1433"
	}
	return "3306"
}

// parts is a database setting taken apart.
type parts struct {
	driver   string // MySQL, Postgres, SQLite, SQLServer, or "" when the setting does not say
	other    string // another kind of database the setting names (Oracle, …)
	file     string // SQLite: the database file, relative to the project root with / (resolveSQLite)
	host     string
	port     string
	socket   string
	name     string
	user     string
	password string
	opts     options // TLS and the like
	form     string  // a DSN in the MySQL driver's own form, used as it is
	// raw holds the keys of a setting given as separate values or as a
	// connection string (fromFields), for a driver that takes settings
	// the shared ones leave out (SQL Server's Encrypt).
	raw fields
	// mssql is what a SQL Server setting says besides the address and the
	// credentials: its TLS and its client (sqlserver.go).
	mssql sqlServerTLS
}

// database is p without the password.
func (p parts) database() Database {
	return Database{Driver: p.driver, Host: p.host, Port: p.port, Name: p.name, User: p.user, Path: p.file}
}

// driverName maps the name a framework gives a database (a URL scheme, a
// Laravel connection, a Rails adapter, a Quarkus db-kind, a Django engine,
// a JDBC driver class) to a driver of DriftGate. other names a database
// DriftGate has no driver for; both are empty for a name it does not know.
func driverName(name string) (driver, other string) {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.IndexByte(n, '+'); i >= 0 { // SQLAlchemy: mysql+pymysql
		n = n[:i]
	}
	if i := strings.LastIndexByte(n, '.'); i >= 0 && strings.Contains(n, "backends") { // Django: django.db.backends.mysql
		n = n[i+1:]
	}
	n = strings.TrimPrefix(strings.TrimPrefix(n, "pdo-"), "pdo_")
	switch n {
	case "mysql", "mysql2", "mysqli", "mysql4", "mariadb", "trilogy", "percona":
		return MySQL, ""
	case "postgres", "postgresql", "pgsql", "postgis", "pg", "postgresql_psycopg2", "psycopg", "psycopg2", "npgsql", "timescaledb", "cockroachdb":
		return Postgres, ""
	case "":
		return "", ""
	case "sqlite", "sqlite3", "spatialite", "file":
		return detected(SQLite)
	case "sqlsrv", "sqlserver", "mssql", "dblib":
		return detected(SQLServer)
	case "oracle", "oci", "oci8":
		return "", "Oracle"
	}
	// A JDBC driver class or a Micronaut dialect.
	switch {
	case strings.Contains(n, "mysql"), strings.Contains(n, "mariadb"):
		return MySQL, ""
	case strings.Contains(n, "postgres"):
		return Postgres, ""
	case strings.Contains(n, "sqlserver"), strings.Contains(n, "sql_server"): // Django: sql_server.pyodbc
		return detected(SQLServer)
	case strings.Contains(n, "h2"), strings.Contains(n, "hsqldb"), strings.Contains(n, "derby"):
		return "", "an in-memory Java database"
	case strings.Contains(n, "sqlite"):
		return detected(SQLite)
	case strings.Contains(n, "oracle"):
		return "", "Oracle"
	}
	return "", ""
}

// restored names the databases DriftGate restores, the last one after
// last (" and ", " or "), for a message.
func restored(last string) string {
	names := []string{"MySQL", "MariaDB", "PostgreSQL"}
	if sqliteReady {
		names = append(names, "SQLite")
	}
	if sqlServerReady {
		names = append(names, "SQL Server")
	}
	return strings.Join(names[:len(names)-1], ", ") + last + names[len(names)-1]
}

// driverNames lists the drivers a setting can name, the last one after
// last (" or ", ", "), for a message.
func driverNames(last string) string {
	names := []string{MySQL, Postgres}
	if sqliteReady {
		names = append(names, SQLite)
	}
	if sqlServerReady {
		names = append(names, SQLServer)
	}
	return strings.Join(names[:len(names)-1], ", ") + last + names[len(names)-1]
}

// setDriver records the database a setting names.
func (p *parts) setDriver(name string) {
	if d, o := driverName(name); d != "" || o != "" {
		p.driver, p.other = d, o
	}
}

// constError is an error that is a constant, so none is built at package
// init.
type constError string

func (e constError) Error() string { return string(e) }

const errURL = constError("not a valid URL; percent-encode special characters such as @ : / % in the user name and password (the value is not shown)")

// Convert returns a DSN given in a form an application uses but DriftGate's
// drivers do not take, a JDBC URL or an ADO.NET connection string, as one
// they take (see Ref.DSN), as is libpq's host=… user=… form; any other DSN
// comes back as it is. A DSN of SQL Server is always rebuilt, also
// go-mssqldb's own URL, so the driver gets no key DriftGate has not read
// and an explicit encrypt. It serves a DSN of the environment or 1Password.
func Convert(dsn, driver string) (string, error) {
	v := strings.TrimSpace(dsn)
	if driver == SQLServer {
		// Every form, go-mssqldb's own URL too: the driver ignores a key it
		// does not know (Integrated Security), and without encrypt it
		// trusts any certificate, so the DSN is rebuilt from what DriftGate
		// reads.
		p, err := fromValue(v)
		if err != nil {
			return "", err
		}
		return p.dsn(driver, "the DSN")
	}
	if !strings.HasPrefix(v, "jdbc:") && !connString(v) && (strings.Contains(v, "://") || isDriverForm(v) || !strings.Contains(v, "=")) {
		return dsn, nil
	}
	p, err := fromValue(v)
	if err != nil {
		return "", err
	}
	return p.dsn(driver, "the DSN")
}

// fromValue reads a DSN as an application holds it: a URL (mysql://,
// postgresql://, jdbc:mysql://, pgsql://, …), the MySQL driver's own form
// (user:pass@tcp(host:3306)/db) or an ADO.NET connection string
// (Server=…;Database=…;Uid=…;Pwd=…).
func fromValue(v string) (parts, error) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "jdbc:")
	v = strings.TrimPrefix(v, "vertx-reactive:")
	v = strings.TrimPrefix(strings.TrimPrefix(v, "r2dbc:"), "pool:")
	if p, ok, err := sqlServerValue(v); ok {
		return p, err
	}
	switch {
	case connString(v):
		kv, _ := parseConnString(v)
		return fromConnString(kv)
	case strings.Contains(v, "://"):
		return fromURL(v)
	case isDriverForm(v):
		cfg, err := mysql.ParseDSN(v)
		if err != nil {
			return parts{}, errors.New("the MySQL driver cannot read the DSN (the value is not shown)")
		}
		p := parts{driver: MySQL, user: cfg.User, password: cfg.Passwd, name: cfg.DBName, form: v}
		if cfg.TLSConfig != "" { // kept when the host is replaced
			if err := p.opts.set("tls", cfg.TLSConfig); err != nil {
				return parts{}, err
			}
		}
		if cfg.Net == "unix" {
			p.socket = cfg.Addr
		} else if h, port, err := net.SplitHostPort(cfg.Addr); err == nil {
			p.host, p.port = h, port
		}
		return p, nil
	case strings.Contains(v, "="):
		// libpq's keyword form (host=h dbname=d user=u password=p) has no ;
		// between its pairs; a ; in it is a password's, when the string
		// read as ADO.NET names no server.
		if !strings.Contains(v, ";") {
			if lp, ok := parseLibpq(v); ok {
				return fromLibpq(lp)
			}
		}
		kv, err := parseConnString(v)
		if err == nil {
			p, err := fromConnString(kv)
			if err != errNoServer {
				return p, err
			}
		}
		// libpq's keyword form: host=h dbname=d user=u password=p
		if lp, ok := parseLibpq(v); ok {
			return fromLibpq(lp)
		}
		if err == nil {
			err = errNoServer
		}
		return parts{}, err
	case isSQLiteValue(v):
		return fromSQLiteValue(v)
	}
	return parts{}, errors.New("not a database URL, a MySQL DSN or a connection string (the value is not shown)")
}

// connString reports an ADO.NET connection string that names a server: it
// is read as one before anything else, as its password may hold :// or
// @tcp( or @/, which would make it look like a URL or the MySQL driver's
// form, and its user then hold a part of the password.
func connString(v string) bool {
	i := strings.IndexByte(v, '=')
	if i <= 0 || strings.ContainsAny(v[:i], ":/@?(") {
		return false
	}
	kv, err := parseConnString(v)
	if err != nil {
		return false
	}
	// A server with = or a space in it is libpq's keyword form read wrong.
	h, ok := kv.get(hostKeys)
	return ok && h != "" && !strings.ContainsAny(h, "= \t")
}

// isDriverForm reports the MySQL driver's own DSN form: user:pass@tcp(…)/
// where the user (before its :) holds no = or ;, which only a connection
// string would.
func isDriverForm(v string) bool {
	at := strings.LastIndexByte(v, '@')
	if at < 0 {
		return strings.HasPrefix(v, "tcp(") || strings.HasPrefix(v, "unix(")
	}
	user := v[:at]
	if i := strings.IndexByte(user, ':'); i >= 0 {
		user = user[:i]
	}
	if strings.ContainsAny(user, "=;") {
		return false
	}
	rest := v[at+1:]
	return strings.HasPrefix(rest, "tcp(") || strings.HasPrefix(rest, "unix(") || strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "tcp6(") || strings.HasPrefix(rest, "tcp4(")
}

func fromURL(v string) (parts, error) {
	u, err := url.Parse(v)
	if err != nil || u.Opaque != "" {
		return parts{}, errURL
	}
	var p parts
	p.setDriver(u.Scheme)
	if p.driver == "" && p.other == "" {
		return parts{}, fmt.Errorf("a %s:// URL names no database DriftGate knows (%s)", printable(u.Scheme), driverNames(", "))
	}
	switch p.driver {
	case "":
		return p, nil
	case SQLite:
		return fromSQLiteURL(v)
	case SQLServer:
		return fromSQLServerURL(v)
	}
	p.host, p.port = u.Hostname(), u.Port()
	p.user = u.User.Username()
	p.password, _ = u.User.Password()
	p.name = strings.Trim(u.Path, "/")
	q := u.Query()
	// JDBC and libpq take the credentials as parameters too.
	if p.user == "" {
		p.user = q.Get("user")
	}
	if p.password == "" {
		p.password = q.Get("password")
	}
	if h := q.Get("host"); p.driver == Postgres && strings.HasPrefix(h, "/") {
		p.socket = h
	}
	if sock := q.Get("socket"); sock != "" { // Prisma's MySQL socket
		if p.driver != MySQL || !strings.HasPrefix(sock, "/") {
			return parts{}, errors.New("the socket parameter of the URL is not the path of a MySQL socket")
		}
		p.socket = sock
	}
	if p.driver == MySQL && p.host == "" && strings.HasPrefix(u.Path, "/") && strings.Contains(u.Path, ".sock") {
		p.socket, p.name = u.Path, ""
	}
	for k, vs := range q {
		if err := p.opts.set(k, vs[len(vs)-1]); err != nil {
			return parts{}, err
		}
	}
	return p, nil
}

// printable is s when it holds only letters, digits and - + ., else a
// placeholder: a name read from a setting that may be quoted in an error.
func printable(s string) string {
	if s == "" || len(s) > 40 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-+.") != "" {
		return "such a"
	}
	return s
}

// field names, without - _ and spaces, in lower case.
var (
	urlKeys      = []string{"url", "dsn", "uri", "databaseurl", "jdbcurl", "connectionstring"}
	hostKeys     = []string{"host", "hostname", "server", "datasource", "address", "addr", "networkaddress"}
	portKeys     = []string{"port"}
	socketKeys   = []string{"socket", "unixsocket"}
	nameKeys     = []string{"database", "dbname", "name", "db", "initialcatalog", "databasename"}
	userKeys     = []string{"username", "user", "userid", "uid", "usr", "login"}
	passwordKeys = []string{"password", "pass", "pwd", "passwd"}
	driverKeys   = []string{"driver", "adapter", "engine", "dbkind", "driverclassname", "dialect", "kind", "type", "model", "connection", "provider"}
)

// fieldKey is a field name as the lists above hold it.
func fieldKey(k string) string {
	b := make([]byte, 0, len(k))
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c == '-' || c == '_' || c == ' ':
		case 'A' <= c && c <= 'Z':
			b = append(b, c+'a'-'A')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// fields is a setting given as separate values (host, port, …), by
// fieldKey.
type fields map[string]string

func (f fields) get(keys []string) (string, bool) {
	for _, k := range keys {
		if v, ok := f[k]; ok {
			return v, true
		}
	}
	return "", false
}

// fromFields reads a setting given as separate values; a URL among them
// comes first, and the values fill in what it leaves out (Spring's url,
// username and password).
func fromFields(f fields) (parts, error) {
	p := parts{raw: f}
	if v, ok := f.get(urlKeys); ok && strings.TrimSpace(v) != "" {
		var err error
		if p, err = fromValue(v); err != nil {
			return parts{}, err
		}
		p.raw = f
	}
	for _, k := range driverKeys { // Magento's engine is innodb, its model mysql4
		if v, ok := f[k]; ok && p.driver == "" && p.other == "" {
			p.setDriver(v)
		}
	}
	fill := func(dst *string, keys []string) {
		if v, ok := f.get(keys); ok && *dst == "" {
			*dst = v
		}
	}
	fill(&p.host, hostKeys)
	fill(&p.port, portKeys)
	fill(&p.socket, socketKeys)
	fill(&p.name, nameKeys)
	fill(&p.user, userKeys)
	fill(&p.password, passwordKeys)
	if p.driver == SQLServer || p.driver == "" && p.other == "" && p.form == "" && sqlServerServerForm(p.host) {
		return sqlServerFields(p, f)
	}
	for k, v := range f {
		if err := p.opts.set(k, v); err != nil {
			return parts{}, err
		}
	}
	if p.form == "" {
		if err := p.splitHost(); err != nil {
			return parts{}, err
		}
	}
	return p, nil
}

// merge fills what p leaves out from q: the URL's parts win, as Laravel
// and Rails merge a database URL over the separate settings.
func (p *parts) merge(q parts) {
	if p.driver == "" && p.other == "" {
		p.driver, p.other = q.driver, q.other
	}
	for _, f := range [][2]*string{{&p.host, &q.host}, {&p.port, &q.port}, {&p.name, &q.name}, {&p.user, &q.user}, {&p.password, &q.password}} {
		if *f[0] == "" {
			*f[0] = *f[1]
		}
	}
	if p.host == "" && p.socket == "" {
		p.socket = q.socket
	}
	if p.opts == (options{}) {
		p.opts = q.opts
	}
}

// splitHost takes a port written with the host (Magento's 127.0.0.1:3306,
// SQL Server's host,1433) or a socket written as the host.
func (p *parts) splitHost() error {
	switch {
	case strings.HasPrefix(p.host, "/"):
		p.socket, p.host = p.host, ""
	case strings.HasPrefix(p.host, "unix:"):
		p.socket, p.host = strings.TrimPrefix(p.host, "unix:"), ""
	}
	if i := strings.IndexByte(p.host, ','); i >= 0 { // several hosts: the first
		p.host = p.host[:i]
	}
	if h, port, err := net.SplitHostPort(p.host); err == nil {
		p.host = h
		if p.port == "" {
			p.port = port
		}
	}
	if p.port != "" && strings.Trim(p.port, "0123456789") != "" {
		return errors.New("the port is not a number")
	}
	return nil
}

// setHost replaces the host and port with host[:port], or with a socket
// given by its path.
func (p *parts) setHost(hostport string) error {
	if strings.HasPrefix(hostport, "/") { // a socket
		if strings.ContainsAny(hostport, "?#@ ") {
			return errors.New("must be host, host:port or the path of a socket")
		}
		p.host, p.port, p.socket, p.form = "", "", hostport, ""
		return nil
	}
	h, port, err := net.SplitHostPort(hostport)
	if err != nil {
		h, port = strings.Trim(hostport, "[]"), ""
	}
	if h == "" || strings.ContainsAny(h, "/@?# ") || strings.Trim(port, "0123456789") != "" {
		return errors.New("must be host, host:port or the path of a socket")
	}
	p.host, p.socket = h, ""
	if port != "" {
		p.port = port
	}
	p.form = "" // rebuilt from the parts, with their TLS
	return nil
}

// dsn builds the DSN of p for driver (the component's; "" takes the
// setting's own). where names the setting in an error.
func (p parts) dsn(driver, where string) (string, error) {
	if driver == "mariadb" {
		driver = MySQL
	}
	switch {
	case p.other != "":
		return "", fmt.Errorf("%s names %s; DriftGate restores %s", where, p.other, restored(" and "))
	case driver == "" && p.driver == "":
		return "", fmt.Errorf("%s does not say which database it is; set the driver (%s)", where, driverNames(" or "))
	case driver != "" && p.driver != "" && driver != p.driver:
		return "", fmt.Errorf("%s names a %s database, the driver is %s", where, p.driver, driver)
	case driver == "":
		driver = p.driver
	}
	switch driver {
	case SQLite:
		return p.sqliteDSN(where)
	case SQLServer:
		return p.sqlServerDSN(where)
	}
	if driver == MySQL && p.form != "" {
		return p.form, nil
	}
	q, err := p.opts.params(driver)
	if err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	if p.socket != "" {
		if driver == Postgres {
			q.Set("host", p.socket)
			if p.port != "" {
				q.Set("port", p.port)
			}
			u := url.URL{Scheme: "postgres", User: userinfo(p.user, p.password), Host: "", Path: "/", RawQuery: q.Encode()}
			return u.String(), nil
		}
		cfg := mysql.NewConfig()
		cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = p.user, p.password, "unix", p.socket
		return cfg.FormatDSN(), nil
	}
	host, port := p.host, p.port
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = defaultPort(driver)
	}
	u := url.URL{Scheme: driver, User: userinfo(p.user, p.password), Host: net.JoinHostPort(host, port), Path: "/", RawQuery: q.Encode()}
	return u.String(), nil
}

func userinfo(user, password string) *url.Userinfo {
	switch {
	case user == "" && password == "":
		return nil
	case password == "":
		return url.User(user)
	}
	return url.UserPassword(user, password)
}
