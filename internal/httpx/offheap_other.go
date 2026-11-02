//go:build !(linux || darwin)

package httpx

// Elsewhere every body is read on the heap.
const mapped = false

func mapRegion(n int) ([]byte, bool) { return nil, false }

func unmapRegion(b []byte) {}
