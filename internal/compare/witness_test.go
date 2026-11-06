package compare

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// TestWitnessTellsLossyDrifts: for every kind whose description leaves
// content out, two different responses give the same description; the
// witness tells them apart, so an expectation of one never matches the
// other.
func TestWitnessTellsLossyDrifts(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	htmlT := [][2]string{{"content-type", "text/html"}}
	binT := [][2]string{{"content-type", "application/octet-stream"}}
	textT := [][2]string{{"content-type", "text/plain"}}
	csvT := [][2]string{{"content-type", "text/csv"}}
	step := func(out string) Side {
		s := side(0, nil, out)
		code := 0
		s.Record.Exit = &code
		return s
	}
	keys := func(order ...int) string {
		var items []string
		for _, n := range order {
			items = append(items, fmt.Sprintf(`{"id":%d}`, n))
		}
		return `{"data":[` + strings.Join(items, ",") + `]}`
	}
	var order1, order2, base30 []int
	for i := 1; i <= 30; i++ {
		base30 = append(base30, i)
	}
	order1 = append(append([]int{}, base30[1:]...), 1)                           // 1 moved to the end
	order2 = append(append(append([]int{}, base30[1:25]...), 1), base30[25:]...) // 1 moved past 25
	cells := func(v string) string { return `<row r="1"><c r="A1"><v>1</v></c></row>` + v }
	// A key's value past what its description shows (memberTextMax).
	long := strings.Repeat("x", memberTextMax)
	for _, tc := range []struct {
		kind           string
		allow          *allowlist.List
		base1, cur1    Side
		base2, cur2    Side
		where, kindSee string
	}{
		{kind: "key-extra", base1: side(200, jsonT, `{"a":1}`), cur1: side(200, jsonT, `{"a":1,"is_admin":{"p":"`+long+`","v":false}}`), cur2: side(200, jsonT, `{"a":1,"is_admin":{"p":"`+long+`","v":true}}`), kindSee: KindKeyExtra},
		{kind: "key-missing", base1: side(200, jsonT, `{"a":1,"x":{"p":"`+long+`","v":1}}`), cur1: side(200, jsonT, `{"a":1}`), base2: side(200, jsonT, `{"a":1,"x":{"p":"`+long+`","v":2}}`), kindSee: KindKeyMissing},
		{kind: "array-length", base1: side(200, jsonT, `[1]`), cur1: side(200, jsonT, `[1,2]`), cur2: side(200, jsonT, `[1,3]`), kindSee: KindArrayLength},
		{kind: "type", base1: side(200, jsonT, `{"a":"x"}`), cur1: side(200, jsonT, `{"a":{"b":1}}`), cur2: side(200, jsonT, `{"a":{"b":2}}`), kindSee: KindType},
		{kind: "body bytes", base1: side(200, binT, "abc"), cur1: side(200, binT, "abd"), cur2: side(200, binT, "abe"), kindSee: KindBody},
		{kind: "text lines", base1: side(200, textT, "a\nb\nc"), cur1: side(200, textT, "a\nX\nc"), cur2: side(200, textT, "a\nX\nd"), kindSee: KindBody},
		{kind: "command output", base1: step("a\nb\nc"), cur1: step("a\nX\nc"), cur2: step("a\nX\nd"), kindSee: KindBody},
		{kind: "element-extra", base1: side(200, htmlT, `<p>a</p>`), cur1: side(200, htmlT, `<p>a</p><div>1</div>`), cur2: side(200, htmlT, `<p>a</p><div>2</div>`), kindSee: KindElementExtra},
		{kind: "element-missing", base1: side(200, htmlT, `<p>a</p><div>1</div>`), cur1: side(200, htmlT, `<p>a</p>`), base2: side(200, htmlT, `<p>a</p><div>2</div>`), kindSee: KindElementMissing},
		{kind: "keyed key-extra", allow: keyedAllow(t, ""), base1: side(200, jsonT, users("1")), cur1: side(200, jsonT, `{"data":[{"id":1,"name":"u1"},{"id":2,"p":"`+long+`","name":"a"}]}`), cur2: side(200, jsonT, `{"data":[{"id":1,"name":"u1"},{"id":2,"p":"`+long+`","name":"b"}]}`), kindSee: KindKeyExtra},
		{kind: "item-order past 20", allow: keyedAllow(t, ""), base1: side(200, jsonT, keys(base30...)), cur1: side(200, jsonT, keys(order1...)), cur2: side(200, jsonT, keys(order2...)), kindSee: KindItemOrder},
		{kind: "xlsx cell type", base1: workbook(t, `<row r="1"><c r="B1"><v>1</v></c></row>`, "", "1"), cur1: workbook(t, `<row r="1"><c r="B1" t="inlineStr"><is><t>1</t></is></c></row>`, "", "1"), cur2: workbook(t, `<row r="1"><c r="B1" t="inlineStr"><is><t>2</t></is></c></row>`, "", "1"), kindSee: KindType},
		{kind: "csv bytes", base1: side(200, csvT, "a,b\n1,2\n"), cur1: side(200, csvT, "\"a\",b\n1,2\n"), cur2: side(200, csvT, "a,\"b\"\n1,2\n"), kindSee: KindBody},
		{kind: "xlsx workbook parts", base1: workbookWith(t, cells(""), "", "1", map[string]string{"xl/x.xml": "aa"}), cur1: workbookWith(t, cells(""), "", "1", map[string]string{"xl/x.xml": "bb"}), cur2: workbookWith(t, cells(""), "", "1", map[string]string{"xl/x.xml": "cc"}), kindSee: KindBody},
	} {
		if tc.base2.Record == nil {
			tc.base2 = tc.base1
		}
		if tc.cur2.Record == nil {
			tc.cur2 = tc.cur1
		}
		d1, d2 := run(t, tc.allow, LevelValues, tc.base1, tc.cur1), run(t, tc.allow, LevelValues, tc.base2, tc.cur2)
		if len(d1) == 0 || len(d1) != len(d2) {
			t.Errorf("%s: %s\n%s", tc.kind, driftList(d1), driftList(d2))
			continue
		}
		if driftList(d1) != driftList(d2) {
			t.Errorf("%s: the descriptions differ already, the case does not test the witness:\n%s\n%s", tc.kind, driftList(d1), driftList(d2))
			continue
		}
		differs := false
		for i := range d1 {
			if d1[i].Kind == tc.kindSee && d1[i].Witness != "" && d1[i].Witness != d2[i].Witness {
				differs = true
			}
		}
		if !differs {
			t.Errorf("%s: two different responses give the same %s drift and witness: %s", tc.kind, tc.kindSee, driftList(d1))
		}
	}
}

