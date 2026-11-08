package data

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/dbx"
	"github.com/orangehill/driftgate/internal/lazy"
)

// mysqlDialect reads and writes MySQL and MariaDB. Rows are read with the
// text protocol, so every value arrives as the text the server prints, and
// written back as literals of the same text.
type mysqlDialect struct{}

// mysqlSession is the same on the source, the target and at the top of a
// dump: UTF-8, UTC for TIMESTAMP columns, no checks of references and
// uniqueness while rows are loaded (they were valid in the source), a zero
// in an AUTO_INCREMENT column stays zero, and a value that does not fit its
// column (a stand-in longer than the original) is an error, not a silent cut.
var mysqlSession = dbx.MySQLSession{UTF8UTC: true, SQLMode: mysqlMode}

const mysqlMode = "NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES"

func (mysqlDialect) quote(name string) string { return dbx.QuoteMySQL(name) }

func (d mysqlDialect) session(ctx context.Context, c *sql.Conn) error {
	return mysqlSession.Apply(ctx, c)
}

func (mysqlDialect) snapshot(ctx context.Context, c *sql.Conn) error {
	if _, err := c.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY")
	return err
}

var definer = lazy.Regexp("\\s*DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|[^\\s@]+)@(`[^`]*`|'[^']*'|\\S+)")

// mysqlCategory sorts a column by its DATA_TYPE.
func mysqlCategory(dataType string) (cat string, hexLit bool) {
	switch strings.ToLower(dataType) {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "uuid":
		return CatText, false
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real":
		return CatNumeric, false
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return CatBinary, true
	case "json":
		return CatJSON, false
	case "bit", "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "geomcollection":
		return CatOther, true
	}
	return CatOther, false
}

