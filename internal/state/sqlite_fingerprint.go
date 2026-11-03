package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A fingerprint of an SQLite file reads it in a read transaction, which
// sees one moment of the file however the application writes meanwhile.
// It is taken at one of three costs:
//
//  1. Reused: when no other connection committed since the last full
//     fingerprint (PRAGMA data_version of the connection kept open, which
//     SQLite moves for commits of any process, in WAL and rollback mode),
//     the schema version is the same and the file is the same, the last
//     fingerprint is returned as it was. (A file whose size or time, or
//     its -wal file's, changed while data_version did not was changed
//     without SQLite: the connections are opened again first.) A restore since (DriftGate's own commits, which
//     do not move data_version) wrote only the items it lists: those are
//     read again, the rest is kept.
//  2. Page digest: a table whose pages (every page of its b-tree when its
//     rows were last read, as dbstat walked it) hold the bytes they held
//     then, with the same root page and CREATE statement, has the same rows
//     (see sqlite_pages.go): its row hash is kept. The pages are read
//     through sqlite_dbpage in one pass over the file; only a table whose
//     digest changed is walked again and its rows read. A changed digest
//     only means a new read, never a missed change. This is what keeps a
//     fingerprint cheap for an application that writes on every request (a
//     Laravel session table in the same file moves data_version every
//     time; excluded, its pages are not hashed).
//  3. Rows: the rows of every other table are read and hashed, spread over
//     up to sqliteConns more connections that share the same moment; a
//     large rowid table in parts, by ranges of rowids (sqliteRead).
//
// The items: table:<name> (its CREATE statement, its indexes' CREATE
// statements, its sqlite_sequence value and its rows; a virtual table its
// CREATE statement only, its rows being in its shadow tables), view:<name>,
// trigger:<name>, pragma:user_version and pragma:application_id. SQLite's
// own tables (sqlite_sequence, sqlite_stat*), temporary objects, excluded
// tables and the mark of a pristine copy are no items; the triggers of an
// excluded table are (their body may write any table).

// sqliteSum is the row hash of a table, with the CREATE statement it was
// read under, its root page, the file header's fields the pages were read
// by (sqliteHeader), and the pages of its b-tree it was read from (in
// dbstat's order) with their digest.
type sqliteSum struct {
	sql    string
	root   int64
	header string
	pages  []uint32
	digest [32]byte
	rows   []byte
}

// sqliteSettle and sqliteSettleTries: a fingerprint first waits for a
// write under way; when one committed while it waited, it waits
// sqliteSettle and looks again, up to sqliteSettleTries times, so the
// writes of a request that ends with several transactions are all seen.
var (
	sqliteSettle      = 5 * time.Millisecond
	sqliteSettleTries = 20
)

// sqliteScanAll is the share of the file's pages from which a page digest
// reads every page of the file, not only the pages it needs.
var sqliteScanAll = 0.5

