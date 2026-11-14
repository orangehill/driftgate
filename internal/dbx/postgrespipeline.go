package dbx

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// PostgresStatement is a query and its arguments.
type PostgresStatement struct {
	SQL  string
	Args []any
}

// PostgresResult is what a statement returned: its rows, each value the
// text the server printed (not Valid for NULL), or the error that ended
// it.
type PostgresResult struct {
	Rows [][]sql.NullString
	Err  error
}

// PostgresPipeline runs statements on c, a connection from OpenPostgres,
// in one round trip: each is sent as exec mode sends it (its arguments
// encoded by pgx as text, their types inferred by the server, results as
// text), followed by a Sync, so each runs in an implicit transaction of
// its own, as one sent alone does, and the error of one leaves the others
// to run. The result of every statement is in the slot of its index; when
// the connection fails (ctx ended), every statement not read yet has that
// error. pgx sends while it reads, so a large result cannot block the
// statements behind it.
func PostgresPipeline(ctx context.Context, c *sql.Conn, stmts []PostgresStatement) []PostgresResult {
	out := make([]PostgresResult, len(stmts))
	if len(stmts) == 0 {
		return out
	}
	read := 0
	err := c.Raw(func(dc any) error {
		conn := dc.(*stdlib.Conn).Conn()
		args := make([]pgx.ExtendedQueryBuilder, len(stmts))
		for i, s := range stmts {
			if err := args[i].Build(conn.TypeMap(), nil, s.Args); err != nil {
				return err
			}
		}
		pl := conn.PgConn().StartPipeline(ctx)
		for i, s := range stmts {
			pl.SendQueryParams(s.SQL, args[i].ParamValues, nil, args[i].ParamFormats, args[i].ResultFormats)
			pl.SendPipelineSync()
		}
		if err := pl.Flush(); err != nil {
			return err
		}
		for ; read < len(out); read++ {
			if err := readPipelined(pl, &out[read]); err != nil {
				pl.Close()
				return err
			}
		}
		return pl.Close()
	})
	for i := read; i < len(out) && err != nil; i++ {
		out[i] = PostgresResult{Err: err}
	}
	return out
}

// readPipelined reads the result of the next statement and its Sync into
// r. An error of the statement (a *pgconn.PgError) goes to r; any other
// one, of the connection, is returned.
func readPipelined(pl *pgconn.Pipeline, r *PostgresResult) error {
	res, err := pl.GetResults()
	if rr, ok := res.(*pgconn.ResultReader); ok {
		for rr.NextRow() {
			vals := rr.Values()
			row := make([]sql.NullString, len(vals))
			for i, v := range vals {
				row[i] = sql.NullString{String: string(v), Valid: v != nil} // a copy: the reader reuses its buffer
			}
			r.Rows = append(r.Rows, row)
		}
		_, err = rr.Close()
	}
	for {
		if pe := (*pgconn.PgError)(nil); err != nil && !errors.As(err, &pe) {
			return err
		}
		if err != nil && r.Err == nil {
			r.Rows, r.Err = nil, err
		}
		if _, ok := res.(*pgconn.PipelineSync); ok {
			return nil
		}
		if res, err = pl.GetResults(); res == nil && err == nil {
			return errors.New("pipeline: no Sync after a statement")
		}
	}
}
