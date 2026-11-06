package appconf

import (
	"fmt"
	"strings"
)

// parseDotenv reads a .env file by the rules phpdotenv (Laravel, Symfony),
// Node's dotenv with dotenv-expand, godotenv and python-dotenv share:
//
//   - NAME=value lines, optionally after export, with spaces around the = ;
//   - # starts a comment line, and a comment after an unquoted value when a
//     space comes before it;
//   - '…' and `…` are literal; "…" takes the escapes \n \r \t \" \\ \$;
//     quoted values may span lines;
//   - ${NAME}, ${NAME:-default} and ${NAME-default} in unquoted and "…"
//     values take an earlier name of the file, else the environment (lookup);
//     a $ without braces is kept, as phpdotenv keeps it, so a password such
//     as pa$word is read as it is written.
//
// A line without = is skipped. No error quotes a value.
func parseDotenv(src []byte, lookup Lookup) (map[string]string, error) {
	out, _, err := parseDotenvLines(src, lookup)
	return out, err
}

// redefined is a name a .env sets twice to different values: the lines of
// the first value and of the other.
type redefined [2]int

// parseDotenvLines is parseDotenv that also reports the names set twice to
// different values: phpdotenv 2 (Laravel 5.5 to 5.7) keeps the first, later
// versions and the other libraries the last.
func parseDotenvLines(src []byte, lookup Lookup) (map[string]string, map[string]redefined, error) {
	s := string(src)
	s = strings.TrimPrefix(s, "\uFEFF")
	out := map[string]string{}
	lines := map[string]int{}
	var twice map[string]redefined
	get := func(name string) (string, bool) {
		if v, ok := out[name]; ok {
			return v, true
		}
		if lookup != nil {
			return lookup(name)
		}
		return "", false
	}
	line := 1
	for i := 0; i < len(s); {
		// Start of a line.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '\n' {
			line++
			i++
			continue
		}
		if s[i] == '#' {
			i = skipLine(s, i)
			continue
		}
		if strings.HasPrefix(s[i:], "export") && i+6 < len(s) && (s[i+6] == ' ' || s[i+6] == '\t') {
			i += 6
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
		}
		start := i
		for i < len(s) && isNameByte(s[i]) {
			i++
		}
		name := s[start:i]
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if name == "" || i >= len(s) || s[i] != '=' {
			i = skipLine(s, i) // not NAME=value: skipped, as the libraries do
			continue
		}
		i++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		at := line
		var b strings.Builder
		switch q := byte(0); {
		case i < len(s) && (s[i] == '\'' || s[i] == '`'):
			q = s[i]
			end := strings.IndexByte(s[i+1:], q)
			if end < 0 {
				return nil, nil, fmt.Errorf("line %d: the value of %s has no closing %c", at, name, q)
			}
			v := s[i+1 : i+1+end]
			line += strings.Count(v, "\n")
			b.WriteString(v)
			i += end + 2
		case i < len(s) && s[i] == '"':
			i++
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '"' {
					closed = true
					i++
					break
				}
				if c == '\n' {
					line++
				}
				if c == '\\' && i+1 < len(s) {
					switch e := s[i+1]; e {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case '"', '\\', '$', '\'':
						b.WriteByte(e)
					default:
						b.WriteByte('\\')
						b.WriteByte(e)
					}
					i += 2
					continue
				}
				if c == '$' {
					i += expand(s[i:], get, &b)
					continue
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, nil, fmt.Errorf("line %d: the value of %s has no closing \"", at, name)
			}
		default:
			end := i
			for end < len(s) && s[end] != '\n' && !(s[end] == '#' && end > i && (s[end-1] == ' ' || s[end-1] == '\t')) {
				end++
			}
			raw := strings.TrimRight(s[i:end], " \t\r")
			for j := 0; j < len(raw); {
				if raw[j] == '$' {
					j += expand(raw[j:], get, &b)
					continue
				}
				b.WriteByte(raw[j])
				j++
			}
			i = end
		}
		v := b.String()
		if old, ok := out[name]; ok && old != v {
			if twice == nil {
				twice = map[string]redefined{}
			}
			if _, ok := twice[name]; !ok {
				twice[name] = redefined{lines[name], at}
			}
		}
		if _, ok := lines[name]; !ok {
			lines[name] = at
		}
		out[name] = v
		i = skipLine(s, i) // a comment or anything after a closing quote
	}
	return out, twice, nil
}

func isNameByte(c byte) bool {
	return c == '_' || c == '.' || c == '-' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// skipLine returns the index after the end of the line i is on.
func skipLine(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j // the newline itself is counted by the caller
	}
	return len(s)
}

