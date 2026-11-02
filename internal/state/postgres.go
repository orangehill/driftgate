package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/dbdsn"
	"github.com/orangehill/driftgate/internal/dbx"
)

// Postgres is a PostgreSQL database. The pristine copy is a second database
// on the same server; a restore recreates the working database from it as a
// template, which PostgreSQL does by copying files, so it is fast even for a
// large database. PostgreSQL cannot read across databases, so a restore always
// covers the whole database (Partial is false).
type Postgres struct {
	o     Options
	admin *sql.DB // the database named in the DSN, for CREATE and DROP DATABASE
	// dropping ends with the drop of the database the last copy replaced
	// (copyDatabase); nil when there is none to wait for.
	dropping chan error
}

// NewPostgres connects to the server. The DSN is a postgres:// URL; its
// database (default postgres) is used only to create and drop databases.
func NewPostgres(o Options) (*Postgres, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	u, err := dbdsn.PostgresURL(o.DSN)
	if err != nil {
		return nil, err
	}
	if admin := strings.Trim(u.Path, "/"); admin == o.Working || admin == o.Pristine {
		return nil, fmt.Errorf("dsn: the database in the URL (%s) is used to create and drop %s and %s, so it must be another one, e.g. postgres", admin, o.Working, o.Pristine)
	}
	admin, err := dbx.OpenPostgres(o.DSN, "", o.Dial)
	if err != nil {
		return nil, err
	}
	return &Postgres{o: o, admin: admin}, nil
}

func (p *Postgres) Name() string  { return p.o.Name }
func (p *Postgres) Partial() bool { return false }

// Close waits for the drop of the database the last copy replaced.
func (p *Postgres) Close() error {
	p.wait()
	return p.admin.Close()
}

func qp(name string) string { return dbx.QuotePostgres(name) }

func (p *Postgres) open(db string) (*sql.DB, error) {
	d, err := dbx.OpenPostgres(p.o.DSN, db, p.o.Dial)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	return d, nil
}

func (p *Postgres) excludedQualified(name string) bool {
	_, table, _ := strings.Cut(name, ".")
	return p.o.excluded(name) || p.o.excluded(table)
}

// Fingerprint hashes the working database. A working database that does not
// exist has no items, so a restore brings back every item of the pristine
// copy.
func (p *Postgres) Fingerprint(ctx context.Context, items []string) (Fingerprint, error) {
	return p.FingerprintAs(ctx, FingerprintFormat, items)
}

// FingerprintAs hashes items of the working database as a fingerprint of
// the given format made them (Formatter): format 1 had no row security,
// policies, rules, privileges, comments, column collations, UNLOGGED or
// replica identity in a table's definition, no rules or privileges in a
// view's, only the last value of a sequence, and no functions, types,
// schemas or materialized views.
func (p *Postgres) FingerprintAs(ctx context.Context, format int, items []string) (Fingerprint, error) {
	f, err := p.fingerprint(ctx, p.o.Working, items, format)
	if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "3D000" { // invalid_catalog_name
		return Fingerprint{}, nil
	}
	return f, err
}

// schemaFilter keeps the schemas of the database itself
// (dbx.PostgresUserSchema). Objects of extensions are fingerprinted too:
// their rows can change, and a restore copies the whole database.
func schemaFilter(col string) string { return dbx.PostgresUserSchema(col) }

