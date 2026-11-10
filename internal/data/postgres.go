package data

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/dbx"
)

// postgresDialect reads and writes PostgreSQL. PostgreSQL has no SHOW
// CREATE, so the schema is rebuilt from the catalogs: schemas, extensions,
// enum types, sequences, functions and tables before the rows; constraints,
// indexes, sequence values, triggers and views after them. Every value is
// read as its text (col::text) and written back as a literal, which the
// server turns into the column's type.
//
// Not supported (the run stops and names them): partitioned and inherited
// tables, materialized views, domains, composite and range types of the
// database itself, aggregates. Not copied: comments, grants, row security
// policies, rules, publications, the ICU locale of the database.
type postgresDialect struct{}

// postgresSession: names always qualified (an empty search_path, as
// pg_dump does), so the definitions read here and written to the target or
// a dump mean the same everywhere; text values in a fixed form.
var postgresSession = []string{
	"SET client_encoding = 'UTF8'",
	"SET standard_conforming_strings = on",
	"SET search_path = ''",
	"SET TimeZone = 'UTC'",
	"SET DateStyle = 'ISO, MDY'",
	"SET IntervalStyle = 'postgres'",
	"SET extra_float_digits = 1",
	"SET bytea_output = 'hex'",
	"SET check_function_bodies = false",
}

func (postgresDialect) quote(name string) string { return dbx.QuotePostgres(name) }

