package allowlist

import (
	"reflect"
	"strings"
	"testing"
)

func TestHeaderForRequest(t *testing.T) {
	l, err := Parse([]byte("version: 1\nheaders:\n  - request: post-*\n    name: etag\n    format: 'W/\"[0-9a-f]+\"'\n    reason: new row\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l.HeaderFor("post-items", "ETag") == nil {
		t.Error("rule for post-items not found")
	}
	if l.HeaderFor("get-items", "etag") != nil || l.Header("etag") != nil {
		t.Error("a rule for some requests applies to others")
	}
}

func TestBodyRuleValidation(t *testing.T) {
	bad := map[string]string{
		"parts without format":   "  - path: html*\n    parts: true\n    any_length: true\n    reason: x\n",
		"parts with any_length":  "  - path: $.a\n    format: 'x'\n    parts: true\n    any_length: true\n    reason: x\n",
		"parts matching empty":   "  - path: html*\n    format: '\\d*'\n    parts: true\n    reason: x\n",
		"html path without root": "  - path: body>p@class\n    format: 'x'\n    reason: x\n",
		"no attribute name":      "  - path: html>body>p@\n    format: 'x'\n    reason: x\n",
		"any_length on elements": "  - path: html>body>ul\n    any_length: true\n    reason: x\n",
	}
	for name, rule := range bad {
		if _, err := Parse([]byte("version: 1\nbody:\n" + rule)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := "version: 1\nbody:\n" +
		"  - path: html>body>div#app>ul>li[3]@class\n    format: 'x'\n    reason: x\n" +
		"  - path: html>body>script#__NEXT_DATA__$.buildId\n    format: '\\w+'\n    reason: x\n" +
		"  - path: self.__next_f$.0.b\n    format: '\\w+'\n    reason: x\n" +
		"  - path: '*.created_at'\n    format: 'x'\n    reason: x\n" +
		"  - path: $.@type\n    format: 'x'\n    reason: x\n" +
		"  - path: html*\n    format: '/_next/static/[\\w/.~-]+'\n    parts: true\n    reason: x\n" +
		"  - path: html>body>script$.items\n    any_length: true\n    reason: x\n" +
		"  - path: html*\n    any_length: true\n    reason: x\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
}

func TestValueRules(t *testing.T) {
	l, err := Parse([]byte("version: 1\nbody:\n" +
		"  - path: html*\n    format: '[0-9a-f]{4}'\n    parts: true\n    reason: hash\n" +
		"  - path: html*\n    format: 'v\\d'\n    parts: true\n    reason: version\n" +
		"  - path: html>body>p\n    format: 'x'\n    reason: whole\n"))
	if err != nil {
		t.Fatal(err)
	}
	whole, parts := l.ValueRules("r", "html>body>p")
	if whole == nil || whole.Reason != "whole" || parts != nil {
		t.Fatalf("a whole-value rule must decide alone: %v %v", whole, parts)
	}
	whole, parts = l.ValueRules("r", "html>body>a@href")
	if whole != nil || len(parts) != 2 {
		t.Fatalf("parts: %v %v", whole, parts)
	}
	if !SameParts(parts, "/app.abcd.js?v1", "/app.0123.js?v2") {
		t.Error("parts of both rules may change")
	}
	if SameParts(parts, "/app.abcd.js", "/lib.abcd.js") || SameParts(nil, "a", "a") {
		t.Error("the rest of the value must stay")
	}
}

// A value cannot pass for a matched part, whatever it holds: the parts are
// set aside structurally, not replaced by a placeholder character.
func TestSamePartsLiteralText(t *testing.T) {
	l, err := Parse([]byte("version: 1\nbody:\n  - path: $*\n    format: 'v\\d+'\n    parts: true\n    reason: x\n" +
		"  - path: $*\n    format: '[0-9a-f]{4}'\n    parts: true\n    reason: y\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, parts := l.ValueRules("r", "$.a")
	for _, c := range [][2]string{{"v1", "\x00"}, {"x\x00", "xv2"}, {"a v1 b", "a \x00 b"}, {"v1", "v1v2"}, {"v1", ""}} {
		if SameParts(parts, c[0], c[1]) || SameParts(parts, c[1], c[0]) {
			t.Errorf("%q and %q are the same", c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"v1", "v22"}, {"a v1 b abcd", "a v9 b 0123"}, {"v1abcd", "v2ffff"}} {
		if !SameParts(parts, c[0], c[1]) {
			t.Errorf("%q and %q differ", c[0], c[1])
		}
	}
}

// ValueRules and TextRules pass over rules without a format: any_length is
// about arrays, and AnyLength finds it.
func TestTextRules(t *testing.T) {
	l, err := Parse([]byte("version: 1\nbody:\n  - path: html*\n    any_length: true\n    reason: x\n  - path: html>body>p\n    format: 'x'\n    reason: y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if whole, _ := l.ValueRules("r", "html>body>p"); whole == nil || whole.Reason != "y" {
		t.Fatalf("ValueRules: %v", whole)
	}
	if whole, _ := l.TextRules("r", "html>body>p"); whole == nil || whole.Reason != "y" {
		t.Fatalf("TextRules: %v", whole)
	}
	if whole, parts := l.TextRules("r", "html>body>div"); whole != nil || parts != nil {
		t.Fatalf("TextRules without a format: %v %v", whole, parts)
	}
	if !l.AnyLength("r", "html>body>div") || l.AnyLength("r", "$.a") {
		t.Fatal("AnyLength")
	}
	if (&BodyRule{AnyLength: true}).Matches("anything") {
		t.Fatal("a rule without a format matches a value")
	}
}

// ForRequest leaves the answers for its request as they were.
func TestForRequest(t *testing.T) {
	l, err := Parse([]byte("version: 1\nbody:\n" +
		"  - request: get-a*\n    path: $.x\n    format: 'a'\n    reason: x\n" +
		"  - path: $.x\n    format: 'b'\n    reason: x\n" +
		"  - request: get-b\n    path: $.*\n    format: 'c'\n    parts: true\n    reason: x\n" +
		"  - request: get-?\n    path: $.y\n    format: 'd'\n    reason: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []string{"get-a", "get-ab", "get-b", "get-c", "post-a", "get-é"} {
		f := l.ForRequest(req)
		for _, path := range []string{"$", "$.x", "$.y", "$.z", "$.x.y"} {
			w1, p1 := l.ValueRules(req, path)
			w2, p2 := f.ValueRules(req, path)
			if (w1 == nil) != (w2 == nil) || (w1 != nil && w1.Format != w2.Format) || len(p1) != len(p2) {
				t.Errorf("%s %s: %v %v, narrowed %v %v", req, path, w1, p1, w2, p2)
			}
		}
	}
	if n := len(l.ForRequest("post-a").Body); n != 1 {
		t.Errorf("post-a keeps %d rules, want 1", n)
	}
}

func TestHeaderIgnoreCaseValidation(t *testing.T) {
	for _, tc := range []struct {
		rule string
		ok   bool
	}{
		{"name: content-type\n    ignore_case: true\n    reason: r", true},
		{"name: content-type\n    format: 'x'\n    reason: r", true},
		{"name: content-type\n    reason: r", false},
		{"name: content-type\n    ignore_case: true\n    format: 'x'\n    reason: r", false},
		{"name: content-type\n    ignore_case: true", false},
	} {
		_, err := Parse([]byte("version: 1\nheaders:\n  - " + tc.rule + "\n"))
		if (err == nil) != tc.ok {
			t.Errorf("%q: err %v", tc.rule, err)
		}
	}
}

// TestRawRule: $raw is the whole body, taken literally; it needs a format,
// and a $ path that is neither $raw nor a JSON path is refused, since it
// would never apply.
func TestRawRule(t *testing.T) {
	bad := map[string]string{
		"raw without format":  "  - path: $raw\n    any_length: true\n    reason: x\n",
		"raw with any_length": "  - path: $raw\n    format: 'x'\n    any_length: true\n    reason: x\n",
		"raw with parts":      "  - path: $raw\n    format: 'x'\n    parts: true\n    reason: x\n",
		"$ path to no place":  "  - path: $body\n    format: 'x'\n    reason: x\n",
		"$raw as a glob":      "  - path: $raw*\n    format: 'x'\n    reason: x\n",
		"$ path with a quote": "  - path: $\"a\"\n    format: 'x'\n    reason: x\n",
	}
	for name, rule := range bad {
		if _, err := Parse([]byte("version: 1\nbody:\n" + rule)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	l, err := Parse([]byte("version: 1\nbody:\n" +
		"  - path: '*'\n    format: '.*'\n    reason: wildcard\n" +
		"  - request: redirect-*\n    path: $raw\n    format: 'x'\n    reason: page\n" +
		"  - path: $\n    any_length: true\n    reason: root\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r := l.RawRule("redirect-home"); r == nil || r.Reason != "page" {
		t.Fatalf("raw rule: %+v", r)
	}
	if r := l.RawRule("home"); r != nil {
		t.Fatalf("a wildcard path or another request's rule counts for the whole body: %+v", r)
	}
	if r := l.ForRequest("redirect-home").RawRule("redirect-home"); r == nil {
		t.Fatal("raw rule lost by ForRequest")
	}
	if r, _ := l.ValueRules("redirect-home", "$.raw"); r == nil || r.Reason != "wildcard" {
		t.Fatalf("value rule: %+v", r)
	}
}

func TestCellRuleValidation(t *testing.T) {
	rule := func(path string) string {
		return "version: 1\nbody:\n  - path: \"" + strings.ReplaceAll(path, `"`, `\"`) + "\"\n    format: 'x'\n    reason: x\n"
	}
	bad := map[string]string{
		"xlsx:sheet1!B2":    "names the worksheet by its part",
		"xlsx:Users":        "names a whole worksheet",
		"xlsx:!A1":          "no worksheet",
		"xlsx:Users!b2":     "capitals",
		"xlsx:Users!2B":     "starts with a digit",
		"xlsx:Users!B":      "ends with a letter",
		"xlsx:Users!B2C":    "ends with a letter",
		"xlsx:Users!*2B?":   "a letter comes after a digit",
		"xlsx:Users!A0":     "rows count from 1",
		"xlsx:Users!?":      "shorter than",
		"xlsx:Users!":       "no cell",
		"xlsx:Users!A-1":    "letters and digits",
		"xlsx:Q1 2026!A1":   "xlsx:'Q1 2026'!",
		"xlsx:'Users'!A1":   "write xlsx:Users!",
		"xlsx:'O'Brien'!A1": "doubled",
		"xlsx:[Users]!A1":   "part name",
		"csv":               "names no cell",
		"csv!":              "no cell",
		"csv!a1":            "capitals",
		"xlsx!A1":           "names no cell",
		"xlsx":              "names no cell",
		"xlsx:Users!??":     "cannot tell the column from the row",
		"csv!*?":            "cannot tell the column from the row",
	}
	for path, want := range bad {
		_, err := Parse([]byte(rule(path)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
	if _, err := Parse([]byte("version: 1\nbody:\n  - path: csv!A1\n    any_length: true\n    reason: x\n")); err == nil || !strings.Contains(err.Error(), "any_length") {
		t.Errorf("any_length on a cell: %v", err)
	}
	for _, path := range []string{
		"xlsx:Users!A1", "xlsx:Users!B*", "xlsx:*!A1", "xlsx:Users!*1", "xlsx:Članovi!AA10",
		"xlsx:'Q1 2026'!A1", "xlsx:'Hi!'!A1", "xlsx:'O''Brien'!C3", "xlsx:'sheet1'!A1", "xlsx:[sheet1]!A1",
		"xlsx:Q* 2026!A1", "xlsx:*", "csv!A1", "csv!B*", "csv!*",
	} {
		if _, err := Parse([]byte(rule(path))); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestCellRules(t *testing.T) {
	l, err := Parse([]byte("version: 1\nbody:\n" +
		"  - path: '*'\n    format: '.*'\n    reason: everything JSON\n" +
		"  - path: 'xlsx:*!A1'\n    format: 'x'\n    reason: cell\n" +
		"  - path: 'csv!B*'\n    format: 'y'\n    reason: column\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*List{l, l.ForRequest("r")} {
		if !c.HasCellRules() {
			t.Fatal("no cell rules")
		}
		if w, _ := c.CellRules("r", "xlsx:'Q1 2026'!A1"); w == nil || w.Reason != "cell" {
			t.Fatalf("xlsx: %v", w)
		}
		if w, _ := c.CellRules("r", "csv!B7"); w == nil || w.Reason != "column" {
			t.Fatalf("csv: %v", w)
		}
		if w, _ := c.CellRules("r", "csv!A7"); w != nil {
			t.Fatalf("csv A7: %v", w)
		}
		if w, _ := c.ValueRules("r", "csv!B7"); w == nil || w.Reason != "everything JSON" {
			t.Fatalf("a value: %v", w)
		}
	}
}

// A cell glob matches the column and the row apart, as documented: B* is
// column B (not BA), *1 row 1 (not A11).
func TestCellGlobs(t *testing.T) {
	cases := []struct {
		path      string
		match, no []string
	}{
		{"xlsx:S!B*", []string{"xlsx:S!B1", "xlsx:S!B1048576"}, []string{"xlsx:S!BA1", "xlsx:S!AB1", "xlsx:T!B1"}},
		{"xlsx:S!*1", []string{"xlsx:S!A1", "xlsx:S!XFD1"}, []string{"xlsx:S!A11", "xlsx:S!A21", "xlsx:S!B101"}},
		{"xlsx:S!*", []string{"xlsx:S!A1", "xlsx:S!ZZ99"}, []string{"xlsx:T!A1"}},
		{"xlsx:S!B?", []string{"xlsx:S!B1", "xlsx:S!B9"}, []string{"xlsx:S!B10", "xlsx:S!BA1"}},
		{"xlsx:S!?1", []string{"xlsx:S!C1"}, []string{"xlsx:S!AC1", "xlsx:S!C11"}},
		{"xlsx:S!B*1", []string{"xlsx:S!B1", "xlsx:S!B21"}, []string{"xlsx:S!BA1", "xlsx:S!B12"}},
		{"xlsx:S!*B2", []string{"xlsx:S!B2", "xlsx:S!AB2"}, []string{"xlsx:S!BA2", "xlsx:S!B22"}},
		{"xlsx:*!A1", []string{"xlsx:S!A1", "xlsx:'Hi!'!A1"}, []string{"xlsx:S!A11", "xlsx:S!AA1"}},
		{"xlsx:'Hi!'!B*", []string{"xlsx:'Hi!'!B3"}, []string{"xlsx:'Hi!'!BB3"}},
		{"csv!*5*", []string{"csv!A5", "csv!C51"}, []string{"csv!A15"}},
		{"xlsx:*", []string{"xlsx:S!A11", "xlsx:S!BA1"}, nil},
		{"xlsx:S*", []string{"xlsx:S!A1", "xlsx:Sheet!A11"}, []string{"xlsx:T!A1"}},
	}
	for _, c := range cases {
		l, err := Parse([]byte("version: 1\nbody:\n  - path: \"" + c.path + "\"\n    format: '.*'\n    reason: r\n"))
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		for _, p := range c.match {
			if !l.Body[0].Reaches(p) {
				t.Errorf("%s does not reach %s", c.path, p)
			}
		}
		for _, p := range c.no {
			if l.Body[0].Reaches(p) {
				t.Errorf("%s reaches %s", c.path, p)
			}
		}
	}
}

// Track counts the requests every rule applies to and the changes it
// applied to; Unused lists the rules that did nothing, and why.
func TestUnused(t *testing.T) {
	l, err := Parse([]byte("version: 1\nheaders:\n  - name: date\n    format: '.*'\n    reason: time\n" +
		"body:\n  - path: $.t\n    format: '\\d+'\n    reason: time\n" +
		"  - path: $.old\n    format: '.*'\n    reason: gone\n" +
		"  - request: nosuch\n    path: $.x\n    format: '.*'\n    reason: typo\n" +
		"  - path: $.list\n    any_length: true\n    reason: paging\n"))
	if err != nil {
		t.Fatal(err)
	}
	tracked := l.Track()
	for _, id := range []string{"a", "b"} {
		r := tracked.ForTarget("").ForRequest(id)
		if id == "a" {
			r.ValueRules(id, "$.t")
			r.AnyLength(id, "$.list")
		}
		r.Done(id, true, true)
	}
	type key struct{ kind, why string }
	got := map[int]key{}
	for _, u := range tracked.Unused(false) {
		got[u.Rule*10+len(u.Kind)] = key{u.Kind, u.Why}
	}
	want := map[int]key{16: {"header", "no change"}, 24: {"body", "no change"}, 34: {"body", "no request"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if u := tracked.Unused(true); len(u) != 2 {
		t.Fatalf("selected: %+v", u)
	}
	if l.Unused(false) != nil {
		t.Fatal("an untracked list")
	}

	// At a level that does not reach values (shape), header and value
	// rules had no chance: none is called stale. A rule that matches no
	// request still is, and an any_length rule with no change too.
	loose := l.Track()
	for _, id := range []string{"a", "b"} {
		loose.ForTarget("").ForRequest(id).Done(id, false, true)
	}
	got = map[int]key{}
	for _, u := range loose.Unused(false) {
		got[u.Rule*10+len(u.Kind)] = key{u.Kind, u.Why}
	}
	want = map[int]key{34: {"body", "no request"}, 44: {"body", "no change"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shape: got %v", got)
	}
	// Bodies not on this machine: body rules had no chance either.
	nobody := l.Track()
	nobody.ForTarget("").ForRequest("a").Done("a", true, false)
	for _, u := range nobody.Unused(false) {
		if u.Kind == "body" && u.Why != "no request" {
			t.Fatalf("no bodies: %+v", u)
		}
	}
}