// expand writes the expansion of the ${…} s starts with to b and returns the
// length it read; a $ that starts no ${…} within the value (before a " or
// the end of the line) is written as it is, as phpdotenv does: pa${ss.
func expand(s string, get Lookup, b *strings.Builder) int {
	end := strings.IndexAny(s, "}\"\n")
	if len(s) < 2 || s[1] != '{' || end < 0 || s[end] != '}' {
		b.WriteByte('$')
		return 1
	}
	inner := s[2:end]
	name, def, hasDef, colon := inner, "", false, false
	if i := strings.IndexAny(inner, ":-"); i >= 0 {
		name = inner[:i]
		switch {
		case strings.HasPrefix(inner[i:], ":-"):
			def, hasDef, colon = inner[i+2:], true, true
		case inner[i] == '-':
			def, hasDef = inner[i+1:], true
		}
	}
	v, ok := get(name)
	if hasDef && (!ok || colon && v == "") {
		v = def
	}
	b.WriteString(v)
	return end + 1
}

// Laravel's keys (config/database.php from 5.5 on).
const (
	dbConnection = "DB_CONNECTION"
	dbHost       = "DB_HOST"
	dbPort       = "DB_PORT"
	dbDatabase   = "DB_DATABASE"
	dbUsername   = "DB_USERNAME"
	dbPassword   = "DB_PASSWORD"
	dbSocket     = "DB_SOCKET"
)

// laravel reports whether a .env sets Laravel's DB_* keys, or the type
// key of a Node setting (nodeEnvs).
func laravel(env map[string]string) bool {
	for _, k := range []string{dbConnection, dbHost, dbDatabase, dbUsername} {
		if _, ok := env[k]; ok {
			return true
		}
	}
	return nodeEnvOf(env) != nil
}

// nodeEnv names the keys of a Node application's database setting in a
// .env: the type of database (TypeORM's type, MikroORM's and Sequelize's
// dialect, as the application passes it on), the server and the
// credentials.
type nodeEnv struct {
	typ, host, port, user, password string
	names                           []string // the database, the first one set
}

// nodeEnvs are the settings of NestJS projects with TypeORM, MikroORM or
// Sequelize: DB_TYPE with the DB_* keys of Laravel (the template most
// follow), TypeORM's own TYPEORM_* (ormconfig from the environment,
// before 0.3), and DATABASE_* (DATABASE_TYPE, as boilerplates name it).
var nodeEnvs = []nodeEnv{
	{typ: "DB_TYPE", host: dbHost, port: dbPort, user: dbUsername, password: dbPassword, names: []string{dbDatabase, "DB_NAME"}},
	{typ: "TYPEORM_CONNECTION", host: "TYPEORM_HOST", port: "TYPEORM_PORT", user: "TYPEORM_USERNAME", password: "TYPEORM_PASSWORD", names: []string{"TYPEORM_DATABASE"}},
	{typ: "DATABASE_TYPE", host: "DATABASE_HOST", port: "DATABASE_PORT", user: "DATABASE_USERNAME", password: "DATABASE_PASSWORD", names: []string{"DATABASE_NAME"}},
}

// nodeEnvOf is the Node setting of a .env without DB_CONNECTION (which
// makes it Laravel's): the first of nodeEnvs whose type names a database
// (a DATABASE_TYPE of something else is no database setting), or nil.
func nodeEnvOf(env map[string]string) *nodeEnv {
	if strings.TrimSpace(env[dbConnection]) != "" {
		return nil
	}
	for i, n := range nodeEnvs {
		if d, o := driverName(env[n.typ]); d != "" || o != "" {
			return &nodeEnvs[i]
		}
	}
	return nil
}

// keys lists the keys of the setting.
func (n *nodeEnv) keys() []string {
	return append([]string{n.typ, n.host, n.port, n.user, n.password}, n.names...)
}

