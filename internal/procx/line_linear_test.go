package procx

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestLineLinear: Line takes time in proportion to its input: four times
// the words take about four times as long, while quadratic work would take
// sixteen times. Only the ratio is checked, never a wall-clock limit, so it
// holds on a slow runner, with the race detector, and under the load of a
// whole go test ./...: the sizes are measured in turns, each the best of
// several runs, and a pair that looks quadratic is measured again before it
// fails. Quadratic work fails every attempt.
func TestLineLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	const small, large = 10000, 40000
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
		s, l := c.make(small), c.make(large)
		var ratio float64
		for attempt := 0; attempt < 3; attempt++ {
			ts, tl := linearBest(s, l)
			ratio = float64(tl) / float64(ts)
			if ratio < 9 { // linear: about 4; quadratic: about 16
				break
			}
		}
		if ratio >= 9 {
			t.Errorf("%s: %d words take %.1f times as long as %d, in each of 3 attempts: not linear", c.name, large, ratio, small)
		}
	}
}

// linearBest measures Line on the small and the large input in turns, and
// returns the best time of each over several runs: a run slowed down by
// the scheduler or the collector does not count.
func linearBest(small, large []string) (time.Duration, time.Duration) {
	bs, bl := time.Duration(1<<62), time.Duration(1<<62)
	for range 7 {
		runtime.GC()
		start := time.Now()
		Line(small)
		bs = min(bs, time.Since(start))
		runtime.GC()
		start = time.Now()
		Line(large)
		bl = min(bl, time.Since(start))
	}
	return bs, bl
}
