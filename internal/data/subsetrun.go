package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"
)

// Subset is the Filter of data subset: before the copy it chooses a sample
// of the source with closed references, in the same read-only snapshot, and
// during the copy it keeps the rows of the sample and nulls references the
// rules allow to leave the sample. Set it as Options.Filter with Rules that
// have a subset: section; the zero value is ready.
//
// The sample is the least set of rows that holds
//   - the roots: per table the rows chosen by rows, percent and where
//     (all: every row);
//   - the followed rows: rows of a table with follow that reference a root
//     or followed row of a parent it follows (per_parent at most per row);
//   - the parents: for every kept row the rows its references point at,
//     transitively, except along nullable references with the rule null.
//
// Only roots and followed rows are followed down: a row taken in as a parent
// does not bring its children, or one user would bring in every table. The
// sample depends only on the source and the rules, never on the order of the
// work, so the same input gives the same sample.
//
// Memory: the identity (primary key) of every kept row, and the values of
// kept rows in referenced columns, are held in memory (roughly 100 bytes per
// kept row and referenced key; tables are never held). The source is only
// read, so nothing can be kept there.
type Subset struct {
	dl     dialect
	tables map[string]*sampleTable
	order  []*sampleTable
}

// SampleReport counts the sample of one table; never a value.
type SampleReport struct {
	Total    int64 `json:"total"`    // rows in the source
	Roots    int64 `json:"roots"`    // rows the sample started from
	Followed int64 `json:"followed"` // rows that reference followed rows of a parent
	Parents  int64 `json:"parents"`  // rows taken in only because kept rows reference them
	// Nulled are references set to NULL: their row is outside the sample
	// (nullable null) or in a table without rows.
	Nulled int64 `json:"nulled,omitempty"`
	// Dangling are references to rows that are not in the source either
	// (declared references without a foreign key); copied as they are.
	Dangling int64 `json:"dangling,omitempty"`
}

const (
	flagRoot   = 1
	flagFollow = 2
)

// sampleTable is the state of one table during the closure and the copy.
type sampleTable struct {
	t     *Table
	rule  *SampleRule // nil: only the rows kept rows reference
	empty bool        // none, or emptied or skipped by its rule
	skip  bool

	cols []*Column       // read by the closure
	id   []int           // the identity of a row: the primary key, or every column
	kept map[string]byte // key of the identity (see key) → flagRoot|flagFollow (0: a parent only)
	keys map[string]*keySet
	out  []*sampleRef // references from this table
	down []*sampleRef // references of tables that follow this one

	copyID    []int // the identity in the copied columns
	copyReady bool
	total     int64
	nulled    int64
	dangling  int64
}

// keySet holds the values of kept rows in columns other rows reference.
type keySet struct {
	cols    []string
	at      []int
	kept    map[string]struct{}
	asked   map[string]struct{} // values asked for, found or not
	pending [][][]byte
}

// scanQuery reads rows of one table.
type scanQuery struct {
	t     *Table
	cols  []*Column
	where string    // the user's SQL condition
	in    []*Column // with vals: rows whose in columns equal one of the tuples
	vals  [][][]byte
	eq    []whenCond
	order int // 0 any, 1 ascending identity, -1 descending
	limit int
}

// rowSource is what the closure reads: the source in its snapshot, or rows
// in memory in tests.
type rowSource interface {
	scan(ctx context.Context, q scanQuery, fn func(row [][]byte) error) error
	count(ctx context.Context, t *Table) (int64, error)
}

// errStop ends a scan early without an error.
var errStop = errors.New("stop")

// batchKeys is the most values asked for in one query.
const batchKeys = 500

// maxInList is the most kept rows the copy asks for by primary key; a table
// with more is read whole and filtered.
const maxInList = 10000

// prepare computes the sample; Anonymize calls it after the schema and the
// rules are checked, before anything is written.
func (s *Subset) prepare(ctx context.Context, src *sql.Conn, dl dialect, schema *Schema, rules *Rules, log io.Writer) error {
	s.dl = dl
	return s.run(ctx, sqlSource{c: src, dl: dl}, schema, rules, log)
}

