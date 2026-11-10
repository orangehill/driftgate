package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbx"
)

// TestRetryNote: the note reads as a sentence for every kind of step
// retryLocked runs.
func TestRetryNote(t *testing.T) {
	err := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	for _, what := range []string{"fingerprint", "restore", "seed", "read of rows", "query"} {
		want := "state db: the server stopped a " + what + " for a lock (Error 1213: Deadlock), as a Galera node does when another session alters a table; it runs again, and this is not noted again\n"
		if got := retryNote("db", what, err); got != want {
			t.Errorf("%s:\n%q\nwant\n%q", what, got, want)
		}
	}
}

// TestRetryLockedNested: a step within a step is not tried again on its
// own, the outer one is: an inner step stopped every time runs once per
// try of the outer one, dbx.MySQLLockRetries+1 times in all, not that
// squared.
func TestRetryLockedNested(t *testing.T) {
	defer func(d time.Duration) { lockRetryWait = d }(lockRetryWait)
	lockRetryWait = time.Millisecond
	retryNoted.Clear()
	defer retryNoted.Clear()
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	m := &MySQL{o: Options{Name: "db", Log: io.Discard}}
	outer, inner := 0, 0
	err := m.retryLocked(context.Background(), "restore", func(ctx context.Context) error {
		outer++
		return m.retryLocked(ctx, "query", func(context.Context) error {
			inner++
			return deadlock
		})
	})
	if !errors.Is(err, deadlock) || outer != dbx.MySQLLockRetries+1 || inner != dbx.MySQLLockRetries+1 {
		t.Errorf("%v after %d outer and %d inner tries", err, outer, inner)
	}
}

// TestMySQLRetryLocked: a fingerprint, restore or seed the server stops for
// a lock (1213, injected) runs again and then gives what a run without the
// stop gives; a retried read sees a write made between the tries; one that
// keeps being stopped returns the server's error after
// dbx.MySQLLockRetries tries more.
func TestMySQLRetryLocked(t *testing.T) {
	defer func(d time.Duration) { lockRetryWait = d }(lockRetryWait)
	lockRetryWait = time.Millisecond
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			retryNoted.Clear()
			work, pristine := "dgt_retry", "dgt_retry_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db, "CREATE TABLE t (id INT PRIMARY KEY, n INT)", "INSERT INTO t VALUES (1, 1), (2, 2)")
			var log bytes.Buffer
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Log: &log})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			// stop fails the first n tries of what, and runs between before
			// the try after a stop.
			calls := 0
			stop := func(what string, n int, between func()) {
				calls = 0
				m.hooks = &mysqlHooks{lockStop: func(w string) error {
					if w != what {
						return nil
					}
					calls++
					if calls <= n {
						if between != nil {
							between()
						}
						return deadlock
					}
					return nil
				}}
			}

			stop("seed", 2, nil)
			if err := m.Seed(ctx); err != nil || calls != 3 {
				t.Fatalf("seed: %v after %d tries", err, calls)
			}
			m.hooks = nil
			fp0 := mustFingerprint(t, m)

			// A write between the tries is seen by the try that passes.
			stop("fingerprint", 1, func() { mustExec(t, db, "UPDATE t SET n = 5 WHERE id = 1") })
			m.forget()
			fp1, err := m.Fingerprint(ctx, nil)
			if err != nil || calls != 2 {
				t.Fatalf("fingerprint: %v after %d tries", err, calls)
			}
			if d := Diff(fp0, fp1); len(d) != 1 || d[0] != "table:t" {
				t.Fatalf("the retried fingerprint missed the write: %v", d)
			}

			stop("restore", 2, nil)
			if err := m.Restore(ctx, []string{"table:t"}); err != nil || calls != 3 {
				t.Fatalf("restore: %v after %d tries", err, calls)
			}
			m.hooks = nil
			if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
				t.Fatalf("after the retried restore: %v", d)
			}
			// One note per kind of step, naming it.
			for _, what := range []string{"seed", "fingerprint", "restore"} {
				if n := strings.Count(log.String(), "stopped a "+what+" for a lock"); n != 1 {
					t.Errorf("%d notes of a retried %s: %q", n, what, log.String())
				}
			}
			noted := log.Len()

			// Stopped every time: the server's error, after dbx.MySQLLockRetries more.
			for _, what := range []string{"fingerprint", "restore", "seed"} {
				stop(what, 1000, nil)
				var err error
				switch what {
				case "fingerprint":
					m.forget()
					_, err = m.Fingerprint(ctx, nil)
				case "restore":
					err = m.Restore(ctx, []string{"table:t"})
				case "seed":
					err = m.Seed(ctx)
				}
				if !errors.Is(err, deadlock) || calls != dbx.MySQLLockRetries+1 {
					t.Errorf("%s: %v after %d tries", what, err, calls)
				}
			}
			if log.Len() != noted {
				t.Errorf("a kind of step noted again: %q", log.String()[noted:])
			}
			// Another error is not tried again.
			calls = 0
			m.hooks = &mysqlHooks{lockStop: func(string) error { calls++; return &mysql.MySQLError{Number: 1146} }}
			if _, err := m.Fingerprint(ctx, nil); err == nil || calls != 1 {
				t.Errorf("1146: %v after %d tries", err, calls)
			}
		})
	}
}