// sheets is an XLSX body with worksheets of the given names (one cell
// each) and styles.xml.
func sheets(t testing.TB, styles string, names ...string) Side {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, s string) {
		w, err := zw.Create(name)
		if err == nil {
			_, err = w.Write([]byte(s))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var list, rels strings.Builder
	for i, n := range names {
		fmt.Fprintf(&list, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, n, i+1, i+1)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Target="worksheets/sheet%d.xml"/>`, i+1, i+1)
	}
	add("xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`+list.String()+`</sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+rels.String()+`</Relationships>`)
	for i, n := range names {
		add(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>`+n+`</t></is></c></row></sheetData></worksheet>`)
	}
	add("xl/styles.xml", `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+styles+`</styleSheet>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return side(200, [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}, buf.String())
}

// TestWitnessSheetsCoverTheWorkbook: a worksheet missing, new or moved is
// reported without the change of the rest of the workbook (its list of
// worksheets changes with it); what else changed there, such as the
// styles, is in the witness, so an expected worksheet drift does not pass
// it.
func TestWitnessSheetsCoverTheWorkbook(t *testing.T) {
	styles := func(n string) string {
		return `<numFmts count="1"><numFmt numFmtId="164" formatCode="` + n + `"/></numFmts>`
	}
	base := sheets(t, styles("0.0"), "A")
	for _, tc := range []struct {
		name       string
		cur1, cur2 Side
		kind       string
	}{
		{"sheet-extra", sheets(t, styles("0.0"), "A", "B"), sheets(t, styles("0.00"), "A", "B"), KindSheetExtra},
		{"sheet-missing", sheets(t, styles("0.0")), sheets(t, styles("0.00")), KindSheetMissing},
	} {
		d1, d2 := run(t, nil, LevelValues, base, tc.cur1), run(t, nil, LevelValues, base, tc.cur2)
		if driftList(d1) != driftList(d2) || !has(d1, tc.kind, "") && len(d1) == 0 {
			t.Fatalf("%s: %s\n%s", tc.name, driftList(d1), driftList(d2))
		}
		if material(d1) == material(d2) {
			t.Errorf("%s: the styles changed, and the drifts read the same: %s", tc.name, driftList(d1))
		}
	}
	two := sheets(t, styles("0.0"), "A", "B")
	moved1, moved2 := sheets(t, styles("0.0"), "B", "A"), sheets(t, styles("0.00"), "B", "A")
	d1, d2 := run(t, nil, LevelValues, two, moved1), run(t, nil, LevelValues, two, moved2)
	if !has(d1, KindSheetOrder, "xlsx") || driftList(d1) != driftList(d2) || material(d1) == material(d2) {
		t.Errorf("sheet-order: %s\n%s", driftList(d1), driftList(d2))
	}
}

// kindsAt are the kinds and places of drifts, without the values their
// descriptions show (a new key's value).
func kindsAt(ds []Drift) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Kind+" "+d.Where)
	}
	return strings.Join(out, "\n")
}

