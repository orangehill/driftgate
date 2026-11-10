package dbx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQLLockRetries is how often RetryMySQLLock tries a step again, and
// MySQLLockWait the usual wait before the first retry.
const (
	MySQLLockRetries = 3
	MySQLLockWait    = 50 * time.Millisecond
)

// MySQLLockStopped reports a statement the server stopped for a lock: a
// deadlock (1213) or a lock wait timeout (1205). A Galera node returns 1213
// to any statement that holds a table another session alters (a
// brute-force abort), also to a read in a snapshot, where a server without
// Galera lets the ALTER wait.
func MySQLLockStopped(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// MySQLLockCause is err as a step reports it once ctx has ended: a lock
// stop then is the cancel's doing. A bare lock error (the server's own,
// unwrapped) is replaced with ctx's error, as the lock error would blame
// the server. One wrapped with what the step has to say, such as what it
// left undone, keeps that text, which the user must act on, and gets ctx's
// error added, so that errors.Is(err, ctx.Err()) holds. Any other err, and
// any err while ctx runs, is returned as it is. A step that wraps a lock
// error calls it on that error once its own work is done.
func MySQLLockCause(ctx context.Context, err error) error {
	if ctx.Err() == nil || !MySQLLockStopped(err) || errors.Is(err, ctx.Err()) {
		return err
	}
	if _, bare := err.(*mysql.MySQLError); bare {
		return ctx.Err()
	}
	return fmt.Errorf("%w (interrupted: %w)", err, ctx.Err())
}

// RetryMySQLLock runs f, again when the server stopped it for a lock (a
// deadlock, 1213, or a lock wait timeout, 1205), up to MySQLLockRetries
// times, after wait, four times as long each time. retrying is given the
// error before each wait. f must start anew on each try: a try's result is
// never kept from one that failed. Any other error is returned at once, and
// the last lock error when every try fails. When ctx ends while a lock is in
// the way, the error is returned at once, without a wait, as MySQLLockCause
// names it: ctx's error in place of a bare lock error.
func RetryMySQLLock(ctx context.Context, wait time.Duration, f func() error, retrying func(error)) error {
	for try := 0; ; try++ {
		err := f()
		if !MySQLLockStopped(err) {
			return err
		}
		// Cancelled while f ran: the driver then mostly returns ctx's error
		// itself, but a lock error may come back together with the cancel.
		if ctx.Err() != nil {
			return MySQLLockCause(ctx, err)
		}
		if try == MySQLLockRetries {
			return err
		}
		retrying(err)
		select {
		case <-ctx.Done():
			return MySQLLockCause(ctx, err)
		case <-time.After(wait):
		}
		wait *= 4
	}
}
