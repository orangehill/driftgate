package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
)

// Options configure a database component.
type Options struct {
	Name        string
	DSN         string
	Working     string   // database the application uses
	Pristine    string   // its pristine copy
	Exclude     []string // globs over table names; never fingerprinted or restored
	RowTracking []string // tables whose changed rows are recorded
	Log         io.Writer
	// Dial, when set, makes every connection to the server: only to an
	// allowed host (dbx.AllowDial).
	Dial dbx.DialFunc
	// AdoptPristine lets Seed replace a pristine database without the mark
	// of one it made: DriftGate's own record names it as the pristine copy
	// (made before the mark existed, by an earlier release).
	AdoptPristine bool
	// Replacing, when set, runs in Seed right before the pristine database
	// is dropped or renamed away, once the new copy may replace it.
	Replacing func() error
}

func (o Options) excluded(table string) bool {
	for _, g := range o.Exclude {
		if glob.Match(g, table) {
			return true
		}
	}
	return false
}

func (o Options) tracked(table string) bool { return contains(o.RowTracking, table) }

// MySQL is a MySQL or MariaDB database. The pristine copy is a second
// database on the same server, and a restore copies back only the tables,
// views, sequences and triggers that differ. Routines and events are not
// tracked; system-versioned tables must be excluded.
type MySQL struct {
	o  Options
	db *sql.DB
	// The last full fingerprint, the server's write counters and the
	// per-table counters read before it (nil: none to trust), and the items
	// the counters do not cover.
	last    Fingerprint
	writes  map[string]int64
	tables  *tableState
	always  []string
	dirty   []string // items left out of last, read by the next fingerprint
	reused  bool
	server  string // mysql or mariadb, once known (mariadb)
	version string // VERSION(), with server
	mode    string // how full fingerprints find the changed tables
	// checked lists the tables the last fingerprint checksummed by their
	// counters (nil: all or none); noTables turns the counters off (tests).
	checked  []string
	noTables bool
	logMu    sync.Mutex
	binlog   sync.Once // the one note that copies go to the binary log
	// unlog is decided once (binlogMu): whether sessions set sql_log_bin
	// = 0 (see session).
	binlogMu      sync.Mutex
	binlogDecided bool
	unlog         bool
}

// checksumConns is how many connections share the CHECKSUM TABLE work.
var checksumConns = 4

// mysqlDSN accepts mysql://user:pass@host:port/ or the driver's own form
// (user:pass@tcp(host:port)/). The database in it is ignored, and no
// server-side prepared statements are used: DriftGate's own queries then
// leave Com_stmt_execute to the application, which the write check counts.
// NewMySQL connects with the same configuration (dbx.OpenMySQL).
func mysqlDSN(dsn string) (*mysql.Config, error) { return dbdsn.MySQLConfig(dsn) }

// NewMySQL connects to the server.
func NewMySQL(o Options) (*MySQL, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	db, err := dbx.OpenMySQL(o.DSN, "", o.Dial)
	if err != nil {
		return nil, err
	}
	return &MySQL{o: o, db: db}, nil
}

func (m *MySQL) Name() string  { return m.o.Name }
func (m *MySQL) Partial() bool { return true }
func (m *MySQL) Close() error  { return m.db.Close() }

func qi(name string) string { return dbx.QuoteMySQL(name) }

// mysqlSession copies data as it is: no foreign key checks, no SQL mode
// that would reject old values such as zero dates, NO_AUTO_VALUE_ON_ZERO
// so a 0 in an AUTO_INCREMENT column is copied as 0 (as mysqldump does),
// and current table statistics (MySQL 8 caches them; see tableCounters).
var mysqlSession = dbx.MySQLSession{SQLMode: "NO_AUTO_VALUE_ON_ZERO", FreshStats: true}

// session returns a connection prepared with mysqlSession. Its writes stay
// out of the binary log (MySQL 8 has it on by default): a restore rewrites
// whole tables, which would otherwise fill the log with copies of the
// pristine rows (measured on MySQL 8.4: 78 MB per restore of an 800k-row
// table). That is only so on a server whose log nothing reads (see
// unlogged): a replica would no longer get the restores and serve the
// application old rows. Without the privilege for it (MySQL:
// SESSION_VARIABLES_ADMIN; MariaDB: BINLOG ADMIN) the copies are logged as
// before, with one note in the log.
func (m *MySQL) session(ctx context.Context) (*sql.Conn, error) {
	c, err := m.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if err := mysqlSession.Apply(ctx, c); err != nil {
		c.Close()
		return nil, err
	}
	unlog, err := m.unlogged(ctx, c)
	if err == nil && unlog {
		_, err = c.ExecContext(ctx, "SET SESSION sql_log_bin = 0")
	}
	if err != nil {
		if ctx.Err() != nil {
			c.Close()
			return nil, ctx.Err()
		}
		m.binlog.Do(func() {
			m.logf("state %s: restores are written to the binary log (SET sql_log_bin = 0: %v)\n", m.o.Name, err)
		})
	}
	return c, nil
}

