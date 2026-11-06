package compare

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/orangehill/driftgate/internal/jsonx"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// encCase is a baseline X and two responses: an expectation recorded for
// X→A must not take X→B for the same drifts when A→B drifts (a false
// pass), and must when it does not (no false fail).
type encCase struct {
	name    string
	headers [][2]string
	x, a, b string
}

// expectationHolds reports whether every drift of X→B is one an
// expectation of X→A names (by digest), as compare decides it.
func expectationHolds(t *testing.T, allow *allowlist.List, x, a, b Side) (holds bool, xa, xb, ab []Drift) {
	t.Helper()
	xa, xb, ab = run(t, allow, LevelValues, x, a), run(t, allow, LevelValues, x, b), run(t, allow, LevelValues, a, b)
	expected := map[string]bool{}
	for _, d := range xa {
		expected[d.DigestText()] = true
	}
	holds = len(xb) > 0
	for _, d := range xb {
		holds = holds && expected[d.DigestText()]
	}
	return holds, xa, xb, ab
}

// checkNoFalsePass fails when the expectation of X→A holds for B although
// A→B drifts.
func checkNoFalsePass(t *testing.T, allow *allowlist.List, cases []encCase) {
	t.Helper()
	for _, tc := range cases {
		x, a, b := side(200, tc.headers, tc.x), side(200, tc.headers, tc.a), side(200, tc.headers, tc.b)
		holds, xa, xb, ab := expectationHolds(t, allow, x, a, b)
		if len(ab) == 0 {
			t.Errorf("%s: A→B does not drift, the case tests nothing", tc.name)
			continue
		}
		if holds {
			t.Errorf("%s: the expectation of X→A holds for B, A→B drifts\nX→A %s\nX→B %s\nA→B %s", tc.name, driftList(xa), driftList(xb), driftList(ab))
		}
	}
}

// TestWitnessValueEscapes: a JSON string written with other escapes is a
// drift (encoding), so what an expectation digests holds how the value is
// written: in a changed value, a new key, the tail of an array, a type
// change and JSON inside a page.
func TestWitnessValueEscapes(t *testing.T) {
	page := func(s string) string {
		return `<html><body><script type="application/json">` + s + `</script></body></html>`
	}
	checkNoFalsePass(t, nil, []encCase{
		{"changed value", jsonType, `{"u":"http:\/\/old"}`, `{"u":"http:\/\/new"}`, `{"u":"http://new"}`},
		{"changed value, other values", jsonType, `{"a":"x","b":1}`, `{"a":"a\/b","b":2}`, `{"a":"a/b","b":2}`},
		{"new key", jsonType, `{"a":1}`, `{"a":1,"k":"a\/b"}`, `{"a":1,"k":"a/b"}`},
		{"array tail", jsonType, `[1]`, `[1,"a\/b"]`, `[1,"a/b"]`},
		{"type change", jsonType, `{"a":1}`, `{"a":"a\/b"}`, `{"a":"a/b"}`},
		{"line end", jsonType, `{"a":"x"}`, `{"a":"a\u000ab"}`, `{"a":"a\nb"}`},
		{"non-ASCII", jsonType, `{"a":"x"}`, `{"a":"\u00e9"}`, `{"a":"\u00E9"}`},
		{"escape moved", jsonType, `{"a":"x"}`, `{"a":"\/a/"}`, `{"a":"/a\/"}`},
		{"JSON in a page", [][2]string{{"content-type", "text/html"}}, page(`{"a":1,"u":"x"}`), page(`{"a":2,"u":"a\/b"}`), page(`{"a":2,"u":"a/b"}`)},
	})
}

