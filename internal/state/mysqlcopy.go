package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/dbx"
)

// Copying tables, for a restore and a seed:
//
//   - the tables are copied over up to copyConns connections at the same
//     time. Tables a foreign key joins, in either database, are copied one
//     after another on one connection: a parent and its child copied at the
//     same time wait for each other's locks (measured: Lock wait timeout);
//   - a table whose definition did not change, with a one-column integer
//     primary key and at least rowDiffMin rows, is not emptied and copied
//     whole: both copies are hashed in chunks of rowDiffChunk keys, and only
//     the chunks that differ are deleted and copied again. The chunk hash
//     (CRC32 of each row's text) can miss a change: a collision, or values
//     the server prints alike (FLOAT). So the table is then checked as the
//     fingerprint checks it (SHOW CREATE TABLE and CHECKSUM TABLE … EXTENDED,
//     or storedChecksum, against the pristine copy), and copied whole when
//     the two differ or anything in the chunked copy fails.
var copyConns = 4

var rowDiffMin int64 = 20000

const rowDiffChunk = 1024

// rowDiffMiss, set by tests, drops chunks from the ones the hashes found
// different, as a hash that misses a change would.
var rowDiffMiss func(chunk int64) bool

// rowDiffHash, set by tests, is called when rowDiff starts the hash of the
// source table.
var rowDiffHash func(table string)

// tableJob is one table to make equal to its source.
type tableJob struct {
	t          string
	sdef, ddef string     // definitions; ddef is empty when dst has no such table
	info       *tableInfo // what copying it reads of src.t; nil: read when needed
}

// tableInfo is what copying a table reads of its source, for all tables
// of a copy in one query (tableInfos) instead of several for each.
type tableInfo struct {
	rows   int64    // TABLE_ROWS, the server's estimate
	pk     []string // the primary key's columns
	pkType string   // the type of its (last) column, lower case
	all    []string // every column in order, quoted
	stored []string // the columns that are not generated, quoted (copyTable)
}

