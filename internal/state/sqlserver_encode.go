package state

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// A row of a table is encoded on the server, as bytes, and hashed by
// DriftGate (crypto/sha256): every column in order, NULL as 0x00 and any
// other value as 0x01, its length (4 bytes) and its bytes, so no two rows
// encode alike — ('ab', 'c') and ('a', 'bc'), NULL and '', 'a' and 'a '
// differ. The bytes are the value's own, not a text of it: a float keeps
// -0 and its last bit, a datetime2 its precision, a string its exact
// characters (case and trailing spaces too, whatever the collation
// compares equal). CHECKSUM and BINARY_CHECKSUM are not used: 32 bits,
// equal for strings the collation compares equal, and blind to some types.
//
// The encoding starts with a varbinary(max) operand: SQL Server cuts the
// result of varbinary(n) + varbinary(n) at 8000 bytes without an error,
// which would hash only the start of a long row. A value that may reach
// 8000 bytes starts its own part with one too. A row whose parts together
// cannot pass 8000 bytes (every column of a type with a bound, the sum of
// the bounds with 5 bytes per part at most 8000) is the same bytes as a
// varbinary(n): SQL Server reads and sends such a value faster than a
// varbinary(max).

// ssColumn is a column of a table as the encoding needs it.
type ssColumn struct {
	ID        int
	Name      string
	Type      string // the system type: an alias type's base type, a CLR type's own name
	CLR       bool   // a CLR type (hierarchyid, geometry, geography, or a user's)
	MaxLength int    // in bytes; -1 for (max)
	Nullable  bool
	Identity  bool
	Computed  bool
	ColumnSet bool // the XML column of a table's sparse columns
	Encrypted bool // Always Encrypted
	Generated int  // GENERATED ALWAYS (1, 2: a temporal table's period; 5–8: ledger)
	// Graph is sys.columns.graph_type of a node or edge table's own
	// columns: 1, 3, 4, 6 and 7 are internal (they cannot be read), 2, 5
	// and 8 their readable form, $node_id or $edge_id, $from_id, $to_id.
	Graph int
	Edge  bool // of an edge table (graph type 2 is $edge_id)
}

// expr is the expression that reads the column: its quoted name, or a
// graph table's pseudo-column.
func (c ssColumn) expr() string {
	switch c.Graph {
	case 2:
		if c.Edge {
			return "$edge_id"
		}
		return "$node_id"
	case 5:
		return "$from_id"
	case 8:
		return "$to_id"
	}
	return qms(c.Name)
}

// internal reports whether the column cannot be read (a graph table's
// internal ids, read through their pseudo-columns instead).
func (c ssColumn) internal() bool {
	switch c.Graph {
	case 1, 3, 4, 6, 7:
		return true
	}
	return false
}

// rowversion reports whether the column is a rowversion (timestamp).
func (c ssColumn) rowversion() bool { return c.Type == "timestamp" }

// hashed reports whether the column's values are part of the table's
// content: computed columns follow from the others (their definition is
// part of the table's), a column set from the sparse columns, and a
// rowversion is new for every row a restore writes (see rowversionMoved).
// A graph table's ids are read through their pseudo-columns.
func (c ssColumn) hashed() bool {
	if c.Graph != 0 {
		return !c.internal()
	}
	return !c.Computed && !c.ColumnSet && !c.rowversion()
}

// sqlServerValue is the expression of a column's value as varbinary, or an
// error for a type DriftGate cannot read as bytes.
func sqlServerValue(c ssColumn) (string, error) {
	q := c.expr()
	fixed := func(n int) string { return fmt.Sprintf("CAST(%s AS binary(%d))", q, n) }
	switch c.Type {
	case "bit", "tinyint":
		return fixed(1), nil
	case "smallint":
		return fixed(2), nil
	case "int", "smallmoney", "real":
		return fixed(4), nil
	case "bigint", "money", "float", "timestamp":
		return fixed(8), nil
	case "uniqueidentifier":
		return fixed(16), nil
	case "decimal", "numeric":
		return "CAST(" + q + " AS varbinary(64))", nil // precision, scale, sign and digits
	case "date", "time", "datetime2", "datetimeoffset", "datetime", "smalldatetime":
		return "CAST(" + q + " AS varbinary(16))", nil // time, datetime2 and datetimeoffset with their precision
	case "char", "varchar", "nchar", "nvarchar", "binary", "varbinary":
		// Strings as the bytes they are stored as (the column's code page,
		// UTF-8 or UTF-16).
		if c.MaxLength < 0 {
			return "CAST(" + q + " AS varbinary(max))", nil
		}
		return fmt.Sprintf("CAST(%s AS varbinary(%d))", q, max(c.MaxLength, 1)), nil
	case "xml", "json":
		// Stored in a binary form of their own; as text they read back the
		// same for the same value.
		return "CAST(CAST(" + q + " AS nvarchar(max)) AS varbinary(max))", nil
	case "text":
		return "CAST(CAST(" + q + " AS varchar(max)) AS varbinary(max))", nil
	case "ntext":
		return "CAST(CAST(" + q + " AS nvarchar(max)) AS varbinary(max))", nil
	case "image":
		return "CAST(" + q + " AS varbinary(max))", nil
	case "sql_variant":
		// The value's type and its properties too: an int 1 and a bigint 1
		// differ only in length, an nvarchar '1' and a varchar '1' in their
		// bytes, a varchar in its collation and declared length. The value
		// takes up to 8000 bytes: its part starts with a varbinary(max)
		// operand (see above).
		prop := func(p, as string) string {
			return sqlServerPart("CAST(CAST(SQL_VARIANT_PROPERTY("+q+", '"+p+"') AS "+as+") AS varbinary(256))", false)
		}
		inner := prop("BaseType", "nvarchar(128)") + " + " + prop("Precision", "int") + " + " + prop("Scale", "int") + " + " +
			prop("MaxLength", "int") + " + " + prop("Collation", "nvarchar(128)") + " + " + sqlServerPart("CAST("+q+" AS varbinary(max))", true)
		return "CASE WHEN " + q + " IS NULL THEN NULL ELSE CAST(0x AS varbinary(max)) + " + inner + " END", nil
	}
	if c.CLR { // hierarchyid, geometry, geography: their serialised form
		return "CAST(" + q + " AS varbinary(max))", nil
	}
	return "", fmt.Errorf("column %s has the type %s, which DriftGate cannot read", c.Name, c.Type)
}

