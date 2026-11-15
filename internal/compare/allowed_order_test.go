package compare

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

// The cases of the byte check after an allowed order: arrays of scalars,
// orders inside items that moved, JSON in a page and in UTF-16, and items
// that compare equal but are written otherwise.
func TestAllowedOrderCases(t *testing.T) {
	scalars := "arrays:\n  - path: $.list\n    any_order: true\n    reason: r\n"
	nested := scalars + "  - path: $.list[*].tags\n    any_order: true\n    reason: r\n"
	objs := "arrays:\n  - path: $.d\n    any_order: true\n    reason: r\n"
	for _, c := range []struct {
		name, allow, base, cur string
		drift                  bool
	}{
		{"numbers reordered", scalars, `{"list":[1,2],"a":1}`, `{"list":[2,1],"a":1}`, false},
		{"numbers reordered, a key escaped", scalars, `{"list":[1,2],"a":1}`, `{"list":[2,1],"\u0061":1}`, true},
		{"strings reordered", scalars, `{"list":["x","y"],"a":1}`, `{"list":["y","x"],"a":1}`, false},
		{"strings reordered, spacing", scalars, `{"list":["x","y"],"a":1}`, `{"list":["y", "x"],"a":1}`, true},
		{"orders inside moved items", nested, `{"list":[{"n":1,"tags":["a","b"]},{"n":2,"tags":["c"]}],"k":1}`, `{"list":[{"n":2,"tags":["c"]},{"n":1,"tags":["b","a"]}],"k":1}`, false},
		{"orders inside moved items, a key escaped", nested, `{"list":[{"n":1,"tags":["a","b"]},{"n":2,"tags":["c"]}],"k":1}`, `{"list":[{"n":2,"tags":["c"]},{"n":1,"tags":["b","a"]}],"\u006b":1}`, true},
		{"items written otherwise only trade places", objs, `{"d":[{"a":1},{"a": 1},{"b":2}]}`, `{"d":[{"a": 1},{"b":2},{"a":1}]}`, false},
	} {
		allow := parseAllow(t, c.allow)
		sides := map[string][2]Side{
			"json":   {side(200, jsonType, c.base), side(200, jsonType, c.cur)},
			"utf-16": {side(200, jsonType, inUTF16(c.base, false)), side(200, jsonType, inUTF16(c.cur, false))},
		}
		page := func(s string) Side {
			return htmlSide(`<html><body><script id="__NEXT_DATA__" type="application/json">` + s + `</script></body></html>`)
		}
		pageAllow := parseAllow(t, strings.ReplaceAll(c.allow, "path: $", "path: html>body>script#__NEXT_DATA__$"))
		for form, s := range sides {
			for _, fast := range []bool{true, false} {
				FastPaths = fast
				ds := One("req", s[0], s[1], allow, LevelValues)
				FastPaths = true
				if (len(ds) > 0) != c.drift {
					t.Errorf("%s (%s, fast %v): %s", c.name, form, fast, driftList(ds))
				}
			}
		}
		if ds := One("req", page(c.base), page(c.cur), pageAllow, LevelValues); (len(ds) > 0) != c.drift {
			t.Errorf("%s (page): %s", c.name, driftList(ds))
		}
	}
}

// A document the order cannot be put back in is compared as it is: a
// payload compared decoded (devalue) reports the order as a drift rather
// than hide anything.
func TestAllowedOrderCannotRebuild(t *testing.T) {
	a, b := []byte(`[1,2]`), []byte(`[2,1]`)
	jb, _ := jsonx.Parse(a)
	other, _ := jsonx.Parse([]byte(`{"x":[2,1]}`)) // values the text does not follow
	if _, ok := unorder(a, b, jb, other, nil); ok {
		t.Error("a text that does not follow its values was rebuilt")
	}
	// A devalue payload is compared decoded: its order is reported.
	page := func(s string) Side {
		return htmlSide(`<html><body><script type="application/json" data-nuxt-data="nuxt-app" id="__NUXT_DATA__">` + s + `</script></body></html>`)
	}
	allow := parseAllow(t, "arrays:\n  - path: '*'\n    any_order: true\n    reason: r\n")
	ds := One("req", page(`[{"list":1},[2,3],"a","b"]`), page(`[{"list":1},[3,2],"a","b"]`), allow, LevelValues)
	if !has(ds, KindEncoding, "html>body>script#__NUXT_DATA__$") {
		t.Errorf("devalue: %s", driftList(ds))
	}
}

// genDoc writes a random JSON document: objects, arrays of scalars,
// objects and arrays, keyed arrays (their names end in _k: objects with a
// unique id). shuffle reorders every array.
type genDoc struct {
	r       *rand.Rand
	shuffle bool
	// change: the n-th place where a change can be made gets one (an
	// escaped character in a key or a string, or a space); places counts
	// them.
	change, places int
}

