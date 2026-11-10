package dbx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestMySQLLockStopped(t *testing.T) {
	for err, want := range map[error]bool{
		&mysql.MySQLError{Number: 1213}:                                    true,
		&mysql.MySQLError{Number: 1205}:                                    true,
		fmt.Errorf("t: %w", &mysql.MySQLError{Number: 1213}):               true,
		errors.Join(errors.New("source"), &mysql.MySQLError{Number: 1213}): true,
		&mysql.MySQLError{Number: 1146}:                                    false,
		errors.New("Deadlock found when trying to get lock"):               false,
		context.Canceled: false,
	} {
		if mysqlLockStopped(err) != want {
			t.Errorf("%v: %v", err, !want)
		}
	}
}

// TestMySQLLockCause: once ctx has ended, a bare lock error becomes ctx's
// error, and a wrapped one keeps its text with ctx's error added (once);
// any other error, and any error while ctx runs, stays as it is.
func TestMySQLLockCause(t *testing.T) {
	bare := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	wrapped := fmt.Errorf("%w. Dropped from db: view old", bare)
	other := &mysql.MySQLError{Number: 1146}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if got := MySQLLockCause(ended, bare); got != context.Canceled {
		t.Errorf("bare: %v", got)
	}
	got := MySQLLockCause(ended, wrapped)
	if !errors.Is(got, context.Canceled) || !errors.Is(got, bare) ||
		got.Error() != "Error 1213: Deadlock. Dropped from db: view old (interrupted: context canceled)" {
		t.Errorf("wrapped: %v", got)
	}
	if again := MySQLLockCause(ended, got); again != got {
		t.Errorf("wrapped twice: %v", again)
	}
	for _, c := range []struct {
		ctx context.Context
		err error
	}{{ended, other}, {ended, nil}, {context.Background(), bare}, {context.Background(), wrapped}} {
		if got := MySQLLockCause(c.ctx, c.err); got != c.err {
			t.Errorf("%v (ctx %v): %v", c.err, c.ctx.Err(), got)
		}
	}
}

// TestRetryMySQLLock: a step stopped every time is tried
// MySQLLockRetries+1 times, noted before each wait, and waits four times
// as long each time. A context that ends while a lock is in the way stops
// the retries at once with its own error, not the lock error: when it ends
// before the first try, during a try (also the last), before a wait or
// during one; a try stopped after it ended is not noted. A lock error
// wrapped with more to say keeps that text, with the cancel added.
func TestRetryMySQLLock(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: 1213, Message: "Deadlock"}
	// run calls RetryMySQLLock with an f that is stopped every time, records
	// the calls of f and retrying, and runs try within f and note within
	// retrying; a call that does not end in time fails the test instead of
	// hanging it.
	run := func(ctx context.Context, wait time.Duration, try, note func()) ([]string, error) {
		t.Helper()
		var calls []string
		done := make(chan error, 1)
		go func() {
			done <- RetryMySQLLock(ctx, wait,
				func() error { calls = append(calls, "f"); try(); return deadlock },
				func(error) { calls = append(calls, "note"); note() })
		}()
		select {
		case err := <-done:
			return calls, err
		case <-time.After(5 * time.Second):
			t.Fatal("RetryMySQLLock did not return")
			return nil, nil
		}
	}
	nothing := func() {}

	start := time.Now()
	calls, err := run(context.Background(), 2*time.Millisecond, nothing, nothing)
	elapsed := time.Since(start)
	want := []string{"f", "note", "f", "note", "f", "note", "f"}
	if !errors.Is(err, deadlock) || !reflect.DeepEqual(calls, want) {
		t.Errorf("stopped every time: %v after %v, want %v", err, calls, want)
	}
	if elapsed < 42*time.Millisecond { // 2 + 8 + 32 ms
		t.Errorf("waited %v, want at least 42ms", elapsed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls, err = run(ctx, time.Hour, nothing, nothing)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, []string{"f"}) {
		t.Errorf("cancelled before: %v after %v", err, calls)
	}

	// The lock error comes back together with the cancel.
	ctx, cancel = context.WithCancel(context.Background())
	calls, err = run(ctx, time.Hour, cancel, nothing)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, []string{"f"}) {
		t.Errorf("cancelled during a try: %v after %v", err, calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	calls, err = run(ctx, time.Hour, nothing, cancel)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, []string{"f", "note"}) {
		t.Errorf("cancelled before the wait: %v after %v", err, calls)
	}

	// A lock error wrapped with what the step left undone keeps that text
	// after a cancel, and still reports it.
	ctx, cancel = context.WithCancel(context.Background())
	err = RetryMySQLLock(ctx, time.Hour, func() error {
		cancel()
		return fmt.Errorf("%w. Dropped from db: view old", deadlock)
	}, func(error) { t.Error("noted") })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Dropped from db: view old") {
		t.Errorf("cancelled, wrapped: %v", err)
	}

	// Cancelled during the last try: the cancel, not the lock error of a
	// step that ran out of tries.
	ctx, cancel = context.WithCancel(context.Background())
	tries := 0
	calls, err = run(ctx, time.Millisecond, func() {
		if tries++; tries == MySQLLockRetries+1 {
			cancel()
		}
	}, nothing)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, want) {
		t.Errorf("cancelled during the last try: %v after %v", err, calls)
	}

	// Cancelled 20ms into an hour's wait: it returns within a few ms of the
	// cancel, and f is not tried again.
	ctx, cancel = context.WithCancel(context.Background())
	var cancelled time.Time
	calls, err = run(ctx, time.Hour, nothing, func() {
		time.AfterFunc(20*time.Millisecond, func() { cancelled = time.Now(); cancel() })
	})
	late := time.Since(cancelled)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(calls, []string{"f", "note"}) {
		t.Errorf("cancelled during the wait: %v after %v", err, calls)
	}
	if late > 150*time.Millisecond {
		t.Errorf("returned %v after the cancel", late)
	}
}
