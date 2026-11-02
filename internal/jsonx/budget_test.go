package jsonx

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// Allocation budgets (see compare's budget_test.go): about 20% above what
// was measured when they were set, in the comments.
func TestBudgetStyleAndValid(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates on its own")
	}
	var b strings.Builder
	b.WriteString("{\n  \"items\": [")
	for i := 0; i < 5000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "\n    {\n      \"id\": %d,\n      \"name\": \"Item \\u0107 %d\",\n      \"price\": %d.5,\n      \"tags\": [\"a\", \"b\"]\n    }", i, i, i)
	}
	b.WriteString("\n  ]\n}\n")
	raw := []byte(b.String())
	perRun := func(f func()) (uint64, uint64) {
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		f()
		var x, y runtime.MemStats
		runtime.ReadMemStats(&x)
		for i := 0; i < 3; i++ {
			f()
		}
		runtime.ReadMemStats(&y)
		return (y.Mallocs - x.Mallocs) / 3, (y.TotalAlloc - x.TotalAlloc) / 3
	}
	for _, c := range []struct {
		name          string
		allocs, bytes uint64
		f             func()
	}{
		// StyleOf reads the layout in one pass without allocating: 0, 0.
		{"StyleOf", 0, 0, func() { StyleOf(raw) }},
		// Valid builds no tree, only the decoded strings; measured 34993
		// allocs, 238432 bytes.
		{"Valid", 42_000, 286_000, func() { Valid(raw) }},
	} {
		allocs, bytes := perRun(c.f)
		t.Logf("%s: %d allocs, %d bytes (budget %d, %d)", c.name, allocs, bytes, c.allocs, c.bytes)
		if allocs > c.allocs || bytes > c.bytes {
			t.Errorf("%s allocates %d times, %d bytes per run; budget %d, %d", c.name, allocs, bytes, c.allocs, c.bytes)
		}
	}
}
