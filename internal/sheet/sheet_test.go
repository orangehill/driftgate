package sheet

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

func TestXLSXRoundTrip(t *testing.T) {
	rows := [][]string{{"region", "value"}, {"North <&>", "12.5"}, {"", "x"}}
	data := WriteXLSX(rows)
	if !bytes.Equal(data, WriteXLSX(rows)) {
		t.Fatal("the same rows gave other bytes")
	}
	if !IsXLSX(data) || IsXLSX([]byte("a,b\n")) {
		t.Fatal("IsXLSX")
	}
	sheets, err := ReadXLSX(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A1": "region", "B1": "value", "A2": "North <&>", "B2": "12.5", "B3": "x"}
	if len(sheets) != 1 || sheets[0].Name != "Sheet1" || !sheets[0].Named || !reflect.DeepEqual(sheets[0].Cells, want) {
		t.Fatalf("%+v", sheets)
	}
	if got := sheets[0].Rows(); !reflect.DeepEqual(got, rows) {
		t.Fatalf("rows %q", got)
	}
}

func TestCSVAndRefs(t *testing.T) {
	s, err := ReadCSV([]byte("\xef\xbb\xbfa,b\r\n1,\"x,y\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Cells, map[string]string{"A1": "a", "B1": "b", "A2": "1", "B2": "x,y"}) {
		t.Fatalf("%+v", s.Cells)
	}
	other := Sheet{Cells: map[string]string{"AA10": "z", "C1": "c"}}
	if got := Refs(s, other); !reflect.DeepEqual(got, []string{"A1", "B1", "C1", "A2", "B2", "AA10"}) {
		t.Fatalf("refs %v", got)
	}
	if Ref(0, 26) != "AA1" || Ref(2, 0) != "A3" {
		t.Fatal("Ref")
	}
}

// rezip rebuilds a workbook with some parts replaced (nil removes one).
func rezip(t *testing.T, data []byte, change map[string]*string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var parts [][2]string
	for _, f := range zr.File {
		raw, err := readPart(f)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := change[f.Name]
		switch {
		case !ok:
			parts = append(parts, [2]string{f.Name, string(raw)})
		case v != nil:
			parts = append(parts, [2]string{f.Name, *v})
		}
		delete(change, f.Name)
	}
	for name, v := range change {
		if v != nil {
			parts = append(parts, [2]string{name, *v})
		}
	}
	return zipParts(parts)
}

func names(t *testing.T, data []byte) []string {
	t.Helper()
	sheets, err := ReadXLSX(data)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range sheets {
		out = append(out, s.PathName())
	}
	return out
}

func TestWorkbookNames(t *testing.T) {
	data := WriteWorkbook(
		Rows{"Users", [][]string{{"a"}}},
		Rows{"Q1 2026", [][]string{{"b"}}},
		Rows{"Hi!", [][]string{{"c"}}},
		Rows{"O'Brien", [][]string{{"d"}}},
		Rows{"Članovi", [][]string{{"e"}}},
		Rows{"sheet7", [][]string{{"f"}}},
		Rows{"[x]", [][]string{{"g"}}},
	)
	want := []string{"Users", "'Q1 2026'", "'Hi!'", "'O''Brien'", "Članovi", "'sheet7'", "'[x]'"}
	if got := names(t, data); !reflect.DeepEqual(got, want) {
		t.Fatalf("names %q", got)
	}
	sheets, _ := ReadXLSX(data)
	if sheets[4].Name != "Članovi" || !sheets[4].Named || sheets[4].Cells["A1"] != "e" {
		t.Fatalf("%+v", sheets[4])
	}

	// The workbook's order, not the part numbers; an absolute target; a
	// chart sheet has no cells and is left out.
	wb := xml.Header + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:x="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>` +
		`<sheet name="Drugi" sheetId="2" x:id="rId2"/><sheet name="Grafikon" sheetId="3" x:id="rId3"/><sheet name="Prvi" sheetId="1" x:id="rId1"/></sheets></workbook>`
	rels := xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="w" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="w" Target="/xl/worksheets/sheet2.xml"/>` +
		`<Relationship Id="rId3" Type="c" Target="chartsheets/sheet1.xml"/></Relationships>`
	chart := `<chartsheet/>`
	two := WriteWorkbook(Rows{"a", [][]string{{"1"}}}, Rows{"b", [][]string{{"2"}}})
	got := names(t, rezip(t, two, map[string]*string{"xl/workbook.xml": &wb, "xl/_rels/workbook.xml.rels": &rels, "xl/chartsheets/sheet1.xml": &chart}))
	if !reflect.DeepEqual(got, []string{"Drugi", "Prvi"}) {
		t.Fatalf("workbook order %q", got)
	}
}

func TestWorkbookFallback(t *testing.T) {
	var sheets []Rows
	for i := 1; i <= 10; i++ {
		sheets = append(sheets, Rows{fmt.Sprintf("List %d", i), [][]string{{strconv.Itoa(i)}}})
	}
	data := WriteWorkbook(sheets...)
	byNumber := []string{"[sheet1]", "[sheet2]", "[sheet3]", "[sheet4]", "[sheet5]", "[sheet6]", "[sheet7]", "[sheet8]", "[sheet9]", "[sheet10]"}
	garbage := "<workbook><sheets><sheet"
	entity := xml.Header + `<!DOCTYPE workbook [<!ENTITY big "xxxxxxxx">]><workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="&big;" r:id="rId1"/></sheets></workbook>`
	dangling := xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet99.xml"/></Relationships>`
	dup := xml.Header + `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="A" r:id="rId1"/><sheet name="a" r:id="rId2"/></sheets></workbook>`
	for name, change := range map[string]map[string]*string{
		"no workbook.xml":      {"xl/workbook.xml": nil},
		"no relationships":     {"xl/_rels/workbook.xml.rels": nil},
		"broken workbook.xml":  {"xl/workbook.xml": &garbage},
		"entity":               {"xl/workbook.xml": &entity},
		"target not there":     {"xl/_rels/workbook.xml.rels": &dangling},
		"names equal but case": {"xl/workbook.xml": &dup},
	} {
		got := names(t, rezip(t, data, change))
		if !reflect.DeepEqual(got, byNumber) {
			t.Errorf("%s: %q", name, got)
		}
	}
}
