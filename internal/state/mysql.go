package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	RestoreOnly []string // globs over table names; restored, but no item of Fingerprint (RestoreOnly)
	// RestoreEmpty are restore_only tables whose pristine copy has no
	// rows: Seed leaves them out as it leaves out an excluded table's.
	RestoreEmpty []string
	RowTracking  []string // tables whose changed rows are recorded
	Log          io.Writer
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

// restoreOnly reports whether a table is restore_only (Options.RestoreOnly
// or RestoreEmpty).
func (o Options) restoreOnly(table string) bool {
	return matchAny(o.RestoreOnly, table) || matchAny(o.RestoreEmpty, table)
}

// restoreEmpty reports whether a table is restored to no rows
// (Options.RestoreEmpty).
func (o Options) restoreEmpty(table string) bool { return matchAny(o.RestoreEmpty, table) }

// restoreOnlyGlobs are the globs of RestoreOnly and RestoreEmpty.
func (o Options) restoreOnlyGlobs() []string {
	return append(append([]string{}, o.RestoreOnly...), o.RestoreEmpty...)
}

func matchAny(globs []string, table string) bool {
	for _, g := range globs {
		if glob.Match(g, table) {
			return true
		}
	}
	return false
}

// bothErr is the error of a table that both exclude and restore_only name.
func (o Options) bothErr(table string) error {
	return fmt.Errorf("state %s: table %s is both excluded and restore_only; name it in one of them", o.Name, table)
}

func (o Options) tracked(table string) bool { return contains(o.RowTracking, table) }

// MySQL is a MySQL or MariaDB database. The pristine copy is a second
// database on the same server, and a restore copies back only the tables,
// views, sequences and triggers that differ. Routines and events are not
// tracked; system-versioned tables must be excluded.
type MySQL struct {
	o  Options
	db *sql.DB
	// multi takes queries of several statements: SHOW CREATE for many
	// objects in one round trip (see showAll). multiRefused is set once
	// one was refused.
	multi        *sql.DB
	multiRefused atomic.Bool
	multiNote    sync.Once
	// multiConnNote: the one note that a batch could not be sent.
	multiConnNote sync.Once
	// The state of new connections (see initConn): connMu guards it.
	connMu       sync.Mutex
	noFreshStats bool                 // the server has no information_schema_stats_expiry (MariaDB)
	logBin       int                  // logBinUndecided, logBinOff or logBinOn
	early        map[driver.Conn]bool // connections made before logBin was decided
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
	// probed holds the DEFINERs definerProbe found the login may use;
	// login is CURRENT_USER(), once read (dropFirstOthers).
	probeMu    sync.Mutex
	probed     map[string]bool
	probeNoted map[string]bool // probes left behind already logged (noteProbe)
	hooks      *mysqlHooks     // tests only
	lockNote   sync.Once       // the note that GET_LOCK is not supported
	login      string
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
	m := &MySQL{o: o, early: map[driver.Conn]bool{}}
	db, err := dbx.OpenMySQLWith(o.DSN, "", o.Dial, dbx.MySQLOpen{Init: m.initConn})
	if err != nil {
		return nil, err
	}
	multi, err := dbx.OpenMySQLWith(o.DSN, "", o.Dial, dbx.MySQLOpen{MultiStatements: true, Init: m.initMulti})
	if err != nil {
		db.Close()
		return nil, err
	}
	// The connections a fingerprint or a restore uses at the same time
	// (checksums, copyTables' connections, each with rowDiff's two of its
	// own) stay open for the next one, until the component is closed at the
	// end of the command: a new connection costs a server that resolves the
	// client's name a reverse DNS lookup (database/sql keeps 2 by default).
	db.SetMaxIdleConns(copyConns * 3)
	m.db, m.multi = db, multi
	return m, nil
}

func (m *MySQL) Name() string  { return m.o.Name }
func (m *MySQL) Partial() bool { return true }
func (m *MySQL) Close() error  { return errors.Join(m.db.Close(), m.multi.Close()) }

func qi(name string) string { return dbx.QuoteMySQL(name) }

// mysqlSession copies data as it is: no foreign key checks, no SQL mode
// that would reject old values such as zero dates, NO_AUTO_VALUE_ON_ZERO
// so a 0 in an AUTO_INCREMENT column is copied as 0 (as mysqldump does),
// and current table statistics (MySQL 8 caches them; see tableCounters).
var mysqlSession = dbx.MySQLSession{SQLMode: "NO_AUTO_VALUE_ON_ZERO", FreshStats: true}

