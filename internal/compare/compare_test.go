package compare

import (
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

func side(status int, headers [][2]string, body string) Side {
	return Side{
		Record: &snapshot.Record{Status: status, Headers: headers, Body: snapshot.Body{SHA256: snapshot.Sum([]byte(body))}},
		Body:   []byte(body),
	}
}

func run(t *testing.T, allow *allowlist.List, level Level, base, cur Side) []Drift {
	t.Helper()
	if allow == nil {
		allow = allowlist.Empty()
	}
	return One("req", base, cur, allow, level)
}

func has(ds []Drift, kind, where string) bool {
	for _, d := range ds {
		if d.Kind == kind && d.Where == where {
			return true
		}
	}
	return false
}

func TestBodyDrifts(t *testing.T) {
	cases := []struct {
		name, base, cur, kind, where string
	}{
		{"type string to int", `{"id":"5"}`, `{"id":5}`, KindType, "$.id"},
		{"int to float", `{"n":1}`, `{"n":1.0}`, KindType, "$.n"},
		{"null to empty string", `{"a":null}`, `{"a":""}`, KindType, "$.a"},
		{"number literal", `{"n":1.0}`, `{"n":1.00}`, KindNumberLiteral, "$.n"},
		{"value", `{"a":"x"}`, `{"a":"y"}`, KindValue, "$.a"},
		{"key missing", `{"a":1,"b":2}`, `{"a":1}`, KindKeyMissing, "$.b"},
		{"key extra", `{"a":1}`, `{"a":1,"b":2}`, KindKeyExtra, "$.b"},
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, KindKeyOrder, "$"},
		{"array length", `[1,2]`, `[1]`, KindArrayLength, "$"},
		{"nested value", `{"d":[{"t":"2024-03-01 10:00:00"}]}`, `{"d":[{"t":"2024-03-01T10:00:00.000000Z"}]}`, KindValue, "$.d[0].t"},
		{"whitespace only", `{"a":1}`, `{"a": 1}`, KindEncoding, "$"},
		{"unicode escape", `{"a":"é"}`, `{"a":"\u00e9"}`, KindEncoding, "$.a"},
		{"byte order mark", `{"a":1}`, "\xEF\xBB\xBF{\"a\":1}", KindEncoding, "$"},
		{"escaping only", `{"a":"x/y"}`, `{"a":"x\/y"}`, KindEncoding, "$.a"},
		{"not json", `<html>a</html>`, `<html>b</html>`, KindBody, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := run(t, nil, LevelValues, side(200, nil, c.base), side(200, nil, c.cur))
			if !has(ds, c.kind, c.where) {
				t.Fatalf("want %s at %s, got %+v", c.kind, c.where, ds)
			}
		})
	}
}

// TestKeyDriftValues: a key that is missing or new shows its value (the
// key is in the place), an object or array cut short.
func TestKeyDriftValues(t *testing.T) {
	long := `"` + strings.Repeat("x", 300) + `"`
	ds := run(t, nil, LevelValues, side(200, nil, `{"a":1,"gone":"old"}`), side(200, nil, `{"a":1,"b":{"x":[1,2]},"c":[`+long+`]}`))
	got := map[string][2]string{}
	for _, d := range ds {
		got[d.Kind+" "+d.Where] = [2]string{d.Baseline, d.Current}
	}
	if v := got["key-missing $.gone"]; v != [2]string{`"old"`, ""} {
		t.Errorf("missing: %q", v)
	}
	if v := got["key-extra $.b"]; v != [2]string{"", `{"x":[1,2]}`} {
		t.Errorf("extra: %q", v)
	}
	if v := got["key-extra $.c"][1]; len([]rune(v)) != memberTextMax+1 || !strings.HasSuffix(v, "…") {
		t.Errorf("long: %q", v)
	}
}

func TestRecursesPastKeyChanges(t *testing.T) {
	// An object whose keys changed is compared further, by the keys both
	// sides share.
	ds := run(t, nil, LevelValues, side(200, nil, `{"a":1,"b":"x"}`), side(200, nil, `{"b":"y"}`))
	if !has(ds, KindKeyMissing, "$.a") || !has(ds, KindValue, "$.b") {
		t.Fatalf("got %+v", ds)
	}
}

