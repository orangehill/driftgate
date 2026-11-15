package compare

import (
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

func parseAllow(t testing.TB, src string) *allowlist.List {
	t.Helper()
	if src == "" {
		return allowlist.Empty()
	}
	l, err := allowlist.Parse([]byte("version: 1\n" + src))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// An array no rule pairs by key whose items came in another order is one
// item-order drift at the array, not a drift at every index; what differs
// within the items paired by content is still reported.
func TestArrayOrder(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	times := "body:\n  - path: $.data[*].at\n    format: '\\d\\d:\\d\\d'\n    reason: time\n"
	cases := []struct {
		name, allow, base, cur, want, note string
		level                              Level
	}{
		{name: "objects", base: `{"data":[{"id":1,"n":"a"},{"id":2,"n":"b"},{"id":3,"n":"c"}]}`, cur: `{"data":[{"id":3,"n":"c"},{"id":1,"n":"a"},{"id":2,"n":"b"}]}`,
			want: "item-order $.data 3 items → same 3 items, other order: 2, 0, 1",
			note: "$.data: the same items in another order; if the order does not matter, add under arrays: in allowlist.yaml: - {request: 'req', path: '$.data', key: id, any_order: true, reason: '…'}"},
		{name: "nested scalars", base: `{"data":[{"roles":["a","b"]},{"roles":["c"]}]}`, cur: `{"data":[{"roles":["b","a"]},{"roles":["c"]}]}`,
			want: "item-order $.data[0].roles 2 items → same 2 items, other order: 1, 0",
			note: "$.data[*].roles: the same items in another order; if the order does not matter, add under arrays: in allowlist.yaml: - {request: 'req', path: '$.data[*].roles', any_order: true, reason: '…'}"},
		{name: "a repeated item", base: `[1,1,2,3]`, cur: `[1,2,1,3]`,
			want: "item-order $ 4 items → same 4 items, other order: 0, 2, 1, 3", note: "*"},
		{name: "a repeated item, another multiset", base: `[1,1,2]`, cur: `[1,2,2]`,
			want: "value $[1] 1 → 2"},
		{name: "an id that two items share is no key", base: `[{"id":1,"n":"a"},{"id":1,"n":"b"}]`, cur: `[{"id":1,"n":"b"},{"id":1,"n":"a"}]`,
			want: "item-order $ 2 items → same 2 items, other order: 1, 0",
			note: "$: the same items in another order; if the order does not matter, add under arrays: in allowlist.yaml: - {request: 'req', path: '$', any_order: true, reason: '…'}"},
		{name: "same under the allowlist", allow: times, base: `{"data":[{"n":"a","at":"10:00"},{"n":"b","at":"10:01"}]}`, cur: `{"data":[{"n":"b","at":"11:00"},{"n":"a","at":"11:01"}]}`,
			want: "item-order $.data 2 items → same 2 items, other order: 1, 0", note: "*"},
		{name: "a value written otherwise in a moved item", base: `["é","b"]`, cur: `["b","\u00e9"]`,
			want: "item-order $ 2 items → same 2 items, other order: 1, 0\nencoding $[0] \"é\" → \"\\u00e9\"", note: "*"},
		{name: "one item changed and moved", base: `[{"n":"a"},{"n":"b"},{"n":"c"}]`, cur: `[{"n":"c"},{"n":"a"},{"n":"x"}]`,
			want: "item-order $ 3 items → 2 of 3 items the same, other order: 2, 0, 1\nvalue $[1].n \"b\" → \"x\"",
			note: "$: the items in another order, one of them changed; if the order does not matter, add under arrays: in allowlist.yaml: - {request: 'req', path: '$', any_order: true, reason: '…'}"},
		{name: "two items changed: by position", base: `["a","b","c"]`, cur: `["c","x","y"]`,
			want: "value $[0] \"a\" → \"c\"\nvalue $[1] \"b\" → \"x\"\nvalue $[2] \"c\" → \"y\""},
		{name: "one item changed in place", base: `["a","b","c"]`, cur: `["a","x","c"]`,
			want: "value $[1] \"b\" → \"x\""},
		{name: "any order allowed", allow: "arrays:\n  - path: $.data\n    any_order: true\n    reason: no ORDER BY\n", base: `{"data":[1,2,3]}`, cur: `{"data":[3,1,2]}`},
		{name: "any order still reports a changed item", allow: "arrays:\n  - path: $.data\n    any_order: true\n    reason: no ORDER BY\n", base: `{"data":["a","b","c"]}`, cur: `{"data":["c","a","x"]}`,
			want: "value $.data[1] \"b\" → \"x\""},
		{name: "any order is no any length", allow: "arrays:\n  - path: $.data\n    any_order: true\n    reason: no ORDER BY\n", base: `{"data":[1,2,3]}`, cur: `{"data":[3,1]}`,
			want: "array-length $.data 3 → 2\nvalue $.data[0] 1 → 3\nvalue $.data[1] 2 → 1"},
		{name: "shape: the same shapes stay", level: LevelShape, base: `[{"a":1},{"a":2}]`, cur: `[{"a":2},{"a":1}]`},
		{name: "shape: other shapes", level: LevelShape, base: `[{"a":1},{"b":2}]`, cur: `[{"b":3},{"a":4}]`,
			want: "item-order $ 2 items → same 2 items, other order: 1, 0", note: "*"},
		{name: "types", level: LevelTypes, base: `[1,"x",true]`, cur: `["y",2,true]`,
			want: "item-order $ 3 items → same 3 items, other order: 1, 0, 2", note: "*"},
		{name: "types: the same types stay", level: LevelTypes, base: `[1,2]`, cur: `[2,1]`},
	}
	for _, c := range cases {
		allow := parseAllow(t, c.allow)
		level := c.level
		if level == 0 {
			level = LevelValues
		}
		for _, fast := range []bool{true, false} {
			for _, witnessed := range []bool{false, true} {
				FastPaths = fast
				ds, notes := oneNotes("req", side(200, jsonT, c.base), side(200, jsonT, c.cur), allow, level, witnessed)
				FastPaths = true
				if got := driftList(ds); got != sortedLines(c.want) {
					t.Errorf("%s (fast %v, witnessed %v):\ngot\n%s\nwant\n%s", c.name, fast, witnessed, got, sortedLines(c.want))
				}
				note := strings.Join(notes, "\n")
				if c.note == "*" && note == "" || c.note != "*" && note != c.note {
					t.Errorf("%s: notes %q, want %q", c.name, note, c.note)
				}
			}
		}
	}
}

// An expectation of a reordering names the items: another reordering that
// reads the same (the same positions, past what the drift shows, or the same
// item changed into something else) has another digest.
func TestArrayOrderWitness(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	digest := func(base, cur string) map[string]string {
		ds, _ := OneWitnessed("req", side(200, jsonT, base), side(200, jsonT, cur), allowlist.Empty(), LevelValues)
		out := map[string]string{}
		for _, d := range ds {
			out[d.Kind+" "+d.Where] = d.DigestText()
		}
		return out
	}
	order := "item-order $"
	a := digest(`["a","b","c"]`, `["c","a","x"]`)
	b := digest(`["a","b","c"]`, `["c","a","y"]`)
	if a[order] == "" || a[order] == b[order] {
		t.Errorf("the order drift of two other changes has one digest: %v %v", a, b)
	}
	if a["value $[1]"] == b["value $[1]"] {
		t.Error("the value drift of two other changes has one digest")
	}
	// Past the 20 positions the drift shows.
	list := func(swaps ...int) string {
		items := make([]string, 25)
		for i := range items {
			items[i] = `"` + string(rune('a'+i)) + `"`
		}
		for _, i := range swaps {
			items[i], items[i+1] = items[i+1], items[i]
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	c, d := digest(list(), list(0, 22)), digest(list(), list(0, 23))
	if c[order] == "" || c[order] == d[order] {
		t.Errorf("two orders that differ past what the drift shows have one digest: %v %v", c, d)
	}
	if e := digest(list(), list(0, 22)); e[order] != c[order] {
		t.Errorf("one reordering, two digests: %v %v", c, e)
	}
}

// An array rule needs a key or any_order.
func TestArrayRuleWithoutKey(t *testing.T) {
	if _, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.data\n    reason: r\n")); err == nil || !strings.Contains(err.Error(), "key or any_order") {
		t.Errorf("a rule with neither key nor any_order: %v", err)
	}
	if _, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.data\n    any_order: true\n    reason: r\n")); err != nil {
		t.Errorf("a rule with any_order only: %v", err)
	}
}

// The rule the note proposes, pasted under arrays:, is valid and ends the
// drift; arrays of one path get one note.
func TestArrayOrderHintPastes(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	for _, c := range []struct{ base, cur string }{
		{`{"a":[{"id":1,"n":"x"},{"id":2,"n":"y"}],"b":[{"tags":["p","q"]},{"tags":["r","s"]}]}`, `{"a":[{"id":2,"n":"y"},{"id":1,"n":"x"}],"b":[{"tags":["q","p"]},{"tags":["s","r"]}]}`},
	} {
		ds, notes := OneNotes("get-it's", side(200, jsonT, c.base), side(200, jsonT, c.cur), allowlist.Empty(), LevelValues)
		if len(ds) != 3 || len(notes) != 2 {
			t.Fatalf("drifts %v, notes %q", ds, notes)
		}
		rules := "arrays:\n"
		for _, n := range notes {
			_, rule, ok := strings.Cut(n, "allowlist.yaml: ")
			if !ok {
				t.Fatalf("no rule in %q", n)
			}
			rules += "  " + rule + "\n"
		}
		allow := parseAllow(t, rules)
		if ds, notes := OneNotes("get-it's", side(200, jsonT, c.base), side(200, jsonT, c.cur), allow, LevelValues); len(ds) != 0 || len(notes) != 0 {
			t.Errorf("with %s: drifts %v, notes %q", rules, ds, notes)
		}
	}
}

// The same items in another order give the same result whether the bytes
// of the items show it (no rule inside the array) or their forms do (a
// rule that can reach inside it).
func TestArrayOrderBothWays(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	other := parseAllow(t, "body:\n  - path: $.d[*].zzz\n    format: x\n    reason: r\n")
	for _, c := range []struct{ base, cur string }{
		{`{"d":[1,2,3,4]}`, `{"d":[4,3,2,1]}`},
		{`{"d":[1,1,2,3]}`, `{"d":[1,2,1,3]}`},
		{`{"d":[1,1,2,1]}`, `{"d":[2,1,1,1]}`},
		{`{"d":[{"a":[1,2]},{"a":[3]},{"a":[1,2]}]}`, `{"d":[{"a":[3]},{"a":[1,2]},{"a":[1,2]}]}`},
		{`{"d":[{"a":1},{"a": 1},{"b":2}]}`, `{"d":[{"b":2},{"a": 1},{"a":1}]}`},
		{`{"d":["x","y"]}`, `{"d":["y","\u0078"]}`},
		{`{"d":[1.0,2]}`, `{"d":[2,1.00]}`},
	} {
		for _, witnessed := range []bool{false, true} {
			a, an := oneNotes("req", side(200, jsonT, c.base), side(200, jsonT, c.cur), allowlist.Empty(), LevelValues, witnessed)
			b, bn := oneNotes("req", side(200, jsonT, c.base), side(200, jsonT, c.cur), other, LevelValues, witnessed)
			if driftList(a) != driftList(b) || strings.Join(an, "\n") != strings.Join(bn, "\n") {
				t.Errorf("%s → %s:\nby bytes\n%s\nby hash\n%s", c.base, c.cur, driftList(a), driftList(b))
			}
			for i := range a {
				if i < len(b) && a[i].DigestText() != b[i].DigestText() {
					t.Errorf("%s → %s: digest of %s differs", c.base, c.cur, a[i].Kind)
				}
			}
		}
	}
}

// A rule the comparison by position used is not counted as used when that
// comparison is undone: a rule only the reorder touched stays unused, and
// is reported as stale.
func TestArrayOrderUnusedRules(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	allow := parseAllow(t, "body:\n  - path: $.data[*].n\n    format: '.*'\n    reason: names\n").Track()
	ds := One("req", side(200, jsonT, `{"data":[{"n":"a","k":1},{"n":"b","k":2}]}`), side(200, jsonT, `{"data":[{"n":"b","k":2},{"n":"a","k":1}]}`), allow, LevelValues)
	if len(ds) != 1 || ds[0].Kind != KindItemOrder {
		t.Fatalf("drifts %v", ds)
	}
	if u := allow.Unused(false); len(u) != 1 || u[0].Why != "no change" {
		t.Errorf("unused %+v", u)
	}
	// A rule a pair still uses stays used.
	allow = parseAllow(t, "body:\n  - path: $.data[*].n\n    format: '.*'\n    reason: names\n").Track()
	One("req", side(200, jsonT, `{"data":[{"n":"a","k":1},{"n":"b","k":2}]}`), side(200, jsonT, `{"data":[{"n":"y","k":2},{"n":"x","k":1}]}`), allow, LevelValues)
	if u := allow.Unused(false); len(u) != 0 {
		t.Errorf("unused %+v", u)
	}
}

// BenchmarkArrayOrder compares a list of 20000 rows with the same rows in
// another order (every pair of rows swapped): the rows are hashed once.
func BenchmarkArrayOrder(b *testing.B) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	rows := func(swap bool) string {
		var items []string
		for i := range 20000 {
			k := i
			if swap {
				k ^= 1
			}
			items = append(items, `{"id":`+strconv.Itoa(k)+`,"name":"row `+strconv.Itoa(k)+`","tags":["a","b"]}`)
		}
		return `{"nodes":[` + strings.Join(items, ",") + `]}`
	}
	base, cur := side(200, jsonT, rows(false)), side(200, jsonT, rows(true))
	for b.Loop() {
		if ds := One("req", base, cur, allowlist.Empty(), LevelValues); len(ds) != 1 {
			b.Fatal(ds)
		}
	}
}

// BenchmarkArrayChanged compares a list of 20000 rows each changed in
// place: no reorder, the drifts by position stand.
func BenchmarkArrayChanged(b *testing.B) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	rows := func(v string) string {
		var items []string
		for i := range 20000 {
			items = append(items, `{"id":`+strconv.Itoa(i)+`,"name":"row `+strconv.Itoa(i)+v+`","tags":["a","b"]}`)
		}
		return `{"nodes":[` + strings.Join(items, ",") + `]}`
	}
	base, cur := side(200, jsonT, rows("")), side(200, jsonT, rows("x"))
	for b.Loop() {
		if ds := One("req", base, cur, allowlist.Empty(), LevelValues); len(ds) != 20000 {
			b.Fatal(len(ds))
		}
	}
}

