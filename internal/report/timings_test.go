package report

import (
	"strings"
	"testing"
)

func TestCompareTimings(t *testing.T) {
	var results []Result
	base, cur := map[string]int64{}, map[string]int64{}
	add := func(id, outcome string, b, c int64) {
		results = append(results, Result{ID: id, Outcome: outcome})
		base[id], cur[id] = b, c
	}
	for i := 0; i < 16; i++ {
		add(string(rune('a'+i)), Pass, 100, 110)
	}
	add("slow", Drift, 300, 900)         // 3x, +600: much slower
	add("fast-noise", Pass, 3, 9)        // 3x but +6 ms
	add("slow-jitter", Pass, 2000, 2300) // +300 ms but 1.15x
	add("edge", Pass, 200, 400)          // exactly 2x and +200
	add("skipped", Skipped, 10, 5000)
	results = append(results, Result{ID: "untimed", Outcome: Pass})

	tm := CompareTimings(results, base, cur)
	if tm == nil || tm.Requests != 20 {
		t.Fatalf("timings %+v", tm)
	}
	if len(tm.Slower) != 2 || tm.Slower[0].ID != "slow" || tm.Slower[1].ID != "edge" {
		t.Errorf("slower %+v", tm.Slower)
	}
	if tm.Baseline.P50 != 100 || tm.Current.P50 != 110 || tm.Baseline.P95 != 300 || tm.Current.P95 != 900 {
		t.Errorf("percentiles %+v %+v", tm.Baseline, tm.Current)
	}
	r := &Report{Timings: tm}
	if n := r.TimingsNote(); !strings.Contains(n, "p50 100 → 110 ms, p95 300 → 900 ms over 20 requests; 2 requests at least 2× and 200 ms slower: slow 300 → 900 ms, edge 200 → 400 ms") {
		t.Error(n)
	}
	// No times on a side: nothing at all.
	if CompareTimings(results, nil, cur) != nil || CompareTimings(results, base, map[string]int64{}) != nil ||
		CompareTimings([]Result{{ID: "x", Outcome: Pass}}, base, cur) != nil {
		t.Error("timings without times on both sides")
	}
	if (&Report{}).TimingsNote() != "" {
		t.Error("note without timings")
	}
}

func TestMedianTimings(t *testing.T) {
	got := MedianTimings([]map[string]int64{
		{"a": 100, "b": 50, "c": 7},
		{"a": 900, "b": 40},
		{"a": 120, "b": 60},
	})
	if got["a"] != 120 || got["b"] != 50 || got["c"] != 7 || len(got) != 3 {
		t.Errorf("median of three %v", got)
	}
	// An even count: the mean of the two middle times.
	if got := MedianTimings([]map[string]int64{{"a": 100}, {"a": 700}}); got["a"] != 400 {
		t.Errorf("median of two %v", got)
	}
	if got := MedianTimings([]map[string]int64{{"a": 100}, {"a": 700}, {"a": 120}, {"a": 130}}); got["a"] != 125 {
		t.Errorf("median of four %v", got)
	}
}

// TestJUnitTimings: the slower requests are in the JUnit report as
// properties and output, never as a failed case.
func TestJUnitTimings(t *testing.T) {
	r := &Report{Tag: "capture:x", Against: "baseline:local", Results: []Result{{ID: "slow", Outcome: Pass}},
		Timings: &Timings{Requests: 1, Baseline: Percentiles{300, 300}, Current: Percentiles{900, 900}, Slower: []Slower{{ID: "slow", Baseline: 300, Current: 900}}}}
	var b strings.Builder
	if err := r.JUnit(&b, 20); err != nil {
		t.Fatal(err)
	}
	x := b.String()
	for _, want := range []string{`<property name="slower:slow" value="300 → 900 ms">`, `<property name="p95_ms" value="300 → 900">`, "<system-out>response times (information, never a gate)", `failures="0"`} {
		if !strings.Contains(x, want) {
			t.Errorf("JUnit lacks %q:\n%s", want, x)
		}
	}
	if strings.Contains(x, "<failure") {
		t.Errorf("a slower request failed:\n%s", x)
	}
}
