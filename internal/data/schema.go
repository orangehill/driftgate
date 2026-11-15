package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
)

// Drivers.
const (
	MySQL    = "mysql" // MySQL and MariaDB
	Postgres = "postgres"
)

// Database names one database on a server.
type Database struct {
	Driver string // mysql (MariaDB too) or postgres
	// DSN reaches the server: mysql://user:pass@host:port/ or the Go driver's
	// own form for MySQL, a postgres:// URL for PostgreSQL. Its database part
	// is ignored (for PostgreSQL it is the database that creates and drops
	// the target, default postgres).
	DSN  string
	Name string // the database
	// Dial, when set, makes every connection to the server: only to an
	// allowed host (dbx.AllowDial). A staging database made from the
	// target keeps it.
	Dial dbx.DialFunc
}

// Hosts lists every host the driver may connect to with the DSN, as the
// driver itself reads it (PostgreSQL: several hosts, fallbacks, host=
// parameters, PG* variables); "" stands for a local socket.
func (d Database) Hosts() ([]string, error) {
	return dbdsn.Hosts(d.Driver, d.DSN)
}

// open connects to the database (db "" means the server only; for
// PostgreSQL the database of the DSN).
func open(d Database, db string) (*sql.DB, error) {
	switch d.Driver {
	case MySQL:
		return dbx.OpenMySQL(d.DSN, db, d.Dial)
	case Postgres:
		return dbx.OpenPostgres(d.DSN, db, d.Dial)
	}
	return nil, fmt.Errorf("unknown driver %q (mysql, postgres)", d.Driver)
}

// Column categories decide which strategies fit a column.
const (
	CatText    = "text"    // character data (and UUID): every strategy
	CatNumeric = "numeric" // integers and decimals: universal or token on the digits
	CatBinary  = "binary"  // bytes: keep, null or empty
	CatJSON    = "json"    // a JSON type: json, null or keep
	CatOther   = "other"   // dates, enums, network addresses, geometry…: keep or null
)

// Column is one column of a table.
type Column struct {
	Name      string
	Type      string // as the server names it: varchar(255), jsonb, int(10) unsigned…
	Category  string
	Nullable  bool
	Generated bool // computed by the server: never written
	Unique    bool // alone in a unique index or the primary key
	// Collation decides which values are equal (MySQL: every text column;
	// PostgreSQL: one other than the type's default). Replaced values that
	// are equal under it get stand-ins that are equal under it.
	Collation string

	nondeterministic bool // PostgreSQL: a collation that does not compare bytes

	hexLit bool // MySQL: written as X'…' (bytes, bits, geometry)
}

// ForeignKey is a reference from rows of Table to rows of RefTable.
type ForeignKey struct {
	Name       string
	Table      string
	Columns    []string
	RefTable   string
	RefColumns []string
}

// Table is one table: its columns in their order, its keys and the
// statements that create it.
type Table struct {
	Name        string // as in classify.yaml: the table, or schema.table outside PostgreSQL's public schema
	Columns     []*Column
	PrimaryKey  []string
	ForeignKeys []ForeignKey

	quoted   string   // the name for SQL
	create   []string // statements that create the table, before its rows
	identity bool     // PostgreSQL: a GENERATED ALWAYS identity column
}

// Column finds a column by name.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Stmt is one SQL statement of the schema. Compound statements (MySQL
// triggers) need another delimiter in a dump.
type Stmt struct {
	SQL      string
	Compound bool

	uses    []string // tables the statement needs; left out when one of them is
	view    string   // the view it creates
	rows    *Table   // an INSERT of rows of this table
	inOrder bool     // its rows are written in the order they were read (see writer)
}

// Schema is what the module knows about a database: its tables in name
// order, their references, and the statements that recreate the rest of
// the schema around the rows.
type Schema struct {
	Driver string
	Tables []*Table

	header []Stmt // session settings, at the top of a dump and on the target
	before []Stmt // before the tables: schemas, extensions, types, sequences
	after  []Stmt // after the rows: constraints, indexes, sequence values, functions, triggers, views

	sequences []sequence // where each sequence continues, read in the snapshot
}

// sequence is a sequence whose next value is read from the source and set
// on the target after the rows.
type sequence struct {
	name string
	read string                    // query for the value (one row)
	set  func(v []string) []string // statements that set it
	uses []string
}

// ErrTargetExists means the target database has tables and replacing it was
// not asked for.
var ErrTargetExists = errors.New("target exists")

