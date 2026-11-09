package data

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
)

// The dump check of data verify reads a dump with statements.next and
// parseRows. A value they read differently from what the writer wrote is a
// value the privacy check never sees as it is: a false pass. The tests here
// write rows through the dialects' literals and the dump writer, and read
// them back.

// awkward are the bytes and words that end or open a string, a comment, a
// statement or a dollar quote somewhere in MySQL or PostgreSQL.
var awkward = []string{
	"'", `\`, "\x00", "\n", "\r", "\x1a", ";", "--", "-- ", "/*", "*/", "$$", "$t$", "E'",
	"DELIMITER", "DELIMITER ;;\n", "\nDELIMITER ;\n", ";;", `"`, "`", "''", `\'`, "X'", "NULL",
	" VALUES\n", "),\n(", ", ", "#", "$1", "e'", "\t", " ",
}

// randText is a value made of awkward pieces, letters and any bytes.
func randText(r *rand.Rand) []byte {
	var b []byte
	for range r.IntN(8) {
		switch r.IntN(4) {
		case 0, 1:
			b = append(b, awkward[r.IntN(len(awkward))]...)
		case 2:
			b = append(b, "abcxyzE0123"[r.IntN(11)])
		default:
			b = append(b, byte(r.IntN(256)))
		}
	}
	return b
}

// randNumber is a value of a numeric column: mostly a plain number, which
// the writer leaves unquoted, sometimes one it must quote.
func randNumber(r *rand.Rand) []byte {
	switch r.IntN(6) {
	case 0:
		return []byte(fmt.Sprintf("-%d.%de%d", r.IntN(1000), r.IntN(100), r.IntN(20)))
	case 1:
		return []byte([]string{"NaN", "Infinity", "-Infinity", "1e", "0x1a", "1,2", "1)", "'1'"}[r.IntN(8)])
	case 2:
		return []byte(fmt.Sprintf("%d.%d", r.IntN(1e6), r.IntN(1e3)))
	default:
		return []byte(fmt.Sprint(r.Int64N(1<<62) - 1<<61))
	}
}

// randName is an identifier with awkward bytes, never one with " VALUES\n"
// (the dump check finds the rows of an INSERT by its first such text).
func randName(r *rand.Rand) string {
	for {
		n := "t" + string(randText(r))
		if !strings.Contains(n, " VALUES\n") {
			return n
		}
	}
}

// without is a random text that does not contain any of not.
func without(r *rand.Rand, not ...string) string {
	for {
		s := string(randText(r))
		ok := true
		for _, n := range not {
			ok = ok && !strings.Contains(s, n)
		}
		if ok {
			return s
		}
	}
}

// mysqlString is a MySQL string literal in single or double quotes.
func mysqlString(r *rand.Rand) string {
	s := (mysqlDialect{}).literal(&Column{Category: CatText}, randText(r))
	if r.IntN(2) == 0 {
		return s
	}
	t := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(string(randText(r)))
	return `"` + t + `"`
}

// pgString is a PostgreSQL string literal: standard, E'…' as quote_literal
// writes a value with a backslash, or E'…' with \' escapes.
func pgString(r *rand.Rand) string {
	v := string(randText(r))
	switch r.IntN(3) {
	case 0:
		return quoteLiteral(v)
	case 1:
		return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v) + "'"
	}
	return "e'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// lineComment is a comment to the end of the line: "-- …", or MySQL's
// "#…", with quotes and delimiters in it.
func lineComment(r *rand.Rand, driver string) string {
	if driver == MySQL && r.IntN(2) == 0 {
		return "#" + without(r, "\n", "\r")
	}
	return "-- " + without(r, "\n", "\r")
}

// dollarBody is the text of a dollar quote with the given tag: pieces that
// overlap the tag ($a$a$), contain part of it ($a $), or nest another
// tag, but end the quote only at its closing tag (as PostgreSQL reads it:
// the first tag after the opening one).
func dollarBody(r *rand.Rand, tag string) string {
	inner := tag[1 : len(tag)-1] // the name between the dollars
	pieces := []string{
		tag[1:],                 // right after the opening tag: $a$a$
		tag[:len(tag)-1] + " ",  // $a without its closing dollar
		"$" + inner + " $",      // $a $
		"$b$ '; $b$", "$$", "$", // another tag, nested; dollars alone
		"\nBEGIN\n  RAISE NOTICE 'a;b';\nEND\n",
	}
	for {
		var b strings.Builder
		for range 1 + r.IntN(4) {
			if r.IntN(3) == 0 {
				b.WriteString(string(randText(r)))
			} else {
				b.WriteString(pieces[r.IntN(len(pieces))])
			}
		}
		body := b.String()
		if strings.Index(body+tag, tag) == len(body) {
			return body
		}
	}
}