func (postgresDialect) session(ctx context.Context, c *sql.Conn) error {
	for _, s := range postgresSession {
		if _, err := c.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (postgresDialect) snapshot(ctx context.Context, c *sql.Conn) error {
	_, err := c.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY")
	return err
}

// userNS filters the schemas of the database itself.
func userNS(col string) string { return dbx.PostgresUserSchema(col) }

// notExtension filters objects that belong to an extension (created by it):
// CREATE EXTENSION makes them again (dbx.PostgresNotExtension).
func notExtension(oid, class string) string { return dbx.PostgresNotExtension(oid, class) }

func pgCategory(typcategory, typname string) (string, bool) {
	switch {
	case typname == "json" || typname == "jsonb":
		return CatJSON, false
	case typname == "bytea":
		return CatBinary, false
	case typname == "uuid" || typcategory == "S":
		return CatText, false
	case typcategory == "N":
		return CatNumeric, false
	}
	return CatOther, false
}

func (d postgresDialect) schema(ctx context.Context, c *sql.Conn, db string) (*Schema, error) {
	s := &Schema{Driver: Postgres}
	for _, q := range postgresSession {
		s.header = append(s.header, Stmt{SQL: q})
	}
	var unsupported []string
	rows, err := queryStrings(ctx, c, `SELECT c.oid::regclass::text,
			CASE WHEN c.relkind = 'p' THEN 'partitioned table' WHEN c.relkind = 'm' THEN 'materialized view' ELSE 'inherited table' END
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE `+userNS("n.nspname")+` AND `+notExtension("c.oid", "pg_class")+`
		  AND (c.relkind IN ('p', 'm') OR (c.relkind = 'r' AND EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)))
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		unsupported = append(unsupported, r[1]+" "+r[0])
	}
	rows, err = queryStrings(ctx, c, `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE `+userNS("n.nspname")+` AND p.prokind IN ('a', 'w') AND `+notExtension("p.oid", "pg_proc")+` ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		unsupported = append(unsupported, "aggregate "+r[0])
	}

	// Schemas, extensions, enum types.
	rows, err = queryStrings(ctx, c, `SELECT n.nspname FROM pg_namespace n WHERE `+userNS("n.nspname")+` AND n.nspname <> 'public'
		AND `+notExtension("n.oid", "pg_namespace")+` ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		s.before = append(s.before, Stmt{SQL: "CREATE SCHEMA IF NOT EXISTS " + d.quote(r[0])})
	}
	rows, err = queryStrings(ctx, c, `SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname <> 'plpgsql' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		s.before = append(s.before, Stmt{SQL: "CREATE EXTENSION IF NOT EXISTS " + d.quote(r[0]) + " WITH SCHEMA " + d.quote(r[1])})
	}
	rows, err = queryStrings(ctx, c, `SELECT format('%I.%I', n.nspname, t.typname), string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
		FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE `+userNS("n.nspname")+` AND `+notExtension("t.oid", "pg_type")+`
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		s.before = append(s.before, Stmt{SQL: "CREATE TYPE " + r[0] + " AS ENUM (" + r[1] + ")"})
	}

	// Sequences that are not an identity column's own.
	rows, err = queryStrings(ctx, c, `SELECT format('%I.%I', n.nspname, c.relname), format_type(s.seqtypid, NULL), s.seqincrement::text,
			s.seqmin::text, s.seqmax::text, s.seqstart::text, s.seqcache::text, s.seqcycle::text,
			coalesce((SELECT format('%s.%I', d.refobjid::regclass::text, a.attname) FROM pg_depend d
				JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
				WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a' LIMIT 1), ''),
			coalesce((SELECT d.refobjid::regclass::text FROM pg_depend d
				WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a' LIMIT 1), '')
		FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE `+userNS("n.nspname")+` AND `+notExtension("c.oid", "pg_class")+`
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = c.oid AND d.classid = 'pg_class'::regclass AND d.deptype = 'i')
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var ownedBy []Stmt
	seqTable := map[string]string{} // sequence → owning table (regclass text)
	for _, r := range rows {
		cycle := " NO CYCLE"
		if r[7] == "true" {
			cycle = " CYCLE"
		}
		s.before = append(s.before, Stmt{SQL: fmt.Sprintf("CREATE SEQUENCE %s AS %s INCREMENT BY %s MINVALUE %s MAXVALUE %s START WITH %s CACHE %s%s",
			r[0], r[1], r[2], r[3], r[4], r[5], r[6], cycle)})
		if r[8] != "" {
			ownedBy = append(ownedBy, Stmt{SQL: "ALTER SEQUENCE " + r[0] + " OWNED BY " + r[8], uses: []string{r[9]}})
			seqTable[r[0]] = r[9]
		}
		name := r[0]
		s.sequences = append(s.sequences, sequence{name: name, read: "SELECT last_value::text, is_called::text FROM " + name,
			set: func(v []string) []string {
				return []string{fmt.Sprintf("SELECT pg_catalog.setval(%s, %s, %s)", quoteLiteral(name), v[0], v[1])}
			}})
	}

	// Functions (procedures too) before the tables: a default or a check may call one.
	rows, err = queryStrings(ctx, c, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE `+userNS("n.nspname")+` AND p.prokind IN ('f', 'p') AND `+notExtension("p.oid", "pg_proc")+`
		ORDER BY n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		s.before = append(s.before, Stmt{SQL: strings.TrimSpace(r[0])})
	}

	// Tables and columns.
	rows, err = queryStrings(ctx, c, `SELECT c.oid::regclass::text, n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND `+userNS("n.nspname")+` AND `+notExtension("c.oid", "pg_class")+`
		  AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
		ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	byReg := map[string]*Table{}
	for _, r := range rows {
		name := r[2]
		if r[1] != "public" {
			name = r[1] + "." + r[2]
		}
		t := &Table{Name: name, quoted: d.quote(r[1]) + "." + d.quote(r[2])}
		s.Tables = append(s.Tables, t)
		byReg[r[0]] = t
	}
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	display := func(reg string) string {
		if t := byReg[reg]; t != nil {
			return t.Name
		}
		return reg
	}
	for i, st := range ownedBy {
		ownedBy[i].uses = []string{display(st.uses[0])}
	}
	for i := range s.sequences {
		if t, ok := seqTable[s.sequences[i].name]; ok {
			s.sequences[i].uses = []string{display(t)}
		}
	}

	rows, err = queryStrings(ctx, c, `SELECT a.attrelid::regclass::text, a.attname, format_type(a.atttypid, a.atttypmod), (NOT a.attnotnull)::text,
			a.attidentity::text, a.attgenerated::text, coalesce(pg_get_expr(ad.adbin, ad.adrelid), ''),
			coalesce((SELECT format('%I.%I', cn.nspname, co.collname) FROM pg_collation co JOIN pg_namespace cn ON cn.oid = co.collnamespace
				WHERE co.oid = a.attcollation AND a.attcollation <> t.typcollation), ''),
			t.typcategory::text, t.typname,
			(bn.nspname NOT IN ('pg_catalog', 'information_schema') AND b.typtype <> 'e' AND NOT EXISTS (SELECT 1 FROM pg_depend e
				WHERE e.objid = b.oid AND e.classid = 'pg_type'::regclass AND e.deptype = 'e'))::text,
			coalesce((SELECT (NOT co.collisdeterministic)::text FROM pg_collation co WHERE co.oid = a.attcollation), 'false')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		JOIN pg_type b ON b.oid = CASE WHEN t.typcategory = 'A' AND t.typelem <> 0 THEN t.typelem ELSE t.oid END
		JOIN pg_namespace bn ON bn.oid = b.typnamespace
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE c.relkind = 'r' AND `+userNS("n.nspname")+` AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1, a.attnum`)
	if err != nil {
		return nil, err
	}
	defs := map[*Table][]string{}
	for _, r := range rows {
		t := byReg[r[0]]
		if t == nil {
			continue
		}
		if r[10] == "true" {
			unsupported = append(unsupported, fmt.Sprintf("column %s.%s of type %s (domain, composite or range type)", t.Name, r[1], r[2]))
		}
		cat, _ := pgCategory(r[8], r[9])
		col := &Column{Name: r[1], Type: r[2], Category: cat, Nullable: r[3] == "true", Generated: r[5] == "s",
			Collation: r[7], nondeterministic: r[11] == "true"}
		t.Columns = append(t.Columns, col)
		def := d.quote(r[1]) + " " + r[2]
		if r[7] != "" {
			def += " COLLATE " + r[7]
		}
		switch {
		case r[5] == "s":
			def += " GENERATED ALWAYS AS (" + r[6] + ") STORED"
		case r[4] == "a":
			def += " GENERATED ALWAYS AS IDENTITY"
			t.identity = true
		case r[4] == "d":
			def += " GENERATED BY DEFAULT AS IDENTITY"
		case r[6] != "":
			def += " DEFAULT " + r[6]
		}
		if r[3] != "true" {
			def += " NOT NULL"
		}
		defs[t] = append(defs[t], def)
	}
	for _, t := range s.Tables {
		t.create = []string{"DROP TABLE IF EXISTS " + t.quoted + " CASCADE",
			"CREATE TABLE " + t.quoted + " (\n    " + strings.Join(defs[t], ",\n    ") + "\n)"}
	}
	if len(unsupported) > 0 {
		return nil, fmt.Errorf("%w:\n  %s", ErrUnsupported, strings.Join(unsupported, "\n  "))
	}

	// Constraints: keys first, references last (they need the keys).
	const sep = "\x1f"
	rows, err = queryStrings(ctx, c, `SELECT con.conrelid::regclass::text, con.conname, con.contype::text, pg_get_constraintdef(con.oid),
			coalesce(con.confrelid::regclass::text, ''),
			array_to_string(ARRAY(SELECT a.attname FROM unnest(con.conkey) WITH ORDINALITY k(n, o)
				JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n ORDER BY k.o), E'\x1f'),
			coalesce(array_to_string(ARRAY(SELECT a.attname FROM unnest(con.confkey) WITH ORDINALITY k(n, o)
				JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.n ORDER BY k.o), E'\x1f'), '')
		FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE con.contype IN ('p', 'u', 'x', 'c', 'f') AND c.relkind = 'r' AND `+userNS("n.nspname")+`
		ORDER BY CASE con.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 WHEN 'x' THEN 2 WHEN 'c' THEN 3 ELSE 4 END, 1, 2`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t := byReg[r[0]]
		if t == nil {
			continue
		}
		cols := strings.Split(r[5], sep)
		uses := []string{t.Name}
		switch r[2] {
		case "p":
			t.PrimaryKey = cols
		case "u":
			if len(cols) == 1 {
				if col := t.Column(cols[0]); col != nil {
					col.Unique = true
				}
			}
		case "f":
			ref := display(r[4])
			uses = append(uses, ref)
			t.ForeignKeys = append(t.ForeignKeys, ForeignKey{Name: r[1], Table: t.Name, Columns: cols, RefTable: ref, RefColumns: strings.Split(r[6], sep)})
		}
		s.after = append(s.after, Stmt{SQL: "ALTER TABLE " + t.quoted + " ADD CONSTRAINT " + d.quote(r[1]) + " " + r[3], uses: uses})
	}
	for _, t := range s.Tables {
		if len(t.PrimaryKey) == 1 {
			if col := t.Column(t.PrimaryKey[0]); col != nil {
				col.Unique = true
			}
		}
	}

	// Indexes that no key constraint made.
	rows, err = queryStrings(ctx, c, `SELECT i.indrelid::regclass::text, pg_get_indexdef(i.indexrelid)
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND `+userNS("n.nspname")+`
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid AND con.conrelid = i.indrelid AND con.contype IN ('p', 'u', 'x'))
		ORDER BY 1, ic.relname`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if t := byReg[r[0]]; t != nil {
			s.after = append(s.after, Stmt{SQL: r[1], uses: []string{t.Name}})
		}
	}
	s.after = append(s.after, ownedBy...)

	// Identity columns continue where the source does.
	rows, err = queryStrings(ctx, c, `SELECT d.refobjid::regclass::text, a.attname, format('%I.%I', sn.nspname, sc.relname)
		FROM pg_depend d JOIN pg_class sc ON sc.oid = d.objid JOIN pg_namespace sn ON sn.oid = sc.relnamespace
		JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'i' AND sc.relkind = 'S'
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		t := byReg[r[0]]
		if t == nil {
			continue
		}
		target := fmt.Sprintf("pg_catalog.pg_get_serial_sequence(%s, %s)", quoteLiteral(t.quoted), quoteLiteral(r[1]))
		s.sequences = append(s.sequences, sequence{name: r[2], read: "SELECT last_value::text, is_called::text FROM " + r[2], uses: []string{t.Name},
			set: func(v []string) []string {
				return []string{fmt.Sprintf("SELECT pg_catalog.setval(%s, %s, %s)", target, v[0], v[1])}
			}})
	}

	// Triggers, after the rows so the copy does not fire them.
	rows, err = queryStrings(ctx, c, `SELECT t.tgrelid::regclass::text, pg_get_triggerdef(t.oid)
		FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT t.tgisinternal AND c.relkind = 'r' AND `+userNS("n.nspname")+` ORDER BY 1, t.tgname`)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if t := byReg[r[0]]; t != nil {
			s.after = append(s.after, Stmt{SQL: r[1], uses: []string{t.Name}})
		}
	}

	// Views, each after the views it reads.
	rows, err = queryStrings(ctx, c, `SELECT c.oid::regclass::text, pg_get_viewdef(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'v' AND `+userNS("n.nspname")+` AND `+notExtension("c.oid", "pg_class")+` ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	var views []string
	viewDefs := map[string]string{}
	for _, r := range rows {
		views = append(views, r[0])
		viewDefs[r[0]] = strings.TrimSuffix(strings.TrimSpace(r[1]), ";")
	}
	deps, err := queryStrings(ctx, c, `SELECT DISTINCT v.oid::regclass::text, d.refobjid::regclass::text
		FROM pg_rewrite rw JOIN pg_class v ON v.oid = rw.ev_class
		JOIN pg_depend d ON d.objid = rw.oid AND d.classid = 'pg_rewrite'::regclass AND d.refclassid = 'pg_class'::regclass
		WHERE v.relkind = 'v' AND d.refobjid <> v.oid ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	reads := map[string]map[string]bool{}
	for _, r := range deps {
		if reads[r[0]] == nil {
			reads[r[0]] = map[string]bool{}
		}
		reads[r[0]][r[1]] = true
	}
	ordered := viewOrder(views, func(v, other string) bool { return reads[v][other] })
	for _, v := range ordered {
		var uses []string
		for dep := range reads[v] {
			if t := byReg[dep]; t != nil {
				uses = append(uses, t.Name)
			}
		}
		sort.Strings(uses)
		s.after = append(s.after, Stmt{SQL: "CREATE VIEW " + v + " AS\n" + viewDefs[v], uses: uses, view: v})
	}
	return s, nil
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func (d postgresDialect) selectRows(t *Table, cols []*Column, where string) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = d.quote(c.Name) + "::text"
	}
	q := "SELECT " + strings.Join(names, ", ") + " FROM " + t.quoted
	if where != "" {
		q += " WHERE " + where
	}
	return q + " ORDER BY " + strings.Join(orderBy(t, func(name string, pk bool) string {
		// Qualified: a bare name would mean the text column of the result.
		if pk {
			return t.quoted + "." + d.quote(name)
		}
		return t.quoted + "." + d.quote(name) + `::text COLLATE "C"`
	}), ", ")
}

func (d postgresDialect) insert(t *Table, cols []*Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = d.quote(c.Name)
	}
	q := "INSERT INTO " + t.quoted + " (" + strings.Join(names, ", ") + ")"
	if t.identity {
		q += " OVERRIDING SYSTEM VALUE"
	}
	return q + " VALUES"
}

func (postgresDialect) literal(c *Column, v []byte) string {
	switch {
	case v == nil:
		return "NULL"
	case c.Category == CatNumeric && plainNumber().Match(v):
		return string(v)
	}
	return quoteLiteral(string(v))
}

// identity: the system identifier of the cluster (made by initdb, kept by
// its copies) and the port it listens on.
func (postgresDialect) identity(ctx context.Context, db *sql.DB) (string, error) {
	var id string
	err := db.QueryRowContext(ctx, "SELECT system_identifier::text || ' ' || current_setting('port') FROM pg_catalog.pg_control_system()").Scan(&id)
	return "postgres " + id, err
}

func (d postgresDialect) inspect(ctx context.Context, admin *sql.DB, dst Database) (dbState, error) {
	var st dbState
	var adminName string
	if err := admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&adminName); err != nil {
		return st, err
	}
	if adminName == dst.Name {
		return st, fmt.Errorf("the database in the target DSN (%s) creates and drops %s, so it must be another one, e.g. postgres", adminName, dst.Name)
	}
	var comment string
	err := admin.QueryRowContext(ctx, "SELECT coalesce(pg_catalog.shobj_description(oid, 'pg_database'), '') FROM pg_catalog.pg_database WHERE datname = $1", dst.Name).Scan(&comment)
	if err == sql.ErrNoRows {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.exists, st.marked = true, strings.HasPrefix(comment, markerComment)
	tdb, err := open(dst, dst.Name)
	if err != nil {
		return st, err
	}
	defer tdb.Close()
	// Relations (tables, views, sequences, foreign tables…), routines, types,
	// schemas and extensions of the database itself.
	err = tdb.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE `+userNS("n.nspname")+` AND `+notExtension("c.oid", "pg_class")+`)
		+ (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			WHERE `+userNS("n.nspname")+` AND `+notExtension("p.oid", "pg_proc")+`)
		+ (SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
			WHERE `+userNS("n.nspname")+` AND t.typtype IN ('e', 'd', 'r', 'm') AND `+notExtension("t.oid", "pg_type")+`)
		+ (SELECT count(*) FROM pg_catalog.pg_namespace n WHERE `+userNS("n.nspname")+` AND n.nspname <> 'public'
			AND `+notExtension("n.oid", "pg_namespace")+`)
		+ (SELECT count(*) FROM pg_catalog.pg_extension WHERE extname <> 'plpgsql')`).Scan(&st.objects)
	return st, err
}

func (d postgresDialect) create(ctx context.Context, admin *sql.DB, src Database, name string) error {
	srcAdmin, err := open(src, "")
	if err != nil {
		return err
	}
	defer srcAdmin.Close()
	var enc, collate, ctype string
	err = srcAdmin.QueryRowContext(ctx, "SELECT pg_encoding_to_char(encoding), datcollate, datctype FROM pg_database WHERE datname = $1", src.Name).Scan(&enc, &collate, &ctype)
	if err == sql.ErrNoRows {
		return fmt.Errorf("source database %s does not exist", src.Name)
	}
	if err != nil {
		return err
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+d.quote(name)+" TEMPLATE template0 ENCODING "+quoteLiteral(enc)+
		" LC_COLLATE "+quoteLiteral(collate)+" LC_CTYPE "+quoteLiteral(ctype)); err != nil {
		return err
	}
	_, err = admin.ExecContext(ctx, "COMMENT ON DATABASE "+d.quote(name)+" IS "+quoteLiteral(markerNote))
	return err
}

// publish gives the staging database the target's name. RENAME needs it
// to have no other sessions: the run's own are closed, and the server
// waits for their processes to end; any others are ended.
func (d postgresDialect) publish(ctx context.Context, admin *sql.DB, staging, target Database) error {
	return dbx.PostgresAlone(ctx, admin, staging.Name, "ALTER DATABASE "+d.quote(staging.Name)+" RENAME TO "+d.quote(target.Name))
}
