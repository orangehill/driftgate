//go:build !windows

package httpx

// resetErrors are the system's errors of a connection the server reset,
// beyond syscall.ECONNRESET and EPIPE (Windows has its own).
var resetErrors []error