// randSchemaStmt is a statement other than an INSERT of rows, with strings,
// quoted names, comments and (PostgreSQL) dollar quotes in it. Some end in
// a comment to the end of the line, which must not hide the delimiter.
func randSchemaStmt(r *rand.Rand, driver string) Stmt {
	st := schemaStmt(r, driver)
	if r.IntN(4) == 0 {
		// The reader trims a statement, as the writer's never ends in a space.
		st.SQL = strings.TrimSpace(st.SQL + " " + lineComment(r, driver))
	}
	return st
}

func schemaStmt(r *rand.Rand, driver string) Stmt {
	if driver == MySQL {
		q := (mysqlDialect{}).quote
		switch r.IntN(5) {
		case 0:
			return Stmt{SQL: "SET NAMES utf8mb4"}
		case 1:
			return Stmt{SQL: "CREATE TABLE " + q(randName(r)) + " (\n  `id` int NOT NULL COMMENT " + mysqlString(r) +
				",\n  `v` text /* " + without(r, "*/") + " */ DEFAULT " + mysqlString(r) +
				" " + lineComment(r, driver) + "\n) ENGINE=InnoDB"}
		case 2:
			return Stmt{SQL: "CREATE TRIGGER " + q(randName(r)) + " AFTER INSERT ON " + q(randName(r)) +
				" FOR EACH ROW BEGIN\n  # don't; it's " + without(r, "\n", "\r") + "\n  SET @a = " + mysqlString(r) + "; /* " + without(r, "*/") + " */\n  SET @b = " +
				mysqlString(r) + "; " + lineComment(r, driver) + "\nEND", Compound: true}
		case 3:
			return Stmt{SQL: "CREATE VIEW " + q(randName(r)) + " AS select " + mysqlString(r) + " AS " + q(randName(r))}
		}
		return Stmt{SQL: "ALTER TABLE " + q(randName(r)) + " ADD CONSTRAINT " + q(randName(r)) + " CHECK (`a`--1 > 0)"}
	}
	q := (postgresDialect{}).quote
	switch r.IntN(6) {
	case 0:
		return Stmt{SQL: "SET standard_conforming_strings = on"}
	case 1:
		tag := []string{"$$", "$a$", "$fn$", "$_x1$", "$body$"}[r.IntN(5)]
		return Stmt{SQL: "CREATE FUNCTION " + q(randName(r)) + "() RETURNS trigger LANGUAGE plpgsql AS " + tag +
			dollarBody(r, tag) + tag}
	case 2:
		return Stmt{SQL: "COMMENT ON TABLE " + q(randName(r)) + " IS " + pgString(r)}
	case 3:
		return Stmt{SQL: "CREATE TABLE " + q(randName(r)) + " (v text DEFAULT " + pgString(r) + " /* " + without(r, "*/", "/*") +
			" */, w$x int -- " + without(r, "\n", "\r") + "\n)"}
	case 4:
		return Stmt{SQL: "CREATE FUNCTION g(text) RETURNS text LANGUAGE sql AS " + pgString(r) + " SET x = $1"}
	}
	return Stmt{SQL: "CREATE VIEW " + q(randName(r)) + " AS SELECT a$b, $1 FROM t WHERE x = " + pgString(r)}
}

// dumpCase is a dump and what reading it must give: every statement, and
// for each INSERT the rows it was written with.
type dumpCase struct {
	driver string
	dump   []byte
	stmts  []string
	ncols  map[int]int        // INSERT statement → its columns
	rows   map[int][][][]byte // INSERT statement → its rows
	heads  map[int]string     // INSERT statement → its head, as dump matches it
}