// tableInfos reads the tableInfo of the tables of db, those it finds.
func tableInfos(ctx context.Context, c *sql.Conn, db string, tables []string) (map[string]*tableInfo, error) {
	out := map[string]*tableInfo{}
	if len(tables) == 0 {
		return out, nil
	}
	args := []any{db}
	marks := make([]string, len(tables))
	for i, t := range tables {
		marks[i] = "?"
		args = append(args, t)
	}
	rows, err := c.QueryContext(ctx, `SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.EXTRA, k.COLUMN_NAME IS NOT NULL, COALESCE(t.TABLE_ROWS, 0)
		FROM information_schema.TABLES t
		JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME
		LEFT JOIN information_schema.KEY_COLUMN_USAGE k ON k.TABLE_SCHEMA = c.TABLE_SCHEMA AND k.TABLE_NAME = c.TABLE_NAME
			AND k.COLUMN_NAME = c.COLUMN_NAME AND k.CONSTRAINT_NAME = 'PRIMARY'
		WHERE t.TABLE_SCHEMA = ? AND t.TABLE_NAME IN (`+strings.Join(marks, ", ")+`)
		ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var table, col, typ string
		var extra sql.NullString
		var key bool
		var n int64
		if err := rows.Scan(&table, &col, &typ, &extra, &key, &n); err != nil {
			return nil, err
		}
		in := out[table]
		if in == nil {
			in = &tableInfo{rows: n}
			out[table] = in
		}
		in.all = append(in.all, qi(col))
		// As columns leaves them out: EXTRA NOT LIKE … (NULL is left out).
		if x := strings.ToUpper(extra.String); extra.Valid && !strings.Contains(x, "VIRTUAL GENERATED") &&
			!strings.Contains(x, "STORED GENERATED") && !strings.Contains(x, "PERSISTENT GENERATED") {
			in.stored = append(in.stored, qi(col))
		}
		if key {
			in.pk, in.pkType = append(in.pk, col), strings.ToLower(typ)
		}
	}
	return out, rows.Err()
}

// copyTables runs the jobs on c and on extra connections of its own. It
// returns the statement counters (Com_) the extra connections moved, which
// the write check must count as DriftGate's own (see afterRestore), or nil
// when they are not known.
func (m *MySQL) copyTables(ctx context.Context, c *sql.Conn, src, dst string, jobs []tableJob) (map[string]int64, error) {
	if _, err := m.mariadb(ctx, c); err != nil { // before the connections read it
		return nil, err
	}
	// One read of what copying reads of each table; a table it does not
	// find, or all of them when it fails, are read as before.
	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.t
	}
	if infos, err := tableInfos(ctx, c, src, names); err == nil {
		for i := range jobs {
			jobs[i].info = infos[jobs[i].t]
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	groups := [][]tableJob{jobs}
	if copyConns > 1 && len(jobs) > 1 {
		groups = fkGroups(ctx, c, []string{src, dst}, jobs)
	}
	if len(groups) <= 1 {
		for _, j := range jobs {
			if err := m.copyOne(ctx, c, src, dst, j); err != nil {
				return nil, err
			}
			chaos.Point("state.copy")
		}
		return map[string]int64{}, nil
	}
	// Largest group first, each to the next free connection.
	sizes, _ := tableSizes(ctx, c, src)
	weight := func(g []tableJob) (n int64) {
		for _, j := range g {
			n += sizes[j.t] + 1
		}
		return n
	}
	sort.SliceStable(groups, func(a, b int) bool { return weight(groups[a]) > weight(groups[b]) })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conns := min(copyConns, len(groups))
	next := make(chan []tableJob)
	errs := make([]error, conns)
	moved := make([]map[string]int64, conns)
	var wg sync.WaitGroup
	for k := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if errs[k] != nil {
					cancel()
				}
				for range next {
				}
			}()
			conn := c
			var before map[string]int64
			if k > 0 {
				var err error
				if conn, err = m.session(ctx); err != nil {
					errs[k] = err
					return
				}
				defer conn.Close()
				if _, err := conn.ExecContext(ctx, "USE "+qi(dst)); err != nil {
					errs[k] = err
					return
				}
				before, _ = sessionWrites(ctx, conn)
			}
			for g := range next {
				for _, j := range g {
					if errs[k] = m.copyOne(ctx, conn, src, dst, j); errs[k] != nil {
						return
					}
					chaos.Point("state.copy")
				}
			}
			if before != nil {
				if after, err := sessionWrites(ctx, conn); err == nil {
					moved[k] = map[string]int64{}
					for n, v := range after {
						if d := v - before[n]; d != 0 {
							moved[k][n] = d
						}
					}
				}
			}
		}()
	}
	for _, g := range groups {
		select {
		case next <- g:
		case <-ctx.Done():
		}
	}
	close(next)
	wg.Wait()
	// The first error that is not another connection giving up after it.
	var first error
	for _, err := range errs {
		if err != nil && (first == nil || errors.Is(first, context.Canceled)) {
			first = err
		}
	}
	if first != nil {
		return nil, first
	}
	extra := map[string]int64{}
	for k := 1; k < conns; k++ {
		if moved[k] == nil {
			return nil, nil
		}
		for n, v := range moved[k] {
			extra[n] += v
		}
	}
	return extra, nil
}

// fkGroups splits the jobs into groups of tables that foreign keys join, in
// any of the databases; every job is in exactly one group. When the keys
// cannot be read, all jobs are one group.
func fkGroups(ctx context.Context, c *sql.Conn, dbs []string, jobs []tableJob) [][]tableJob {
	parent := map[string]string{}
	var root func(string) string
	root = func(t string) string {
		p, ok := parent[t]
		if !ok || p == t {
			return t
		}
		r := root(p)
		parent[t] = r
		return r
	}
	for _, db := range dbs {
		rows, err := c.QueryContext(ctx, `SELECT TABLE_NAME, REFERENCED_TABLE_NAME FROM information_schema.REFERENTIAL_CONSTRAINTS
			WHERE CONSTRAINT_SCHEMA = ? AND UNIQUE_CONSTRAINT_SCHEMA = ?`, db, db)
		if err != nil {
			return [][]tableJob{jobs}
		}
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				rows.Close()
				return [][]tableJob{jobs}
			}
			if ra, rb := root(a), root(b); ra != rb {
				parent[ra] = rb
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return [][]tableJob{jobs}
		}
	}
	index := map[string]int{}
	var out [][]tableJob
	for _, j := range jobs {
		r := root(j.t)
		i, ok := index[r]
		if !ok {
			i = len(out)
			index[r] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], j)
	}
	return out
}

// tableSizes reads the size of every table of db (data and indexes).
func tableSizes(ctx context.Context, c *sql.Conn, db string) (map[string]int64, error) {
	sizes := map[string]int64{}
	rows, err := c.QueryContext(ctx, "SELECT TABLE_NAME, COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", db)
	if err != nil {
		return sizes, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return sizes, err
		}
		sizes[name] = n
	}
	return sizes, rows.Err()
}

// copyOne makes dst.t equal to src.t: by chunks (rowDiff), or by emptying
// or creating it again and copying every row; then sets AUTO_INCREMENT.
func (m *MySQL) copyOne(ctx context.Context, c *sql.Conn, src, dst string, j tableJob) error {
	t := j.t
	same := j.ddef != "" && autoIncrement().ReplaceAllString(j.sdef, "") == autoIncrement().ReplaceAllString(j.ddef, "")
	if same {
		done, err := m.rowDiff(ctx, c, src, dst, j)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if done {
			return nil
		}
		if err != nil {
			m.logf("state %s: chunked restore of %s: %v; copying it whole\n", m.o.Name, t, err)
		}
		// Same definition: only the rows and the counter are copied.
		if _, err := c.ExecContext(ctx, "TRUNCATE TABLE "+qi(dst)+"."+qi(t)); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	} else {
		if err := dropObject(ctx, c, dst, t, "table"); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, dbx.MySQLCreateTable(j.sdef)); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	if err := j.copyRows(ctx, c, src, dst, ""); err != nil {
		return fmt.Errorf("%s: %w", t, err)
	}
	if err := setAutoIncrement(ctx, c, dst, t, j.sdef); err != nil {
		return err
	}
	return analyze(ctx, c, dst, t)
}

// analyze makes InnoDB compute the table's statistics now. TRUNCATE and
// CREATE TABLE leave the persistent statistics empty (n_rows 0) until the
// server recomputes them in the background, seconds after the copy: a query
// in between gets another plan than on a table loaded from a dump, and a
// query without ORDER BY then returns its rows in another order (measured on
// MariaDB 10.3 and MySQL 8.4: a join driven from the other table). The
// statement changes no data (Com_analyze is not a write, see readCommands)
// and is not logged.
func analyze(ctx context.Context, c *sql.Conn, db, t string) error {
	rows, err := c.QueryContext(ctx, "ANALYZE NO_WRITE_TO_BINLOG TABLE "+qi(db)+"."+qi(t))
	if err != nil {
		return fmt.Errorf("%s: %w", t, err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, op, typ, msg sql.NullString
		if err := rows.Scan(&table, &op, &typ, &msg); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		if strings.EqualFold(typ.String, "error") {
			return fmt.Errorf("%s: ANALYZE TABLE: %s", t, msg.String)
		}
	}
	return rows.Err()
}

func setAutoIncrement(ctx context.Context, c *sql.Conn, db, t, def string) error {
	if ai := autoIncrement().FindStringSubmatch(def); ai != nil {
		if _, err := c.ExecContext(ctx, "ALTER TABLE "+qi(db)+"."+qi(t)+" AUTO_INCREMENT = "+ai[1]); err != nil {
			return err
		}
	}
	return nil
}

func (m *MySQL) logf(format string, args ...any) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	fmt.Fprintf(m.o.Log, format, args...)
}

// purgeCrash reports a server that crashes (signal 11) now and then on
// ALTER TABLE … AUTO_INCREMENT right after a chunk of a table with a VIRTUAL
// generated column was deleted and copied again (MariaDB 10.3.39: one
// restore in four, 10.4.34 at the first; 10.5, 10.6 and 11.4 ran it 12
// times without one): there such a table is copied whole, which deletes no
// row.
func (m *MySQL) purgeCrash() bool {
	var major, minor int
	fmt.Sscanf(m.version, "%d.%d", &major, &minor)
	return m.server == dbx.FlavorMariaDB && (major < 10 || major == 10 && minor < 5)
}

// intTypes are the column types rowDiff takes for a primary key.
var intTypes = map[string]bool{"tinyint": true, "smallint": true, "mediumint": true, "int": true, "bigint": true}

// rowDiff copies only the chunks of src.t whose hash differs in dst.t, and
// checks the result. done is false, with no error, when the table does not
// qualify or most of it changed (a whole copy is then cheaper); false with
// an error when the chunked copy failed or left the table different from
// src.t. Either way dst.t is then copied whole.
func (m *MySQL) rowDiff(ctx context.Context, c *sql.Conn, src, dst string, j tableJob) (done bool, err error) {
	t := j.t
	in := j.info
	if in == nil {
		if in, err = readTableInfo(ctx, c, src, t); err != nil {
			return false, err
		}
	}
	if in.rows < rowDiffMin || virtualColumn().MatchString(j.sdef) && m.purgeCrash() {
		return false, nil
	}
	pk := in.pk
	if len(pk) != 1 || !intTypes[in.pkType] {
		return false, nil
	}
	all := in.all
	if all == nil {
		if all, err = columns(ctx, c, src, t, true); err != nil {
			return false, err
		}
	}

	// The pristine side is read on connections of its own, while this one
	// reads dst: its chunk hashes, and, once the chunks are copied, the
	// fingerprint the result must match.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id := qi(pk[0])
	// Every column with a NULL flag, so NULL and '' differ; two separators
	// and orders, so a value that holds a separator does not shift the rest.
	parts := make([]string, 0, 2*len(all))
	for _, col := range all {
		parts = append(parts, "ISNULL("+col+")", col)
	}
	rev := make([]string, len(parts))
	for i, p := range parts {
		rev[len(parts)-1-i] = p
	}
	// Built by concatenation: a quoted identifier may hold a %.
	query := func(db string) string {
		return "SELECT " + id + " DIV " + strconv.Itoa(rowDiffChunk) + ", COUNT(*), BIT_XOR(CRC32(CONCAT_WS(0x1f, " + strings.Join(parts, ", ") +
			"))), SUM(CRC32(CONCAT_WS(0x1e, " + strings.Join(rev, ", ") + "))) FROM " + qi(db) + "." + qi(t) + " GROUP BY 1"
	}
	var a map[int64]string
	var errA error
	read := make(chan struct{})
	go func() {
		defer close(read)
		cc, err := m.session(ctx)
		if err != nil {
			errA = err
			return
		}
		defer cc.Close()
		a, errA = chunkHashes(ctx, cc, query(src))
	}()
	b, errB := chunkHashes(ctx, c, query(dst))
	<-read
	if err := errors.Join(errA, errB); err != nil {
		return false, err
	}
	var diff []int64
	for k, v := range a {
		if w, ok := b[k]; (!ok || w != v) && (rowDiffMiss == nil || !rowDiffMiss(k)) {
			diff = append(diff, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && (rowDiffMiss == nil || !rowDiffMiss(k)) {
			diff = append(diff, k)
		}
	}
	if len(diff)*4 > len(a)+1 {
		return false, nil
	}
	// Started only now: a table that is copied whole needs no hash, and a
	// query cancelled halfway would close its connection.
	var want string
	var wantErr error
	checked := make(chan struct{})
	if rowDiffHash != nil {
		rowDiffHash(t)
	}
	go func() {
		defer close(checked)
		want, wantErr = m.tableHash(ctx, nil, src, t)
	}()
	defer func() {
		cancel()
		<-checked
	}()
	sort.Slice(diff, func(i, j int) bool { return diff[i] < diff[j] })
	// Every differing chunk is emptied before any is filled: a unique value
	// that moved between chunks is then not in the way.
	for _, k := range diff {
		if _, err := c.ExecContext(ctx, "DELETE FROM "+qi(dst)+"."+qi(t)+" WHERE "+chunkRange(id, k)); err != nil {
			return false, err
		}
	}
	for _, k := range diff {
		if err := j.copyRows(ctx, c, src, dst, chunkRange(id, k)); err != nil {
			return false, err
		}
	}
	if err := setAutoIncrement(ctx, c, dst, t, j.sdef); err != nil {
		return false, err
	}
	got, err := m.tableHash(ctx, c, dst, t)
	if err != nil {
		return false, err
	}
	<-checked
	if wantErr != nil {
		return false, wantErr
	}
	if got != want {
		return false, errors.New("the chunk hashes missed a change")
	}
	return true, nil
}

// readTableInfo reads, as rowDiff always did, the estimated rows of src.t
// and, when there are enough for rowDiff, its primary key; all is left nil
// (read by rowDiff when it needs it).
func readTableInfo(ctx context.Context, c *sql.Conn, src, t string) (*tableInfo, error) {
	in := &tableInfo{}
	if err := c.QueryRowContext(ctx, "SELECT COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", src, t).Scan(&in.rows); err != nil {
		return nil, err
	}
	if in.rows < rowDiffMin {
		return in, nil
	}
	rows, err := c.QueryContext(ctx, `SELECT k.COLUMN_NAME, c.DATA_TYPE FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.COLUMNS c ON c.TABLE_SCHEMA = k.TABLE_SCHEMA AND c.TABLE_NAME = k.TABLE_NAME AND c.COLUMN_NAME = k.COLUMN_NAME
		WHERE k.TABLE_SCHEMA = ? AND k.TABLE_NAME = ? AND k.CONSTRAINT_NAME = 'PRIMARY'`, src, t)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		in.pk, in.pkType = append(in.pk, name), strings.ToLower(typ)
	}
	return in, rows.Err()
}

// copyRows is copyTable with the columns of j.info, when it has them.
func (j tableJob) copyRows(ctx context.Context, c *sql.Conn, src, dst, where string) error {
	if j.info == nil {
		return copyTable(ctx, c, src, dst, j.t, where)
	}
	return copyColumns(ctx, c, src, dst, j.t, where, j.info.stored)
}

// chunkHashes reads chunk → row count and hashes.
func chunkHashes(ctx context.Context, c *sql.Conn, query string) (map[int64]string, error) {
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var k int64
		var n, x, s sql.NullString
		if err := rows.Scan(&k, &n, &x, &s); err != nil {
			return nil, err
		}
		out[k] = n.String + "/" + x.String + "/" + s.String
	}
	return out, rows.Err()
}

// chunkRange is the primary-key range of chunk k, as DIV makes it: DIV
// truncates toward zero, so chunk 0 holds -rowDiffChunk+1 … rowDiffChunk-1.
// The bounds can be outside int64 (BIGINT UNSIGNED).
func chunkRange(id string, k int64) string {
	n := big.NewInt(rowDiffChunk)
	at := func(i int64) *big.Int { return new(big.Int).Mul(big.NewInt(i), n) }
	switch {
	case k > 0:
		return fmt.Sprintf("%s >= %s AND %s < %s", id, at(k), id, at(k+1))
	case k < 0:
		return fmt.Sprintf("%s > %s AND %s <= %s", id, at(k-1), id, at(k))
	}
	return fmt.Sprintf("%s > -%d AND %s < %d", id, rowDiffChunk, id, rowDiffChunk)
}

// tableHash is the fingerprint of one table (see fingerprint), read on c or,
// when c is nil, on a session of its own.
func (m *MySQL) tableHash(ctx context.Context, c *sql.Conn, db, t string) (string, error) {
	if c == nil {
		cc, err := m.session(ctx)
		if err != nil {
			return "", err
		}
		defer cc.Close()
		c = cc
	}
	def, err := showCreate(ctx, c, "TABLE", db, t)
	if err != nil {
		return "", err
	}
	mariadb, err := m.mariadb(ctx, c)
	if err != nil {
		return "", err
	}
	sums, err := checksumGroup(ctx, c, db, []string{t}, map[string]string{t: def}, mariadb)
	if err != nil {
		return "", err
	}
	sum, ok := sums[t]
	if !ok {
		return "", fmt.Errorf("CHECKSUM TABLE returned no row for %s", t)
	}
	return hash([]byte(def + "\x00" + sum)), nil
}
