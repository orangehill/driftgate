package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orangehill/driftgate/internal/dbx"
)

// pgSequential runs the statements one by one through database/sql, as a
// fingerprint read them before dbx.PostgresPipeline: its oracle.
func pgSequential(ctx context.Context, c *sql.Conn, stmts []dbx.PostgresStatement) []dbx.PostgresResult {
	out := make([]dbx.PostgresResult, len(stmts))
	for i, s := range stmts {
		out[i].Rows, out[i].Err = pgQuery(ctx, c, s)
	}
	return out
}

func pgQuery(ctx context.Context, c *sql.Conn, s dbx.PostgresStatement) ([][]sql.NullString, error) {
	rows, err := c.QueryContext(ctx, s.SQL, s.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]sql.NullString
	for rows.Next() {
		row := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range row {
			dest[i] = &row[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// TestPgStageRows: a NULL is an error but in the column that may be NULL,
// where it is ""; a query's error is returned as it is.
func TestPgStageRows(t *testing.T) {
	boom := errors.New("boom")
	s := pgStage{res: []dbx.PostgresResult{
		{Rows: [][]sql.NullString{{{String: "a", Valid: true}, {String: "b", Valid: true}}, {{String: "c", Valid: true}, {}}}},
		{Err: boom},
	}}
	if _, err := s.rows(0, -1); err == nil || !strings.Contains(err.Error(), "NULL in column 2 of row 2") {
		t.Errorf("NULL read as a string: %v", err)
	}
	if _, err := s.rows(0, 0); err == nil {
		t.Error("NULL in another column than the one that may be NULL")
	}
	if got, err := s.rows(0, 1); err != nil || !reflect.DeepEqual(got, [][]string{{"a", "b"}, {"c", ""}}) {
		t.Errorf("NULL where it may be: %q, %v", got, err)
	}
	if _, err := s.rows(1, -1); err != boom {
		t.Errorf("the query's error: %v", err)
	}
	// The views' query failing while read stops the fingerprint (it once
	// left the views after the error out).
	all := func(string) bool { return true }
	if err := (&Postgres{}).views(&s, 1, Fingerprint{}, all, false, nil); err != boom {
		t.Errorf("views with the query's error: %v", err)
	}
}

// TestPostgresFingerprintPinned: the queries of a fingerprint and how
// their results are joined and hashed are those every stored PostgreSQL
// fingerprint was made with. A change here changes every baseline, which
// then stops: it needs a new format (FingerprintAs), not a new pin.
func TestPostgresFingerprintPinned(t *testing.T) {
	var all []string
	for _, format := range []int{1, 2} {
		for _, mode := range []fpMode{fpRows, fpRestoreOnly, fpSchema} {
			all = append(all, pgDefinitions(format, mode, false)...)
			all = append(all, pgDefinitions(format, mode, true)...)
			all = append(all, pgSequences(format, mode))
		}
	}
	for _, mode := range []fpMode{fpRows, fpRestoreOnly, fpSchema} {
		all = append(all, pgSchemaItems(mode)...)
	}
	all = append(all, pgOwnedSequences, pgRowsQuery("app.news"))
	if got, want := hash([]byte(strings.Join(all, "\x00"))), "d22e351f95cb9c76c99062994e5b8b084792b7ac4a4bdaebb9b8042953260d38"; got != want {
		t.Errorf("the queries of a fingerprint hash to %s, want %s", got, want)
	}
	v := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	s := &pgStage{res: []dbx.PostgresResult{
		{Rows: [][]sql.NullString{{v("3"), v("abc")}}},
		{Rows: [][]sql.NullString{{v("public.counter"), v("10")}}},
	}}
	if got, err := rowsOf(s, 0, "public.news"); err != nil || got != "3\x00abc" {
		t.Errorf("rowsOf = %q, %v", got, err)
	}
	f := Fingerprint{"sequence:public.counter": "def", "sequence:public.used": "def 7"}
	if err := nextValues(s, 1, f); err != nil {
		t.Fatal(err)
	}
	if want := (Fingerprint{"sequence:public.counter": hash([]byte("def next 10")), "sequence:public.used": hash([]byte("def 7"))}); !reflect.DeepEqual(f, want) {
		t.Errorf("sequences %v, want %v", f, want)
	}
}

// pgPipelineFixture makes a working database with every kind of object a
// fingerprint reads, seeds it and changes the working database after.
func pgPipelineFixture(t *testing.T, o Options) *Postgres {
	t.Helper()
	p, err := NewPostgres(o)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		for _, db := range []string{o.Working, o.Pristine} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
	}
	drop()
	t.Cleanup(func() {
		p.Close()
		drop()
	})
	mustExec(t, p.admin, "CREATE DATABASE "+o.Working)
	db, err := p.open(o.Working)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	mustExec(t, db,
		"CREATE EXTENSION hstore",
		"CREATE SCHEMA tenant",
		"CREATE TYPE mood AS ENUM ('sad', 'happy')",
		"CREATE DOMAIN positive AS int CHECK (VALUE > 0)",
		"CREATE TYPE pair AS (a int, b text COLLATE \"C\")",
		"CREATE FUNCTION add1(i int) RETURNS int LANGUAGE sql AS 'SELECT i + 1'",
		"CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'",
		"CREATE AGGREGATE total(int) (SFUNC = int4pl, STYPE = int, INITCOND = '0')",
		"CREATE TABLE news (id serial PRIMARY KEY, title text COLLATE \"C\", m mood, p positive, h hstore, at timestamptz DEFAULT now(), body bytea)",
		"COMMENT ON TABLE news IS 'the news'",
		"COMMENT ON COLUMN news.title IS 'its title'",
		"CREATE TABLE tags (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY, news_id int REFERENCES news(id), name text)",
		"CREATE INDEX tags_name ON tags (name)",
		"CREATE TRIGGER tags_touch BEFORE INSERT ON tags FOR EACH ROW EXECUTE FUNCTION touch()",
		"ALTER TABLE tags DISABLE TRIGGER tags_touch",
		"CREATE TABLE secured (id int PRIMARY KEY, owner text)",
		"ALTER TABLE secured ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY own ON secured USING (owner = current_user)",
		"CREATE TABLE ruled (id int)",
		"CREATE RULE ruled_nothing AS ON DELETE TO ruled DO INSTEAD NOTHING",
		"CREATE UNLOGGED TABLE scratch (id int)",
		"ALTER TABLE scratch SET (fillfactor = 70)",
		"GRANT SELECT ON news TO PUBLIC",
		"CREATE TABLE measurements (at date NOT NULL, v int) PARTITION BY RANGE (at)",
		"CREATE TABLE measurements_2026 PARTITION OF measurements FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
		"CREATE TABLE tenant.accounts (id bigserial PRIMARY KEY, name text)",
		"CREATE TABLE audit_log (id serial PRIMARY KEY, note text)",
		`CREATE TABLE "we'ird ""name"", {x}\" (id serial PRIMARY KEY)`,
		"CREATE TABLE cache (id serial PRIMARY KEY, v text)",
		"CREATE TABLE skipme (id serial PRIMARY KEY)",
		"CREATE VIEW news_titles AS SELECT id, title FROM news",
		"COMMENT ON VIEW news_titles IS 'titles'",
		"CREATE MATERIALIZED VIEW news_count AS SELECT count(*) AS n FROM news",
		"CREATE MATERIALIZED VIEW tag_count AS SELECT count(*) AS n FROM tags WITH NO DATA",
		"CREATE SEQUENCE called",
		"CREATE SEQUENCE never",
		"CREATE SEQUENCE reset_to START 1",
		"CREATE SEQUENCE custom INCREMENT 5 START 10 MAXVALUE 1000 CYCLE",
		"SELECT nextval('called')",
		"SELECT setval('reset_to', 42, false)",
		"INSERT INTO news (title, m, p, h, at, body) VALUES ('First', 'happy', 1, 'a=>1', '2026-01-02 03:04:05+00', '\\x00ff'), ('Second', NULL, NULL, NULL, NULL, NULL)",
		"INSERT INTO tags (news_id, name) VALUES (1, 'a')",
		"INSERT INTO secured VALUES (1, 'x')",
		"INSERT INTO measurements VALUES ('2026-05-01', 3)",
		"INSERT INTO tenant.accounts (name) VALUES ('acme')",
		"INSERT INTO audit_log (note) VALUES ('a')",
		`INSERT INTO "we'ird ""name"", {x}\" DEFAULT VALUES`,
		"INSERT INTO cache (v) VALUES ('c')",
		"INSERT INTO skipme DEFAULT VALUES",
	)
	db.Close() // the seed ends the sessions on the working database
	if err := p.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db, err = p.open(o.Working); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		"INSERT INTO news (title) VALUES ('Third')",
		"INSERT INTO audit_log (note) VALUES ('b')",
		"INSERT INTO cache (v) VALUES ('d')",
		"SELECT nextval('never')",
		"REFRESH MATERIALIZED VIEW tag_count",
		"ALTER TABLE tags ENABLE TRIGGER tags_touch",
	)
	return p
}

// TestPostgresPipelineSameAsSequential: every read of a fingerprint gives,
// item by item, what the same queries give sent one by one.
func TestPostgresPipelineSameAsSequential(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	p := pgPipelineFixture(t, Options{Name: "db", DSN: dsn, Working: "dgt_pipe", Pristine: "dgt_pipe_pristine",
		Exclude: []string{"skipme"}, RestoreOnly: []string{"audit_log", `we'ird "name", {x}\`}, RestoreEmpty: []string{"cache"}})
	both := func(name string, read func() (Fingerprint, error)) Fingerprint {
		t.Helper()
		p.run = pgSequential
		want, werr := read()
		p.run = dbx.PostgresPipeline
		got, err := read()
		if err != nil || werr != nil {
			t.Fatalf("%s: %v; sent one by one: %v", name, err, werr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: differs from the queries sent one by one in %v", name, Diff(got, want))
		}
		return got
	}
	for _, format := range []int{1, 2} {
		for _, mode := range []fpMode{fpRows, fpRestoreOnly, fpSchema} {
			for _, pristine := range []bool{false, true} {
				name := fmt.Sprintf("format %d, mode %d, pristine %v", format, mode, pristine)
				f := both(name, func() (Fingerprint, error) { return p.read(ctx, pristine, nil, format, mode) })
				if len(f) == 0 {
					t.Errorf("%s: no items", name)
				}
			}
		}
	}
	f := both("Fingerprint", func() (Fingerprint, error) { return p.Fingerprint(ctx, nil) })
	for _, item := range []string{"table:public.news", "table:tenant.accounts", "view:public.news_titles", "matview:public.news_count", "matview:public.tag_count",
		"sequence:public.never", "sequence:public.reset_to", "sequence:public.custom", "type:public.mood", "type:public.positive", "type:public.pair",
		"function:public.add1(i integer)", "schema:tenant"} {
		if f[item] == "" {
			t.Errorf("no item %s", item)
		}
	}
	both("SchemaFingerprint", func() (Fingerprint, error) { return p.SchemaFingerprint(ctx, false) })
	ro := both("RestoreOnlyFingerprint", func() (Fingerprint, error) { return p.RestoreOnlyFingerprint(ctx, false) })
	if _, ok := ro[`table:public.we'ird "name", {x}\`]; !ok || len(ro) != 6 { // 3 tables, 3 sequences
		t.Errorf("restore_only items: %v", slices.Sorted(maps.Keys(ro)))
	}
	items := []string{"table:public.news", "view:public.news_titles", "sequence:public.never", "matview:public.tag_count", "table:public.missing"}
	both("Fingerprint(items)", func() (Fingerprint, error) { return p.Fingerprint(ctx, items) })
	// What a restore of restore_empty tables reads (restoreEmpty).
	empty := []string{"table:public.cache", "sequence:public.cache_id_seq"}
	if f := both("restore_empty items", func() (Fingerprint, error) {
		return p.fingerprint(ctx, p.o.Working, empty, FingerprintFormat, fpRestoreOnly)
	}); len(f) != 2 {
		t.Errorf("restore_empty items: %v", f)
	}
}

// TestPostgresPipelineNoRestoreOnly: with no restore_only table the
// definitions are read of none (nil names), and the pipeline reads what the
// queries sent one by one read.
func TestPostgresPipelineNoRestoreOnly(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	for _, only := range [][]string{nil, {"nothing_matches"}} {
		p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: "dgt_pipe_none", Pristine: "dgt_pipe_none_pristine", RestoreOnly: only})
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, p.admin, "DROP DATABASE IF EXISTS dgt_pipe_none WITH (FORCE)", "CREATE DATABASE dgt_pipe_none")
		db, err := p.open("dgt_pipe_none")
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, "CREATE TABLE news (id serial PRIMARY KEY)", "INSERT INTO news DEFAULT VALUES")
		db.Close()
		for _, run := range []func(context.Context, *sql.Conn, []dbx.PostgresStatement) []dbx.PostgresResult{pgSequential, dbx.PostgresPipeline} {
			p.run = run
			if f, err := p.RestoreOnlyFingerprint(ctx, false); err != nil || len(f) != 0 {
				t.Errorf("restore_only %q: %v, %v", only, f, err)
			}
		}
		p.admin.Exec("DROP DATABASE IF EXISTS dgt_pipe_none WITH (FORCE)")
		p.Close()
	}
}