// Fingerprint hashes the working file (see above).
func (s *SQLite) Fingerprint(ctx context.Context, items []string) (_ Fingerprint, err error) {
	defer s.busy(&err)
	s.reused, s.scanned = false, nil
	if err := s.current(); err != nil {
		return nil, err
	}
	lock, dv, err := s.barrier(ctx)
	if err != nil {
		return nil, err
	}
	// While lock holds the write lock no commit can happen: what main
	// reads now and the snapshot the read transactions start on are the
	// same moment.
	release := func() {
		lock.ExecContext(context.Background(), "ROLLBACK")
		lock.Close()
	}
	var sv, tables int64
	if err := s.main.QueryRowContext(ctx, "SELECT (SELECT schema_version FROM pragma_schema_version), (SELECT count(*) FROM sqlite_schema WHERE type = 'table')").Scan(&sv, &tables); err != nil {
		release()
		return nil, err
	}
	stamp := s.stampNow()
	if s.unseen(dv, stamp) {
		release()
		// Opened again, the connections read the file anew; the row
		// hashes stay, as a page digest is of the bytes now read.
		if err := s.reopen(); err != nil {
			return nil, err
		}
		return s.Fingerprint(ctx, items)
	}
	s.seen = sqliteSeen{dv: dv, stamp: stamp, ok: true}
	quiet := s.last != nil && dv == s.dv && sv == s.sv
	if quiet && len(s.restored) == 0 {
		release()
		s.reused = true
		f := maps.Clone(s.last)
		if items != nil {
			f = f.Subset(items)
		}
		return f, nil
	}
	// What is read: every item, the items asked for, or, when only
	// DriftGate's restores wrote since the last fingerprint, the tables
	// they wrote, and the views, triggers and pragmas (only their
	// statements and values are read).
	var want, reread func(item string) bool
	// present lists the tables the file has now: fingerprint asks want
	// about each of them.
	present := map[string]bool{}
	n := tables
	switch {
	case quiet:
		reread = func(item string) bool {
			_, known := s.last[item]
			return s.restored[item] || !strings.HasPrefix(item, "table:") || !known
		}
		want = func(item string) bool {
			if strings.HasPrefix(item, "table:") {
				present[item] = true
			}
			return reread(item)
		}
		n = int64(len(s.restored))
	case items != nil:
		want = func(item string) bool { return contains(items, item) }
		n = int64(len(items))
	}
	conns, err := s.snapshot(ctx, int(min(int64(sqliteConns), n-1)))
	release()
	if err != nil {
		return nil, err
	}
	defer s.end(conns)
	if s.sums == nil {
		s.sums = map[string]sqliteSum{}
	}
	f, read, err := s.fingerprint(ctx, conns, "main", want, s.sums)
	s.scanned = read
	if err != nil {
		return nil, err
	}
	switch {
	case quiet:
		// A table the restores wrote that is gone, or any view,
		// trigger or pragma not read now, is no longer there; nor is a
		// table the file does not have now, whichever way it went (the
		// shadow tables of a virtual table a restore dropped).
		last := maps.Clone(s.last)
		maps.DeleteFunc(last, func(item string, _ string) bool {
			return reread(item) || strings.HasPrefix(item, "table:") && !present[item]
		})
		maps.Copy(last, f)
		s.last, s.restored = last, nil
		f = maps.Clone(last)
		if items != nil {
			f = f.Subset(items)
		}
	case items == nil:
		s.last, s.dv, s.sv, s.restored = maps.Clone(f), dv, sv, nil
	}
	return f, nil
}

// dataVersion reads main's PRAGMA data_version, which moves when another
// connection commits.
func (s *SQLite) dataVersion(ctx context.Context) (int64, error) {
	var dv int64
	err := s.main.QueryRowContext(ctx, "PRAGMA data_version").Scan(&dv)
	return dv, err
}

// barrier waits until no write transaction is under way (BEGIN IMMEDIATE
// waits for the application's to end) and returns the connection that
// then holds the write lock, with main's data_version. The lock is held
// only while the read transactions start, not while they read: an
// application writer would otherwise wait for the whole scan (PHP's PDO
// SQLite waits up to 60 s, which Laravel keeps unless its busy_timeout is
// set), or fail with "database is locked" if it does not wait for locks.
func (s *SQLite) barrier(ctx context.Context) (*sql.Conn, int64, error) {
	lock, err := s.db.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	for try := 0; ; try++ {
		before, err := s.dataVersion(ctx)
		if err == nil {
			_, err = lock.ExecContext(ctx, "BEGIN IMMEDIATE")
		}
		if err != nil {
			lock.Close()
			return nil, 0, err
		}
		dv, err := s.dataVersion(ctx)
		if err != nil {
			lock.ExecContext(context.Background(), "ROLLBACK")
			lock.Close()
			return nil, 0, err
		}
		if dv == before || try == sqliteSettleTries {
			return lock, dv, nil
		}
		lock.ExecContext(context.Background(), "ROLLBACK")
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, 0, ctx.Err()
		case <-time.After(sqliteSettle):
		}
	}
}