// ErrNotTarget means the target database holds objects but was not written
// by DriftGate (it has no marker), so it is never dropped, not even with
// Replace.
var ErrNotTarget = errors.New("not a DriftGate target")

// ErrUnsupported means the database has objects the module cannot copy yet.
var ErrUnsupported = errors.New("not supported yet")

// Table finds a table by name.
func (s *Schema) Table(name string) *Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// References lists the foreign keys that point at a table, in a stable order.
func (s *Schema) References(table string) []ForeignKey {
	var out []ForeignKey
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			if fk.RefTable == table {
				out = append(out, fk)
			}
		}
	}
	return out
}

// Order lists the tables parents first: a table comes after every table its
// foreign keys point at, except along a cycle (self-references and loops are
// broken in name order). data subset walks the rows in this order.
func (s *Schema) Order() []*Table {
	byName := map[string]*Table{}
	for _, t := range s.Tables {
		byName[t.Name] = t
	}
	done := map[string]bool{}
	visiting := map[string]bool{}
	var out []*Table
	var visit func(t *Table)
	visit = func(t *Table) {
		if done[t.Name] || visiting[t.Name] {
			return
		}
		visiting[t.Name] = true
		var parents []string
		for _, fk := range t.ForeignKeys {
			if fk.RefTable != t.Name {
				parents = append(parents, fk.RefTable)
			}
		}
		sort.Strings(parents)
		for _, p := range parents {
			if pt := byName[p]; pt != nil {
				visit(pt)
			}
		}
		visiting[t.Name] = false
		done[t.Name] = true
		out = append(out, t)
	}
	for _, t := range s.Tables {
		visit(t)
	}
	return out
}

// dialect is what differs between MySQL/MariaDB and PostgreSQL.
type dialect interface {
	// session prepares a connection: text values in a stable form (time zone,
	// date style), for reading the source and writing the target alike.
	session(ctx context.Context, c *sql.Conn) error
	// snapshot starts a read-only transaction that sees one consistent state.
	snapshot(ctx context.Context, c *sql.Conn) error
	// schema reads the tables and everything else that is recreated.
	schema(ctx context.Context, c *sql.Conn, db string) (*Schema, error)
	// selectRows reads every row of a table as text, in a stable order.
	selectRows(t *Table, cols []*Column, where string) string
	literal(c *Column, v []byte) string
	quote(name string) string
	// insert starts an INSERT of the given columns.
	insert(t *Table, cols []*Column) string
	// identity names the server a connection reaches, by what the server
	// says about itself (not by the DSN that reached it); two connections
	// to one server give the same text.
	identity(ctx context.Context, db *sql.DB) (string, error)
	// inspect tells what a database on the server of admin holds.
	inspect(ctx context.Context, admin *sql.DB, d Database) (dbState, error)
	// create makes an empty database like the source, marked as a target.
	create(ctx context.Context, admin *sql.DB, src Database, name string) error
	// publish gives the staging database the name of the target, which does
	// not exist; on an error the target is left out.
	publish(ctx context.Context, admin *sql.DB, staging, target Database) error
}

func dialectFor(driver string) (dialect, error) {
	switch driver {
	case MySQL:
		return mysqlDialect{}, nil
	case Postgres:
		return postgresDialect{}, nil
	}
	return nil, fmt.Errorf("unknown driver %q (mysql, postgres)", driver)
}

// ReadSchema reads the schema of a database: tables, columns, keys and
// foreign keys (data subset builds its row graph from them).
func ReadSchema(ctx context.Context, d Database) (*Schema, error) {
	dl, err := dialectFor(d.Driver)
	if err != nil {
		return nil, err
	}
	db, err := open(d, d.Name)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if err := dl.session(ctx, c); err != nil {
		return nil, err
	}
	return dl.schema(ctx, c, d.Name)
}

func queryStrings(ctx context.Context, c *sql.Conn, q string, args ...any) ([][]string, error) {
	rows, err := c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// viewOrder sorts views so that a view comes after the views it reads:
// by name, each moved behind the views reads(view, other) names.
func viewOrder(names []string, reads func(view, other string) bool) []string {
	sort.Strings(names)
	done := map[string]bool{}
	visiting := map[string]bool{}
	var out []string
	var visit func(n string)
	visit = func(n string) {
		if done[n] || visiting[n] {
			return
		}
		visiting[n] = true
		for _, o := range names {
			if o != n && reads(n, o) {
				visit(o)
			}
		}
		visiting[n] = false
		done[n] = true
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}
