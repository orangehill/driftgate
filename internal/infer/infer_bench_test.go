package infer

import (
	"fmt"
	"testing"
)

// BenchmarkPattern: two value sets with versions, ranges, times and ids.
func BenchmarkPattern(b *testing.B) {
	var a, c []string
	for i := range 2 {
		a = append(a, fmt.Sprintf("app v1.2.%d build %d status 200 at 2026-11-0%dT10:00:0%dZ id=%d x-time: %dms tok_%08d", i, 7+i, i+1, i, 100+i, 12+i, i*99))
		c = append(c, fmt.Sprintf("Showing %d-%d of 120 (2 hidden) page %d of 3", i*10+1, i*10+10, i+1))
	}
	for range b.N {
		Pattern(a)
		Pattern(c)
	}
}