// session returns a connection prepared with mysqlSession (by initConn,
// once per connection). Its writes stay out of the binary log (MySQL 8 has
// it on by default): a restore rewrites whole tables, which would otherwise
// fill the log with copies of the pristine rows (measured on MySQL 8.4: 78
// MB per restore of an 800k-row table). That is only so on a server whose
// log nothing reads (see unlogged): a replica would no longer get the
// restores and serve the application old rows. Without the privilege for
// it (MySQL: SESSION_VARIABLES_ADMIN; MariaDB: BINLOG ADMIN) the copies are
// logged as before, with one note in the log.
func (m *MySQL) session(ctx context.Context) (*sql.Conn, error) {
	c, err := m.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	unlog, err := m.unlogged(ctx, c)
	if err == nil && unlog && m.madeEarly(c) {
		// Made before the decision: initConn could not leave the log.
		if _, err = c.ExecContext(ctx, "SET SESSION sql_log_bin = 0"); err != nil && ctx.Err() == nil {
			m.setLogBin(logBinOn)
		}
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
	if m.unlog {
		m.setLogBin(logBinOff)
	} else {
		m.setLogBin(logBinOn)
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
	restoreOnly              []string           // tables restored but not fingerprinted (Options.RestoreOnly)
	kind                     map[string]string  // name → table, view or sequence
	triggers                 []dbx.MySQLTrigger // by table, timing, event and firing order
	probes                   map[string]string  // probe objects left behind (probeName): name → table, view or trigger
}

// allTables lists every table of the database, also those restored only
// or excluded: any of them may hold a trigger probe (definerProbe).
func (o *mysqlObjects) allTables() []string {
	return slices.Concat(o.tables, o.restoreOnly, o.excludedTables)
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
		case probeName(name):
			// A probe left behind (definerProbe): never part of the state.
			m.noteProbe(db, name)
			if o.probes == nil {
				o.probes = map[string]string{}
			}
			o.probes[name] = "table"
			if typ == "VIEW" {
				o.probes[name] = "view"
			}
		case typ == "VIEW":
			if !m.o.excluded(name) {
				o.views = append(o.views, name)
				o.kind[name] = "view"
			}
		case m.o.excluded(name):
			if typ == "BASE TABLE" {
				if m.o.restoreOnly(name) {
					return nil, m.o.bothErr(name)
				}
				o.excludedTables = append(o.excludedTables, name)
			}
		case typ == "SEQUENCE":
			o.sequences = append(o.sequences, name)
			o.kind[name] = "sequence"
		case typ == "SYSTEM VERSIONED":
			return nil, fmt.Errorf("state %s: %s is a system-versioned table, whose history cannot be restored; add it to exclude", m.o.Name, name)
		case m.o.restoreOnly(name):
			o.restoreOnly = append(o.restoreOnly, name)
			o.kind[name] = "table"
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
		if probeName(t.Name) && !probeName(t.Table) {
			// A probe on a table of the application (definerProbe).
			m.noteProbe(db, t.Name)
			if o.probes == nil {
				o.probes = map[string]string{}
			}
			o.probes[t.Name] = "trigger"
			continue
		}
		if !m.o.excluded(t.Table) && !probeName(t.Table) {
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
	var mode string // said once the fingerprint is kept (keep may turn the counters off)
	if writes != nil {
		var why string
		if tables, why, err = m.tableCounters(ctx, c, m.o.Working); err != nil {
			tables, why = nil, err.Error()
		}
		if tables != nil {
			mode = "fingerprint " + m.counterSource()
		} else {
			mode = noCountersMode(why)
		}
	}
	if writes != nil && tables != nil && m.last != nil && m.tables != nil && onlyRowWrites(m.writes, writes) {
		f, ok, err := m.incremental(ctx, c, tables)
		if err != nil {
			return nil, err
		}
		if ok {
			if why := m.keep(ctx, c, f, writes, tables, nil); why != "" {
				mode = noCountersMode(why)
			}
			m.setMode(mode)
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
			if why := m.keep(ctx, c, f, writes, tables, nil); why != "" {
				mode = noCountersMode(why)
			}
		}
		m.setMode(mode)
	}
	return f, nil
}

// noCountersMode is the mode of fingerprints without per-table counters.
func noCountersMode(why string) string {
	return "fingerprint of every table after any write (no per-table counters: " + why + ")"
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

// PristineFingerprint hashes items of the pristine database.
func (m *MySQL) PristineFingerprint(ctx context.Context, items []string) (Fingerprint, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return m.fingerprint(ctx, c, m.o.Pristine, items)
}

func (m *MySQL) fingerprint(ctx context.Context, c *sql.Conn, db string, items []string) (Fingerprint, error) {
	return m.fingerprintAs(ctx, c, db, items, FingerprintFormat)
}

// fingerprintAs is fingerprint in a format: format 1 hashed views and
// triggers without the collation they were made with. A read the server
// stops for a lock is read again in full (retryLocked).
func (m *MySQL) fingerprintAs(ctx context.Context, c *sql.Conn, db string, items []string, format int) (f Fingerprint, err error) {
	err = m.retryLocked(ctx, "fingerprint", func(ctx context.Context) error {
		f, err = m.fingerprintOnce(ctx, c, db, items, format, fpRows)
		return err
	})
	return f, err
}

// fingerprintOnce hashes items (nil: all) of db; with fpRestoreOnly only
// the restore_only tables, which it otherwise leaves out; with fpSchema
// those too, without checksums, AUTO_INCREMENT or where a sequence goes on.
func (m *MySQL) fingerprintOnce(ctx context.Context, c *sql.Conn, db string, items []string, format int, mode fpMode) (Fingerprint, error) {
	objs, err := m.objects(ctx, c, db)
	if err != nil {
		return nil, err
	}
	want := func(item string) bool { return items == nil || contains(items, item) }
	names := objs.tables
	switch mode {
	case fpRestoreOnly:
		names = objs.restoreOnly
		want = func(item string) bool { return strings.HasPrefix(item, "table:") }
	case fpSchema:
		names = slices.Concat(objs.tables, objs.restoreOnly)
	}
	f := Fingerprint{}
	// One read of SHOW CREATE for every table, view and trigger (showAll).
	var tables, views []string
	var trigs []dbx.MySQLTrigger
	var reqs []showReq
	for _, t := range names {
		if want("table:" + t) {
			tables = append(tables, t)
			reqs = append(reqs, showReq{"TABLE", db, t})
		}
	}
	for _, v := range objs.views {
		if want("view:" + v) {
			views = append(views, v)
			reqs = append(reqs, showReq{"VIEW", db, v})
		}
	}
	for _, tr := range objs.triggers {
		if want("trigger:" + tr.Name) {
			trigs = append(trigs, tr)
			reqs = append(reqs, showReq{"TRIGGER", db, tr.Name})
		}
	}
	shown, err := m.showAll(ctx, c, reqs)
	if err != nil {
		return nil, err
	}
	if err := m.hashCreated(ctx, c, db, tables, shown[:len(tables)], f, mode); err != nil {
		return nil, err
	}
	shown = shown[len(tables):]
	for i, v := range views {
		r := shown[i]
		if format >= 2 {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db) + "\x00" + r["collation_connection"]))
		} else {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db)))
		}
	}
	shown = shown[len(views):]
	for _, s := range objs.sequences {
		if !want("sequence:" + s) {
			continue
		}
		def, err := showCreate(ctx, c, "SEQUENCE", db, s)
		if err != nil {
			return nil, err
		}
		var next string
		if mode.rows() {
			if next, err = sequenceNext(ctx, c, db, s); err != nil {
				return nil, err
			}
		}
		f["sequence:"+s] = hash([]byte(def + "\x00" + next))
	}
	for i, tr := range trigs {
		r := shown[i]
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

// hashCreated adds to f the item of every table: its CREATE statement, from
// its row of SHOW CREATE TABLE in shown, and its checksum; with fpSchema
// the statement without AUTO_INCREMENT only.
func (m *MySQL) hashCreated(ctx context.Context, c *sql.Conn, db string, tables []string, shown []map[string]string, f Fingerprint, mode fpMode) error {
	creates := map[string]string{}
	for i, t := range tables {
		def, err := dbx.ShowCreateStatement("TABLE", t, shown[i])
		if err != nil {
			return err
		}
		creates[t] = def
		if !mode.rows() {
			f["table:"+t] = hash([]byte(autoIncrement().ReplaceAllString(def, "")))
		}
	}
	if !mode.rows() {
		return nil
	}
	sums, err := m.checksums(ctx, c, db, tables, creates)
	if err != nil {
		return err
	}
	for _, t := range tables {
		sum, ok := sums[t]
		if !ok {
			return fmt.Errorf("CHECKSUM TABLE returned no row for %s", t)
		}
		f["table:"+t] = hash([]byte(creates[t] + "\x00" + sum))
	}
	return nil
}

// RestoreOnlyFingerprint hashes the restore_only tables of the working
// database, or of the pristine one (RestoreOnly). It leaves the last
// fingerprint and its counters as they were.
func (m *MySQL) RestoreOnlyFingerprint(ctx context.Context, pristine bool) (Fingerprint, error) {
	return m.read(ctx, pristine, fpRestoreOnly)
}

// SchemaFingerprint hashes the definitions of the working database, or of
// the pristine one (Schemer).
func (m *MySQL) SchemaFingerprint(ctx context.Context, pristine bool) (Fingerprint, error) {
	return m.read(ctx, pristine, fpSchema)
}

// read fingerprints the working database, or the pristine one, as mode
// reads it, again when the server stops it for a lock (retryLocked).
func (m *MySQL) read(ctx context.Context, pristine bool, mode fpMode) (f Fingerprint, err error) {
	err = m.retryLocked(ctx, "fingerprint", func(ctx context.Context) error {
		f, err = m.readOnce(ctx, pristine, mode)
		return err
	})
	return f, err
}

func (m *MySQL) readOnce(ctx context.Context, pristine bool, mode fpMode) (Fingerprint, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	db := m.o.Working
	if pristine {
		db = m.o.Pristine
	}
	return m.fingerprintOnce(ctx, c, db, nil, FingerprintFormat, mode)
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
	return copyColumns(ctx, c, src, dst, table, where, cols)
}

// copyColumns is copyTable with the columns to copy (quoted) known.
func copyColumns(ctx context.Context, c *sql.Conn, src, dst, table, where string, cols []string) error {
	list := strings.Join(cols, ", ")
	q := "INSERT INTO " + qi(dst) + "." + qi(table) + " (" + list + ") SELECT " + list + " FROM " + qi(src) + "." + qi(table)
	if where != "" {
		q += " WHERE " + where
	}
	_, err := c.ExecContext(ctx, q)
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
	if me := (*mysql.MySQLError)(nil); errors.As(err, &me) && me.Number == 1227 {
		// MySQL: a view whose DEFINER has SYSTEM_USER (root) may be dropped
		// only by a login that has it too.
		return fmt.Errorf("%s %s cannot be dropped from %s to be made again: the server asks for a privilege this login lacks, one its DEFINER has (on MySQL SYSTEM_USER, which root has). Create the triggers and views with this login as their DEFINER (run the migrations as it) and seed again (%w)", kind, name, db, me)
	}
	return err
}

// copyObjects makes the given tables, views, sequences and triggers of dst
// equal to src. An object that src does not have is dropped from dst.
//
// The triggers of every table whose rows are copied are dropped first and
// created again afterwards, in their firing order and with their SQL mode, so
// the copy does not fire them. The tables are copied by copyTables, whose
// count of its other connections' statements is returned.
//
// known holds definitions of src already read (Seed), which are not read
// again.
func (m *MySQL) copyObjects(ctx context.Context, c *sql.Conn, src, dst string, tables, views, sequences, triggers []string, known map[showReq]map[string]string) (map[string]int64, error) {
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
	var copied []string
	for _, t := range tables {
		if srcObjs.kind[t] == "table" {
			copied = append(copied, t)
		}
	}
	// Every SHOW CREATE the copy needs is read here, at once (showAll),
	// before dst is changed: the tables in both databases, and the
	// triggers and views of src, which the copy does not change.
	var reqs []showReq
	for _, t := range copied {
		reqs = append(reqs, showReq{"TABLE", src, t})
		if dstObjs.kind[t] == "table" {
			reqs = append(reqs, showReq{"TABLE", dst, t})
		}
	}
	var trigs []dbx.MySQLTrigger // srcObjs.triggers is in firing order per table
	var defReqs []showReq        // of trigs, then srcViews
	for _, tr := range srcObjs.triggers {
		if trigTables[tr.Table] || contains(triggers, tr.Name) {
			trigs = append(trigs, tr)
			defReqs = append(defReqs, showReq{"TRIGGER", src, tr.Name})
		}
	}
	var srcViews []string
	for _, v := range views {
		if srcObjs.kind[v] == "view" {
			srcViews = append(srcViews, v)
			defReqs = append(defReqs, showReq{"VIEW", src, v})
		}
	}
	for _, r := range defReqs {
		if _, ok := known[r]; !ok {
			reqs = append(reqs, r)
		}
	}
	shown, err := m.showAll(ctx, c, reqs)
	if err != nil {
		return nil, err
	}
	var jobs []tableJob
	for _, t := range copied {
		sdef, err := dbx.ShowCreateStatement("TABLE", t, shown[0])
		if err != nil {
			return nil, err
		}
		shown = shown[1:]
		ddef := ""
		if dstObjs.kind[t] == "table" {
			if ddef, err = dbx.ShowCreateStatement("TABLE", t, shown[0]); err != nil {
				return nil, err
			}
			shown = shown[1:]
		}
		jobs = append(jobs, tableJob{t: t, sdef: sdef, ddef: ddef})
	}
	// A name among the views and sequences is dropped below whatever dst
	// holds under it, a table too: its job then starts from no table.
	for i, j := range jobs {
		if contains(views, j.t) || contains(sequences, j.t) {
			jobs[i].ddef = ""
		}
	}
	defShown := make([]map[string]string, len(defReqs))
	for i, r := range defReqs {
		if d, ok := known[r]; ok {
			defShown[i] = d
		} else {
			defShown[i], shown = shown[0], shown[1:]
		}
	}
	trigShown, viewShown := defShown[:len(trigs)], defShown[len(trigs):]
	var trigDefs []map[string]string // in firing order per table
	for i, tr := range trigs {
		if r := trigShown[i]; r != nil {
			r["name"], r["table"], r["timing"], r["event"] = tr.Name, tr.Table, tr.Timing, tr.Event
			trigDefs = append(trigDefs, r)
		}
	}
	pending := map[string]viewDef{}
	for i, v := range srcViews {
		if r := viewShown[i]; r != nil {
			pending[v] = viewDef{localView(r["Create View"], src), r["collation_connection"]}
		}
	}

	// The sequences made below, in order: those src has. A Galera node
	// refuses some of them; that is told before dst is changed.
	var seqs []string
	for _, q := range sequences {
		if srcObjs.kind[q] == "sequence" {
			seqs = append(seqs, q)
		}
	}
	if err := m.galeraSequences(ctx, c, src, seqs); err != nil {
		return nil, err
	}

	// The login is first tried on each DEFINER of what is made again
	// (definerProbe), so a refusal leaves dst as it was.
	var probes []probeItem
	for _, r := range trigDefs {
		probes = append(probes, probeItem{"trigger", r["name"], r["SQL Original Statement"], r["table"]})
	}
	for name, v := range pending {
		probes = append(probes, probeItem{kind: "view", name: name, stmt: v.def})
	}
	if err := m.definerProbe(ctx, c, dst, dst, dstObjs.allTables(), dstObjs.probes, probes); err != nil {
		return nil, err
	}

	// Triggers and views are made with the collation of the connection
	// that made them (their string literals have it), as mysqldump does;
	// the session's own is set back after each.
	var own string
	if err := c.QueryRowContext(ctx, "SELECT @@collation_connection").Scan(&own); err != nil {
		return nil, err
	}
	// A session that cannot be set back is not put back into the pool
	// (sessions are prepared once, see initConn), and the restore stops.
	var lost error
	create := func(stmt, mode, collation string) error {
		if lost != nil {
			return lost
		}
		if _, err := c.ExecContext(ctx, "SET SESSION sql_mode = ?, collation_connection = ?", mode, collation); err != nil {
			return err // a SET that fails changes nothing
		}
		_, err := c.ExecContext(ctx, stmt)
		if _, err2 := c.ExecContext(ctx, "SET SESSION sql_mode = ?, collation_connection = ?", mysqlSession.SQLMode, own); err2 != nil {
			return setBack(c, &lost, err2)
		}
		return err
	}

	// Views first: they are created last, once their tables exist. An object
	// that changed kind (a view that became a table) is dropped as what it is.
	var drops []dropItem
	for _, name := range append(append([]string{}, views...), sequences...) {
		if k := dstObjs.kind[name]; k != "" {
			drops = append(drops, dropItem{kind: k, name: name})
		}
	}
	for _, t := range tables {
		if k := dstObjs.kind[t]; k != "" && k != "table" {
			drops = append(drops, dropItem{kind: k, name: t})
		}
	}
	for _, tr := range dstObjs.triggers {
		if trigTables[tr.Table] || contains(triggers, tr.Name) {
			drops = append(drops, dropItem{kind: "trigger", name: tr.Name, definer: tr.Definer})
		}
	}
	for _, t := range tables { // tables src no longer has
		if srcObjs.kind[t] != "table" && dstObjs.kind[t] == "table" {
			drops = append(drops, dropItem{kind: "table", name: t})
		}
	}
	if err := m.dropFirstOthers(ctx, c, dst, drops); err != nil {
		return nil, err
	}
	// ids are the connections of the copy (CONNECTION_ID()): the server
	// may still run their statements after a cancel.
	ids := []int64{0}
	if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&ids[0]); err != nil {
		return nil, err
	}
	// onBackground goes on after a failure or a cancel on a session of its
	// own that no cancel stops: it ends what the copy's connections still
	// run (KILL QUERY), so their locks go, and waits 30 s at most for a
	// lock. It rebinds ctx and c, which create, makeSeq and makeObjects
	// read when they run, so they run there; done gives the session up.
	onBackground := func() (done func()) {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		s, err := m.session(bg)
		if err != nil {
			return cancel
		}
		for _, id := range ids {
			if id != 0 { // a connection whose id was not read
				s.ExecContext(bg, fmt.Sprintf("KILL QUERY %d", id))
			}
		}
		s.ExecContext(bg, "SET SESSION lock_wait_timeout = 30")
		s.ExecContext(bg, "USE "+qi(dst))
		ctx, c = bg, s
		return func() { discardConn(s); cancel() }
	}
	makeSeq := func(q string) error { return copySequence(ctx, c, src, dst, q) }
	// makeObjects makes the sequences of seqs, the triggers of trigDefs (in
	// firing order per table) and the views of pending, and takes each
	// made off its list: what a failure leaves on them is missing from
	// dst. A closure: it shares create's collation and lost, and reads ctx
	// and c when it runs, which onBackground rebinds.
	makeObjects := func() error {
		for len(seqs) > 0 {
			if err := makeSeq(seqs[0]); err != nil {
				return err
			}
			seqs = seqs[1:]
		}
		for len(trigDefs) > 0 {
			r := trigDefs[0]
			if err := create(r["SQL Original Statement"], r["sql_mode"], r["collation_connection"]); err != nil {
				if lost == nil {
					err = createRefused(ctx, c, "trigger", r["name"], dst, r["SQL Original Statement"], err)
				}
				return err
			}
			trigDefs = trigDefs[1:]
		}
		// A view may use another view: create them until no more succeed.
		for len(pending) > 0 {
			before := len(pending)
			var lastErr error
			for name, v := range pending {
				if err := create(v.def, mysqlSession.SQLMode, v.collation); err != nil {
					if lost != nil {
						return lost
					}
					lastErr = createRefused(ctx, c, "view", name, dst, v.def, err)
					continue
				}
				delete(pending, name)
			}
			if len(pending) == before { // no view could be created in this pass
				return lastErr
			}
		}
		return nil
	}
	// What makes again what a failure leaves missing: a restore of the
	// working database, or a seed of the pristine copy.
	recovery := "they are made again from the pristine copy"
	if dst == m.o.Pristine {
		recovery = "driftgate seed runs again"
	}
	var dropped []dropItem
	for _, d := range drops {
		var err error
		if m.hooks != nil && m.hooks.dropRefused != nil && m.hooks.dropRefused(d.kind, d.name) {
			err = &mysql.MySQLError{Number: 1227, Message: "refused by a test"}
		} else {
			err = dropObject(ctx, c, dst, d.name, d.kind)
		}
		if err != nil {
			if lost != nil {
				return nil, lost
			}
			defer onBackground()()
			return nil, undoDrops(dst, recovery, dropped, srcObjs.kind, srcObjs.triggers, dstObjs.triggers, trigDefs, pending, create, makeSeq, err)
		}
		dropped = append(dropped, d)
	}
	// From here on every sequence of seqs, trigger of trigDefs and view of
	// pending is missing from dst until it is made: a failure names them
	// (notMade).
	var extra map[string]int64
	if m.hooks != nil && m.hooks.copyFails != nil {
		err = m.hooks.copyFails()
	} else {
		extra, err = m.copyTables(ctx, c, src, dst, jobs, &ids)
	}
	if err == nil {
		err = makeObjects()
	}
	if err != nil && lost == nil && dst == m.o.Working {
		// A failed or stopped restore makes them again: the working schema
		// stays the pristine copy's.
		defer onBackground()()
		makeObjects()
	}
	if err != nil {
		return nil, notMade(dst, recovery, err, lost, seqs, trigDefs, pending)
	}
	return extra, nil
}