func TestIdenticalIsClean(t *testing.T) {
	body := `{"a":[1,2.50,"é",null,true,{"x":{}}],"a":2}`
	if ds := run(t, nil, LevelValues, side(200, [][2]string{{"x", "1"}}, body), side(200, [][2]string{{"x", "1"}}, body)); len(ds) != 0 {
		t.Fatalf("got %+v", ds)
	}
}

func TestStatusAndHeaders(t *testing.T) {
	base := side(200, [][2]string{{"content-type", "application/json"}, {"vary", "a"}, {"vary", "b"}}, `{}`)
	cur := side(500, [][2]string{{"content-type", "text/html"}, {"vary", "b"}, {"vary", "a"}, {"x-new", "1"}}, `{}`)
	ds := run(t, nil, LevelValues, base, cur)
	if !has(ds, KindStatus, "") || !has(ds, KindHeaderValue, "header:content-type") || !has(ds, KindHeaderExtra, "header:x-new") {
		t.Fatalf("got %+v", ds)
	}
	if has(ds, KindHeaderValue, "header:vary") {
		t.Fatal("header order must not count")
	}
	if ds := run(t, nil, LevelStatus, base, cur); len(ds) != 1 {
		t.Fatalf("status level: %+v", ds)
	}
}

func TestLevels(t *testing.T) {
	base, cur := side(200, nil, `{"a":"1","b":1}`), side(200, nil, `{"a":1,"b":2}`)
	if ds := run(t, nil, LevelShape, base, cur); len(ds) != 0 {
		t.Fatalf("shape: %+v", ds)
	}
	if ds := run(t, nil, LevelTypes, base, cur); len(ds) != 1 || ds[0].Kind != KindType {
		t.Fatalf("types: %+v", ds)
	}
	if ds := run(t, nil, LevelValues, base, cur); len(ds) != 2 {
		t.Fatalf("values: %+v", ds)
	}
	if ds := run(t, nil, LevelShape, side(200, nil, `{"a":[1]}`), side(200, nil, `{"a":{}}`)); !has(ds, KindType, "$.a") {
		t.Fatalf("shape must see structure changes: %+v", ds)
	}
}

