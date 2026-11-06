package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/dbx"
)

// Reading SHOW CREATE for many objects: one statement each costs a round
// trip each (141 tables over a link with 55 ms of latency: 8 s). showAll
// sends them as one query of many statements on a connection of its own
// (MySQL.multi) and reads each statement's result in turn; the rows are the
// ones each statement returns on its own, so nothing that is hashed changes.
//
//   - A statement that fails stops the server from running the ones after
//     it (an object dropped in the meantime: 1146 or 1360). That one and the
//     rest are then read one by one, which treats a missing object as the
//     single statement always has (dbx.ShowCreate, missingOK).
//   - A server or proxy that refuses a query of several statements (the
//     first one ever sent fails): the statements are then read one by one,
//     spread over several connections (showSpread), from then on.

// showReq is one SHOW CREATE kind db.name.
type showReq struct{ kind, db, name string }

// showBatch turns the query of several statements off (tests: the reads
// one by one, as before, to compare with).
var showBatch = true

// showBatchMax is the most statements in one query.
const showBatchMax = 500

// showAll runs SHOW CREATE for every request and returns the columns of
// each by name, or nil for an object that does not exist (dbx.ShowCreate
// with missingOK). c is a session; it reads what is read one by one.
func (m *MySQL) showAll(ctx context.Context, c *sql.Conn, reqs []showReq) ([]map[string]string, error) {
	out := make([]map[string]string, len(reqs))
	if !showBatch || len(reqs) < 2 {
		return out, showEach(ctx, c, reqs, out)
	}
	if m.multiRefused.Load() {
		return out, m.showSpread(ctx, c, reqs, out)
	}
	for done := 0; done < len(reqs); {
		end := min(done+showBatchMax, len(reqs))
		n, err := m.showMany(ctx, reqs[done:end], out[done:end])
		done += n
		if err == nil {
			m.multiOK.Store(true)
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if n == 0 && !m.multiOK.Load() && !dbx.ShowCreateMissing(err) {
			m.multiRefused.Store(true)
			m.multiNote.Do(func() {
				m.logf("state %s: SHOW CREATE is read one statement at a time (a query of several statements failed: %v)\n", m.o.Name, err)
			})
			return out, m.showSpread(ctx, c, reqs[done:], out[done:])
		}
		return out, showEach(ctx, c, reqs[done:], out[done:])
	}
	return out, nil
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
		return 0, err
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