// definerClause reads the account of a DEFINER clause as SHOW CREATE
// writes it (DEFINER=`app`@`%`).
var definerClause = lazy.Regexp("DEFINER=`((?:[^`]|``)*)`@`((?:[^`]|``)*)`")

// createRefused explains why the server refused to create the trigger or
// view kind name in db as stmt defines it, when that is a privilege: a
// restore and a seed make them with their DEFINER, as SHOW CREATE gives it.
// A login may name another account as DEFINER only with SET_ANY_DEFINER
// (MySQL 8.2 and later; SET_USER_ID before; and SYSTEM_USER for an account
// that has it), SET USER (MariaDB) or SUPER: error 1227. With the binary
// log on, a login without SUPER may create no trigger at all unless
// log_bin_trust_function_creators is set (MySQL and MariaDB): error 1419. The DEFINER is never dropped or changed to make the object
// fit the login: a trigger, and a view with SQL SECURITY DEFINER, run with
// their definer's rights, so the copy would run under another account
// than the original, and the DEFINER is part of the fingerprint, so the
// copy would not be the one checked. login is the login's account,
// CURRENT_USER(), read only on such an error.
func createRefused(ctx context.Context, c *sql.Conn, kind, name, db, stmt string, err error) error {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1227 && me.Number != 1419 {
		return fmt.Errorf("%s %s: %w", kind, name, err)
	}
	var login string
	if c.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&login) != nil {
		login = ""
	}
	return refusedText(kind, name, db, stmt, login, me)
}

