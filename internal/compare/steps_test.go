package compare

import (
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/sheet"
)

func TestStepDrifts(t *testing.T) {
	exit := func(code int, out string) Side {
		s := side(0, nil, out)
		s.Record.Exit = &code
		return s
	}
	ds := run(t, nil, LevelStatus, exit(0, "a\nb\n"), exit(1, "a\nc\n"))
	if len(ds) != 1 || !has(ds, KindExit, "exit") || ds[0].Baseline != "0" || ds[0].Current != "1" {
		t.Fatalf("exit at status level: %+v", ds)
	}
	ds = run(t, nil, LevelValues, exit(0, "a\nb\nc\n"), exit(0, "a\nx\ny\n"))
	if len(ds) != 1 || !has(ds, KindBody, "line 2") || ds[0].Baseline != "b" || ds[0].Current != "x" {
		t.Fatalf("output: %+v", ds)
	}
	if ds = run(t, nil, LevelValues, exit(0, `{"n":1}`), exit(0, `{"n":2}`)); !has(ds, KindValue, "$.n") {
		t.Fatalf("JSON output: %+v", ds)
	}
	a, b := side(429, nil, `{}`), side(429, nil, `{}`)
	a.Record.Repeat, b.Record.Repeat = "200×60, 429×1", "200×59, 429×2"
	if ds = run(t, nil, LevelStatus, a, b); len(ds) != 1 || !has(ds, KindRepeat, "repeat") {
		t.Fatalf("repeat: %+v", ds)
	}
}

func TestSheetDrifts(t *testing.T) {
	xlsxType := [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}
	csvType := [][2]string{{"content-type", "text/csv; charset=utf-8"}}
	x := func(rows ...[]string) Side { return side(200, xlsxType, string(sheet.WriteXLSX(rows))) }
	ds := run(t, nil, LevelValues, x([]string{"region", "value"}, []string{"North", "12.5"}), x([]string{"region", "amount"}, []string{"North", "12.5"}, []string{"East", "4"}))
	if len(ds) != 3 || !has(ds, KindCell, "xlsx:Sheet1!B1") || !has(ds, KindCell, "xlsx:Sheet1!A3") || !has(ds, KindCell, "xlsx:Sheet1!B3") {
		t.Fatalf("xlsx: %+v", ds)
	}
	if ds[0].Baseline != "value" || ds[0].Current != "amount" {
		t.Fatalf("values: %+v", ds[0])
	}
	// A value changed: no drift at shape level, a new cell is one.
	ds = run(t, nil, LevelShape, x([]string{"a", "1"}), x([]string{"a", "2", "3"}))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:Sheet1!C1") {
		t.Fatalf("xlsx at shape level: %+v", ds)
	}
	c := func(body string) Side { return side(200, csvType, body) }
	if ds = run(t, nil, LevelValues, c("a,b\n1,2\n"), c("a,b\n1,3\n")); len(ds) != 1 || !has(ds, KindCell, "csv!B2") {
		t.Fatalf("csv: %+v", ds)
	}
	if ds = run(t, nil, LevelValues, c("a,b\n1,2\n"), c("a,b\r\n1,2\r\n")); len(ds) != 1 || !has(ds, KindBody, "csv") {
		t.Fatalf("csv line endings: %+v", ds)
	}
	// Not a workbook after all: bytes.
	if ds = run(t, nil, LevelValues, side(200, xlsxType, "PK broken"), side(200, xlsxType, "PK other")); !has(ds, KindBody, "") {
		t.Fatalf("broken xlsx: %+v", ds)
	}
}

func TestCellSignature(t *testing.T) {
	a := Drift{Kind: KindCell, Where: "xlsx:Sheet1!B2"}.Signature()
	b := Drift{Kind: KindCell, Where: "xlsx:Sheet1!B30"}.Signature()
	if a != b || a != "cell xlsx:Sheet1!B*" || a == (Drift{Kind: KindCell, Where: "xlsx:Sheet1!C2"}).Signature() {
		t.Fatalf("%s %s", a, b)
	}
	for _, kind := range []string{KindCellFormat, KindCellFormula, KindType} {
		a, b := Drift{Kind: kind, Where: "xlsx:S!B2"}.Signature(), Drift{Kind: kind, Where: "xlsx:S!B3"}.Signature()
		if a != b {
			t.Errorf("%s: %s %s", kind, a, b)
		}
	}
}

var xlsxHeaders = [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}

func book(sheets ...sheet.Rows) Side {
	return side(200, xlsxHeaders, string(sheet.WriteWorkbook(sheets...)))
}

