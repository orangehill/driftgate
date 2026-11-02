//go:build unix

package data

import (
	"runtime"
	"syscall"
)

// peakRSS is the peak resident memory of the process in bytes; 0 when unknown.
func peakRSS() int64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	rss := int64(ru.Maxrss) // kilobytes on Linux, bytes on macOS
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	return rss
}