func material(ds []Drift) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.DigestText())
	}
	return strings.Join(out, "\n")
}

// TestWitnessAllowsWhatTheAllowlistAllows: a value an allowlist rule lets
// change inside content only one side has (a new key, item or element)
// does not change the witness, so the expectation holds on the next
// capture; a value no rule names still does.
func TestWitnessAllowsWhatTheAllowlistAllows(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	htmlT := [][2]string{{"content-type", "text/html"}}
	allow, err := allowlist.Parse([]byte("version: 1\nbody:\n" +
		"  - path: $.x.created_at\n    format: '\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}'\n    reason: time\n" +
		"  - path: $[*].nonce\n    format: '[0-9a-f]{8}'\n    reason: nonce\n" +
		"  - path: html>body>div@data-at\n    format: '\\d+'\n    reason: time\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		base, cur1, cur2 Side
		same             bool
	}{
		{"new key, allowed time", side(200, jsonT, `{"a":1}`), side(200, jsonT, `{"a":1,"x":{"created_at":"2026-01-01 10:00:00"}}`), side(200, jsonT, `{"a":1,"x":{"created_at":"2026-01-02 11:00:00"}}`), true},
		{"new key, value no rule names", side(200, jsonT, `{"a":1}`), side(200, jsonT, `{"a":1,"x":{"id":1}}`), side(200, jsonT, `{"a":1,"x":{"id":2}}`), false},
		{"new key, allowed time out of format", side(200, jsonT, `{"a":1}`), side(200, jsonT, `{"a":1,"x":{"created_at":"2026-01-01 10:00:00"}}`), side(200, jsonT, `{"a":1,"x":{"created_at":"soon"}}`), false},
		{"new item, allowed nonce", side(200, jsonT, `[{"nonce":"00000000"}]`), side(200, jsonT, `[{"nonce":"00000000"},{"nonce":"0a0a0a0a"}]`), side(200, jsonT, `[{"nonce":"00000000"},{"nonce":"1b1b1b1b"}]`), true},
		{"new element, allowed attribute", side(200, htmlT, `<p>a</p>`), side(200, htmlT, `<p>a</p><div data-at="1">x</div>`), side(200, htmlT, `<p>a</p><div data-at="2">x</div>`), true},
		{"new element, other text", side(200, htmlT, `<p>a</p>`), side(200, htmlT, `<p>a</p><div data-at="1">x</div>`), side(200, htmlT, `<p>a</p><div data-at="1">y</div>`), false},
	} {
		d1, d2 := run(t, allow, LevelValues, tc.base, tc.cur1), run(t, allow, LevelValues, tc.base, tc.cur2)
		if len(d1) == 0 || kindsAt(d1) != kindsAt(d2) {
			t.Errorf("%s: %s\n%s", tc.name, driftList(d1), driftList(d2))
			continue
		}
		if same := material(d1) == material(d2); same != tc.same {
			t.Errorf("%s: same material %v, want %v: %s", tc.name, same, tc.same, driftList(d1))
		}
	}
}

// TestDriftsDoNotHideBytes: a drift in a body does not hide what else
// changed in its bytes (a final line end of a JSON body, the line endings
// of a CSV file): an expectation of the drift does not pass them.
func TestDriftsDoNotHideBytes(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	csvT := [][2]string{{"content-type", "text/csv"}}
	if ds := run(t, nil, LevelValues, side(200, jsonT, `{"a":1}`), side(200, jsonT, "\ufeff"+`{"a":2}`+"\n")); !has(ds, KindValue, "$.a") || !has(ds, KindEncoding, "$") {
		t.Errorf("json: %s", driftList(ds))
	}
	// CRLF line ends next to a cell drift are no drift of their own (as
	// before expectations), but they are in its witness.
	lf, crlf := run(t, nil, LevelValues, side(200, csvT, "a,b\n1,2\n"), side(200, csvT, "a,b\n1,3\n")), run(t, nil, LevelValues, side(200, csvT, "a,b\n1,2\n"), side(200, csvT, "a,b\r\n1,3\r\n"))
	if driftList(lf) != driftList(crlf) || digests(lf) == digests(crlf) {
		t.Errorf("csv line ends: %s\n%s", driftList(lf), driftList(crlf))
	}
	if ds := run(t, nil, LevelValues, side(200, csvT, "a,b\n1,2\n"), side(200, csvT, "\ufeffa,b\n1,3\n")); !has(ds, KindBody, "csv") {
		t.Errorf("csv byte order mark: %s", driftList(ds))
	}
}

// TestWitnessCanonical: the reviewed collisions of hand-joined witnesses
// (an attribute holding "@b=2", the spacing of a text, a value's kind
// under a rule, a parts rule's text that reads as structure) give
// different digests; what stands around a JSON document, keys written
// with escapes and fields quoted without need are drifts of their own.
func TestWitnessCanonical(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	htmlT := [][2]string{{"content-type", "text/html"}}
	csvT := [][2]string{{"content-type", "text/csv"}}
	rule := func(y string) *allowlist.List {
		l, err := allowlist.Parse([]byte("version: 1\nbody:\n" + y))
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	any := rule("  - path: $.x.v\n    format: '.*'\n    reason: r\n")
	digit := rule("  - path: $.x.v\n    format: '\\d+'\n    reason: r\n")
	parts := rule("  - path: $.x.a\n    format: '\\d+'\n    parts: true\n    reason: r\n  - path: $.x.b\n    format: '\\d+'\n    parts: true\n    reason: r\n")
	for _, tc := range []struct {
		name         string
		allow        *allowlist.List
		base, c1, c2 Side
	}{
		{"string or number under a rule", digit, side(200, jsonT, `{}`), side(200, jsonT, `{"x":{"v":"5"}}`), side(200, jsonT, `{"x":{"v":5}}`)},
		{"null or a string under .*", any, side(200, jsonT, `{}`), side(200, jsonT, `{"x":{"v":null}}`), side(200, jsonT, `{"x":{"v":"null"}}`)},
		{"true or a string under .*", any, side(200, jsonT, `{}`), side(200, jsonT, `{"x":{"v":true}}`), side(200, jsonT, `{"x":{"v":"true"}}`)},
		{"a parts rule's text as structure", parts, side(200, jsonT, `{}`), side(200, jsonT, `{"x":{"a":"1,\"b\":2"}}`), side(200, jsonT, `{"x":{"a":7,"b":8}}`)},
		{"an attribute holding @b=2", nil, side(200, htmlT, `<p>a</p>`), side(200, htmlT, `<p>a</p><div a="1@b=2">x</div>`), side(200, htmlT, `<p>a</p><div a="1" b="2">x</div>`)},
		{"spacing of a new element's text", nil, side(200, htmlT, `<p>a</p>`), side(200, htmlT, `<p>a</p><div>a <b>x</b></div>`), side(200, htmlT, `<p>a</p><div>a<b>x</b></div>`)},
		{"spacing of a changed text", nil, side(200, htmlT, `<div>a<b>x</b>c</div>`), side(200, htmlT, `<div>a<b>x</b>d</div>`), side(200, htmlT, `<div>a<b>x</b> d</div>`)},
	} {
		if tc.allow == nil {
			tc.allow = allowlist.Empty()
		}
		d1, d2 := run(t, tc.allow, LevelValues, tc.base, tc.c1), run(t, tc.allow, LevelValues, tc.base, tc.c2)
		if len(run(t, tc.allow, LevelValues, tc.c1, tc.c2)) == 0 {
			t.Fatalf("%s: the case does not drift", tc.name)
		}
		if digests(d1) == digests(d2) {
			t.Errorf("%s: the same digests: %s", tc.name, driftList(d1))
		}
	}
	// A layout drift does not hide a byte order mark, and the drift says
	// what stands around the document.
	ds := run(t, nil, LevelValues, side(200, jsonT, `{"a":1}`), side(200, jsonT, "\ufeff{\n  \"a\": 1\n}\n"))
	if !has(ds, KindEncoding, "$") || !strings.Contains(driftList(ds), "byte order mark, final line end") || strings.Contains(driftList(ds), "\x00") {
		t.Errorf("edges: %s", driftList(ds))
	}
	// A final line end alone is the layout, once.
	if ds := run(t, nil, LevelValues, side(200, jsonT, `{"a":1}`), side(200, jsonT, `{"a":2}`+"\n")); strings.Count(driftList(ds), "encoding") != 1 || !strings.Contains(driftList(ds), "layout") {
		t.Errorf("final line end: %s", driftList(ds))
	}
	// Keys written with escapes and fields quoted without need next to
	// another drift are no drift of their own, but in its witness.
	plain, esc := run(t, nil, LevelValues, side(200, jsonT, `{"a":1,"b":1}`), side(200, jsonT, `{"a":1,"b":2}`)), run(t, nil, LevelValues, side(200, jsonT, `{"a":1,"b":1}`), side(200, jsonT, `{"\u0061":1,"b":2}`))
	if driftList(plain) != driftList(esc) || digests(plain) == digests(esc) {
		t.Errorf("key escapes: %s\n%s", driftList(plain), driftList(esc))
	}
	if ds := run(t, nil, LevelValues, side(200, jsonT, `{"a":1}`), side(200, jsonT, `{"\u0061":1}`)); !strings.Contains(driftList(ds), "bytes") {
		t.Errorf("key escapes alone: %s", driftList(ds))
	}
	q1, q2 := run(t, nil, LevelValues, side(200, csvT, "a,b\n1,2\n"), side(200, csvT, "a,b\n1,3\n")), run(t, nil, LevelValues, side(200, csvT, "a,b\n1,2\n"), side(200, csvT, "a,\"b\"\n1,3\n"))
	if driftList(q1) != driftList(q2) || digests(q1) == digests(q2) {
		t.Errorf("quotes: %s\n%s", driftList(q1), driftList(q2))
	}
}

// TestWitnessTextNextToElements: a text "<b>" and a child <b> never read
// the same, in the witness or in the report; the spacing next to an
// element only one side has is a text drift, and in the witness of that
// element.
func TestWitnessTextNextToElements(t *testing.T) {
	htmlT := [][2]string{{"content-type", "text/html"}}
	page := func(body string) Side { return side(200, htmlT, "<html><body>"+body+"</body></html>") }
	x := page(`<div><b></b>zz</div>`)
	a := page(`<div>t1 <b></b>&lt;b&gt;</div>`)
	b := page(`<div>t1 &lt;b&gt;<b></b></div>`)
	if len(run(t, nil, LevelValues, a, b)) == 0 || digests(run(t, nil, LevelValues, x, a)) == digests(run(t, nil, LevelValues, x, b)) {
		t.Errorf("text or element: %s\n%s", driftList(run(t, nil, LevelValues, x, a)), driftList(run(t, nil, LevelValues, x, b)))
	}
	if ds := run(t, nil, LevelValues, x, a); !strings.Contains(driftList(ds), `"t1 <b> &lt;b>"`) {
		t.Errorf("display: %s", driftList(ds))
	}
	// Spacing next to a replaced element: a client reads "aa" and "a a".
	ds := run(t, nil, LevelValues, page(`<div>a<b id="1"></b>a</div>`), page(`<div>a <b id="x y"></b>a</div>`))
	if !has(ds, KindText, "html>body>div") {
		t.Errorf("spacing next to a replaced element: %s", driftList(ds))
	}
	// The same visible text after the element went: no text drift.
	if ds := run(t, nil, LevelValues, page(`<p>Hello <i>x</i> big</p>`), page(`<p>Hello big</p>`)); has(ds, KindText, "html>body>p") {
		t.Errorf("same reading: %s", driftList(ds))
	}
	// The witness of a new element has the texts around it.
	y := page(`<div>a</div>`)
	c1, c2 := page(`<div>a<i>n</i></div>`), page(`<div>a <i>n</i></div>`)
	d1, d2 := run(t, nil, LevelValues, y, c1), run(t, nil, LevelValues, y, c2)
	if digests(d1) == digests(d2) {
		t.Errorf("texts around a new element: %s\n%s", driftList(d1), driftList(d2))
	}
}