func TestSheetNames(t *testing.T) {
	users := sheet.Rows{Name: "Users", Rows: [][]string{{"ime", "broj"}, {"Ana", "1"}}}
	members := sheet.Rows{Name: "Članovi", Rows: [][]string{{"x"}}}
	odd := func(v string) sheet.Rows { return sheet.Rows{Name: "Q1 2026! 'O'", Rows: [][]string{{"a", v}}} }

	// Cells are placed by the workbook's names, quoted when needed.
	ds := run(t, nil, LevelValues, book(users, members, odd("1")), book(users, sheet.Rows{Name: "Članovi", Rows: [][]string{{"y"}}}, odd("2")))
	if len(ds) != 2 || !has(ds, KindCell, "xlsx:Članovi!A1") || !has(ds, KindCell, "xlsx:'Q1 2026! ''O'''!B1") {
		t.Fatalf("named: %+v", ds)
	}
	if s := (Drift{Kind: KindCell, Where: "xlsx:'Q1 2026! ''O'''!B17"}).Signature(); s != "cell xlsx:'Q1 2026! ''O'''!B*" {
		t.Fatalf("signature %s", s)
	}

	// A renamed worksheet is missing under the old name and new under the
	// new one, with the hint; its cells are not compared.
	renamedUsers := users
	renamedUsers.Name = "Accounts"
	ds = run(t, nil, LevelValues, book(users, members), book(renamedUsers, members))
	if len(ds) != 2 || !has(ds, KindSheetMissing, "xlsx:Users") || !has(ds, KindSheetExtra, "xlsx:Accounts") {
		t.Fatalf("renamed: %+v", ds)
	}
	if ds[0].Baseline != "4 cells" || ds[0].Current != "the same cells are in xlsx:Accounts" || ds[1].Current != "4 cells" {
		t.Fatalf("renamed values: %+v", ds)
	}
	// At shape level too: a worksheet is structure.
	if ds = run(t, nil, LevelShape, book(users, members), book(renamedUsers, members)); len(ds) != 2 {
		t.Fatalf("renamed at shape level: %+v", ds)
	}

	// Worksheets in another order: one drift, the cells still compared by
	// name, and no drift for the other bytes.
	ds = run(t, nil, LevelValues, book(users, members, odd("1")), book(members, users, odd("1")))
	if len(ds) != 1 || !has(ds, KindSheetOrder, "xlsx") || ds[0].Baseline != `["Users","Članovi","'Q1 2026! ''O'''"]` || ds[0].Current != `["Članovi","Users","'Q1 2026! ''O'''"]` {
		t.Fatalf("order: %+v", ds)
	}
	ds = run(t, nil, LevelValues, book(users, members), book(members, sheet.Rows{Name: "Users", Rows: [][]string{{"ime", "broj"}, {"Ana", "2"}}}))
	if len(ds) != 2 || !has(ds, KindSheetOrder, "xlsx") || !has(ds, KindCell, "xlsx:Users!B2") {
		t.Fatalf("order and a cell: %+v", ds)
	}
}

func TestCellRules(t *testing.T) {
	allow, err := allowlist.Parse([]byte("version: 1\nbody:\n" +
		"  - path: 'xlsx:Users!A1'\n    format: 'Exported \\d{4}-\\d\\d-\\d\\d \\d\\d:\\d\\d'\n    reason: export time\n" +
		"  - path: 'xlsx:*!C*'\n    format: '[0-9a-f]{8}'\n    reason: generated id\n" +
		"  - path: 'csv!A1'\n    format: 'Exported \\d\\d:\\d\\d'\n    reason: export time\n" +
		"  - path: '*'\n    format: '.*'\n    reason: JSON only\n"))
	if err != nil {
		t.Fatal(err)
	}
	users := func(at, n, id1, id2 string) sheet.Rows {
		return sheet.Rows{Name: "Users", Rows: [][]string{{at}, {"Ana", n, id1}, {"Bora", "2", id2}}}
	}
	// The export time and the ids may change; another cell still drifts,
	// and the bytes the allowed cells changed are no drift of their own.
	ds := run(t, allow, LevelValues,
		book(users("Exported 2026-10-29 09:15", "1", "0000aaaa", "0000bbbb")),
		book(users("Exported 2026-10-30 10:00", "5", "1111cccc", "2222dddd")))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:Users!B2") {
		t.Fatalf("allowed: %+v", ds)
	}
	ds = run(t, allow, LevelValues,
		book(users("Exported 2026-10-29 09:15", "1", "0000aaaa", "0000bbbb")),
		book(users("Exported 2026-10-30 10:00", "1", "1111cccc", "2222dddd")))
	if len(ds) != 0 {
		t.Fatalf("only allowed changes: %+v", ds)
	}
	// A value out of the format is a format drift; an emptied cell too.
	ds = run(t, allow, LevelValues,
		book(users("Exported 2026-10-29 09:15", "1", "0000aaaa", "0000bbbb")),
		book(users("juče", "1", "0000aaaa", "")))
	if len(ds) != 2 || !has(ds, KindFormat, "xlsx:Users!A1") || !has(ds, KindFormat, "xlsx:Users!C3") || ds[0].Current != "juče" {
		t.Fatalf("format: %+v", ds)
	}
	// The rule is about Users: the same cell elsewhere drifts.
	other := sheet.Rows{Name: "Other", Rows: [][]string{{"Exported 2026-10-29 09:15"}}}
	other2 := sheet.Rows{Name: "Other", Rows: [][]string{{"Exported 2026-10-30 10:00"}}}
	if ds = run(t, allow, LevelValues, book(other), book(other2)); len(ds) != 1 || !has(ds, KindCell, "xlsx:Other!A1") {
		t.Fatalf("other sheet: %+v", ds)
	}

	csvType := [][2]string{{"content-type", "text/csv"}}
	c := func(body string) Side { return side(200, csvType, body) }
	if ds = run(t, allow, LevelValues, c("Exported 09:15\na,1\n"), c("Exported 10:00\na,2\n")); len(ds) != 1 || !has(ds, KindCell, "csv!B2") {
		t.Fatalf("csv: %+v", ds)
	}
	if ds = run(t, allow, LevelValues, c("Exported 09:15\na,1\n"), c("Exported 10:00\na,1\n")); len(ds) != 0 {
		t.Fatalf("csv allowed: %+v", ds)
	}
}
