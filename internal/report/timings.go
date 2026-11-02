package report

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// A request is much slower when it takes at least twice as long and at
// least 200 ms more: the ratio alone flags noise on fast requests (3 → 7
// ms), the difference alone the jitter of slow ones (2.0 → 2.3 s); both
// together are a change a user of the frontend notices.
const (
	SlowerFactor = 2
	SlowerMS     = 200
)

// Timings compares the response times of the two sides, in milliseconds
// per request as the capture measured them. It is information only, never
// part of the verdict: one measurement per request on a shared machine is
// too noisy for a gate.
type Timings struct {
	Requests int         `json:"requests"` // compared requests timed on both sides
	Baseline Percentiles `json:"baseline"`
	Current  Percentiles `json:"current"`
	Slower   []Slower    `json:"slower,omitempty"` // the most added time first
}

// Percentiles of the response times of one side (nearest rank).
type Percentiles struct {
	P50 int64 `json:"p50_ms"`
	P95 int64 `json:"p95_ms"`
}

// Slower is a request that became much slower.
type Slower struct {
	ID       string `json:"id"`
	Baseline int64  `json:"baseline_ms"`
	Current  int64  `json:"current_ms"`
}

// CompareTimings compares the times of the compared requests (passed or
// drifted) that both sides timed; nil when a side has no times (a set made
// before timings were kept) or no request has both.
func CompareTimings(results []Result, base, cur map[string]int64) *Timings {
	if len(base) == 0 || len(cur) == 0 {
		return nil
	}
	var bs, cs []int64
	t := &Timings{}
	for _, res := range results {
		if res.Outcome != Pass && res.Outcome != Drift {
			continue
		}
		b, okB := base[res.ID]
		c, okC := cur[res.ID]
		if !okB || !okC {
			continue
		}
		bs, cs = append(bs, b), append(cs, c)
		if c >= SlowerFactor*b && c-b >= SlowerMS {
			t.Slower = append(t.Slower, Slower{ID: res.ID, Baseline: b, Current: c})
		}
	}
	if len(bs) == 0 {
		return nil
	}
	t.Requests = len(bs)
	t.Baseline, t.Current = percentiles(bs), percentiles(cs)
	sort.SliceStable(t.Slower, func(i, j int) bool {
		return t.Slower[i].Current-t.Slower[i].Baseline > t.Slower[j].Current-t.Slower[j].Baseline
	})
	return t
}

func percentiles(ms []int64) Percentiles {
	slices.Sort(ms)
	rank := func(q float64) int64 { return ms[int(math.Ceil(q*float64(len(ms))))-1] }
	return Percentiles{P50: rank(0.5), P95: rank(0.95)}
}

// TimingsNote is one line on the response times, with up to ten much
// slower requests; empty when they were not compared.
func (r *Report) TimingsNote() string {
	t := r.Timings
	if t == nil {
		return ""
	}
	s := fmt.Sprintf("response times (information, never a gate): p50 %d → %d ms, p95 %d → %d ms over %s",
		t.Baseline.P50, t.Current.P50, t.Baseline.P95, t.Current.P95, plural(t.Requests, "request"))
	if n := len(t.Slower); n > 0 {
		var list []string
		for i, x := range t.Slower {
			if i == 10 {
				list = append(list, fmt.Sprintf("and %d more", n-10))
				break
			}
			list = append(list, fmt.Sprintf("%s %d → %d ms", x.ID, x.Baseline, x.Current))
		}
		s += fmt.Sprintf("; %s at least %d× and %d ms slower: %s", plural(n, "request"), SlowerFactor, SlowerMS, strings.Join(list, ", "))
	}
	return s
}
