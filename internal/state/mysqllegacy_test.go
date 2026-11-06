package state

// The fingerprint code of the release before SHOW CREATE was batched,
// copied as it was (only renamed): the tests compare the fingerprint the
// current code reads with the one this reads, item by item.

import (
	"context"
	"database/sql"
	"fmt"
)

func (m *MySQL) legacyFingerprintAs(ctx context.Context, c *sql.Conn, db string, items []string, format int) (Fingerprint, error) {
	objs, err := m.objects(ctx, c, db)
	if err != nil {
		return nil, err
	}
	want := func(item string) bool { return items == nil || contains(items, item) }
	f := Fingerprint{}
	if err := m.legacyHashTables(ctx, c, db, objs.tables, want, f); err != nil {
		return nil, err
	}
	for _, v := range objs.views {
		if !want("view:" + v) {
			continue
		}
		r, err := show(ctx, c, "VIEW", db, v)
		if err != nil {
			return nil, err
		}
		if format >= 2 {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db) + "\x00" + r["collation_connection"]))
		} else {
			f["view:"+v] = hash([]byte(localView(r["Create View"], db)))
		}
	}
	for _, s := range objs.sequences {
		if !want("sequence:" + s) {
			continue
		}
		def, err := showCreate(ctx, c, "SEQUENCE", db, s)
		if err != nil {
			return nil, err
		}
		next, err := sequenceNext(ctx, c, db, s)
		if err != nil {
			return nil, err
		}
		f["sequence:"+s] = hash([]byte(def + "\x00" + next))
	}
	for _, tr := range objs.triggers {
		if !want("trigger:" + tr.Name) {
			continue
		}
		r, err := show(ctx, c, "TRIGGER", db, tr.Name)
		if err != nil {
			return nil, err
		}
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
func (m *MySQL) legacyHashTables(ctx context.Context, c *sql.Conn, db string, names []string, want func(string) bool, f Fingerprint) error {
	var tables []string
	creates := map[string]string{}
	for _, t := range names {
		if !want("table:" + t) {
			continue
		}
		def, err := showCreate(ctx, c, "TABLE", db, t)
		if err != nil {
			return err
		}
		tables = append(tables, t)
		creates[t] = def
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