func (s *Subset) run(ctx context.Context, src rowSource, schema *Schema, rules *Rules, log io.Writer) error {
	sr := rules.Subset
	if sr == nil {
		return errNoSubset
	}
	e := &CheckError{}
	s.tables = map[string]*sampleTable{}
	s.order = nil
	for _, t := range schema.Order() {
		tr := rules.Tables[t.Name]
		st := &sampleTable{t: t, rule: sr.Tables[t.Name], kept: map[string]byte{}, keys: map[string]*keySet{}}
		st.skip = tr.action() == ActionSkip
		st.empty = (tr != nil && tr.emptied()) || (st.rule != nil && st.rule.Mode == SampleNone)
		if tr != nil && tr.emptied() && st.rule != nil && st.rule.Mode != SampleNone {
			e.Invalid = append(e.Invalid, fmt.Sprintf("subset %s: the table is emptied or skipped by its rule; leave it out of subset: or use none", t.Name))
		}
		s.tables[t.Name] = st
		s.order = append(s.order, st)
	}
	for _, tn := range sortedKeys(sr.Tables) {
		if s.tables[tn] == nil {
			e.Stale = append(e.Stale, "subset table "+tn)
		}
	}
	refs := sr.references(schema, s.tables, e)
	for _, r := range refs {
		r.child.out = append(r.child.out, r)
	}
	for _, st := range s.order {
		if st.rule == nil {
			continue
		}
		for _, pn := range st.rule.Follow {
			p := s.tables[pn]
			if p == nil {
				e.Stale = append(e.Stale, fmt.Sprintf("subset %s: follow %s", st.t.Name, pn))
				continue
			}
			found := false
			for _, r := range st.out {
				if r.parent == p {
					r.follow, r.perParent, r.desc = true, st.rule.PerParent, st.rule.order() == OrderLatest
					r.done = map[string]struct{}{}
					p.down = append(p.down, r)
					found = true
				}
			}
			if !found {
				e.Invalid = append(e.Invalid, fmt.Sprintf("subset %s: follows %s but has no reference to it (declare one under references)", st.t.Name, pn))
			}
		}
	}
	if len(e.Unclassified)+len(e.Stale)+len(e.Proposed)+len(e.Invalid) > 0 {
		return e
	}
	if err := s.columns(refs); err != nil {
		return err
	}

	for _, st := range s.order {
		if st.skip {
			continue
		}
		n, err := src.count(ctx, st.t)
		if err != nil {
			return fmt.Errorf("source %s: %w", st.t.Name, err)
		}
		st.total = n
	}
	for _, st := range s.order {
		if st.rule != nil && !st.empty && st.rule.roots() {
			if err := s.roots(ctx, src, st); err != nil {
				return err
			}
		}
	}
	for {
		more := false
		for _, st := range s.order {
			for _, k := range sortedKeys(st.keys) {
				if ks := st.keys[k]; len(ks.pending) > 0 {
					more = true
					if err := s.pull(ctx, src, st, ks); err != nil {
						return err
					}
				}
			}
		}
		for _, st := range s.order {
			for _, r := range st.out {
				if r.follow && len(r.pending) > 0 {
					more = true
					if err := s.followDown(ctx, src, r); err != nil {
						return err
					}
				}
			}
		}
		if !more {
			break
		}
	}
	var kept, tables int
	for _, st := range s.order {
		if len(st.kept) > 0 {
			kept += len(st.kept)
			tables++
		}
	}
	fmt.Fprintf(log, "subset: %d rows in %d tables, references closed\n", kept, tables)
	return nil
}

// columns decides what the closure reads of each table: the identity, the
// columns of references in both directions and the when columns.
func (s *Subset) columns(refs []*sampleRef) error {
	need := map[*sampleTable]map[string]bool{}
	mark := func(st *sampleTable, cols ...string) {
		if need[st] == nil {
			need[st] = map[string]bool{}
		}
		for _, c := range cols {
			need[st][c] = true
		}
	}
	for _, st := range s.order {
		if len(st.t.PrimaryKey) > 0 {
			for _, k := range st.t.PrimaryKey {
				if c := st.t.Column(k); c == nil || c.Generated {
					return fmt.Errorf("%w: data subset of %s, whose primary key has a generated column", ErrUnsupported, st.t.Name)
				}
			}
			mark(st, st.t.PrimaryKey...)
		} else {
			for _, c := range st.t.Columns {
				if !c.Generated {
					mark(st, c.Name)
				}
			}
		}
	}
	for _, r := range refs {
		mark(r.child, r.cols...)
		mark(r.parent, r.refCols...)
		for _, w := range r.when {
			mark(r.child, w.col)
		}
	}
	for _, st := range s.order {
		at := map[string]int{}
		for _, c := range st.t.Columns {
			if need[st][c.Name] {
				at[c.Name] = len(st.cols)
				st.cols = append(st.cols, c)
			}
		}
		st.id = nil
		if len(st.t.PrimaryKey) > 0 {
			for _, k := range st.t.PrimaryKey {
				st.id = append(st.id, at[k])
			}
		} else {
			for _, c := range st.t.Columns {
				if !c.Generated {
					st.id = append(st.id, at[c.Name])
				}
			}
		}
		for _, r := range refs {
			if r.child == st {
				r.at = positions(at, r.cols)
				for i := range r.when {
					r.when[i].at = at[r.when[i].col]
				}
			}
			if r.parent == st {
				k := strings.Join(r.refCols, "\x00")
				ks := st.keys[k]
				if ks == nil {
					ks = &keySet{cols: r.refCols, at: positions(at, r.refCols), kept: map[string]struct{}{}, asked: map[string]struct{}{}}
					st.keys[k] = ks
				}
				r.keys = ks
			}
		}
	}
	return nil
}