// refusedText is the error of createRefused.
func refusedText(kind, name, db, stmt, login string, me *mysql.MySQLError) error {
	// other is the DEFINER when it is not the login's account.
	other := ""
	if m := definerClause().FindStringSubmatch(stmt); m != nil && login != "" {
		user, host := strings.ReplaceAll(m[1], "``", "`"), strings.ReplaceAll(m[2], "``", "`")
		if !strings.Contains(login, "@") || !sameAccount(user+"@"+host, login) {
			other = m[0][len("DEFINER="):]
		}
	}
	const setDefiner = "naming another account as DEFINER needs SET_ANY_DEFINER (MySQL 8.2 and later; SET_USER_ID before; and SYSTEM_USER when that account has it, as root does), SET USER (MariaDB) or SUPER"
	const why = "DriftGate creates triggers and views with their DEFINER and does not change it: a trigger, and a view with SQL SECURITY DEFINER, run with their definer's rights, and the DEFINER is part of the fingerprint"
	switch {
	case me.Number == 1419:
		msg := fmt.Sprintf("%s %s cannot be created in %s: the server writes a binary log (or is a Galera node) and log_bin_trust_function_creators is off, so a login without SUPER may create no trigger; set it on the server (as an administrator: SET PERSIST log_bin_trust_function_creators = 1) or give the login SUPER", kind, name, db)
		if other != "" {
			msg += fmt.Sprintf(". Its DEFINER %s is not this login (%s) either: %s; grant it, or create the triggers and views with this login as their DEFINER (run the migrations as it) and seed again", other, login, setDefiner)
		}
		return fmt.Errorf("%s. %s (%w)", msg, why, me)
	case other != "":
		return fmt.Errorf("%s %s cannot be created in %s: its DEFINER is %s, this login is %s, and %s. Grant it to this login, or create the triggers and views with this login as their DEFINER (run the migrations as it) and seed again. %s (%w)",
			kind, name, db, other, login, setDefiner, why, me)
	}
	return fmt.Errorf("%s %s: %w", kind, name, me)
}

