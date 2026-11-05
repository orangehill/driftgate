package procx

import (
	"strings"
	"testing"
	"time"
)

// TestLineLinear: Line takes time in proportion to its input: four times
// the words take far less than sixteen times as long.
func TestLineLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	for _, c := range []struct {
		name string
		make func(n int) []string
	}{
		{"argv", func(n int) []string {
			return append([]string{"rm"}, strings.Split(strings.Repeat("file.txt ", n), " ")...)
		}},
		{"script", func(n int) []string { return []string{"sh", "-c", strings.Repeat("a -p x; ", n)} }},
		{"assignments", func(n int) []string { return []string{"x", strings.Repeat("password=b&", n)} }},
		{"substitutions", func(n int) []string { return []string{"sh", "-c", strings.Repeat("$(a) ", n)} }},
	} {
		took := func(n int) time.Duration {
			argv := c.make(n)
			best := time.Duration(1 << 62)
			for range 3 {
				start := time.Now()
				Line(argv)
				best = min(best, time.Since(start))
			}
			return best
		}
		small, large := took(5000), took(20000)
		if large > 10*small+50*time.Millisecond {
			t.Errorf("%s: %v for 5000, %v for 20000", c.name, small, large)
		}
	}
}