// TestWitnessValueEscapesUnderRules: a rule covers how the values it
// lets change are written, so their escapes are no drift and keep the
// expectation (a token that holds a "/" or "+" only now and then); the
// escapes of values no rule covers still count.
func TestWitnessValueEscapesUnderRules(t *testing.T) {
	allow := htmlAllow(t, "  - path: $.u\n    format: '\\d+'\n    parts: true\n    reason: id\n  - path: $.t\n    format: '.*'\n    reason: time\n")
	checkNoFalsePass(t, allow, []encCase{
		{"parts, a value no rule covers written otherwise", jsonType, `{"u":"\/a\/1","z":1}`, `{"u":"\/a\/2","z":2,"y":"a\/b"}`, `{"u":"\/a\/2","z":2,"y":"a/b"}`},
		{"a key written otherwise", jsonType, `{"t":"1","z":1}`, `{"t":"2","z":2,"a/b":1}`, `{"t":"2","z":2,"a\/b":1}`},
	})
	for _, tc := range []encCase{
		{"parts, the same value written otherwise", jsonType, `{"u":"\/a\/1","z":1}`, `{"u":"\/a\/2","z":2}`, `{"u":"/a\/2","z":2}`},
		{"a token with a slash now and then", jsonType, `{"t":"x","z":1}`, `{"t":"ab\/c","z":2}`, `{"t":"abd","z":2}`},
		{"a token with a plus now and then", jsonType, `{"t":"x","z":1}`, `{"t":"ab\u002Bc","z":2}`, `{"t":"abd","z":2}`},
		{"parts, another token", jsonType, `{"u":"\/a\/1","z":1}`, `{"u":"\/a\/2","z":2}`, `{"u":"\/a\/345","z":2}`},
		{"parts, another token in a drift", jsonType, `{"u":"\/b\/1"}`, `{"u":"\/a\/2"}`, `{"u":"\/a\/345"}`},
		{"whole value", jsonType, `{"t":"1","z":1}`, `{"t":"a\/2","z":2}`, `{"t":"a/2","z":2}`},
	} {
		x, a, b := side(200, tc.headers, tc.x), side(200, tc.headers, tc.a), side(200, tc.headers, tc.b)
		holds, xa, xb, ab := expectationHolds(t, allow, x, a, b)
		if len(ab) == 0 && !holds {
			t.Errorf("%s: A→B does not drift, and the expectation of X→A fails for B\nX→A %s\nX→B %s", tc.name, driftList(xa), driftList(xb))
		}
	}
}

// TestEscapes: how a document escapes its strings and their texts give
// the document back; another token where the document escapes a
// character the same way everywhere leaves it as it is.
func TestEscapes(t *testing.T) {
	esc := func(doc string) string {
		d := []byte(doc)
		return string(escapes(d, func(f func([]byte)) { eachString(d, f) }))
	}
	for _, pair := range [][2]string{
		{`["a\/b"]`, `["a/b"]`},
		{`["\/a/"]`, `["/a\/"]`},
		{`["\u00e9"]`, `["\u00E9"]`},
		{`["a\u000ab"]`, `["a\nb"]`},
		{`["\ud83d\udc4b"]`, `["\ud83d\udc4b\/"]`},
		{`["\u00e9\u00e9"]`, `["\u00e9é"]`},
		{`{"o":{"\u0061":1},"p":{"a":1}}`, `{"o":{"a":1},"p":{"\u0061":1}}`},
		{"[\"\\ud800\",\"\xed\xa0\x80\"]", "[\"\xed\xa0\x80\",\"\\ud800\"]"}, // a lone surrogate escaped or as its bytes
		{`["\ud800\udc00"]`, `["𐀀"]`},
		{`["a/b","a\/b"]`, `["a\/b","a/b"]`},
	} {
		if esc(pair[0]) == esc(pair[1]) {
			t.Errorf("%s and %s: the same escapes", pair[0], pair[1])
		}
	}
	if esc(`["a/b",""]`) != "" {
		t.Error("escapes of strings without them")
	}
	for _, pair := range [][2]string{
		{`{"u":"\/items\/1\/edit","t":"AB\/C"}`, `{"u":"\/items\/12345\/edit","t":"A\/B\/CD"}`},
		{`{"n":"by \u00e9t\u00e9"}`, `{"n":"by \u00e9\u00e9\u00e9"}`},
	} {
		if esc(pair[0]) != esc(pair[1]) {
			t.Errorf("%s and %s: other escapes", pair[0], pair[1])
		}
	}
}

