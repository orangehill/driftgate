//go:build unix

package files

import "syscall"

// Flags for opening an original: a named pipe or device must not block the
// open, and a symbolic link in place of a file is not followed.
const (
	openNonblock = syscall.O_NONBLOCK
	openNoFollow = syscall.O_NOFOLLOW
)