// pgRole makes a role that logs in with the password "app" and returns the
// DSN that connects as it.
func pgRole(t *testing.T, p *Postgres, dsn, role string) string {
	t.Helper()
	p.admin.Exec("DROP ROLE IF EXISTS " + role)
	mustExec(t, p.admin, "CREATE ROLE "+role+" LOGIN PASSWORD 'app'")
	t.Cleanup(func() {
		for _, db := range []string{p.o.Working, p.o.Pristine} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
		p.admin.Exec("DROP ROLE IF EXISTS " + role)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "app")
	return u.String()
}

// TestPostgresPipelineRevoked: a table the role may not read fails its
// count in the middle of the second round trip: the error names it, the
// queries after it run (the pipeline is read to its end, nothing left
// unsynced), and once the role may read it, the next read works.
func TestPostgresPipelineRevoked(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	o := Options{Name: "db", DSN: dsn, Working: "dgt_pipe_revoked", Pristine: "dgt_pipe_revoked_pristine"}
	p := pgPipelineFixture(t, o)
	o.DSN = pgRole(t, p, dsn, "dgt_pipe_app")
	db, err := p.open(o.Working)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "GRANT SELECT ON ALL TABLES IN SCHEMA public, tenant TO dgt_pipe_app", "GRANT SELECT ON ALL SEQUENCES IN SCHEMA public, tenant TO dgt_pipe_app",
		"GRANT USAGE ON SCHEMA tenant TO dgt_pipe_app", "REVOKE SELECT ON news FROM dgt_pipe_app, PUBLIC")
	app, err := NewPostgres(o)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var second []dbx.PostgresResult
	var stmts []dbx.PostgresStatement
	app.run = func(ctx context.Context, c *sql.Conn, s []dbx.PostgresStatement) []dbx.PostgresResult {
		res := dbx.PostgresPipeline(ctx, c, s)
		stmts, second = s, res
		return res
	}
	_, err = app.Fingerprint(ctx, nil)
	if pe := (*pgconn.PgError)(nil); err == nil || !strings.HasPrefix(err.Error(), "public.news: ") || !errors.As(err, &pe) || pe.Code != "42501" {
		t.Fatalf("fingerprint of a table the role may not read: %v", err)
	}
	if strings.Contains(err.Error(), "unsynced") {
		t.Fatalf("pipeline left unsynced: %v", err)
	}
	failed := slices.IndexFunc(second, func(r dbx.PostgresResult) bool { return r.Err != nil })
	if failed < 0 || failed == len(second)-1 || !strings.Contains(stmts[failed].SQL, `"news"`) {
		t.Fatalf("the error is in statement %d of %d", failed, len(second))
	}
	for i, r := range second[failed+1:] {
		if r.Err != nil {
			t.Errorf("statement %d after the error: %v", failed+1+i, r.Err)
		}
	}
	mustExec(t, db, "GRANT SELECT ON news TO dgt_pipe_app")
	got, err := app.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatalf("after the grant: %v", err)
	}
	app.run = pgSequential
	if want, err := app.Fingerprint(ctx, nil); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after the grant: %v, differs in %v", err, Diff(got, want))
	}
}

