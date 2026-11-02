package compare

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// both compares with and without the fast paths, which must give the same
// drifts.
func both(t *testing.T, allow *allowlist.List, base, cur Side) []Drift {
	t.Helper()
	fast := run(t, allow, LevelValues, base, cur)
	FastPaths = false
	defer func() { FastPaths = true }()
	slow := run(t, allow, LevelValues, base, cur)
	if !reflect.DeepEqual(fast, slow) {
		t.Fatalf("fast paths change the result:\n fast %+v\n slow %+v", fast, slow)
	}
	return fast
}

// A rule judges only a value that changed: an unchanged value that does not
// match its format is the baseline's value, and no drift whatever else in
// the body changed; the same object on both sides is passed over even with
// a rule below it, also a wildcard rule.
func TestFastPathsUnchangedValue(t *testing.T) {
	const base = `{"t":"x","a":{"b":{"id":"abc","n":[1,2]}},"c":{"id":"abc"}}`
	const cur = `{"t":"y","a":{"b":{"id":"abc","n":[1,2]}},"c":{"id":"abc"}}`
	for _, rules := range []string{
		"  - path: $.a.b.id\n    format: '\\d+'\n    reason: x\n  - request: other\n    path: $.c\n    format: '\\d+'\n    reason: x\n",
		"  - path: '*.id'\n    format: '\\d+'\n    reason: x\n",
	} {
		allow := htmlAllow(t, rules)
		ds := both(t, allow, side(200, nil, base), side(200, nil, cur))
		if len(ds) != 1 || !has(ds, KindValue, "$.t") {
			t.Fatalf("%s: got %+v", rules, ds)
		}
		ds = both(t, allow, side(200, nil, base), side(200, nil, strings.Replace(cur, `"id":"abc","n"`, `"id":"abd","n"`, 1)))
		if len(ds) != 2 || !has(ds, KindFormat, "$.a.b.id") {
			t.Fatalf("%s: changed id: %+v", rules, ds)
		}
	}
}

// Identical parts of JSON inside a page are passed over, changed ones and
// rules inside them are not.
func TestFastPathsInPages(t *testing.T) {
	next := func(build, title string) string {
		return `<div id="__next"></div><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"` + title +
			`","list":[{"id":1},{"id":2}]},"meta":{"v":"1"}},"buildId":"` + build + `"}</script>`
	}
	allow := htmlAllow(t, "  - path: html>body>script#__NEXT_DATA__$.buildId\n    format: 'b\\d'\n    reason: x\n"+
		"  - path: html>body>script#__NEXT_DATA__$.props.meta.v\n    format: '[a-z]'\n    reason: x\n")
	ds := both(t, allow, htmlSide(next("b1", "a")), htmlSide(next("b2", "a")))
	const root = "html>body>script#__NEXT_DATA__$"
	if len(ds) != 0 {
		t.Fatalf("got %+v", ds)
	}
	ds = both(t, allow, htmlSide(next("b1", "a")), htmlSide(next("b2", "b")))
	if len(ds) != 1 || !has(ds, KindValue, root+".props.pageProps.title") {
		t.Fatalf("title: %+v", ds)
	}
	// Nuxt's payload is decoded into new values: an object written the same
	// way on both sides can stand for different values.
	ds = both(t, nil, htmlSide(nuxtPage(`[{"a":1},"x"]`)), htmlSide(nuxtPage(`[{"a":1},"y"]`)))
	if len(ds) != 1 || !has(ds, KindValue, "html>body>script#__NUXT_DATA__$.a") {
		t.Fatalf("nuxt: %+v", ds)
	}
}

// Objects with the same keys are paired by position, duplicates included;
// other objects as before.
func TestFastPathsObjects(t *testing.T) {
	cases := [][2]string{
		{`{"a":1,"a":2,"b":3}`, `{"a":1,"a":5,"b":3}`},
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{`{"a":1,"b":2}`, `{"a":1,"c":2}`},
		{`{"a":{"x":1},"b":[1,{"y":"\u00e9"}]}`, `{"a":{"x":1},"b":[1,{"y":"é"}]}`},
		{`{"a.b@c":[1,2],"k":1}`, `{"a.b@c":[1,2,3],"k":1}`},
		{`{"a":[{"b":1}],"z":0}`, `{"a":[{"b":1}],"z":1}`},
	}
	for _, c := range cases {
		if ds := both(t, nil, side(200, nil, c[0]), side(200, nil, c[1])); len(ds) == 0 {
			t.Errorf("%s → %s: no drift", c[0], c[1])
		}
	}
}
