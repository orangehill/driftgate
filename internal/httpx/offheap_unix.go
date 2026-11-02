//go:build linux || darwin

package httpx

import "syscall"

// mapped: bodies that are not small are read into mapped regions.
const mapped = true

// mapRegion maps n bytes of private, anonymous memory: address space only,
// until a page is written.
func mapRegion(n int) ([]byte, bool) {
	b, err := syscall.Mmap(-1, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	return b, err == nil
}

func unmapRegion(b []byte) { syscall.Munmap(b) }