func positions(at map[string]int, cols []string) []int {
	out := make([]int, len(cols))
	for i, c := range cols {
		out[i] = at[c]
	}
	return out
}

// roots reads the rows a table's sample starts from.
func (s *Subset) roots(ctx context.Context, src rowSource, st *sampleTable) error {
	r := st.rule
	q := scanQuery{t: st.t, cols: st.cols, where: r.Where}
	if r.Mode != SampleAll && r.Rows > 0 {
		q.order = 1
		if r.order() == OrderLatest {
			q.order = -1
		}
		if r.Percent == 0 {
			q.limit = r.Rows
		}
	}
	n := 0
	err := src.scan(ctx, q, func(row [][]byte) error {
		if r.Percent > 0 && !inPercent(tupleKey(pick(row, st.id)), r.Percent) {
			return nil
		}
		if err := s.admit(st, row, flagRoot); err != nil {
			return err
		}
		if n++; r.Mode != SampleAll && r.Rows > 0 && n >= r.Rows {
			return errStop
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("source %s: %w", st.t.Name, err)
	}
	return nil
}

// inPercent chooses a row by a hash of its identity: stable when other rows
// come and go, and spread over the whole table.
func inPercent(id string, percent float64) bool {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()%1_000_000 < uint64(percent*10_000)
}

// pull reads the rows kept rows reference and are not kept yet.
func (s *Subset) pull(ctx context.Context, src rowSource, st *sampleTable, ks *keySet) error {
	pending := ks.pending
	ks.pending = nil
	in := make([]*Column, len(ks.cols))
	for i, cn := range ks.cols {
		in[i] = st.t.Column(cn)
	}
	for start := 0; start < len(pending); start += batchKeys {
		q := scanQuery{t: st.t, cols: st.cols, in: in, vals: pending[start:min(start+batchKeys, len(pending))]}
		err := src.scan(ctx, q, func(row [][]byte) error { return s.admit(st, row, 0) })
		if err != nil {
			return fmt.Errorf("source %s: %w", st.t.Name, err)
		}
	}
	return nil
}

// followDown reads the rows of a following table that reference rows of its
// parent kept as roots or followed.
func (s *Subset) followDown(ctx context.Context, src rowSource, r *sampleRef) error {
	pending := r.pending
	r.pending = nil
	child := r.child
	in := make([]*Column, len(r.cols))
	for i, cn := range r.cols {
		in[i] = child.t.Column(cn)
	}
	for start := 0; start < len(pending); start += batchKeys {
		q := scanQuery{t: child.t, cols: child.cols, in: in, vals: pending[start:min(start+batchKeys, len(pending))], eq: r.when, order: 1}
		if r.desc {
			q.order = -1
		}
		per := map[string]int{}
		err := src.scan(ctx, q, func(row [][]byte) error {
			k := tupleKey(pick(row, r.at))
			if r.perParent > 0 && per[k] >= r.perParent {
				return nil
			}
			per[k]++
			return s.admit(child, row, flagFollow)
		})
		if err != nil {
			return fmt.Errorf("source %s: %w", child.t.Name, err)
		}
	}
	return nil
}

// admit keeps one row: it asks for the rows its references point at and,
// for a root or followed row, for the rows that follow it.
func (s *Subset) admit(st *sampleTable, row [][]byte, flag byte) error {
	id := st.key(pick(row, st.id))
	old, seen := st.kept[id]
	if seen && old|flag == old {
		return nil
	}
	st.kept[id] = old | flag
	if !seen {
		for _, ks := range st.keys {
			if v := pick(row, ks.at); !hasNull(v) {
				ks.kept[tupleKey(v)] = struct{}{}
			}
		}
		for _, r := range st.out {
			v := pick(row, r.at)
			if hasNull(v) || !r.matches(row, false) {
				continue
			}
			if r.parent.empty {
				if !r.nullable {
					return &CheckError{Invalid: []string{fmt.Sprintf("subset reference %s: kept rows of %s reference %s, which has no rows (none, or emptied by its rule); give %s none as well", r, st.t.Name, r.parent.t.Name, st.t.Name)}}
				}
				continue
			}
			if r.null {
				continue
			}
			k := tupleKey(v)
			if _, ok := r.keys.kept[k]; ok {
				continue
			}
			if _, ok := r.keys.asked[k]; ok {
				continue
			}
			r.keys.asked[k] = struct{}{}
			r.keys.pending = append(r.keys.pending, v)
		}
	}
	if old == 0 && flag != 0 {
		for _, r := range st.down {
			v := pick(row, r.keys.at)
			if hasNull(v) {
				continue
			}
			k := tupleKey(v)
			if _, ok := r.done[k]; ok {
				continue
			}
			r.done[k] = struct{}{}
			r.pending = append(r.pending, v)
		}
	}
	return nil
}

// matches checks the when conditions of a declared reference on a row read
// by the closure, or (copy) on a row of the copy.
func (r *sampleRef) matches(row [][]byte, copy bool) bool {
	for _, w := range r.when {
		i := w.at
		if copy {
			i = w.cat
		}
		if i < 0 || row[i] == nil || string(row[i]) != w.value {
			return false
		}
	}
	return true
}

func pick(row [][]byte, at []int) [][]byte {
	out := make([][]byte, len(at))
	for i, j := range at {
		out[i] = row[j]
	}
	return out
}

func hasNull(v [][]byte) bool {
	for _, b := range v {
		if b == nil {
			return true
		}
	}
	return false
}

// tupleKey encodes values as one string: each value with its length, NULL
// apart from the empty string.
func tupleKey(v [][]byte) string { return string(appendTuple(nil, v)) }

func appendTuple(b []byte, v [][]byte) []byte {
	for _, x := range v {
		if x == nil {
			b = append(b, 0)
			continue
		}
		b = binary.AppendUvarint(b, uint64(len(x))+1)
		b = append(b, x...)
	}
	return b
}

// key is the key of a row's identity in kept: the identity itself (a
// tupleKey) for a table with a primary key, which Where reads back; for a
// table without one, whose identity is every column, BLOBs and long texts
// too, the first 16 bytes of its SHA-256, so the memory per kept row stays
// the same however long the rows are.
func (st *sampleTable) key(v [][]byte) string {
	b := appendTuple(nil, v)
	if len(st.t.PrimaryKey) > 0 {
		return string(b)
	}
	h := sha256.Sum256(b)
	return string(h[:16])
}

// splitKey decodes tupleKey.
func splitKey(k string) [][]byte {
	var out [][]byte
	b := []byte(k)
	for len(b) > 0 {
		n, w := binary.Uvarint(b)
		b = b[w:]
		if n == 0 {
			out = append(out, nil)
			continue
		}
		out = append(out, b[:n-1])
		b = b[n-1:]
	}
	return out
}

// Where asks the source only for the kept rows when there are few of them.
func (s *Subset) Where(t *Table) string {
	st := s.tables[t.Name]
	if st == nil || len(st.kept) == 0 {
		return "1 = 0"
	}
	if int64(len(st.kept)) == st.total || len(t.PrimaryKey) == 0 || len(st.kept) > maxInList {
		return ""
	}
	keys := make([]string, 0, len(st.kept))
	for k := range st.kept {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([][][]byte, len(keys))
	for i, k := range keys {
		vals[i] = splitKey(k)
	}
	pk := make([]*Column, len(t.PrimaryKey))
	for i, k := range t.PrimaryKey {
		pk[i] = t.Column(k)
	}
	return inList(s.dl, pk, vals)
}

// Keep keeps the rows of the sample; on a kept row it nulls the references
// whose row stays outside the sample (nullable null, or a table without rows).
func (s *Subset) Keep(t *Table, cols []*Column, row [][]byte) bool {
	st := s.tables[t.Name]
	if st == nil {
		return false
	}
	if !st.copyReady {
		st.prepareCopy(cols)
	}
	if _, ok := st.kept[st.key(pick(row, st.copyID))]; !ok {
		return false
	}
	for _, r := range st.out {
		if r.copyAt == nil {
			continue
		}
		v := pick(row, r.copyAt)
		if hasNull(v) || !r.matches(row, true) {
			continue
		}
		if _, ok := r.keys.kept[tupleKey(v)]; ok {
			continue
		}
		if r.nullable && (r.null || r.parent.empty) {
			for _, i := range r.copyAt {
				row[i] = nil
			}
			st.nulled++
			continue
		}
		st.dangling++
	}
	return true
}

// prepareCopy finds the identity and the references in the copied columns.
func (st *sampleTable) prepareCopy(cols []*Column) {
	at := map[string]int{}
	for i, c := range cols {
		at[c.Name] = i
	}
	find := func(names []string) []int {
		out := make([]int, len(names))
		for i, n := range names {
			j, ok := at[n]
			if !ok {
				return nil
			}
			out[i] = j
		}
		return out
	}
	if len(st.t.PrimaryKey) > 0 {
		st.copyID = find(st.t.PrimaryKey)
	} else {
		var names []string
		for _, c := range st.t.Columns {
			if !c.Generated {
				names = append(names, c.Name)
			}
		}
		st.copyID = find(names)
	}
	for _, r := range st.out {
		r.copyAt = find(r.cols)
		for i := range r.when {
			r.when[i].cat = -1
			if j, ok := at[r.when[i].col]; ok {
				r.when[i].cat = j
			}
		}
	}
	st.copyReady = true
}

// report counts the sample of a table; nil for a skipped one.
func (s *Subset) report(t *Table) *SampleReport {
	st := s.tables[t.Name]
	if st == nil || st.skip {
		return nil
	}
	r := &SampleReport{Total: st.total, Nulled: st.nulled, Dangling: st.dangling}
	for _, f := range st.kept {
		switch {
		case f&flagRoot != 0:
			r.Roots++
		case f&flagFollow != 0:
			r.Followed++
		default:
			r.Parents++
		}
	}
	return r
}

// sqlSource reads the source database in its snapshot.
type sqlSource struct {
	c  *sql.Conn
	dl dialect
}

func (q sqlSource) count(ctx context.Context, t *Table) (int64, error) {
	var n int64
	err := q.c.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.quoted).Scan(&n)
	return n, err
}

func (q sqlSource) scan(ctx context.Context, sq scanQuery, fn func(row [][]byte) error) error {
	rows, err := q.c.QueryContext(ctx, scanSQL(q.dl, sq))
	if err != nil {
		return err
	}
	defer rows.Close()
	raw := make([]sql.RawBytes, len(sq.cols))
	ptrs := make([]any, len(raw))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		row := make([][]byte, len(raw))
		for i, r := range raw {
			if r != nil {
				row[i] = append([]byte{}, r...)
			}
		}
		if err := fn(row); err == errStop {
			return nil
		} else if err != nil {
			return err
		}
	}
	return rows.Err()
}

// scanSQL reads columns as text, the way the copy reads them (so the
// identities match), ordered like the copy when an order is asked for.
func scanSQL(dl dialect, sq scanQuery) string {
	_, pg := dl.(postgresDialect)
	names := make([]string, len(sq.cols))
	for i, c := range sq.cols {
		names[i] = dl.quote(c.Name)
		if pg {
			names[i] += "::text"
		}
	}
	q := "SELECT " + strings.Join(names, ", ") + " FROM " + sq.t.quoted
	var conds []string
	if sq.where != "" {
		conds = append(conds, "("+sq.where+")")
	}
	if len(sq.in) > 0 {
		conds = append(conds, inList(dl, sq.in, sq.vals))
	}
	for _, w := range sq.eq {
		conds = append(conds, dl.quote(w.col)+" = "+dl.literal(sq.t.Column(w.col), []byte(w.value)))
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if sq.order != 0 {
		dir := ""
		if sq.order < 0 {
			dir = " DESC"
		}
		q += " ORDER BY " + strings.Join(orderBy(sq.t, func(name string, pk bool) string {
			e := dl.quote(name)
			switch {
			case pg && pk:
				e = sq.t.quoted + "." + e
			case pg:
				e = sq.t.quoted + "." + e + `::text COLLATE "C"`
			case !pk:
				e = "CAST(" + e + " AS BINARY)"
			}
			return e + dir
		}), ", ")
	}
	if sq.limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", sq.limit)
	}
	return q
}

// inList is the condition that the columns equal one of the tuples.
func inList(dl dialect, cols []*Column, vals [][][]byte) string {
	var b strings.Builder
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = dl.quote(c.Name)
	}
	if len(cols) == 1 {
		b.WriteString(names[0] + " IN (")
	} else {
		b.WriteString("(" + strings.Join(names, ", ") + ") IN (")
	}
	for i, v := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		if len(cols) > 1 {
			b.WriteByte('(')
		}
		for j, x := range v {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(dl.literal(cols[j], x))
		}
		if len(cols) > 1 {
			b.WriteByte(')')
		}
	}
	b.WriteByte(')')
	return b.String()
}
