package httpx

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestLoadFailureWindowsReset: a connection Windows reports reset or
// aborted is a load failure, as ECONNRESET is elsewhere.
func TestLoadFailureWindowsReset(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.WSAECONNRESET, syscall.WSAECONNABORTED} {
		err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", e)}
		if got := loadFailure(context.Background(), err); got != "connection reset" {
			t.Errorf("%v: %q", e, got)
		}
	}
}