// insertStmt writes rows the way engine.rows does: the head, then
// "\n(" v, v ")" for the first row and ",\n(" … for the next ones.
func insertStmt(dl dialect, t *Table, cols []*Column, rows [][][]byte) Stmt {
	var b strings.Builder
	head := dl.insert(t, cols)
	for k, vals := range rows {
		if k == 0 {
			b.WriteString(head)
			b.WriteString("\n(")
		} else {
			b.WriteString(",\n(")
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(dl.literal(cols[i], v))
		}
		b.WriteByte(')')
	}
	return Stmt{SQL: b.String(), rows: t}
}

// randTable is a table of 1–5 columns: numeric, text and (MySQL) X'…'.
func randTable(r *rand.Rand, dl dialect, driver string) (*Table, []*Column) {
	name := randName(r)
	t := &Table{Name: name, quoted: dl.quote(name), identity: driver == Postgres && r.IntN(3) == 0}
	for range 1 + r.IntN(5) {
		c := &Column{Name: randName(r), Category: CatText}
		switch r.IntN(3) {
		case 0:
			c.Category = CatNumeric
		case 1:
			if driver == MySQL {
				c.Category, c.hexLit = CatBinary, true
			}
		}
		t.Columns = append(t.Columns, c)
	}
	return t, t.Columns
}

func randValue(r *rand.Rand, c *Column) []byte {
	switch {
	case r.IntN(5) == 0:
		return nil
	case r.IntN(8) == 0:
		return []byte{}
	case c.Category == CatNumeric && r.IntN(4) > 0:
		return randNumber(r)
	}
	return randText(r)
}