// TestWitnessSpacing: whitespace between the tokens of a JSON document
// that changed alone is a drift (encoding), also where the layout the
// comparison samples stays the same; an expectation holds only with the
// same whitespace.
func TestWitnessSpacing(t *testing.T) {
	checkNoFalsePass(t, nil, []encCase{
		{"inside an array", jsonType, `{"a":[1,2],"b":[1,2],"z":1}`, `{"a":[1,2],"b":[1,2],"z":2}`, `{"a":[1,2],"b":[1, 2],"z":2}`},
		{"moved", jsonType, `{"a":[1,2],"b":[1,2],"z":1}`, `{"a":[1, 2],"b":[1,2],"z":2}`, `{"a":[1,2],"b":[1, 2],"z":2}`},
		{"after a colon", jsonType, `{"a":1,"b":1,"z":1}`, `{"a":1,"b":1,"z":2}`, `{"a":1,"b": 1,"z":2}`},
		{"a later indentation", jsonType, "{\n  \"a\": 1,\n  \"b\": 1\n}", "{\n  \"a\": 2,\n  \"b\": 1\n}", "{\n  \"a\": 2,\n    \"b\": 1\n}"},
		{"a later line end", jsonType, "{\n  \"a\": 1,\n  \"b\": 1\n}", "{\n  \"a\": 2,\n  \"b\": 1\n}", "{\n  \"a\": 2,\r\n  \"b\": 1\n}"},
	})
}

// TestWitnessSpacingUnderRules: a list an any_length rule lets grow,
// from empty on, keeps an expectation for another capture of the same
// encoder: compact, with spaces after ',' and ':' (Python), pretty with
// two or four spaces.
func TestWitnessSpacingUnderRules(t *testing.T) {
	allow := htmlAllow(t, "  - path: $.items\n    any_length: true\n    reason: paging\n  - path: $.at\n    format: '\\d+'\n    reason: time\n")
	doc := func(at string, total, n int) any {
		items := []any{}
		for i := range n {
			items = append(items, map[string]any{"id": i, "tags": []any{"a", "b"}, "o": map[string]any{}})
		}
		return map[string]any{"at": at, "items": items, "meta": map[string]any{"total": total}}
	}
	python := func(v any) string {
		b, _ := json.Marshal(v)
		s := string(b)
		s = strings.ReplaceAll(s, `,"`, `, "`)
		s = strings.ReplaceAll(s, `":`, `": `)
		return strings.ReplaceAll(s, `,{`, `, {`)
	}
	for name, enc := range map[string]func(any) string{
		"compact":  func(v any) string { b, _ := json.Marshal(v); return string(b) },
		"python":   python,
		"pretty 2": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
		"pretty 4": func(v any) string { b, _ := json.MarshalIndent(v, "", "    "); return string(b) + "\n" },
	} {
		for _, n := range [][2]int{{0, 1}, {1, 0}, {1, 3}, {2, 0}, {0, 5}} {
			x, a, b := side(200, jsonType, enc(doc("1", 1, 2))), side(200, jsonType, enc(doc("2", 2, n[0]))), side(200, jsonType, enc(doc("3", 2, n[1])))
			holds, xa, xb, ab := expectationHolds(t, allow, x, a, b)
			if len(ab) == 0 && !holds {
				t.Errorf("%s, %d → %d items: the same encoder fails the expectation\nX→A %s\nX→B %s", name, n[0], n[1], driftList(xa), driftList(xb))
			}
		}
	}
}