// mysqlHooks, set by tests on a MySQL, make a restore fail where a server
// would: dropRefused refuses the drops it names (1227) without sending
// them, and copyFails replaces the copy of the tables with its error.
type mysqlHooks struct {
	dropRefused func(kind, name string) bool
	copyFails   func() error
	lockStop    func(what string) error // before each try of retryLocked
}

// galeraSequences refuses, on a Galera node, to copy a sequence of src the
// node would refuse to create (1235): one with a cache and an increment
// other than 0, or one not stored by InnoDB. Copied without its cache or
// with another engine, it would be another object, so it is named instead.
func (m *MySQL) galeraSequences(ctx context.Context, c *sql.Conn, src string, seqs []string) error {
	if len(seqs) == 0 {
		return nil
	}
	if galera, err := dbx.GaleraNode(ctx, c); err != nil || !galera {
		return err
	}
	refused, err := dbx.GaleraRefusedSequences(ctx, c, src, seqs)
	if err != nil || len(refused) == 0 {
		return err
	}
	return fmt.Errorf("state %s: the server is a Galera cluster node, which creates no sequence with a cache and an increment other than 0, nor one of another engine than InnoDB (1235), so these sequences of %s cannot be copied: %s; %s, or add them to exclude", m.o.Name, src, strings.Join(refused, ", "), dbx.GaleraSequencesHint)
}