// unlogged decides, once, whether sessions leave the binary log: only when
// the server writes one, and nothing else reads it — no replica (SHOW
// REPLICAS, before MySQL 8.0.22 and on MariaDB SHOW SLAVE HOSTS), no other
// client reading it (a Binlog Dump thread: a replica MariaDB does not list
// without report_host, or a change-data-capture tool), no Galera cluster
// and no group replication. When that cannot be told (the user may not
// list replicas: MySQL REPLICATION SLAVE, MariaDB REPLICATION MASTER
// ADMIN), the copies are logged, with one note.
func (m *MySQL) unlogged(ctx context.Context, c *sql.Conn) (bool, error) {
	m.binlogMu.Lock()
	defer m.binlogMu.Unlock()
	if m.binlogDecided {
		return m.unlog, nil
	}
	why, err := binlogReaders(ctx, c)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	m.binlogDecided = true
	switch {
	case err != nil:
		m.logf("state %s: restores are written to the binary log (whether a replica reads it is unknown: %v)\n", m.o.Name, err)
	case why != "":
		m.logf("state %s: restores are written to the binary log (%s)\n", m.o.Name, why)
	default:
		var on int
		if err := c.QueryRowContext(ctx, "SELECT @@log_bin").Scan(&on); err != nil {
			m.binlogDecided = false
			return false, err
		}
		m.unlog = on != 0
	}
	return m.unlog, nil
}

// binlogReaders names what reads the server's binary log besides
// DriftGate's own sessions, or "" for nothing.
func binlogReaders(ctx context.Context, c *sql.Conn) (string, error) {
	var name, value string
	err := c.QueryRowContext(ctx, "SHOW GLOBAL VARIABLES LIKE 'wsrep_on'").Scan(&name, &value)
	if err == nil && strings.EqualFold(value, "ON") {
		return "the server is a Galera cluster node", nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'group_replication' AND PLUGIN_STATUS = 'ACTIVE'").Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "the server runs group replication", nil
	}
	rows, err := c.QueryContext(ctx, "SHOW REPLICAS")
	if me := (*mysql.MySQLError)(nil); errors.As(err, &me) && me.Number == 1064 { // syntax: before MySQL 8.0.22, MariaDB
		rows, err = c.QueryContext(ctx, "SHOW SLAVE HOSTS")
	}
	if err != nil {
		return "", err
	}
	replicas := rows.Next()
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	if replicas {
		return "the server has replicas", nil
	}
	if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE COMMAND LIKE 'Binlog Dump%'").Scan(&n); err != nil {
		return "", err
	}
	if n > 0 {
		return "a replica or another client reads it", nil
	}
	return "", nil
}

type mysqlObjects struct {
	tables, views, sequences []string
	excludedTables           []string           // still needed as structure: views may use them
	kind                     map[string]string  // name → table, view or sequence
	triggers                 []dbx.MySQLTrigger // by table, timing, event and firing order
}

func (o *mysqlObjects) trigger(name string) (dbx.MySQLTrigger, bool) {
	for _, t := range o.triggers {
		if t.Name == name {
			return t, true
		}
	}
	return dbx.MySQLTrigger{}, false
}

func (m *MySQL) objects(ctx context.Context, c *sql.Conn, db string) (*mysqlObjects, error) {
	o := &mysqlObjects{kind: map[string]string{}}
	objs, err := dbx.MySQLObjects(ctx, c, db)
	if err != nil {
		return nil, err
	}
	for _, obj := range objs {
		name, typ := obj.Name, obj.Type
		switch {
		case name == pristineTable && typ == "BASE TABLE":
			continue // Seed's mark of a pristine database, not the application's
		case typ == "VIEW":
			if !m.o.excluded(name) {
				o.views = append(o.views, name)
				o.kind[name] = "view"
			}
		case m.o.excluded(name):
			if typ == "BASE TABLE" {
				o.excludedTables = append(o.excludedTables, name)
			}
		case typ == "SEQUENCE":
			o.sequences = append(o.sequences, name)
			o.kind[name] = "sequence"
		case typ == "SYSTEM VERSIONED":
			return nil, fmt.Errorf("state %s: %s is a system-versioned table, whose history cannot be restored; add it to exclude", m.o.Name, name)
		default:
			o.tables = append(o.tables, name)
			o.kind[name] = "table"
		}
	}
	trs, err := dbx.MySQLTriggers(ctx, c, db)
	if err != nil {
		return nil, err
	}
	for _, t := range trs {
		if !m.o.excluded(t.Table) {
			o.triggers = append(o.triggers, t)
		}
	}
	return o, nil
}

// show runs SHOW CREATE and returns its columns by name, or nil when the
// object does not exist: an object that the working or the pristine
// database lacks is a difference to restore (dbx.ShowCreate).
func show(ctx context.Context, c *sql.Conn, kind, db, name string) (map[string]string, error) {
	return dbx.ShowCreate(ctx, c, kind, db, name, true)
}

