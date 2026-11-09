package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/lazy"
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

// sqlServerAfterDiff, when set, runs after a restore found the changed
// rows of its large tables (tests: a write made meanwhile).
var sqlServerAfterDiff func()

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
var ssInteger = lazy.Regexp(`^-?[0-9]{1,38}$`)

// Restore brings the given items of the working database back to the
// pristine copy; nil means every item that differs. It runs again while
// another database of the server is in transition, or the working
// database's own state stops a statement on another one (workingStopped).
func (s *SQLServer) Restore(ctx context.Context, items []string) error {
	s.ended = nil // over the runs: each may end sessions, a run stopped before it ended them counts them again
	try := s.restore
	if s.hooks != nil && s.hooks.restore != nil {
		try = s.hooks.restore
	}
	return s.retryWhile(ctx, s.workingStopped, func() error { return try(ctx, items) })
}

func (s *SQLServer) restore(ctx context.Context, items []string) (err error) {
	defer func() { err = s.denied(err) }()
	if _, err := s.server(ctx); err != nil {
		return err
	}
	if err := s.checkWorking(ctx); err != nil {
		return err
	}
	// The working database, the pristine copy with its mark and a
	// <pristine>_dgx, in one round trip.
	dbs, m, err := s.databases(ctx, []string{s.o.Working, s.o.Pristine, s.excludedDB()}, s.o.Pristine)
	if err != nil {
		return err
	}
	w, dgx := dbs[0], dbs[2]
	if w == nil {
		return fmt.Errorf("state %s: working database %s does not exist", s.o.Name, s.o.Working)
	}
	if w.UserAccess == "SINGLE_USER" {
		// DriftGate's connection left in the pool would hold the one
		// session the database allows.
		defer s.closePool(s.o.Working)
	}
	mark, err := s.checkMark(ctx, dbs[1], m)
	if err != nil {
		return err
	}
	if w.State != "ONLINE" {
		whole, err := s.notOnline(ctx, w)
		if err != nil {
			return err
		}
		if whole {
			return s.restoreWhole(ctx, mark, w)
		}
		if w, err = s.database(ctx, s.o.Working); err != nil {
			return err
		}
		if dgx, err = s.database(ctx, s.excludedDB()); err != nil { // notOnline may have dropped it
			return err
		}
	}
	if err := s.recoverExcludedOf(ctx, w, dgx); err != nil {
		return err
	}
	if items == nil {
		if items, err = s.differing(ctx, mark); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return nil
	}
	err = s.restoreItems(ctx, items, mark)
	var fb *ssFallback
	if errors.As(err, &fb) {
		fmt.Fprintf(s.o.Log, "state %s: %s; restoring the whole database from the pristine copy's backup\n", s.o.Name, fb.why)
		return s.restoreWhole(ctx, mark, w)
	}
	return err
}

// notOnline decides what a restore does with a working database w that is
// not online: true restores the whole database. A restore of the whole
// database that stopped halfway left it restoring or offline, with the
// rows of excluded tables saved when there are any; it is unusable until
// a restore completes it. One the user took offline is brought online
// first, so its excluded rows can be saved. Any other state, with tables
// excluded whose rows DriftGate has not saved, is refused: a restore would
// lose them.
func (s *SQLServer) notOnline(ctx context.Context, w *sqlServerDatabase) (bool, error) {
	saved, err := s.leftSave(ctx, w)
	switch {
	case err != nil:
		return false, err
	case saved || w.State == "RESTORING" && len(s.o.Exclude) == 0:
		fmt.Fprintf(s.o.Log, "state %s: working database %s is %s: a restore of the whole database stopped halfway, and the database is unusable until this restore completes it\n", s.o.Name, s.o.Working, w.State)
		return true, nil
	case w.State == "OFFLINE":
		fmt.Fprintf(s.o.Log, "state %s: working database %s is offline; bringing it online\n", s.o.Name, s.o.Working)
		c, err := s.conn(ctx, "")
		if err != nil {
			return false, err
		}
		defer c.Close()
		_, err = c.ExecContext(ctx, "ALTER DATABASE "+qms(s.o.Working)+" SET ONLINE")
		return false, err
	case len(s.o.Exclude) == 0:
		fmt.Fprintf(s.o.Log, "state %s: working database %s is %s; restoring the whole database from the pristine copy's backup\n", s.o.Name, s.o.Working, w.State)
		return true, nil
	}
	return false, fmt.Errorf("state %s: working database %s is %s, and DriftGate has not saved the rows of its excluded tables, which a restore of the whole database would lose; bring it online (or drop it, if those rows do not matter) and restore again", s.o.Name, s.o.Working, w.State)
}

