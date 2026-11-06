package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/lazy"
)

// Per-table change signals let a fingerprint checksum again only the tables
// that were written since the last one, instead of every table after any
// write. They come from server settings DriftGate reads but never changes:
//
//   - MySQL: each table's UPDATE_TIME, which InnoDB sets at commit for every
//     table a transaction changed, and the write counts of
//     performance_schema.table_io_waits_summary_by_table, when
//     performance_schema is ON and table I/O is instrumented for the working
//     database and for every connection;
//   - MariaDB: information_schema.TABLE_STATISTICS, when userstat is ON.
//
// Measured on MySQL 8.4 and MariaDB 11.4: a trigger's writes count on the
// table it writes; rows that a foreign key changes (ON DELETE/UPDATE CASCADE,
// SET NULL) are not counted on the child table, so the tables such a key
// reaches are checksummed with their parent; MariaDB does not count INSERT
// IGNORE, whose only trace is AUTO_INCREMENT, read with the counters; MySQL
// counts a write that runs as part of a join (UPDATE with a subquery, a
// multi-table UPDATE, INSERT … SELECT) as a read or not at all, which its
// UPDATE_TIME does not miss; TRUNCATE, ALTER, RENAME, CREATE and DROP move the
// server's Com_ counters (prepared or not), and then everything is read again.

// tableState is a reading of the per-table change signals.
type tableState struct {
	signal map[string]string // base table → counters, AUTO_INCREMENT, creation and update time
	at     int64             // server time (Unix seconds) the update times were read at
	update map[string]int64  // base table → UPDATE_TIME (Unix seconds), where the server keeps it
	always map[string]bool   // tables whose signals say nothing (partitioned tables on MySQL)
}

// mariadb reports whether the server is MariaDB; m.server and m.version
// are read once.
func (m *MySQL) mariadb(ctx context.Context, c *sql.Conn) (bool, error) {
	if m.server == "" {
		if err := c.QueryRowContext(ctx, "SELECT VERSION()").Scan(&m.version); err != nil {
			return false, err
		}
		m.server = dbx.FlavorMySQL
		if dbx.IsMariaDB(m.version) {
			m.server = dbx.FlavorMariaDB
		}
	}
	return m.server == dbx.FlavorMariaDB, nil
}

// tableCounters reads the per-table change signals of the database. A nil
// state comes with the reason the signals cannot be trusted.
func (m *MySQL) tableCounters(ctx context.Context, c *sql.Conn, db string) (*tableState, string, error) {
	if m.noTables {
		return nil, "turned off", nil
	}
	if _, err := m.mariadb(ctx, c); err != nil {
		return nil, "", err
	}
	counts := map[string]string{}
	var query string
	if m.server == dbx.FlavorMariaDB {
		var on sql.NullInt64
		if err := c.QueryRowContext(ctx, "SELECT @@userstat").Scan(&on); err != nil || on.Int64 != 1 {
			return nil, "userstat is OFF", nil
		}
		query = "SELECT TABLE_NAME, CONCAT(ROWS_CHANGED, '/', ROWS_CHANGED_X_INDEXES) FROM information_schema.TABLE_STATISTICS WHERE TABLE_SCHEMA = ?"
	} else {
		if why, err := mysqlCountersOff(ctx, c, db); why != "" || err != nil {
			return nil, why, err
		}
		query = "SELECT OBJECT_NAME, CONCAT(COUNT_INSERT, '/', COUNT_UPDATE, '/', COUNT_DELETE) FROM performance_schema.table_io_waits_summary_by_table WHERE OBJECT_SCHEMA = ? AND OBJECT_TYPE = 'TABLE'"
	}
	if err := scanPairs(ctx, c, counts, query, db); err != nil {
		return nil, "", err
	}
	st := &tableState{signal: map[string]string{}, update: map[string]int64{}, always: map[string]bool{}}
	rows, err := c.QueryContext(ctx, `SELECT UNIX_TIMESTAMP(), TABLE_NAME, COALESCE(AUTO_INCREMENT, '-'), COALESCE(CREATE_TIME, '-'),
		UNIX_TIMESTAMP(UPDATE_TIME), COALESCE(CREATE_OPTIONS, '')
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`, db)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var now sql.NullFloat64
		var name, ai, created, options string
		var updated sql.NullFloat64
		if err := rows.Scan(&now, &name, &ai, &created, &updated, &options); err != nil {
			return nil, "", err
		}
		st.at = int64(now.Float64)
		n, ok := counts[name]
		if !ok {
			n = "-" // not opened since the server started
		}
		up := "-"
		if updated.Valid {
			st.update[name] = int64(updated.Float64)
			up = fmt.Sprint(st.update[name])
		}
		st.signal[name] = n + "|" + ai + "|" + created + "|" + up
		if m.server == dbx.FlavorMySQL && strings.Contains(strings.ToLower(options), "partitioned") {
			st.always[name] = true // no UPDATE_TIME
		}
	}
	return st, "", rows.Err()
}