// showCreate returns the CREATE statement of a table, view or sequence, or ""
// when it does not exist.
func showCreate(ctx context.Context, c *sql.Conn, kind, db, name string) (string, error) {
	return dbx.ShowCreateSQL(ctx, c, kind, db, name, true)
}

// sequenceNext reads where a MariaDB sequence continues.
func sequenceNext(ctx context.Context, c *sql.Conn, db, name string) (string, error) {
	var next, cycles string
	err := c.QueryRowContext(ctx, "SELECT next_not_cached_value, cycle_count FROM "+qi(db)+"."+qi(name)).Scan(&next, &cycles)
	return next + "/" + cycles, err
}

var autoIncrement = lazy.Regexp(` AUTO_INCREMENT=(\d+)`)

// localView rewrites a view definition that names its own database (MySQL
// writes `db`.`table`) so it can be compared and created in another database.
func localView(def, db string) string { return strings.ReplaceAll(def, qi(db)+".", "") }

// Fingerprint hashes the working database. A full fingerprint first reads the
// server's write counters: when none moved since the last full fingerprint,
// no table can have changed, and only the items the counters do not cover
// (sequences, tables of other engines) are read again. When only rows were
// written, the per-table counters (see tableCounters) name the tables to
// checksum again; anything else reads every table.
func (m *MySQL) Fingerprint(ctx context.Context, items []string) (Fingerprint, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	m.reused, m.checked = false, nil
	writes, err := writeCounters(ctx, c)
	if err != nil {
		writes = nil // no counters: always the full fingerprint
	}
	if writes != nil && m.last != nil && maps.Equal(writes, m.writes) {
		f := maps.Clone(m.last)
		again := append(append([]string{}, m.always...), m.dirty...)
		if items != nil {
			f = f.Subset(items)
			again = slices.DeleteFunc(again, func(it string) bool { return !contains(items, it) })
		}
		if len(again) > 0 {
			part, err := m.fingerprint(ctx, c, m.o.Working, again)
			if err != nil {
				return nil, err
			}
			maps.Copy(f, part)
		}
		m.reused = true
		m.refresh(ctx, c)
		return f, nil
	}
	if items != nil {
		return m.fingerprint(ctx, c, m.o.Working, items)
	}
	// Both kinds of counters are read before any table, so a write after
	// them shows at the next fingerprint.
	var tables *tableState
	if writes != nil {
		var why string
		if tables, why, err = m.tableCounters(ctx, c, m.o.Working); err != nil {
			tables, why = nil, err.Error()
		}
		if tables != nil {
			m.setMode("fingerprint " + m.counterSource())
		} else {
			m.setMode("fingerprint of every table after any write (no per-table counters: " + why + ")")
		}
	}
	if writes != nil && tables != nil && m.last != nil && m.tables != nil && onlyRowWrites(m.writes, writes) {
		f, ok, err := m.incremental(ctx, c, tables)
		if err != nil {
			return nil, err
		}
		if ok {
			m.keep(ctx, c, f, writes, tables, nil)
			return f, nil
		}
	}
	f, err := m.fingerprint(ctx, c, m.o.Working, nil)
	if err != nil {
		return nil, err
	}
	m.forget()
	if writes != nil {
		if m.always, err = uncounted(ctx, c, m.o.Working); err == nil {
			m.keep(ctx, c, f, writes, tables, nil)
		}
	}
	return f, nil
}

// Reused reports whether the last fingerprint came from the write check.
func (m *MySQL) Reused() bool { return m.reused }

// readCommands are statements that change no data: they do not count as
// writes in writeCounters.
var readCommands = lazy.Regexp(`^Com_(select|show_.*|set_option|stmt_(prepare|execute|close|reset|fetch|send_long_data)|change_db|checksum|check|begin|commit|rollback|savepoint|release_savepoint|rollback_to_savepoint|empty_query|admin_commands|help|kill|lock_tables|unlock_tables|xa_.*|ha_(open|close|read)|signal|resignal|get_diagnostics|analyze|explain_other|preload_keys|assign_to_keycache|do)$`)