// snapshot starts a read transaction on main and on extra more
// connections; they all see the same moment as long as nothing commits
// before the last one has read (the caller holds the write lock).
func (s *SQLite) snapshot(ctx context.Context, extra int) ([]*sql.Conn, error) {
	conns := []*sql.Conn{s.main}
	for range max(extra, 0) {
		c, err := s.db.Conn(ctx)
		if err != nil {
			s.end(conns)
			return nil, err
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		if err := sqliteBegin(ctx, c); err != nil {
			s.end(conns[:i])
			for _, c := range conns[max(i, 1):] {
				c.Close()
			}
			return nil, err
		}
	}
	return conns, nil
}

// sqliteBegin starts a read transaction and its snapshot (a BEGIN alone
// takes none until the first read).
func sqliteBegin(ctx context.Context, c *sql.Conn) error {
	if _, err := c.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	var n int64
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n); err != nil {
		c.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}

// end ends the read transactions of snapshot and gives the extra
// connections back.
func (s *SQLite) end(conns []*sql.Conn) {
	for i, c := range conns {
		c.ExecContext(context.Background(), "ROLLBACK")
		if i > 0 {
			c.Close()
		}
	}
}

// fingerprint hashes the items of a schema (main, or the pristine file's)
// that want accepts (nil: all of them), read on conns, which share one
// snapshot. With sums, a table whose pages did not change keeps its row
// hash from there, and the row hashes read are kept in it.
func (s *SQLite) fingerprint(ctx context.Context, conns []*sql.Conn, schema string, want func(item string) bool, sums map[string]sqliteSum) (Fingerprint, []string, error) {
	objs, err := s.objects(ctx, conns[0], schema)
	if err != nil {
		return nil, nil, err
	}
	all := want == nil
	if all {
		want = func(string) bool { return true }
	}
	var tables []*sqliteTable
	for _, name := range objs.order {
		if want("table:" + name) {
			tables = append(tables, objs.tables[name])
		}
	}
	got, read, err := s.tableSums(ctx, conns, schema, tables, sums)
	if err != nil {
		return nil, nil, err
	}
	if sums != nil {
		if all {
			clear(sums)
		}
		maps.Copy(sums, got)
	}
	f := Fingerprint{}
	for _, t := range tables {
		h := sha256.New()
		writeFields(h, "table", t.sql)
		for _, ix := range t.indexes {
			writeFields(h, ix)
		}
		if seq, ok := objs.seq[t.name]; ok {
			h.Write(appendValue([]byte{'s'}, seq))
		} else {
			h.Write([]byte{'-'})
		}
		h.Write(got[t.name].rows)
		f["table:"+t.name] = hex.EncodeToString(h.Sum(nil))
	}
	for _, v := range objs.vorder {
		if want("view:" + v) {
			f["view:"+v] = hashFields("view", objs.views[v])
		}
	}
	for _, name := range objs.torder {
		if want("trigger:" + name) {
			tr := objs.triggers[name]
			f["trigger:"+name] = hashFields("trigger", tr.table, tr.sql)
		}
	}
	for _, p := range []string{"user_version", "application_id"} {
		if !want("pragma:" + p) {
			continue
		}
		var v int64
		if err := conns[0].QueryRowContext(ctx, "PRAGMA "+qs(schema)+"."+p).Scan(&v); err != nil {
			return nil, nil, err
		}
		f["pragma:"+p] = hashFields("pragma", p, strconv.FormatInt(v, 10))
	}
	sort.Strings(read)
	return f, read, nil
}

// tableSums reads the row hash of every table, spread over conns. With
// sums, the pages of the tables that have a row hash there are read
// first, and the tables whose pages are unchanged keep it. The others are
// read: how and, for a rowid table, the range of its rowids; then the
// walk of their pages (with sums) and their rows, a large rowid table's
// in parts of whole buckets, the largest parts (by the table's pages at
// the last reading) first, each to the next free connection. It also
// returns the tables whose rows it read.
func (s *SQLite) tableSums(ctx context.Context, conns []*sql.Conn, schema string, tables []*sqliteTable, sums map[string]sqliteSum) (map[string]sqliteSum, []string, error) {
	out := map[string]sqliteSum{}
	var header string
	var hashes *sqlitePageHashes
	order := make([]*sqliteTable, 0, len(tables))
	if sums != nil {
		var err error
		if header, err = sqliteHeader(ctx, conns[0], schema); err != nil {
			return nil, nil, err
		}
		var kept []*sqliteTable
		var want []uint32
		for _, t := range tables {
			if old, ok := sums[t.name]; ok && !t.virtual && old.rows != nil && old.sql == t.sql && old.root == t.root && old.header == header {
				kept = append(kept, t)
				want = append(want, old.pages...)
			}
		}
		if len(kept) > 0 {
			slices.Sort(want)
			want = slices.Compact(want)
			var count int64
			if err := conns[0].QueryRowContext(ctx, "PRAGMA "+qs(schema)+".page_count").Scan(&count); err != nil {
				return nil, nil, err
			}
			// Most of the file: read all of it in order, which also
			// gives the pages of the tables walked again.
			if float64(len(want)) >= sqliteScanAll*float64(count) {
				want = nil
			}
			if hashes, err = readPages(ctx, conns, schema, want); err != nil {
				return nil, nil, err
			}
			for _, t := range kept {
				old := sums[t.name]
				if d, ok := hashes.digest(old.pages); ok && d == old.digest {
					out[t.name] = old
				}
			}
		}
	}
	for _, t := range tables {
		if _, ok := out[t.name]; !ok {
			order = append(order, t)
		}
	}
	type part struct {
		r      *sqliteRead
		walk   bool
		lo, hi int64
		cost   int
		pages  []uint32
		digest [32]byte
		rows   []byte
		bucket []sqliteBucket
	}
	reads := make([]*sqliteRead, len(order))
	var parts []*part
	var mu sync.Mutex
	err := sqliteEach(ctx, conns, len(order), func(c *sql.Conn, i int) error {
		t := order[i]
		if t.virtual {
			return nil
		}
		r, err := sqliteReadOf(ctx, c, schema, t)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		reads[i] = r
		pages := len(sums[t.name].pages)
		var add []*part
		if sums != nil {
			add = append(add, &part{r: r, walk: true, cost: pages})
		}
		if r.key < 0 {
			add = append(add, &part{r: r, cost: pages})
		} else {
			lo, hi, ok, err := r.bounds(ctx, c)
			if err != nil {
				return fmt.Errorf("%s: %w", t.name, err)
			}
			if ok {
				// Parts of whole buckets: a bucket is read on one
				// connection.
				first, last := lo>>sqliteBucketBits, hi>>sqliteBucketBits
				n := min(last-first+1, int64(4*len(conns)))
				if pages > 0 && pages < 256 {
					n = 1
				}
				for k := range n {
					from, to := first+(last-first+1)*k/n, first+(last-first+1)*(k+1)/n-1
					add = append(add, &part{r: r, lo: from << sqliteBucketBits, hi: to<<sqliteBucketBits | (1<<sqliteBucketBits - 1), cost: pages / int(n)})
				}
			}
		}
		mu.Lock()
		parts = append(parts, add...)
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].cost > parts[j].cost })
	err = sqliteEach(ctx, conns, len(parts), func(c *sql.Conn, i int) error {
		p := parts[i]
		var err error
		switch {
		case p.walk:
			p.pages, p.digest, err = sqliteWalk(ctx, c, schema, p.r.t.name, hashes)
		case p.r.key < 0:
			p.rows, err = p.r.hash(ctx, c)
		default:
			p.bucket, err = p.r.buckets(ctx, c, p.lo, p.hi)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p.r.t.name, err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	byTable := map[*sqliteRead][]*part{}
	for _, p := range parts {
		byTable[p.r] = append(byTable[p.r], p)
	}
	var read []string
	for i, t := range order {
		sum := sqliteSum{sql: t.sql, root: t.root, header: header}
		if r := reads[i]; r != nil {
			read = append(read, t.name)
			var buckets []sqliteBucket
			for _, p := range byTable[r] {
				switch {
				case p.walk:
					sum.pages, sum.digest = p.pages, p.digest
				case r.key < 0:
					sum.rows = p.rows
				default:
					buckets = append(buckets, p.bucket...)
				}
			}
			if r.key >= 0 {
				sum.rows = r.combine(buckets)
			}
		}
		out[t.name] = sum
	}
	return out, read, nil
}

// sqliteEach runs do(c, i) for i from 0 to n-1, spread over conns: each
// connection takes the next i when it is free. It stops at the first
// error.
func sqliteEach(ctx context.Context, conns []*sql.Conn, n int, do func(c *sql.Conn, i int) error) error {
	var next atomic.Int64
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for k, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				if err := ctx.Err(); err != nil {
					errs[k] = err
					next.Store(int64(n))
					return
				}
				if err := do(c, i); err != nil {
					errs[k] = err
					next.Store(int64(n))
					return
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// sqliteColumns describes how a table's rows are read and copied.
type sqliteColumns struct {
	stored []string // the columns that hold data, in order (not generated ones)
	pk     []string // the primary key's columns, in key order
	ipk    string   // the INTEGER PRIMARY KEY column that is the rowid
	rowid  string   // for another rowid table, a name its rowid can be read by
	// generated is set when the table has generated columns.
	generated bool
}

func readColumns(ctx context.Context, q queryer, schema, table string) (*sqliteColumns, bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, pk, hidden FROM pragma_table_xinfo(?, ?) ORDER BY cid", table, schema)
	if err != nil {
		return nil, false, err
	}
	cols := &sqliteColumns{}
	type pkCol struct {
		name string
		pos  int
	}
	var pks []pkCol
	names := map[string]bool{}
	for rows.Next() {
		var name string
		var pk, hidden int
		if err := rows.Scan(&name, &pk, &hidden); err != nil {
			rows.Close()
			return nil, false, err
		}
		names[strings.ToLower(name)] = true
		if pk > 0 {
			pks = append(pks, pkCol{name, pk})
		}
		switch hidden {
		case 0:
			cols.stored = append(cols.stored, name)
		case 2, 3: // generated: VIRTUAL, STORED
			cols.generated = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
	for _, p := range pks {
		cols.pk = append(cols.pk, p.name)
	}
	var pkIndex int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list(?, ?) WHERE origin = 'pk'", table, schema).Scan(&pkIndex); err != nil {
		return nil, false, err
	}
	// A rowid table's primary key is its rowid when it is one column and
	// SQLite made no index for it (INTEGER PRIMARY KEY, but not … DESC).
	if len(cols.pk) == 1 && pkIndex == 0 {
		cols.ipk = cols.pk[0]
	}
	for _, alias := range []string{"rowid", "_rowid_", "oid"} {
		if !names[alias] {
			cols.rowid = alias
			break
		}
	}
	return cols, len(names) > 0, nil
}

// sqliteBucketBits: the rows of a rowid table are hashed in buckets of
// rowids (rowid >> sqliteBucketBits), so that connections can read the
// parts of a large table at the same time.
const sqliteBucketBits = 12

// sqliteRead is how the rows of a table are read: in key order, rowid
// order for a rowid table (its rowid included when no column holds it),
// primary key order for a WITHOUT ROWID table. Every value is hashed with
// its storage class, so 1, 1.0, '1' and x'31' all differ, as do NULL and
// an empty text. Generated columns are left out: the CREATE statement says
// how they are computed.
//
// The row hash of a rowid table is the hash of the columns read and of its
// buckets in order, each bucket's number and the hash of its rows. A table
// whose rowid no name reaches (every name is a column's) and a WITHOUT
// ROWID table are hashed whole: the columns, then the rows.
type sqliteRead struct {
	t     *sqliteTable
	sel   []string // what is read of a row
	from  string   // the table, with its schema
	order string   // the key the rows are read in order of (none: the table's own order)
	// For a rowid table read in buckets: the rowid's name and where it is
	// in sel; key is -1 for a table hashed whole.
	rowid string
	key   int
}

func sqliteReadOf(ctx context.Context, c *sql.Conn, schema string, t *sqliteTable) (*sqliteRead, error) {
	cols, ok, err := readColumns(ctx, c, schema, t.name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no such table")
	}
	r := &sqliteRead{t: t, key: -1}
	switch {
	case t.noRowid:
		quoted := make([]string, len(cols.pk))
		for i, k := range cols.pk {
			quoted[i] = qs(k)
		}
		r.order = strings.Join(quoted, ", ")
	case cols.ipk != "":
		r.rowid = qs(cols.ipk)
	case cols.rowid != "":
		r.sel = append(r.sel, cols.rowid)
		r.rowid, r.key = cols.rowid, 0
	}
	for _, col := range cols.stored {
		if col == cols.ipk && !t.noRowid {
			r.key = len(r.sel)
		}
		r.sel = append(r.sel, "+"+qs(col)) // no declared type: DATETIME text stays text
	}
	if r.rowid != "" {
		r.order = r.rowid
	}
	r.from = qs(schema) + "." + qs(t.name)
	return r, nil
}

// hash reads the rows of a table hashed whole.
func (r *sqliteRead) hash(ctx context.Context, c *sql.Conn) ([]byte, error) {
	q := "SELECT " + strings.Join(r.sel, ", ") + " FROM " + r.from
	if r.order != "" {
		q += " ORDER BY " + r.order
	} else {
		q += " NOT INDEXED" // every rowid name is a column: the table's own order
	}
	h := sha256.New()
	writeFields(h, r.sel...)
	buf := make([]byte, 0, 64<<10)
	err := sqliteScan(ctx, c, q, len(r.sel), func(vals []any) {
		for _, v := range vals {
			buf = appendValue(buf, v)
		}
		if len(buf) >= 32<<10 {
			h.Write(buf)
			buf = buf[:0]
		}
	})
	if err != nil {
		return nil, err
	}
	h.Write(buf)
	return h.Sum(nil), nil
}

// sqliteBucket is the hash of the rows of a bucket.
type sqliteBucket struct {
	n   int64
	sum [32]byte
}

// bounds reads the lowest and highest rowid of a rowid table; ok is false
// for an empty table.
func (r *sqliteRead) bounds(ctx context.Context, c *sql.Conn) (lo, hi int64, ok bool, err error) {
	var a, b sql.NullInt64
	err = c.QueryRowContext(ctx, "SELECT min("+r.rowid+"), max("+r.rowid+") FROM "+r.from).Scan(&a, &b)
	return a.Int64, b.Int64, a.Valid && b.Valid, err
}

// buckets hashes the rows whose rowid is from lo to hi, which begin and
// end whole buckets.
func (r *sqliteRead) buckets(ctx context.Context, c *sql.Conn, lo, hi int64) ([]sqliteBucket, error) {
	var out []sqliteBucket
	h := sha256.New()
	buf := make([]byte, 0, 64<<10)
	cur := int64(0)
	end := func() {
		h.Write(buf)
		buf = buf[:0]
		b := sqliteBucket{n: cur}
		h.Sum(b.sum[:0])
		out = append(out, b)
		h.Reset()
	}
	first := true
	var bad error
	q := "SELECT " + strings.Join(r.sel, ", ") + " FROM " + r.from + " WHERE " + r.rowid + " BETWEEN ? AND ? ORDER BY " + r.rowid
	err := sqliteScan(ctx, c, q, len(r.sel), func(vals []any) {
		id, ok := vals[r.key].(int64)
		if !ok {
			bad = fmt.Errorf("rowid %v is no integer", vals[r.key])
			return
		}
		if n := id >> sqliteBucketBits; first || n != cur {
			if !first {
				end()
			}
			first, cur = false, n
		}
		for _, v := range vals {
			buf = appendValue(buf, v)
		}
		if len(buf) >= 32<<10 {
			h.Write(buf)
			buf = buf[:0]
		}
	}, lo, hi)
	if err == nil {
		err = bad
	}
	if err != nil {
		return nil, err
	}
	if !first {
		end()
	}
	return out, nil
}

// combine is the row hash of a rowid table from its buckets.
func (r *sqliteRead) combine(buckets []sqliteBucket) []byte {
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].n < buckets[j].n })
	h := sha256.New()
	writeFields(h, r.sel...)
	var b [8]byte
	for _, k := range buckets {
		binary.BigEndian.PutUint64(b[:], uint64(k.n))
		h.Write(b[:])
		h.Write(k.sum[:])
	}
	return h.Sum(nil)
}

// sqliteScan runs a query and calls row with the values of each row (the
// slice is reused). The rows are read from the driver itself: through
// database/sql every blob would be copied once more.
func sqliteScan(ctx context.Context, c *sql.Conn, query string, n int, row func(vals []any), args ...any) error {
	return c.Raw(func(dc any) error {
		q, ok := dc.(driver.QueryerContext)
		if !ok {
			return fmt.Errorf("sqlite: no QueryContext on %T", dc)
		}
		named := make([]driver.NamedValue, len(args))
		for i, a := range args {
			named[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
		}
		rows, err := q.QueryContext(ctx, query, named)
		if err != nil {
			return err
		}
		defer rows.Close()
		dest := make([]driver.Value, n)
		vals := make([]any, n)
		for {
			if err := rows.Next(dest); err == io.EOF {
				return nil
			} else if err != nil {
				return err
			}
			for i, v := range dest {
				vals[i] = v
			}
			row(vals)
		}
	})
}

// appendValue appends one value with its storage class: a tag, then an
// integer or a real's bits in 8 bytes, or a text's or blob's length and
// bytes. No two different values (or sequences of them) append the same
// bytes.
func appendValue(b []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, 'n')
	case int64:
		return binary.BigEndian.AppendUint64(append(b, 'i'), uint64(x))
	case float64:
		return binary.BigEndian.AppendUint64(append(b, 'r'), math.Float64bits(x))
	case string:
		return append(binary.AppendUvarint(append(b, 't'), uint64(len(x))), x...)
	case []byte:
		return append(binary.AppendUvarint(append(b, 'b'), uint64(len(x))), x...)
	case bool: // not a storage class SQLite returns; kept apart all the same
		if x {
			return append(b, 'T')
		}
		return append(b, 'F')
	case time.Time: // not returned for the +column reads; kept apart all the same
		return append(binary.AppendUvarint(append(b, 'd'), uint64(len(x.Format(time.RFC3339Nano)))), x.Format(time.RFC3339Nano)...)
	default:
		s := fmt.Sprintf("%T:%v", x, x)
		return append(binary.AppendUvarint(append(b, '?'), uint64(len(s))), s...)
	}
}

// writeField writes one length-prefixed field.
func writeField(w io.Writer, p []byte) {
	w.Write(binary.AppendUvarint(nil, uint64(len(p))))
	w.Write(p)
}

func writeFields(h io.Writer, fields ...string) {
	for _, f := range fields {
		writeField(h, []byte(f))
	}
}

func hashFields(fields ...string) string {
	h := sha256.New()
	writeFields(h, fields...)
	return hex.EncodeToString(h.Sum(nil))
}

// SeededFingerprint is the fingerprint of the pristine file, what a seed
// records: the copy is one moment of the working file, which the
// application may have changed since.
func (s *SQLite) SeededFingerprint(ctx context.Context) (Fingerprint, error) {
	return s.pristineFingerprint(ctx)
}

// pristineFingerprint hashes the pristine file, read-only; it is kept as
// long as the file is the same and unchanged.
func (s *SQLite) pristineFingerprint(ctx context.Context) (Fingerprint, error) {
	fi, err := os.Stat(s.o.Pristine)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("state %s: pristine file %s does not exist; run driftgate seed", s.o.Name, s.o.Pristine)
	}
	if err != nil {
		return nil, err
	}
	if s.pristine != nil && sameVersion(fi, s.pristineID) {
		return s.pristine, nil
	}
	db, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := sqliteBegin(ctx, c); err != nil {
		return nil, fmt.Errorf("pristine file %s: %w", s.o.Pristine, err)
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	f, _, err := s.fingerprint(ctx, []*sql.Conn{c}, "main", nil, nil)
	if err != nil {
		return nil, err
	}
	s.pristine, s.pristineID = f, fi
	return f, nil
}

// sameVersion reports whether two reads of a file's identity are of the
// same file, not written in between.
func sameVersion(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
