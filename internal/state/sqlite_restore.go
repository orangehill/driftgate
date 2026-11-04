package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/orangehill/driftgate/internal/chaos"
)

// Restore brings the given items of the working file back to the pristine
// file, inside the working file and in one transaction (BEGIN IMMEDIATE:
// it waits for a write of the application's to end, and the application
// sees all of the restore or none of it). The pristine file is attached
// read-only. nil means every item that differs.
//
// A table with the pristine definition (CREATE statement and indexes) is
// emptied and filled again with INSERT … SELECT *, which SQLite copies as
// raw records (its transfer optimization) when the table's rowid is an
// INTEGER PRIMARY KEY or it has none; a table whose rowid is hidden is
// copied with its rowids named, so they are kept too. Any other table is
// dropped and created from the pristine file's statements. Its
// sqlite_sequence row is set to the pristine one.
//
// Nothing the restore does may fire the application's logic: foreign keys
// are off on DriftGate's connections, so deletes do not cascade, and the
// triggers of the tables written are dropped before and created again
// after, from the pristine file, inside the transaction. (SQLite can also
// turn triggers off for a connection, SQLITE_DBCONFIG_ENABLE_TRIGGER, but
// the driver does not expose sqlite3_db_config, and reaching its
// connection handle would take its unexported fields. Dropping a table's
// triggers keeps the transfer and truncate optimizations as well, and a
// failed restore rolls the drop back with everything else.)
//
// Excluded tables, SQLite's statistics and the pristine mark are never
// touched.
func (s *SQLite) Restore(ctx context.Context, items []string) (err error) {
	defer s.busy(&err)
	if items != nil && len(items) == 0 {
		return nil
	}
	if err := s.current(); err != nil {
		return err
	}
	if _, err := os.Stat(s.o.Pristine); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state %s: pristine file %s does not exist; run driftgate seed", s.o.Name, s.o.Pristine)
	} else if err != nil {
		return err
	}
	if items == nil {
		w, err := s.Fingerprint(ctx, nil)
		if err != nil {
			return err
		}
		p, err := s.pristineFingerprint(ctx)
		if err != nil {
			return err
		}
		if items = Diff(w, p); len(items) == 0 {
			return nil
		}
	}
	if s.seen.ok {
		dv, err := s.dataVersion(ctx)
		if err != nil {
			return err
		}
		if s.unseen(dv, s.stampNow()) {
			// Written through the old pages, the file would be corrupt.
			if err := s.reopen(); err != nil {
				return err
			}
		}
	}
	uri, err := sqliteURI(s.o.Pristine, "mode=ro")
	if err != nil {
		return err
	}
	c := s.main
	if _, err := c.ExecContext(ctx, "ATTACH DATABASE ? AS "+sqlitePristine, uri); err != nil {
		return fmt.Errorf("state %s: pristine file %s: %v", s.o.Name, s.o.Pristine, err)
	}
	defer c.ExecContext(context.Background(), "DETACH DATABASE "+sqlitePristine)
	// The last fingerprint stays, with the items the restore writes
	// listed: the next fingerprint reads those again and keeps the rest,
	// when no other connection committed since the last one was read
	// (main's data_version, which the restore's own commit does not move).
	// When the restore fails, the next fingerprint reads everything.
	last, restored := s.last, s.restored
	s.forget()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			c.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	dv, err := s.dataVersion(ctx)
	if err != nil {
		return err
	}
	written, err := s.restore(ctx, c, items)
	if err != nil {
		return err
	}
	chaos.Point("state.sqlite.restore")
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	s.seen = sqliteSeen{dv: dv, stamp: s.stampNow(), ok: true}
	if last == nil {
		return nil
	}
	// Read after the commit: a commit of another connection in between
	// moves data_version, and the next fingerprint reads every item.
	var sv int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.schema_version").Scan(&sv); err != nil {
		return nil // the next fingerprint reads every item
	}
	if restored == nil {
		restored = map[string]bool{}
	}
	for _, it := range items {
		restored[it] = true
	}
	for _, t := range written {
		restored["table:"+t] = true
	}
	s.last, s.restored, s.sv = last, restored, sv
	return nil
}

