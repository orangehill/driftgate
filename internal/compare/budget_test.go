package compare

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/sheet"
)

// Allocation budgets of the hot paths of a comparison: unlike timings on a
// shared CI runner, the allocations of one run are the same every time, so
// a change that makes a path allocate more fails here. Each budget is
// about 20% above what the code allocated when it was set (the measured
// values are in the comments, linux/amd64 and darwin/arm64 alike); lower a
// budget when an optimization lands, raise it only with a reason.

// perRun is the allocations and bytes of one call of f, averaged over runs
// after a warm-up, as testing.AllocsPerRun counts them.
func perRun(runs int, f func()) (allocs, bytes uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	for i := 0; i < runs; i++ {
		f()
	}
	runtime.ReadMemStats(&b)
	return (b.Mallocs - a.Mallocs) / uint64(runs), (b.TotalAlloc - a.TotalAlloc) / uint64(runs)
}

type budget struct {
	name          string
	allocs, bytes uint64
	f             func()
}

func checkBudgets(t *testing.T, budgets []budget) {
	t.Helper()
	if raceEnabled {
		t.Skip("the race detector allocates on its own")
	}
	for _, c := range budgets {
		allocs, bytes := perRun(3, c.f)
		t.Logf("%s: %d allocs, %d bytes (budget %d, %d)", c.name, allocs, bytes, c.allocs, c.bytes)
		if allocs > c.allocs || bytes > c.bytes {
			t.Errorf("%s allocates %d times, %d bytes per run; budget %d, %d", c.name, allocs, bytes, c.allocs, c.bytes)
		}
	}
}

func TestBudgetCompare(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	const n = 5000 // 1.5 MB
	base := side(200, jsonT, listBody(n, false, false))
	cur := side(200, jsonT, listBody(n, true, false))
	one := side(200, jsonT, strings.Replace(listBody(n, false, false), `"price": 11,`, `"price": 99,`, 1))
	empty, rules := allowlist.Empty(), manyRules(t, 100, true)
	keyed, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.items\n    key: id\n    reason: r\n"))
	if err != nil {
		t.Fatal(err)
	}
	body := listBody(n, false, false)
	first := strings.Index(body, "\n    {")
	inserted := side(200, jsonT, body[:first]+"\n    {\"id\": -1, \"name\": \"new\"},"+body[first:])

	rows := make([][]string, 500)
	for i := range rows {
		rows[i] = []string{fmt.Sprint("user", i), fmt.Sprint(i), "2026-10-30", fmt.Sprint(i * 7)}
	}
	changed := make([][]string, len(rows))
	copy(changed, rows)
	changed[250] = []string{"user250", "x", "2026-10-30", "1750"}
	xb := book(sheet.Rows{Name: "Users", Rows: rows})
	xc := book(sheet.Rows{Name: "Users", Rows: changed})

	checkBudgets(t, []budget{
		// 1.5 MB list, one value changed: both trees are built; measured
		// 302287 allocs, 16.10 MB.
		{"json/one-change", 363_000, 19_400_000, func() { One("req", base, one, empty, LevelValues) }},
		// Every item's time changed (allowed) and one price, 100 rules,
		// some starting with *; measured 332304 allocs, 16.83 MB.
		{"json/rules=100", 399_000, 20_200_000, func() { One("req", base, cur, rules, LevelValues) }},
		// One item inserted at the front, items paired by id; measured
		// 307460 allocs, 17.4 MB.
		{"json/keyed", 369_000, 20_900_000, func() { One("req", base, inserted, keyed, LevelValues) }},
		// An XLSX of 2000 cells, one changed; measured 218689 allocs,
		// 11.67 MB.
		{"xlsx/2000-cells", 262_000, 14_000_000, func() { One("req", xb, xc, empty, LevelValues) }},
	})
}
