package compare

import (
	"archive/zip"
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
)

// workbook is an XLSX body with one worksheet S holding cells (the XML of
// <sheetData>), styles (the XML of styles.xml, none when empty) and a
// creation time in docProps.
func workbook(t *testing.T, cells, styles, created string) Side {
	t.Helper()
	return workbookWith(t, cells, styles, created, nil)
}

// workbookWith is workbook with more parts (name → content).
func workbookWith(t *testing.T, cells, styles, created string, parts map[string]string) Side {
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
	add("xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="S" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	add("xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`)
	add("xl/worksheets/sheet1.xml", `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+cells+`</sheetData></worksheet>`)
	if styles != "" {
		add("xl/styles.xml", `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+styles+`</styleSheet>`)
	}
	for _, name := range slices.Sorted(maps.Keys(parts)) {
		add(name, parts[name])
	}
	add("docProps/core.xml", `<coreProperties><created>`+created+`</created></coreProperties>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return side(200, [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}, buf.String())
}

// A cell's type is compared: the number 1 that became the text "1" is a
// type drift at the types level, also when another cell was allowed to
// change (which used to turn off the only check that saw it).
func TestCellTypes(t *testing.T) {
	allow := htmlAllow(t, "  - path: xlsx:S!A1\n    format: '\\d'\n    reason: time\n")
	a1 := func(v string) string { return `<row r="1"><c r="A1"><v>` + v + `</v></c>` }
	base := workbook(t, a1("1")+`<c r="B1"><v>1</v></c></row>`, "", "2026-01-01")
	cur := workbook(t, a1("2")+`<c r="B1" t="inlineStr"><is><t>1</t></is></c></row>`, "", "2026-01-02")
	for _, level := range []Level{LevelTypes, LevelValues} {
		if ds := run(t, allow, level, base, cur); len(ds) != 1 || !has(ds, KindType, "xlsx:S!B1") || ds[0].Baseline != "number" || ds[0].Current != "string" {
			t.Fatalf("%s: %+v", level, ds)
		}
	}
	if ds := run(t, allow, LevelShape, base, cur); len(ds) != 0 {
		t.Fatalf("shape: %+v", ds)
	}
	// A shared string and an inline string are both text.
	shared := workbook(t, `<row r="1"><c r="B1" t="str"><v>x</v></c></row>`, "", "1")
	inline := workbook(t, `<row r="1"><c r="B1" t="inlineStr"><is><t>x</t></is></c></row>`, "", "1")
	if ds := both(t, nil, shared, inline); len(ds) != 0 {
		t.Fatalf("string kinds: %+v", ds)
	}
}

// Number format, style and formula of a cell are compared at the values
// level; the rest of the workbook too, whatever cells a rule let change.
// The document properties (times, author) are not part of it.
func TestCellFormatsAndFormulas(t *testing.T) {
	allow := htmlAllow(t, "  - path: xlsx:S!A1\n    format: '\\d'\n    reason: time\n")
	styles := `<numFmts><numFmt numFmtId="164" formatCode="0.00"/></numFmts><cellXfs><xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="14"/><xf numFmtId="0" fontId="1"/></cellXfs>`
	book := func(a1, b1 string) Side {
		return workbook(t, `<row r="1"><c r="A1"><v>`+a1+`</v></c>`+b1+`</row>`, styles, "2026-01-0"+a1)
	}
	base := book("1", `<c r="B1" s="1"><f>A1*2</f><v>2</v></c>`)
	cases := []struct{ name, b1, kind, baseline, current string }{
		{"format", `<c r="B1" s="2"><f>A1*2</f><v>2</v></c>`, KindCellFormat, `"0.00"`, `"builtin 14"`},
		{"general", `<c r="B1"><f>A1*2</f><v>2</v></c>`, KindCellFormat, `"0.00"`, `""`},
		{"formula", `<c r="B1" s="1"><f>A1+A1</f><v>2</v></c>`, KindCellFormula, `"A1*2"`, `"A1+A1"`},
		{"value", `<c r="B1" s="1"><v>2</v></c>`, KindCellFormula, `"A1*2"`, `""`},
	}
	for _, c := range cases {
		ds := run(t, allow, LevelValues, base, book("2", c.b1))
		if len(ds) != 1 || !has(ds, c.kind, "xlsx:S!B1") || ds[0].Baseline != c.baseline || ds[0].Current != c.current {
			t.Errorf("%s: %+v", c.name, ds)
		}
		if ds := run(t, allow, LevelTypes, base, book("2", c.b1)); len(ds) != 0 {
			t.Errorf("%s at types: %+v", c.name, ds)
		}
	}
	plain := func(a1, s string) Side { return book(a1, `<c r="B1"`+s+`><v>5</v></c>`) }
	if ds := run(t, allow, LevelValues, plain("1", ""), plain("2", ` s="3"`)); len(ds) != 1 || !has(ds, KindCellFormat, "xlsx:S!B1") || ds[0].Current != "style 3" {
		t.Errorf("style: %+v", ds)
	}
	// Only the document properties differ (and an allowed cell): no drift.
	if ds := run(t, allow, LevelValues, plain("1", ""), plain("2", "")); len(ds) != 0 {
		t.Errorf("document properties: %+v", ds)
	}
	// The styles changed, and an allowed cell too: still a body drift.
	other := workbook(t, `<row r="1"><c r="A1"><v>2</v></c><c r="B1"><v>5</v></c></row>`, styles+`<fonts count="2"/>`, "x")
	if ds := run(t, allow, LevelValues, plain("1", ""), other); len(ds) != 1 || !has(ds, KindBody, "xlsx") {
		t.Errorf("styles: %+v", ds)
	}
	// A shared formula: the cells that use it have the defining cell's text.
	shared := func(f string) Side {
		return workbook(t, `<row r="1"><c r="A1"><f t="shared" si="0" ref="A1:A2">`+f+`</f><v>1</v></c></row><row r="2"><c r="A2"><f t="shared" si="0"/><v>1</v></c></row>`, "", "1")
	}
	if ds := run(t, nil, LevelValues, shared("B1"), shared("C1")); len(ds) != 2 || !has(ds, KindCellFormula, "xlsx:S!A2") {
		t.Errorf("shared formula: %+v", ds)
	}
}

// An empty cell keeps its formula and its style: a sum over another range
// that came out empty, a formula that has no cached value, a fill on an
// empty cell. Neither side's value changed, so only these say what did.
func TestEmptyCells(t *testing.T) {
	styles := `<numFmts><numFmt numFmtId="164" formatCode="0.00"/></numFmts><cellXfs><xf numFmtId="0"/><xf numFmtId="0" fillId="1"/><xf numFmtId="0" fillId="2"/><xf numFmtId="164"/></cellXfs>`
	book := func(a1 string) Side {
		return workbook(t, `<row r="1">`+a1+`<c r="B1"><v>1</v></c></row>`, styles, "1")
	}
	cases := []struct{ name, base, cur, kind, baseline, current string }{
		{"empty result", `<c r="A1"><f>SUM(B1:B2)</f><v></v></c>`, `<c r="A1"><f>SUM(B1:B9)</f><v></v></c>`, KindCellFormula, `"SUM(B1:B2)"`, `"SUM(B1:B9)"`},
		{"no cached value", `<c r="A1"><f>SUM(B1:B2)</f></c>`, `<c r="A1"><f>NOW()</f></c>`, KindCellFormula, `"SUM(B1:B2)"`, `"NOW()"`},
		{"formula removed", `<c r="A1"><f>SUM(B1:B2)</f></c>`, ``, KindCellFormula, `"SUM(B1:B2)"`, `""`},
		{"style", `<c r="A1" s="1"/>`, `<c r="A1" s="2"/>`, KindCellFormat, "style 1", "style 2"},
		{"style added", ``, `<c r="A1" s="2"/>`, KindCellFormat, "style 0", "style 2"},
		{"style removed", `<c r="A1" s="1"/>`, `<c r="A1"/>`, KindCellFormat, "style 1", "style 0"},
		{"number format", `<c r="A1" s="1"/>`, `<c r="A1" s="3"/>`, KindCellFormat, `""`, `"0.00"`},
	}
	for _, c := range cases {
		ds := run(t, nil, LevelValues, book(c.base), book(c.cur))
		if len(ds) != 1 || !has(ds, c.kind, "xlsx:S!A1") || ds[0].Baseline != c.baseline || ds[0].Current != c.current {
			t.Errorf("%s: %+v", c.name, ds)
		}
		if ds := run(t, nil, LevelTypes, book(c.base), book(c.cur)); len(ds) != 0 {
			t.Errorf("%s at types: %+v", c.name, ds)
		}
	}
	// The same empty cells, and an empty cell with no style of its own
	// against none: no drift.
	for _, a1 := range []string{`<c r="A1"><f>SUM(B1:B2)</f></c>`, `<c r="A1" s="1"/>`} {
		if ds := run(t, nil, LevelValues, book(a1), book(a1)); len(ds) != 0 {
			t.Errorf("same %s: %+v", a1, ds)
		}
	}
	if ds := run(t, nil, LevelValues, book(`<c r="A1" s="0"/>`), book(``)); len(ds) != 0 {
		t.Errorf("default style: %+v", ds)
	}
	// A value emptied is the value; its formula is still compared.
	ds := run(t, nil, LevelValues, book(`<c r="A1"><f>B1</f><v>1</v></c>`), book(`<c r="A1"><f>B2</f></c>`))
	if len(ds) != 2 || !has(ds, KindCell, "xlsx:S!A1") || !has(ds, KindCellFormula, "xlsx:S!A1") {
		t.Errorf("emptied: %+v", ds)
	}
	// A value removed with its cell is one drift, not one per property.
	ds = run(t, nil, LevelValues, book(`<c r="A1" s="1"><f>B1</f><v>1</v></c>`), book(``))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:S!A1") {
		t.Errorf("removed: %+v", ds)
	}
}

// The runs of a rich text are formatted (bold, a color): the same text in
// other runs is a cell-format drift, for a shared and an inline string.
func TestRichText(t *testing.T) {
	sst := func(si string) Side {
		return workbookWith(t, `<row r="1"><c r="A1" t="s"><v>0</v></c></row>`, "", "1", map[string]string{
			"xl/sharedStrings.xml": `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` + si + `</sst>`})
	}
	inline := func(is string) Side {
		return workbook(t, `<row r="1"><c r="A1" t="inlineStr"><is>`+is+`</is></c></row>`, "", "1")
	}
	plain, bold := `<si><t>xy</t></si>`, `<si><r><rPr><b/><color rgb="FFFF0000"/></rPr><t>xy</t></r></si>`
	ds := run(t, nil, LevelValues, sst(plain), sst(bold))
	if len(ds) != 1 || !has(ds, KindCellFormat, "xlsx:S!A1") || ds[0].Baseline != "plain text" || ds[0].Current != `rich text <r len="2"><b/><color rgb="FFFF0000"/></r>` {
		t.Errorf("shared: %+v", ds)
	}
	split := `<si><r><t>x</t></r><r><rPr><b/></rPr><t>y</t></r></si>`
	splitOther := `<si><r><rPr><b/></rPr><t>x</t></r><r><t>y</t></r></si>`
	if ds := run(t, nil, LevelValues, sst(split), sst(splitOther)); len(ds) != 1 || !has(ds, KindCellFormat, "xlsx:S!A1") {
		t.Errorf("bold moved: %+v", ds)
	}
	is := func(si string) string { return strings.TrimSuffix(strings.TrimPrefix(si, "<si>"), "</si>") }
	if ds := run(t, nil, LevelValues, inline(is(plain)), inline(is(bold))); len(ds) != 1 || !has(ds, KindCellFormat, "xlsx:S!A1") {
		t.Errorf("inline: %+v", ds)
	}
	// Runs without properties, or indented, are the same text.
	same := []string{`<si><r><t>x</t></r><r><t>y</t></r></si>`, `<si><r><rPr></rPr><t>xy</t></r></si>`}
	for _, si := range same {
		if ds := run(t, nil, LevelValues, sst(plain), sst(si)); len(ds) != 0 {
			t.Errorf("%s: %+v", si, ds)
		}
	}
	indented := "<si><r><rPr>\n  <b/>\n  <color rgb=\"FFFF0000\"/>\n</rPr><t>xy</t></r></si>"
	if ds := run(t, nil, LevelValues, sst(bold), sst(indented)); len(ds) != 0 {
		t.Errorf("indented: %+v", ds)
	}
	// Another text in the same formatting is the value only.
	if ds := run(t, nil, LevelValues, sst(bold), sst(strings.Replace(bold, "xy", "xz", 1))); len(ds) != 1 || !has(ds, KindCell, "xlsx:S!A1") {
		t.Errorf("text: %+v", ds)
	}
}

// Rows and cells without their optional reference are placed as ECMA-376
// says, not dropped.
func TestCellsWithoutReference(t *testing.T) {
	book := func(v string) Side {
		return workbook(t, `<row><c><v>1</v></c><c><v>`+v+`</v></c></row><row r="4"><c r="C4"><v>3</v></c><c><v>4</v></c></row><row><c><v>5</v></c></row>`, "", "1")
	}
	ds := run(t, nil, LevelValues, book("2"), book("9"))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:S!B1") {
		t.Fatalf("got %+v", ds)
	}
	ds = run(t, nil, LevelValues, book("2"), workbook(t, `<row><c><v>1</v></c><c><v>2</v></c></row><row r="4"><c r="C4"><v>3</v></c><c><v>8</v></c></row><row><c><v>5</v></c></row>`, "", "1"))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:S!D4") {
		t.Fatalf("D4: %+v", ds)
	}
	ds = run(t, nil, LevelValues, book("2"), workbook(t, `<row><c><v>1</v></c><c><v>2</v></c></row><row r="4"><c r="C4"><v>3</v></c><c><v>4</v></c></row><row><c><v>6</v></c></row>`, "", "1"))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:S!A5") {
		t.Fatalf("A5: %+v", ds)
	}
}

// In a CSV, a cell a rule let change still leaves the line ends and the
// byte order mark compared.
func TestCSVStyleWithAllowedCell(t *testing.T) {
	allow := htmlAllow(t, "  - path: csv!A1\n    format: '\\d'\n    reason: time\n")
	csv := func(s string) Side { return side(200, [][2]string{{"content-type", "text/csv"}}, s) }
	if ds := run(t, allow, LevelValues, csv("1,a\n2,b\n"), csv("2,a\n2,b\n")); len(ds) != 0 {
		t.Fatalf("allowed: %+v", ds)
	}
	if ds := run(t, allow, LevelValues, csv("1,a\n2,b\n"), csv("2,a\r\n2,b\r\n")); len(ds) != 1 || !has(ds, KindBody, "csv") {
		t.Fatalf("line ends: %+v", ds)
	}
	if ds := run(t, nil, LevelValues, csv("1,a\n"), csv("1,\"a\"\n")); len(ds) != 1 || !has(ds, KindBody, "csv") {
		t.Fatalf("quoting: %+v", ds)
	}
}