// writeCounters reads the server's counters that move with every change of
// data or structure: rows written, updated and deleted, and every statement
// that is not a read. Com_stmt_execute is not one of them: applications
// (Laravel, through PDO) prepare every query on the server, reads too, and
// the rows and statement kinds those executions change are counted anyway. Measured on MariaDB 11.4 and MySQL 8.4: DriftGate's own
// fingerprint moves none of them; on MySQL Handler_write moves with every
// SHOW STATUS, so Innodb_rows_* stand in for it there.
func writeCounters(ctx context.Context, c *sql.Conn) (map[string]int64, error) {
	rows, err := c.QueryContext(ctx, `SHOW GLOBAL STATUS WHERE Variable_name LIKE 'Com\_%' OR Variable_name LIKE 'Innodb\_rows\_%' OR Variable_name IN ('Handler_write', 'Handler_update', 'Handler_delete')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := map[string]int64{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		var n int64
		fmt.Sscan(value, &n)
		all[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, innodbRows := all["Innodb_rows_inserted"]
	out := map[string]int64{}
	for name, n := range all {
		switch {
		case name == "Handler_write" && innodbRows,
			name == "Innodb_rows_read",
			readCommands().MatchString(name):
			continue
		}
		out[name] = n
	}
	if len(out) == 0 {
		return nil, errors.New("no write counters")
	}
	return out, nil
}

// uncounted lists the items whose changes the write counters may miss:
// sequences (MariaDB hands out cached values without a write) and tables of
// engines other than InnoDB.
func uncounted(ctx context.Context, c *sql.Conn, db string) ([]string, error) {
	rows, err := c.QueryContext(ctx, "SELECT TABLE_NAME, TABLE_TYPE, COALESCE(ENGINE, '') FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name, typ, engine string
		if err := rows.Scan(&name, &typ, &engine); err != nil {
			return nil, err
		}
		switch {
		case typ == "SEQUENCE":
			out = append(out, "sequence:"+name)
		case typ == "BASE TABLE" && !strings.EqualFold(engine, "InnoDB"):
			out = append(out, "table:"+name)
		}
	}
	return out, rows.Err()
}

// FingerprintAs hashes items of the working database as a fingerprint of
// the given format made them (Formatter).
func (m *MySQL) FingerprintAs(ctx context.Context, format int, items []string) (Fingerprint, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return m.fingerprintAs(ctx, c, m.o.Working, items, format)
}

func (m *MySQL) fingerprint(ctx context.Context, c *sql.Conn, db string, items []string) (Fingerprint, error) {
	return m.fingerprintAs(ctx, c, db, items, FingerprintFormat)
}

// fingerprintAs is fingerprint in a format: format 1 hashed views and
// triggers without the collation they were made with.
func (m *MySQL) fingerprintAs(ctx context.Context, c *sql.Conn, db string, items []string, format int) (Fingerprint, error) {
	objs, err := m.objects(ctx, c, db)
	if err != nil {
		return nil, err
	}
	want := func(item string) bool { return items == nil || contains(items, item) }
	f := Fingerprint{}
	var tables []string
	creates := map[string]string{}
	for _, t := range objs.tables {
		if !want("table:" + t) {
			continue
		}
		def, err := showCreate(ctx, c, "TABLE", db, t)
		if err != nil {
			return nil, err
		}
		tables = append(tables, t)
		creates[t] = def
	}
	sums, err := m.checksums(ctx, c, db, tables, creates)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		sum, ok := sums[t]
		if !ok {
			return nil, fmt.Errorf("CHECKSUM TABLE returned no row for %s", t)
		}
		f["table:"+t] = hash([]byte(creates[t] + "\x00" + sum))
	}
	for _, v := range objs.views {
		if !want("view:" + v) {
			continue
		}
		r, err := show(ctx, c, "VIEW", db, v)
		if err != nil {
			return nil, err
		}
		if format >= 2 {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db) + "\x00" + r["collation_connection"]))
		} else {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db)))
		}
	}
	for _, s := range objs.sequences {
		if !want("sequence:" + s) {
			continue
		}
		def, err := showCreate(ctx, c, "SEQUENCE", db, s)
		if err != nil {
			return nil, err
		}
		next, err := sequenceNext(ctx, c, db, s)
		if err != nil {
			return nil, err
		}
		f["sequence:"+s] = hash([]byte(def + "\x00" + next))
	}
	for _, tr := range objs.triggers {
		if !want("trigger:" + tr.Name) {
			continue
		}
		r, err := show(ctx, c, "TRIGGER", db, tr.Name)
		if err != nil {
			return nil, err
		}
		if format >= 2 {
			f["trigger:"+tr.Name] = hash([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s",
				tr.Table, tr.Timing, tr.Event, tr.Order, r["sql_mode"], r["collation_connection"], r["SQL Original Statement"])))
		} else {
			f["trigger:"+tr.Name] = hash([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s",
				tr.Table, tr.Timing, tr.Event, tr.Order, r["sql_mode"], r["SQL Original Statement"])))
		}
	}
	return f, nil
}

// checksums checksums the tables (see checksumGroup; defs are their CREATE
// statements), spread by size over several connections: the server reads
// each table in full, and separate connections read them at the same time.
func (m *MySQL) checksums(ctx context.Context, c *sql.Conn, db string, tables []string, defs map[string]string) (map[string]string, error) {
	mariadb, err := m.mariadb(ctx, c)
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	if len(tables) > 1 {
		var err error
		if sizes, err = tableSizes(ctx, c, db); err != nil {
			return nil, err
		}
	}
	// Largest first, each to the connection with the least work so far.
	order := append([]string{}, tables...)
	sort.SliceStable(order, func(i, j int) bool { return sizes[order[i]] > sizes[order[j]] })
	groups := make([][]string, min(checksumConns, len(order)))
	load := make([]int64, len(groups))
	for _, t := range order {
		k := 0
		for g := range groups {
			if load[g] < load[k] {
				k = g
			}
		}
		groups[k] = append(groups[k], t)
		load[k] += sizes[t] + 1
	}
	var mu sync.Mutex
	sums := map[string]string{}
	errs := make([]error, len(groups))
	var wg sync.WaitGroup
	for g, group := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := c
			if g > 0 {
				var err error
				if conn, err = m.db.Conn(ctx); err != nil {
					errs[g] = err
					return
				}
				defer conn.Close()
			}
			got, err := checksumGroup(ctx, conn, db, group, defs, mariadb)
			if err != nil {
				errs[g] = err
				return
			}
			mu.Lock()
			maps.Copy(sums, got)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return sums, errors.Join(errs...)
}

// checksumGroup checksums tables on one connection with CHECKSUM TABLE …
// EXTENDED; one statement for many tables saves a round trip each. A table
// whose definition (in defs) has a VIRTUAL generated column, or on MariaDB
// (mariadb) any generated column, is checksummed by storedChecksum instead.
func checksumGroup(ctx context.Context, c *sql.Conn, db string, tables []string, defs map[string]string, mariadb bool) (map[string]string, error) {
	generated := virtualColumn()
	if mariadb {
		generated = generatedColumn()
	}
	sums := map[string]string{}
	var plain []string
	for _, t := range tables {
		if !generated.MatchString(defs[t]) {
			plain = append(plain, t)
			continue
		}
		sum, ok, err := storedChecksum(ctx, c, db, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		if !ok {
			plain = append(plain, t)
			continue
		}
		sums[t] = sum
	}
	tables = plain
	prefix := db + "."
	for start := 0; start < len(tables); start += 200 {
		end := min(start+200, len(tables))
		names := make([]string, 0, end-start)
		for _, t := range tables[start:end] {
			names = append(names, qi(db)+"."+qi(t))
		}
		rows, err := c.QueryContext(ctx, "CHECKSUM TABLE "+strings.Join(names, ", ")+" EXTENDED")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var sum sql.NullString
			if err := rows.Scan(&name, &sum); err != nil {
				rows.Close()
				return nil, err
			}
			sums[strings.TrimPrefix(name, prefix)] = sum.String
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return sums, nil
}

// virtualColumn and generatedColumn find a VIRTUAL generated column, and
// any generated column, in a CREATE TABLE statement (MySQL and MariaDB both
// print … GENERATED ALWAYS AS (…) VIRTUAL, STORED or, MariaDB, PERSISTENT).
// A match elsewhere, in a comment or a default, only costs storedChecksum
// a look at the columns.
var (
	virtualColumn   = lazy.Regexp(` GENERATED ALWAYS AS \(.*\) VIRTUAL\b`)
	generatedColumn = lazy.Regexp(` GENERATED ALWAYS AS \(.*\) (VIRTUAL|STORED|PERSISTENT)\b`)
)

// bigTypes are the column types storedChecksum hashes on their own, so a
// row's text stays far below max_allowed_packet (CONCAT_WS returns NULL
// beyond it).
var bigTypes = map[string]bool{"blob": true, "mediumblob": true, "longblob": true, "text": true, "mediumtext": true, "longtext": true, "json": true,
	"geometry": true, "point": true, "linestring": true, "polygon": true, "multipoint": true, "multilinestring": true, "multipolygon": true, "geometrycollection": true}

// storedChecksum stands in for CHECKSUM TABLE … EXTENDED on a table with
// generated columns: MariaDB 11.4 checksums the bytes of VIRTUAL ones too,
// and MariaDB 10.6 those of STORED ones, and those can differ between two
// copies of the same rows, or for one table from one CHECKSUM TABLE to the
// next (measured; MySQL 8.0 and 8.4 were stable, and take this path for
// VIRTUAL ones only). It reads every row (1M rows: 2.0 s, against 0.24 s
// for CHECKSUM TABLE on MariaDB 11.4). It reads the stored columns only,
// STORED generated ones included: every row's values, each with its length
// (-1 for NULL), are hashed with MD5, and the first 64 bits of the hashes
// are summed with the row count, so the order of the rows does not matter
// and a table without a primary key works too. A FLOAT is read as a DOUBLE
// and a TIMESTAMP as seconds since the epoch: their text can be the same
// for different values. ok is false when the table has no generated column.
func storedChecksum(ctx context.Context, c *sql.Conn, db, t string) (sum string, ok bool, err error) {
	rows, err := c.QueryContext(ctx, `SELECT COLUMN_NAME, DATA_TYPE, EXTRA FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, db, t)
	if err != nil {
		return "", false, err
	}
	var parts []string
	for rows.Next() {
		var name, typ, extra string
		if err := rows.Scan(&name, &typ, &extra); err != nil {
			rows.Close()
			return "", false, err
		}
		switch extra = strings.ToUpper(extra); {
		case strings.Contains(extra, "VIRTUAL"):
			ok = true
			continue
		case strings.Contains(extra, "STORED GENERATED"), strings.Contains(extra, "PERSISTENT GENERATED"):
			ok = true // not DEFAULT_GENERATED: MySQL's mark of a default expression
		}
		v := qi(name)
		switch typ = strings.ToLower(typ); {
		case typ == "float":
			v = "CAST(" + v + " AS DOUBLE)"
		case typ == "timestamp":
			v = "UNIX_TIMESTAMP(" + v + ")"
		}
		text := "CAST(" + v + " AS BINARY)"
		if bigTypes[typ] {
			text = "MD5(" + v + ")"
		}
		parts = append(parts, "COALESCE(LENGTH("+v+"), -1)", text)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !ok {
		return "", false, err
	}
	q := "SELECT COUNT(*), 0"
	if len(parts) > 0 {
		q = "SELECT COUNT(*), COALESCE(SUM(CAST(CONV(LEFT(MD5(CONCAT_WS(0x1f, " + strings.Join(parts, ", ") + ")), 16), 16, 10) AS UNSIGNED)), 0)"
	}
	var n, total string
	if err := c.QueryRowContext(ctx, q+" FROM "+qi(db)+"."+qi(t)).Scan(&n, &total); err != nil {
		return "", false, err
	}
	return "stored " + n + " " + total, true, nil
}

// copyTable fills dst.t from src.t, the rows where says or all when it is
// empty; generated columns are computed again.
func copyTable(ctx context.Context, c *sql.Conn, src, dst, table, where string) error {
	cols, err := columns(ctx, c, src, table, false)
	if err != nil {
		return err
	}
	list := strings.Join(cols, ", ")
	q := "INSERT INTO " + qi(dst) + "." + qi(table) + " (" + list + ") SELECT " + list + " FROM " + qi(src) + "." + qi(table)
	if where != "" {
		q += " WHERE " + where
	}
	_, err = c.ExecContext(ctx, q)
	return err
}

// columns lists the quoted columns of db.table in order; without generated,
// not the generated ones. (MySQL 8 also marks columns with a default
// expression DEFAULT_GENERATED; those hold data and are listed.)
func columns(ctx context.Context, c *sql.Conn, db, table string, generated bool) ([]string, error) {
	q := `SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`
	if !generated {
		q += ` AND EXTRA NOT LIKE '%VIRTUAL GENERATED%' AND EXTRA NOT LIKE '%STORED GENERATED%' AND EXTRA NOT LIKE '%PERSISTENT GENERATED%'`
	}
	rows, err := c.QueryContext(ctx, q+" ORDER BY ORDINAL_POSITION", db, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		cols = append(cols, qi(col))
	}
	return cols, rows.Err()
}

func dropObject(ctx context.Context, c *sql.Conn, db, name, kind string) error {
	_, err := c.ExecContext(ctx, "DROP "+strings.ToUpper(kind)+" IF EXISTS "+qi(db)+"."+qi(name))
	return err
}

// copyObjects makes the given tables, views, sequences and triggers of dst
// equal to src. An object that src does not have is dropped from dst.
//
// The triggers of every table whose rows are copied are dropped first and
// created again afterwards, in their firing order and with their SQL mode, so
// the copy does not fire them. The tables are copied by copyTables, whose
// count of its other connections' statements is returned.
func (m *MySQL) copyObjects(ctx context.Context, c *sql.Conn, src, dst string, tables, views, sequences, triggers []string) (map[string]int64, error) {
	if _, err := c.ExecContext(ctx, "USE "+qi(dst)); err != nil {
		return nil, err
	}
	srcObjs, err := m.objects(ctx, c, src)
	if err != nil {
		return nil, err
	}
	dstObjs, err := m.objects(ctx, c, dst)
	if err != nil {
		return nil, err
	}
	// Views first: they are created last, once their tables exist. An object
	// that changed kind (a view that became a table) is dropped as what it is.
	for _, name := range append(append([]string{}, views...), sequences...) {
		if k := dstObjs.kind[name]; k != "" {
			if err := dropObject(ctx, c, dst, name, k); err != nil {
				return nil, err
			}
		}
	}
	for _, t := range tables {
		if k := dstObjs.kind[t]; k != "" && k != "table" {
			if err := dropObject(ctx, c, dst, t, k); err != nil {
				return nil, err
			}
		}
	}

	trigTables := map[string]bool{}
	for _, t := range tables {
		trigTables[t] = true
	}
	for _, name := range triggers {
		if tr, ok := srcObjs.trigger(name); ok {
			trigTables[tr.Table] = true
		}
		if tr, ok := dstObjs.trigger(name); ok {
			trigTables[tr.Table] = true
		}
	}
	for _, tr := range dstObjs.triggers {
		if trigTables[tr.Table] || contains(triggers, tr.Name) {
			if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+qi(dst)+"."+qi(tr.Name)); err != nil {
				return nil, err
			}
		}
	}

	var jobs []tableJob
	for _, t := range tables {
		if srcObjs.kind[t] != "table" {
			if dstObjs.kind[t] == "table" {
				if err := dropObject(ctx, c, dst, t, "table"); err != nil {
					return nil, err
				}
			}
			continue
		}
		sdef, err := showCreate(ctx, c, "TABLE", src, t)
		if err != nil {
			return nil, err
		}
		ddef := ""
		if dstObjs.kind[t] == "table" {
			if ddef, err = showCreate(ctx, c, "TABLE", dst, t); err != nil {
				return nil, err
			}
		}
		jobs = append(jobs, tableJob{t: t, sdef: sdef, ddef: ddef})
	}
	extra, err := m.copyTables(ctx, c, src, dst, jobs)
	if err != nil {
		return nil, err
	}

	for _, s := range sequences {
		if srcObjs.kind[s] != "sequence" {
			continue
		}
		def, err := showCreate(ctx, c, "SEQUENCE", src, s)
		if err != nil {
			return nil, err
		}
		if _, err := c.ExecContext(ctx, def); err != nil {
			return nil, fmt.Errorf("sequence %s: %w", s, err)
		}
		var next int64
		if err := c.QueryRowContext(ctx, "SELECT next_not_cached_value FROM "+qi(src)+"."+qi(s)).Scan(&next); err != nil {
			return nil, err
		}
		if _, err := c.ExecContext(ctx, fmt.Sprintf("ALTER SEQUENCE %s.%s RESTART WITH %d", qi(dst), qi(s), next)); err != nil {
			return nil, fmt.Errorf("sequence %s: %w", s, err)
		}
	}

	// Triggers and views are made with the collation of the connection
	// that made them (their string literals have it), as mysqldump does;
	// the session's own is set back after each.
	var own string
	if err := c.QueryRowContext(ctx, "SELECT @@collation_connection").Scan(&own); err != nil {
		return nil, err
	}
	create := func(stmt, mode, collation string) error {
		if _, err := c.ExecContext(ctx, "SET SESSION sql_mode = ?, collation_connection = ?", mode, collation); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, stmt)
		if _, err2 := c.ExecContext(ctx, "SET SESSION sql_mode = ?, collation_connection = ?", mysqlSession.SQLMode, own); err == nil {
			err = err2
		}
		return err
	}

	// srcObjs.triggers is in firing order per table.
	for _, tr := range srcObjs.triggers {
		if !trigTables[tr.Table] && !contains(triggers, tr.Name) {
			continue
		}
		r, err := show(ctx, c, "TRIGGER", src, tr.Name)
		if err != nil {
			return nil, err
		}
		if r == nil {
			continue
		}
		if err := create(r["SQL Original Statement"], r["sql_mode"], r["collation_connection"]); err != nil {
			return nil, fmt.Errorf("trigger %s: %w", tr.Name, err)
		}
	}

	// A view may use another view: create them until no more succeed.
	type view struct{ def, collation string }
	pending := map[string]view{}
	for _, v := range views {
		if srcObjs.kind[v] != "view" {
			continue
		}
		r, err := show(ctx, c, "VIEW", src, v)
		if err != nil {
			return nil, err
		}
		if r != nil {
			pending[v] = view{localView(r["Create View"], src), r["collation_connection"]}
		}
	}
	for len(pending) > 0 {
		before := len(pending)
		var lastErr error
		for name, v := range pending {
			if err := create(v.def, mysqlSession.SQLMode, v.collation); err != nil {
				lastErr = fmt.Errorf("view %s: %w", name, err)
				continue
			}
			delete(pending, name)
		}
		if len(pending) == before {
			return nil, lastErr // no view could be created in this pass
		}
	}
	return extra, nil
}

