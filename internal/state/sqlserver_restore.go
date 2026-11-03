package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
)

// A restore brings the changed tables and sequences back item by item, in
// one transaction on the working database:
//
//   - the database's DDL triggers are turned off first (ALTER TABLE and
//     ALTER SEQUENCE fire them, and one may refuse), then the DML triggers
//     of the tables it writes, so none fires;
//   - foreign keys that refer to those tables are turned off (NOCHECK), so
//     no cascade runs and rows can be written in any order, and so are
//     constraints the pristine copy's rows need not meet (not trusted);
//   - a table is emptied (TRUNCATE where SQL Server allows it, else
//     DELETE) and its rows copied from the pristine copy, or, for a large
//     table with a primary key, only the rows that differ are deleted and
//     copied (their keys found by comparing the encoded rows of both);
//   - identity columns are set where the pristine copy's stand
//     (ssIdentityPlan), sequences with RESTART WITH and one NEXT VALUE FOR
//     (ssRestartSQL);
//   - constraints are turned on again as they were (WITH CHECK when
//     trusted), then the triggers;
//   - where the identity columns and sequences stand is read again and
//     compared with the pristine copy before COMMIT.
//
// Anything else — another kind of item, a table whose definition changed,
// a table item by item cannot restore (memory-optimized, temporal, graph,
// ledger, FileTable, external, Always Encrypted, CLR type columns), an
// identity or sequence that does not end up where the pristine copy's
// stands — restores the whole database instead (sqlserver_database.go).

// sqlServerDeltaRows is the size from which a table with a primary key is
// restored by its changed rows only.
var sqlServerDeltaRows int64 = 5000

// ssFallback says why a restore restores the whole database.
type ssFallback struct{ why string }

func (f *ssFallback) Error() string { return f.why }

func fallback(format string, args ...any) error {
	return &ssFallback{why: fmt.Sprintf(format, args...)}
}

// ssInteger is a value DriftGate writes into a statement as a number.
var ssInteger = regexp.MustCompile(`^-?[0-9]{1,38}$`)

// Restore brings the given items of the working database back to the
// pristine copy; nil means every item that differs.
func (s *SQLServer) Restore(ctx context.Context, items []string) error {
	if _, err := s.server(ctx); err != nil {
		return err
	}
	if err := s.checkWorking(ctx); err != nil {
		return err
	}
	w, err := s.database(ctx, s.o.Working)
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("state %s: working database %s does not exist", s.o.Name, s.o.Working)
	}
	mark, err := s.pristineMark(ctx)
	if err != nil {
		return err
	}
	if w.State != "ONLINE" {
		fmt.Fprintf(s.o.Log, "state %s: working database %s is %s; restoring the whole database from the pristine copy's backup\n", s.o.Name, s.o.Working, w.State)
		return s.restoreDatabase(ctx, mark, w)
	}
	if err := s.recoverExcluded(ctx, w); err != nil {
		return err
	}
	if items == nil {
		if items, err = s.differing(ctx); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return nil
	}
	err = s.restoreItems(ctx, items)
	var fb *ssFallback
	if errors.As(err, &fb) {
		fmt.Fprintf(s.o.Log, "state %s: %s; restoring the whole database from the pristine copy's backup\n", s.o.Name, fb.why)
		return s.restoreDatabase(ctx, mark, w)
	}
	return err
}

