package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestLockStopped(t *testing.T) {
	for err, want := range map[error]bool{
		&mysql.MySQLError{Number: 1213}:                      true,
		&mysql.MySQLError{Number: 1205}:                      true,
		fmt.Errorf("t: %w", &mysql.MySQLError{Number: 1213}): true,
		&mysql.MySQLError{Number: 1146}:                      false,
		errors.New("Deadlock found when trying to get lock"): false,
		context.Canceled:                                     false,
	} {
		if lockStopped(err) != want {
			t.Errorf("%v: %v", err, !want)
		}
	}
}

// TestMySQLRetryLocked: a fingerprint, restore or seed the server stops for
// a lock (1213, injected) runs again and then gives what a run without the
// stop gives; a retried read sees a write made between the tries; one that
// keeps being stopped returns the server's error after lockRetries tries
// more.
func TestMySQLRetryLocked(t *testing.T) {
	defer func(d time.Duration) { lockRetryWait = d }(lockRetryWait)
	lockRetryWait = time.Millisecond
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock; try restarting transaction"}
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
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
			if n := strings.Count(log.String(), "stopped a"); n != 1 {
				t.Errorf("%d notes of a retry: %q", n, log.String())
			}

			// Stopped every time: the server's error, after lockRetries more.
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
				if !errors.Is(err, deadlock) || calls != lockRetries+1 {
					t.Errorf("%s: %v after %d tries", what, err, calls)
				}
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
