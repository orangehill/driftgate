package state

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
)

// lockRetries is how often a step the server stopped for a lock is tried
// again, after lockRetryWait, four times as long each time.
var (
	lockRetries   = 3
	lockRetryWait = 50 * time.Millisecond
)

// lockStopped reports a statement the server stopped for a lock: a deadlock
// (1213) or a lock wait timeout (1205). A Galera node returns 1213 to any
// statement that holds a table another session alters (a brute-force
// abort), also to a read, where a server without Galera lets the ALTER
// wait.
func lockStopped(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// retryingKey marks a context inside retryLocked: a step within a step is
// not tried again on its own, the outer one is.
type retryingKey struct{}

// retryLocked runs f, a step that may run again as a whole, again when the
// server stopped it for a lock (lockStopped), up to lockRetries times.
// Only steps whose every try starts anew are given to it: reads, which read
// the state again, and the restore and the seed, which copy from the
// pristine or working database whatever a try before left (a restore or a
// seed stopped at any point is completed by the next one). A try's result
// is never kept from one that failed. When every try fails, the last
// error is returned.
func (m *MySQL) retryLocked(ctx context.Context, what string, f func(context.Context) error) error {
	if ctx.Value(retryingKey{}) != nil {
		return m.lockTry(ctx, what, f)
	}
	ctx = context.WithValue(ctx, retryingKey{}, true)
	wait := lockRetryWait
	for try := 0; ; try++ {
		err := m.lockTry(ctx, what, f)
		if !lockStopped(err) || try == lockRetries || ctx.Err() != nil {
			return err
		}
		m.retryNote.Do(func() {
			m.logf("state %s: the server stopped a %s for a lock (%v), as a Galera node does when another session alters a table; trying again\n", m.o.Name, what, err)
		})
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 4
	}
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