// read reads the setting of a .env: its type names the driver (an SQLite
// file is the database key, relative to the project root, where a Node
// application usually runs), and SQL Server is tedious's, which encrypts
// and verifies unless the application's code says otherwise.
func (n *nodeEnv) read(file string, env map[string]string) (parts, error) {
	p := parts{host: env[n.host], port: env[n.port], user: env[n.user], password: env[n.password]}
	for _, k := range n.names {
		if v := strings.TrimSpace(env[k]); v != "" {
			p.name = v
			break
		}
	}
	p.setDriver(env[n.typ])
	switch {
	case p.driver == SQLite:
		if p.name == "" {
			return parts{}, fmt.Errorf("%s: %s is sqlite, and no %s names the file", file, n.typ, strings.Join(n.names, " or "))
		}
		if p.name == ":memory:" {
			return parts{}, fmt.Errorf("%s: %s: %w", file, n.names[0], errMemory)
		}
		return parts{driver: SQLite, name: p.name}, nil
	case p.driver == SQLServer:
		p.mssql.read, p.mssql.client = true, "true"
		p.mssql.note = "tedious encrypts and verifies the server's certificate unless the application's code says otherwise, which DriftGate does not read; for a local server with a self-signed certificate, give state: a DSN of its own: dsn: { env: NAME } with sqlserver://…?encrypt=true&TrustServerCertificate=true"
	}
	if err := p.splitHost(); err != nil {
		return parts{}, fmt.Errorf("%s: %s: %w", file, n.port, err)
	}
	return p, nil
}

// laravelURLs are the keys Laravel takes a database URL from: DB_URL (11
// and later), DATABASE_URL (5.8 to 10).
var laravelURLs = []string{"DB_URL", "DATABASE_URL"}

// readDotenv reads the setting of a .env file: the URL in key, or, without
// a key, Laravel's: DB_URL or DATABASE_URL when set (their parts win, as
// Laravel merges them), with DB_CONNECTION, DB_HOST, DB_PORT, DB_DATABASE,
// DB_USERNAME, DB_PASSWORD and DB_SOCKET (MySQL only), and
// MYSQL_ATTR_SSL_CA (TLS verified with that CA). A key it reads that
// the file sets twice to different values is an error: Laravel 5.5 to 5.7
// take the first, later versions the last.
func readDotenv(root, file, key string, lookup Lookup) (parts, error) {
	raw, err := readFile(root, file)
	if err != nil {
		return parts{}, err
	}
	env, twice, err := parseDotenvLines(raw, lookup)
	if err != nil {
		return parts{}, fmt.Errorf("%s: %w", file, err)
	}
	node := nodeEnvOf(env)
	used := []string{key}
	switch {
	case key == "" && node != nil:
		used = node.keys()
	case key == "":
		used = append([]string{dbConnection, dbHost, dbPort, dbDatabase, dbUsername, dbPassword, dbSocket}, laravelURLs...)
	}
	for _, k := range used {
		if l, ok := twice[k]; ok && node != nil && key == "" {
			return parts{}, fmt.Errorf("%s: %s is set twice to different values (lines %d and %d): keep one", file, k, l[0], l[1])
		} else if ok {
			return parts{}, fmt.Errorf("%s: %s is set twice to different values (lines %d and %d); Laravel 5.5 to 5.7 take the first, later versions the last: keep one", file, k, l[0], l[1])
		}
	}
	if key != "" {
		v, ok := env[key]
		if !ok || strings.TrimSpace(v) == "" {
			return parts{}, fmt.Errorf("%s: %s is not set", file, key)
		}
		return fromValue(v)
	}
	if node != nil {
		return node.read(file, env)
	}
	if d, _ := driverName(env[dbConnection]); d == SQLite {
		return laravelSQLite(file, env)
	}
	if !laravel(env) {
		return parts{}, fmt.Errorf("%s: no DB_HOST, DB_DATABASE or DB_USERNAME (name the key of a URL with key:)", file)
	}
	p := parts{host: env[dbHost], port: env[dbPort], name: env[dbDatabase], user: env[dbUsername], password: env[dbPassword], socket: env[dbSocket]}
	p.setDriver(env[dbConnection])
	if p.driver == SQLServer {
		return laravelSQLServer(root, file, env, p)
	}
	if p.driver == Postgres { // Laravel's pgsql connection has no socket
		p.socket = ""
	}
	if p.host == "" && p.socket == "" {
		p.host = "127.0.0.1" // config/database.php: env('DB_HOST', '127.0.0.1')
	}
	if ca := env["MYSQL_ATTR_SSL_CA"]; ca != "" && p.driver != Postgres {
		// config/database.php (6 and later): PDO::MYSQL_ATTR_SSL_CA, which
		// verifies the server's certificate with it.
		p.opts.ca = ca
	}
	if err := p.splitHost(); err != nil {
		return parts{}, fmt.Errorf("%s: %s: %w", file, dbPort, err)
	}
	for _, k := range laravelURLs {
		if u := strings.TrimSpace(env[k]); u != "" {
			up, err := fromValue(u)
			if err != nil {
				return parts{}, fmt.Errorf("%s: %s: %w", file, k, err)
			}
			up.merge(p)
			return up, nil
		}
	}
	return p, nil
}
