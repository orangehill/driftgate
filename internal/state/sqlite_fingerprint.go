package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A fingerprint of an SQLite file reads it in a read transaction, which
// sees one moment of the file however the application writes meanwhile.
// It is taken at one of three costs:
//
//  1. Reused: when no other connection committed since the last full
//     fingerprint (PRAGMA data_version of the connection kept open, which
//     SQLite moves for commits of any process, in WAL and rollback mode),
//     the schema version is the same, the file is the same, its size and
//     time (and its -wal file's) are too, and DriftGate did not restore
//     since, the last fingerprint is returned as it was.
//  2. Page digest: a table whose pages (every page of its b-tree, read
//     through dbstat and sqlite_dbpage) hash to what they hashed to when its
//     rows were last read, with the same CREATE statement, has the same
//     rows: its row hash is kept. A changed digest only means a new read,
//     never a missed change. This is what keeps a fingerprint cheap for an
//     application that writes on every request (a Laravel session table in
//     the same file moves data_version every time; excluded, its pages are
//     not read).
//  3. Rows: the rows of every other table are read and hashed, spread over
//     up to sqliteConns more connections that share the same moment.
//
// The items: table:<name> (its CREATE statement, its indexes' CREATE
// statements, its sqlite_sequence value and its rows; a virtual table its
// CREATE statement only, its rows being in its shadow tables), view:<name>,
// trigger:<name>, pragma:user_version and pragma:application_id. SQLite's
// own tables (sqlite_sequence, sqlite_stat*), temporary objects, excluded
// tables and the mark of a pristine copy are no items; the triggers of an
// excluded table are (their body may write any table).