// restoreWhole restores the whole database and checks every item against
// the pristine copy's fingerprint: a restore of the whole database
// changes items the caller did not name.
func (s *SQLServer) restoreWhole(ctx context.Context, mark *sqlServerMark, w *sqlServerDatabase) (err error) {
	saved, err := s.restoreDatabase(ctx, mark, w)
	if err != nil {
		return err
	}
	// After the check, which takes several sessions: a working database
	// that was SINGLE_USER allows one.
	switch {
	case saved:
		defer func() {
			if eerr := s.endSave(ctx); err == nil {
				err = eerr
			}
		}()
	case w.State == "ONLINE" && w.UserAccess == "SINGLE_USER":
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), sqlServerCleanup)
			defer cancel()
			m, cerr := s.conn(cctx, "")
			if cerr == nil {
				defer m.Close()
				cerr = s.giveBack(cctx, m, "SINGLE_USER")
			}
			if err == nil {
				err = cerr
			}
		}()
	}
	p, err := s.pristineFingerprint(ctx, mark)
	if err != nil {
		return err
	}
	f, err := s.Fingerprint(ctx, nil)
	if err != nil {
		return err
	}
	if d := Diff(f, p); len(d) > 0 {
		return fmt.Errorf("state %s: after a restore of the whole database %d items differ from the pristine copy: %v", s.o.Name, len(d), limitItems(d, 20))
	}
	return nil
}

func limitItems(items []string, n int) []string {
	if len(items) > n {
		return append(items[:n:n], "…")
	}
	return items
}

// differing lists the items in which the working database and the
// pristine copy differ.
func (s *SQLServer) differing(ctx context.Context, mark *sqlServerMark) ([]string, error) {
	w, err := s.Fingerprint(ctx, nil)
	if err != nil {
		return nil, err
	}
	p, err := s.pristineFingerprint(ctx, mark)
	if err != nil {
		return nil, err
	}
	return Diff(w, p), nil
}

// SeededFingerprint is the fingerprint of the pristine copy itself, with
// the items named as the working database's: the record of a seed. The
// working database fingerprinted after the seed would differ from the
// copy by every write made since its backup.
func (s *SQLServer) SeededFingerprint(ctx context.Context) (f Fingerprint, err error) {
	err = s.retryOthers(ctx, func() error {
		f, err = s.seededFingerprint(ctx)
		return err
	})
	return f, err
}

func (s *SQLServer) seededFingerprint(ctx context.Context) (Fingerprint, error) {
	mark, err := s.pristineMark(ctx)
	if err != nil {
		return nil, s.denied(err)
	}
	p, err := s.pristineFingerprint(ctx, mark)
	if err != nil {
		return nil, s.denied(err)
	}
	return maps.Clone(p), nil
}

// RestoreOnlyFingerprint hashes the restore_only tables of the working
// database, or of the pristine copy (RestoreOnly), read anew: the last
// fingerprint and its signals stay as they were. A working database that
// is not online has none of them, so the restore brings them all back.
func (s *SQLServer) RestoreOnlyFingerprint(ctx context.Context, pristine bool) (Fingerprint, error) {
	return s.read(ctx, pristine, fpRestoreOnly)
}

// SchemaFingerprint hashes the definitions of the working database, or of
// the pristine copy (Schemer): nil for a working database a restore of
// the whole database left unusable (schemaReadable).
func (s *SQLServer) SchemaFingerprint(ctx context.Context, pristine bool) (Fingerprint, error) {
	return s.read(ctx, pristine, fpSchema)
}

// read fingerprints the working database, or the pristine copy, as mode
// reads it, retried as Fingerprint is (retryOthers).
func (s *SQLServer) read(ctx context.Context, pristine bool, mode fpMode) (f Fingerprint, err error) {
	err = s.retryOthers(ctx, func() error {
		f, err = s.readOnce(ctx, pristine, mode)
		return err
	})
	return f, err
}