// TestSpacing: the spacing of a document with its layout and tokens
// gives it back; an encoder's documents of other values and lengths,
// empty lists included, have none.
func TestSpacing(t *testing.T) {
	sp := func(raw string) string {
		b, a := edges([]byte(raw))
		return string(spacing([]byte(raw)[len(b):len(raw)-len(a)], jsonx.StyleOf([]byte(raw))))
	}
	for _, raw := range []string{
		`{"a":[1,2],"b":"x y","c":[],"d":{}}`,
		"{\"a\":1}\n",
		`{"a": [1, 2], "b": {"c": []}}`,
		"{\n  \"list\": [\n    {\n      \"k\": 1\n    },\n    2\n  ],\n  \"e\": [],\n  \"o\": {}\n}",
		"{\r\n\t\"list\": [\r\n\t\t1\r\n\t]\r\n}",
	} {
		if sp(raw) != "" {
			t.Errorf("%q: spacing of an encoder's layout", raw)
		}
	}
	for _, pair := range [][2]string{
		{`{"a":[1, 2],"b":[1,2]}`, `{"a":[1,2],"b":[1, 2]}`},
		{"{\n  \"a\": 1,\n  \"b\": 1\n}", "{\n  \"a\": 1,\n    \"b\": 1\n}"},
		{`{"a":[]}`, `{"a":[ ]}`},
	} {
		if sp(pair[0]) == sp(pair[1]) {
			t.Errorf("%q and %q: the same spacing", pair[0], pair[1])
		}
	}
}

// TestWitnessCSVLayout: a CSV file written otherwise with the same cells
// is a drift (body), so an expectation holds only with the same layout,
// by place: a field quoted in another cell, a line end of another record,
// an empty field more, a blank line, a quote written lazily.
func TestWitnessCSVLayout(t *testing.T) {
	csvT := [][2]string{{"content-type", "text/csv"}}
	checkNoFalsePass(t, nil, []encCase{
		{"trailing empty field", csvT, "a,b\n1,2\n", "a,b\n1,3,\n", "a,b\n1,3\n"},
		{"blank line", csvT, "a,b\n1,2\n", "a,b\n\n1,3\n", "a,b\n1,3\n"},
		{"blank line at the end", csvT, "a,b\n1,2\n", "a,b\n1,3\n\n", "a,b\n1,3\n"},
		{"quotes moved", csvT, "a,b\n1,1\nx,y\n", "a,b\n\"1\",1\nx,z\n", "a,b\n1,\"1\"\nx,z\n"},
		{"line ends moved", csvT, "a,b\n1,2\n3,4\n", "a,b\r\n1,2\n3,5\n", "a,b\n1,2\r\n3,5\n"},
		{"lazy quote", csvT, "a\nx\n", "a\n\"q\"\"r\"\n", "a\nq\"r\n"},
		{"lazy quote in quotes", csvT, "a\nx\n", "a\n\"q\"r\"\n", "a\n\"q\"\"r\"\n"},
		{"empty field in quotes", csvT, "a,b\n1,2\n", "a,b\n,3\n", "a,b\n\"\",3\n"},
	})
	// Cells a rule lets change keep the layout.
	allow := htmlAllow(t, "  - path: csv!A*\n    format: '\\d+'\n    reason: id\n")
	x, a, b := side(200, csvT, "id,n\n1,x\n2,x\n"), side(200, csvT, "id,n\n\"10\",y\n\"20\",y\n"), side(200, csvT, "id,n\n\"345\",y\n\"6\",y\n")
	if holds, xa, xb, ab := expectationHolds(t, allow, x, a, b); len(ab) != 0 || !holds {
		t.Errorf("allowed cells fail the expectation\nX→A %s\nX→B %s\nA→B %s", driftList(xa), driftList(xb), driftList(ab))
	}
}