// mysqlCountersOff returns why MySQL's per-table signals cannot be trusted
// (statsExpiryOff, performanceSchemaOff), or "" when they can. One query
// reads all that the two read one query at a time; when it fails, they are
// asked in turn, for the reason they give.
func mysqlCountersOff(ctx context.Context, c *sql.Conn, db string) (string, error) {
	cols := []string{"@@session.information_schema_stats_expiry", "@@performance_schema"}
	var args []any
	for _, ch := range performanceSchemaChecks {
		cols = append(cols, "("+ch.query+")")
		if strings.Contains(ch.query, "?") {
			args = append(args, db)
		}
	}
	var expiry, on sql.NullInt64
	offs := make([]sql.NullBool, len(performanceSchemaChecks))
	dest := []any{&expiry, &on}
	for i := range offs {
		dest = append(dest, &offs[i])
	}
	if err := c.QueryRowContext(ctx, "SELECT "+strings.Join(cols, ", "), args...).Scan(dest...); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if why := statsExpiryOff(ctx, c); why != "" {
			return why, nil
		}
		return performanceSchemaOff(ctx, c, db)
	}
	switch {
	case !expiry.Valid || expiry.Int64 != 0:
		return statsExpiryWhy, nil
	case on.Int64 != 1:
		return "performance_schema is OFF", nil
	}
	for i, ch := range performanceSchemaChecks {
		if offs[i].Valid && offs[i].Bool {
			return ch.why, nil
		}
	}
	return "", nil
}

const statsExpiryWhy = "information_schema_stats_expiry cannot be 0 for this session"

// statsExpiryOff returns why UPDATE_TIME may be stale, or "": from the
// statistics cache it can be a day old.
func statsExpiryOff(ctx context.Context, c *sql.Conn) string {
	var expiry sql.NullInt64
	if err := c.QueryRowContext(ctx, "SELECT @@session.information_schema_stats_expiry").Scan(&expiry); err != nil || !expiry.Valid || expiry.Int64 != 0 {
		return statsExpiryWhy
	}
	return ""
}

// performanceSchemaOff returns why MySQL's table I/O counters may miss a
// write, or "" when every write to the database is counted.
func performanceSchemaOff(ctx context.Context, c *sql.Conn, db string) (string, error) {
	var on sql.NullInt64
	if err := c.QueryRowContext(ctx, "SELECT @@performance_schema").Scan(&on); err != nil || on.Int64 != 1 {
		return "performance_schema is OFF", nil
	}
	for _, ch := range performanceSchemaChecks {
		var args []any
		if strings.Contains(ch.query, "?") {
			args = append(args, db)
		}
		var off sql.NullBool
		if err := c.QueryRowContext(ctx, ch.query, args...).Scan(&off); err != nil {
			return "performance_schema is not readable: " + err.Error(), nil
		}
		if off.Valid && off.Bool {
			return ch.why, nil
		}
	}
	return "", nil
}