func splitItems(items []string) (tables, views, sequences, triggers []string) {
	for _, it := range items {
		kind, name, _ := strings.Cut(it, ":")
		switch kind {
		case "table":
			tables = append(tables, name)
		case "view":
			views = append(views, name)
		case "sequence":
			sequences = append(sequences, name)
		case "trigger":
			triggers = append(triggers, name)
		}
	}
	return
}

func (m *MySQL) Restore(ctx context.Context, items []string) error {
	c, err := m.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	ok, err := dbx.MySQLDatabaseExists(ctx, c, m.o.Pristine)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("state %s: pristine database %s does not exist; run driftgate seed", m.o.Name, m.o.Pristine)
	}
	if items == nil {
		w, err := m.fingerprint(ctx, c, m.o.Working, nil)
		if err != nil {
			return err
		}
		p, err := m.fingerprint(ctx, c, m.o.Pristine, nil)
		if err != nil {
			return err
		}
		items = Diff(w, p)
	}
	if len(items) == 0 {
		return nil
	}
	// This connection's statement counters tell its own changes of
	// structure from anyone else's (see afterRestore).
	var sess0 map[string]int64
	if m.tables != nil {
		sess0, _ = sessionWrites(ctx, c)
	}
	tables, views, sequences, triggers := splitItems(items)
	extra, err := m.copyObjects(ctx, c, m.o.Pristine, m.o.Working, tables, views, sequences, triggers)
	if err != nil {
		m.forget()
		return err
	}
	return m.afterRestore(ctx, c, items, sess0, extra)
}

