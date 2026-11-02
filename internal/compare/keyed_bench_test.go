package compare

import (
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// BenchmarkKeyed compares a 1.5 MB list (listBody) with the same list that
// has one item more at the front: by position every item drifts, by key
// one item is new. "reordered" moves the last item to the front.
func BenchmarkKeyed(b *testing.B) {
	const n = 5000
	jsonT := [][2]string{{"content-type", "application/json"}}
	body := listBody(n, false, false)
	first := strings.Index(body, "\n    {")
	inserted := body[:first] + "\n    {\"id\": -1, \"name\": \"new\"}," + body[first:]
	base, cur := side(200, jsonT, body), side(200, jsonT, inserted)
	last := strings.LastIndex(body, ",\n    {")
	end := strings.LastIndex(body, "\n  ]")
	moved := side(200, jsonT, body[:first]+body[last+1:end]+","+body[first:last]+body[end:])
	keyed, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.items\n    key: id\n    any_order: true\n    reason: bench\n"))
	if err != nil {
		b.Fatal(err)
	}
	run := func(b *testing.B, allow *allowlist.List, cur Side) {
		b.ReportAllocs()
		for b.Loop() {
			One("req", base, cur, allow, LevelValues)
		}
	}
	b.Run("insert/position", func(b *testing.B) { run(b, allowlist.Empty(), cur) })
	b.Run("insert/key", func(b *testing.B) { run(b, keyed, cur) })
	b.Run("reordered/key", func(b *testing.B) { run(b, keyed, moved) })
}
