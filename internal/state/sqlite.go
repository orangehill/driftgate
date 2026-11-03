package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/orangehill/driftgate/internal/glob"
)

// SQLite is an SQLite database file. The pristine copy is a second file,
// made by VACUUM INTO, and a restore copies back, inside the working file,
// only the tables, views and triggers that differ, in one transaction, the
// way the application's own SQLite writes: the file is never replaced, so
// connections the application keeps open see the restored data, and its
// -wal, -shm and -journal files are left to SQLite.
//
// Two rules keep the locks of the application's connections intact: every
// connection opens the file through an escaped file: URI (a plain name loses
// its query, so mode=ro would open the file writable), and the process never
// opens and closes the working or pristine file itself (closing a file drops
// every POSIX lock the process holds on it, also those of another
// component's connections): files are only looked at with os.Stat, and
// SQLite itself tells whether one is a database.
type SQLite struct {
	o  Options
	id os.FileInfo // the working file the connections were opened on
	db *sql.DB     // the working file, read-write
	// main stays open for the life of the component: PRAGMA data_version
	// tells changes from other connections since its own last read.
	main *sql.Conn

	// The last full fingerprint and the data_version and schema_version it
	// was read at; own is set by a restore through main, whose commits do
	// not move main's data_version.
	last   Fingerprint
	dv, sv int64
	stamp  sqliteStamp
	own    bool
	reused bool
	// scanned lists the tables the last fingerprint read row by row (the
	// others kept their row hash).
	scanned []string
	// sums holds the row hash of every table read by the last fingerprints,
	// with the digest of the pages it was read from (sqlitePages).
	sums map[string]sqliteSum
	// pristine is the fingerprint of the pristine file, with the identity
	// of the file it was read from.
	pristine   Fingerprint
	pristineID os.FileInfo
}

// sqliteConns is how many connections besides main share a full
// fingerprint's reading of the tables.
var sqliteConns = 4

// sqlitePristine is the schema name the pristine file is attached as for a
// restore.
const sqlitePristine = "dg_pristine"