// copySequence makes sequence q of dst as src has it, where it goes on.
//
// The ALTER that sets where it goes on states the increment and the cache
// again, as they are: a Galera node checks an ALTER SEQUENCE against the
// cache of the statement, not the sequence's, and refuses a bare RESTART
// (1235, "CACHE without INCREMENT BY 0").
func copySequence(ctx context.Context, c *sql.Conn, src, dst, q string) error {
	def, err := showCreate(ctx, c, "SEQUENCE", src, q)
	if err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, def); err != nil {
		return fmt.Errorf("sequence %s: %w", q, err)
	}
	var next, increment, cycles int64
	var cache uint64
	if err := c.QueryRowContext(ctx, "SELECT next_not_cached_value, increment, cache_size, cycle_count FROM "+qi(src)+"."+qi(q)).Scan(&next, &increment, &cache, &cycles); err != nil {
		return err
	}
	caching := "NOCACHE"
	if cache != 0 {
		caching = fmt.Sprintf("CACHE %d", cache)
	}
	if _, err := c.ExecContext(ctx, fmt.Sprintf("ALTER SEQUENCE %s.%s INCREMENT BY %d %s RESTART WITH %d", qi(dst), qi(q), increment, caching, next)); err != nil {
		return fmt.Errorf("sequence %s: %w", q, err)
	}
	// RESTART counts no cycle; one that has cycled gets its count back
	// (SETVAL's round), at the same next value.
	if cycles != 0 {
		if _, err := c.ExecContext(ctx, fmt.Sprintf("SELECT SETVAL(%s.%s, %d, 0, %d)", qi(dst), qi(q), next, cycles)); err != nil {
			return fmt.Errorf("sequence %s: %w", q, err)
		}
	}
	return nil
}

// undoDrops answers a drop the server refused (refused) after the drops
// of dropped succeeded: the sequences, triggers and views among them that
// the pristine copy has (srcKind; trigDefs, in firing order per table;
// views) are made again from it, which definerProbe showed the login may
// do for the triggers and views. A trigger is made again next to its
// pristine neighbour that dst still has (FOLLOWS or PRECEDES; dstTriggers
// are dst's before the drops); when the working copy's triggers of that
// table were reordered, or others stand between, the order may still
// differ from the pristine one, which the message says. It names what was
// made again; what is missing until it is made again from the pristine copy
// (its creation failed, or it is of another kind in the pristine copy);
// and what is gone, which no restore makes again (the pristine copy has
// nothing of that name).
func undoDrops(dst, recovery string, dropped []dropItem, srcKind map[string]string, srcTriggers, dstTriggers []dbx.MySQLTrigger, trigDefs []map[string]string, views map[string]viewDef,
	create func(stmt, mode, collation string) error, makeSeq func(string) error, refused error) error {
	var again, missing, gone []string
	isDropped := map[string]bool{}
	inTrigDefs := func(name string) bool {
		return slices.ContainsFunc(trigDefs, func(r map[string]string) bool { return r["name"] == name })
	}
	// The table of each trigger, in the pristine copy and in dst: a
	// trigger may be on another table in one of them (a renamed table).
	srcTable, dstTable := map[string]string{}, map[string]string{}
	for _, tr := range srcTriggers {
		srcTable[tr.Name] = tr.Table
	}
	for _, tr := range dstTriggers {
		dstTable[tr.Name] = tr.Table
	}
	for _, d := range dropped {
		isDropped[d.kind+" "+d.name] = true
		switch {
		case d.kind == "trigger" && inTrigDefs(d.name):
		case d.kind == "trigger" && srcTable[d.name] != "":
			missing = append(missing, fmt.Sprintf("trigger %s (on table %s here, on %s in the pristine copy)", d.name, dstTable[d.name], srcTable[d.name]))
		case d.kind == "trigger":
			gone = append(gone, "trigger "+d.name)
		case srcKind[d.name] == "":
			gone = append(gone, d.kind+" "+d.name)
		case d.kind == "view" && views[d.name].def != "":
		case d.kind == "sequence" && srcKind[d.name] == "sequence":
			if err := makeSeq(d.name); err != nil {
				missing = append(missing, fmt.Sprintf("sequence %s (%v)", d.name, err))
			} else {
				again = append(again, "sequence "+d.name)
			}
		default: // another kind in the pristine copy: made by a restore
			missing = append(missing, fmt.Sprintf("%s %s (a %s in the pristine copy)", d.kind, d.name, srcKind[d.name]))
		}
	}
	// present: the triggers dst has now, by name, with their place.
	present := map[string]dbx.MySQLTrigger{}
	for _, tr := range dstTriggers {
		if !isDropped["trigger "+tr.Name] {
			present[tr.Name] = tr
		}
	}
	same := func(r map[string]string, tr dbx.MySQLTrigger) bool {
		return tr.Table == r["table"] && tr.Timing == r["timing"] && tr.Event == r["event"]
	}
	triggersAgain := false
	for i, r := range trigDefs {
		if !isDropped["trigger "+r["name"]] {
			continue
		}
		anchor := ""
		for j := i - 1; j >= 0 && anchor == ""; j-- {
			if tr, ok := present[trigDefs[j]["name"]]; ok && same(trigDefs[j], tr) && same(r, tr) {
				anchor = "FOLLOWS " + qi(tr.Name)
			}
		}
		for j := i + 1; j < len(trigDefs) && anchor == ""; j++ {
			if tr, ok := present[trigDefs[j]["name"]]; ok && same(trigDefs[j], tr) && same(r, tr) {
				anchor = "PRECEDES " + qi(tr.Name)
			}
		}
		if err := create(anchored(r["SQL Original Statement"], anchor), r["sql_mode"], r["collation_connection"]); err != nil {
			missing = append(missing, fmt.Sprintf("trigger %s (%v)", r["name"], err))
			continue
		}
		if t := dstTable[r["name"]]; t != "" && t != r["table"] {
			again = append(again, fmt.Sprintf("trigger %s (on table %s, as in the pristine copy; it was on %s)", r["name"], r["table"], t))
		} else {
			again = append(again, "trigger "+r["name"])
		}
		triggersAgain = true
		present[r["name"]] = dbx.MySQLTrigger{Name: r["name"], Table: r["table"], Timing: r["timing"], Event: r["event"]}
	}
	// A view may use another view: create them until no more succeed.
	left := map[string]error{}
	for _, d := range dropped {
		if d.kind == "view" && views[d.name].def != "" {
			left[d.name] = nil
		}
	}
	for progress := true; progress && len(left) > 0; {
		progress = false
		for name := range left {
			v := views[name]
			if err := create(v.def, mysqlSession.SQLMode, v.collation); err != nil {
				left[name] = err
				continue
			}
			delete(left, name)
			again = append(again, "view "+name)
			progress = true
		}
	}
	for name, err := range left {
		missing = append(missing, fmt.Sprintf("view %s (%v)", name, err))
	}
	sort.Strings(again)
	sort.Strings(missing)
	sort.Strings(gone)
	more := ""
	if len(again) > 0 {
		more += fmt.Sprintf(". Dropped before the refusal and made again from the pristine copy: %s", strings.Join(again, ", "))
		if triggersAgain {
			more += " (each trigger next to its pristine neighbour; where the working copy's triggers of a table were reordered, or others stand between, the firing order may differ from the pristine one until " + recovery + ")"
		}
	}
	if len(missing) > 0 {
		more += fmt.Sprintf(". Now missing from %s until %s: %s", dst, recovery, strings.Join(missing, "; "))
	}
	if len(gone) > 0 {
		more += fmt.Sprintf(". Dropped from %s, as the pristine copy has nothing of that name, which no restore makes again: %s", dst, strings.Join(gone, ", "))
	}
	return fmt.Errorf("%w%s", refused, more)
}

