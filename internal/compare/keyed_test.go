package compare

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

func keyedAllow(t testing.TB, extra string) *allowlist.List {
	t.Helper()
	l, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.data\n    key: id\n    reason: users by id\n" + extra))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func users(ids ...string) string {
	var items []string
	for _, id := range ids {
		items = append(items, fmt.Sprintf(`{"id":%s,"name":"u%s"}`, id, strings.Trim(id, `"`)))
	}
	return `{"data":[` + strings.Join(items, ",") + `]}`
}

func driftList(ds []Drift) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Kind+" "+d.Where+" "+d.Baseline+" → "+d.Current)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func TestKeyedArrays(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	cases := []struct {
		name, extra, base, cur, want, note string
	}{
		{name: "insert at the front", base: users("1", "2", "3"), cur: users("0", "1", "2", "3"),
			want: "key-extra $.data[id=0]  → {\"id\":0,\"name\":\"u0\"}"},
		{name: "removed", base: users("1", "2", "3"), cur: users("1", "3"),
			want: "key-missing $.data[id=2] {\"id\":2,\"name\":\"u2\"} → "},
		{name: "value of a paired item", base: users("1", "2"), cur: `{"data":[{"id":1,"name":"u1"},{"id":2,"name":"x"}]}`,
			want: `value $.data[id=2].name "u2" → "x"`},
		{name: "reordered", base: users("1", "2", "3"), cur: users("3", "1", "2"),
			want: "item-order $.data id=1, id=2, id=3 → id=3, id=1, id=2"},
		{name: "reordered, any order allowed", extra: "    any_order: true\n", base: users("1", "2", "3"), cur: users("3", "1", "2")},
		{name: "any order still reports a change", extra: "    any_order: true\n", base: users("1", "2"), cur: `{"data":[{"id":2,"name":"x"},{"id":1,"name":"u1"}]}`,
			want: `value $.data[id=2].name "u2" → "x"`},
		{name: "string keys", base: users(`"a"`, `"b"`), cur: users(`"b"`, `"c"`),
			want: "key-extra $.data[id=\"c\"]  → {\"id\":\"c\",\"name\":\"uc\"}\nkey-missing $.data[id=\"a\"] {\"id\":\"a\",\"name\":\"ua\"} → "},
		{name: "key type change is a type drift in the item", base: users("1"), cur: users(`"1"`),
			want: "type $.data[id=1].id int → string"},
		{name: "item without the key", base: users("1", "2"), cur: `{"data":[{"name":"u0"},{"id":1,"name":"u1"},{"id":2,"name":"u2"}]}`,
			want: "array-length $.data 2 → 3\nkey-missing $.data[0].id 1 → \nvalue $.data[0].name \"u1\" → \"u0\"\nvalue $.data[1].id 2 → 1\nvalue $.data[1].name \"u2\" → \"u1\"",
			note: "$.data: items paired by position, not by id: in the current response item 0 has no id"},
		{name: "duplicate key", base: users("1", "1"), cur: users("1", "2"),
			want: "value $.data[1].id 1 → 2\nvalue $.data[1].name \"u1\" → \"u2\"",
			note: "$.data: items paired by position, not by id: in the baseline response items 0 and 1 have the same id"},
	}
	for _, c := range cases {
		allow := keyedAllow(t, c.extra)
		for _, fast := range []bool{true, false} {
			FastPaths = fast
			ds, notes := OneNotes("req", side(200, jsonT, c.base), side(200, jsonT, c.cur), allow, LevelValues)
			FastPaths = true
			if got := driftList(ds); got != sortedLines(c.want) {
				t.Errorf("%s (fast %v):\ngot\n%s\nwant\n%s", c.name, fast, got, sortedLines(c.want))
			}
			if strings.Join(notes, "\n") != c.note {
				t.Errorf("%s: notes %q, want %q", c.name, notes, c.note)
			}
		}
	}
}

func sortedLines(s string) string {
	if s == "" {
		return ""
	}
	l := strings.Split(s, "\n")
	sort.Strings(l)
	return strings.Join(l, "\n")
}