// pristineTable marks a pristine database made by Seed: a database without
// it is never dropped (see checkPristine). It is no object of the state:
// never fingerprinted, restored or copied.
const pristineTable = "driftgate_pristine"

// Seed copies the working database into a new pristine database. Excluded
// tables are copied without rows, so views that use them still work. The
// mark (pristineTable) is made first, so a seed that stops halfway leaves a
// database the next seed may drop; DriftGate's record of the pristine
// fingerprint is removed before a seed (capture.Seed), so such a database is
// not taken for a pristine copy.
func (m *MySQL) Seed(ctx context.Context) error {
	c, err := m.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	charset, collation, err := dbx.MySQLCharset(ctx, c, m.o.Working)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("state %s: working database %s does not exist", m.o.Name, m.o.Working)
	}
	if err != nil {
		return err
	}
	objs, err := m.objects(ctx, c, m.o.Working)
	if err != nil {
		return err
	}
	if err := m.checkPristine(ctx, c); err != nil {
		return err
	}
	if m.o.Replacing != nil {
		if err := m.o.Replacing(); err != nil {
			return err
		}
	}
	mark := qi(m.o.Pristine) + "." + qi(pristineTable)
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + qi(m.o.Pristine),
		"CREATE DATABASE " + qi(m.o.Pristine) + " CHARACTER SET " + charset + " COLLATE " + collation,
		"CREATE TABLE " + mark + " (id TINYINT NOT NULL PRIMARY KEY, note VARCHAR(200) NOT NULL)",
		"INSERT INTO " + mark + " VALUES (1, 'DriftGate pristine copy, made by driftgate seed and replaced by the next one')",
		"USE " + qi(m.o.Pristine),
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, t := range objs.excludedTables {
		def, err := showCreate(ctx, c, "TABLE", m.o.Working, t)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, def); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	var trigs []string
	for _, tr := range objs.triggers {
		trigs = append(trigs, tr.Name)
	}
	_, err = m.copyObjects(ctx, c, m.o.Working, m.o.Pristine, objs.tables, objs.views, objs.sequences, trigs)
	return err
}