// TestCSVLayout: the layout of a CSV file and its cells give it back.
func TestCSVLayout(t *testing.T) {
	same := [][2]string{
		{"a,b\n1,2\n", "a,b\n3,4\n"},
		{"\"a\",b\r\n\"1\",2\r\n", "\"x\",b\r\n\"7\",9\r\n"},
	}
	for _, p := range same {
		if string(csvLayout([]byte(p[0]))) != string(csvLayout([]byte(p[1]))) {
			t.Errorf("%q and %q: another layout", p[0], p[1])
		}
	}
	other := [][2]string{
		{"a,b\n", "a,b"},
		{"a,b\n", "a,b\r\n"},
		{"a,b\n", "a,b,\n"},
		{"a\n\nb\n", "a\nb\n\n"},
		{"\"1\",1\n", "1,\"1\"\n"},
		{"\"q\"\"r\"\n", "q\"r\n"},
		{"\"a\nb\"\n", "\"a\r\nb\"\n"},
	}
	for _, p := range other {
		if string(csvLayout([]byte(p[0]))) == string(csvLayout([]byte(p[1]))) {
			t.Errorf("%q and %q: the same layout", p[0], p[1])
		}
	}
}

// TestWitnessOutsideTheDocument: a body whose values changed only as the
// rules allow, next to a drift outside it (a header, the status, another
// part of a page), is folded into that drift by how it is written.
func TestWitnessOutsideTheDocument(t *testing.T) {
	headers := func(ct, v string) [][2]string { return [][2]string{{"content-type", ct}, {"x-v", v}} }
	parts := "  - path: $.u\n    format: '\\d+'\n    parts: true\n    reason: build\n"
	whole := "  - path: $.u\n    format: '\\d+'\n    reason: build\n"
	cells := "  - path: csv!A*\n    format: '\\d+'\n    reason: id\n"
	page := func(p, script string) string {
		return `<html><body><p>` + p + `</p><script type="application/json">` + script + `</script></body></html>`
	}
	for _, tc := range []struct {
		name, rules, ct string
		status          int
		x, a, b         string
	}{
		{"a value no rule covers written otherwise", parts, "application/json", 0, `{"u":"\/b\/1","v":"a\/b"}`, `{"u":"\/b\/2","v":"a\/b"}`, `{"u":"\/b\/2","v":"a/b"}`},
		{"whitespace", whole, "application/json", 0, `{"u":"1","l":[1,2]}`, `{"u":"2","l":[1,2]}`, `{"u":"2","l":[1, 2]}`},
		{"whitespace, the status", whole, "application/json", 201, `{"u":"1","l":[1,2]}`, `{"u":"2","l":[1,2]}`, `{"u":"2","l":[1, 2]}`},
		{"a key escaped", whole, "application/json", 0, `{"u":"1","a/b":1}`, `{"u":"2","a/b":1}`, `{"u":"2","a\/b":1}`},
		{"a key escape moved", whole, "application/json", 0, `{"u":"1","o":{"a":1},"p":{"a":1}}`, `{"u":"2","o":{"\u0061":1},"p":{"a":1}}`, `{"u":"2","o":{"a":1},"p":{"\u0061":1}}`},
		{"a CSV field quoted", cells, "text/csv", 0, "id,n\n1,x\n", "id,n\n2,x\n", "id,n\n2,\"x\"\n"},
		{"CSV line ends", cells, "text/csv", 0, "id,n\n1,x\n", "id,n\n2,x\n", "id,n\r\n2,x\r\n"},
		{"JSON in a page, another text", "  - path: html*$.u\n    format: '\\d+'\n    reason: build\n", "text/html", -1,
			page("t", `{"u":"1","l":[1,2]}`), page("u", `{"u":"2","l":[1,2]}`), page("u", `{"u":"2","l":[1, 2]}`)},
	} {
		allow := htmlAllow(t, tc.rules)
		hx, ha := headers(tc.ct, "1"), headers(tc.ct, "2")
		status := 200
		switch {
		case tc.status > 0:
			status, ha = tc.status, hx
		case tc.status < 0:
			ha = hx
		}
		x, a, b := side(200, hx, tc.x), side(status, ha, tc.a), side(status, ha, tc.b)
		holds, xa, xb, ab := expectationHolds(t, allow, x, a, b)
		if len(ab) == 0 {
			t.Errorf("%s: A→B does not drift, the case tests nothing", tc.name)
		} else if holds {
			t.Errorf("%s: the expectation of X→A holds for B, A→B drifts\nX→A %s\nX→B %s\nA→B %s", tc.name, driftList(xa), driftList(xb), driftList(ab))
		}
	}
}

