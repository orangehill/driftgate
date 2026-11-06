package state

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// sqlServerSplitRows is the size from which a table with an integer
// clustered primary key is read in parts, one per connection.
var sqlServerSplitRows int64 = 50000

// ssScan is what a read of a table's rows found: the hash of its rows
// and, for a table with a rowversion column, the largest value and the
// hash of all of them.
type ssScan struct {
	sum   ssSum
	rv    ssSum
	rvMax []byte
}

func (sc *ssScan) merge(o *ssScan) {
	sc.sum.merge(o.sum)
	sc.rv.merge(o.rv)
	if bytes.Compare(o.rvMax, sc.rvMax) > 0 {
		sc.rvMax = o.rvMax
	}
}

// ssTask is a table or a key range of one, read by one connection.
type ssTask struct {
	t     *ssTable
	row   string // sqlServerRow
	where string // "" for the whole table
}

// ssFingerprint is a fingerprint with what it was made of: the inventory
// and the text of every table's rows (content), by object id.
type ssFingerprint struct {
	f       Fingerprint
	inv     *ssInventory
	content map[int64]string
	read    int // the tables read (not reused)
}

// fingerprint hashes items (nil: all) of the database c is connected to.
// inv and values, when not nil, are its inventory and values read already
// (sqlserver_reuse.go); reuse, when not nil, gives the content of a table
// that need not be read again (the per-table counters). A table's item
// hashes its definition and its rows; every other item its definition. In
// the working database a table with a rowversion column notes when its
// values moved (rowversionMoved). The restore_only tables are left out;
// with restoreOnly they are all it hashes.
func (s *SQLServer) fingerprint(ctx context.Context, c *sql.Conn, db string, items []string, inv *ssInventory, vals *ssValues, reuse func(t *ssTable, def string) (string, bool), restoreOnly bool) (*ssFingerprint, error) {
	var err error
	if vals == nil {
		if vals, err = values(ctx, c); err != nil {
			return nil, err
		}
	}
	if inv == nil {
		if inv, err = s.inventory(ctx, c); err != nil {
			return nil, err
		}
	}
	if vals, err = s.readOnlyValues(ctx, db, vals); err != nil {
		return nil, err
	}
	inv.apply(vals)
	want := func(item string) bool { return items == nil || contains(items, item) }
	out := &ssFingerprint{f: Fingerprint{}, inv: inv, content: map[int64]string{}}
	var tasks []ssTask
	for _, t := range inv.Tables {
		if t.Excluded || t.RestoreOnly != restoreOnly || !want(t.Item) {
			continue
		}
		if reuse != nil {
			if content, ok := reuse(t, inv.Defs[t.Item]); ok {
				out.content[t.ID] = content
				continue
			}
		}
		if t.External {
			out.content[t.ID] = "external" // its rows are another server's (PolyBase)
			continue
		}
		if t.Security != "" {
			return nil, fmt.Errorf("state %s: table %s has row-level security (%s), which hides rows from DriftGate's reads or blocks its writes; turn the policy off in this database or exclude the table", s.o.Name, t.qname(), t.Security)
		}
		row, err := sqlServerRow(t.Columns)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Item, err)
		}
		parts, err := s.split(ctx, c, t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Item, err)
		}
		for _, where := range parts {
			tasks = append(tasks, ssTask{t: t, row: row, where: where})
		}
	}
	scans, err := s.scan(ctx, db, tasks)
	if err != nil {
		return nil, err
	}
	out.read = len(scans)
	for id, sc := range scans {
		t := inv.byID[id]
		content := sc.sum.String()
		if t.rowversion() != "" && strings.EqualFold(db, s.o.Working) {
			moved, err := s.rowversionMoved(ctx, t, sc)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.Item, err)
			}
			if moved {
				content += " rowversion moved"
			}
		}
		out.content[id] = content
	}
	for item, def := range inv.Defs {
		t := inv.ByItem[item]
		if !want(item) || (t != nil && t.RestoreOnly) != restoreOnly {
			continue
		}
		if t != nil {
			out.f[item] = hash([]byte(def + "\x00" + out.content[t.ID]))
		} else {
			out.f[item] = hash([]byte(def))
		}
	}
	return out, nil
}

// readOnlyValues gives a read-only database (the pristine copy) the
// snapshot isolation setting of the working database it was made of, from
// its mark: SQL Server reports it on for any read-only database (the
// values read -1 then). Without a mark it stays -1.
func (s *SQLServer) readOnlyValues(ctx context.Context, db string, v *ssValues) (*ssValues, error) {
	const unknown = " snapshot_isolation -1 "
	i := slices.IndexFunc(v.db, func(l string) bool { return strings.Contains(l, unknown) })
	if i < 0 {
		return v, nil
	}
	m, err := s.readMark(ctx, db)
	if err != nil || m == nil || m.Snapshot == nil {
		return v, err
	}
	w := *v
	w.db = slices.Clone(v.db)
	w.db[i] = strings.Replace(w.db[i], unknown, fmt.Sprintf(" snapshot_isolation %d ", *m.Snapshot), 1)
	return &w, nil
}

// integerTypes are the key types a table is split by.
var integerTypes = map[string]bool{"tinyint": true, "smallint": true, "int": true, "bigint": true}

