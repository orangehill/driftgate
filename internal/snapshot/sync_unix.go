//go:build unix

package snapshot

import (
	"os"
	"syscall"
)

// syncFile hands a file's data to the disk: fsync. On macOS that is not the
// F_FULLFSYNC of os.File.Sync, which also empties the drive's cache and
// takes milliseconds: a crash of the system no longer loses the file, a
// power cut at the wrong moment still may.
func syncFile(f *os.File) error { return syscall.Fsync(int(f.Fd())) }
