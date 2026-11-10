package data

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/orangehill/driftgate/internal/dbx"
)

// lockRetryWait is the wait before the first retry of a read the server
// stopped for a lock (dbx.RetryMySQLLock).
var lockRetryWait = 50 * time.Millisecond

// retryNoted holds the reads (what) whose retry was noted: one note per
// read and process.
var retryNoted sync.Map

// retryNote is the one note of a read (what) that runs again.
func retryNote(what string, err error) string {
	return fmt.Sprintf("%s: the server stopped the read of the %s for a lock (%v), as a Galera node does when another session alters a table; it is read again, and this is not noted again\n", what, what, err)
}

// readFault, tests only, runs at the end of each try of a retried read
// (retryRead), once the try has built its state; an error it returns is
// that try's.
var readFault func(what string) error

// retryRead runs f again when the server stopped it for a lock
// (dbx.RetryMySQLLock), up to dbx.MySQLLockRetries times, with one note in
// log. f is a read in a snapshot of its own that writes nothing and builds
// all its state anew on each try. When every try fails, the last error is
// returned.
func retryRead(ctx context.Context, log io.Writer, what string, f func() error) error {
	return dbx.RetryMySQLLock(ctx, lockRetryWait, func() error {
		if err := f(); err != nil || readFault == nil {
			return err
		}
		return readFault(what)
	}, func(err error) {
		if _, told := retryNoted.LoadOrStore(what, true); !told && log != nil {
			fmt.Fprint(log, retryNote(what, err))
		}
	})
}