// long reports whether a column's encoded value may pass 8000 bytes.
func (c ssColumn) long() bool {
	switch c.Type {
	case "char", "varchar", "nchar", "nvarchar", "binary", "varbinary":
		return c.MaxLength < 0 || c.MaxLength > 7990
	case "xml", "json", "text", "ntext", "image", "sql_variant":
		return true
	}
	return c.CLR // the other types are a few bytes
}

// bound is the most bytes a column's value (sqlServerValue) takes, from
// the type of the expression; 0 when the value has no such bound below
// 8000 bytes.
func (c ssColumn) bound() int {
	switch c.Type {
	case "bit", "tinyint":
		return 1
	case "smallint":
		return 2
	case "int", "smallmoney", "real":
		return 4
	case "bigint", "money", "float", "timestamp":
		return 8
	case "uniqueidentifier", "date", "time", "datetime2", "datetimeoffset", "datetime", "smalldatetime":
		return 16
	case "decimal", "numeric":
		return 64
	case "char", "varchar", "nchar", "nvarchar", "binary", "varbinary":
		if c.MaxLength >= 0 && !c.long() {
			return max(c.MaxLength, 1)
		}
	}
	return 0
}

// sqlServerPart encodes one value v (varbinary): 0x00 for NULL, else 0x01,
// the length in 4 bytes and the bytes. long starts it with a
// varbinary(max) operand (see above).
func sqlServerPart(v string, long bool) string {
	head := "0x01"
	if long {
		head = "CAST(0x01 AS varbinary(max))"
	}
	return "ISNULL(" + head + " + CAST(DATALENGTH(" + v + ") AS binary(4)) + " + v + ", 0x00)"
}

// sqlServerColumn is the encoded part of one column.
func sqlServerColumn(c ssColumn) (string, error) {
	v, err := sqlServerValue(c)
	if err != nil {
		return "", err
	}
	return sqlServerPart(v, c.long()), nil
}

// sqlServerRow is the expression of a row's encoding: its hashed columns
// (ssColumn.hashed) in the order of the table.
func sqlServerRow(cols []ssColumn) (string, error) {
	parts, bound, err := sqlServerParts(cols)
	if err != nil {
		return "", err
	}
	if bound > 0 && bound <= 8000 {
		return "CAST(" + strings.Join(parts, "\n + ") + fmt.Sprintf(" AS varbinary(%d))", bound), nil
	}
	return strings.Join(append([]string{"CAST(0x AS varbinary(max))"}, parts...), "\n + "), nil
}

// sqlServerParts are the encoded parts of a row's hashed columns and the
// most bytes they take together, 0 when a column has no bound.
func sqlServerParts(cols []ssColumn) (parts []string, bound int, err error) {
	bounded := true
	for _, c := range cols {
		if !c.hashed() {
			continue
		}
		if c.Encrypted {
			return nil, 0, fmt.Errorf("column %s is encrypted with Always Encrypted, which DriftGate does not support", c.Name)
		}
		p, err := sqlServerColumn(c)
		if err != nil {
			return nil, 0, err
		}
		parts = append(parts, p)
		b := c.bound()
		bounded = bounded && b > 0
		bound += 5 + b // 0x01 (0x00 for NULL), the length and the value
	}
	if !bounded {
		bound = 0
	}
	return parts, bound, nil
}

// ssSum hashes a multiset of rows: the count and, of every row's SHA-256,
// the eight 32-bit lanes (signed, big-endian) summed exactly. The order of
// the rows does not matter, so tables without a key (heaps, duplicates
// too) and tables read in parts by several connections hash the same.
type ssSum struct {
	n     int64
	lanes [8]int64
}

func (s *ssSum) add(row []byte) {
	h := sha256.Sum256(row)
	for i := range s.lanes {
		s.lanes[i] += int64(int32(binary.BigEndian.Uint32(h[i*4:])))
	}
	s.n++
}

func (s *ssSum) merge(o ssSum) {
	for i := range s.lanes {
		s.lanes[i] += o.lanes[i]
	}
	s.n += o.n
}

func (s ssSum) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", s.n)
	for _, l := range s.lanes {
		fmt.Fprintf(&b, " %d", l)
	}
	return b.String()
}