// TestPostgresPipelineCancel: ctx ending in the second round trip ends the
// read at once with ctx's error, and the next read works.
func TestPostgresPipelineCancel(t *testing.T) {
	dsn := postgresServer(t)
	p := pgPipelineFixture(t, Options{Name: "db", DSN: dsn, Working: "dgt_pipe_cancel", Pristine: "dgt_pipe_cancel_pristine"})
	want, err := p.Fingerprint(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage := 0
	p.run = func(ctx context.Context, c *sql.Conn, s []dbx.PostgresStatement) []dbx.PostgresResult {
		if stage++; stage == 2 {
			// A slow query first, ended by the cancel.
			s = append([]dbx.PostgresStatement{{SQL: "SELECT pg_sleep(10)"}}, s...)
			time.AfterFunc(200*time.Millisecond, cancel)
			return dbx.PostgresPipeline(ctx, c, s)[1:]
		}
		return dbx.PostgresPipeline(ctx, c, s)
	}
	start := time.Now()
	if _, err := p.Fingerprint(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled in the second round trip: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cancelled read returned after %v", d)
	}
	p.run = dbx.PostgresPipeline
	got, err := p.Fingerprint(context.Background(), nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("the read after the cancel: %v, differs in %v", err, Diff(got, want))
	}
}