// TestWitnessCSVLineEndsInCells: a cell a rule lets change, quoted over
// lines with CRLF, keeps an expectation for another such cell; the same
// cell with LF does not.
func TestWitnessCSVLineEndsInCells(t *testing.T) {
	csvT := [][2]string{{"content-type", "text/csv"}}
	allow := htmlAllow(t, "  - path: csv!B*\n    format: '(?s).*'\n    reason: note\n")
	x, a := side(200, csvT, "id,n\n1,x\n"), side(200, csvT, "id,n\n2,\"a\r\nb\"\n")
	if holds, xa, xb, ab := expectationHolds(t, allow, x, a, side(200, csvT, "id,n\n2,\"c\r\nd\"\n")); len(ab) != 0 || !holds {
		t.Errorf("another cell with CRLF fails the expectation\nX→A %s\nX→B %s\nA→B %s", driftList(xa), driftList(xb), driftList(ab))
	}
	checkNoFalsePass(t, allow, []encCase{{"LF in place of CRLF", csvT, "id,n\n1,x\n", "id,n\n2,\"a\r\nb\"\n", "id,n\n2,\"a\nb\"\n"}})
}

// TestWitnessResidueOrigins: a document captured from another origin
// reads the same, also where it writes a character in more than one way
// and the origin holds it.
func TestWitnessResidueOrigins(t *testing.T) {
	o1, o2 := "http://localhost:8080", "http://127.0.0.1:61212"
	doc := func(o string) string { return `["` + o + `/a","null",1]` }
	x := at(side(200, jsonType, `["`+o1+`/a","null",0]`), o1)
	a, b := at(side(200, jsonType, doc(o1)), o1), at(side(200, jsonType, doc(o2)), o2)
	if holds, xa, xb, ab := expectationHolds(t, nil, x, a, b); len(ab) != 0 || !holds {
		t.Errorf("another origin fails the expectation\nX→A %s\nX→B %s\nA→B %s", driftList(xa), driftList(xb), driftList(ab))
	}
}

// TestWitnessDevalueSlots: a devalue payload (__NUXT_DATA__) laid out in
// other slots decodes to the same value and is an encoding drift; an
// expectation holds only with the same layout, while a value in a slot
// a rule lets change keeps it.
func TestWitnessDevalueSlots(t *testing.T) {
	htmlT := [][2]string{{"content-type", "text/html"}}
	page := func(p, payload string) string {
		return `<html><body><p>` + p + `</p><script id="__NUXT_DATA__" type="application/json">` + payload + `</script></body></html>`
	}
	checkNoFalsePass(t, nil, []encCase{
		{"a value changed", htmlT, page("1", `[{"m":1,"n":2},"hi","world"]`), page("1", `[{"m":1,"n":2},"hello","world"]`), page("1", `[{"m":2,"n":1},"world","hello"]`)},
		{"a text changed", htmlT, page("1", `[{"m":1,"n":2},"hello","world"]`), page("2", `[{"m":1,"n":2},"hello","world"]`), page("2", `[{"m":2,"n":1},"world","hello"]`)},
		{"a slot no one refers to", htmlT, page("1", `[{"m":1},"hello","a"]`), page("2", `[{"m":1},"hello","a"]`), page("2", `[{"m":1},"hello","b"]`)},
	})
	allow := htmlAllow(t, "  - path: 'html*'\n    format: '\\d+'\n    reason: id\n")
	x, a, b := side(200, htmlT, page("a", `[{"id":1},5]`)), side(200, htmlT, page("b", `[{"id":1},6]`)), side(200, htmlT, page("b", `[{"id":1},7]`))
	if holds, xa, xb, ab := expectationHolds(t, allow, x, a, b); len(ab) != 0 || !holds {
		t.Errorf("an allowed value fails the expectation\nX→A %s\nX→B %s\nA→B %s", driftList(xa), driftList(xb), driftList(ab))
	}
}