func (p *Postgres) fingerprint(ctx context.Context, db string, items []string, format int) (Fingerprint, error) {
	d, err := p.open(db)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	want := func(item string) bool { return items == nil || contains(items, item) }

	// Definitions: columns, constraints, indexes and triggers per table.
	// What only some tables or columns have is added only where they have
	// it, so the hash of any other table stays what it was (format 1).
	defs := map[string]*strings.Builder{}
	add := func(rel, text string) {
		if defs[rel] == nil {
			defs[rel] = &strings.Builder{}
		}
		defs[rel].WriteString(text + "\n")
	}
	// A column's collation (format_type leaves it out), privileges and
	// comment, where it has them (format 2).
	column := ` ||
		        coalesce(' collate ' || quote_ident(cn.nspname) || '.' || quote_ident(co.collname), '') ||
		        coalesce(' acl ' || a.attacl::text, '') || coalesce(' comment ' || quote_literal(ds.description), '')`
	if format < 2 {
		column = ""
	}
	queries := []string{
		`SELECT n.nspname || '.' || c.relname, a.attname || ' ' || format_type(a.atttypid, a.atttypmod) || ' ' ||
		        coalesce(pg_get_expr(d.adbin, d.adrelid), '') || ' ' || a.attnotnull::text || ' ' || a.attidentity::text || ' ' || a.attgenerated::text` + column + `
		   FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		   JOIN pg_type ty ON ty.oid = a.atttypid
		   LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		   LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> ty.typcollation
		   LEFT JOIN pg_namespace cn ON cn.oid = co.collnamespace
		   LEFT JOIN pg_description ds ON ds.objoid = a.attrelid AND ds.classoid = 'pg_class'::regclass AND ds.objsubid = a.attnum
		  WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p') AND ` + schemaFilter("n.nspname") + `
		  ORDER BY 1, a.attnum`,
		`SELECT n.nspname || '.' || c.relname, 'constraint ' || con.conname || ' ' || pg_get_constraintdef(con.oid)
		   FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE ` + schemaFilter("n.nspname") + ` ORDER BY 1, 2`,
		`SELECT schemaname || '.' || tablename, 'index ' || indexdef FROM pg_indexes WHERE ` + schemaFilter("schemaname") + ` ORDER BY 1, 2`,
		`SELECT n.nspname || '.' || c.relname, 'trigger ' || pg_get_triggerdef(t.oid)
		   FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE NOT t.tgisinternal AND ` + schemaFilter("n.nspname") + ` ORDER BY 1, 2`,
		// Row security and its policies, only where a table has them, so
		// the hash of any other table stays what it was.
		`SELECT n.nspname || '.' || c.relname, 'row security ' || c.relrowsecurity::text || ' ' || c.relforcerowsecurity::text
		   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE (c.relrowsecurity OR c.relforcerowsecurity) AND ` + schemaFilter("n.nspname") + ` ORDER BY 1, 2`,
		`SELECT schemaname || '.' || tablename, 'policy ' || policyname || ' ' || permissive || ' ' || roles::text || ' ' || cmd || ' ' ||
		        coalesce(qual, '') || ' ' || coalesce(with_check, '')
		   FROM pg_policies WHERE ` + schemaFilter("schemaname") + ` ORDER BY 1, 2`,
		// Rules (CREATE RULE) of tables and views (a view's own rule is its
		// definition); and of tables and views, UNLOGGED, the replica
		// identity, privileges and the comment where they are not the
		// default. One query: most databases have none of them.
		`SELECT n.nspname || '.' || c.relname, 'rule ' || pg_get_ruledef(r.oid)
		   FROM pg_rewrite r JOIN pg_class c ON c.oid = r.ev_class JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE r.rulename <> '_RETURN' AND ` + schemaFilter("n.nspname") + `
		 UNION ALL
		 SELECT n.nspname || '.' || c.relname, 'relation ' || c.relpersistence::text || ' ' || c.relreplident::text ||
		        coalesce(' acl ' || c.relacl::text, '') || coalesce(' comment ' || quote_literal(ds.description), '')
		   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		   LEFT JOIN pg_description ds ON ds.objoid = c.oid AND ds.classoid = 'pg_class'::regclass AND ds.objsubid = 0
		  WHERE c.relkind IN ('r', 'p', 'v') AND (c.relpersistence <> 'p' OR (c.relkind <> 'v' AND c.relreplident <> 'd') OR c.relacl IS NOT NULL OR ds.description IS NOT NULL)
		    AND ` + schemaFilter("n.nspname") + ` ORDER BY 1, 2`,
	}
	if format < 2 {
		queries = queries[:4] // no row security, policies, rules, privileges…
	}
	for _, q := range queries {
		rows, err := d.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rel, text string
			if err := rows.Scan(&rel, &text); err != nil {
				rows.Close()
				return nil, err
			}
			add(rel, text)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	f := Fingerprint{}
	tables, err := names(ctx, d, `SELECT schemaname || '.' || tablename FROM pg_tables WHERE `+schemaFilter("schemaname")+` ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		if p.excludedQualified(t) || !want("table:"+t) {
			continue
		}
		schema, name, _ := strings.Cut(t, ".")
		var count int64
		var sum string
		q := `SELECT count(*), coalesce(md5(string_agg(md5(r::text), '' ORDER BY md5(r::text))), '') FROM ` + qp(schema) + "." + qp(name) + " AS r"
		if err := d.QueryRowContext(ctx, q).Scan(&count, &sum); err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		def := ""
		if defs[t] != nil {
			def = defs[t].String()
		}
		f["table:"+t] = hash([]byte(fmt.Sprintf("%s\x00%d\x00%s", def, count, sum)))
	}
	rows, err := d.QueryContext(ctx, `SELECT schemaname || '.' || viewname, definition FROM pg_views WHERE `+schemaFilter("schemaname"))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v, def string
		if err := rows.Scan(&v, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if !p.excludedQualified(v) && want("view:"+v) {
			if defs[v] != nil {
				def += "\x00" + defs[v].String()
			}
			f["view:"+v] = hash([]byte(def))
		}
	}
	rows.Close()
	if err := p.sequences(ctx, d, f, want, format); err != nil {
		return nil, err
	}
	if format < 2 {
		return f, nil
	}
	if err := p.schemaItems(ctx, d, f, want); err != nil {
		return nil, err
	}
	return f, nil
}

// sequences hashes the sequences: the last value, or "not called", and,
// where they are not those CREATE SEQUENCE gives a sequence of its type,
// its parameters (ALTER SEQUENCE … INCREMENT, MAXVALUE…) and the value
// setval(…, false) left for the first nextval. (Its type alone is left
// out: a sequence of serial and one of CREATE SEQUENCE differ in it, and
// their hash stays that of format 1.) pg_sequences shows no
// value for a sequence not called yet; that value is read from the
// sequence itself, in one query for all of them.
// Format 1 has the last value only.
func (p *Postgres) sequences(ctx context.Context, d *sql.DB, f Fingerprint, want func(string) bool, format int) error {
	params, own := "''", "NULL"
	if format >= 2 {
		params = `CASE WHEN start_value = 1 AND min_value = 1 AND increment_by = 1 AND NOT cycle AND cache_size = 1 AND
		                 max_value = CASE data_type WHEN 'smallint'::regtype THEN 32767 WHEN 'integer'::regtype THEN 2147483647 ELSE 9223372036854775807 END
		            THEN '' ELSE format(' as %s start %s min %s max %s increment %s cycle %s cache %s', data_type, start_value, min_value, max_value, increment_by, cycle, cache_size) END`
		// The sequence's own value when not called, if it is not its start.
		own = `CASE WHEN last_value IS NULL THEN format('SELECT %L, %s FROM %I.%I WHERE last_value <> %s',
		            schemaname || '.' || sequencename, 'last_value', schemaname, sequencename, start_value) END`
	}
	rows, err := d.QueryContext(ctx, `SELECT schemaname || '.' || sequencename, coalesce(last_value::text, 'not called') || `+params+`, `+own+`
		  FROM pg_sequences WHERE `+schemaFilter("schemaname"))
	if err != nil {
		return err
	}
	var unused []string // not called yet: SELECTs of their own value
	for rows.Next() {
		var s, v string
		var q sql.NullString
		if err := rows.Scan(&s, &v, &q); err != nil {
			rows.Close()
			return err
		}
		if p.excludedQualified(s) || !want("sequence:"+s) {
			continue
		}
		f["sequence:"+s] = v // hashed below, with the value of unused
		if q.Valid {
			unused = append(unused, q.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(unused) > 0 {
		rows, err := d.QueryContext(ctx, strings.Join(unused, " UNION ALL "))
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			var last int64
			if err := rows.Scan(&s, &last); err != nil {
				rows.Close()
				return err
			}
			f["sequence:"+s] += fmt.Sprintf(" next %d", last)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for item, v := range f {
		if strings.HasPrefix(item, "sequence:") {
			f[item] = hash([]byte(v))
		}
	}
	return nil
}

// schemaItems hashes the objects of the schema that hold no rows of a
// table but change what the application gets: functions and procedures
// (CREATE OR REPLACE), enum types with their labels in order (ALTER TYPE …
// ADD VALUE), domains with their constraints, composite types with their
// attributes (ALTER TYPE … ADD ATTRIBUTE), schemas other than public, and
// materialized views with their rows (REFRESH). Objects of extensions
// are left out: CREATE EXTENSION makes them, and a library such as PostGIS
// has thousands.
func (p *Postgres) schemaItems(ctx context.Context, d *sql.DB, f Fingerprint, want func(string) bool) error {
	queries := []string{
		`SELECT 'function:' || n.nspname || '.' || f.proname || '(' || pg_get_function_identity_arguments(f.oid) || ')', pg_get_functiondef(f.oid)
		   FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace
		  WHERE f.prokind IN ('f', 'p', 'w') AND ` + schemaFilter("n.nspname") + ` AND ` + dbx.PostgresNotExtension("f.oid", "pg_proc"),
		`SELECT 'type:' || n.nspname || '.' || t.typname, 'enum ' || string_agg(quote_literal(e.enumlabel), ', ' ORDER BY e.enumsortorder)
		   FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_enum e ON e.enumtypid = t.oid
		  WHERE ` + schemaFilter("n.nspname") + ` AND ` + dbx.PostgresNotExtension("t.oid", "pg_type") + ` GROUP BY 1`,
		`SELECT 'type:' || n.nspname || '.' || t.typname, 'domain ' || format_type(t.typbasetype, t.typtypmod) || ' ' || t.typnotnull::text || ' ' ||
		        coalesce(t.typdefault, '') || ' ' || coalesce((SELECT collname FROM pg_collation WHERE oid = t.typcollation), '') || ' ' ||
		        coalesce((SELECT string_agg(c.conname || ' ' || pg_get_constraintdef(c.oid), ', ' ORDER BY c.conname) FROM pg_constraint c WHERE c.contypid = t.oid), '')
		   FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		  WHERE t.typtype = 'd' AND ` + schemaFilter("n.nspname") + ` AND ` + dbx.PostgresNotExtension("t.oid", "pg_type"),
		`SELECT 'type:' || n.nspname || '.' || t.typname, 'composite ' || string_agg(quote_ident(a.attname) || ' ' || format_type(a.atttypid, a.atttypmod) ||
		        CASE WHEN a.attcollation <> ty.typcollation THEN ' collate ' || (SELECT quote_ident(cn.nspname) || '.' || quote_ident(co.collname)
		          FROM pg_collation co JOIN pg_namespace cn ON cn.oid = co.collnamespace WHERE co.oid = a.attcollation) ELSE '' END, ', ' ORDER BY a.attnum)
		   FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace JOIN pg_class c ON c.oid = t.typrelid AND c.relkind = 'c'
		   JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped JOIN pg_type ty ON ty.oid = a.atttypid
		  WHERE ` + schemaFilter("n.nspname") + ` AND ` + dbx.PostgresNotExtension("t.oid", "pg_type") + ` GROUP BY 1`,
		`SELECT 'schema:' || n.nspname, '' FROM pg_namespace n
		  WHERE n.nspname <> 'public' AND ` + schemaFilter("n.nspname") + ` AND ` + dbx.PostgresNotExtension("n.oid", "pg_namespace"),
		`SELECT 'matview:' || schemaname || '.' || matviewname, definition || ' ' || ispopulated::text FROM pg_matviews WHERE ` + schemaFilter("schemaname"),
	}
	defs := map[string]string{}
	for _, q := range queries {
		rows, err := d.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item, def string
			if err := rows.Scan(&item, &def); err != nil {
				rows.Close()
				return err
			}
			if !want(item) {
				continue
			}
			kind, name, _ := strings.Cut(item, ":")
			if kind == "matview" && p.excludedQualified(name) {
				continue
			}
			defs[item] = def
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for item, def := range defs {
		kind, name, _ := strings.Cut(item, ":")
		sum := ""
		if kind == "matview" {
			sum = "not populated"
			if strings.HasSuffix(def, " true") {
				schema, rel, _ := strings.Cut(name, ".")
				var count int64
				var rowsSum string
				q := `SELECT count(*), coalesce(md5(string_agg(md5(r::text), '' ORDER BY md5(r::text))), '') FROM ` + qp(schema) + "." + qp(rel) + " AS r"
				if err := d.QueryRowContext(ctx, q).Scan(&count, &rowsSum); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				sum = fmt.Sprintf("%d\x00%s", count, rowsSum)
			}
		}
		f[item] = hash([]byte(def + "\x00" + sum))
	}
	return nil
}

// pgLiteral quotes a string for PostgreSQL (standard_conforming_strings).
func pgLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func names(ctx context.Context, d *sql.DB, q string) ([]string, error) {
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) exists(ctx context.Context, db string) (bool, error) {
	var n int
	err := p.admin.QueryRowContext(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", db).Scan(&n)
	return n > 0, err
}

// copyDatabase replaces dst with a copy of src. The copy is made under
// another name (newName) and takes dst's name only once it is complete, so
// a failed or interrupted copy leaves dst as it was. prepare, when not nil,
// runs on the finished copy (Seed's mark); replacing, when not nil, right
// before dst gives up its name (Options.Replacing).
//
// The copy keeps what PostgreSQL does not copy from a template: dst's
// owner, settings, privileges, comment and connection limit (see
// pgDatabase). Other sessions on dst are ended (the application
// reconnects); src must have none, which is why the pristine database does
// not accept connections.
//
// dst is renamed away (oldName) and the copy renamed to dst; the old
// database is dropped after copyDatabase returns, in the background, and
// the next copy or Close waits for it. From PostgreSQL 15 DROP DATABASE
// forces a checkpoint, which writes every page of a copy made just before
// (STRATEGY WAL_LOG leaves them in shared buffers): dropping first was
// unsafe, and dropping after the copy made a restore of 22 MB 58 → 123 ms;
// in the background the restore takes as long as one of v1.2.
func (p *Postgres) copyDatabase(ctx context.Context, src, dst string, prepare func(tmp string) error, replacing func() error) error {
	p.wait()
	tmp, old := newName(dst), oldName(dst)
	// Left by a copy that was killed or whose drop failed.
	left, err := names(ctx, p.admin, "SELECT datname FROM pg_database WHERE datname IN ("+pgLiteral(tmp)+", "+pgLiteral(old)+")")
	if err != nil {
		return err
	}
	// dst's properties; when a copy was killed between the two renames,
	// those of the database it renamed away.
	keep, err := p.database(ctx, dst)
	if err == nil && keep == nil {
		keep, err = p.database(ctx, old)
	}
	if err != nil {
		return err
	}
	for _, db := range left {
		if _, err := p.admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qp(db)+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	create := "CREATE DATABASE " + qp(tmp) + " TEMPLATE " + qp(src)
	if keep != nil {
		create += " OWNER " + qp(keep.Owner)
		if keep.ConnLimit != -1 {
			create += fmt.Sprintf(" CONNECTION LIMIT %d", keep.ConnLimit)
		}
	}
	if p.fileCopy(ctx, src) {
		create += " STRATEGY FILE_COPY"
	}
	for attempt := 0; attempt < 5; attempt++ {
		// Sessions on the source block the copy; end them and try again.
		p.admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", src)
		if _, err = p.admin.ExecContext(ctx, create); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err == nil && keep != nil {
		err = p.keep(ctx, tmp, keep)
	}
	if err == nil && prepare != nil {
		err = prepare(tmp)
	}
	if err == nil {
		chaos.Point("state.copy")
		if replacing != nil {
			err = replacing()
		}
	}
	if err == nil {
		err = p.swap(ctx, tmp, dst, old, keep)
	}
	if err != nil {
		p.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+qp(tmp)+" WITH (FORCE)")
		return err
	}
	done := make(chan error, 1)
	p.dropping = done
	go func() {
		_, err := p.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+qp(old)+" WITH (FORCE)")
		done <- err
	}()
	return nil
}

// swap gives tmp the name of dst, whose database is renamed to old. dst
// stops accepting connections first, so no session can start between the
// end of the others and the rename (RENAME has no FORCE).
func (p *Postgres) swap(ctx context.Context, tmp, dst, old string, keep *pgDatabase) error {
	if keep != nil && keep.name == dst {
		if _, err := p.admin.ExecContext(ctx, "ALTER DATABASE "+qp(dst)+" ALLOW_CONNECTIONS false"); err != nil {
			return err
		}
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			p.admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dst)
			if _, err = p.admin.ExecContext(ctx, "ALTER DATABASE "+qp(dst)+" RENAME TO "+qp(old)); err == nil {
				break
			}
			if pe := (*pgconn.PgError)(nil); !errors.As(err, &pe) || pe.Code != "55006" { // object_in_use
				break
			}
		}
		if err != nil {
			p.admin.ExecContext(context.Background(), "ALTER DATABASE "+qp(dst)+" ALLOW_CONNECTIONS "+strconv.FormatBool(keep.AllowConn))
			return err
		}
	}
	chaos.Point("state.swap")
	_, err := p.admin.ExecContext(ctx, "ALTER DATABASE "+qp(tmp)+" RENAME TO "+qp(dst))
	if err != nil && keep != nil && keep.name == dst {
		bg := context.Background()
		if _, rerr := p.admin.ExecContext(bg, "ALTER DATABASE "+qp(old)+" RENAME TO "+qp(dst)); rerr == nil {
			p.admin.ExecContext(bg, "ALTER DATABASE "+qp(dst)+" ALLOW_CONNECTIONS "+strconv.FormatBool(keep.AllowConn))
		}
	}
	return err
}

// wait waits for the drop of the database the last copy replaced.
func (p *Postgres) wait() {
	if p.dropping == nil {
		return
	}
	if err := <-p.dropping; err != nil {
		fmt.Fprintf(p.o.Log, "state %s: drop of the replaced database: %v (the next copy drops it)\n", p.o.Name, err)
	}
	p.dropping = nil
}

// pgDatabase is what a copy keeps of the database it replaces: CREATE
// DATABASE … TEMPLATE copies the contents, not the database's owner, its
// settings (ALTER DATABASE … SET, ALTER ROLE … IN DATABASE … SET), its
// privileges, comment or connection limit. Without them the application's
// role may lose its rights (on PostgreSQL 15+ the schema public belongs
// to the database's owner) and its sessions their time zone or
// search_path.
type pgDatabase struct {
	name      string
	Owner     string      `json:"owner"`
	ConnLimit int         `json:"connlimit"`
	AllowConn bool        `json:"allowconn"`
	Comment   *string     `json:"comment"`
	ACL       []pgGrant   `json:"acl"` // nil: the default privileges
	Settings  []pgSetting `json:"settings"`
}

type pgGrant struct {
	Grantee   string `json:"grantee"` // "" is PUBLIC
	Privilege string `json:"privilege"`
	Grantable bool   `json:"grantable"`
}

type pgSetting struct {
	Role    string `json:"role"` // "": every role (ALTER DATABASE … SET)
	Setting string `json:"setting"`
}

// database reads the properties of db that a copy keeps; nil when db does
// not exist.
func (p *Postgres) database(ctx context.Context, db string) (*pgDatabase, error) {
	var raw sql.NullString
	err := p.admin.QueryRowContext(ctx, `SELECT json_build_object(
		'owner', pg_get_userbyid(d.datdba), 'connlimit', d.datconnlimit, 'allowconn', d.datallowconn,
		'comment', shobj_description(d.oid, 'pg_database'),
		'acl', (SELECT json_agg(json_build_object('grantee', coalesce(pg_get_userbyid(nullif(a.grantee, 0)), ''),
			'privilege', a.privilege_type, 'grantable', a.is_grantable)) FROM aclexplode(d.datacl) a),
		'settings', (SELECT json_agg(json_build_object('role', coalesce(r.rolname, ''), 'setting', c))
			FROM pg_db_role_setting s LEFT JOIN pg_roles r ON r.oid = s.setrole, unnest(s.setconfig) c WHERE s.setdatabase = d.oid))::text
		FROM pg_database d WHERE d.datname = $1`, db).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := &pgDatabase{name: db}
	if err := json.Unmarshal([]byte(raw.String), d); err != nil {
		return nil, err
	}
	return d, nil
}

// keep gives tmp the properties of the database it replaces (its owner and
// connection limit are given by CREATE DATABASE).
func (p *Postgres) keep(ctx context.Context, tmp string, d *pgDatabase) error {
	var stmts []string
	for _, s := range d.Settings {
		name, value, _ := strings.Cut(s.Setting, "=")
		set := " SET " + qp(name) + " TO " + pgSettingValue(name, value)
		if s.Role == "" {
			stmts = append(stmts, "ALTER DATABASE "+qp(tmp)+set)
		} else {
			stmts = append(stmts, "ALTER ROLE "+qp(s.Role)+" IN DATABASE "+qp(tmp)+set)
		}
	}
	if d.ACL != nil {
		// From the default privileges (the owner's and PUBLIC's) to none,
		// then those dst has.
		stmts = append(stmts, "REVOKE ALL ON DATABASE "+qp(tmp)+" FROM PUBLIC, "+qp(d.Owner))
		for _, g := range d.ACL {
			grantee := "PUBLIC"
			if g.Grantee != "" {
				grantee = qp(g.Grantee)
			}
			grant := "GRANT " + g.Privilege + " ON DATABASE " + qp(tmp) + " TO " + grantee
			if g.Grantable {
				grant += " WITH GRANT OPTION"
			}
			stmts = append(stmts, grant)
		}
	}
	if d.Comment != nil {
		stmts = append(stmts, "COMMENT ON DATABASE "+qp(tmp)+" IS "+pgLiteral(*d.Comment))
	}
	for _, q := range stmts {
		if _, err := p.admin.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

// pgListQuote are the settings whose value is a list of names
// (GUC_LIST_QUOTE): pg_db_role_setting holds the names double-quoted where
// needed, and SET takes each as a literal of its own, as pg_dump writes it.
var pgListQuote = map[string]bool{"search_path": true, "temp_tablespaces": true, "local_preload_libraries": true,
	"session_preload_libraries": true, "shared_preload_libraries": true, "unix_socket_directories": true}

// pgSettingValue is the value of a setting of pg_db_role_setting as SET
// takes it.
func pgSettingValue(name, value string) string {
	if !pgListQuote[strings.ToLower(name)] {
		return pgLiteral(value)
	}
	var out []string
	for s := strings.TrimSpace(value); ; {
		var elem string
		if strings.HasPrefix(s, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(s); i++ {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i++
						continue
					}
					break
				}
				b.WriteByte(s[i])
			}
			elem, s = b.String(), strings.TrimSpace(s[min(i+1, len(s)):])
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			elem, s = strings.TrimSpace(s[:end]), s[end:]
		}
		out = append(out, pgLiteral(elem))
		if !strings.HasPrefix(s, ",") {
			break
		}
		s = strings.TrimSpace(s[1:])
	}
	return strings.Join(out, ", ")
}

// newSuffix ends the name a database is copied under, oldSuffix the name
// the database it replaces has until it is dropped (copyDatabase).
const (
	newSuffix = "__driftgate_new"
	oldSuffix = "__driftgate_old"
)

// newName is the name dst is copied under: dst's name with newSuffix, cut to
// PostgreSQL's 63 bytes.
func newName(dst string) string { return suffixed(dst, newSuffix) }

// oldName is the name of the database a copy replaced, until it is dropped.
func oldName(dst string) string { return suffixed(dst, oldSuffix) }

func suffixed(dst, suffix string) string {
	base := dst
	for len(base)+len(suffix) > 63 {
		_, n := utf8.DecodeLastRuneInString(base)
		base = base[:len(base)-n]
	}
	return base + suffix
}

// fileCopyMin is the size from which a database is copied file by file.
var fileCopyMin int64 = 100 << 20

// fileCopy reports whether src is copied with STRATEGY FILE_COPY (PostgreSQL
// 15+) rather than the default WAL_LOG, which writes every block to the WAL:
// from about 100 MB the file copy is two to three times faster (827 MB:
// 4.5–5.8 s → 1.7–2.2 s), below it slower (18 MB: 210 → 280 ms), as it
// forces a checkpoint before and after the copy. The copy is the same.
func (p *Postgres) fileCopy(ctx context.Context, src string) bool {
	var version, size int64
	err := p.admin.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::int, pg_database_size($1)", src).Scan(&version, &size)
	return err == nil && version >= 150000 && size >= fileCopyMin
}

func (p *Postgres) Restore(ctx context.Context, items []string) error {
	if items != nil && len(items) == 0 {
		return nil
	}
	ok, err := p.exists(ctx, p.o.Pristine)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("state %s: pristine database %s does not exist; run driftgate seed", p.o.Name, p.o.Pristine)
	}
	return p.copyDatabase(ctx, p.o.Pristine, p.o.Working, nil, nil)
}

// pristineComment starts the comment of a pristine database made by Seed; a
// database without it is never dropped (see Seed). A copy made from the
// pristine database as a template does not inherit the comment.
const pristineComment = "DriftGate pristine copy"

// Seed copies the working database into a new pristine database that does
// not accept connections, so nothing can change it or block a restore. The
// database it replaces must carry pristineComment, be empty, or be adopted
// (Options.AdoptPristine): any other database named as the pristine one is
// someone's data and is not dropped.
func (p *Postgres) Seed(ctx context.Context) error {
	ok, err := p.exists(ctx, p.o.Working)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("state %s: working database %s does not exist", p.o.Name, p.o.Working)
	}
	if err := p.checkPristine(ctx); err != nil {
		return err
	}
	return p.copyDatabase(ctx, p.o.Working, p.o.Pristine, func(tmp string) error {
		if _, err := p.admin.ExecContext(ctx, "COMMENT ON DATABASE "+qp(tmp)+" IS "+pgLiteral(pristineComment+" of "+p.o.Working+", made by driftgate seed and replaced by the next one")); err != nil {
			return err
		}
		_, err := p.admin.ExecContext(ctx, "ALTER DATABASE "+qp(tmp)+" ALLOW_CONNECTIONS false")
		return err
	}, p.o.Replacing)
}

// checkPristine refuses to replace a pristine database that Seed did not
// make: one that has objects of its own and no pristineComment, unless it is
// adopted.
func (p *Postgres) checkPristine(ctx context.Context) error {
	var comment sql.NullString
	var allow bool
	err := p.admin.QueryRowContext(ctx, "SELECT shobj_description(oid, 'pg_database'), datallowconn FROM pg_database WHERE datname = $1", p.o.Pristine).Scan(&comment, &allow)
	if errors.Is(err, sql.ErrNoRows) || p.o.AdoptPristine || strings.HasPrefix(comment.String, pristineComment) {
		return nil
	}
	if err != nil {
		return err
	}
	objects := -1 // unknown: it does not accept connections
	if allow {
		d, err := p.open(p.o.Pristine)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE `+schemaFilter("n.nspname")+`)
			+ (SELECT count(*) FROM pg_proc f JOIN pg_namespace n ON n.oid = f.pronamespace WHERE `+schemaFilter("n.nspname")+`)`).Scan(&objects); err != nil {
			return err
		}
	}
	if objects == 0 {
		return nil
	}
	return fmt.Errorf("state %s: database %s, named as the pristine copy, was not made by driftgate seed and is not empty, so it is not dropped; drop it yourself or name another pristine database", p.o.Name, p.o.Pristine)
}

