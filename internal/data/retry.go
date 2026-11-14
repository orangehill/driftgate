package data

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/orangehill/driftgate/internal/dbx"
)

// lockRetryWait is the wait before the first retry of a read the server
// stopped for a lock (dbx.RetryMySQLLock).
var lockRetryWait = dbx.MySQLLockWait

// retryNoted holds the reads (what) whose retry was noted in a log: one
// note per read and process.
var retryNoted sync.Map

// retryNote is the one note of a read (what) that runs again.
func retryNote(what string, err error) string {
	return fmt.Sprintf("%s: the server stopped the read of the %s for a lock (%v), as a Galera node does when another session alters a table; it is read again, and this is not noted again\n", what, what, err)
}

// readFault, tests only, runs after each try of a retried read
// (retryRead) whose f succeeded, once the try has built its state; an
// error it returns is that try's.
var readFault func(what string) error

// retryRead runs f again when the server stopped it for a lock
// (dbx.RetryMySQLLock), up to dbx.MySQLLockRetries times, with one note in
// log. f is a read in a snapshot of its own that writes nothing and builds
// all its state anew on each try. When every try fails, the last error is
// returned, or, after a cancel, the error as dbx.RetryMySQLLock names it; a
// try stopped after ctx ended is not noted.
func retryRead(ctx context.Context, log io.Writer, what string, f func() error) error {
	return dbx.RetryMySQLLock(ctx, lockRetryWait, func() error {
		if err := f(); err != nil || readFault == nil {
			return err
		}
		return readFault(what)
	}, func(err error) {
		if log == nil {
			return // not noted: a later retry with a log notes it
		}
		if _, told := retryNoted.LoadOrStore(what, true); !told {
			fmt.Fprint(log, retryNote(what, err))
		}
	})
}