// TestRootConstants: a document that is NaN or Infinity has no bytes
// before its value: another constant is a value drift, nothing more.
func TestRootConstants(t *testing.T) {
	if ds := run(t, nil, LevelValues, side(200, jsonType, "NaN"), side(200, jsonType, "Infinity\n")); len(ds) != 2 || !has(ds, KindValue, "$") || !strings.Contains(driftList(ds), "layout") {
		t.Errorf("constants: %s", driftList(ds))
	}
	if ds := run(t, nil, LevelValues, side(200, jsonType, "NaN"), side(200, jsonType, "Infinity")); len(ds) != 1 || !has(ds, KindValue, "$") {
		t.Errorf("constants: %s", driftList(ds))
	}
}

// inUTF16 writes a text in UTF-16, little or big endian, as an encoder
// that writes JSON so may (the parser reads it as the reference does).
func inUTF16(s string, big bool) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if big {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return string(b)
}

// TestWitnessUTF16: a JSON body in UTF-16, whose bytes the residue does
// not read as UTF-8 (U+2013 is 13 20, and 20 a space), is compared and
// witnessed without a panic, as the document it is.
func TestWitnessUTF16(t *testing.T) {
	for _, big := range []bool{false, true} {
		doc := func(s string) Side { return side(200, jsonType, inUTF16(s, big)) }
		x, a := doc(`{"a":"–"}`), doc(`{"a":"—"}`)
		if ds := run(t, nil, LevelValues, x, a); len(ds) != 1 || !has(ds, KindValue, "$.a") {
			t.Fatalf("big endian %v: %s", big, driftList(ds))
		}
		if holds, xa, xb, ab := expectationHolds(t, nil, x, a, doc(`{"a":"—"}`)); len(ab) != 0 || !holds {
			t.Errorf("the same body fails the expectation\nX→A %s\nX→B %s", driftList(xa), driftList(xb))
		}
		checkNoFalsePass(t, nil, []encCase{{"written otherwise", jsonType, inUTF16(`{"a":"–","b":1}`, big), inUTF16(`{"a":"—","b":1}`, big), inUTF16(`{"a":"—", "b":1}`, big)}})
		// Values a rule lets change keep the expectation: the residue is
		// that of the text.
		allow := htmlAllow(t, "  - path: $.t\n    format: '\\d+'\n    reason: r\n")
		if holds, xa, xb, ab := expectationHolds(t, allow, doc(`{"t":0,"a":"x"}`), doc(`{"t":12,"a":"—"}`), doc(`{"t":3,"a":"—"}`)); len(ab) != 0 || !holds {
			t.Errorf("an allowed value fails the expectation\nX→A %s\nX→B %s\nA→B %s", driftList(xa), driftList(xb), driftList(ab))
		}
	}
}

