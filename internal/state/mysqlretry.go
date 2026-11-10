package state

import (
	"context"
	"fmt"
	"sync"

	"github.com/orangehill/driftgate/internal/dbx"
)

// lockRetryWait is the wait before the first retry of a step the server
// stopped for a lock (dbx.RetryMySQLLock).
var lockRetryWait = dbx.MySQLLockWait

// retryNoted holds the kinds of step (what) whose retry was noted: one
// note per kind and process, whichever component it was in.
var retryNoted sync.Map

// retryingKey marks a context inside retryLocked: a step within a step is
// not tried again on its own, the outer one is.
type retryingKey struct{}

// retryLocked runs f, a step that may run again as a whole, again when the
// server stopped it for a lock (dbx.RetryMySQLLock), up to
// dbx.MySQLLockRetries times. Only steps whose every try starts anew are
// given to it: reads, which read the state again, and the restore and the
// seed, which copy from the pristine or working database whatever a try
// before left (a restore or a seed stopped at any point is completed by the
// next one). A try's result is never kept from one that failed. When every
// try fails, the last error is returned, or ctx's error as
// dbx.RetryMySQLLock says; a try stopped after ctx ended is not noted.
func (m *MySQL) retryLocked(ctx context.Context, what string, f func(context.Context) error) error {
	if ctx.Value(retryingKey{}) != nil {
		return m.lockTry(ctx, what, f)
	}
	ctx = context.WithValue(ctx, retryingKey{}, true)
	return dbx.RetryMySQLLock(ctx, lockRetryWait, func() error { return m.lockTry(ctx, what, f) }, func(err error) {
		if _, told := retryNoted.LoadOrStore(what, true); !told {
			m.logf("%s", retryNote(m.o.Name, what, err))
		}
	})
}

// retryNote is the one note of a kind of step (what) that runs again.
func retryNote(name, what string, err error) string {
	return fmt.Sprintf("state %s: the server stopped a %s for a lock (%v), as a Galera node does when another session alters a table; it runs again, and this is not noted again\n", name, what, err)
}

// lockTry is one try of f; a test may make it fail first (mysqlHooks).
func (m *MySQL) lockTry(ctx context.Context, what string, f func(context.Context) error) error {
	if m.hooks != nil && m.hooks.lockStop != nil {
		if err := m.hooks.lockStop(what); err != nil {
			return err
		}
	}
	return f(ctx)
}
