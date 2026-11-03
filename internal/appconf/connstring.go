package appconf

import (
	"fmt"
	"slices"
	"strings"
)

// parseConnString reads an ADO.NET connection string by the rules of
// DbConnectionStringBuilder: key=value pairs separated by ;, a key's case
// and surrounding spaces ignored and == standing for a = in it, a value
// quoted with " or ' when it holds a ; or the quote (doubled inside). Keys
// come back as fieldKey makes them (User Id and UserID are userid). No
// error quotes the string, not even a key: when a password holds a ; the
// rest of it reads as a key.
func parseConnString(s string) (fields, error) {
	out := fields{}
	pair := 0
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ';' || isSpace(s[i])) {
			i++
		}
		if i >= len(s) {
			break
		}
		pair++
		// The key, up to a single =.
		var key strings.Builder
		for {
			if i >= len(s) {
				return nil, fmt.Errorf("connection string: pair %d has no =", pair)
			}
			if s[i] == '=' {
				if i+1 < len(s) && s[i+1] == '=' {
					key.WriteByte('=')
					i += 2
					continue
				}
				i++
				break
			}
			if s[i] == ';' {
				return nil, fmt.Errorf("connection string: pair %d has no =", pair)
			}
			key.WriteByte(s[i])
			i++
		}
		k := fieldKey(strings.TrimSpace(key.String()))
		if k == "" {
			return nil, fmt.Errorf("connection string: pair %d has no key", pair)
		}
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		var v string
		if i < len(s) && (s[i] == '"' || s[i] == '\'') {
			q := s[i]
			i++
			var b strings.Builder
			for {
				if i >= len(s) {
					return nil, fmt.Errorf("connection string: the value of pair %d has no closing quote", pair)
				}
				if s[i] == q {
					if i+1 < len(s) && s[i+1] == q {
						b.WriteByte(q)
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
				return nil, fmt.Errorf("connection string: text after the quoted value of pair %d", pair)
			}
		} else {
			j := strings.IndexByte(s[i:], ';')
			if j < 0 {
				j = len(s) - i
			}
			v = strings.TrimSpace(s[i : i+j])
			i += j
		}
		out[k] = v // the last one counts, as in .NET
	}
	return out, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// fromConnString reads the keys of MySqlConnector, MySql.Data and Npgsql;
// a connection string with the keys or the server forms only SQL Server or
// SQLite take is read as theirs (sqlServerConnString, sqliteConnString).
func fromConnString(kv fields) (parts, error) {
	// SQLite first: a Windows path of its file (C:\data\app.db) is not
	// SQL Server's host\instance.
	if p, ok, err := sqliteConnString(kv); ok {
		return p, err
	}
	if p, ok, err := sqlServerConnString(kv); ok {
		return p, err
	}
	if _, ok := kv.get(hostKeys); !ok {
		return parts{}, errNoServer
	}
	// MySqlConnector takes Npgsql's key names too (Host, Username), so the
	// keys do not tell the two apart: the project's packages or the
	// component's driver do.
	return fromFields(kv)
}

// errNoServer refuses a connection string without a server: it is not
// one DriftGate reads right (a ; in a password, another format), and a
// default address would hide that.
const errNoServer = constError("the connection string names no server (Server=, Host=) (the value is not shown)")

// libpqKeys are the keywords of libpq's connection strings.
var libpqKeys = []string{"host", "hostaddr", "port", "dbname", "user", "password", "passfile", "channel_binding", "connect_timeout",
	"client_encoding", "options", "application_name", "fallback_application_name", "keepalives", "keepalives_idle",
	"keepalives_interval", "keepalives_count", "tcp_user_timeout", "replication", "gssencmode", "sslmode", "requiressl",
	"sslnegotiation", "sslcompression", "sslcert", "sslkey", "sslpassword", "sslcertmode", "sslrootcert", "sslcrl",
	"sslcrldir", "sslsni", "requirepeer", "ssl_min_protocol_version", "ssl_max_protocol_version", "krbsrvname",
	"gsslib", "gssdelegation", "service", "target_session_attrs", "load_balance_hosts"}

// parseLibpq reads libpq's keyword form: keyword = value pairs separated by
// spaces, a value in '…' when it holds spaces, \ and \' escaped. It is
// that form only when every keyword is one of libpq's.
func parseLibpq(s string) (fields, bool) {
	out := fields{}
	i := 0
	for {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			return out, len(out) > 0
		}
		start := i
		for i < len(s) && s[i] != '=' && !isSpace(s[i]) {
			i++
		}
		key := s[start:i]
		if !slices.Contains(libpqKeys, key) {
			return nil, false
		}
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return nil, false
		}
		i++
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		var b strings.Builder
		quoted := i < len(s) && s[i] == '\''
		if quoted {
			i++
		}
		for ; i < len(s); i++ {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if quoted && c == '\'' || !quoted && isSpace(c) {
				break
			}
			b.WriteByte(c)
		}
		if quoted {
			if i >= len(s) {
				return nil, false
			}
			i++
		}
		out[fieldKey(key)] = b.String()
	}
}

// fromLibpq reads a libpq keyword string, which is PostgreSQL's.
func fromLibpq(f fields) (parts, error) {
	if h, ok := f["hostaddr"]; ok && f["host"] == "" {
		f["host"] = h
	}
	if f["host"] == "" {
		return parts{}, errNoServer
	}
	p, err := fromFields(f)
	p.driver = Postgres
	return p, err
}