// TestRuleCoversOnlyItsValues: a value a rule covers written otherwise is
// no drift, and hides nothing else the document writes otherwise: a key
// escaped next to it is still the "N bytes" drift it is alone.
func TestRuleCoversOnlyItsValues(t *testing.T) {
	allow := htmlAllow(t, "  - path: $.t\n    format: '.*'\n    reason: r\n  - path: $.p\n    format: '\\d+'\n    parts: true\n    reason: r\n")
	for _, tc := range []struct{ base, cur string }{
		{`{"t":"x","a/b":1}`, `{"t":"x","a\/b":1}`},
		{`{"t":"x/y","a/b":1}`, `{"t":"x\/y","a\/b":1}`},
		{`{"p":"\/a\/1","a/b":1}`, `{"p":"/a/1","a\/b":1}`},
	} {
		if ds := run(t, allow, LevelValues, side(200, jsonType, tc.base), side(200, jsonType, tc.cur)); len(ds) != 1 || !has(ds, KindEncoding, "$") {
			t.Errorf("%s → %s: %s", tc.base, tc.cur, driftList(ds))
		}
	}
	for _, tc := range []struct{ base, cur string }{
		{`{"t":"x/y","a":1}`, `{"t":"x\/y","a":1}`},
		{`{"p":"\/a\/1","a":1}`, `{"p":"/a/1","a":1}`},
	} {
		if ds := run(t, allow, LevelValues, side(200, jsonType, tc.base), side(200, jsonType, tc.cur)); len(ds) != 0 {
			t.Errorf("%s → %s: %s", tc.base, tc.cur, driftList(ds))
		}
	}
	// With a byte order mark the document is read after it: a key is no
	// value at $, even under a rule that covers $ too.
	const bom = "\ufeff"
	all := htmlAllow(t, "  - path: $*\n    format: '.*'\n    reason: r\n")
	if ds := run(t, all, LevelValues, side(200, jsonType, bom+`{"t":"x/y","a/b":1}`), side(200, jsonType, bom+`{"t":"x\/y","a\/b":1}`)); len(ds) != 1 || !has(ds, KindEncoding, "$") {
		t.Errorf("byte order mark, a key escaped: %s", driftList(ds))
	}
	if ds := run(t, allow, LevelValues, side(200, jsonType, bom+`{"t":"x/y"}`), side(200, jsonType, bom+`{"t":"x\/y"}`)); len(ds) != 0 {
		t.Errorf("byte order mark, a covered value written otherwise: %s", driftList(ds))
	}
}

// TestRuleCoversDevalue: values of a devalue payload are compared at the
// paths of the value it decodes to, so the bytes of a payload are never
// masked by the paths of its JSON: a slot nothing refers to that changed
// next to a covered value written otherwise is the drift it is alone.
func TestRuleCoversDevalue(t *testing.T) {
	htmlT := [][2]string{{"content-type", "text/html"}}
	page := func(payload string) Side {
		return side(200, htmlT, `<html><body><script id="__NUXT_DATA__" type="application/json">`+payload+`</script></body></html>`)
	}
	allow := htmlAllow(t, "  - path: 'html>body>script#__NUXT_DATA__$*'\n    format: '.*'\n    reason: r\n")
	if ds := run(t, allow, LevelValues, page(`[{"a":1},"x/y","old"]`), page(`[{"a":1},"x\/y","new"]`)); len(ds) == 0 {
		t.Errorf("an unreferenced slot changed: no drift")
	}
}

// TestLayoutInUTF16: a document in UTF-16 or UTF-32 is read as its text:
// its layout changes as in UTF-8, also next to a value a rule lets
// change, and another encoding is a drift whatever else changed.
func TestLayoutInUTF16(t *testing.T) {
	allow := htmlAllow(t, "  - path: $.t\n    format: '\\d+'\n    reason: r\n")
	for _, big := range []bool{false, true} {
		doc := func(s string) Side { return side(200, jsonType, inUTF16(s, big)) }
		for _, tc := range [][2]string{
			{`{"t":1,"a":2}`, `{"t":2, "a":2}`},
			{"{\n  \"t\": 1,\n  \"a\": 2\n}", "{\n    \"t\": 2,\n    \"a\": 2\n}"},
			{`{"t":1,"a":2}`, `{"t":2,"a":2}` + "\n"},
		} {
			utf8 := run(t, allow, LevelValues, side(200, jsonType, tc[0]), side(200, jsonType, tc[1]))
			ds := run(t, allow, LevelValues, doc(tc[0]), doc(tc[1]))
			if len(ds) == 0 || places(ds) != places(utf8) {
				t.Errorf("big endian %v, %q → %q: %s, in UTF-8 %s", big, tc[0], tc[1], driftList(ds), driftList(utf8))
			}
		}
		if ds := run(t, allow, LevelValues, side(200, jsonType, `{"t":1,"a":2}`), doc(`{"t":2,"a":2}`)); !has(ds, KindEncoding, "$") {
			t.Errorf("big endian %v, UTF-8 → UTF-16: %s", big, driftList(ds))
		}
	}
}
