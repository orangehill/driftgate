//go:build !unix

package data

// peakRSS is not known here (Windows has no getrusage).
func peakRSS() int64 { return 0 }