func (s *SQLServer) readOnce(ctx context.Context, pristine bool, mode fpMode) (Fingerprint, error) {
	db := s.o.Working
	var inv *ssInventory
	if pristine {
		mark, err := s.pristineMark(ctx)
		if err != nil {
			return nil, s.denied(err)
		}
		if inv, err = s.pristineCatalog(ctx, mark); err != nil {
			return nil, s.denied(err)
		}
		db = s.o.Pristine
	} else {
		d, err := s.database(ctx, db)
		if err != nil {
			return nil, err
		}
		if !mode.rows() {
			if ok, err := s.schemaReadable(ctx, d); !ok || err != nil {
				return nil, err
			}
		} else if d == nil || d.State != "ONLINE" {
			return Fingerprint{}, nil
		}
	}
	c, err := s.conn(ctx, db)
	if err != nil {
		return nil, s.denied(err)
	}
	defer c.Close()
	if !mode.rows() { // the definitions, without where identities and sequences stand
		if inv == nil {
			if inv, err = s.inventory(ctx, c); err != nil {
				return nil, err
			}
		}
		f := Fingerprint{}
		for item, def := range inv.base {
			f[item] = hash([]byte(def))
		}
		return f, nil
	}
	fp, err := s.fingerprint(ctx, c, db, nil, inv, nil, nil, mode)
	if err != nil {
		return nil, err
	}
	return fp.f, nil
}

// schemaReadable reports whether the schema of the working database d can
// be read. One a restore of the whole database that stopped left unusable
// (its excluded rows saved, or RESTORING with nothing excluded) cannot;
// one taken offline is brought online first (notOnline); any other state
// is an error.
func (s *SQLServer) schemaReadable(ctx context.Context, d *sqlServerDatabase) (bool, error) {
	switch {
	case d == nil:
		return false, fmt.Errorf("state %s: working database %s does not exist", s.o.Name, s.o.Working)
	case d.State == "ONLINE":
		return true, nil
	case d.State != "OFFLINE" && d.State != "RESTORING":
		return false, fmt.Errorf("state %s: working database %s is %s", s.o.Name, s.o.Working, d.State)
	}
	whole, err := s.notOnline(ctx, d)
	return !whole && err == nil, err
}

// PristineID names the pristine copy: the backup set GUID in its mark,
// new with every seed. A record of the copy's fingerprint kept with it
// shows whether the copy is still the one recorded.
func (s *SQLServer) PristineID(ctx context.Context) (id string, err error) {
	err = s.retryOthers(ctx, func() error {
		mark, err := s.pristineMark(ctx)
		if err == nil {
			id = mark.Fallback
		}
		return err
	})
	return id, err
}

// pristineFingerprint fingerprints the pristine copy, once per copy (it is
// read-only).
func (s *SQLServer) pristineFingerprint(ctx context.Context, mark *sqlServerMark) (Fingerprint, error) {
	inv, err := s.pristineCatalog(ctx, mark)
	if err != nil {
		return nil, err
	}
	if s.pcat.f != nil {
		return s.pcat.f, nil
	}
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	fp, err := s.fingerprint(ctx, c, s.o.Pristine, nil, inv, nil, nil, fpRows)
	if err != nil {
		return nil, err
	}
	s.pcat.f = fp.f
	return fp.f, nil
}