// NewSQLite opens the working database file. Working and Pristine are
// absolute paths the configuration has checked to be inside the project;
// the DSN is not used. The working file must exist and be an SQLite
// database; the pristine file may not exist yet (driftgate seed makes it),
// and is never the working file or one of the files SQLite keeps next to it.
func NewSQLite(o Options) (*SQLite, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if !filepath.IsAbs(o.Working) || !filepath.IsAbs(o.Pristine) {
		return nil, fmt.Errorf("state %s: the working and pristine files must be absolute paths", o.Name)
	}
	o.Working, o.Pristine = filepath.Clean(o.Working), filepath.Clean(o.Pristine)
	if err := sqliteSeparate(o.Working, o.Pristine); err != nil {
		return nil, fmt.Errorf("state %s: %v", o.Name, err)
	}
	s := &SQLite{o: o}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

// sqliteSidecars are the files SQLite keeps next to a database file, and the
// one a seed makes next to the pristine file before it replaces it.
var sqliteSidecars = []string{"-wal", "-shm", "-journal"}

// sqliteNew is the file a seed makes the new pristine copy in.
const sqliteNew = ".driftgate-new"

// sqliteSeparate refuses a pristine file that is the working file, or one
// of the files that belong to either: a seed replaces the pristine file and
// removes what SQLite left next to it and next to its new copy. A working
// file whose name starts with the pristine file's (in any letter case) is
// refused too, whatever follows: every file a seed makes or removes is
// named so.
func sqliteSeparate(working, pristine string) error {
	own := []string{pristine, pristine + sqliteNew}
	for _, suf := range sqliteSidecars {
		own = append(own, pristine+suf, pristine+sqliteNew+suf)
	}
	for _, p := range own {
		if samePath(p, working) {
			return fmt.Errorf("the pristine file %s is the working database file %s or one next to it", pristine, working)
		}
	}
	for _, suf := range sqliteSidecars {
		if samePath(pristine, working+suf) {
			return fmt.Errorf("the pristine file %s is the working database's %s file", pristine, suf)
		}
	}
	if len(working) >= len(pristine) && strings.EqualFold(working[:len(pristine)], pristine) {
		return fmt.Errorf("the working database file %s is named like the pristine file %s or a file a seed makes or removes next to it; name another pristine file", working, pristine)
	}
	return nil
}

// samePath reports whether two cleaned absolute paths name the same file:
// the same file on disk, or, for one that does not exist, the same name
// (ignoring case where the file systems usually do).
func samePath(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA == nil && errB == nil {
		return os.SameFile(fa, fb)
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// sqliteFile checks, without opening it, that path is a file that can be
// a database (not a folder, not empty) and returns its identity; whether it
// is an SQLite database is up to SQLite (sqliteCheck).
func sqliteFile(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errors.New("is a folder")
	}
	if fi.Size() == 0 {
		return nil, errors.New("is empty (run the application's migrations first)")
	}
	return fi, nil
}

// sqliteCheck reads the schema of the file a connection is open on, which
// fails for a file that is no SQLite database.
func sqliteCheck(ctx context.Context, q queryer) error {
	var n int64
	err := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&n)
	if se := (*sqlite.Error)(nil); errors.As(err, &se) && se.Code()&0xff == sqliteNotADB {
		return errNotSQLite
	}
	return err
}

var errNotSQLite = errors.New("is not an SQLite database")

// sqliteNotADB is SQLITE_NOTADB.
const sqliteNotADB = 26

// sqliteStamp is what the file system says about the working file and its
// -wal file: a change SQLite was not told about (a copy written over the
// file) shows there, not in data_version.
type sqliteStamp struct {
	size, walSize   int64
	mtime, walMtime int64
	wal             bool
}

func (s *SQLite) stampNow() sqliteStamp {
	var st sqliteStamp
	if fi, err := os.Stat(s.o.Working); err == nil {
		st.size, st.mtime = fi.Size(), fi.ModTime().UnixNano()
	}
	if fi, err := os.Stat(s.o.Working + "-wal"); err == nil {
		st.wal, st.walSize, st.walMtime = true, fi.Size(), fi.ModTime().UnixNano()
	}
	return st
}

// sqliteURI writes an absolute path as a file: URI with the given query
// parameters. Every byte other than letters, digits, -._~ and the path's
// slashes (and a Windows drive's colon) is percent-encoded, so ?, # and %
// in a name stay part of it.
func sqliteURI(path string, params ...string) (string, error) {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(p, "//") {
			return "", fmt.Errorf("%s: a database on a network share (UNC path) is not supported", path)
		}
		p = "/" + p // file:///C:/…
	}
	var b strings.Builder
	b.WriteString("file://")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	if len(params) > 0 {
		b.WriteString("?" + strings.Join(params, "&"))
	}
	return b.String(), nil
}

// sqliteBusyWait is how long DriftGate's connections wait for a lock
// another connection holds (busy_timeout of sqliteSession and
// sqliteReadOnly).
var sqliteBusyWait = 10 * time.Second

// busy turns SQLite's "database is locked" (SQLITE_BUSY, after
// sqliteBusyWait) into what happened and what to do.
func (s *SQLite) busy(err *error) {
	var e *sqlite.Error
	if *err == nil || !errors.As(*err, &e) || e.Code()&0xff != 5 {
		return
	}
	*err = fmt.Errorf("state %s: another connection held a lock on %s for %s (%w): a long transaction of the application, or a worker (queue, scheduler) writing to it; end that transaction or stop the worker, then run the command again", s.o.Name, s.o.Working, sqliteBusyWait, *err)
}

// sqliteSession is how DriftGate's connections to the working file work: the
// file must exist (mode=rw, never created), a lock held by the application
// is waited for, foreign keys do not cascade a restore's deletes, and the
// schema's functions run only where SQLite deems them safe. The journal
// mode and locking mode stay the application's.
var sqliteSession = []string{"mode=rw", "_pragma=busy_timeout(10000)", "_pragma=foreign_keys(0)", "_pragma=trusted_schema(0)"}

// sqliteReadOnly opens a file for reading only: the pristine copy for a
// query or a fingerprint, or a file a seed checks before it replaces it.
var sqliteReadOnly = []string{"mode=ro", "_pragma=busy_timeout(10000)", "_pragma=trusted_schema(0)", "_pragma=query_only(1)", "_defensive=1"}

func sqliteOpen(path string, params []string) (*sql.DB, error) {
	uri, err := sqliteURI(path, params...)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", uri)
}