// notMade adds to err, a failure after the drops, the sequences (seqs),
// triggers and views that are not made yet: missing from dst until
// recovery (made again from the pristine copy, or a new seed). A lost session (lost) is returned in err's
// place, with them.
func notMade(dst, recovery string, err, lost error, seqs []string, trigDefs []map[string]string, views map[string]viewDef) error {
	if lost != nil {
		err = lost
	}
	var names []string
	for _, q := range seqs {
		names = append(names, "sequence "+q)
	}
	for _, r := range trigDefs {
		names = append(names, "trigger "+r["name"])
	}
	var vs []string
	for name := range views {
		vs = append(vs, "view "+name)
	}
	sort.Strings(vs)
	names = append(names, vs...)
	if len(names) == 0 {
		return err
	}
	return fmt.Errorf("%w. Not made, so missing from %s until %s: %s", err, dst, recovery, strings.Join(names, ", "))
}

// forEachRow and firingOrder match, at the start of the text, FOR EACH ROW
// and the firing order that may follow it.
var (
	forEachRow  = lazy.Regexp(`(?i)^FOR\s+EACH\s+ROW\s+`)
	firingOrder = lazy.Regexp("(?i)^(?:FOLLOWS|PRECEDES)\\s+(?:`(?:[^`]|``)*`|[^\\s`]+)\\s+")
)

// anchored is a trigger's CREATE statement with its firing order set to
// anchor (FOLLOWS x, PRECEDES x), or with none when anchor is empty. The
// clause goes after FOR EACH ROW, found outside quoted names and strings:
// a name before it may hold those words in backticks, and the body comes
// after it.
func anchored(stmt, anchor string) string {
	for i := 0; i < len(stmt); i++ {
		if q := stmt[i]; q == '`' || q == '\'' || q == '"' {
			for i++; i < len(stmt); i++ {
				if stmt[i] == '\\' && q != '`' {
					i++
				} else if stmt[i] == q {
					if i+1 < len(stmt) && stmt[i+1] == q { // doubled
						i++
						continue
					}
					break
				}
			}
			continue
		}
		if i > 0 && isWordByte(stmt[i-1]) {
			continue
		}
		m := forEachRow().FindStringIndex(stmt[i:])
		if m == nil {
			continue
		}
		j := i + m[1]
		k := j
		if o := firingOrder().FindStringIndex(stmt[j:]); o != nil {
			k = j + o[1]
		}
		if anchor != "" {
			anchor += " "
		}
		return stmt[:j] + anchor + stmt[k:]
	}
	return stmt
}

// isWordByte reports a byte of an unquoted name or keyword.
func isWordByte(b byte) bool {
	return b == '_' || b == '$' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || b >= 0x80
}

// viewDef is a view's CREATE statement and the collation it was made with.
type viewDef struct{ def, collation string }

