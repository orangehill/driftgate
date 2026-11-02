//go:build !unix

package snapshot

import "os"

// syncFile hands a file's data to the disk.
func syncFile(f *os.File) error { return f.Sync() }
