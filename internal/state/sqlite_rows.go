package state

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Rows reads the rows of the tracked tables among items (nil: every
// tracked table), all at one moment. A row's key is its primary key, or
// rowid=N for a table without one.
func (s *SQLite) Rows(ctx context.Context, items []string) (Rows, error) {
	if err := s.current(); err != nil {
		return nil, err
	}
	tables, _, _, _ := splitItems(items)
	if items == nil {
		tables = s.o.RowTracking
	}
	out := Rows{}
	var tracked []string
	for _, t := range tables {
		if s.o.tracked(t) {
			tracked = append(tracked, t)
		}
	}
	if len(tracked) == 0 {
		return out, nil
	}
	c, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := sqliteBegin(ctx, c); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.Background(), "ROLLBACK")
	for _, t := range tracked {
		cols, ok, err := readColumns(ctx, c, "main", t)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // not in the file (now): no rows
		}
		key := cols.pk
		sel := make([]string, 0, len(cols.stored)+1)
		names := make([]string, 0, len(cols.stored)+1)
		if len(key) == 0 {
			if cols.rowid == "" {
				return nil, fmt.Errorf("state %s: row_tracking table %s has no primary key, and its columns hide the rowid", s.o.Name, t)
			}
			key = []string{"rowid"}
			sel, names = append(sel, cols.rowid), append(names, "rowid")
		}
		for _, col := range cols.stored {
			sel, names = append(sel, "+"+qs(col)), append(names, col)
		}
		r, err := sqliteReadRows(ctx, c, "SELECT "+strings.Join(sel, ", ")+" FROM main."+qs(t), names, key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		out[t] = r
	}
	return out, nil
}

// sqliteReadRows reads every row of a query into key → column → value
// hash. A value is hashed with its storage class (appendValue); in the key
// it is written as text: a number in decimal, a text as it is, a blob in
// hex (x'…'), NULL as \x00NULL.
func sqliteReadRows(ctx context.Context, c queryer, query string, cols, pk []string) (map[string]map[string]string, error) {
	rows, err := c.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	out := map[string]map[string]string{}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := map[string]string{}
		byName := map[string]any{}
		for i, col := range cols {
			byName[col] = vals[i]
			row[col] = hash(appendValue(nil, vals[i]))[:16]
		}
		key := make([]string, 0, len(pk))
		for _, k := range pk {
			key = append(key, k+"="+keyText(byName[k]))
		}
		out[strings.Join(key, ",")] = row
	}
	return out, rows.Err()
}

func keyText(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	case []byte:
		return "x'" + hex.EncodeToString(x) + "'"
	}
	return fmt.Sprint(v)
}