// restore copies the items from the attached pristine file into main, in
// the transaction of c, and returns the tables it dropped, created or
// filled.
func (s *SQLite) restore(ctx context.Context, c *sql.Conn, items []string) ([]string, error) {
	work, err := s.objects(ctx, c, "main")
	if err != nil {
		return nil, err
	}
	pris, err := s.objects(ctx, c, sqlitePristine)
	if err != nil {
		return nil, err
	}
	tableItems, viewItems, _, triggerItems := splitItems(items)

	// What to do with each table: drop it, create it from the pristine
	// file, copy its rows. A view that becomes a table or a table that
	// becomes a view is dropped and created as the other.
	inTables := map[string]bool{}
	var tables []string
	for _, t := range tableItems {
		if !inTables[t] && (work.tables[t] != nil || pris.tables[t] != nil || work.views[t] != "" || pris.views[t] != "") {
			inTables[t] = true
			tables = append(tables, t)
		}
	}
	virtualFirst(tables, work, pris)
	dropViews, createViews := set(viewItems), set(viewItems)
	var drop, create, fill []string
	for _, t := range tables {
		w, p := work.tables[t], pris.tables[t]
		if work.views[t] != "" {
			dropViews[t] = true
		}
		if pris.views[t] != "" {
			createViews[t] = true
		}
		switch {
		case p == nil:
			if w != nil {
				drop = append(drop, t)
			}
		case w == nil || w.def() != p.def():
			if w != nil {
				drop = append(drop, t)
			}
			create = append(create, t)
			if p.virtual {
				// Creating it makes its shadow tables anew: their rows
				// come from the pristine file too.
				for _, name := range pris.order {
					if sh := pris.tables[name]; sh.shadow && strings.HasPrefix(name, t+"_") {
						fill = append(fill, name)
					}
				}
			} else {
				fill = append(fill, t)
			}
		case !p.virtual:
			fill = append(fill, t)
		}
	}
	fill = unique(fill)

	// Triggers: of every table or view written or replaced, and the ones
	// named. They are dropped first, so none fires, and the pristine
	// file's are created at the end.
	// Keyed by the folded name: a trigger may name its table in another
	// case, and a table renamed only in case is two items.
	trigTables := map[string]bool{}
	for _, t := range append(append([]string{}, tables...), fill...) {
		trigTables[asciiLower(t)] = true
	}
	for v := range dropViews {
		trigTables[asciiLower(v)] = true
	}
	named := set(triggerItems)
	for _, name := range work.torder {
		if tr := work.triggers[name]; trigTables[asciiLower(tr.table)] || named[name] {
			if _, err := c.ExecContext(ctx, "DROP TRIGGER IF EXISTS main."+qs(name)); err != nil {
				return nil, fmt.Errorf("trigger %s: %w", name, err)
			}
		}
	}
	for _, v := range work.vorder {
		if dropViews[v] {
			if _, err := c.ExecContext(ctx, "DROP VIEW IF EXISTS main."+qs(v)); err != nil {
				return nil, fmt.Errorf("view %s: %w", v, err)
			}
		}
	}
	for _, t := range drop {
		if _, err := c.ExecContext(ctx, "DROP TABLE IF EXISTS main."+qs(t)); err != nil {
			return nil, tableErr(t, work.tables[t], err)
		}
	}
	// Dropping a virtual table drops its shadow tables too: they are
	// written as well.
	for _, t := range drop {
		if w := work.tables[t]; w != nil && w.virtual {
			for _, name := range work.order {
				if sh := work.tables[name]; sh.shadow && strings.HasPrefix(name, t+"_") {
					drop = append(drop, name)
				}
			}
		}
	}
	for _, t := range create {
		p := pris.tables[t]
		if !p.virtual {
			// A shadow table its virtual table made just now is left as made.
			exists, err := sqliteExists(ctx, c, t)
			if err != nil {
				return nil, err
			}
			if exists {
				continue
			}
		}
		if _, err := c.ExecContext(ctx, p.sql); err != nil {
			return nil, tableErr(t, p, err)
		}
		for _, ix := range p.indexes {
			if _, err := c.ExecContext(ctx, ix); err != nil {
				return nil, fmt.Errorf("table %s: index: %w", t, err)
			}
		}
	}
	shadows := false
	for _, t := range fill {
		if err := copyRows(ctx, c, pris.tables[t]); err != nil {
			return nil, fmt.Errorf("table %s: %w", t, err)
		}
		shadows = shadows || pris.tables[t].shadow
	}
	if shadows {
		if err := newSchemaVersion(ctx, c); err != nil {
			return nil, err
		}
	}
	// INSERT … SELECT leaves sqlite_sequence at the working file's value.
	hasSeq, err := sqliteExists(ctx, c, "sqlite_sequence")
	if err != nil {
		return nil, err
	}
	if hasSeq {
		for _, t := range unique(append(append([]string{}, fill...), create...)) {
			if _, err := c.ExecContext(ctx, "DELETE FROM main.sqlite_sequence WHERE name = ?", t); err != nil {
				return nil, err
			}
			if seq, ok := pris.seq[t]; ok {
				if _, err := c.ExecContext(ctx, "INSERT INTO main.sqlite_sequence (name, seq) VALUES (?, ?)", t, seq); err != nil {
					return nil, err
				}
			}
		}
	}
	// Views may use views made later: SQLite resolves their names when
	// they are used, not when they are made.
	for _, v := range pris.vorder {
		if createViews[v] {
			if _, err := c.ExecContext(ctx, pris.views[v]); err != nil {
				return nil, fmt.Errorf("view %s: %w", v, err)
			}
		}
	}
	for _, name := range pris.torder {
		tr := pris.triggers[name]
		if !trigTables[asciiLower(tr.table)] && !named[name] {
			continue
		}
		exists, err := sqliteExists(ctx, c, tr.table)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue // its table is not restored: the trigger item stays different
		}
		if _, err := c.ExecContext(ctx, tr.sql); err != nil {
			return nil, fmt.Errorf("trigger %s: %w", name, err)
		}
	}
	for _, it := range items {
		p, ok := strings.CutPrefix(it, "pragma:")
		if !ok || p != "user_version" && p != "application_id" {
			continue
		}
		var v int64
		if err := c.QueryRowContext(ctx, "PRAGMA "+sqlitePristine+"."+p).Scan(&v); err != nil {
			return nil, err
		}
		if _, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA main.%s = %d", p, v)); err != nil {
			return nil, err
		}
	}
	return unique(append(append(append([]string{}, drop...), create...), fill...)), nil
}