func TestKeyedArraysRules(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	// any_length lets items come and go; their values are still compared.
	allow := keyedAllow(t, "body:\n  - path: $.data\n    any_length: true\n    reason: a feed\n")
	ds := run(t, allow, LevelValues, side(200, jsonT, users("1", "2")), side(200, jsonT, `{"data":[{"id":0,"name":"u0"},{"id":1,"name":"x"}]}`))
	if got := driftList(ds); got != `value $.data[id=1].name "u1" → "x"` {
		t.Errorf("any_length:\n%s", got)
	}
	// A nested key, and value rules over keyed paths.
	l, err := allowlist.Parse([]byte(`version: 1
arrays:
  - path: $.rows
    key: user.id
    reason: r
body:
  - path: $.rows[*].at
    format: '\d+'
    reason: time
`))
	if err != nil {
		t.Fatal(err)
	}
	base := `{"rows":[{"user":{"id":7},"at":"1"},{"user":{"id":8},"at":"2"}]}`
	cur := `{"rows":[{"user":{"id":9},"at":"5"},{"user":{"id":7},"at":"3"},{"user":{"id":8},"at":"x"}]}`
	ds = run(t, l, LevelValues, side(200, jsonT, base), side(200, jsonT, cur))
	want := "format $.rows[user.id=8].at \"2\" → \"x\"\nkey-extra $.rows[user.id=9]  → {\"user\":{\"id\":9},\"at\":\"5\"}"
	if got := driftList(ds); got != want {
		t.Errorf("nested key:\n%s\nwant\n%s", got, want)
	}
	// Grouped: one key path across items.
	a := Drift{Kind: KindValue, Where: `$.data[id=1].name`}
	b := Drift{Kind: KindValue, Where: `$.data[id="x]"].name`}
	if a.Signature() != b.Signature() || a.Signature() != "value $.data[*].name" {
		t.Errorf("signatures %q %q", a.Signature(), b.Signature())
	}
	for _, w := range []string{`$.data["@id"="a@b.c"].name`, `$.data["user id"=-2].name`, `$.data["ključ"="x\"]"].name`} {
		if sig := (Drift{Kind: KindValue, Where: w}).Signature(); sig != "value $.data[*].name" {
			t.Errorf("signature of %s: %q", w, sig)
		}
	}
	// A request the rule is not for: positional.
	l2, _ := allowlist.Parse([]byte("version: 1\narrays:\n  - request: other\n    path: $.data\n    key: id\n    reason: r\n"))
	ds = run(t, l2, LevelValues, side(200, jsonT, users("1")), side(200, jsonT, users("0", "1")))
	if !has(ds, KindArrayLength, "$.data") {
		t.Errorf("rule of another request applied: %v", ds)
	}
}

// A key name that is not plain is quoted in the step, so the step names
// the key and the value apart whatever the key holds.
func TestKeyedArraysKeyNames(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	for key, step := range map[string]string{
		"id": `id=2`, "user_id": `user_id=2`, "@id": `"@id"=2`, "user id": `"user id"=2`,
		"ključ": `"ključ"=2`, `a]=b"`: `"a]=b\""=2`, "x.@y": `"x.@y"=2`,
	} {
		l, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: $.data\n    key: " + jsonx.Quote(key) + "\n    reason: r\n"))
		if err != nil {
			t.Fatal(key, err)
		}
		member := jsonx.Quote(key)
		if strings.Contains(key, ".") {
			first, rest, _ := strings.Cut(key, ".")
			member = jsonx.Quote(first) + `:{` + jsonx.Quote(rest)
		}
		item := func(id int, name string) string {
			if strings.Contains(key, ".") {
				return fmt.Sprintf(`{%s:%d},"name":%q}`, member, id, name)
			}
			return fmt.Sprintf(`{%s:%d,"name":%q}`, member, id, name)
		}
		base := `{"data":[` + item(1, "a") + "," + item(2, "b") + `]}`
		cur := `{"data":[` + item(1, "a") + "," + item(2, "c") + `]}`
		ds := run(t, l, LevelValues, side(200, jsonT, base), side(200, jsonT, cur))
		if want := "$.data[" + step + "].name"; len(ds) != 1 || ds[0].Where != want {
			t.Errorf("%s: %v, want %s", key, ds, want)
		}
	}
}

func TestArrayRuleValidation(t *testing.T) {
	for name, rule := range map[string]string{
		"no key":      "  - path: $.a\n    reason: r\n",
		"no reason":   "  - path: $.a\n    key: id\n",
		"cell path":   "  - path: xlsx:S!A1\n    key: id\n    reason: r\n",
		"raw":         "  - path: $raw\n    key: id\n    reason: r\n",
		"empty step":  "  - path: $.a\n    key: user..id\n    reason: r\n",
		"page path":   "  - path: html>body>ul\n    key: id\n    reason: r\n",
		"unknown key": "  - path: $.a\n    key: id\n    order: any\n    reason: r\n",
	} {
		if _, err := allowlist.Parse([]byte("version: 1\narrays:\n" + rule)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
