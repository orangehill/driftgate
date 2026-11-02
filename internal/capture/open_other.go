//go:build !unix

package capture

// Without the flag, the type of the opened file is still checked before it
// is read.
const openNonblock = 0
