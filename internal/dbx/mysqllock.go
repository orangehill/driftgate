package dbx

import (
	"context"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQLLockRetries is how often RetryMySQLLock tries a step again, and
// MySQLLockWait the usual wait before the first retry.
const (
	MySQLLockRetries = 3
	MySQLLockWait    = 50 * time.Millisecond
)

// mysqlLockStopped reports a statement the server stopped for a lock: a
// deadlock (1213) or a lock wait timeout (1205). A Galera node returns 1213
// to any statement that holds a table another session alters (a
// brute-force abort), also to a read in a snapshot, where a server without
// Galera lets the ALTER wait.
func mysqlLockStopped(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// MySQLLockCause is err, or ctx's error in its place when err is a lock
// stop and ctx has ended (RetryMySQLLock's rule). A step that wraps such a
// cause with more to say, such as what it left undone, replaces the cause
// with it first: RetryMySQLLock would otherwise return ctx's error alone.
func MySQLLockCause(ctx context.Context, err error) error {
	if ctx.Err() != nil && mysqlLockStopped(err) {
		return ctx.Err()
	}
	return err
}

// RetryMySQLLock runs f, again when the server stopped it for a lock (a
// deadlock, 1213, or a lock wait timeout, 1205), up to MySQLLockRetries
// times, after wait, four times as long each time. retrying is given the
// error before each wait. f must start anew on each try: a try's result is
// never kept from one that failed. Any other error is returned at once, and
// the last lock error when every try fails. When ctx ends while a lock is in
// the way, ctx's error is returned at once, without a wait, not the lock
// error, which would blame the server.
func RetryMySQLLock(ctx context.Context, wait time.Duration, f func() error, retrying func(error)) error {
	for try := 0; ; try++ {
		// Cancelled while f ran: the driver then mostly returns ctx's error
		// itself, but a lock error may come back together with the cancel.
		err := MySQLLockCause(ctx, f())
		if !mysqlLockStopped(err) {
			return err
		}
		if try == MySQLLockRetries {
			return err
		}
		retrying(err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 4
	}
}
