package compare

import (
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// listBody is a pretty-printed list of n items (about 300 bytes each). With
// cur, every item has another updated_at (allow-listed) and item 7 another
// price; with deep, item 3 holds one more level of nesting.
func listBody(n int, cur, deep bool) string {
	var b strings.Builder
	b.WriteString("{\n  \"meta\": {\n    \"generated\": \"2026-10-29T10:00:00Z\",\n    \"build\": \"b1\"\n  },\n  \"items\": [")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		at, price := "2026-10-29T10:00:00Z", 10+i%7
		if cur {
			at = "2026-10-29T11:11:11Z"
			if i == 7 {
				price++
			}
		}
		extra := `"tags": ["a", "b"]`
		if deep && i == 3 {
			extra = "\"tags\": [\n        {\n          \"a\": 1\n        }\n      ]"
		}
		fmt.Fprintf(&b, "\n    {\n      \"id\": %d,\n      \"name\": \"Item %d\",\n      \"price\": %d,\n      \"updated_at\": %q,\n      \"owner\": {\n        \"id\": %d,\n        \"email\": \"u%d@example.com\"\n      },\n      %s\n    }",
			i, i, price, at, i%13, i%13, extra)
	}
	b.WriteString("\n  ]\n}\n")
	return b.String()
}

// manyRules is an allowlist of n rules of every kind: literal JSON paths,
// paths with a wildcard in the middle or at the start, page and cell paths;
// the one that matters is $.items[*].updated_at.
func manyRules(b testing.TB, n int, leadingStar bool) *allowlist.List {
	var y strings.Builder
	y.WriteString("version: 1\nbody:\n")
	for i := 0; i < n; i++ {
		var path string
		switch i % 5 {
		case 0:
			path = fmt.Sprintf("$.meta.field%d", i)
		case 1:
			path = fmt.Sprintf("$.items[*].other%d", i)
		case 2:
			path = fmt.Sprintf("html>body>div#x%d@data-id", i)
		case 3:
			path = fmt.Sprintf("xlsx:Sheet1!B%d", i+1)
		case 4:
			path = fmt.Sprintf("$.data%d[*].id", i)
			if leadingStar {
				path = fmt.Sprintf("*.unused%d", i)
			}
		}
		fmt.Fprintf(&y, "- path: '%s'\n  format: '\\w+'\n  reason: bench\n", path)
	}
	y.WriteString("- path: '$.items[*].updated_at'\n  format: '\\d{4}-\\d{2}-\\d{2}T[\\d:]+Z'\n  reason: time\n")
	l, err := allowlist.Parse([]byte(y.String()))
	if err != nil {
		b.Fatal(err)
	}
	return l
}

// BenchmarkRules compares a 1.5 MB list whose times changed everywhere
// (allow-listed) and one price changed, with allowlists of growing size;
// "star" has rules that start with a wildcard. "one-change" changes a
// single value: everything else is the same source text. "deeper" adds a
// level of nesting in one item, which pretty-printing indents further.
func BenchmarkRules(b *testing.B) {
	const n = 5000
	jsonT := [][2]string{{"content-type", "application/json"}}
	base := side(200, jsonT, listBody(n, false, false))
	cur := side(200, jsonT, listBody(n, true, false))
	one := side(200, jsonT, strings.Replace(listBody(n, false, false), `"price": 11,`, `"price": 99,`, 1))
	deeper := side(200, jsonT, listBody(n, false, true))
	run := func(b *testing.B, allow *allowlist.List, cur Side) {
		b.ReportAllocs()
		for b.Loop() {
			One("req", base, cur, allow, LevelValues)
		}
	}
	b.Run("rules=1", func(b *testing.B) { run(b, manyRules(b, 0, false), cur) })
	b.Run("rules=100", func(b *testing.B) { run(b, manyRules(b, 100, false), cur) })
	b.Run("rules=100/star", func(b *testing.B) { run(b, manyRules(b, 100, true), cur) })
	b.Run("one-change/rules=100/star", func(b *testing.B) { run(b, manyRules(b, 100, true), one) })
	b.Run("deeper", func(b *testing.B) { run(b, allowlist.Empty(), deeper) })
}