// performanceSchemaChecks are the settings under which MySQL's table I/O
// counters may miss a write: each query returns true when they may.
var performanceSchemaChecks = []struct{ why, query string }{
	{"the instrument wait/io/table/sql/handler is disabled", "SELECT COUNT(*) = 0 FROM performance_schema.setup_instruments WHERE NAME = 'wait/io/table/sql/handler' AND ENABLED = 'YES'"},
	{"the consumer global_instrumentation is disabled", "SELECT COUNT(*) = 0 FROM performance_schema.setup_consumers WHERE NAME = 'global_instrumentation' AND ENABLED = 'YES'"},
	{"setup_objects does not instrument every table of the database",
		"SELECT SUM(ENABLED = 'NO') > 0 OR SUM(ENABLED = 'YES' AND OBJECT_NAME = '%') = 0 OR COUNT(*) = 0 FROM performance_schema.setup_objects WHERE OBJECT_TYPE = 'TABLE' AND OBJECT_SCHEMA IN (?, '%')"},
	{"setup_actors does not instrument every connection",
		"SELECT SUM(ENABLED = 'NO') > 0 OR SUM(ENABLED = 'YES' AND HOST = '%' AND USER = '%' AND ROLE = '%') = 0 OR COUNT(*) = 0 FROM performance_schema.setup_actors"},
	{"setup_threads does not instrument connections", "SELECT COUNT(*) > 0 FROM performance_schema.setup_threads WHERE NAME = 'thread/sql/one_connection' AND ENABLED = 'NO'"},
	{"a connection is not instrumented", "SELECT COUNT(*) > 0 FROM performance_schema.threads WHERE TYPE = 'FOREGROUND' AND INSTRUMENTED = 'NO'"},
	{"performance_schema lost tables (performance_schema_max_table_instances or _handles is too small)",
		"SELECT COALESCE(SUM(VARIABLE_VALUE), 0) > 0 FROM performance_schema.global_status WHERE VARIABLE_NAME IN ('Performance_schema_table_instances_lost', 'Performance_schema_table_handles_lost')"},
}

func scanPairs(ctx context.Context, c *sql.Conn, out map[string]string, query string, args ...any) error {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		out[k] = v.String
	}
	return rows.Err()
}

// rowCounters are the write counters that move with row changes only; the
// per-table counters tell which tables those were. Any other counter that
// moves (ALTER, TRUNCATE, RENAME, a stored procedure …) means everything is
// read again.
var rowCounters = map[string]bool{
	"Handler_write": true, "Handler_update": true, "Handler_delete": true,
	"Innodb_rows_inserted": true, "Innodb_rows_updated": true, "Innodb_rows_deleted": true,
	"Com_insert": true, "Com_insert_select": true, "Com_replace": true, "Com_replace_select": true,
	"Com_update": true, "Com_update_multi": true, "Com_delete": true, "Com_delete_multi": true,
}

// onlyRowWrites reports whether only row counters moved between two reads.
func onlyRowWrites(before, after map[string]int64) bool {
	if len(before) != len(after) {
		return false
	}
	for name, n := range after {
		if b, ok := before[name]; !ok || (b != n && !rowCounters[name]) {
			return false
		}
	}
	return true
}

// cascades maps every table to the tables whose rows its foreign keys
// change (ON DELETE/UPDATE CASCADE, SET NULL, SET DEFAULT), and lists the
// tables such a key changes from a parent in another database.
func cascades(ctx context.Context, c *sql.Conn, db string) (map[string][]string, []string, error) {
	rows, err := c.QueryContext(ctx, `SELECT TABLE_NAME, REFERENCED_TABLE_NAME, UNIQUE_CONSTRAINT_SCHEMA FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = ? AND (UPDATE_RULE IN ('CASCADE', 'SET NULL', 'SET DEFAULT') OR DELETE_RULE IN ('CASCADE', 'SET NULL', 'SET DEFAULT'))`, db)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	children := map[string][]string{}
	var foreign []string
	for rows.Next() {
		var child, parent, schema string
		if err := rows.Scan(&child, &parent, &schema); err != nil {
			return nil, nil, err
		}
		if schema != db {
			foreign = append(foreign, child)
			continue
		}
		children[parent] = append(children[parent], child)
	}
	return children, foreign, rows.Err()
}