func (p *Postgres) Rows(ctx context.Context, items []string) (Rows, error) {
	d, err := p.open(p.o.Working)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	c, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out := Rows{}
	tables, _, _, _ := splitItems(items)
	if items == nil {
		for _, t := range p.o.RowTracking {
			if !strings.Contains(t, ".") {
				t = "public." + t
			}
			tables = append(tables, t)
		}
	}
	for _, t := range tables {
		_, bare, _ := strings.Cut(t, ".")
		if !p.o.tracked(t) && !p.o.tracked(bare) {
			continue
		}
		schema, name, _ := strings.Cut(t, ".")
		rel := qp(schema) + "." + qp(name)
		rows, err := c.QueryContext(ctx, `SELECT a.attname FROM pg_index i
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
			WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY array_position(i.indkey, a.attnum)`, rel)
		if err != nil {
			return nil, err
		}
		var pk []string
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				rows.Close()
				return nil, err
			}
			pk = append(pk, col)
		}
		rows.Close()
		if len(pk) == 0 {
			return nil, fmt.Errorf("state %s: row_tracking table %s has no primary key", p.o.Name, t)
		}
		r, err := readRows(ctx, c, "SELECT * FROM "+rel, pk)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		out[t] = r
	}
	return out, nil
}

// Query runs a read-only query (see CheckQuery) on the pristine database,
// which accepts connections only for that moment.
func (p *Postgres) Query(ctx context.Context, query string) ([]string, error) {
	if err := CheckQuery(query); err != nil {
		return nil, err
	}
	if _, err := p.admin.ExecContext(ctx, "ALTER DATABASE "+qp(p.o.Pristine)+" ALLOW_CONNECTIONS true"); err != nil {
		return nil, fmt.Errorf("pristine database %s: %w (run driftgate seed)", p.o.Pristine, err)
	}
	defer p.admin.ExecContext(context.Background(), "ALTER DATABASE "+qp(p.o.Pristine)+" ALLOW_CONNECTIONS false")
	d, err := p.open(p.o.Pristine)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return firstColumn(rows)
}