// An order a rule allows hides nothing else: a key escaped or spacing
// inside an item that moved, or a key escaped elsewhere, is still an
// encoding drift; the order alone is none. Keyless and keyed rules alike.
func TestAllowedOrderComparesTheRest(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	for _, rule := range []string{"    any_order: true\n", "    key: id\n    any_order: true\n"} {
		allow := parseAllow(t, "arrays:\n  - path: $.data\n"+rule+"    reason: no ORDER BY\n")
		base := `{"k":1,"data":[{"id":1,"slug":"a"},{"id":2,"slug":"b"}]}`
		for _, c := range []struct{ name, cur, want string }{
			{"order only", `{"k":1,"data":[{"id":2,"slug":"b"},{"id":1,"slug":"a"}]}`, ""},
			{"a key escaped in a moved item", `{"k":1,"data":[{"id":2,"sl\u0075g":"b"},{"id":1,"slug":"a"}]}`, "encoding $ 56 bytes → 61 bytes"},
			{"spacing in a moved item", `{"k":1,"data":[{"id":2, "slug":"b"},{"id":1,"slug":"a"}]}`, "encoding $ 56 bytes → 57 bytes"},
			{"a key escaped elsewhere", `{"\u006b":1,"data":[{"id":2,"slug":"b"},{"id":1,"slug":"a"}]}`, "encoding $ 56 bytes → 61 bytes"},
		} {
			for _, fast := range []bool{true, false} {
				FastPaths = fast
				ds := One("req", side(200, jsonT, base), side(200, jsonT, c.cur), allow, LevelValues)
				FastPaths = true
				if got := driftList(ds); got != c.want {
					t.Errorf("%s%s (fast %v): got\n%s\nwant\n%s", rule, c.name, fast, got, c.want)
				}
			}
		}
	}
}
