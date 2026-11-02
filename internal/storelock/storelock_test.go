package storelock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestMain lets the test binary hold a lock as another process would.
func TestMain(m *testing.M) {
	if dir := os.Getenv("STORELOCK_HOLD"); dir != "" {
		if _, err := Acquire(context.Background(), dir, nil); err != nil {
			os.Exit(1)
		}
		os.Stdout.WriteString("locked\n")
		bufio.NewReader(os.Stdin).ReadString('\n') // until the test lets go
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestAcquireWaitsForAnotherProcess: a store locked by another process is
// waited for, with that process's ID, until it ends (here killed, which
// releases the lock like a clean exit) or the wait is cancelled; within
// one process the lock is taken again without waiting.
func TestAcquireWaitsForAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "STORELOCK_HOLD="+dir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		cmd.Process.Kill()
		t.Fatalf("the other process: %q", line)
	}

	busy := 0
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := Acquire(ctx, dir, func(pid int) {
		busy++
		if pid != cmd.Process.Pid {
			t.Errorf("holder %d, want %d", pid, cmd.Process.Pid)
		}
	})
	if !errors.Is(err, context.DeadlineExceeded) || busy != 1 {
		t.Fatalf("while locked: %v, told %d times", err, busy)
	}

	got := make(chan error, 1)
	go func() {
		release, err := Acquire(context.Background(), dir, nil)
		if err == nil {
			defer release()
			again, err := Acquire(context.Background(), dir, nil) // the same process: no wait
			if err == nil {
				again()
			}
		}
		got <- err
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case err := <-got:
		t.Fatalf("taken while another process holds it: %v", err)
	default:
	}
	cmd.Process.Kill() // SIGKILL: no cleanup of its own
	cmd.Wait()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock of a killed process was not released")
	}
	if Holder(dir) != os.Getpid() {
		t.Fatalf("holder %d", Holder(dir))
	}
}
