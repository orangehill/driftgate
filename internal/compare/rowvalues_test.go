package compare

import (
	"slices"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestRowValues: a row that changed alike on both sides (the same kind
// and columns) but got another value drifts, naming the column; a column
// learned as volatile on either side, a column only one side named a
// value of (ignored, or a set that recorded none) and a comparison below
// the values level do not.
func TestRowValues(t *testing.T) {
	rec := func(volatile []string, rows ...snapshot.RowChange) *snapshot.Record {
		return &snapshot.Record{Version: 1, ID: "x", Status: 200, Headers: [][2]string{}, Body: snapshot.Body{SHA256: "a"},
			SideEffects: &snapshot.SideEffects{Items: []string{"db/table:follows"}, Rows: rows, Volatile: volatile}}
	}
	row := func(change string, values map[string]string, cols ...string) snapshot.RowChange {
		return snapshot.RowChange{Item: "db/table:follows", Key: "id=1", Change: change, Columns: cols, Values: values}
	}
	cases := []struct {
		name      string
		base, cur *snapshot.Record
		level     Level
		want      string
		values    []string
	}{
		{"same values", rec(nil, row("updated", map[string]string{"target": "a"}, "target")), rec(nil, row("updated", map[string]string{"target": "a"}, "target")), LevelValues, "", nil},
		{"update", rec(nil, row("updated", map[string]string{"target": "a", "n": "1"}, "n", "target")), rec(nil, row("updated", map[string]string{"target": "b", "n": "1"}, "n", "target")), LevelValues,
			"updated n,target → updated n,target; other value: target", []string{"db/table:follows.target"}},
		{"insert, two columns", rec(nil, row("inserted", map[string]string{"target": "a", "n": "1"})), rec(nil, row("inserted", map[string]string{"target": "b", "n": "2"})), LevelValues,
			"inserted → inserted; other values: n,target", []string{"db/table:follows.n", "db/table:follows.target"}},
		{"volatile in the baseline", rec([]string{"db/table:follows.target"}, row("inserted", map[string]string{"target": "a"})), rec(nil, row("inserted", map[string]string{"target": "b"})), LevelValues, "", nil},
		{"volatile in the capture", rec(nil, row("inserted", map[string]string{"target": "a"})), rec([]string{"db/table:follows.target"}, row("inserted", map[string]string{"target": "b"})), LevelValues, "", nil},
		{"ignored on one side", rec(nil, row("inserted", map[string]string{"target": "a", "seen": "1"})), rec(nil, row("inserted", map[string]string{"target": "a"})), LevelValues, "", nil},
		{"baseline without values", rec(nil, row("inserted", nil)), rec(nil, row("inserted", map[string]string{"target": "b"})), LevelValues, "", nil},
		{"shape level", rec(nil, row("inserted", map[string]string{"target": "a"})), rec(nil, row("inserted", map[string]string{"target": "b"})), LevelShape, "", nil},
		{"another kind stays a kind drift", rec(nil, row("inserted", map[string]string{"target": "a"})), rec(nil, row("updated", map[string]string{"target": "b"}, "target")), LevelValues, "inserted → updated target", nil},
	}
	// A set compared against that was captured once: not compared.
	b, v := rec(nil, row("inserted", map[string]string{"target": "a"})), rec(nil, row("inserted", map[string]string{"target": "b"}))
	if got := One("x", Side{Record: b, NoRowValues: true}, Side{Record: v}, allowlist.Empty(), LevelValues); len(got) != 0 {
		t.Errorf("captured once: %+v", got)
	}
	for _, c := range cases {
		got := One("x", Side{Record: c.base}, Side{Record: c.cur}, allowlist.Empty(), c.level)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: drifts %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Kind != KindSideEffect || got[0].Where != "side-effects:db/table:follows[id=1]" || got[0].Baseline+" → "+got[0].Current != c.want || !slices.Equal(got[0].RowValues, c.values) {
			t.Errorf("%s: drifts %+v, want %s", c.name, got, c.want)
		}
	}
}