func testAllow(t *testing.T) *allowlist.List {
	l, err := allowlist.Parse([]byte(`
version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: time of the response
body:
  - request: req
    path: $.token
    format: '[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+'
    reason: new JWT on every login
  - path: $.items
    any_length: true
    reason: test
`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestAllowlist(t *testing.T) {
	allow := testAllow(t)
	h := func(v string) [][2]string { return [][2]string{{"date", v}} }
	base := side(200, h("Wed, 28 Oct 2026 10:08:35 GMT"), `{"token":"a.b.c","items":[1]}`)
	if ds := run(t, allow, LevelValues, base, side(200, h("Thu, 29 Oct 2026 11:00:00 GMT"), `{"token":"aa.bbb.cccc","items":[1,2]}`)); len(ds) != 0 {
		t.Fatalf("allowed changes reported: %+v", ds)
	}
	ds := run(t, allow, LevelValues, base, side(200, h("yesterday"), `{"token":"not-a-jwt","items":[1]}`))
	if !has(ds, KindFormat, "header:date") || !has(ds, KindFormat, "$.token") {
		t.Fatalf("format changes not reported: %+v", ds)
	}
	// An array allowed to change its length explains the bytes that differ.
	if ds := run(t, allow, LevelValues, side(200, nil, `{"items":[1]}`), side(200, nil, `{"items":[1,2]}`)); len(ds) != 0 {
		t.Fatalf("allowed length change reported: %+v", ds)
	}
	// An allowed value change does not hide a change of layout.
	ds = run(t, allow, LevelValues, base, side(200, h("Wed, 28 Oct 2026 10:08:35 GMT"), `{"token":"aa.b.c", "items":[1]}`))
	if !has(ds, KindEncoding, "$") {
		t.Fatalf("layout change not reported: %+v", ds)
	}
}

func TestNotCaptured(t *testing.T) {
	if ds := run(t, nil, LevelValues, side(200, nil, `{}`), Side{}); !has(ds, KindNotCaptured, "") {
		t.Fatalf("got %+v", ds)
	}
}

func TestSignatureGroupsListItems(t *testing.T) {
	a := Drift{Kind: KindValue, Where: "$.data[0].created_at"}
	b := Drift{Kind: KindValue, Where: "$.data[17].created_at"}
	if a.Signature() != b.Signature() {
		t.Fatal("list items must share a signature")
	}
}

func TestSideEffects(t *testing.T) {
	rec := func(se *snapshot.SideEffects) *snapshot.Record {
		return &snapshot.Record{Version: 1, ID: "x", Status: 200, Headers: [][2]string{}, Body: snapshot.Body{SHA256: "a"}, SideEffects: se}
	}
	base := &snapshot.SideEffects{Items: []string{"db/table:faq"}, Rows: []snapshot.RowChange{{Item: "db/faq", Key: "id=3", Change: "inserted"}}}
	same := &snapshot.SideEffects{Items: []string{"db/table:faq"}, Rows: []snapshot.RowChange{{Item: "db/faq", Key: "id=3", Change: "inserted"}}}
	if d := One("x", Side{Record: rec(base)}, Side{Record: rec(same)}, allowlist.Empty(), LevelValues); len(d) != 0 {
		t.Fatalf("equal side effects: %v", d)
	}
	cur := &snapshot.SideEffects{Items: []string{"db/table:users"}, Rows: []snapshot.RowChange{{Item: "db/faq", Key: "id=3", Change: "updated", Columns: []string{"q"}}, {Item: "db/faq", Key: "id=4", Change: "inserted"}}}
	got := One("x", Side{Record: rec(base)}, Side{Record: rec(cur)}, allowlist.Empty(), LevelValues)
	want := []string{
		"side-effect side-effects:db/table:faq changed → unchanged",
		"side-effect side-effects:db/table:users unchanged → changed",
		"side-effect side-effects:db/faq[id=3] inserted → updated q",
		"side-effect side-effects:db/faq[id=4] unchanged → inserted",
	}
	if len(got) != len(want) {
		t.Fatalf("drifts %v", got)
	}
	for i, d := range got {
		if s := d.Kind + " " + d.Where + " " + d.Baseline + " → " + d.Current; s != want[i] {
			t.Errorf("drift %d: %s, want %s", i, s, want[i])
		}
	}
	// A capture that did not track the state has nothing to compare.
	if d := One("x", Side{Record: rec(base)}, Side{Record: rec(nil)}, allowlist.Empty(), LevelValues); len(d) != 0 {
		t.Fatalf("untracked side: %v", d)
	}
	if d := One("x", Side{Record: rec(base)}, Side{Record: rec(cur)}, allowlist.Empty(), LevelStatus); len(d) != 0 {
		t.Fatalf("level status: %v", d)
	}
}

// TestRowTrackingAfterBaseline: rows of a table the baseline did not
// track (row_tracking came after it) are not compared, and a note says
// so once; rows both sides tracked are compared, and a baseline that does
// not say what it tracked has its rows compared as before.
func TestRowTrackingAfterBaseline(t *testing.T) {
	rec := func(se *snapshot.SideEffects) *snapshot.Record {
		return &snapshot.Record{Version: 1, ID: "x", Status: 200, Headers: [][2]string{}, Body: snapshot.Body{SHA256: "a"}, SideEffects: se}
	}
	base := &snapshot.SideEffects{Items: []string{"db/table:orders", "db/table:users"}, Rows: []snapshot.RowChange{{Item: "db/table:users", Key: "id=1", Change: "updated", Columns: []string{"name"}}}}
	cur := &snapshot.SideEffects{Items: []string{"db/table:orders", "db/table:users"}, Rows: []snapshot.RowChange{
		{Item: "db/table:orders", Key: "id=7", Change: "inserted"}, {Item: "db/table:orders", Key: "id=8", Change: "inserted"},
		{Item: "db/table:users", Key: "id=1", Change: "updated", Columns: []string{"email"}}}}
	b := Side{Record: rec(base), RowTables: []string{"db/table:users"}}
	c := Side{Record: rec(cur), RowTables: []string{"db/table:orders", "db/table:users"}}
	drifts, notes := OneNotes("x", b, c, allowlist.Empty(), LevelValues)
	if len(drifts) != 1 || drifts[0].Where != "side-effects:db/table:users[id=1]" {
		t.Errorf("drifts %+v", drifts)
	}
	if len(notes) != 1 || notes[0] != "side-effects:db/table:orders: rows not compared: not tracked in the baseline (row_tracking came after it); make the baseline again to compare them (accept keeps the baseline's tracking)" {
		t.Errorf("notes %q", notes)
	}
	// Turned off since: the baseline's rows of it are not compared.
	if drifts, notes := OneNotes("x", Side{Record: rec(cur), RowTables: c.RowTables}, Side{Record: rec(base), RowTables: b.RowTables}, allowlist.Empty(), LevelValues); len(drifts) != 1 ||
		len(notes) != 1 || !strings.Contains(notes[0], "row_tracking no longer covers the table") {
		t.Errorf("row_tracking removed: %+v %q", drifts, notes)
	}
	b.RowTables = nil
	if drifts, notes := OneNotes("x", b, c, allowlist.Empty(), LevelValues); len(drifts) != 3 || len(notes) != 0 {
		t.Errorf("a baseline that does not say: %+v %q", drifts, notes)
	}
}

func TestContentLengthFollowsBody(t *testing.T) {
	side := func(body string) Side {
		return Side{Record: &snapshot.Record{Version: 1, ID: "x", Status: 200, Headers: [][2]string{{"content-length", fmt.Sprint(len(body))}},
			Body: snapshot.Body{SHA256: snapshot.Sum([]byte(body)), Storage: "public"}}, Body: []byte(body)}
	}
	got := One("x", side(`{"a":1}`), side(`{"a":12}`), allowlist.Empty(), LevelValues)
	if len(got) != 1 || got[0].Where != "$.a" {
		t.Fatalf("drifts %+v", got)
	}
	// The same body with another length is still reported.
	b := side(`{"a":1}`)
	c := side(`{"a":1}`)
	c.Record.Headers = [][2]string{{"content-length", "99"}}
	if got := One("x", b, c, allowlist.Empty(), LevelValues); len(got) != 1 || got[0].Where != "header:content-length" {
		t.Fatalf("drifts %+v", got)
	}
}

func TestHeaderIgnoreCase(t *testing.T) {
	allow, err := allowlist.Parse([]byte("version: 1\nheaders:\n  - name: content-type\n    ignore_case: true\n    reason: charset names are case-insensitive\n"))
	if err != nil {
		t.Fatal(err)
	}
	base := side(200, [][2]string{{"content-type", "text/html; charset=UTF-8"}}, `{}`)
	if ds := run(t, allow, LevelValues, base, side(200, [][2]string{{"content-type", "text/html; charset=utf-8"}}, `{}`)); len(ds) != 0 {
		t.Fatalf("letter case only: %+v", ds)
	}
	for _, v := range []string{"text/html; charset=iso-8859-1", "application/json", "text/html;charset=utf-8"} {
		if ds := run(t, allow, LevelValues, base, side(200, [][2]string{{"content-type", v}}, `{}`)); !has(ds, KindHeaderValue, "header:content-type") {
			t.Fatalf("%s: got %+v", v, ds)
		}
	}
	if ds := run(t, allow, LevelValues, base, side(200, nil, `{}`)); !has(ds, KindHeaderMissing, "header:content-type") {
		t.Fatalf("missing header: %+v", ds)
	}
}

// A large pair is parsed at the same time, with the result of one parse
// after the other.
func TestParseBoth(t *testing.T) {
	big := func(v string) []byte { return []byte(`{"a":"` + strings.Repeat("x", 600<<10) + `","v":` + v + `}`) }
	for _, c := range [][2][]byte{{big("1"), big("2")}, {big("1"), []byte("{")}, {[]byte("nope"), big("2")}, {[]byte(`[1]`), []byte(`[2]`)}} {
		jb, errB, jc, errC := parseBoth(c[0], c[1])
		wb, werrB := jsonx.Parse(c[0])
		wc, werrC := jsonx.Parse(c[1])
		text := func(v *jsonx.Value) string {
			if v == nil {
				return "nil"
			}
			return string(jsonx.Marshal(v, false))
		}
		if fmt.Sprint(errB) != fmt.Sprint(werrB) || fmt.Sprint(errC) != fmt.Sprint(werrC) || text(jb) != text(wb) || text(jc) != text(wc) {
			t.Fatalf("%.20s / %.20s: %v %v", c[0], c[1], errB, errC)
		}
	}
}