// newSchemaVersion moves main's schema_version on by one, as a CREATE or
// DROP does, so every connection to the file reads the schema again and
// connects its virtual tables anew. A virtual table's module keeps what it
// read of its shadow tables on each connection, and how it tells that
// they changed need not hold for a restore, which writes earlier rows
// back. FTS5 reads a table's configuration (its %_config rows: the rank
// function, secure-delete, the merge settings) again only when the cookie
// in its structure record changes, and then sets only the settings that
// have a row: a connection keeps a rank function the application set
// after the seed when the restore deletes its row, and keeps any setting
// when the restored record carries a cookie the connection saw with other
// rows (the configuration changed once after the restore as before it).
// Its matches would be ranked by the configuration of before. (The
// structure record itself is read again when PRAGMA data_version moves,
// which the restore's commit does for every other connection, though not
// by PRAGMA integrity_check, which checks the rows against the record the
// connection read last and finds them corrupt.)
//
// The schema_version is not part of a fingerprint, so a file restored this
// way is as like the pristine file as one restored without.
func newSchemaVersion(ctx context.Context, c *sql.Conn) error {
	var v int64
	if err := c.QueryRowContext(ctx, "PRAGMA main.schema_version").Scan(&v); err != nil {
		return err
	}
	// It is a 32-bit field, and wraps as SQLite's own change does.
	_, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA main.schema_version = %d", int32(v)+1))
	return err
}

// copyRows empties a table of main and copies the pristine file's rows
// into it.
func copyRows(ctx context.Context, c *sql.Conn, t *sqliteTable) error {
	cols, ok, err := readColumns(ctx, c, sqlitePristine, t.name)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no such table in the pristine file")
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM main."+qs(t.name)); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, copySQL(t, cols))
	return err
}

// copySQL is the statement that copies a table's rows from the pristine
// file: INSERT … SELECT * where SQLite can copy the records as they are
// (the rowid is a column, or there is none, and no column is generated),
// else with the columns that hold data named, the rowid first when it is
// hidden.
func copySQL(t *sqliteTable, cols *sqliteColumns) string {
	dst, src := "main."+qs(t.name), qs(sqlitePristine)+"."+qs(t.name)
	hidden := !t.noRowid && cols.ipk == ""
	if !hidden && !cols.generated {
		return "INSERT INTO " + dst + " SELECT * FROM " + src
	}
	var list []string
	if hidden && cols.rowid != "" {
		list = append(list, cols.rowid)
	}
	for _, col := range cols.stored {
		list = append(list, qs(col))
	}
	names := strings.Join(list, ", ")
	q := "INSERT INTO " + dst + " (" + names + ") SELECT " + names + " FROM " + src
	if hidden && cols.rowid == "" {
		q += " NOT INDEXED" // every rowid name is a column: copied in rowid order
	}
	return q
}

// sqliteExists reports whether main has a table or view of that name (in
// any case, as SQLite looks names up).
func sqliteExists(ctx context.Context, c *sql.Conn, name string) (bool, error) {
	var n int
	err := c.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_schema WHERE type IN ('table', 'view') AND name = ? COLLATE NOCASE", name).Scan(&n)
	return n > 0, err
}

// tableErr names the table of a failed statement, and the missing module
// of a virtual table.
func tableErr(name string, t *sqliteTable, err error) error {
	if (t == nil || t.virtual) && strings.Contains(err.Error(), "no such module") {
		return fmt.Errorf("virtual table %s: %w (DriftGate's SQLite does not have this module; add the table to exclude)", name, err)
	}
	return fmt.Errorf("table %s: %w", name, err)
}

// virtualFirst puts virtual tables first, so their shadow tables are made
// before the others are looked at.
func virtualFirst(tables []string, work, pris *sqliteObjects) {
	isVirtual := func(t string) bool {
		return work.tables[t] != nil && work.tables[t].virtual || pris.tables[t] != nil && pris.tables[t].virtual
	}
	var v, other []string
	for _, t := range tables {
		if isVirtual(t) {
			v = append(v, t)
		} else {
			other = append(other, t)
		}
	}
	copy(tables, append(v, other...))
}

func set(list []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range list {
		m[s] = true
	}
	return m
}

func unique(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
