package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Rows reads the rows of the tracked tables among items (nil: every
// tracked table): per row, its primary key as column=value pairs and a
// hash of every column's value, encoded as the fingerprint encodes it
// (sqlServerValue; a rowversion too: an UPDATE that changes nothing else
// still gives a row a new one). A table in row_tracking is schema.name, or
// a name in dbo.
func (s *SQLServer) Rows(ctx context.Context, items []string) (Rows, error) {
	if _, err := s.server(ctx); err != nil {
		return nil, err
	}
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	tables, _, _, _ := splitItems(items)
	if items == nil {
		for _, t := range s.o.RowTracking {
			if !strings.Contains(t, ".") {
				t = "dbo." + t
			}
			tables = append(tables, t)
		}
	}
	out := Rows{}
	for _, t := range tables {
		schema, name, _ := strings.Cut(t, ".")
		if !s.tracked(schema, name) {
			continue
		}
		r, err := s.rowsOf(ctx, c, schema, name)
		if err != nil {
			return nil, err
		}
		out[t] = r
	}
	return out, nil
}

// rowsOf reads the rows of one table (Rows).
func (s *SQLServer) rowsOf(ctx context.Context, c *sql.Conn, schema, name string) (map[string]map[string]string, error) {
	rows, err := c.QueryContext(ctx, `SELECT c.name, CASE WHEN ut.is_assembly_type = 1 THEN ut.name ELSE ISNULL(st.name, ut.name) END, ut.is_assembly_type,
		c.max_length, c.is_column_set, CASE WHEN c.encryption_type IS NULL THEN 0 ELSE 1 END, ISNULL(c.graph_type, 0), OBJECTPROPERTY(c.object_id, 'TableIsEdge'),
		ISNULL((SELECT ic.key_ordinal FROM sys.indexes i JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		         WHERE i.object_id = c.object_id AND i.is_primary_key = 1 AND ic.column_id = c.column_id), 0)
		FROM sys.columns c JOIN sys.types ut ON ut.user_type_id = c.user_type_id LEFT JOIN sys.types st ON st.user_type_id = c.system_type_id
		WHERE c.object_id = OBJECT_ID(@p1) ORDER BY c.column_id`, qs2(schema, name))
	if err != nil {
		return nil, err
	}
	var cols []ssColumn
	key := map[int]ssColumn{}
	for rows.Next() {
		var col ssColumn
		var ordinal int
		var edge sql.NullInt64
		if err := rows.Scan(&col.Name, &col.Type, &col.CLR, &col.MaxLength, &col.ColumnSet, &col.Encrypted, &col.Graph, &edge, &ordinal); err != nil {
			rows.Close()
			return nil, err
		}
		col.Edge = edge.Int64 == 1
		if col.ColumnSet || col.Encrypted || col.internal() {
			continue // the sparse columns are read one by one; encrypted values and a graph's internal ids are not readable
		}
		cols = append(cols, col)
		if ordinal > 0 {
			key[ordinal] = col
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("state %s: row_tracking table %s.%s does not exist", s.o.Name, schema, name)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("state %s: row_tracking table %s.%s has no primary key", s.o.Name, schema, name)
	}
	var secured int
	if err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.security_predicates pr JOIN sys.security_policies p ON p.object_id = pr.object_id
		WHERE pr.target_object_id = OBJECT_ID(@p1) AND p.is_enabled = 1`, qs2(schema, name)).Scan(&secured); err != nil {
		return nil, err
	}
	if secured > 0 {
		return nil, fmt.Errorf("state %s: row_tracking table %s.%s has row-level security, which hides rows from DriftGate's reads; turn the policy off in this database or stop tracking the table", s.o.Name, schema, name)
	}
	var sel []string
	for i := 1; i <= len(key); i++ {
		sel = append(sel, sqlServerKeyText(key[i]))
	}
	for _, col := range cols {
		part, err := sqlServerColumn(col)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", schema, name, err)
		}
		sel = append(sel, part)
	}
	rows, err = c.QueryContext(ctx, "SELECT "+strings.Join(sel, ", ")+" FROM "+qs2(schema, name))
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", schema, name, err)
	}
	defer rows.Close()
	vals := make([]sql.RawBytes, len(sel))
	ptrs := make([]any, len(sel))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	out := map[string]map[string]string{}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		var k []string
		for i := 1; i <= len(key); i++ {
			k = append(k, key[i].Name+"="+string(vals[i-1]))
		}
		row := map[string]string{}
		for i, col := range cols {
			row[col.Name] = hash(vals[len(key)+i])[:16]
		}
		out[strings.Join(k, ",")] = row
	}
	return out, rows.Err()
}

// sqlServerKeyText is the expression of a key column's value as text:
// binary as 0x…, dates and times in ISO 8601, floats exactly.
func sqlServerKeyText(c ssColumn) string {
	q := qms(c.Name)
	switch c.Type {
	case "binary", "varbinary", "timestamp":
		return "CONVERT(varchar(max), " + q + ", 1)"
	case "date", "time", "datetime2", "datetimeoffset", "datetime", "smalldatetime":
		return "CONVERT(nvarchar(64), " + q + ", 126)"
	case "float", "real":
		return "CONVERT(nvarchar(64), " + q + ", 3)"
	}
	return "CONVERT(nvarchar(max), " + q + ")"
}