func (d mysqlDialect) schema(ctx context.Context, c *sql.Conn, db string) (*Schema, error) {
	s := &Schema{Driver: MySQL}
	for _, q := range mysqlSession.Statements() {
		s.header = append(s.header, Stmt{SQL: q})
	}
	objs, err := dbx.MySQLObjects(ctx, c, db)
	if err != nil {
		return nil, err
	}
	// A probe DriftGate's state module left behind is no object to copy.
	objs = slices.DeleteFunc(objs, func(o dbx.MySQLObject) bool { return dbx.MySQLProbe(o.Name) })
	if len(objs) == 0 {
		ok, err := dbx.MySQLDatabaseExists(ctx, c, db)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("database %s does not exist", db)
		}
	}
	byName := map[string]*Table{}
	var views, sequences []string
	for _, o := range objs {
		name, typ := o.Name, o.Type
		switch typ {
		case "VIEW":
			views = append(views, name)
		case "SEQUENCE":
			sequences = append(sequences, name)
		case "SYSTEM VERSIONED":
			return nil, fmt.Errorf("%w: %s is a system-versioned table, whose history cannot be copied", ErrUnsupported, name)
		default:
			if name == markerTable {
				continue // DriftGate's mark of a target: never read or copied
			}
			t := &Table{Name: name, quoted: d.quote(name)}
			s.Tables = append(s.Tables, t)
			byName[name] = t
		}
	}

	cols, err := queryStrings(ctx, c, `SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, EXTRA, COLLATION_NAME
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION`, db)
	if err != nil {
		return nil, err
	}
	for _, r := range cols {
		t := byName[r[0]]
		if t == nil {
			continue // a view
		}
		cat, hexLit := mysqlCategory(r[2])
		extra := strings.ToUpper(r[5])
		t.Columns = append(t.Columns, &Column{
			Name: r[1], Type: r[3], Category: cat, Nullable: r[4] == "YES",
			Generated: strings.Contains(extra, "VIRTUAL GENERATED") || strings.Contains(extra, "STORED GENERATED") || strings.Contains(extra, "PERSISTENT GENERATED"),
			hexLit:    hexLit, Collation: r[6],
		})
	}

	idx, err := queryStrings(ctx, c, `SELECT TABLE_NAME, INDEX_NAME, COLUMN_NAME, NON_UNIQUE
		FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, db)
	if err != nil {
		return nil, err
	}
	unique := map[[2]string][]string{}
	for _, r := range idx {
		t := byName[r[0]]
		if t == nil || r[3] != "0" {
			continue
		}
		if r[1] == "PRIMARY" {
			t.PrimaryKey = append(t.PrimaryKey, r[2])
		}
		k := [2]string{r[0], r[1]}
		unique[k] = append(unique[k], r[2])
	}
	for k, list := range unique {
		if len(list) == 1 {
			if col := byName[k[0]].Column(list[0]); col != nil {
				col.Unique = true
			}
		}
	}

	fks, err := queryStrings(ctx, c, `SELECT TABLE_NAME, CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION`, db)
	if err != nil {
		return nil, err
	}
	for _, r := range fks {
		t := byName[r[0]]
		if t == nil {
			continue
		}
		n := len(t.ForeignKeys)
		if n == 0 || t.ForeignKeys[n-1].Name != r[1] {
			t.ForeignKeys = append(t.ForeignKeys, ForeignKey{Name: r[1], Table: r[0], RefTable: r[3]})
			n++
		}
		fk := &t.ForeignKeys[n-1]
		fk.Columns = append(fk.Columns, r[2])
		fk.RefColumns = append(fk.RefColumns, r[4])
	}

	for _, t := range s.Tables {
		def, err := dbx.ShowCreateSQL(ctx, c, "TABLE", db, t.Name, false)
		if err != nil {
			return nil, err
		}
		t.create = []string{"DROP TABLE IF EXISTS " + t.quoted, dbx.MySQLCreateTable(def)}
	}
	for _, name := range sequences {
		def, err := dbx.ShowCreateSQL(ctx, c, "SEQUENCE", db, name, false)
		if err != nil {
			return nil, err
		}
		s.before = append(s.before, Stmt{SQL: "DROP SEQUENCE IF EXISTS " + d.quote(name)}, Stmt{SQL: def})
	}

	// Stored routines and events before the triggers and views that may
	// call them. Like those, each is made with the SQL mode and collation
	// it was made with, without its DEFINER, in the database it is written to.
	routines, err := queryStrings(ctx, c, `SELECT ROUTINE_TYPE, ROUTINE_NAME FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?
		UNION ALL SELECT 'EVENT', EVENT_NAME FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? ORDER BY 1, 2`, db, db)
	if err != nil {
		return nil, err
	}
	for _, r := range routines {
		kind, name := r[0], r[1]
		show, err := dbx.ShowCreate(ctx, c, kind, db, name, false)
		if err != nil {
			return nil, err
		}
		def := show["Create "+strings.ToUpper(kind[:1])+strings.ToLower(kind[1:])]
		if def == "" {
			return nil, fmt.Errorf("%w: the definition of %s %s cannot be read: the user needs the SHOW_ROUTINE privilege (MySQL), SELECT on mysql.proc (MariaDB) or to be its definer", ErrUnsupported, strings.ToLower(kind), name)
		}
		set := "SET sql_mode = " + quoteLiteral(show["sql_mode"]) + ", collation_connection = " + quoteLiteral(show["collation_connection"])
		reset := "SET sql_mode = '" + mysqlMode + "'"
		if kind == "EVENT" {
			set += ", time_zone = " + quoteLiteral(show["time_zone"])
			reset += ", time_zone = '+00:00'"
		}
		s.after = append(s.after,
			Stmt{SQL: "DROP " + kind + " IF EXISTS " + d.quote(name)},
			Stmt{SQL: set},
			Stmt{SQL: unqualify(strings.TrimSpace(definer().ReplaceAllString(def, "")), db), Compound: true},
			Stmt{SQL: "SET NAMES utf8mb4"},
			Stmt{SQL: reset},
		)
	}

	// Triggers, in firing order per table, each with its own SQL mode and
	// collation (string literals compare with it).
	trs, err := dbx.MySQLTriggers(ctx, c, db)
	if err != nil {
		return nil, err
	}
	for _, tr := range trs {
		if dbx.MySQLProbe(tr.Name) || dbx.MySQLProbe(tr.Table) {
			continue
		}
		show, err := dbx.ShowCreate(ctx, c, "TRIGGER", db, tr.Name, false)
		if err != nil {
			return nil, err
		}
		uses := []string{tr.Table}
		s.after = append(s.after,
			Stmt{SQL: "SET sql_mode = " + quoteLiteral(show["sql_mode"]) + ", collation_connection = " + quoteLiteral(show["collation_connection"]), uses: uses},
			Stmt{SQL: unqualify(strings.TrimSpace(definer().ReplaceAllString(show["SQL Original Statement"], "")), db), Compound: true, uses: uses},
			Stmt{SQL: "SET NAMES utf8mb4", uses: uses},
			Stmt{SQL: "SET sql_mode = '" + mysqlMode + "'", uses: uses},
		)
	}

	defs := map[string]string{}
	collations := map[string]string{}
	for _, v := range views {
		show, err := dbx.ShowCreate(ctx, c, "VIEW", db, v, false)
		if err != nil {
			return nil, err
		}
		defs[v] = unqualify(definer().ReplaceAllString(show["Create View"], ""), db)
		collations[v] = show["collation_connection"]
	}
	mentions := func(def, name string) bool { return strings.Contains(def, d.quote(name)) }
	for _, v := range viewOrder(views, func(v, other string) bool { return mentions(defs[v], other) }) {
		var uses []string
		for _, t := range s.Tables {
			if mentions(defs[v], t.Name) {
				uses = append(uses, t.Name)
			}
		}
		s.after = append(s.after, Stmt{SQL: "DROP VIEW IF EXISTS " + d.quote(v), uses: uses},
			Stmt{SQL: "SET collation_connection = " + quoteLiteral(collations[v]), uses: uses},
			Stmt{SQL: defs[v], uses: uses, view: v},
			Stmt{SQL: "SET NAMES utf8mb4", uses: uses})
	}
	// The increment and the cache are stated again, as they are: a Galera
	// node checks an ALTER SEQUENCE against the cache of the statement and
	// refuses a bare RESTART (1235).
	for _, name := range sequences {
		q := d.quote(name)
		s.sequences = append(s.sequences, sequence{name: name, read: "SELECT next_not_cached_value, increment, IF(cache_size = 0, 'NOCACHE', CONCAT('CACHE ', cache_size)) FROM " + q,
			set: func(v []string) string {
				return "ALTER SEQUENCE " + q + " INCREMENT BY " + v[1] + " " + v[2] + " RESTART WITH " + v[0]
			}})
	}
	return s, nil
}

func (d mysqlDialect) selectRows(t *Table, cols []*Column, where string) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = d.quote(c.Name)
	}
	q := "SELECT " + strings.Join(names, ", ") + " FROM " + t.quoted
	if where != "" {
		q += " WHERE " + where
	}
	return q + " ORDER BY " + strings.Join(orderBy(t, func(name string, pk bool) string {
		if pk {
			return d.quote(name)
		}
		return "CAST(" + d.quote(name) + " AS BINARY)"
	}), ", ")
}

// orderBy is a total order of the rows: the primary key, or for a table
// without one, every column by its bytes (identical rows are interchangeable).
func orderBy(t *Table, expr func(name string, pk bool) string) []string {
	var out []string
	if len(t.PrimaryKey) > 0 {
		for _, k := range t.PrimaryKey {
			out = append(out, expr(k, true))
		}
		return out
	}
	for _, c := range t.Columns {
		out = append(out, expr(c.Name, false))
	}
	return out
}

func (d mysqlDialect) insert(t *Table, cols []*Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = d.quote(c.Name)
	}
	return "INSERT INTO " + t.quoted + " (" + strings.Join(names, ", ") + ") VALUES"
}

var plainNumber = lazy.Regexp(`^-?[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?$`)

func (mysqlDialect) literal(c *Column, v []byte) string {
	switch {
	case v == nil:
		return "NULL"
	case c.hexLit:
		if len(v) == 0 {
			return "''"
		}
		return "X'" + hex.EncodeToString(v) + "'"
	case c.Category == CatNumeric && plainNumber().Match(v):
		return string(v)
	}
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('\'')
	for _, ch := range v {
		switch ch {
		case 0:
			b.WriteString(`\0`)
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(ch)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// unqualify removes the name of the database db where it qualifies another
// name (`db`.`t`, db.t, db . t), outside strings and comments, so that a
// definition read from one database makes the same object in another: a
// trigger of the copy must not write into the source. A name of another
// database stays.
func unqualify(q, db string) string {
	var b strings.Builder
	b.Grow(len(q))
	// qualifies tells whether name, which ends at end, is followed by a dot,
	// and where the dot ends.
	qualifies := func(name string, end int) (int, bool) {
		if !strings.EqualFold(name, db) {
			return 0, false
		}
		for end < len(q) && strings.IndexByte(" \t\r\n", q[end]) >= 0 {
			end++
		}
		return end + 1, end < len(q) && q[end] == '.'
	}
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(q) {
				if q[j] == '\\' && c != '`' {
					j += 2
					continue
				}
				if q[j] == c {
					if j+1 < len(q) && q[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			j = min(j+1, len(q))
			// "db". is a qualifier only under ANSI_QUOTES, and invalid otherwise.
			if c != '\'' && j-i >= 2 {
				if next, ok := qualifies(strings.ReplaceAll(q[i+1:j-1], string(c)+string(c), string(c)), j); ok {
					i = next
					continue
				}
			}
			b.WriteString(q[i:j])
			i = j
		case c == '#' || strings.HasPrefix(q[i:], "-- ") || strings.HasPrefix(q[i:], "--\n"):
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q) - i
			}
			b.WriteString(q[i : i+j])
			i += j
		case strings.HasPrefix(q[i:], "/*"):
			j := strings.Index(q[i+2:], "*/")
			if j < 0 {
				j = len(q) - i - 4
			}
			b.WriteString(q[i : i+j+4])
			i += j + 4
		case mysqlIdentByte(c):
			j := i
			for j < len(q) && mysqlIdentByte(q[j]) {
				j++
			}
			if next, ok := qualifies(q[i:j], j); ok && (i == 0 || q[i-1] != '.') {
				i = next
				continue
			}
			b.WriteString(q[i:j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func mysqlIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// identity: MySQL's server UUID; MariaDB has none, so its host name, port,
// data directory and server id, which differ between any two servers that
// can be reached at once (two containers have two host names).
func (mysqlDialect) identity(ctx context.Context, db *sql.DB) (string, error) {
	flavor, err := dbx.Flavor(ctx, db)
	if err != nil {
		return "", err
	}
	q := "SELECT @@server_uuid"
	if flavor == dbx.FlavorMariaDB {
		q = "SELECT CONCAT_WS(' ', @@hostname, @@port, @@datadir, @@server_id)"
	}
	var id sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&id); err != nil {
		return "", err
	}
	if strings.TrimSpace(id.String) == "" {
		return "", errors.New("the server has no identity")
	}
	return flavor + " " + id.String, nil
}

func (mysqlDialect) inspect(ctx context.Context, admin *sql.DB, d Database) (dbState, error) {
	var st dbState
	var marked int
	var err error
	if st.exists, err = dbx.MySQLDatabaseExists(ctx, admin, d.Name); err != nil || !st.exists {
		return st, err
	}
	// Tables, views and sequences; routines; events (triggers belong to tables).
	err = admin.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME <> ?)
			+ (SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?)
			+ (SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?),
			(SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND TABLE_TYPE = 'BASE TABLE')`,
		d.Name, markerTable, d.Name, d.Name, d.Name, markerTable).Scan(&st.objects, &marked)
	st.marked = marked > 0
	return st, err
}

func (d mysqlDialect) create(ctx context.Context, admin *sql.DB, src Database, name string) error {
	srcDB, err := open(src, "")
	if err != nil {
		return err
	}
	defer srcDB.Close()
	charset, collation, err := dbx.MySQLCharset(ctx, srcDB, src.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("source database %s does not exist", src.Name)
	}
	if err != nil {
		return err
	}
	q := d.quote(name)
	for _, st := range []string{
		"CREATE DATABASE " + q + " CHARACTER SET " + charset + " COLLATE " + collation,
		"CREATE TABLE " + q + "." + d.quote(markerTable) + " (id TINYINT NOT NULL PRIMARY KEY, note VARCHAR(200) NOT NULL)",
		"INSERT INTO " + q + "." + d.quote(markerTable) + " VALUES (1, " + quoteLiteral(markerNote) + ")",
	} {
		if _, err := admin.ExecContext(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// publish: MySQL cannot rename a database, so the target is created like
// the staging database and the tables (with the marker and MariaDB's
// sequences) move into it in one RENAME TABLE. A table with triggers cannot
// move to another database and a view cannot move at all, so they are
// dropped first and made again in the target from the staging database's
// own definitions.
func (d mysqlDialect) publish(ctx context.Context, admin *sql.DB, staging, target Database) (err error) {
	c, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := d.session(ctx, c); err != nil {
		return err
	}
	s, err := d.schema(ctx, c, staging.Name)
	if err != nil {
		return err
	}
	charset, collation, err := dbx.MySQLCharset(ctx, c, staging.Name)
	if err != nil {
		return err
	}
	objs, err := dbx.MySQLObjects(ctx, c, staging.Name)
	if err != nil {
		return err
	}
	trs, err := dbx.MySQLTriggers(ctx, c, staging.Name)
	if err != nil {
		return err
	}
	from, to := d.quote(staging.Name)+".", d.quote(target.Name)+"."
	var moves []string
	var stmts []string
	for _, tr := range trs {
		stmts = append(stmts, "DROP TRIGGER "+from+d.quote(tr.Name))
	}
	for _, o := range objs {
		if o.Type == "VIEW" {
			stmts = append(stmts, "DROP VIEW "+from+d.quote(o.Name))
		} else {
			moves = append(moves, from+d.quote(o.Name)+" TO "+to+d.quote(o.Name))
		}
	}
	if _, err := c.ExecContext(ctx, "CREATE DATABASE "+d.quote(target.Name)+" CHARACTER SET "+charset+" COLLATE "+collation); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			dropDB(context.Background(), d, admin, target.Driver, target.Name)
		}
	}()
	stmts = append(stmts, "RENAME TABLE "+strings.Join(moves, ", "), "USE "+d.quote(target.Name))
	for _, st := range stmts {
		if _, err := c.ExecContext(ctx, st); err != nil {
			return err
		}
	}
	for _, st := range s.after {
		if _, err := c.ExecContext(ctx, st.SQL); err != nil {
			return fmt.Errorf("%w (%.200s)", quietError(err), st.SQL)
		}
	}
	return nil
}
