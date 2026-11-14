// Package dbx holds what the state components (internal/state) and the data
// module (internal/data) both do with MySQL/MariaDB and PostgreSQL servers:
// connecting, quoting names, setting up a session, telling MariaDB from
// MySQL, reading SHOW CREATE, filtering PostgreSQL's system schemas,
// checking that a database is on a host DriftGate may rewrite, trying a
// MySQL step again when the server stopped it for a lock, finding the
// sequences a Galera node refuses to create, and running a PostgreSQL
// statement that needs a database to have no other sessions. Reading a DSN
// stays in internal/dbdsn, which keeps every error free of the DSN.
//
// Where the two callers differ on purpose, the difference is a parameter
// here, with the reason at the parameter.
package dbx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib" // also registers the "pgx" driver

	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/httpx"
)

// Querier is a *sql.DB, *sql.Conn or *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// QuoteMySQL quotes an identifier for MySQL and MariaDB.
func QuoteMySQL(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// QuotePostgres quotes an identifier for PostgreSQL.
func QuotePostgres(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// DialFunc connects to a database server: network is tcp or unix, addr
// host:port or the socket's path.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// OpenMySQL connects to a MySQL or MariaDB server with the configuration
// dbdsn.MySQLConfig makes of the DSN, and db as the default database (""
// for none). That configuration interpolates parameters on the client, so
// DriftGate's own queries never prepare a statement on the server: the
// state components' write check leaves Com_stmt_execute to the
// application. A dial function other than nil makes every connection
// (AllowDial).
func OpenMySQL(dsn, db string, dial DialFunc) (*sql.DB, error) {
	return OpenMySQLWith(dsn, db, dial, MySQLOpen{})
}

// MySQLOpen are the options of OpenMySQLWith.
type MySQLOpen struct {
	// MultiStatements lets one query hold several statements, and turns
	// the client-side interpolation of parameters off: a query on such a
	// connection takes no parameters, so nothing is ever interpolated into
	// a text the server splits into statements, and none is prepared on
	// the server. Only quoted names (QuoteMySQL) go into its queries.
	MultiStatements bool
	// Init runs once on every new connection, before the pool hands it
	// out: it sets the session up for good, so that is not done again each
	// time the connection is taken from the pool. Its error closes the
	// connection.
	Init func(ctx context.Context, c driver.ExecerContext, conn driver.Conn) error
}

// OpenMySQLWith is OpenMySQL with options.
func OpenMySQLWith(dsn, db string, dial DialFunc, o MySQLOpen) (*sql.DB, error) {
	cfg, err := dbdsn.MySQLConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.DBName = db
	if dial != nil {
		cfg.DialFunc = dial
	}
	if o.MultiStatements {
		cfg.MultiStatements = true
		cfg.InterpolateParams = false
	}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(mysqlConnector{conn, cfg.Addr, o.Init}), nil
}

// mysqlConnector turns a panic of the MySQL driver while it connects into
// an error: the driver reads the server's first packet without checking
// its length, and a server that is not MySQL (its X protocol, on port
// 33060) makes it slice past the end. It runs init on every new connection.
type mysqlConnector struct {
	driver.Connector
	addr string
	init func(ctx context.Context, c driver.ExecerContext, conn driver.Conn) error
}

func (c mysqlConnector) Connect(ctx context.Context) (conn driver.Conn, err error) {
	defer func() {
		if recover() != nil {
			conn, err = nil, fmt.Errorf("the server at %s did not answer as MySQL; is it the X protocol port (33060) instead of the classic one (3306)?", c.addr)
		}
	}()
	conn, err = c.Connector.Connect(ctx)
	if err != nil || c.init == nil {
		return conn, err
	}
	ex, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()
		return nil, errors.New("the MySQL driver cannot run a statement on a new connection")
	}
	if err := c.init(ctx, ex, conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// PostgresDSN is the connection string for the pgx driver: the postgres://
// URL with the database db ("" keeps the URL's own, default postgres).
func PostgresDSN(dsn, db string) (string, error) {
	u, err := dbdsn.PostgresURL(dsn)
	if err != nil {
		return "", err
	}
	if db != "" {
		u.Path = "/" + db
	}
	return u.String(), nil
}

// OpenPostgres connects to a PostgreSQL server with the URL PostgresDSN
// makes. A dial function other than nil makes every connection, and gets
// the host by name: pgx's own lookup is turned off, so the name is
// resolved once, by the dial function, which checks the address it
// connects to.
//
// Every statement runs in one round trip (pgx's QueryExecModeExec): it is
// sent with its parameters, which stay bind parameters whose types the
// server infers, as it did when the statement was prepared first (two
// round trips; each database/sql pool is short-lived, so the statement
// cache never saved one). Parameters go as text: a string as it is, a
// []string as an array literal pgx quotes. Results come as text too;
// DriftGate's queries read strings, counts and booleans, which read the
// same in either format.
func OpenPostgres(dsn, db string, dial DialFunc) (*sql.DB, error) {
	cs, err := PostgresDSN(dsn, db)
	if err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(cs)
	if err != nil {
		return nil, errors.New("the PostgreSQL DSN cannot be read") // pgx's error quotes the DSN
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	if dial != nil {
		cfg.DialFunc = pgconn.DialFunc(dial)
		cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	}
	return stdlib.OpenDB(*cfg), nil
}

// AllowDial returns a dial function that connects only to a host one of
// the allow lists (config.Config.AllowHostLists) allows, as AllowHost
// decides: a local socket, a host named in a list, or a name whose
// address is in an allowed range. The address is checked when the
// connection is made, and the connection goes to the checked address, so
// a name that resolves elsewhere after AllowHost (DNS rebinding) is not
// reached. hint names the setting that turns the check off.
func AllowDial(lists [][]string, hint string) (DialFunc, error) {
	if len(lists) == 0 {
		lists = [][]string{httpx.LocalHosts}
	}
	var allows []*httpx.Allow
	for _, l := range lists {
		a, err := httpx.NewAllow(l, false)
		if err != nil {
			return nil, err
		}
		a.LookupIP = lookupIP
		allows = append(allows, a)
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "unix" {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		var err error
		for _, a := range allows {
			var c net.Conn
			if c, err = a.Dial(ctx, network, addr); err == nil || !errors.Is(err, httpx.ErrHostNotAllowed) {
				return c, err
			}
		}
		host, _, _ := net.SplitHostPort(addr)
		return nil, fmt.Errorf("%w: the database host %s resolves to no allowed address; set %s", httpx.ErrHostNotAllowed, host, hint)
	}, nil
}

// lookupIP replaces the system resolver of AllowHost and AllowDial (tests).
var lookupIP func(ctx context.Context, network, host string) ([]net.IP, error)

// Servers a MySQL DSN may reach.
const (
	FlavorMySQL   = "mysql"
	FlavorMariaDB = "mariadb"
)

// IsMariaDB tells MariaDB from MySQL by the text of VERSION().
func IsMariaDB(version string) bool { return strings.Contains(strings.ToLower(version), "mariadb") }

// Flavor reads which server a MySQL connection reached: FlavorMySQL or
// FlavorMariaDB.
func Flavor(ctx context.Context, q Querier) (string, error) {
	var version string
	if err := q.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		return "", err
	}
	if IsMariaDB(version) {
		return FlavorMariaDB, nil
	}
	return FlavorMySQL, nil
}

// MySQLSession sets up a MySQL or MariaDB connection for copying data as
// it is: no checks of references and uniqueness while rows are loaded
// (they were valid where they came from).
type MySQLSession struct {
	// UTF8UTC sets utf8mb4 and UTC for TIMESTAMP columns, so values read
	// the same on every server. The data module sets it; the state
	// components do not, as they copy rows inside one server and hash the
	// text the server prints in its own session settings.
	UTF8UTC bool
	// SQLMode is the session's sql_mode. The data module wants a value
	// that does not fit its column to be an error; the state components
	// want only NO_AUTO_VALUE_ON_ZERO, so old values (zero dates) and a 0
	// in an AUTO_INCREMENT column are copied back as they were.
	SQLMode string
	// FreshStats turns MySQL 8's cache of table statistics off for the
	// session (information_schema_stats_expiry = 0), so UPDATE_TIME and
	// sizes are current; MariaDB does not know the variable and is left as
	// it is. Only the state components read those statistics.
	FreshStats bool
}

// Statements are the SET statements of the session, also the head of a
// dump that loads into a server the same way (FreshStats is not among them).
func (s MySQLSession) Statements() []string {
	var out []string
	if s.UTF8UTC {
		out = append(out, "SET NAMES utf8mb4", "SET time_zone = '+00:00'")
	}
	for _, a := range s.Assignments() {
		out = append(out, "SET "+a)
	}
	return out
}

// Assignments are the session's settings other than UTF8UTC's (SET NAMES
// cannot share a SET statement) and FreshStats, as one SET statement takes
// them, separated by commas.
func (s MySQLSession) Assignments() []string {
	return []string{
		"foreign_key_checks = 0",
		"unique_checks = 0",
		"sql_mode = '" + strings.ReplaceAll(s.SQLMode, "'", "''") + "'",
	}
}

// Apply runs the session's statements on c.
func (s MySQLSession) Apply(ctx context.Context, c *sql.Conn) error {
	for _, q := range s.Statements() {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if s.FreshStats {
		c.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
	}
	return nil
}

// ShowCreate runs SHOW CREATE kind (TABLE, VIEW, SEQUENCE, TRIGGER…) on
// db.name and returns its columns by name.
//
// missingOK decides what an object that does not exist is (errors 1146 and
// 1360, or no row): with it, nil and no error; without it, an error. The
// state components pass true: they compare the working and the pristine
// database, and an object one of them lacks is a difference they restore,
// not a failure. The data module passes false: it reads objects it has just
// listed, and one that is gone means the source changed under the read,
// which must not end in a copy without it.
func ShowCreate(ctx context.Context, q Querier, kind, db, name string, missingOK bool) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, ShowCreateQuery(kind, db, name))
	if err != nil {
		if missingOK && ShowCreateMissing(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("SHOW CREATE %s %s: %w", kind, name, err)
	}
	defer rows.Close()
	out, err := ShowCreateRow(rows)
	if err != nil {
		return nil, err
	}
	if out == nil {
		if missingOK {
			return nil, nil
		}
		return nil, fmt.Errorf("SHOW CREATE %s %s: no row", kind, name)
	}
	return out, rows.Err()
}

// showCreateColumn is the column of SHOW CREATE that holds the statement.
var showCreateColumn = map[string]string{"TABLE": "Create Table", "VIEW": "Create View", "SEQUENCE": "Create Table"}

// ShowCreateSQL returns the CREATE statement of a table, view or sequence;
// "" for one that does not exist with missingOK (see ShowCreate).
func ShowCreateSQL(ctx context.Context, q Querier, kind, db, name string, missingOK bool) (string, error) {
	r, err := ShowCreate(ctx, q, kind, db, name, missingOK)
	if err != nil {
		return "", err
	}
	return ShowCreateStatement(kind, name, r)
}

// ShowCreateStatement is the CREATE statement in a row of SHOW CREATE kind
// (ShowCreate's columns by name): "" for nil, an object that does not exist.
func ShowCreateStatement(kind, name string, r map[string]string) (string, error) {
	if r == nil {
		return "", nil
	}
	col := showCreateColumn[kind]
	def, ok := r[col]
	if !ok {
		return "", fmt.Errorf("SHOW CREATE %s %s: no column %q", kind, name, col)
	}
	return def, nil
}

// ShowCreateQuery is the statement ShowCreate runs.
func ShowCreateQuery(kind, db, name string) string {
	return "SHOW CREATE " + kind + " " + QuoteMySQL(db) + "." + QuoteMySQL(name)
}

// ShowCreateMissing reports whether err is SHOW CREATE's error for an
// object that does not exist (no such table or view, no such trigger).
func ShowCreateMissing(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1146 || me.Number == 1360)
}

// ShowCreateRow reads the current result set of SHOW CREATE as ShowCreate
// returns it: its columns by name, or nil when it has no row.
func ShowCreateRow(rows *sql.Rows) (map[string]string, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, rows.Err()
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, col := range cols {
		out[col] = vals[i].String
	}
	return out, nil
}

// MySQLDatabaseExists tells whether the server has the database.
func MySQLDatabaseExists(ctx context.Context, q Querier, db string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", db).Scan(&n)
	return n > 0, err
}

// MySQLCharset reads the default character set and collation of a
// database; sql.ErrNoRows when it does not exist.
func MySQLCharset(ctx context.Context, q Querier, db string) (charset, collation string, err error) {
	err = q.QueryRowContext(ctx, "SELECT DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", db).Scan(&charset, &collation)
	return charset, collation, err
}

// MySQLObject is a table, view or sequence: TABLE_TYPE is BASE TABLE,
// VIEW, SEQUENCE (MariaDB) or SYSTEM VERSIONED (MariaDB).
type MySQLObject struct{ Name, Type string }

// MySQLObjects lists the tables, views and sequences of a database by name.
func MySQLObjects(ctx context.Context, q Querier, db string) ([]MySQLObject, error) {
	rows, err := q.QueryContext(ctx, "SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MySQLObject
	for rows.Next() {
		var o MySQLObject
		if err := rows.Scan(&o.Name, &o.Type); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MySQLProbe reports a name of the short-lived objects the state module
// makes to try whether a login may use a DEFINER: dgt_probe_ and 16
// lower-case hexadecimal digits. Such an object is never the application's.
func MySQLProbe(name string) bool {
	const prefix = "dgt_probe_"
	if len(name) != len(prefix)+16 || !strings.HasPrefix(name, prefix) {
		return false
	}
	return strings.Trim(name[len(prefix):], "0123456789abcdef") == ""
}

// MySQLTrigger is a trigger and its place among the triggers of its table.
type MySQLTrigger struct {
	Name, Table, Timing, Event string
	Order                      int
	Definer                    string // user@host, as CURRENT_USER() writes it
}

// MySQLTriggers lists the triggers of a database in firing order per
// table, the order they are created again in.
func MySQLTriggers(ctx context.Context, q Querier, db string) ([]MySQLTrigger, error) {
	rows, err := q.QueryContext(ctx, `SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION, ACTION_ORDER, DEFINER
		FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?
		ORDER BY EVENT_OBJECT_TABLE, ACTION_TIMING, EVENT_MANIPULATION, ACTION_ORDER`, db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MySQLTrigger
	for rows.Next() {
		var t MySQLTrigger
		if err := rows.Scan(&t.Name, &t.Table, &t.Timing, &t.Event, &t.Order, &t.Definer); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PostgresUserSchema is an SQL condition that the schema named in col is
// one of the database itself: not pg_catalog, information_schema or another
// schema of the system (pg_toast, pg_temp_N, pg_toast_temp_N; PostgreSQL
// keeps the prefix pg_ for them, and the underscore is escaped, so pgxtemp
// is a user schema). The state components and the data module both use it.
func PostgresUserSchema(col string) string {
	return "(" + col + " NOT IN ('pg_catalog', 'information_schema') AND " + col + " NOT LIKE 'pg\\_%')"
}

// PostgresNotExtension is an SQL condition that the object oid of the
// catalog class (pg_class, pg_proc…) does not belong to an extension.
//
// Only the data module uses it: it writes CREATE EXTENSION, which makes the
// extension's objects again, so copying them too would fail. The state
// components fingerprint extension objects with the rest: their rows can
// change (PostGIS's spatial_ref_sys), and a restore copies the whole
// database anyway.
func PostgresNotExtension(oid, class string) string {
	return "NOT EXISTS (SELECT 1 FROM pg_depend e WHERE e.objid = " + oid + " AND e.classid = '" + class + "'::regclass AND e.deptype = 'e')"
}

// AllowHost allows a local socket ("") and a host that one of the allow
// lists (config.Config.AllowHostLists) allows.
// Without any list only the local system is allowed, not the private ranges
// an HTTP target may use: a database on a private address may well be
// production, and both a state restore and a data target rewrite it. hint
// names the setting that turns the check off for the caller.
func AllowHost(ctx context.Context, lists [][]string, host, hint string) error {
	if host == "" {
		return nil
	}
	if len(lists) == 0 {
		lists = [][]string{httpx.LocalHosts}
	}
	var err error
	for _, l := range lists {
		var allow *httpx.Allow
		if allow, err = httpx.NewAllow(l, false); err == nil {
			allow.LookupIP = lookupIP
			if err = allow.Check(ctx, host); err == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("the database host %s is not allowed (%v); it must be a copy on this machine or on a host target.allow_hosts names, or set %s", host, err, hint)
}

// AllowHosts checks every host the driver may connect to with the DSN
// (several hosts, fallbacks, PostgreSQL's host= parameter) with AllowHost.
func AllowHosts(ctx context.Context, driver, dsn string, lists [][]string, hint string) error {
	hosts, err := dbdsn.Hosts(driver, dsn)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		if err := AllowHost(ctx, lists, host, hint); err != nil {
			return err
		}
	}
	return nil
}

// MySQLCreateTable returns the statement that makes a table again from its
// SHOW CREATE TABLE (a state copy, a dump), so the new table prints the
// same. MySQL 8 marks a column
// whose collation a statement names, and prints it as CHARACTER SET cs
// COLLATE x; a column that takes the table's collation without naming it
// prints as COLLATE x when x is not its character set's default. Run as
// printed, that COLLATE names the collation: the copy's column then prints
// with CHARACTER SET, and its fingerprint differs from the source's
// (measured on MySQL 8.0 and 8.4; a restore left the table different from
// the pristine copy's record). So a column's COLLATE that is the table's
// own collation, without a CHARACTER SET before it, is left out: the column
// takes the same collation from the table. Nothing in quotes, backquotes or
// parentheses (enum values, defaults, comments, expressions) is changed.
// MariaDB records no such mark: where it prints such a COLLATE, leaving it
// out changes nothing either.
func MySQLCreateTable(def string) string {
	type word struct {
		s          string
		start, end int
		depth      int
	}
	var words []word
	depth := 0
	for i := 0; i < len(def); {
		switch ch := def[i]; {
		case ch == '\'' || ch == '"' || ch == '`':
			// SHOW CREATE TABLE doubles a quote in a string and escapes a
			// backslash with one (the session has no NO_BACKSLASH_ESCAPES).
			for i++; i < len(def); i++ {
				if def[i] == '\\' && ch != '`' {
					i++
				} else if def[i] == ch {
					if i+1 < len(def) && def[i+1] == ch {
						i++
						continue
					}
					break
				}
			}
			i++
		case ch == '(':
			depth++
			i++
		case ch == ')':
			depth--
			i++
		case ch == '_' || ch == '$' || ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			j := i
			for j < len(def) && (def[j] == '_' || def[j] == '$' || def[j] >= '0' && def[j] <= '9' || def[j] >= 'a' && def[j] <= 'z' || def[j] >= 'A' && def[j] <= 'Z') {
				j++
			}
			words = append(words, word{def[i:j], i, j, depth})
			i = j
		default:
			i++
		}
	}
	// The table's collation: COLLATE=x among its options, after the columns.
	table := ""
	for k, w := range words {
		if w.depth == 0 && w.s == "COLLATE" && k+1 < len(words) && words[k+1].start == w.end+1 && def[w.end] == '=' {
			table = words[k+1].s
		}
	}
	if table == "" {
		return def
	}
	var b strings.Builder
	last := 0
	for k, w := range words {
		if w.depth != 1 || w.s != "COLLATE" || k+1 >= len(words) || k < 3 {
			continue
		}
		x := words[k+1]
		if x.s != table || x.depth != 1 || x.start != w.end+1 || def[w.end] != ' ' || def[w.start-1] != ' ' ||
			x.end < len(def) && !strings.ContainsRune(" ,\n", rune(def[x.end])) {
			continue
		}
		if words[k-2].s == "SET" && words[k-3].s == "CHARACTER" {
			continue
		}
		b.WriteString(def[last : w.start-1])
		last = x.end
	}
	if last == 0 {
		return def
	}
	b.WriteString(def[last:])
	return b.String()
}
