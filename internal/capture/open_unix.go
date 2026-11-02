//go:build unix

package capture

import "syscall"

// openNonblock keeps a named pipe or device put in place of an upload file
// from blocking the open; the opened file is checked to be regular.
const openNonblock = syscall.O_NONBLOCK
