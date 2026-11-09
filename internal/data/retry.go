package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/go-sql-driver/mysql"
)

// lockRetries is how often a read the server stopped for a lock is tried
// again, after lockRetryWait, four times as long each time.
var (
	lockRetries   = 3
	lockRetryWait = 50 * time.Millisecond
)

// readFault, tests only, runs at the end of each try of a retried read,
// once the try has built its state; an error it returns is that try's.
var readFault func(what string) error

// fault is readFault's error for what, nil without one.
func fault(what string) error {
	if readFault == nil {
		return nil
	}
	return readFault(what)
}

// lockStopped reports a statement the server stopped for a lock: a deadlock
// (1213) or a lock wait timeout (1205). A Galera node returns 1213 to any
// statement, a read in a snapshot too, that holds a table another session
// alters.
func lockStopped(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}

// retryRead runs f again when the server stopped it for a lock
// (lockStopped), up to lockRetries times, with one note in log. f is a
// read in a snapshot of its own that writes nothing and builds all its
// state anew on each try. When every try fails, the last error is returned.
func retryRead(ctx context.Context, log io.Writer, what string, f func() error) error {
	wait := lockRetryWait
	for try := 0; ; try++ {
		err := f()
		if !lockStopped(err) || try == lockRetries || ctx.Err() != nil {
			return err
		}
		if try == 0 && log != nil {
			fmt.Fprintf(log, "%s: the server stopped the read for a lock (%v), as a Galera node does when another session alters a table; reading again\n", what, err)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 4
	}
}
