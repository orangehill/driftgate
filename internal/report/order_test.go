package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// The same items in another order are one item-order drift, explained as
// such, with the note that names the array rule, in every format.
func TestItemOrderInEveryFormat(t *testing.T) {
	side := func(body string) compare.Side {
		return compare.Side{Record: &snapshot.Record{Status: 200, Headers: [][2]string{{"content-type", "application/json"}}}, Body: []byte(body)}
	}
	ds, notes := compare.OneNotes("get-nodes", side(`{"nodes":[{"id":1},{"id":2},{"id":3}]}`), side(`{"nodes":[{"id":2},{"id":3},{"id":1}]}`), allowlist.Empty(), compare.LevelValues)
	if len(ds) != 1 || ds[0].Kind != compare.KindItemOrder || len(notes) != 1 {
		t.Fatalf("drifts %v, notes %q", ds, notes)
	}
	r := Build("capture:c", "baseline:local", []Result{{ID: "get-nodes", Outcome: Drift, Drifts: ds, Notes: notes}})
	if len(r.Groups) != 1 || r.Groups[0].Signature != "item-order $.nodes" || !strings.Contains(r.Groups[0].Explanation, "items of $.nodes are in a different order") {
		t.Fatalf("groups %+v", r.Groups)
	}
	hint := "key: id, any_order: true"
	want := map[string][]string{
		"text":     {"item-order", "$.nodes", "same 3 items, other order: 1, 2, 0", hint},
		"json":     {`"kind": "item-order"`, `"where": "$.nodes"`, hint},
		"markdown": {"item-order", "items of $.nodes are in a different order", `key: id, any\_order: true`}, // rendered as written
		"junit":    {"item-order $.nodes", hint},
	}
	for _, f := range []string{"text", "json", "markdown", "junit"} {
		var b bytes.Buffer
		if err := r.Write(&b, f, DefaultDrifts); err != nil {
			t.Fatal(err)
		}
		for _, w := range want[f] {
			if !strings.Contains(b.String(), w) {
				t.Errorf("%s without %q:\n%s", f, w, b.String())
			}
		}
	}
}