// setBack records that c's session could not be set back after a SET
// failed with err: the session may be half changed, so c is closed instead
// of put back into the pool.
func setBack(c *sql.Conn, lost *error, err error) error {
	*lost = fmt.Errorf("setting the session back: %w", err)
	discardConn(c)
	return *lost
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

// Restore makes the items of the working database as the pristine one has
// them; with nil, every item that differs. A restore the server stops for
// a lock runs again as a whole
// (retryLocked): it copies the items from the pristine database whatever
// the try before left.
func (m *MySQL) Restore(ctx context.Context, items []string) error {
	return m.retryLocked(ctx, "restore", func(ctx context.Context) error { return m.restore(ctx, items) })
}

func (m *MySQL) restore(ctx context.Context, items []string) error {
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
	extra, err := m.copyObjects(ctx, c, m.o.Pristine, m.o.Working, tables, views, sequences, triggers, nil)
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

// mysqlMarkNote is the note of the mark of a pristine copy of working.
func mysqlMarkNote(working string) string {
	return pristineComment + " of " + working + ", made by driftgate seed and replaced by the next one"
}

// Seed copies the working database into a new pristine database. Excluded
// and restore_empty tables are copied without rows, so views that use them still work. The
// mark (pristineTable) is made first, so a seed that stops halfway leaves a
// database the next seed may drop; DriftGate's record of the pristine
// fingerprint is removed before a seed (capture.Seed), so such a database is
// not taken for a pristine copy. A seed the server stops for a lock runs
// again as a whole (retryLocked), as a second seed would.
func (m *MySQL) Seed(ctx context.Context) error {
	return m.retryLocked(ctx, "seed", m.seed)
}

func (m *MySQL) seed(ctx context.Context) error {
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
	// A sequence a Galera node cannot copy is told before the old copy is
	// dropped (copyObjects checks again).
	if err := m.galeraSequences(ctx, c, m.o.Working, objs.sequences); err != nil {
		return err
	}
	// The triggers and views are made again in the pristine copy: the
	// login is tried on their DEFINERs before the old copy is dropped, in
	// the working database (the privileges tried are global ones). Their
	// definitions are read once, for the probe and the copy (known).
	var defReqs []showReq
	var items []probeItem
	for _, tr := range objs.triggers {
		defReqs, items = append(defReqs, showReq{"TRIGGER", m.o.Working, tr.Name}), append(items, probeItem{kind: "trigger", name: tr.Name, table: tr.Table})
	}
	for _, v := range objs.views {
		defReqs, items = append(defReqs, showReq{"VIEW", m.o.Working, v}), append(items, probeItem{kind: "view", name: v})
	}
	defs, err := m.showAll(ctx, c, defReqs)
	if err != nil {
		return err
	}
	known := map[showReq]map[string]string{}
	var probes []probeItem
	for i, it := range items {
		known[defReqs[i]] = defs[i]
		if r := defs[i]; r != nil {
			it.stmt = r["SQL Original Statement"]
			if it.kind == "view" {
				it.stmt = r["Create View"]
			}
			probes = append(probes, it)
		}
	}
	if err := m.definerProbe(ctx, c, m.o.Working, m.o.Pristine, objs.allTables(), objs.probes, probes); err != nil {
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
		"CREATE TABLE " + mark + " (id TINYINT NOT NULL PRIMARY KEY, note VARCHAR(200) CHARACTER SET utf8mb4 NOT NULL)",
	} {
		if _, err := c.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	// The note names the working database: clean drops only a copy of
	// this one.
	if _, err := c.ExecContext(ctx, "INSERT INTO "+mark+" VALUES (1, ?)", mysqlMarkNote(m.o.Working)); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "USE "+qi(m.o.Pristine)); err != nil {
		return err
	}
	copied := append([]string{}, objs.tables...)
	empty := append([]string{}, objs.excludedTables...)
	for _, t := range objs.restoreOnly {
		if m.o.restoreEmpty(t) {
			empty = append(empty, t)
		} else {
			copied = append(copied, t)
		}
	}
	reqs := make([]showReq, len(empty))
	for i, t := range empty {
		reqs[i] = showReq{"TABLE", m.o.Working, t}
	}
	shown, err := m.showAll(ctx, c, reqs)
	if err != nil {
		return err
	}
	for i, t := range empty {
		def, err := dbx.ShowCreateStatement("TABLE", t, shown[i])
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, dbx.MySQLCreateTable(def)); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	var trigs []string
	for _, tr := range objs.triggers {
		trigs = append(trigs, tr.Name)
	}
	_, err = m.copyObjects(ctx, c, m.o.Working, m.o.Pristine, copied, objs.views, objs.sequences, trigs, known)
	return err
}

// SeededFingerprint is the fingerprint of the pristine database, what a
// seed records: the tables are copied one by one, and the application may
// have changed the working database while they were and since. The items
// are named as the working database's; the mark (pristineTable) is none of
// them, and the counters the working database's fingerprints start from
// are left as they were. The record then always matches the copy, but a
// copy the application wrote during can hold one table from before a
// write and another from after it: a seed still runs while the
// application does not write.
func (m *MySQL) SeededFingerprint(ctx context.Context) (Fingerprint, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ok, err := dbx.MySQLDatabaseExists(ctx, c, m.o.Pristine)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("state %s: pristine database %s does not exist; run driftgate seed", m.o.Name, m.o.Pristine)
	}
	return m.fingerprint(ctx, c, m.o.Pristine, nil)
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

func (m *MySQL) Rows(ctx context.Context, items []string) (out Rows, err error) {
	err = m.retryLocked(ctx, "read of rows", func(ctx context.Context) error {
		out, err = m.rows(ctx, items)
		return err
	})
	return out, err
}

func (m *MySQL) rows(ctx context.Context, items []string) (Rows, error) {
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
func (m *MySQL) Query(ctx context.Context, query string) (out []string, err error) {
	if err := CheckQuery(query); err != nil {
		return nil, err
	}
	err = m.retryLocked(ctx, "query", func(ctx context.Context) error {
		out, err = m.query(ctx, query)
		return err
	})
	return out, err
}

func (m *MySQL) query(ctx context.Context, query string) ([]string, error) {
	c, err := m.session(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	// The user's query may change the session (a function that sets a
	// variable, the time zone): sessions are prepared once (initConn), so
	// this one is closed, never put back into the pool.
	defer discardConn(c)
	if _, err := c.ExecContext(ctx, "USE "+qi(m.o.Pristine)); err != nil {
		return nil, fmt.Errorf("pristine database %s: %w (run driftgate seed)", m.o.Pristine, err)
	}
	// The session stays read-only too, should the transaction end early
	// (an implicit commit).
	if _, err := c.ExecContext(ctx, "SET SESSION TRANSACTION READ ONLY"); err != nil {
		return nil, err
	}
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