// open checks the working file and connects to it.
func (s *SQLite) open() error {
	id, err := sqliteFile(s.o.Working)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state %s: working database file %s does not exist", s.o.Name, s.o.Working)
	}
	if err != nil {
		return fmt.Errorf("state %s: working database file %s: %v", s.o.Name, s.o.Working, err)
	}
	db, err := sqliteOpen(s.o.Working, sqliteSession)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(2 + sqliteConns)
	db.SetMaxIdleConns(2 + sqliteConns)
	c, err := db.Conn(context.Background())
	if err == nil {
		err = sqliteCheck(context.Background(), c)
	}
	if err != nil {
		if c != nil {
			c.Close()
		}
		db.Close()
		return fmt.Errorf("state %s: working database file %s: %v", s.o.Name, s.o.Working, err)
	}
	s.id, s.db, s.main = id, db, c
	return nil
}

// current makes sure the connections are to the file now at the working
// path: when the application (or anyone) replaced the file, they are opened
// again on the new one, and nothing read from the old one is trusted.
func (s *SQLite) current() error {
	fi, err := os.Stat(s.o.Working)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("state %s: working database file %s does not exist", s.o.Name, s.o.Working)
	}
	if err != nil {
		return err
	}
	if os.SameFile(fi, s.id) {
		return nil
	}
	s.sums = nil
	return s.reopen()
}

// reopen opens the connections again: nothing they cached of the file is
// kept, nor the fingerprint the next one could be reused from.
func (s *SQLite) reopen() error {
	s.forget()
	s.main.Close()
	s.db.Close()
	return s.open()
}

// forget drops the fingerprint the next one could be reused from.
func (s *SQLite) forget() { s.last, s.own = nil, false }

func (s *SQLite) Name() string  { return s.o.Name }
func (s *SQLite) Partial() bool { return true }

func (s *SQLite) Close() error {
	s.main.Close()
	return s.db.Close()
}

// Reused reports whether the last fingerprint was the one before it,
// nothing having been written to the file since.
func (s *SQLite) Reused() bool { return s.reused }

// qs quotes an SQLite identifier.
func qs(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// ql quotes an SQLite text literal.
func ql(text string) string { return `'` + strings.ReplaceAll(text, `'`, `''`) + `'` }

// sqliteInternal reports whether a table belongs to SQLite or to DriftGate
// rather than to the application: sqlite_sequence (part of the tables it
// counts for), sqlite_stat* (statistics ANALYZE keeps) and the mark of a
// pristine copy.
func sqliteInternal(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "sqlite_") || name == pristineTable
}

