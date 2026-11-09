package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// lockRetries is how often a read the server stopped for a lock is tried
// again, after lockRetryWait, four times as long each time.
var (
	lockRetries   = 3
	lockRetryWait = 50 * time.Millisecond
)

// retryNoted holds the reads (what) whose retry was noted: one note per
// read and process.
var retryNoted sync.Map

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
		if _, told := retryNoted.LoadOrStore(what, true); !told && log != nil {
			fmt.Fprintf(log, "%s: the server stopped the read of the %s for a lock (%v), as a Galera node does when another session alters a table; it is read again, and further reads of the %s stopped so are not noted\n", what, what, err, what)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait *= 4
	}
}