// writeDump writes items through the dump writer as Anonymize does: the
// header, comments, statements of the schema and INSERTs of rows.
func writeDump(t testing.TB, driver string, items []any) []byte {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	w.WriteString(dumpHeader(DumpFormat, driver))
	fmt.Fprintf(w, "-- Pseudonymised: personal values replaced by rules in classify.yaml under a project key.\n\n")
	out := newWriter(context.Background(), &dumpSink{w: w}, nil)
	for _, it := range items {
		var err error
		switch it := it.(type) {
		case string:
			err = out.comment(context.Background(), it)
		case Stmt:
			err = out.exec(context.Background(), it)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := out.close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func randDumpCase(t testing.TB, r *rand.Rand, driver string) *dumpCase {
	dl, err := dialectFor(driver)
	if err != nil {
		t.Fatal(err)
	}
	dc := &dumpCase{driver: driver, ncols: map[int]int{}, rows: map[int][][][]byte{}, heads: map[int]string{}}
	var items []any
	for range 1 + r.IntN(8) {
		switch r.IntN(4) {
		case 0:
			items = append(items, []string{"Schema", "After the rows", "Table " + without(r, "\n", "\r")}[r.IntN(3)])
		case 1:
			st := randSchemaStmt(r, driver)
			items = append(items, st)
			dc.stmts = append(dc.stmts, st.SQL)
		default:
			tb, cols := randTable(r, dl, driver)
			rows := make([][][]byte, 1+r.IntN(4))
			for k := range rows {
				rows[k] = make([][]byte, len(cols))
				for i, c := range cols {
					rows[k][i] = randValue(r, c)
				}
			}
			st := insertStmt(dl, tb, cols, rows)
			items = append(items, st)
			dc.ncols[len(dc.stmts)] = len(cols)
			dc.rows[len(dc.stmts)] = rows
			dc.heads[len(dc.stmts)] = dl.insert(tb, cols)
			dc.stmts = append(dc.stmts, st.SQL)
		}
	}
	dc.dump = writeDump(t, driver, items)
	return dc
}

// readDump reads a dump as scanner.dump does: every statement, and the
// rows of each INSERT.
func readDump(r io.Reader, driver string, ncols func(i int, q string) int) (stmts []string, rows map[int][][][]byte, err error) {
	rows = map[int][][][]byte{}
	st := newStatements(r, driver)
	if err := st.header(driver); err != nil {
		return nil, rows, err
	}
	for {
		q, err := st.next()
		if err == io.EOF {
			return stmts, rows, nil
		}
		if err != nil {
			return stmts, rows, err
		}
		i := len(stmts)
		stmts = append(stmts, q)
		if !strings.HasPrefix(q, "INSERT INTO ") {
			continue
		}
		at := strings.Index(q, " VALUES\n")
		if at < 0 {
			return stmts, rows, fmt.Errorf("statement %d: an INSERT without rows", i)
		}
		n := ncols(i, q[:at+len(" VALUES")])
		if n < 0 {
			continue
		}
		if err := parseRows(q[at+len(" VALUES\n"):], n, driver, func(vals [][]byte) {
			row := make([][]byte, len(vals))
			for k, v := range vals {
				if v != nil {
					row[k] = append([]byte{}, v...)
				}
			}
			rows[i] = append(rows[i], row)
		}); err != nil {
			return stmts, rows, fmt.Errorf("statement %d: %w", i, err)
		}
	}
}

// check reads dc's dump (through reader, which may hand it out in small
// pieces) and reports where it differs from what was written.
func (dc *dumpCase) check(reader func(io.Reader) io.Reader) error {
	stmts, rows, err := readDump(reader(bytes.NewReader(dc.dump)), dc.driver, func(i int, head string) int {
		if dc.heads[i] != head {
			return -1
		}
		return dc.ncols[i]
	})
	if err != nil {
		return err
	}
	if len(stmts) != len(dc.stmts) {
		return fmt.Errorf("%d statements, %d written:\n%q\nwritten:\n%q", len(stmts), len(dc.stmts), stmts, dc.stmts)
	}
	for i := range stmts {
		if stmts[i] != dc.stmts[i] {
			return fmt.Errorf("statement %d:\n%q\nwritten:\n%q", i, stmts[i], dc.stmts[i])
		}
		want, ok := dc.rows[i]
		if !ok {
			continue
		}
		if _, ok := rows[i]; !ok {
			return fmt.Errorf("statement %d: the INSERT's head is not the table's (%q)", i, dc.heads[i])
		}
		if err := sameRows(rows[i], want); err != nil {
			return fmt.Errorf("statement %d: %w", i, err)
		}
	}
	return nil
}

// sameRows compares rows byte for byte, NULL (nil) apart from an empty value.
func sameRows(got, want [][][]byte) error {
	if len(got) != len(want) {
		return fmt.Errorf("%d rows, %d written", len(got), len(want))
	}
	for k := range got {
		if len(got[k]) != len(want[k]) {
			return fmt.Errorf("row %d: %d values, %d written", k, len(got[k]), len(want[k]))
		}
		for i := range got[k] {
			g, w := got[k][i], want[k][i]
			if (g == nil) != (w == nil) || !bytes.Equal(g, w) {
				return fmt.Errorf("row %d value %d: %s, written %s", k, i, showValue(g), showValue(w))
			}
		}
	}
	return nil
}

func showValue(v []byte) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%q", v)
}

var dumpReaders = []func(io.Reader) io.Reader{
	func(r io.Reader) io.Reader { return r },
	iotest.OneByteReader,
	iotest.HalfReader,
}

// TestDumpRoundTrip writes random statements and rows with every awkward
// byte through the dump writer and reads them back: the same statements,
// the same values byte for byte, NULL kept.
//
// Mutating statements.next is caught here, with one exception: reading ”
// in a quote as the quote's end and a new quote is the same as reading it
// as an escaped quote (the statement's bytes do not change), so that
// mutant is equivalent.
func TestDumpRoundTrip(t *testing.T) {
	seeds := 3000
	if testing.Short() {
		seeds = 300
	}
	for _, driver := range []string{MySQL, Postgres} {
		t.Run(driver, func(t *testing.T) {
			t.Parallel()
			failed := 0
			for seed := range uint64(seeds) {
				r := rand.New(rand.NewPCG(seed, 11))
				dc := randDumpCase(t, r, driver)
				if err := dc.check(dumpReaders[seed%3]); err != nil {
					t.Errorf("seed %d: %v\ndump:\n%q", seed, err, dc.dump)
					if failed++; failed == 5 {
						return
					}
				}
			}
		})
	}
}

// FuzzStatements reads any bytes as a dump: no panic, no endless loop, a
// statement for at least every byte it reads. The input also goes into a
// dump as values of a row (split at 0xfe), which must read back as written.
func FuzzStatements(f *testing.F) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		for _, driver := range []string{MySQL, Postgres} {
			f.Add(randDumpCase(f, r, driver).dump, driver == Postgres)
		}
	}
	for _, s := range awkward {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Add([]byte("DELIMITER \nx;y"), false)
	f.Add([]byte("$a$ $a"), true)
	f.Fuzz(func(t *testing.T, data []byte, pg bool) {
		driver := MySQL
		if pg {
			driver = Postgres
		}
		// Any bytes: every call reads at least one byte or ends.
		st := newStatements(bytes.NewReader(data), driver)
		for calls := 0; ; calls++ {
			if calls > len(data)+1 {
				t.Fatalf("%d statements from %d bytes", calls, len(data))
			}
			q, err := st.next()
			if err != nil {
				if err != io.EOF && !errors.Is(err, errUnterminated) {
					t.Fatalf("error %v", err)
				}
				break
			}
			if _, rows, ok := strings.Cut(q, " VALUES\n"); ok {
				for n := 1; n <= 3; n++ {
					parseRows(rows, n, driver, func([][]byte) {})
				}
			}
		}

		// The input as values: read back as written.
		dl, _ := dialectFor(driver)
		parts := bytes.Split(data, []byte{0xfe})
		tb := &Table{Name: "t", quoted: dl.quote("t")}
		row := make([][]byte, len(parts))
		for i, p := range parts {
			c := &Column{Name: fmt.Sprint("c", i), Category: CatText}
			switch {
			case len(p) > 0 && p[0] == 'N':
				p = nil // NULL
			case len(p) > 0 && p[0] == 'X' && driver == MySQL:
				c.Category, c.hexLit = CatBinary, true
			case len(p) > 0 && p[0] == '9':
				c.Category = CatNumeric
			}
			tb.Columns = append(tb.Columns, c)
			row[i] = p
		}
		ins := insertStmt(dl, tb, tb.Columns, [][][]byte{row, row})
		other := Stmt{SQL: "CREATE TABLE " + dl.quote(string(data)) + " (x int)"}
		dc := &dumpCase{
			driver: driver,
			dump:   writeDump(t, driver, []any{"Schema", other, "Table t", ins}),
			stmts:  []string{other.SQL, ins.SQL},
			ncols:  map[int]int{1: len(row)},
			rows:   map[int][][][]byte{1: {row, row}},
			heads:  map[int]string{1: dl.insert(tb, tb.Columns)},
		}
		if err := dc.check(func(r io.Reader) io.Reader { return r }); err != nil {
			t.Fatalf("%v\ndump:\n%q", err, dc.dump)
		}
	})
}

// TestDumpHeader: data verify reads only a dump Anonymize wrote (this
// format or an earlier one, for the same driver), since the tokenizer
// follows how mysql and psql split the statements it writes. A dump of
// another tool is refused, not read in part.
func TestDumpHeader(t *testing.T) {
	ok := []struct{ driver, dump string }{
		{MySQL, dumpHeader(DumpFormat, MySQL)},
		{MySQL, dumpHeader(1, MySQL)},
		{MySQL, dumpHeader(DumpFormat, "mariadb")},
		{"mariadb", dumpHeader(DumpFormat, MySQL)},
		{Postgres, dumpHeader(1, Postgres) + "SET x = 1;\n"},
	}
	for _, c := range ok {
		if err := newStatements(strings.NewReader(c.dump), c.driver).header(c.driver); err != nil {
			t.Errorf("%s %q: %v", c.driver, c.dump, err)
		}
	}
	refused := []struct{ driver, dump string }{
		{MySQL, ""},
		{MySQL, "-- MySQL dump 10.13  Distrib 8.0.36\nINSERT INTO `t` VALUES (1,'a');\n"},
		{Postgres, "--\n-- PostgreSQL database dump\n--\nCOPY public.t (v) FROM stdin;\na\n\\.\n"},
		{Postgres, dumpHeader(DumpFormat, MySQL)},
		{MySQL, dumpHeader(DumpFormat+1, MySQL)},
		{MySQL, "\n" + dumpHeader(DumpFormat, MySQL)},
	}
	for _, c := range refused {
		if err := newStatements(strings.NewReader(c.dump), c.driver).header(c.driver); err == nil {
			t.Errorf("%s %q: accepted", c.driver, c.dump)
		}
	}
	// scanner.dump refuses it before reading a statement.
	sc := (&leakIndex{}).scanner("dump")
	defer sc.close()
	err := sc.dump(strings.NewReader(refused[1].dump), &Schema{Driver: MySQL}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not a dump of driftgate") {
		t.Errorf("scanner.dump: %v", err)
	}
}
