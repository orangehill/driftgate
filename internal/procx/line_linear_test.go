package procx

import (
	"fmt"
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
	const small, large = 20000, 80000
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
		var ratios []string
		linear := false
		for attempt := 0; attempt < 3 && !linear; attempt++ {
			ts, tl := linearBest(s, l)
			ratio := float64(tl) / float64(ts)
			ratios = append(ratios, fmt.Sprintf("%.1f (%v, %v)", ratio, ts, tl))
			linear = ratio < 9 // linear: about 4; quadratic: about 16
		}
		if !linear {
			t.Errorf("%s: %d words take %s times as long as %d, in 3 attempts: not linear", c.name, large, strings.Join(ratios, ", "), small)
		}
	}
}

// linearBest measures Line on the small and the large input in turns, and
// returns the best time of each over several runs: a run slowed down by
// the scheduler or the collector does not count. Each run calls Line as
// many times as the small input needs to take 5 ms, the same count for
// both sizes, so a coarse clock (Windows) never measures a run as zero.
func linearBest(small, large []string) (time.Duration, time.Duration) {
	reps := 1
	for {
		start := time.Now()
		for range reps {
			Line(small)
		}
		if time.Since(start) >= 5*time.Millisecond || reps >= 1<<16 {
			break
		}
		reps *= 2
	}
	run := func(args []string) time.Duration {
		runtime.GC()
		start := time.Now()
		for range reps {
			Line(args)
		}
		return time.Since(start)
	}
	bs, bl := time.Duration(1<<62), time.Duration(1<<62)
	for range 7 {
		bs = min(bs, run(small))
		bl = min(bl, run(large))
	}
	return bs, bl
}