// written returns the tables to checksum again: those whose signals differ
// between the two readings or that were updated in or after the second of
// the first one, the tables their foreign keys reach, and the tables a key
// in another database reaches. ok is false when a table appeared or went
// away; known lists tables whose change is accounted for elsewhere.
func written(ctx context.Context, c *sql.Conn, db string, before, after *tableState, known map[string]bool) (map[string]bool, bool, error) {
	out := map[string]bool{}
	for t, s := range after.signal {
		old, ok := before.signal[t]
		if !ok && !known[t] {
			return nil, false, nil
		}
		if up, has := after.update[t]; !ok || old != s || (has && up >= before.at) || after.always[t] {
			out[t] = true
		}
	}
	for t := range before.signal {
		if _, ok := after.signal[t]; !ok && !known[t] {
			return nil, false, nil
		}
	}
	children, foreign, err := cascades(ctx, c, db)
	if err != nil {
		return nil, false, err
	}
	queue := make([]string, 0, len(out)+len(known)+len(foreign))
	for t := range out {
		queue = append(queue, t)
	}
	for t := range known {
		queue = append(queue, t)
	}
	for _, t := range foreign {
		out[t] = true
		queue = append(queue, t)
	}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		for _, ch := range children[t] {
			if !out[ch] {
				out[ch] = true
				queue = append(queue, ch)
			}
		}
	}
	return out, true, nil
}

// openWrites reports whether a connection holds uncommitted changes:
// CHECKSUM TABLE does not see them, and their commit moves no counter, so
// no fingerprint read now may be reused. An error means it is not known.
// The transactions come from SHOW ENGINE INNODB STATUS: INNODB_TRX is a copy
// InnoDB refreshes only when nobody read it for 100 ms, and with several
// readers it shows a transaction committed long ago, or misses an open one.
// Only its TRANSACTIONS section counts: the latest foreign key error and
// deadlock, which the server keeps until it restarts, show the
// transactions they ended, long gone, the same way.
func openWrites(ctx context.Context, c *sql.Conn) (bool, error) {
	var typ, name, status string
	if err := c.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status); err != nil {
		return false, err
	}
	return openWritesIn(status)
}

// openWritesIn reads the open writes from the text of SHOW ENGINE INNODB
// STATUS (openWrites).
func openWritesIn(status string) (bool, error) {
	start := strings.Index(status, "\nTRANSACTIONS\n")
	if !strings.Contains(status, "END OF INNODB MONITOR OUTPUT") || strings.Contains(status, "truncated") || start < 0 {
		return false, errors.New("InnoDB status incomplete")
	}
	status = status[start:]
	if end := strings.Index(status, "\nFILE I/O\n"); end > 0 {
		status = status[:end]
	}
	return undoEntries().MatchString(status), nil
}

// undoEntries matches a transaction that changed rows.
var undoEntries = lazy.Regexp(`undo log entries [1-9]`)