// checkPristine refuses to drop a pristine database that Seed did not make:
// one that has tables, views, routines or events and no pristineTable,
// unless it is adopted (Options.AdoptPristine).
func (m *MySQL) checkPristine(ctx context.Context, c *sql.Conn) error {
	if m.o.AdoptPristine {
		return nil
	}
	db := m.o.Pristine
	var objects, marked int
	err := c.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME <> ?)
			+ (SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?)
			+ (SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?),
			(SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND TABLE_TYPE = 'BASE TABLE')`,
		db, pristineTable, db, db, db, pristineTable).Scan(&objects, &marked)
	if err != nil || objects == 0 || marked > 0 {
		return err
	}
	return fmt.Errorf("state %s: database %s, named as the pristine copy, was not made by driftgate seed and holds %d objects, so it is not dropped; drop it yourself or name another pristine database", m.o.Name, db, objects)
}

func (m *MySQL) Rows(ctx context.Context, items []string) (Rows, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := Rows{}
	tables, _, _, _ := splitItems(items)
	if items == nil {
		tables = m.o.RowTracking
	}
	for _, t := range tables {
		if !m.o.tracked(t) {
			continue
		}
		rows, err := c.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY ORDINAL_POSITION`, m.o.Working, t)
		if err != nil {
			return nil, err
		}
		var pk []string
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				rows.Close()
				return nil, err
			}
			pk = append(pk, col)
		}
		rows.Close()
		if len(pk) == 0 {
			return nil, fmt.Errorf("state %s: row_tracking table %s has no primary key", m.o.Name, t)
		}
		r, err := readRows(ctx, c, "SELECT * FROM "+qi(m.o.Working)+"."+qi(t), pk)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		out[t] = r
	}
	return out, nil
}

