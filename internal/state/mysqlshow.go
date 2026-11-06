package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbx"
)

// Reading SHOW CREATE for many objects: one statement each costs a round
// trip each (141 tables over a link with 55 ms of latency: 8 s). showAll
// sends them as one query of many statements on a connection of its own
// (MySQL.multi) and reads each statement's result in turn; the rows are the
// ones each statement returns on its own, so nothing that is hashed changes.
//
//   - A statement that fails stops the server from running the ones after
//     it (an object dropped in the meantime: 1146 or 1360). That one is
//     then read alone, which treats a missing object as the single
//     statement always has (dbx.ShowCreate, missingOK), and the batches go
//     on after it.
//   - A server or proxy that refuses a query of several statements (a
//     batch fails with a syntax error at its first statement, also after
//     batches that worked), or a connection
//     character set in which a quoted name is not safe in one (initMulti):
//     the statements are then read one by one, spread over several
//     connections (showSpread), from then on.

// showReq is one SHOW CREATE kind db.name.
type showReq struct{ kind, db, name string }

// showBatchMax is the most statements in one query (a variable for tests).
var showBatchMax = 500

// showAll runs SHOW CREATE for every request and returns the columns of
// each by name, or nil for an object that does not exist (dbx.ShowCreate
// with missingOK). c is a session; it reads what is read one by one.
func (m *MySQL) showAll(ctx context.Context, c *sql.Conn, reqs []showReq) ([]map[string]string, error) {
	out := make([]map[string]string, len(reqs))
	if len(reqs) < 2 {
		return out, showEach(ctx, c, reqs, out)
	}
	if m.multiRefused.Load() {
		return out, m.showSpread(ctx, c, reqs, out)
	}
	for done := 0; done < len(reqs); {
		end := min(done+showBatchMax, len(reqs))
		n, err := showManyFunc(m, ctx, reqs[done:end], out[done:end])
		switch {
		case err == nil:
			done = end
			continue
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case n >= end-done:
			// Every statement was read, but the query did not end as it
			// should: none of its results is trusted, the chunk is read
			// again one by one.
			clear(out[done:end])
			if err := showEach(ctx, c, reqs[done:end], out[done:end]); err != nil {
				return nil, err
			}
			done = end
			continue
		case n == 0 && multiRefusal(err):
			// Also after batches that worked: a proxy that stops passing the
			// flag on would otherwise cost a failed batch per statement.
			m.multiRefused.Store(true)
			m.multiNote.Do(func() {
				m.logf("state %s: SHOW CREATE is read one statement at a time (a query of several statements was refused: %v)\n", m.o.Name, err)
			})
			return out, m.showSpread(ctx, c, reqs[done:], out[done:])
		case n == 0 && (errors.Is(err, errMultiConn) || !serverError(err)):
			// No batch could be sent (no connection for it, or it was
			// lost): the rest one by one on c, with one note.
			m.multiConnNote.Do(func() {
				m.logf("state %s: SHOW CREATE is read one statement at a time (%v)\n", m.o.Name, err)
			})
			return out, showEach(ctx, c, reqs[done:], out[done:])
		}
		// The statement after the n read failed: it is read alone, as it
		// always was (a missing object is nil, another error fails as it
		// does alone), and the batches go on after it.
		k := done + n
		r := reqs[k]
		if out[k], err = show(ctx, c, r.kind, r.db, r.name); err != nil {
			return nil, err
		}
		done = k + 1
	}
	return out, nil
}

// errMultiConn marks showMany's failure to get a connection for the query
// (max_connections, max_user_connections, the network): not an error of
// any statement.
var errMultiConn = errors.New("no connection for a query of several statements")

// showManyFunc is showMany (tests: a batch that goes wrong).
var showManyFunc = (*MySQL).showMany

// multiRefusal reports whether err is a refusal of queries of several
// statements: the server's syntax error at the first separator (the
// client flag did not reach it), or initMulti's refusal of the
// connection's character set.
func multiRefusal(err error) bool {
	var me *mysql.MySQLError
	return errors.Is(err, errMultiCharset) || errors.As(err, &me) && me.Number == 1064
}

// showEach reads the requests one by one on c.
func showEach(ctx context.Context, c *sql.Conn, reqs []showReq, out []map[string]string) error {
	for i, r := range reqs {
		var err error
		if out[i], err = show(ctx, c, r.kind, r.db, r.name); err != nil {
			return err
		}
	}
	return nil
}

// showMany runs the requests as one query on a connection of m.multi. n
// is how many of them were read when err is not nil: the statement after
// them failed, or its result could not be read.
func (m *MySQL) showMany(ctx context.Context, reqs []showReq, out []map[string]string) (n int, err error) {
	conn, err := m.multi.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errMultiConn, err)
	}
	defer func() {
		if err != nil {
			discardConn(conn) // whatever the failed query left on it
			return
		}
		conn.Close()
	}()
	stmts := make([]string, len(reqs))
	for i, r := range reqs {
		stmts[i] = dbx.ShowCreateQuery(r.kind, r.db, r.name)
	}
	rows, err := conn.QueryContext(ctx, strings.Join(stmts, "; "))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for i := range reqs {
		if i > 0 && !rows.NextResultSet() {
			if err := rows.Err(); err != nil {
				return i, err
			}
			return i, fmt.Errorf("SHOW CREATE: %d results for %d statements", i, len(reqs))
		}
		r, err := dbx.ShowCreateRow(rows)
		if err != nil {
			return i, err
		}
		out[i] = r
	}
	if rows.NextResultSet() {
		return len(reqs), fmt.Errorf("SHOW CREATE: more results than %d statements", len(reqs))
	}
	return len(reqs), rows.Err()
}

// showSpread reads the requests one by one, spread over c and up to
// checksumConns-1 sessions of their own. The error is the one the reads one
// by one on c would have stopped at: that of the first request that fails.
func (m *MySQL) showSpread(ctx context.Context, c *sql.Conn, reqs []showReq, out []map[string]string) error {
	conns := min(checksumConns, len(reqs))
	errs := make([]error, len(reqs))
	var mu sync.Mutex
	var left []int // the shares of the sessions that could not be opened
	var wg sync.WaitGroup
	for k := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := c
			if k > 0 {
				var err error
				if conn, err = m.session(ctx); err != nil {
					mu.Lock()
					left = append(left, k)
					mu.Unlock()
					return
				}
				defer conn.Close()
			}
			for i := k; i < len(reqs); i += conns {
				r := reqs[i]
				if out[i], errs[i] = show(ctx, conn, r.kind, r.db, r.name); errs[i] != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, k := range left {
		for i := k; i < len(reqs); i += conns {
			r := reqs[i]
			if out[i], errs[i] = show(ctx, c, r.kind, r.db, r.name); errs[i] != nil {
				break
			}
		}
	}
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
