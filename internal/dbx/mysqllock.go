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

// RetryMySQLLock runs f, again when the server stopped it for a lock (a
// deadlock, 1213, or a lock wait timeout, 1205), up to MySQLLockRetries
// times, after wait, four times as long each time. retrying is given the
// error before each wait. f must start anew on each try: a try's result is
// never kept from one that failed. When every try fails, or ctx ends, the
// last error is returned.
func RetryMySQLLock(ctx context.Context, wait time.Duration, f func() error, retrying func(error)) error {
	for try := 0; ; try++ {
		err := f()
		if !mysqlLockStopped(err) || try == MySQLLockRetries || ctx.Err() != nil {
			return err
		}
		retrying(err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 4
	}
}