// asciiLower folds a name as SQLite compares names: A–Z only.
func asciiLower(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// sqliteTable is a table of the schema: its CREATE statement, the CREATE
// statements of its indexes (sorted), and what kind of table it is.
type sqliteTable struct {
	name, sql string
	indexes   []string
	virtual   bool // CREATE VIRTUAL TABLE: its rows live in its shadow tables
	shadow    bool // a table a virtual table keeps its rows in
	noRowid   bool // WITHOUT ROWID
}

// def is the definition a restore compares: the table and its indexes.
func (t *sqliteTable) def() string {
	var b bytes.Buffer
	writeField(&b, []byte(t.sql))
	for _, ix := range t.indexes {
		writeField(&b, []byte(ix))
	}
	return b.String()
}

type sqliteTrigger struct{ table, sql string }

// sqliteObjects is the application's schema in one database file.
type sqliteObjects struct {
	tables   map[string]*sqliteTable
	order    []string // tables in schema order
	excluded []string // excluded tables (not the shadow tables of an excluded virtual table)
	// exvirtual holds the statement of every excluded virtual table, and
	// exshadow lists the shadow tables of those (a seed's cleaning).
	exvirtual map[string]string
	exshadow  []string
	views     map[string]string
	vorder    []string
	triggers  map[string]sqliteTrigger
	torder    []string
	seq       map[string]any // sqlite_sequence: table → seq
}

// excludedTable reports whether a table is excluded: by name, or as a
// shadow table of an excluded virtual table.
func (o Options) excludedTable(name string, owner string) bool {
	return o.sqliteExcluded(name) || owner != "" && o.sqliteExcluded(owner)
}

// sqliteExcluded is excluded for SQLite, whose names are ASCII-case-
// insensitive: exclude: sessions also names a table made as Sessions.
func (o Options) sqliteExcluded(name string) bool {
	for _, g := range o.Exclude {
		if glob.Match(asciiLower(g), asciiLower(name)) {
			return true
		}
	}
	return false
}

// objects reads the schema of a database file (schema: main or the
// attached pristine file). Tables of SQLite and DriftGate, temporary
// objects and excluded tables and views are left out; excluded tables are
// listed apart. Indexes and triggers name their table as the table does.
func (s *SQLite) objects(ctx context.Context, q queryer, schema string) (*sqliteObjects, error) {
	o := &sqliteObjects{tables: map[string]*sqliteTable{}, views: map[string]string{}, triggers: map[string]sqliteTrigger{}, seq: map[string]any{}, exvirtual: map[string]string{}}
	kinds := map[string]string{}
	noRowid := map[string]bool{}
	rows, err := q.QueryContext(ctx, "SELECT name, type, wr FROM pragma_table_list WHERE schema = ?", schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, kind string
		var wr bool
		if err := rows.Scan(&name, &kind, &wr); err != nil {
			rows.Close()
			return nil, err
		}
		kinds[name], noRowid[name] = kind, wr
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM "+qs(schema)+".sqlite_schema ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	type entry struct{ kind, name, table, sql string }
	var all []entry
	for rows.Next() {
		var e entry
		var text sql.NullString
		if err := rows.Scan(&e.kind, &e.name, &e.table, &text); err != nil {
			rows.Close()
			return nil, err
		}
		e.sql = text.String
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	virtual := map[string]bool{}
	for _, e := range all {
		if e.kind == "table" && kinds[e.name] == "virtual" {
			virtual[e.name] = true
		}
	}
	// owner finds the virtual table a shadow table belongs to: its name is
	// the virtual table's, an underscore and a suffix of the module's.
	owner := func(name string) string {
		best := ""
		for v := range virtual {
			if strings.HasPrefix(name, v+"_") && len(v) > len(best) {
				best = v
			}
		}
		return best
	}
	// SQLite names are ASCII-case-insensitive, and an index or trigger
	// keeps the table name as its statement wrote it (ON USERS for users):
	// it is looked up by the table's own name.
	canon := map[string]string{}
	for _, e := range all {
		if e.kind == "table" || e.kind == "view" {
			canon[asciiLower(e.name)] = e.name
		}
	}
	table := func(name string) string {
		if c, ok := canon[asciiLower(name)]; ok {
			return c
		}
		return name
	}
	for _, e := range all {
		if e.kind != "table" {
			continue
		}
		if sqliteInternal(e.name) {
			continue
		}
		shadow := kinds[e.name] == "shadow"
		own := ""
		if shadow {
			own = owner(e.name)
		}
		if s.o.excludedTable(e.name, own) {
			switch {
			case shadow:
				o.exshadow = append(o.exshadow, e.name)
			case virtual[e.name]:
				o.excluded = append(o.excluded, e.name)
				o.exvirtual[e.name] = e.sql
			default:
				o.excluded = append(o.excluded, e.name)
			}
			continue
		}
		o.tables[e.name] = &sqliteTable{name: e.name, sql: e.sql, virtual: virtual[e.name], shadow: shadow, noRowid: noRowid[e.name]}
		o.order = append(o.order, e.name)
	}
	for _, e := range all {
		switch e.kind {
		case "index":
			if t := o.tables[table(e.table)]; t != nil && e.sql != "" {
				t.indexes = append(t.indexes, e.sql)
			}
		case "view":
			if !s.o.sqliteExcluded(e.name) {
				o.views[e.name] = e.sql
				o.vorder = append(o.vorder, e.name)
			}
		case "trigger":
			// Every trigger is an item, an excluded table's too: its body
			// may write any table.
			o.triggers[e.name] = sqliteTrigger{table: table(e.table), sql: e.sql}
			o.torder = append(o.torder, e.name)
		}
	}
	for _, t := range o.tables {
		sort.Strings(t.indexes)
	}
	if kinds["sqlite_sequence"] != "" {
		rows, err := q.QueryContext(ctx, "SELECT name, seq FROM "+qs(schema)+".sqlite_sequence")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var seq any
			if err := rows.Scan(&name, &seq); err != nil {
				rows.Close()
				return nil, err
			}
			o.seq[table(name)] = seq
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// queryer is a *sql.Conn, *sql.DB or *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// sqliteDenied are what a query on the pristine copy may not use, besides
// what CheckQuery refuses: statements that reach other files or change the
// connection, functions that reach files or extensions, and the tables that
// read the file's pages and free space directly.
var (
	sqliteDeniedWords = map[string]bool{"attach": true, "detach": true, "pragma": true, "vacuum": true, "reindex": true,
		"sqlite_dbpage": true, "dbstat": true, "sqlite_dbdata": true, "sqlite_dbptr": true}
	sqliteDeniedFuncs = map[string]bool{"load_extension": true, "fts3_tokenizer": true, "readfile": true, "writefile": true, "edit": true}
)

// sqliteCheckQuery is CheckQuery with the SQLite deny list over the bare
// words of the query; the authorizer of the query's connection
// (sqliteAuthorize) also checks quoted names.
func sqliteCheckQuery(query string) error {
	if err := CheckQuery(query); err != nil {
		return err
	}
	q := query
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := strings.IndexByte(q[i+1:], end)
			if j < 0 {
				return errors.New("sql: unterminated quoted text")
			}
			i += j + 2
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			j := i
			for j < len(q) && (q[j] == '_' || q[j] == '$' || q[j] >= 'a' && q[j] <= 'z' || q[j] >= 'A' && q[j] <= 'Z' || q[j] >= '0' && q[j] <= '9' || q[j] >= 0x80) {
				j++
			}
			word := strings.ToLower(q[i:j])
			if sqliteDeniedWords[word] {
				return fmt.Errorf("sql: %s is not accepted", word)
			}
			if sqliteDeniedFuncs[word] {
				return fmt.Errorf("sql: the function %s is not accepted", word)
			}
			i = j
		default:
			i++
		}
	}
	return nil
}

// sqliteAuthorize lets a query's connection compile only reads: SELECT,
// reading columns of tables other than the page-level ones, and functions
// other than the denied ones. Everything else (PRAGMA, ATTACH, writes,
// transactions) fails to compile.
func sqliteAuthorize(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
	switch action {
	case sqlite.AuthSelect, sqlite.AuthRecursive:
		return sqlite.AuthorizerOK
	case sqlite.AuthRead:
		if sqliteDeniedWords[strings.ToLower(arg1)] {
			return sqlite.AuthorizerDeny
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthFunction:
		if sqliteDeniedFuncs[strings.ToLower(arg2)] {
			return sqlite.AuthorizerDeny
		}
		return sqlite.AuthorizerOK
	}
	return sqlite.AuthorizerDeny
}

// Query runs a read-only query (see CheckQuery and sqliteCheckQuery) on the
// pristine file, opened read-only, in defensive mode and with an
// authorizer that lets only reads compile. Values come back as SQLite holds
// them: a DATETIME column's text is not parsed into a time.
func (s *SQLite) Query(ctx context.Context, query string) ([]string, error) {
	if err := sqliteCheckQuery(query); err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.o.Pristine); err != nil {
		return nil, fmt.Errorf("pristine file %s: %w (run driftgate seed)", s.o.Pristine, err)
	}
	db, err := sqliteOpen(s.o.Pristine, sqliteReadOnly)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("pristine file %s: %w (run driftgate seed)", s.o.Pristine, err)
	}
	defer c.Close()
	if err := c.Raw(func(dc any) error {
		a, ok := dc.(sqlite.AuthorizerRegisterer)
		if !ok {
			return fmt.Errorf("sqlite: no authorizer on %T", dc)
		}
		return a.RegisterAuthorizer(sqliteAuthorize)
	}); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(query), ";"))
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		rows.Close()
		return nil, err
	}
	if len(types) == 0 || !sqliteTimeTypes[types[0].DatabaseTypeName()] {
		defer rows.Close()
		return firstColumn(rows)
	}
	// The driver reads the text of a column declared DATE, DATETIME or
	// TIMESTAMP as a time, and no longer as the text it is: the first
	// column is read through a CTE, whose +column has no declared type.
	// (SQLite keeps the order of a CTE used once; a query whose first
	// column is no such column does not depend on it.)
	rows.Close()
	names := make([]string, len(types))
	for i := range types {
		names[i] = fmt.Sprintf("dg_%d", i+1)
	}
	rows, err = c.QueryContext(ctx, "WITH dg_query("+strings.Join(names, ", ")+") AS ("+q+") SELECT +dg_1 FROM dg_query")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return firstColumn(rows)
}

// sqliteTimeTypes are the declared types whose text the driver reads as a
// time.
var sqliteTimeTypes = map[string]bool{"DATE": true, "DATETIME": true, "TIMESTAMP": true}