func (g *genDoc) mark(plain, changed string) string {
	g.places++
	if g.places == g.change {
		return changed
	}
	return plain
}

func (g *genDoc) str(s string) string {
	q := strconv.Quote(s)
	// An escape for the first letter, the parser reads it as the letter.
	return g.mark(q, `"`+fmt.Sprintf(`\u%04x`, s[0])+q[2:])
}

func (g *genDoc) value(r *rand.Rand, depth int) string {
	switch k := r.IntN(6); {
	case depth > 3 || k == 0:
		return []string{"1", "2", `"a"`, `"b"`, "true", "null", `"a/b"`}[r.IntN(7)]
	case k == 1:
		var items []string
		for range r.IntN(6) {
			items = append(items, []string{"1", "2", "3", `"x"`, `"y"`}[r.IntN(5)])
		}
		return g.array(items)
	case k == 2:
		var items []string
		for range r.IntN(5) {
			items = append(items, g.value(r, depth+1))
		}
		return g.array(items)
	}
	return g.object(r, depth)
}

func (g *genDoc) array(items []string) string {
	if g.shuffle {
		g.r.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	}
	sep := g.mark(",", ", ")
	return "[" + strings.Join(items, sep) + "]"
}

func (g *genDoc) object(r *rand.Rand, depth int) string {
	var members []string
	for i := range 1 + r.IntN(5) {
		name := []string{"alpha", "beta", "gamma", "delta", "eps"}[i]
		var v string
		if r.IntN(3) == 0 && depth < 3 {
			name += "_k"
			var items []string
			for id := range r.IntN(5) {
				items = append(items, `{"id":`+strconv.Itoa(id)+`,"v":`+g.value(r, depth+2)+`}`)
			}
			v = g.array(items)
		} else {
			v = g.value(r, depth+1)
		}
		members = append(members, g.str(name)+":"+v)
	}
	return "{" + strings.Join(members, ",") + "}"
}

// orderAllow lets every array come in any order, the keyed ones paired by
// id: for a JSON body (root $) or one in a page.
func orderAllow(t testing.TB, root string) *allowlist.List {
	l, err := allowlist.Parse([]byte("version: 1\narrays:\n  - path: '" + root + "*_k'\n    key: id\n    any_order: true\n    reason: r\n  - path: '" + root + "*'\n    any_order: true\n    reason: r\n"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// checkAllowedOrder compares a random document with itself reordered (no
// drift) and with itself reordered and one byte change (a drift), as JSON,
// in a page and in UTF-16.
func checkAllowedOrder(t *testing.T, seed uint64) {
	base := func(change int) (string, int) {
		g := &genDoc{r: rand.New(rand.NewPCG(seed, 1)), change: change}
		doc := `{"top":` + g.value(rand.New(rand.NewPCG(seed, 2)), 0) + `,"tail":` + g.str("end") + `}`
		return doc, g.places
	}
	shuffled := func(change int) string {
		g := &genDoc{r: rand.New(rand.NewPCG(seed, 3)), shuffle: true, change: change}
		return `{"top":` + g.value(rand.New(rand.NewPCG(seed, 2)), 0) + `,"tail":` + g.str("end") + `}`
	}
	b, places := base(0)
	cur := shuffled(0)
	changed := shuffled(1 + int(seed%uint64(places)))
	forms := []struct {
		name string
		wrap func(string) Side
		root string
	}{
		{"json", func(s string) Side { return side(200, jsonType, s) }, "$"},
		{"utf-16", func(s string) Side { return side(200, jsonType, inUTF16(s, true)) }, "$"},
		{"page", func(s string) Side {
			return htmlSide(`<html><body><script id="__NEXT_DATA__" type="application/json">` + s + `</script></body></html>`)
		}, "html>body>script#__NEXT_DATA__$"},
	}
	for _, f := range forms {
		allow := orderAllow(t, f.root)
		if ds := One("req", f.wrap(b), f.wrap(cur), allow, LevelValues); len(ds) != 0 {
			t.Errorf("seed %d, %s: a reorder drifts:\n%s\n%s\n%s", seed, f.name, b, cur, driftList(ds))
		}
		if changed == cur {
			continue
		}
		if ds := One("req", f.wrap(b), f.wrap(changed), allow, LevelValues); len(ds) == 0 {
			t.Errorf("seed %d, %s: a change passes:\n%s\n%s", seed, f.name, b, changed)
		}
	}
}

// TestAllowedOrderProperty: an order a rule allows is no drift, and every
// other byte change still is, in random documents.
func TestAllowedOrderProperty(t *testing.T) {
	for seed := range uint64(1500) {
		checkAllowedOrder(t, seed)
	}
}

func FuzzAllowedOrder(f *testing.F) {
	for seed := range uint64(20) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) { checkAllowedOrder(t, seed) })
}
