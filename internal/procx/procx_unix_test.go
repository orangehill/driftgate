//go:build unix

package procx

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTimeoutKillsTheGroup: a command whose child holds its output open
// (make restart, a shell's background job) ends at its time limit with the
// child, instead of waiting for the child.
func TestTimeoutKillsTheGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := Command(ctx, "sh", "-c", "sleep 30 & echo $!; sleep 30")
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	if err := Run(cmd); err == nil {
		t.Fatal("no error after the time limit")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("output %q", out.String())
	}
	for i := 0; ; i++ {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if i == 100 {
			t.Fatalf("the background child %d still runs", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBackgroundChildAfterSuccess: a command that ends well but leaves a
// child holding its output (it started a server) ended well, soon after.
func TestBackgroundChildAfterSuccess(t *testing.T) {
	WaitDelay = 200 * time.Millisecond
	defer func() { WaitDelay = 5 * time.Second }()
	cmd := Command(context.Background(), "sh", "-c", "sleep 3 & echo started")
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	if err := Run(cmd); err != nil || !strings.Contains(out.String(), "started") {
		t.Fatalf("%v, %q", err, out.String())
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
	cmd = Command(context.Background(), "sh", "-c", "exit 3")
	if err := Run(cmd); err == nil {
		t.Fatal("a failed command succeeded")
	}
}
