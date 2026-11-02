//go:build !unix

package files

// Without these flags, the type of the opened file is still checked before
// it is read.
const (
	openNonblock = 0
	openNoFollow = 0
)