// differing lists the items in which the working database and the
// pristine copy differ.
func (s *SQLServer) differing(ctx context.Context) ([]string, error) {
	w, err := s.Fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	p, err := s.fingerprint(ctx, c, s.o.Pristine, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return Diff(w, p.f), nil
}

// pristineMark checks the pristine copy before a restore: it exists, Seed
// made it of this working database on this server (its mark), and it is
// read-only and open to every session (a seed that stopped after the
// rename leaves it writable).
func (s *SQLServer) pristineMark(ctx context.Context) (*sqlServerMark, error) {
	info, err := s.server(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.database(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("state %s: pristine database %s does not exist; run driftgate seed", s.o.Name, s.o.Pristine)
	}
	if d.State != "ONLINE" {
		return nil, fmt.Errorf("state %s: pristine database %s is %s; run driftgate seed", s.o.Name, s.o.Pristine, d.State)
	}
	m, err := s.readMark(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	switch {
	case m == nil:
		return nil, fmt.Errorf("state %s: database %s was not made by driftgate seed (it has no DriftGate mark); run driftgate seed", s.o.Name, s.o.Pristine)
	case !strings.EqualFold(m.Working, s.o.Working):
		return nil, fmt.Errorf("state %s: database %s is the pristine copy of %s, not of %s; run driftgate seed or name another pristine database", s.o.Name, s.o.Pristine, m.Working, s.o.Working)
	case m.Server != info.Name:
		return nil, fmt.Errorf("state %s: pristine database %s was made on server %s, not on %s; run driftgate seed", s.o.Name, s.o.Pristine, m.Server, info.Name)
	}
	if !d.ReadOnly || d.UserAccess != "MULTI_USER" {
		s.closePool(s.o.Pristine)
		c, err := s.conn(ctx, "")
		if err != nil {
			return nil, err
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, "ALTER DATABASE "+qs(s.o.Pristine)+" SET MULTI_USER; ALTER DATABASE "+qs(s.o.Pristine)+" SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// catalog reads the inventory of the database c is connected to with the
// values of its identity columns and sequences.
func (s *SQLServer) catalog(ctx context.Context, c *sql.Conn) (*ssInventory, error) {
	inv, err := s.inventory(ctx, c)
	if err != nil {
		return nil, err
	}
	v, err := values(ctx, c)
	if err != nil {
		return nil, err
	}
	inv.apply(v)
	return inv, nil
}

// ssFacts are what decides how a table can be emptied: TRUNCATE is
// refused for a table a foreign key refers to (even one turned off) and
// for one an indexed view reads. Full-text indexes are filled again in
// the background after a restore.
type ssFacts struct {
	referenced, indexed, fulltext map[int64]bool
}

func (f *ssFacts) truncatable(id int64) bool { return !f.referenced[id] && !f.indexed[id] }

const ssFactsSQL = `
SELECT DISTINCT d.referenced_id FROM sys.sql_expression_dependencies d
  JOIN sys.indexes i ON i.object_id = d.referencing_id AND i.index_id = 1 JOIN sys.views v ON v.object_id = d.referencing_id
 WHERE d.referenced_id IS NOT NULL;
SELECT object_id FROM sys.fulltext_indexes;`

func (s *SQLServer) restoreFacts(ctx context.Context, c *sql.Conn, inv *ssInventory) (*ssFacts, error) {
	f := &ssFacts{referenced: map[int64]bool{}, indexed: map[int64]bool{}, fulltext: map[int64]bool{}}
	for _, fk := range inv.ForeignKeys {
		f.referenced[fk.Referenced] = true
	}
	ids := func(m map[int64]bool) func(*sql.Rows) error {
		return func(r *sql.Rows) error {
			var id int64
			if err := r.Scan(&id); err != nil {
				return err
			}
			m[id] = true
			return nil
		}
	}
	return f, readSets(ctx, c, ssFactsSQL, ids(f.indexed), ids(f.fulltext))
}

// ssRestoreTable is a table an item-level restore writes.
type ssRestoreTable struct {
	w, p   *ssTable
	clear  string // TRUNCATE or DELETE
	reseed string // the value of DBCC CHECKIDENT … RESEED, or ""
	delta  bool   // may be restored by its changed rows
	keys   string // the temporary table of the changed keys; "": every row is copied
}

// ssRestorePlan is what an item-level restore does.
type ssRestorePlan struct {
	tables []*ssRestoreTable
	seqs   [][2]*ssSequence // working, pristine
}

// ssInsertColumns are the columns a copy of a table's rows writes: not
// computed, rowversion, GENERATED ALWAYS, column sets or a graph table's
// internal columns.
func ssInsertColumns(t *ssTable) []ssColumn {
	var out []ssColumn
	for _, c := range t.Columns {
		if !c.Computed && !c.rowversion() && c.Generated == 0 && !c.internal() && !c.ColumnSet {
			out = append(out, c)
		}
	}
	return out
}

// ssSystemCLR are the CLR types every database has; values of others
// cannot be copied between databases.
var ssSystemCLR = map[string]bool{"hierarchyid": true, "geometry": true, "geography": true}

// ssUnrestorable says why a table cannot be restored item by item, or "".
func ssUnrestorable(t *ssTable) string {
	switch {
	case t.MemoryOptimized:
		return "is memory-optimized"
	case t.Temporal != 0:
		return "is system-versioned (temporal)"
	case t.Graph:
		return "is a graph table"
	case t.FileTable:
		return "is a FileTable"
	case t.Ledger != 0:
		return "is a ledger table"
	case t.External:
		return "is an external table"
	case len(ssInsertColumns(t)) == 0:
		return "has no column a copy can write"
	}
	for _, c := range t.Columns {
		if c.Encrypted {
			return "has a column encrypted with Always Encrypted"
		}
		if c.CLR && !ssSystemCLR[c.Type] {
			return "has a column of the CLR type " + c.Type
		}
	}
	return ""
}

// ssIdentityPlan says how to empty a table and where to reseed it so its
// identity column stands where target does (ss0: last_value NULL means
// never used since created or truncated, and the next value is then
// IDENT_CURRENT; else last_value plus the increment). empty: target's
// table has no rows. ok is false when that cannot be done without
// restoring the database: last_value back to NULL needs TRUNCATE, and a
// non-NULL last_value on an empty table needs a table that had rows.
func ssIdentityPlan(target, now *ssIdentity, empty, truncatable bool) (clear, reseed string, ok bool) {
	switch {
	case target.Last.Valid && empty:
		if !now.Last.Valid {
			return "", "", false
		}
		return "DELETE", target.Last.String, true // TRUNCATE would make last_value NULL
	case target.Last.Valid && truncatable:
		return "TRUNCATE", target.Last.String, true
	case target.Last.Valid:
		return "DELETE", target.Last.String, true
	case !now.Last.Valid: // both unused: the table is empty
		if now.Current != target.Current {
			reseed = target.Current
		}
		return "DELETE", reseed, true
	case !truncatable:
		return "", "", false
	}
	if target.Current != now.Seed {
		reseed = target.Current
	}
	return "TRUNCATE", reseed, true
}

// planRestore decides how to restore items, or returns an ssFallback.
func (s *SQLServer) planRestore(winv, pinv *ssInventory, items []string, facts *ssFacts) (*ssRestorePlan, error) {
	plan := &ssRestorePlan{}
	seqs := func(inv *ssInventory, item string) *ssSequence {
		for _, q := range inv.Sequences {
			if q.Item == item {
				return q
			}
		}
		return nil
	}
	for _, item := range items {
		kind, _, _ := strings.Cut(item, ":")
		switch kind {
		case "table":
			wt, pt := winv.ByItem[item], pinv.ByItem[item]
			switch {
			case wt == nil && pt == nil, wt != nil && wt.Excluded, pt != nil && pt.Excluded:
				continue
			case wt == nil || pt == nil:
				return nil, fallback("%s was created or dropped", item)
			case winv.base[item] != pinv.base[item]:
				return nil, fallback("the definition of %s changed", item)
			}
			if why := ssUnrestorable(pt); why != "" {
				return nil, fallback("%s %s", item, why)
			}
			rt := &ssRestoreTable{w: wt, p: pt, clear: "DELETE"}
			truncatable := facts.truncatable(wt.ID)
			if pt.Identity != nil {
				var ok bool
				if rt.clear, rt.reseed, ok = ssIdentityPlan(pt.Identity, wt.Identity, pt.Rows == 0, truncatable); !ok {
					return nil, fallback("the identity column of %s cannot be set back to where the pristine copy's stands item by item", item)
				}
				if rt.reseed != "" && !ssInteger.MatchString(rt.reseed) {
					return nil, fallback("the identity value of %s cannot be written", item)
				}
			} else if truncatable {
				rt.clear = "TRUNCATE"
			}
			rt.delta = pt.Key != nil && pt.Rows >= sqlServerDeltaRows && (pt.Identity == nil || pt.Identity.Last.Valid)
			plan.tables = append(plan.tables, rt)
		case "sequence":
			wq, pq := seqs(winv, item), seqs(pinv, item)
			switch {
			case wq == nil && pq == nil:
				continue
			case wq == nil || pq == nil:
				return nil, fallback("%s was created or dropped", item)
			case winv.base[item] != pinv.base[item]:
				return nil, fallback("the definition of %s changed", item)
			}
			if _, ok := ssRestartSQL("", pq, 0); !ok {
				return nil, fallback("the value of %s cannot be written", item)
			}
			plan.seqs = append(plan.seqs, [2]*ssSequence{wq, pq})
		default:
			return nil, fallback("%s changed, and a %s is restored with the whole database only", item, kind)
		}
	}
	return plan, nil
}

// ssRestartSQL sets a sequence where want stands (ss0): RESTART WITH
// rewrites current_value and clears last_used_value; one NEXT VALUE FOR
// then hands out and uses the pristine copy's last value. n numbers the
// variable within the batch. ok is false for a value that is not a
// number.
func ssRestartSQL(name string, want *ssSequence, n int) (string, bool) {
	if !want.LastUsed.Valid {
		if !ssInteger.MatchString(want.Current) {
			return "", false
		}
		return fmt.Sprintf("ALTER SEQUENCE %s RESTART WITH %s;\n", name, want.Current), true
	}
	if !ssInteger.MatchString(want.LastUsed.String) {
		return "", false
	}
	return fmt.Sprintf("ALTER SEQUENCE %s RESTART WITH %s;\nDECLARE @dg_n%d sql_variant = NEXT VALUE FOR %s;\n", name, want.LastUsed.String, n, name), true
}

// ssClearSQL empties a table.
func ssClearSQL(clear string, t *ssTable) string {
	if clear == "TRUNCATE" {
		return "TRUNCATE TABLE " + t.qname()
	}
	if t.MemoryOptimized {
		return "DELETE FROM " + t.qname() + " WITH (SNAPSHOT)"
	}
	return "DELETE FROM " + t.qname()
}

// ssGuard is what a write of rows turns off first and on again after it:
// triggers, foreign keys and constraints.
type ssGuard struct {
	off      []string
	checks   []string     // constraints on again, as they were
	tolerant []ssTolerant // trusted foreign keys of excluded tables (tolerate)
	triggers []string     // triggers on again, the database's DDL triggers last
}

type ssTolerant struct{ table, schema, name string }

// ssGuardFor is the guard of writing the rows of the tables in set.
// tolerant tells the tables whose trusted foreign keys stay trusted only
// when their rows allow it (excluded tables: their rows are not the
// pristine copy's).
func ssGuardFor(inv *ssInventory, set map[int64]bool, tolerant func(*ssTable) bool) *ssGuard {
	g := &ssGuard{}
	var ddl []string
	for _, tr := range inv.Triggers {
		if !tr.Disabled && tr.Table == 0 {
			g.off = append(g.off, "DISABLE TRIGGER "+qs(tr.Name)+" ON DATABASE")
			ddl = append(ddl, "ENABLE TRIGGER "+qs(tr.Name)+" ON DATABASE")
		}
	}
	for _, tr := range inv.Triggers {
		if t := inv.byID[tr.Table]; t != nil && !tr.Disabled && set[tr.Table] {
			g.off = append(g.off, "DISABLE TRIGGER "+qs2(tr.Schema, tr.Name)+" ON "+t.qname())
			g.triggers = append(g.triggers, "ENABLE TRIGGER "+qs2(tr.Schema, tr.Name)+" ON "+t.qname())
		}
	}
	g.triggers = append(g.triggers, ddl...)
	for _, fk := range inv.ForeignKeys {
		child := inv.byID[fk.Table]
		if fk.Disabled || child == nil {
			continue
		}
		alter := "ALTER TABLE " + child.qname()
		switch {
		case set[fk.Referenced]:
			g.off = append(g.off, alter+" NOCHECK CONSTRAINT "+qs(fk.Name))
			switch {
			case fk.NotTrusted:
				g.checks = append(g.checks, alter+" CHECK CONSTRAINT "+qs(fk.Name))
			case tolerant(child):
				g.tolerant = append(g.tolerant, ssTolerant{table: child.qname(), schema: child.Schema, name: fk.Name})
			default:
				g.checks = append(g.checks, alter+" WITH CHECK CHECK CONSTRAINT "+qs(fk.Name))
			}
		case set[fk.Table] && fk.NotTrusted:
			g.off = append(g.off, alter+" NOCHECK CONSTRAINT "+qs(fk.Name))
			g.checks = append(g.checks, alter+" CHECK CONSTRAINT "+qs(fk.Name))
		}
	}
	for _, ck := range inv.Checks {
		if t := inv.byID[ck.Table]; t != nil && set[ck.Table] && !ck.Disabled && ck.NotTrusted {
			g.off = append(g.off, "ALTER TABLE "+t.qname()+" NOCHECK CONSTRAINT "+qs(ck.Name))
			g.checks = append(g.checks, "ALTER TABLE "+t.qname()+" CHECK CONSTRAINT "+qs(ck.Name))
		}
	}
	return g
}

func ssStatements(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return strings.Join(list, ";\n") + ";\n"
}

func (g *ssGuard) offSQL() string { return ssStatements(g.off) }

// tolerate checks the rows of the tolerant foreign keys (DBCC
// CHECKCONSTRAINTS; a failed WITH CHECK would end the transaction) and
// returns the statements that turn everything on again: such a key stays
// trusted when no row breaks it, else it is turned on without the check.
func (s *SQLServer) tolerate(ctx context.Context, c *sql.Conn, g *ssGuard) (string, error) {
	checks := g.checks
	for _, t := range g.tolerant {
		rows, err := c.QueryContext(ctx, "DBCC CHECKCONSTRAINTS ("+sqlServerString(qs2(t.schema, t.name))+") WITH ALL_CONSTRAINTS, NO_INFOMSGS")
		if err != nil {
			return "", err
		}
		broken := rows.Next()
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		if broken {
			fmt.Fprintf(s.o.Log, "state %s: foreign key %s of excluded table %s is no longer trusted: its rows refer to rows the restore removed\n", s.o.Name, t.name, t.table)
			checks = append(checks, "ALTER TABLE "+t.table+" CHECK CONSTRAINT "+qs(t.name))
		} else {
			checks = append(checks, "ALTER TABLE "+t.table+" WITH CHECK CHECK CONSTRAINT "+qs(t.name))
		}
	}
	return ssStatements(checks) + ssStatements(g.triggers), nil
}

// restoreItems restores items one by one (see above), or returns an
// ssFallback before it changed anything.
func (s *SQLServer) restoreItems(ctx context.Context, items []string) error {
	pc, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return err
	}
	pinv, err := s.catalog(ctx, pc)
	pc.Close()
	if err != nil {
		return err
	}
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			// IDENTITY_INSERT is session state that outlives the
			// transaction: the connection does not go back to the pool.
			discard(c)
			s.forget()
		}
	}()
	winv, err := s.catalog(ctx, c)
	if err != nil {
		return err
	}
	facts, err := s.restoreFacts(ctx, c, winv)
	if err != nil {
		return err
	}
	plan, err := s.planRestore(winv, pinv, items, facts)
	if err != nil {
		return err
	}
	if len(plan.tables)+len(plan.seqs) == 0 {
		done = true
		c.Close()
		return nil
	}
	if err := s.findChanged(ctx, c, plan); err != nil {
		return err
	}
	set := map[int64]bool{}
	for _, rt := range plan.tables {
		set[rt.w.ID] = true
	}
	g := ssGuardFor(winv, set, func(t *ssTable) bool { return t.Excluded })
	batch := "BEGIN TRANSACTION;\n" + g.offSQL() + s.restoreSQL(plan)
	if len(g.tolerant) == 0 {
		batch += ssStatements(g.checks) + ssStatements(g.triggers)
	}
	if _, err := c.ExecContext(ctx, batch); err != nil {
		return fmt.Errorf("state %s: restore: %w", s.o.Name, err)
	}
	if len(g.tolerant) > 0 {
		on, err := s.tolerate(ctx, c, g)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, on); err != nil {
			return fmt.Errorf("state %s: restore: %w", s.o.Name, err)
		}
	}
	after, err := values(ctx, c)
	if err != nil {
		return err
	}
	if why := plan.check(after, pinv); why != "" {
		if _, err := c.ExecContext(ctx, "IF @@TRANCOUNT > 0 ROLLBACK"); err != nil {
			return err
		}
		return fallback("%s", why)
	}
	chaos.Point("state.sqlserver.restore")
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("state %s: restore: %w", s.o.Name, err)
	}
	var tables []*ssTable
	for _, rt := range plan.tables {
		if rt.w.rowversion() != "" {
			tables = append(tables, rt.w)
		}
	}
	if err := s.rememberRowversions(ctx, c, tables); err != nil {
		return err
	}
	if err := s.waitFullText(ctx, c, plan, facts); err != nil {
		return err
	}
	done = true
	c.Close()
	s.restored(items)
	return nil
}

// check compares where the identity columns and sequences of the plan
// stand now (w) with the pristine copy's; "" when they are the same.
func (p *ssRestorePlan) check(w *ssValues, pinv *ssInventory) string {
	for _, rt := range p.tables {
		if rt.p.Identity == nil {
			continue
		}
		got := w.ident[rt.w.ID]
		want := [2]string{"NULL", rt.p.Identity.Current}
		if rt.p.Identity.Last.Valid {
			want = [2]string{rt.p.Identity.Last.String, ""}
		}
		if got != want {
			return fmt.Sprintf("the identity column of %s stands at %v after the restore, not at %v as in the pristine copy", rt.w.Item, got, want)
		}
	}
	for _, q := range p.seqs {
		last := "NULL"
		if q[1].LastUsed.Valid {
			last = q[1].LastUsed.String
		}
		exhausted := "0"
		if q[1].Exhausted {
			exhausted = "1"
		}
		want := [3]string{q[1].Current, last, exhausted}
		if got := w.seq[q[0].ID]; got != want {
			return fmt.Sprintf("sequence %s stands at %v after the restore, not at %v as in the pristine copy", q[0].Item, got, want)
		}
	}
	return ""
}

// restoreSQL is the part of the restore's batch that writes rows,
// identity columns and sequences.
func (s *SQLServer) restoreSQL(plan *ssRestorePlan) string {
	var b strings.Builder
	pristine := qs(s.o.Pristine) + "."
	for _, rt := range plan.tables {
		t := rt.w.qname()
		var names, pnames []string
		for _, c := range ssInsertColumns(rt.p) {
			names = append(names, qs(c.Name))
			pnames = append(pnames, "p."+qs(c.Name))
		}
		list := strings.Join(names, ", ")
		if rt.keys != "" {
			fmt.Fprintf(&b, "DELETE w FROM %s w JOIN %s k ON %s;\n", t, rt.keys, ssKeyJoin(rt.p.Key, "w", "k"))
		} else {
			fmt.Fprintf(&b, "%s;\n", ssClearSQL(rt.clear, rt.w))
		}
		if rt.p.Identity != nil {
			fmt.Fprintf(&b, "SET IDENTITY_INSERT %s ON;\n", t)
		}
		if rt.keys != "" {
			fmt.Fprintf(&b, "INSERT INTO %s (%s) SELECT %s FROM %s%s p JOIN %s k ON %s;\n", t, list, strings.Join(pnames, ", "), pristine, rt.p.qname(), rt.keys, ssKeyJoin(rt.p.Key, "p", "k"))
		} else {
			fmt.Fprintf(&b, "INSERT INTO %s WITH (TABLOCK) (%s) SELECT %s FROM %s%s;\n", t, list, list, pristine, rt.p.qname())
		}
		if rt.p.Identity != nil {
			fmt.Fprintf(&b, "SET IDENTITY_INSERT %s OFF;\n", t)
			if rt.reseed != "" {
				fmt.Fprintf(&b, "DBCC CHECKIDENT (%s, RESEED, %s) WITH NO_INFOMSGS;\n", sqlServerString(t), rt.reseed)
			}
		}
	}
	for i, q := range plan.seqs {
		stmt, _ := ssRestartSQL(qs2(q[0].Schema, q[0].Name), q[1], i+1)
		b.WriteString(stmt)
	}
	return b.String()
}

// ssKeyJoin joins two aliases of a table on its key columns.
func ssKeyJoin(key []string, a, b string) string {
	var on []string
	for _, k := range key {
		on = append(on, a+"."+qs(k)+" = "+b+"."+qs(k))
	}
	return strings.Join(on, " AND ")
}

// ssParallel lets the comparison of a large table use every core: SQL
// Server otherwise reads it on one, as the plan of a query that returns
// few rows looks cheap. A server that does not know the hint runs the
// comparison without it.
const ssParallel = " OPTION (USE HINT ('ENABLE_PARALLEL_PLAN_PREFERENCE'))"

// findChanged finds the keys of the changed rows of every large table of
// the plan (delta): rows whose encoding (sqlServerRow, as the fingerprint
// reads them) differs from the pristine copy's row with the same key, and
// rows with a key on one side only. They go into a temporary table of the
// connection. A table where more than a quarter of the rows changed is
// copied whole.
func (s *SQLServer) findChanged(ctx context.Context, c *sql.Conn, plan *ssRestorePlan) error {
	n := 0
	for _, rt := range plan.tables {
		if !rt.delta {
			continue
		}
		row, err := sqlServerRow(rt.p.Columns)
		if err != nil {
			continue // copied whole: the copy does not need the encoding
		}
		n++
		k := fmt.Sprintf("#dg_k%d", n)
		var keys, wkeys, pkeys []string
		for _, col := range rt.p.Key {
			keys = append(keys, qs(col))
			wkeys = append(wkeys, "w."+qs(col))
			pkeys = append(pkeys, "p."+qs(col))
		}
		list := strings.Join(keys, ", ")
		w, p := rt.w.qname(), qs(s.o.Pristine)+"."+rt.p.qname()
		// UNION ALL: the temporary table gets the key columns' types and
		// collations without an IDENTITY property.
		if _, err := c.ExecContext(ctx, fmt.Sprintf("SELECT TOP (0) %s INTO %s FROM %s UNION ALL SELECT TOP (0) %s FROM %s", list, k, p, list, p)); err != nil {
			return err
		}
		differ := fmt.Sprintf("INSERT INTO %s SELECT %s FROM (SELECT %s, %s AS dg_r FROM %s) w JOIN (SELECT %s, %s AS dg_r FROM %s) p ON %s WHERE w.dg_r <> p.dg_r OR DATALENGTH(w.dg_r) <> DATALENGTH(p.dg_r)",
			k, strings.Join(wkeys, ", "), list, row, w, list, row, p, ssKeyJoin(rt.p.Key, "w", "p"))
		_, err = c.ExecContext(ctx, differ+ssParallel)
		if sqlServerNumber(err) == 10715 { // not a valid hint (older servers)
			_, err = c.ExecContext(ctx, differ)
		}
		if err != nil {
			return err
		}
		var count int64
		err = c.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO %[1]s SELECT %[2]s FROM %[3]s w WHERE NOT EXISTS (SELECT 1 FROM %[4]s p WHERE %[5]s);
INSERT INTO %[1]s SELECT %[6]s FROM %[4]s p WHERE NOT EXISTS (SELECT 1 FROM %[3]s w WHERE %[5]s);
SELECT COUNT_BIG(*) FROM %[1]s`, k, strings.Join(wkeys, ", "), w, p, ssKeyJoin(rt.p.Key, "w", "p"), strings.Join(pkeys, ", "))).Scan(&count)
		if err != nil {
			return err
		}
		if count*4 <= rt.p.Rows {
			rt.keys = k
		}
	}
	return nil
}

// waitFullText waits until the full-text indexes of the restored tables
// are filled again (they follow a change in the background), up to a
// minute.
func (s *SQLServer) waitFullText(ctx context.Context, c *sql.Conn, plan *ssRestorePlan, facts *ssFacts) error {
	var ids []string
	for _, rt := range plan.tables {
		if facts.fulltext[rt.w.ID] {
			ids = append(ids, fmt.Sprint(rt.w.ID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	q := "SELECT COUNT(*) FROM sys.fulltext_indexes WHERE object_id IN (" + strings.Join(ids, ", ") + ") AND OBJECTPROPERTYEX(object_id, 'TableFulltextPopulateStatus') <> 0"
	deadline := time.Now().Add(time.Minute)
	for {
		var busy int
		if err := c.QueryRowContext(ctx, q).Scan(&busy); err != nil {
			return err
		}
		if busy == 0 || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