// pristineMark checks the pristine copy before a restore: it exists, Seed
// made it of this working database on this server (its mark), and it is
// read-only and open to every session (a seed that stopped after the
// rename leaves it writable).
func (s *SQLServer) pristineMark(ctx context.Context) (*sqlServerMark, error) {
	dbs, m, err := s.databases(ctx, []string{s.o.Pristine}, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	return s.checkMark(ctx, dbs[0], m)
}

// checkMark is pristineMark of the pristine copy d and its mark m, as
// databases read them.
func (s *SQLServer) checkMark(ctx context.Context, d *sqlServerDatabase, m *sqlServerMark) (*sqlServerMark, error) {
	info, err := s.server(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("state %s: pristine database %s does not exist; run driftgate seed", s.o.Name, s.o.Pristine)
	}
	if d.State != "ONLINE" {
		return nil, fmt.Errorf("state %s: pristine database %s is %s; run driftgate seed", s.o.Name, s.o.Pristine, d.State)
	}
	switch {
	case m == nil:
		return nil, fmt.Errorf("state %s: database %s was not made by driftgate seed (it has no DriftGate mark); run driftgate seed", s.o.Name, s.o.Pristine)
	case !strings.EqualFold(m.Working, s.o.Working):
		return nil, fmt.Errorf("state %s: database %s is the pristine copy of %s, not of %s; run driftgate seed or name another pristine database", s.o.Name, s.o.Pristine, m.Working, s.o.Working)
	case m.Server != info.Name:
		return nil, fmt.Errorf("state %s: pristine database %s was made on server %s, not on %s; run driftgate seed", s.o.Name, s.o.Pristine, m.Server, info.Name)
	case s.seen != "" && !strings.EqualFold(m.Fallback, s.seen):
		return nil, fmt.Errorf("state %s: pristine database %s was replaced by another seed while DriftGate ran (backup %s, not %s); run the command again", s.o.Name, s.o.Pristine, m.Fallback, s.seen)
	}
	s.seen = m.Fallback
	if !d.ReadOnly || d.UserAccess != "MULTI_USER" {
		s.closePool(s.o.Pristine)
		c, err := s.conn(ctx, "")
		if err != nil {
			return nil, err
		}
		defer c.Close()
		if _, err := c.ExecContext(ctx, "ALTER DATABASE "+qms(s.o.Pristine)+" SET MULTI_USER; ALTER DATABASE "+qms(s.o.Pristine)+" SET READ_ONLY WITH ROLLBACK IMMEDIATE"); err != nil {
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

// ssPristineCatalog is the catalog of the pristine copy whose mark names
// the backup set fallback.
type ssPristineCatalog struct {
	fallback string
	inv      *ssInventory
	f        Fingerprint // pristineFingerprint, once read
}

// pristineCatalog reads the pristine copy's catalog once per copy.
func (s *SQLServer) pristineCatalog(ctx context.Context, mark *sqlServerMark) (*ssInventory, error) {
	if s.pcat != nil && mark.Fallback != "" && s.pcat.fallback == mark.Fallback {
		return s.pcat.inv, nil
	}
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	inv, err := s.catalog(ctx, c)
	if err != nil {
		return nil, err
	}
	s.pcat = &ssPristineCatalog{fallback: mark.Fallback, inv: inv}
	return inv, nil
}

// workingCatalog is catalog for the working database: the inventory the
// last fingerprint read when the catalog did not change since (the guard
// of sqlserver_reuse.go), with the values read now.
func (s *SQLServer) workingCatalog(ctx context.Context, c *sql.Conn) (*ssInventory, error) {
	sig, err := s.signals(ctx, c, false, true, false)
	if err != nil {
		return nil, err
	}
	inv := s.cached(sig)
	if inv == nil {
		if inv, err = s.inventory(ctx, c); err != nil {
			return nil, err
		}
		s.cache(sig, inv)
	}
	inv.apply(sig.values)
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
			// TRUNCATE where it is needed or saves time: it changes the
			// table's modify_date, so the next fingerprint and restore
			// read the catalog again (sqlserver_reuse.go).
			large := truncatable && wt.Rows >= sqlServerDeltaRows
			if pt.Identity != nil {
				var ok bool
				if rt.clear, rt.reseed, ok = ssIdentityPlan(pt.Identity, wt.Identity, pt.Rows == 0, truncatable); !ok {
					return nil, fallback("the identity column of %s cannot be set back to where the pristine copy's stands item by item", item)
				}
				if rt.reseed != "" && !ssInteger().MatchString(rt.reseed) {
					return nil, fallback("the identity value of %s cannot be written", item)
				}
				if rt.clear == "TRUNCATE" && pt.Identity.Last.Valid && !large {
					rt.clear = "DELETE" // the table gets rows: last_value is not NULL either way
				}
			} else if large {
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
		if !ssInteger().MatchString(want.Current) {
			return "", false
		}
		return fmt.Sprintf("ALTER SEQUENCE %s RESTART WITH %s;\n", name, want.Current), true
	}
	if !ssInteger().MatchString(want.LastUsed.String) {
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
	// altered are the tables whose definition the guard changes (ALTER
	// TABLE, DISABLE TRIGGER), ddl the database's DDL triggers it turns
	// off: their modify_date moves (ssOwnDDL).
	altered map[int64]bool
	ddl     []int64
}

type ssTolerant struct{ table, schema, name string }

// ssGuardFor is the guard of writing the rows of the tables in set.
// tolerant tells the tables whose trusted foreign keys stay trusted only
// when their rows allow it (excluded tables: their rows are not the
// pristine copy's). With own, the rows written are such rows, so the
// foreign keys of the tables in set are turned off too and checked after.
func ssGuardFor(inv *ssInventory, set map[int64]bool, tolerant func(*ssTable) bool, own bool) *ssGuard {
	g := &ssGuard{altered: map[int64]bool{}}
	var ddl []string
	for _, tr := range inv.Triggers {
		if !tr.Disabled && tr.Table == 0 {
			g.off = append(g.off, "DISABLE TRIGGER "+qms(tr.Name)+" ON DATABASE")
			ddl = append(ddl, "ENABLE TRIGGER "+qms(tr.Name)+" ON DATABASE")
			g.ddl = append(g.ddl, tr.ID)
		}
	}
	for _, tr := range inv.Triggers {
		if t := inv.byID[tr.Table]; t != nil && !tr.Disabled && set[tr.Table] {
			g.off = append(g.off, "DISABLE TRIGGER "+qs2(tr.Schema, tr.Name)+" ON "+t.qname())
			g.triggers = append(g.triggers, "ENABLE TRIGGER "+qs2(tr.Schema, tr.Name)+" ON "+t.qname())
			g.altered[t.ID] = true
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
		case set[fk.Referenced] || own && set[fk.Table]:
			g.altered[child.ID] = true
			g.off = append(g.off, alter+" NOCHECK CONSTRAINT "+qms(fk.Name))
			switch {
			case fk.NotTrusted:
				g.checks = append(g.checks, alter+" CHECK CONSTRAINT "+qms(fk.Name))
			case tolerant(child):
				g.tolerant = append(g.tolerant, ssTolerant{table: child.qname(), schema: child.Schema, name: fk.Name})
			default:
				g.checks = append(g.checks, alter+" WITH CHECK CHECK CONSTRAINT "+qms(fk.Name))
			}
		case set[fk.Table] && fk.NotTrusted:
			g.altered[child.ID] = true
			g.off = append(g.off, alter+" NOCHECK CONSTRAINT "+qms(fk.Name))
			g.checks = append(g.checks, alter+" CHECK CONSTRAINT "+qms(fk.Name))
		}
	}
	for _, ck := range inv.Checks {
		if t := inv.byID[ck.Table]; t != nil && set[ck.Table] && !ck.Disabled && ck.NotTrusted {
			g.altered[t.ID] = true
			g.off = append(g.off, "ALTER TABLE "+t.qname()+" NOCHECK CONSTRAINT "+qms(ck.Name))
			g.checks = append(g.checks, "ALTER TABLE "+t.qname()+" CHECK CONSTRAINT "+qms(ck.Name))
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
			checks = append(checks, "ALTER TABLE "+t.table+" CHECK CONSTRAINT "+qms(t.name))
		} else {
			checks = append(checks, "ALTER TABLE "+t.table+" WITH CHECK CHECK CONSTRAINT "+qms(t.name))
		}
	}
	return ssStatements(checks) + ssStatements(g.triggers), nil
}

// restoreItems restores items one by one (see above), or returns an
// ssFallback before it changed anything.
func (s *SQLServer) restoreItems(ctx context.Context, items []string, mark *sqlServerMark) error {
	pinv, err := s.pristineCatalog(ctx, mark)
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
	winv, err := s.workingCatalog(ctx, c)
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
	// The changed rows are found in the restore's transaction, under an
	// exclusive lock of each table: a write between the comparison and
	// the restore would otherwise stay.
	batch := "BEGIN TRANSACTION;\n"
	var locks string
	for _, rt := range plan.tables {
		if rt.delta {
			locks += "SELECT TOP (1) NULL FROM " + rt.w.qname() + " WITH (TABLOCKX);\n"
		}
	}
	if locks != "" {
		if _, err := c.ExecContext(ctx, batch+locks); err != nil {
			return fmt.Errorf("state %s: restore: locking the tables whose changed rows it finds: %w%s%s", s.o.Name, err, sqlServerAt(batch+locks, err), s.blockers(err))
		}
		batch = ""
		if err := s.findChanged(ctx, c, plan); err != nil {
			return err
		}
		if sqlServerAfterDiff != nil {
			sqlServerAfterDiff()
		}
	}
	set := map[int64]bool{}
	for _, rt := range plan.tables {
		set[rt.w.ID] = true
	}
	g := ssGuardFor(winv, set, func(t *ssTable) bool { return t.Excluded }, false)
	batch += g.offSQL() + s.restoreSQL(plan)
	if len(g.tolerant) == 0 {
		batch += ssStatements(g.checks) + ssStatements(g.triggers)
	}
	if _, err := c.ExecContext(ctx, batch); err != nil {
		return fmt.Errorf("state %s: restore: writing the rows from %s: %w%s%s", s.o.Name, s.o.Pristine, err, sqlServerAt(batch, err), s.blockers(err))
	}
	if len(g.tolerant) > 0 {
		on, err := s.tolerate(ctx, c, g)
		if err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, on); err != nil {
			return fmt.Errorf("state %s: restore: turning the constraints and triggers on again: %w%s", s.o.Name, err, sqlServerAt(on, err))
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
	own, err := s.ownDDL(ctx, c, g, plan)
	if err != nil {
		return err
	}
	chaos.Point("state.sqlserver.restore")
	if own, err = s.commitOwn(ctx, c, own); err != nil {
		return fmt.Errorf("state %s: restore: commit: %w", s.o.Name, err)
	}
	s.own = own
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

// ssModifyDate is a modify_date as CONVERT(varchar(30), …, 121) gives it.
var ssModifyDate = lazy.Regexp(`^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}$`)

// sqlServerOwnWait bounds how long a restore's COMMIT waits for the
// server's clock to pass its own changes of the catalog (ownDDL).
var sqlServerOwnWait = 50 * time.Millisecond

// ownDDL reads, in the restore's transaction (c), the modify_date of the
// objects whose definition it changed (ssOwnDDL): the tables it altered or
// truncated, every object of theirs (constraints, triggers), the
// sequences it restarted and the database's DDL triggers. Only dates of
// the last seconds up to the server's clock count: the restore's own
// changes are among them, an older date is not recent anyway, and a date
// ahead of the clock (another time zone, a clock set back) is never
// exempted. The transaction holds a schema lock on each until it ends, so
// no other session changes them before.
func (s *SQLServer) ownDDL(ctx context.Context, c *sql.Conn, g *ssGuard, plan *ssRestorePlan) (ssOwnDDL, error) {
	var own ssOwnDDL
	var objects, ddl []string
	for id := range g.altered {
		objects = append(objects, fmt.Sprint(id))
	}
	for _, rt := range plan.tables {
		if rt.keys == "" && rt.clear == "TRUNCATE" {
			objects = append(objects, fmt.Sprint(rt.w.ID))
		}
	}
	for _, id := range g.ddl {
		ddl = append(ddl, fmt.Sprint(id))
	}
	const recent = " AND modify_date BETWEEN DATEADD(second, -5, GETDATE()) AND GETDATE()"
	var parts []string
	if len(objects) > 0 {
		in := strings.Join(objects, ", ")
		parts = append(parts, "SELECT object_id, CONVERT(varchar(30), modify_date, 121) FROM sys.objects WHERE (object_id IN ("+in+") OR parent_object_id IN ("+in+"))"+recent)
	}
	if len(plan.seqs) > 0 {
		var seqs []string
		for _, q := range plan.seqs {
			seqs = append(seqs, fmt.Sprint(q[0].ID))
		}
		parts = append(parts, "SELECT object_id, CONVERT(varchar(30), modify_date, 121) FROM sys.objects WHERE object_id IN ("+strings.Join(seqs, ", ")+")"+recent)
	}
	if len(ddl) > 0 {
		parts = append(parts, "SELECT object_id, CONVERT(varchar(30), modify_date, 121) FROM sys.triggers WHERE parent_class = 0 AND object_id IN ("+strings.Join(ddl, ", ")+")"+recent)
	}
	if len(parts) == 0 {
		return own, nil
	}
	var values []string
	latest := ""
	err := readSets(ctx, c, strings.Join(parts, "\nUNION ALL ")+";", func(r *sql.Rows) error {
		var id int64
		var date string
		if err := r.Scan(&id, &date); err != nil {
			return err
		}
		if !ssModifyDate().MatchString(date) {
			return fmt.Errorf("state %s: SQL Server returned the modify_date %q", s.o.Name, date)
		}
		values = append(values, fmt.Sprintf("%d %s|", id, date))
		latest = max(latest, date)
		return nil
	})
	if err != nil || latest == "" {
		return own, err
	}
	return ssOwnDDL{values: "|" + strings.Join(values, ""), latest: latest}, nil
}

// commitOwn commits the restore's transaction (c) once the server's clock
// is 10 ms past the latest of own (ownDDL), so a change after it has a
// later modify_date however the clock's time is rounded to datetime's
// steps of about 3 ms. The wait is bounded (sqlServerOwnWait, and a number
// of rounds should the clock jump); when the clock has not passed by then,
// the restore commits all the same and returns no own changes: the next
// fingerprint reads the catalog anew.
func (s *SQLServer) commitOwn(ctx context.Context, c *sql.Conn, own ssOwnDDL) (ssOwnDDL, error) {
	if own.latest == "" {
		_, err := c.ExecContext(ctx, "COMMIT")
		return ssOwnDDL{}, err
	}
	past := "GETDATE() > DATEADD(millisecond, 10, CONVERT(datetime, '" + own.latest + "', 121))"
	var ok bool
	err := c.QueryRowContext(ctx, fmt.Sprintf(`DECLARE @dg_until datetime2 = DATEADD(millisecond, %d, SYSUTCDATETIME()), @dg_rounds int = 0;
WHILE NOT %s AND SYSUTCDATETIME() < @dg_until AND @dg_rounds < 1000000 BEGIN WAITFOR DELAY '00:00:00.001'; SET @dg_rounds += 1; END;
DECLARE @dg_past bit = CASE WHEN %s THEN 1 ELSE 0 END;
COMMIT;
SELECT @dg_past;`, sqlServerOwnWait.Milliseconds(), past, past)).Scan(&ok)
	if err != nil || !ok {
		return ssOwnDDL{}, err
	}
	own.at = time.Now()
	return own, nil
}

// blockers names, after a lock waited for too long (1222), the other
// sessions with an open transaction in the working database, as far as
// the login may see them (all with VIEW SERVER STATE).
func (s *SQLServer) blockers(err error) string {
	if sqlServerNumber(err) != 1222 {
		return ""
	}
	c, cerr := s.conn(context.Background(), "")
	if cerr != nil {
		return ""
	}
	defer c.Close()
	rows, qerr := c.QueryContext(context.Background(), `SELECT TOP (5) CONCAT(N'session ', session_id, ISNULL(N' (' + NULLIF(program_name, N'') + N')', N''), ISNULL(N' on ' + NULLIF(host_name, N''), N''), ISNULL(N' as ' + NULLIF(login_name, N''), N''))
		FROM sys.dm_exec_sessions WHERE database_id = DB_ID(@p1) AND session_id <> @@SPID AND is_user_process = 1 AND open_transaction_count > 0`, s.o.Working)
	if qerr != nil {
		return ""
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil {
			list = append(list, l)
		}
	}
	if len(list) == 0 {
		return " (another session holds locks on the tables; the login cannot see which without VIEW SERVER STATE)"
	}
	return " (open transactions: " + strings.Join(list, "; ") + ")"
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
	pristine := qms(s.o.Pristine) + "."
	for _, rt := range plan.tables {
		t := rt.w.qname()
		var names, pnames []string
		for _, c := range ssInsertColumns(rt.p) {
			names = append(names, qms(c.Name))
			pnames = append(pnames, "p."+qms(c.Name))
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
		on = append(on, a+"."+qms(k)+" = "+b+"."+qms(k))
	}
	return strings.Join(on, " AND ")
}

// ssParallel lets the comparison of a large table use every core: SQL
// Server otherwise reads it on one, as the plan of a query that returns
// few rows looks cheap. A server that does not know the hint runs the
// comparison without it.
const ssParallel = " OPTION (USE HINT ('ENABLE_PARALLEL_PLAN_PREFERENCE'))"

// findChanged finds the keys of the changed rows of every large table of
// the plan (delta) in one statement per table: a full join of the working
// table and the pristine copy's on the key, where a key is on one side
// only or a column differs (ssDiffers). The keys go into a temporary table
// of the connection. A table where more than a quarter of the rows changed
// is copied whole.
func (s *SQLServer) findChanged(ctx context.Context, c *sql.Conn, plan *ssRestorePlan) error {
	n := 0
	for _, rt := range plan.tables {
		if !rt.delta {
			continue
		}
		n++
		k := fmt.Sprintf("#dg_k%d", n)
		w, p := rt.w.qname(), qms(s.o.Pristine)+"."+rt.p.qname()
		q, ok := ssChangedSQL(rt.p, w, p, k)
		if !ok {
			continue // copied whole: a copy does not need the encoding
		}
		var count int64
		err := c.QueryRowContext(ctx, q+ssParallel+";\nSELECT COUNT_BIG(*) FROM "+k).Scan(&count)
		if sqlServerNumber(err) == 10715 { // not a valid hint (older servers)
			k += "_"
			q, _ = ssChangedSQL(rt.p, w, p, k)
			err = c.QueryRowContext(ctx, q+";\nSELECT COUNT_BIG(*) FROM "+k).Scan(&count)
		}
		if err != nil {
			return err
		}
		if count*4 <= rt.p.Rows {
			rt.keys = k
		}
	}
	return nil
}

// ssChangedSQL is the batch that makes the temporary table k (it must not
// exist: the batch is compiled before it runs) and fills it with the keys
// of the rows in which table w differs from table p (both of definition
// t, which has a key), up to its last statement's OPTION; ok is false when
// a column has no encoding. The temporary table gets the key columns'
// types and collations without an IDENTITY property (UNION ALL).
func ssChangedSQL(t *ssTable, w, p, k string) (string, bool) {
	var keys, cols, on, pick, differ []string
	for i, col := range t.Key {
		name := qms(fmt.Sprintf("dg k%d", i+1))
		keys = append(keys, qms(col))
		cols = append(cols, qms(col)+" AS "+name)
		on = append(on, "w."+name+" = p."+name)
		pick = append(pick, "ISNULL(w."+name+", p."+name+")")
	}
	for _, col := range t.Columns {
		if !col.hashed() {
			continue
		}
		if col.Encrypted {
			return "", false
		}
		name := qms(fmt.Sprintf("dg c%d", len(differ)+1))
		v, d, err := ssDiffers(col, "w."+name, "p."+name)
		if err != nil {
			return "", false
		}
		cols = append(cols, v+" AS "+name)
		differ = append(differ, d)
	}
	first := "w." + qms("dg k1")
	list := strings.Join(keys, ", ")
	sel := strings.Join(cols, ", ")
	return fmt.Sprintf("SELECT TOP (0) %s INTO %s FROM %s UNION ALL SELECT TOP (0) %s FROM %s;\n", list, k, p, list, p) +
		fmt.Sprintf("INSERT INTO %s SELECT %s FROM (SELECT %s FROM %s) w FULL JOIN (SELECT %s FROM %s) p ON %s WHERE %s",
			k, strings.Join(pick, ", "), sel, w, sel, p, strings.Join(on, " AND "),
			strings.Join(append([]string{first + " IS NULL", "p." + qms("dg k1") + " IS NULL"}, differ...), " OR ")), true
}

// ssDiffers is how a delta compares a column (ssChangedSQL): the
// expression it reads the column as, and the condition under which the
// values a and b of that expression differ. Two values differ exactly
// when their encodings (sqlServerColumn) do, the way SQL Server compares
// each type fastest:
//
//   - integers, bit, money, uniqueidentifier and the date and time types
//     but datetimeoffset by their values: = is exact for them;
//   - float and real by their values, and a zero by its bytes too (0 and
//     -0 compare equal; a NaN cannot be stored);
//   - strings and binary strings, decimal and datetimeoffset as the bytes
//     of their encoding, with their lengths: a collation compares strings
//     equal that differ (case, trailing spaces), varbinary values that
//     differ in trailing zero bytes compare equal, decimal values with
//     another sign of zero and datetimeoffset values of the same instant
//     in another offset compare equal;
//   - any other type (xml, text, sql_variant, CLR types) by its encoded
//     part, which is never NULL.
//
// A NULL differs from any value.
func ssDiffers(c ssColumn, a, b string) (expr, differ string, err error) {
	null := ""
	if c.Nullable {
		null = " OR " + a + " IS NULL AND " + b + " IS NOT NULL OR " + a + " IS NOT NULL AND " + b + " IS NULL"
	}
	switch c.Type {
	case "bit", "tinyint", "smallint", "int", "bigint", "smallmoney", "money", "uniqueidentifier",
		"date", "time", "datetime2", "datetime", "smalldatetime":
		return c.expr(), "(" + a + " <> " + b + null + ")", nil
	case "real", "float":
		n := 8
		if c.Type == "real" {
			n = 4
		}
		return c.expr(), fmt.Sprintf("(%s <> %s OR %s = 0 AND CAST(%s AS binary(%d)) <> CAST(%s AS binary(%d))%s)", a, b, a, a, n, b, n, null), nil
	case "decimal", "numeric", "datetimeoffset", "char", "varchar", "nchar", "nvarchar", "binary", "varbinary":
		v, err := sqlServerValue(c)
		return v, "(" + a + " <> " + b + " OR DATALENGTH(" + a + ") <> DATALENGTH(" + b + ")" + null + ")", err
	}
	part, err := sqlServerColumn(c)
	return part, "(" + a + " <> " + b + ")", err
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

// EndedSessions is how many sessions of other programs in the working
// database the last Restore ended (state.SessionEnder): a restore of the
// whole database ends every one, which keepOut and the OFFLINE count
// before. Without VIEW SERVER STATE the login sees only its own sessions,
// and the count is 0.
func (s *SQLServer) EndedSessions() int { return len(s.ended) }

// ssSession is a session of the server: its id, which the server reuses,
// with its login time.
type ssSession struct {
	id    int
	login time.Time
}

// countSessions adds to ended the sessions of other programs in database
// id that are about to be ended, each once: a run of the restore that
// stopped before it ended them (another database's 927 or 952), and the
// count before OFFLINE after keepOut's, see them again. DriftGate's own
// there (just closed by closePool, they may still be logging out) are
// told by their program name and process id together: an application in
// the same process (a test) or another DriftGate is counted.
func (s *SQLServer) countSessions(ctx context.Context, m *sql.Conn, id int) {
	rows, err := m.QueryContext(ctx, `SELECT session_id, login_time FROM sys.dm_exec_sessions WHERE database_id = @p1 AND session_id <> @@SPID AND is_user_process = 1
		AND NOT (program_name = @p2 AND host_process_id = @p3)`, id, s.cfg.AppName, os.Getpid())
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var x ssSession
		if rows.Scan(&x.id, &x.login) != nil {
			return
		}
		if s.ended == nil {
			s.ended = map[ssSession]bool{}
		}
		s.ended[x] = true
	}
}