// sqliteSum is the row hash of a table, with the CREATE statement it was
// read under and the digest of the pages it was read from.
type sqliteSum struct {
	sql    string
	digest [32]byte
	pages  int
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

// Fingerprint hashes the working file (see above).
func (s *SQLite) Fingerprint(ctx context.Context, items []string) (Fingerprint, error) {
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
	if s.last != nil && !s.own && dv == s.dv && sv == s.sv {
		release()
		if stamp != s.stamp {
			// The file changed without a commit SQLite saw (a copy
			// written over it): the connections may hold its old pages.
			// Opened again, they read it anew; the row hashes stay, as
			// a page digest is of the bytes now read.
			if err := s.reopen(); err != nil {
				return nil, err
			}
			return s.Fingerprint(ctx, items)
		}
		s.reused = true
		f := maps.Clone(s.last)
		if items != nil {
			f = f.Subset(items)
		}
		return f, nil
	}
	extra := int(min(int64(sqliteConns), tables-1))
	if items != nil {
		extra = min(extra, len(items)-1)
	}
	conns, err := s.snapshot(ctx, extra)
	release()
	if err != nil {
		return nil, err
	}
	defer s.end(conns)
	if s.sums == nil {
		s.sums = map[string]sqliteSum{}
	}
	f, read, err := s.fingerprint(ctx, conns, "main", items, s.sums)
	s.scanned = read
	if err != nil {
		return nil, err
	}
	if items == nil {
		s.last, s.dv, s.sv, s.stamp, s.own = maps.Clone(f), dv, sv, stamp, false
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
// application writer that does not wait for locks (Laravel's default
// busy_timeout is 0) would otherwise fail.
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
// read on conns, which share one snapshot. With sums, a table whose pages
// did not change keeps its row hash from there, and the row hashes read
// are kept in it.
func (s *SQLite) fingerprint(ctx context.Context, conns []*sql.Conn, schema string, items []string, sums map[string]sqliteSum) (Fingerprint, []string, error) {
	objs, err := s.objects(ctx, conns[0], schema)
	if err != nil {
		return nil, nil, err
	}
	want := func(item string) bool { return items == nil || contains(items, item) }
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
		if items == nil {
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

// tableSums reads the row hash of every table, spread over conns: the
// largest tables (by their pages at the last reading) first, each to the
// next free connection. It also returns the tables whose rows it read.
func (s *SQLite) tableSums(ctx context.Context, conns []*sql.Conn, schema string, tables []*sqliteTable, sums map[string]sqliteSum) (map[string]sqliteSum, []string, error) {
	order := append([]*sqliteTable{}, tables...)
	sort.SliceStable(order, func(i, j int) bool { return sums[order[i].name].pages > sums[order[j].name].pages })
	work := make(chan *sqliteTable, len(order))
	for _, t := range order {
		work <- t
	}
	close(work)
	var mu sync.Mutex
	out := map[string]sqliteSum{}
	var read []string
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				if ctx.Err() != nil {
					errs[i] = ctx.Err()
					return
				}
				sum, fresh, err := tableSum(ctx, c, schema, t, sums)
				if err != nil {
					errs[i] = fmt.Errorf("%s: %w", t.name, err)
					return
				}
				mu.Lock()
				out[t.name] = sum
				if fresh {
					read = append(read, t.name)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out, read, errors.Join(errs...)
}

// tableSum reads the row hash of one table, or keeps the one in sums when
// its pages and its CREATE statement are what they were; fresh is set when
// it read the rows.
func tableSum(ctx context.Context, c *sql.Conn, schema string, t *sqliteTable, sums map[string]sqliteSum) (sum sqliteSum, fresh bool, err error) {
	sum.sql = t.sql
	if t.virtual {
		return sum, false, nil
	}
	if sums != nil {
		if sum.digest, sum.pages, err = sqlitePages(ctx, c, schema, t.name); err != nil {
			return sum, false, err
		}
		if old, ok := sums[t.name]; ok && old.sql == t.sql && old.digest == sum.digest && old.rows != nil {
			sum.rows = old.rows
			return sum, false, nil
		}
	}
	sum.rows, err = sqliteRowsHash(ctx, c, schema, t)
	return sum, true, err
}

// sqlitePages hashes every page of a table's b-tree (its root, inner,
// leaf and overflow pages, in the order dbstat walks them) and counts
// them. Two b-trees whose pages hold the same bytes hold the same rows.
func sqlitePages(ctx context.Context, c *sql.Conn, schema, table string) ([32]byte, int, error) {
	rows, err := c.QueryContext(ctx, "SELECT s.pageno, p.data FROM dbstat("+ql(schema)+") AS s JOIN sqlite_dbpage("+ql(schema)+") AS p ON p.pgno = s.pageno WHERE s.name = ?", table)
	if err != nil {
		return [32]byte{}, 0, err
	}
	defer rows.Close()
	h := sha256.New()
	var n [8]byte
	pages := 0
	for rows.Next() {
		var pgno int64
		var data sql.RawBytes
		if err := rows.Scan(&pgno, &data); err != nil {
			return [32]byte{}, 0, err
		}
		binary.BigEndian.PutUint64(n[:], uint64(pgno))
		h.Write(n[:])
		binary.BigEndian.PutUint64(n[:], uint64(len(data)))
		h.Write(n[:])
		h.Write(data)
		pages++
	}
	var d [32]byte
	h.Sum(d[:0])
	return d, pages, rows.Err()
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

// sqliteRowsHash hashes the rows of a table in key order: rowid order for
// a rowid table (its rowid included when no column holds it), primary key
// order for a WITHOUT ROWID table. Every value is hashed with its storage
// class, so 1, 1.0, '1' and x'31' all differ, as do NULL and an empty text. Generated
// columns are left out: the CREATE statement says how they are computed.
func sqliteRowsHash(ctx context.Context, c *sql.Conn, schema string, t *sqliteTable) ([]byte, error) {
	cols, ok, err := readColumns(ctx, c, schema, t.name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no such table")
	}
	var sel []string
	order := ""
	switch {
	case t.noRowid:
		quoted := make([]string, len(cols.pk))
		for i, k := range cols.pk {
			quoted[i] = qs(k)
		}
		order = strings.Join(quoted, ", ")
	case cols.ipk != "":
		order = qs(cols.ipk)
	case cols.rowid != "":
		sel = append(sel, cols.rowid)
		order = cols.rowid
	}
	for _, col := range cols.stored {
		sel = append(sel, "+"+qs(col)) // no declared type: DATETIME text stays text
	}
	q := "SELECT " + strings.Join(sel, ", ") + " FROM " + qs(schema) + "." + qs(t.name)
	if order != "" {
		q += " ORDER BY " + order
	} else {
		q += " NOT INDEXED" // every rowid name is a column: the table's own order
	}
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	h := sha256.New()
	writeFields(h, sel...)
	vals := make([]any, len(sel))
	ptrs := make([]any, len(sel))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	buf := make([]byte, 0, 64<<10)
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for _, v := range vals {
			buf = appendValue(buf, v)
		}
		if len(buf) >= 32<<10 {
			h.Write(buf)
			buf = buf[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	h.Write(buf)
	return h.Sum(nil), nil
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