// sessionWrites reads this connection's own statement counters.
func sessionWrites(ctx context.Context, c *sql.Conn) (map[string]int64, error) {
	rows, err := c.QueryContext(ctx, `SHOW SESSION STATUS LIKE 'Com\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		var n int64
		fmt.Sscan(value, &n)
		out[name] = n
	}
	return out, rows.Err()
}

// setMode records how fingerprints are taken and says so when it changes.
func (m *MySQL) setMode(mode string) {
	if mode != m.mode {
		m.mode = mode
		fmt.Fprintf(m.o.Log, "state %s: %s\n", m.o.Name, mode)
	}
}

// Mode says how the last full fingerprint knew which tables to read.
func (m *MySQL) Mode() string { return m.mode }

func (m *MySQL) counterSource() string {
	if m.server == dbx.FlavorMariaDB {
		return "per table (MariaDB userstat, information_schema.TABLE_STATISTICS)"
	}
	return "per table (MySQL UPDATE_TIME and performance_schema table I/O counters)"
}

// forget drops the last fingerprint: the next one reads everything.
func (m *MySQL) forget() { m.last, m.writes, m.tables, m.dirty = nil, nil, nil, nil }

// keep makes f, without the dirty items, the fingerprint the next one starts
// from, when no other connection holds uncommitted changes. Without
// per-table counters the write check alone decides, as it always has.
func (m *MySQL) keep(ctx context.Context, c *sql.Conn, f Fingerprint, writes map[string]int64, tables *tableState, dirty []string) {
	m.forget()
	if writes == nil {
		return
	}
	open, err := openWrites(ctx, c)
	switch {
	case err == nil && open:
		return
	case err != nil:
		tables = nil // the per-table path needs to know
	}
	m.last, m.writes, m.tables, m.dirty = maps.Clone(f), writes, tables, dirty
}

// refresh reads the per-table signals again when nothing was written since
// the last fingerprint but some table was updated in the second it was read
// (written takes those for changed): with the write counters unchanged
// after the new reading too, no write came before it.
func (m *MySQL) refresh(ctx context.Context, c *sql.Conn) {
	if m.tables == nil {
		return
	}
	recent := false
	for _, up := range m.tables.update {
		recent = recent || up >= m.tables.at
	}
	if !recent {
		return
	}
	tables, _, err := m.tableCounters(ctx, c, m.o.Working)
	if err != nil || tables == nil {
		return
	}
	if writes, err := writeCounters(ctx, c); err == nil && maps.Equal(writes, m.writes) {
		m.tables = tables
	}
}

// incremental checksums again only the tables written since the last
// fingerprint (and the ones their foreign keys reach) and the dirty items.
// ok is false when the counters do not say enough.
func (m *MySQL) incremental(ctx context.Context, c *sql.Conn, tables *tableState) (Fingerprint, bool, error) {
	changed, ok, err := written(ctx, c, m.o.Working, m.tables, tables, nil)
	if err != nil || !ok {
		return nil, false, err
	}
	items := append(append([]string{}, m.always...), m.dirty...)
	for _, it := range m.dirty {
		if t, ok := strings.CutPrefix(it, "table:"); ok {
			changed[t] = true
		}
	}
	m.checked = []string{}
	for t := range changed {
		items = append(items, "table:"+t)
		m.checked = append(m.checked, t)
	}
	sort.Strings(m.checked)
	f := maps.Clone(m.last)
	if len(items) > 0 {
		part, err := m.fingerprint(ctx, c, m.o.Working, items)
		if err != nil {
			return nil, false, err
		}
		maps.Copy(f, part)
	}
	return f, true, nil
}

// afterRestore keeps the last fingerprint after a restore of the given
// items, so the next one does not read everything again because of the
// restore's own writes: the restored items, and the tables written since,
// are left to the next fingerprint. Every statement other than a row write
// since the last fingerprint must be this connection's (sess0 is its
// counters before the restore) or one the restore's other connections made
// (extra, nil when not known); otherwise, or when anything is unclear, the
// next fingerprint reads everything.
func (m *MySQL) afterRestore(ctx context.Context, c *sql.Conn, items []string, sess0, extra map[string]int64) error {
	if m.last == nil || m.tables == nil || sess0 == nil || extra == nil {
		m.forget()
		return nil
	}
	writes, err := writeCounters(ctx, c)
	if err != nil {
		m.forget()
		return nil
	}
	sess1, err := sessionWrites(ctx, c)
	if err != nil || len(writes) != len(m.writes) {
		m.forget()
		return nil
	}
	for name, n := range writes {
		if !rowCounters[name] && n-m.writes[name] != sess1[name]-sess0[name]+extra[name] {
			m.forget()
			return nil
		}
	}
	tables, _, err := m.tableCounters(ctx, c, m.o.Working)
	if err != nil || tables == nil {
		m.forget()
		return nil
	}
	restored, _, _, _ := splitItems(items)
	known := map[string]bool{}
	for _, t := range restored {
		known[t] = true
	}
	changed, ok, err := written(ctx, c, m.o.Working, m.tables, tables, known)
	if err != nil || !ok {
		m.forget()
		return err
	}
	dirty := append(append([]string{}, m.dirty...), items...)
	for t := range changed {
		dirty = append(dirty, "table:"+t)
	}
	f := maps.Clone(m.last)
	for _, it := range dirty {
		delete(f, it)
	}
	if m.always, err = uncounted(ctx, c, m.o.Working); err != nil {
		m.forget()
		return nil
	}
	m.keep(ctx, c, f, writes, tables, dirty)
	return nil
}