// split returns the key ranges a table is read in: one part (the whole
// table) unless it is large and its clustered primary key is one integer
// column. The first and last ranges are open, so every row is in exactly
// one of them however the key changes meanwhile.
func (s *SQLServer) split(ctx context.Context, c *sql.Conn, t *ssTable) ([]string, error) {
	n := sqlServerConns
	if t.Rows < sqlServerSplitRows || n < 2 || len(t.Key) != 1 || !t.KeyClustered {
		return []string{""}, nil
	}
	key := ""
	for _, col := range t.Columns {
		if col.Name == t.Key[0] && integerTypes[col.Type] {
			key = qms(col.Name)
		}
	}
	if key == "" {
		return []string{""}, nil
	}
	var lo, hi sql.NullInt64
	if err := c.QueryRowContext(ctx, "SELECT CAST(MIN("+key+") AS bigint), CAST(MAX("+key+") AS bigint) FROM "+t.qname()).Scan(&lo, &hi); err != nil {
		return nil, err
	}
	span := uint64(hi.Int64) - uint64(lo.Int64)
	if !lo.Valid || span < uint64(n) {
		return []string{""}, nil
	}
	bounds := make([]int64, n-1)
	for i := range bounds {
		bounds[i] = lo.Int64 + int64(span/uint64(n)*uint64(i+1))
	}
	parts := []string{fmt.Sprintf("%s < %d", key, bounds[0])}
	for i := 1; i < len(bounds); i++ {
		parts = append(parts, fmt.Sprintf("%s >= %d AND %s < %d", key, bounds[i-1], key, bounds[i]))
	}
	return append(parts, fmt.Sprintf("%s >= %d", key, bounds[len(bounds)-1])), nil
}

// scan reads the tasks on up to sqlServerConns connections to db, the
// largest tables first, and returns what it found per table (object id).
func (s *SQLServer) scan(ctx context.Context, db string, tasks []ssTask) (map[int64]*ssScan, error) {
	out := map[int64]*ssScan{}
	for _, task := range tasks {
		out[task.t.ID] = &ssScan{}
	}
	if len(tasks) == 0 {
		return out, nil
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].t.Rows > tasks[j].t.Rows })
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
		next  int
		wg    sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	for range min(sqlServerConns, len(tasks)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.conn(ctx, db)
			if err != nil {
				fail(err)
				return
			}
			defer c.Close()
			for {
				mu.Lock()
				if next == len(tasks) || first != nil {
					mu.Unlock()
					return
				}
				task := tasks[next]
				next++
				mu.Unlock()
				sc, err := scanTask(ctx, c, task)
				if err != nil {
					fail(fmt.Errorf("%s: %w", task.t.Item, err))
					return
				}
				mu.Lock()
				out[task.t.ID].merge(sc)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

// scanTask reads the rows of one task, as the server encodes them, and
// hashes them.
func scanTask(ctx context.Context, c *sql.Conn, task ssTask) (*ssScan, error) {
	rv := task.t.rowversion()
	q := "SELECT " + task.row
	if rv != "" {
		q += ", " + qms(rv)
	}
	q += " FROM " + task.t.qname()
	if task.where != "" {
		q += " WHERE " + task.where
	}
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sc := &ssScan{}
	var row, v sql.RawBytes
	dest := []any{&row}
	if rv != "" {
		dest = append(dest, &v)
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		sc.sum.add(row)
		if rv != "" {
			sc.rv.add(v)
			if bytes.Compare(v, sc.rvMax) > 0 {
				sc.rvMax = bytes.Clone(v)
			}
		}
	}
	return sc, rows.Err()
}

// rowversionMoved reports whether a table's rowversion values moved while
// its rows stayed the same (an UPDATE that sets a column to its own value
// gives the row a new rowversion, which an application may send as an
// ETag). The values are not part of a table's content: a restore writes
// rows anew, and SQL Server gives them new values. They moved when they
// are neither the pristine copy's (as after a seed) nor have the largest
// value DriftGate's own last restore of the table left (rememberRowversions).
func (s *SQLServer) rowversionMoved(ctx context.Context, t *ssTable, sc *ssScan) (bool, error) {
	if m, ok := s.rvMax[t.Item]; ok && bytes.Equal(m, sc.rvMax) {
		return false, nil
	}
	p, ok, err := s.pristineRowversions(ctx, t)
	if err != nil || !ok {
		return false, err
	}
	return p != sc.rv.String(), nil
}

// pristineRowversions hashes the rowversion values of a table of the
// pristine copy, once (Seed makes a new copy: forgetPristine). ok is false
// when the pristine copy has no such database or table.
func (s *SQLServer) pristineRowversions(ctx context.Context, t *ssTable) (string, bool, error) {
	if p, ok := s.rvPristine[t.Item]; ok {
		return p, true, nil
	}
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		if d, derr := s.database(ctx, s.o.Pristine); derr == nil && d == nil {
			return "", false, nil
		}
		return "", false, err
	}
	defer c.Close()
	rows, err := c.QueryContext(ctx, "SELECT "+qms(t.rowversion())+" FROM "+t.qname())
	if sqlServerNumber(err) == 208 || sqlServerNumber(err) == 207 { // no such table or column
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var sum ssSum
	var v sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return "", false, err
		}
		sum.add(v)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if s.rvPristine == nil {
		s.rvPristine = map[string]string{}
	}
	s.rvPristine[t.Item] = sum.String()
	return sum.String(), true, nil
}

// rememberRowversions notes the largest rowversion value of tables right
// after DriftGate restored them (c is connected to the working database),
// so a fingerprint does not take the new values the restore gave the rows
// for a change (rowversionMoved).
func (s *SQLServer) rememberRowversions(ctx context.Context, c *sql.Conn, tables []*ssTable) error {
	for _, t := range tables {
		rv := t.rowversion()
		if rv == "" {
			continue
		}
		var m []byte
		if err := c.QueryRowContext(ctx, "SELECT MAX("+qms(rv)+") FROM "+t.qname()).Scan(&m); err != nil {
			return fmt.Errorf("%s: %w", t.Item, err)
		}
		if s.rvMax == nil {
			s.rvMax = map[string][]byte{}
		}
		s.rvMax[t.Item] = m
	}
	return nil
}
