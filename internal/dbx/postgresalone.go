package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgTerminate ends the other sessions on a database. It leaves out
// autovacuum workers: a role that is not a superuser may not end them
// (before PostgreSQL 18, or on 18 without pg_signal_autovacuum_worker), and
// pg_terminate_backend's error aborts the whole statement, so the sessions
// listed after one would stay. CREATE DATABASE … TEMPLATE and RENAME end
// autovacuum workers themselves. They are the sessions on a database
// without a role (usesysid); backend_type cannot tell them, as it is NULL
// to a role for the sessions of roles it may not see, which must stay in
// the list so that their error is returned. A session the role may not end
// aborts the statement just the same: when it then ends within the
// server's wait while one listed after it stays, the statement fails with
// both errors, and a rerun succeeds. Ending each session in a statement of
// its own would close that gap, at the cost of a race of its own.
const pgTerminate = "SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid() AND usesysid IS NOT NULL"

// How often PostgresAlone tries, and how long it waits between the tries.
const (
	pgAloneTries = 5
	pgAloneWait  = 200 * time.Millisecond
)

// PostgresAlone runs stmt, a statement that needs db to have no other
// sessions (CREATE DATABASE … TEMPLATE db, ALTER DATABASE db RENAME), on
// admin: it ends them first, and tries again, at most pgAloneTries times, while
// stmt fails because a session is still there (55006 object_in_use). Any
// other error is returned at once. A failure to end them is returned with
// that 55006, so that it is not hidden behind "being accessed by other
// users"; when stmt succeeds anyway (the session ended by itself within
// the server's wait), it does not matter. When ctx ends while a session is
// in the way, ctx's error is returned at once, without a wait, not the
// 55006, which would blame the sessions.
func PostgresAlone(ctx context.Context, admin *sql.DB, db, stmt string) error {
	for try := 1; ; try++ {
		_, terr := admin.ExecContext(ctx, pgTerminate, db)
		_, err := admin.ExecContext(ctx, stmt)
		if pe := (*pgconn.PgError)(nil); !errors.As(err, &pe) || pe.Code != "55006" { // object_in_use
			return err
		}
		// Cancelled while stmt ran: pgx then mostly returns ctx's error
		// itself, but a 55006 may come back together with the cancel.
		// Too narrow a window for a test.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terr != nil {
			return fmt.Errorf("%w; ending the other sessions on %s: %w", err, db, terr)
		}
		if try == pgAloneTries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pgAloneWait):
		}
	}
}