// readRows reads every row of a query into key → column → value hash.
func readRows(ctx context.Context, c *sql.Conn, query string, pk []string) (map[string]map[string]string, error) {
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	vals := make([]sql.RawBytes, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]string{}
		byName := map[string]string{}
		for i, col := range cols {
			v := "\x00NULL"
			if vals[i] != nil {
				v = string(vals[i])
			}
			byName[col] = v
			row[col] = hash([]byte(v))[:16]
		}
		var key []string
		for _, k := range pk {
			key = append(key, k+"="+byName[k])
		}
		out[strings.Join(key, ",")] = row
	}
	return out, rows.Err()
}

// Query runs a read-only query (see CheckQuery) on the pristine database.
func (m *MySQL) Query(ctx context.Context, query string) ([]string, error) {
	if err := CheckQuery(query); err != nil {
		return nil, err
	}
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "USE "+qi(m.o.Pristine)); err != nil {
		return nil, fmt.Errorf("pristine database %s: %w (run driftgate seed)", m.o.Pristine, err)
	}
	// The session stays read-only too, should the transaction end early
	// (an implicit commit); the connection goes back to the pool writable.
	if _, err := c.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "SET SESSION TRANSACTION READ WRITE")
	if _, err := c.ExecContext(ctx, "START TRANSACTION READ ONLY"); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return firstColumn(rows)
}
