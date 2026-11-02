//go:build unix

package cli

import (
	"bytes"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// SIGTERM (a cancelled CI job, docker stop) cancels the context like Ctrl-C,
// so the command cleans up; a second signal ends the process at once.
func TestSignalContext(t *testing.T) {
	codes := make(chan int, 1)
	exit = func(code int) { codes <- code }
	defer func() { exit = os.Exit }()
	var mu sync.Mutex
	var msg bytes.Buffer
	ctx, stop := signalContext(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return msg.Write(p)
	}))
	defer stop()
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
	syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	select {
	case code := <-codes:
		if code != 130 {
			t.Fatalf("exit code %d after a second signal, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal did not end the process")
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(msg.Bytes(), []byte("terminated: stopping")) {
		t.Fatalf("message: %q", msg.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
