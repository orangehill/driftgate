package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// pipelineConn is a connection from OpenPostgres to DRIFTGATE_TEST_POSTGRES.
func pipelineConn(t *testing.T) (*sql.DB, *sql.Conn) {
	t.Helper()
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	db, err := OpenPostgres(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return db, c
}

// text is the values of a result's rows, NULL as "<NULL>".
func text(r PostgresResult) [][]string {
	out := [][]string{}
	for _, row := range r.Rows {
		var vals []string
		for _, v := range row {
			if !v.Valid {
				v.String = "<NULL>"
			}
			vals = append(vals, v.String)
		}
		out = append(out, vals)
	}
	return out
}

// TestPostgresPipelineArgs: a []string goes as the array literal exec
// mode sends (SELECT $1::text gives back the very bytes), whose type the
// server infers for = ANY($1); nil is NULL, which matches no row, as an
// empty array does.
func TestPostgresPipelineArgs(t *testing.T) {
	_, c := pipelineConn(t)
	ctx := context.Background()
	names := []string{"public.plain", `public."quoted"`, "a,b", "{braces}", `back\slash`, "it's", "NULL", "", " spaced ", "ćirilica ж"}
	in := "ARRAY[" + strings.Repeat("?, ", len(names)-1) + "?]"
	for _, n := range names {
		in = strings.Replace(in, "?", "'"+strings.ReplaceAll(n, "'", "''")+"'", 1)
	}
	for _, arg := range []any{names, []string(nil), []string{}} {
		res := PostgresPipeline(ctx, c, []PostgresStatement{
			{SQL: "SELECT $1::text", Args: []any{arg}},
			{SQL: "SELECT s FROM unnest(" + in + ") s WHERE s = ANY($1) ORDER BY 1", Args: []any{arg}},
		})
		for i, r := range res {
			if r.Err != nil {
				t.Fatalf("%v: statement %d: %v", arg, i, r.Err)
			}
		}
		var want sql.NullString
		if err := c.QueryRowContext(ctx, "SELECT $1::text", arg).Scan(&want); err != nil {
			t.Fatal(err)
		}
		if got := res[0].Rows[0][0]; got != want {
			t.Errorf("%#v went as %+v, exec mode sends %+v", arg, got, want)
		}
		rows, err := c.QueryContext(ctx, "SELECT s FROM unnest("+in+") s WHERE s = ANY($1) ORDER BY 1", arg)
		if err != nil {
			t.Fatal(err)
		}
		wantRows := [][]string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			wantRows = append(wantRows, []string{s})
		}
		rows.Close()
		if got := text(res[1]); !reflect.DeepEqual(got, wantRows) {
			t.Errorf("%#v: = ANY($1) matched %q, in exec mode %q", arg, got, wantRows)
		}
		if n := len(arg.([]string)); len(wantRows) != n {
			t.Errorf("%#v matched %d rows, want %d", arg, len(wantRows), n)
		}
	}
}

// TestPostgresPipelineResults: values are copied (each row has its own),
// NULL is not Valid, and an error ends its statement only: it is in its
// slot, without the rows read before it, the statements after it run, and
// the connection is usable after.
func TestPostgresPipelineResults(t *testing.T) {
	_, c := pipelineConn(t)
	ctx := context.Background()
	for range 2 { // the second time on the same connection
		res := PostgresPipeline(ctx, c, []PostgresStatement{
			{SQL: "SELECT g::text, NULL::text FROM generate_series(1, 1000) g"},
			{SQL: "SELECT 1 / (g - 3) FROM generate_series(1, 5) g"},
			{SQL: "SELECT nonsense FROM"},
			{SQL: "SELECT 'after'"},
		})
		if res[0].Err != nil || len(res[0].Rows) != 1000 {
			t.Fatalf("first: %d rows, %v", len(res[0].Rows), res[0].Err)
		}
		for i, row := range res[0].Rows {
			if row[0].String != strconv.Itoa(i+1) || !row[0].Valid || row[1].Valid {
				t.Fatalf("row %d: %+v", i+1, row)
			}
		}
		for i, code := range map[int]string{1: "22012", 2: "42601"} {
			if pe := (*pgconn.PgError)(nil); !errors.As(res[i].Err, &pe) || pe.Code != code || res[i].Rows != nil {
				t.Errorf("statement %d: %v, %d rows; want %s", i, res[i].Err, len(res[i].Rows), code)
			}
		}
		if got := text(res[3]); res[3].Err != nil || !reflect.DeepEqual(got, [][]string{{"after"}}) {
			t.Errorf("after the errors: %q, %v", got, res[3].Err)
		}
		var one int
		if err := c.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("the connection after the pipeline: %v", err)
		}
	}
	if res := PostgresPipeline(ctx, c, nil); len(res) != 0 {
		t.Errorf("no statements: %v", res)
	}
}

// TestPostgresPipelineLarge: requests and results larger than the
// sockets' buffers do not block each other.
func TestPostgresPipelineLarge(t *testing.T) {
	_, c := pipelineConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pad := "/* " + strings.Repeat("x", 4000) + " */ "
	var stmts []PostgresStatement
	for i := range 2000 {
		stmts = append(stmts, PostgresStatement{SQL: pad + fmt.Sprintf("SELECT %d, repeat('y', 8000)", i)})
	}
	stmts = append(stmts, PostgresStatement{SQL: "SELECT g, repeat('z', 1000) FROM generate_series(1, 20000) g"})
	res := PostgresPipeline(ctx, c, stmts)
	for i, r := range res[:2000] {
		if r.Err != nil || len(r.Rows) != 1 || r.Rows[0][0].String != strconv.Itoa(i) || len(r.Rows[0][1].String) != 8000 {
			t.Fatalf("statement %d: %d rows, %v", i, len(r.Rows), r.Err)
		}
	}
	if last := res[2000]; last.Err != nil || len(last.Rows) != 20000 || last.Rows[19999][0].String != "20000" {
		t.Fatalf("large result: %d rows, %v", len(last.Rows), last.Err)
	}
}

// TestPostgresPipelineCancel: when ctx ends, every statement not read yet
// has its error, at once; the pool then gives a working connection.
func TestPostgresPipelineCancel(t *testing.T) {
	db, c := pipelineConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := PostgresPipeline(ctx, c, []PostgresStatement{{SQL: "SELECT 1"}, {SQL: "SELECT pg_sleep(10)"}, {SQL: "SELECT 2"}})
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the pipeline returned %v after ctx ended", d)
	}
	if res[0].Err != nil {
		t.Errorf("first: %v", res[0].Err)
	}
	for _, r := range res[1:] {
		if !errors.Is(r.Err, context.DeadlineExceeded) {
			t.Errorf("after ctx ended: %v", r.Err)
		}
	}
	c.Close()
	var one int
	if err := db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("after the cancel: %v", err)
	}
}
