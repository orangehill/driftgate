// Package sheet reads and writes the spreadsheets APIs upload and export:
// CSV, and XLSX (Office Open XML). A cell is the text of its value; an XLSX
// cell also has a type, a number format and a formula (Cell), and the rest
// of a workbook (styles, the worksheets apart from their cells) is kept to
// be compared as it is.
package sheet

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/lazy"
)

// Sheet is one worksheet: cell reference (A1) → value.
type Sheet struct {
	// Name is the name the workbook gives the worksheet ("Users"), or
	// its part name ("sheet1") when the workbook does not name it (Named
	// false); "csv" for a CSV file.
	Name  string
	Named bool
	Cells map[string]string
	// Meta holds what an XLSX cell has besides its value, for every cell
	// in Cells and every empty cell with a formula or a style of its own
	// (a cell without a value is not in Cells); nil for a CSV file.
	Meta map[string]Cell
	// Rich holds how the rich texts (a shared or inline string of runs in
	// a bold font, a color) are formatted, by cell: each run as <r len="N">,
	// its length in characters, with its properties; nil when no cell has
	// one. Kept apart from Meta, which every cell has.
	Rich map[string]string
}

// Cell is what an XLSX cell has besides the text of its value.
type Cell struct {
	// Type is the type of the value: number, string, bool, error or date.
	// A shared, inline and formula string are all string: how a writer
	// stores a text is not its type.
	Type string
	// Format is the number format: its format code, "builtin N" for a
	// built-in format the workbook does not write out, "" for General.
	Format string
	// Formula is the formula's text without "=" (of a shared formula, the
	// text of the cell that defines it); "" for a plain value.
	Formula string
	// Style is the index of the cell's style (font, fill, borders and the
	// number format) in the workbook's styles; "" for the default.
	Style string
}

// PathName writes the worksheet in a drift or rule path (xlsx:<PathName>!B2).
// A name of letters, digits, _ and . stands as it is; any other is quoted as
// in a spreadsheet formula, with a quote inside doubled:
//
//	Users   Članovi   'Q1 2026!'   'O''Brien'   'sheet1'   [sheet1]
//
// A worksheet the workbook does not name is its part name in brackets,
// [sheet1], which no workbook name can be ([ and ] are not allowed in one).
// A name written like a part name (sheet1) is quoted too: that bare form is
// what DriftGate wrote before it read the workbook's names, and is never
// written now.
func (s Sheet) PathName() string {
	if !s.Named {
		return "[" + s.Name + "]"
	}
	return QuoteName(s.Name)
}

var partLike = lazy.Regexp(`^sheet\d+$`)

// QuoteName writes a workbook's name of a worksheet in a path (PathName).
func QuoteName(name string) string {
	bare := name != "" && !partLike().MatchString(name)
	for _, r := range name {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.') {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// IsPartLike reports whether a bare name in a path is in the form of a part
// name (sheet1), which paths no longer use (PathName).
func IsPartLike(name string) bool { return partLike().MatchString(name) }

// Refs lists the references of the cells of both sheets, with a value or
// only a formula or a style (Meta), row by row and column by column.
func Refs(a, b Sheet) []string {
	type cell struct {
		ref      string
		row, col int
	}
	list := make([]cell, 0, max(len(a.Cells), len(a.Meta)))
	add := func(ref string) {
		r, c := split(ref)
		list = append(list, cell{ref, r, c})
	}
	if a.Meta != nil {
		for ref := range a.Meta {
			add(ref)
		}
	} else {
		for ref := range a.Cells {
			add(ref)
		}
	}
	inA := func(ref string) bool {
		if a.Meta != nil {
			_, ok := a.Meta[ref]
			return ok
		}
		_, ok := a.Cells[ref]
		return ok
	}
	if b.Meta != nil {
		for ref := range b.Meta {
			if !inA(ref) {
				add(ref)
			}
		}
	} else {
		for ref := range b.Cells {
			if !inA(ref) {
				add(ref)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].row != list[j].row {
			return list[i].row < list[j].row
		}
		return list[i].col < list[j].col
	})
	out := make([]string, len(list))
	for i, c := range list {
		out[i] = c.ref
	}
	return out
}

// split returns the row and the column (both from 1) of a reference; 0, 0
// when it is none (letters A-Z, then digits).
func split(ref string) (row, col int) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		col = col*26 + int(ref[i]-'A'+1)
		i++
	}
	if i == 0 || i == len(ref) {
		return 0, 0
	}
	for _, d := range ref[i:] {
		if d < '0' || d > '9' {
			return 0, 0
		}
		row = row*10 + int(d-'0')
	}
	return row, col
}

// Ref writes the reference of a cell; row and column count from 0.
func Ref(row, col int) string {
	name := ""
	for col++; col > 0; col = (col - 1) / 26 {
		name = string(rune('A'+(col-1)%26)) + name
	}
	return name + strconv.Itoa(row+1)
}

// ReadCSV reads a CSV file (a UTF-8 byte order mark is skipped). Empty
// cells are left out, as in a spreadsheet.
func ReadCSV(data []byte) (Sheet, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	rows, err := r.ReadAll()
	if err != nil {
		return Sheet{}, err
	}
	s := Sheet{Name: "csv", Named: true, Cells: map[string]string{}}
	for i, row := range rows {
		for j, v := range row {
			if v != "" {
				s.Cells[Ref(i, j)] = v
			}
		}
	}
	return s, nil
}

// Rows returns the cells of a sheet as rows, empty cells as "".
func (s Sheet) Rows() [][]string {
	var out [][]string
	for ref, v := range s.Cells {
		r, c := split(ref)
		if r == 0 {
			continue
		}
		for len(out) < r {
			out = append(out, nil)
		}
		for len(out[r-1]) < c {
			out[r-1] = append(out[r-1], "")
		}
		out[r-1][c-1] = v
	}
	return out
}

// IsXLSX reports whether data is a ZIP archive with a workbook in it.
func IsXLSX(data []byte) bool {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return false
	}
	for _, f := range zr.File {
		if f.Name == "xl/workbook.xml" {
			return true
		}
	}
	return false
}

// maxPart bounds one decompressed part of a workbook.
const maxPart = 64 << 20

var sheetPart = lazy.Regexp(`^xl/worksheets/(sheet(\d+))\.xml$`)

// worksheet is a worksheet to read: its name and its part.
type worksheet struct {
	name  string
	named bool
	part  *zip.File
}

// Workbook is a read XLSX file: its worksheets, and the rest of it.
type Workbook struct {
	Sheets []Sheet
	// Rest is the workbook apart from its cells, by part name in order:
	// every part but the document properties (docProps/: times, author,
	// application), the shared strings and the calculation chain (both
	// follow from the cells), with the worksheets written without their
	// cells (withoutValues). Two workbooks with the same cells and the same
	// Rest look the same in a spreadsheet application.
	Rest []Part
}

// Part is a part of a workbook: its name and content.
type Part struct {
	Name string
	Data []byte
}

// ReadXLSX reads the cells of every worksheet (ReadWorkbook).
func ReadXLSX(data []byte) ([]Sheet, error) {
	wb, err := ReadWorkbook(data)
	if err != nil {
		return nil, err
	}
	return wb.Sheets, nil
}

// ReadWorkbook reads the cells of every worksheet, in the order of the
// workbook's tabs and under the names the workbook gives them. When the
// workbook does not say which part holds which worksheet (xl/workbook.xml or
// its relationships are missing, cannot be read, or point at a part that is
// not there), every worksheet part is read in the order of its number and
// named after it (sheet1, Named false): one rule for the whole workbook, so
// a damaged workbook never mixes the two. Shared strings and inline strings
// are resolved; a number, boolean or formula result is the text stored in
// the cell. A row or cell without its optional reference (r) is where
// ECMA-376 puts it: the row after the previous row, the column after the
// previous cell of the row.
func ReadWorkbook(data []byte) (*Workbook, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	parts := map[string]*zip.File{}
	for _, f := range zr.File {
		parts[f.Name] = f
	}
	list, err := named(parts)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = byPart(zr.File)
	}
	if len(list) == 0 {
		return nil, errors.New("no worksheet in the workbook")
	}
	var shared []textItem
	if f := parts["xl/sharedStrings.xml"]; f != nil {
		raw, err := readPart(f)
		if err != nil {
			return nil, err
		}
		var sst struct {
			Items []textItem `xml:"si"`
		}
		if err := xml.Unmarshal(raw, &sst); err != nil {
			return nil, fmt.Errorf("sharedStrings.xml: %w", err)
		}
		shared = sst.Items
	}
	formats, err := numberFormats(parts["xl/styles.xml"])
	if err != nil {
		return nil, err
	}
	wb := &Workbook{}
	for _, w := range list {
		raw, err := readPart(w.part)
		if err != nil {
			return nil, err
		}
		s, err := readSheet(w, raw, shared, formats)
		if err != nil {
			return nil, err
		}
		wb.Sheets = append(wb.Sheets, s)
	}
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "docProps/") || f.Name == "xl/sharedStrings.xml" || f.Name == "xl/calcChain.xml" || strings.HasSuffix(f.Name, "/") {
			continue
		}
		raw, err := readPart(f)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml") {
			if raw, err = withoutValues(raw); err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
		}
		wb.Rest = append(wb.Rest, Part{Name: f.Name, Data: raw})
	}
	sort.Slice(wb.Rest, func(i, j int) bool { return wb.Rest[i].Name < wb.Rest[j].Name })
	return wb, nil
}

// readSheet reads the cells of one worksheet.
func readSheet(w worksheet, raw []byte, shared []textItem, formats []string) (Sheet, error) {
	var ws struct {
		Rows []struct {
			Ref   string `xml:"r,attr"`
			Cells []struct {
				Ref     string    `xml:"r,attr"`
				Type    string    `xml:"t,attr"`
				Style   string    `xml:"s,attr"`
				Value   *string   `xml:"v"`
				Inline  *textItem `xml:"is"`
				Formula *struct {
					Text   string `xml:",chardata"`
					Type   string `xml:"t,attr"`
					Shared string `xml:"si,attr"`
				} `xml:"f"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(raw, &ws); err != nil {
		return Sheet{}, fmt.Errorf("%s: %w", w.part.Name, err)
	}
	s := Sheet{Name: w.name, Named: w.named, Cells: map[string]string{}, Meta: map[string]Cell{}}
	sharedFormulas := map[string]string{} // si → the text of the cell that defines it
	row := 0
	for _, r := range ws.Rows {
		row++
		if r.Ref != "" {
			n, err := strconv.Atoi(r.Ref)
			if err != nil || n < 1 {
				return Sheet{}, fmt.Errorf("%s: row %q is no row number", s.PathName(), r.Ref)
			}
			row = n
		}
		col := 0
		for _, c := range r.Cells {
			col++
			ref := c.Ref
			if ref == "" {
				ref = Ref(row-1, col-1)
			} else if _, col = split(ref); col == 0 {
				return Sheet{}, fmt.Errorf("%s: %q is no cell reference", s.PathName(), ref)
			}
			v, rich := "", ""
			switch {
			case c.Inline != nil:
				v, rich = c.Inline.text(), c.Inline.rich()
			case c.Value != nil && c.Type == "s":
				i, err := strconv.Atoi(strings.TrimSpace(*c.Value))
				if err != nil || i < 0 || i >= len(shared) {
					return Sheet{}, fmt.Errorf("%s!%s: no shared string %q", s.PathName(), ref, *c.Value)
				}
				v, rich = shared[i].text(), shared[i].rich()
			case c.Value != nil:
				v = *c.Value
			}
			meta := Cell{Type: cellType(c.Type), Style: c.Style}
			if meta.Style == "0" {
				meta.Style = ""
			}
			if i, err := strconv.Atoi(c.Style); err == nil && i >= 0 && i < len(formats) {
				meta.Format = formats[i]
			}
			if f := c.Formula; f != nil {
				meta.Formula = f.Text
				if f.Type == "shared" {
					if f.Text != "" {
						sharedFormulas[f.Shared] = f.Text
					} else {
						meta.Formula = sharedFormulas[f.Shared]
					}
				}
			}
			// An empty cell still shows its style, and its formula is
			// still the workbook's (a sum that came out empty).
			if v != "" {
				s.Cells[ref] = v
				s.Meta[ref] = meta
				if rich != "" {
					if s.Rich == nil {
						s.Rich = map[string]string{}
					}
					s.Rich[ref] = rich
				}
			} else if meta.Formula != "" || meta.Style != "" {
				s.Meta[ref] = meta
			}
		}
	}
	return s, nil
}

// cellType names the type of a cell's value by its t attribute (ECMA-376
// ST_CellType; a number when there is none).
func cellType(t string) string {
	switch t {
	case "", "n":
		return "number"
	case "s", "inlineStr", "str":
		return "string"
	case "b":
		return "bool"
	case "e":
		return "error"
	case "d":
		return "date"
	}
	return t
}

// numberFormats returns the number format of every cell style (cellXfs in
// xl/styles.xml, by index): the format code the workbook writes, "builtin
// N" for a built-in format it does not, "" for General (0).
func numberFormats(f *zip.File) ([]string, error) {
	if f == nil {
		return nil, nil
	}
	raw, err := readPart(f)
	if err != nil {
		return nil, err
	}
	var st struct {
		Formats []struct {
			ID   string `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		Styles []struct {
			ID string `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
	if xml.Unmarshal(raw, &st) != nil {
		return nil, nil // the styles are compared as a part of the rest
	}
	codes := map[string]string{}
	for _, nf := range st.Formats {
		codes[nf.ID] = nf.Code
	}
	out := make([]string, len(st.Styles))
	for i, x := range st.Styles {
		switch code, ok := codes[x.ID]; {
		case ok:
			out[i] = code
		case x.ID != "" && x.ID != "0":
			out[i] = "builtin " + x.ID
		}
	}
	return out, nil
}

// withoutValues writes a worksheet without its cells, which are compared
// cell by cell (Sheet, Cell): of the rows only their settings (height,
// hidden, outline, style) are kept, by row number, and not the cells they
// span. Whitespace between elements is left out, so a writer that indents
// differently writes the same.
func withoutValues(raw []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	inData, skip, row := false, 0, 0
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case skip > 0:
				skip++
				continue
			case inData && t.Name.Local == "row":
				row++
				var attrs []string
				for _, a := range t.Attr {
					switch {
					case a.Name.Space == "" && a.Name.Local == "r":
						if n, err := strconv.Atoi(a.Value); err == nil {
							row = n
						}
					case a.Name.Space == "" && a.Name.Local != "spans":
						attrs = append(attrs, fmt.Sprintf(" %s=%q", a.Name.Local, a.Value))
					}
				}
				if len(attrs) > 0 {
					fmt.Fprintf(&out, "<row %d%s/>", row, strings.Join(attrs, ""))
				}
				skip = 1
				continue
			}
			out.WriteString("<" + t.Name.Space + " " + t.Name.Local)
			for _, a := range t.Attr {
				fmt.Fprintf(&out, " %s %s=%q", a.Name.Space, a.Name.Local, a.Value)
			}
			out.WriteString(">")
			inData = inData || t.Name.Local == "sheetData"
		case xml.EndElement:
			if skip > 0 {
				skip--
				continue
			}
			inData = inData && t.Name.Local != "sheetData"
			out.WriteString("</" + t.Name.Local + ">")
		case xml.CharData:
			if skip == 0 {
				out.Write(bytes.TrimSpace(t))
			}
		}
	}
}

// byPart lists the worksheet parts in the order of their numbers, named
// after the part.
func byPart(files []*zip.File) []worksheet {
	var list []worksheet
	num := map[*zip.File]int{}
	for _, f := range files {
		if m := sheetPart().FindStringSubmatch(f.Name); m != nil {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue // more digits than an int holds: no worksheet of ours
			}
			num[f] = n
			list = append(list, worksheet{name: m[1], part: f})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return num[list[i].part] < num[list[j].part] })
	return list
}

// named lists the worksheets as the workbook names and orders them, or
// returns nil when the workbook does not say (ReadXLSX). A sheet that is
// not a worksheet (a chart sheet) has no cells and is left out. An error is
// a part that is too large: reading the parts by number would not be safer.
func named(parts map[string]*zip.File) ([]worksheet, error) {
	wbPart, relsPart := parts["xl/workbook.xml"], parts["xl/_rels/workbook.xml.rels"]
	if wbPart == nil || relsPart == nil {
		return nil, nil
	}
	rawWB, err := readPart(wbPart)
	if err != nil {
		return nil, err
	}
	rawRels, err := readPart(relsPart)
	if err != nil {
		return nil, err
	}
	var wb struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			// r:id, in the relationships namespace; encoding/xml matches
			// the namespace URL, so a workbook with another prefix reads too.
			ID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sheets>sheet"`
	}
	var rels struct {
		List []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(rawWB, &wb) != nil || xml.Unmarshal(rawRels, &rels) != nil || len(wb.Sheets) == 0 {
		return nil, nil
	}
	targets := map[string]string{}
	for _, r := range rels.List {
		if r.Mode == "" || r.Mode == "Internal" {
			targets[r.ID] = r.Target
		}
	}
	var list []worksheet
	seen := map[string]bool{}
	for _, s := range wb.Sheets {
		target, ok := targets[s.ID]
		if !ok || s.Name == "" || seen[strings.ToLower(s.Name)] {
			// Excel keeps names unique regardless of case; a workbook that
			// does not cannot be matched by name.
			return nil, nil
		}
		seen[strings.ToLower(s.Name)] = true
		name := resolve(target)
		f := parts[name]
		if f == nil {
			return nil, nil
		}
		if !strings.HasPrefix(name, "xl/worksheets/") {
			continue
		}
		list = append(list, worksheet{name: s.Name, named: true, part: f})
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

// resolve turns the target of a relationship of xl/workbook.xml into a part
// name: relative to xl/, or from the root when it starts with /.
func resolve(target string) string {
	p := "xl/" + target
	if strings.HasPrefix(target, "/") {
		p = target
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// textItem is a string of rich text runs (<r>) or plain text (<t>).
type textItem struct {
	T    string `xml:"t"`
	Runs []struct {
		T     string `xml:"t"`
		Props *struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"rPr"`
	} `xml:"r"`
}

func (t textItem) text() string {
	if len(t.Runs) == 0 {
		return t.T
	}
	s := t.T
	for _, r := range t.Runs {
		s += r.T
	}
	return s
}

// betweenTags is whitespace between two tags, which a writer may indent.
var betweenTags = lazy.Regexp(`>\s+<`)

// rich writes how the runs of a rich text are formatted (Sheet.Rich): ""
// for plain text, also for runs without properties.
func (t textItem) rich() string {
	formatted := false
	for _, r := range t.Runs {
		formatted = formatted || r.Props != nil && len(bytes.TrimSpace(r.Props.Inner)) > 0
	}
	if !formatted {
		return ""
	}
	var b strings.Builder
	if t.T != "" {
		fmt.Fprintf(&b, `<r len="%d"></r>`, utf8.RuneCountInString(t.T))
	}
	for _, r := range t.Runs {
		fmt.Fprintf(&b, `<r len="%d">`, utf8.RuneCountInString(r.T))
		if r.Props != nil {
			b.Write(betweenTags().ReplaceAll(bytes.TrimSpace(r.Props.Inner), []byte("><")))
		}
		b.WriteString("</r>")
	}
	return b.String()
}

func readPart(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, maxPart+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	if len(raw) > maxPart {
		return nil, fmt.Errorf("%s: larger than %d bytes", f.Name, maxPart)
	}
	return raw, nil
}

// WriteXLSX writes a workbook with one worksheet, Sheet1, holding rows as
// text cells. The output is the same bytes for the same rows: stored
// entries without times.
func WriteXLSX(rows [][]string) []byte {
	return WriteWorkbook(Rows{Name: "Sheet1", Rows: rows})
}

// Rows is a worksheet to write: its name in the workbook and its rows.
type Rows struct {
	Name string
	Rows [][]string
}

// WriteWorkbook writes a workbook with a worksheet for each of sheets, in
// that order, holding the rows as text cells; the n-th one is the part
// xl/worksheets/sheet<n>.xml. Like WriteXLSX, the same sheets give the same
// bytes.
func WriteWorkbook(sheets ...Rows) []byte {
	var types, names, rels strings.Builder
	var parts [][2]string
	for k, s := range sheets {
		n := strconv.Itoa(k + 1)
		var esc bytes.Buffer
		xml.EscapeText(&esc, []byte(s.Name))
		fmt.Fprintf(&types, `<Override PartName="/xl/worksheets/sheet%s.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, n)
		fmt.Fprintf(&names, `<sheet name="%s" sheetId="%s" r:id="rId%s"/>`, esc.String(), n, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%s" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%s.xml"/>`, n, n)
		parts = append(parts, [2]string{"xl/worksheets/sheet" + n + ".xml", worksheetXML(s.Rows)})
	}
	parts = append([][2]string{
		{"[Content_Types].xml", xml.Header + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			types.String() +
			`</Types>`},
		{"_rels/.rels", xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
			`</Relationships>`},
		{"xl/workbook.xml", xml.Header + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets>` + names.String() + `</sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", xml.Header + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			rels.String() +
			`</Relationships>`},
	}, parts...)
	return zipParts(parts)
}

func worksheetXML(rows [][]string) string {
	var sheet strings.Builder
	sheet.WriteString(xml.Header + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, i+1)
		for j, v := range row {
			if v == "" {
				continue
			}
			var esc bytes.Buffer
			xml.EscapeText(&esc, []byte(v))
			fmt.Fprintf(&sheet, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, Ref(i, j), esc.String())
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	return sheet.String()
}

// zipParts writes the parts (name, content) as stored entries without times.
func zipParts(parts [][2]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		n := uint64(len(p[1]))
		f, err := zw.CreateRaw(&zip.FileHeader{Name: p[0], Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte(p[1])), CompressedSize64: n, UncompressedSize64: n})
		if err == nil {
			_, err = io.WriteString(f, p[1])
		}
		if err != nil {
			panic(err) // writing to memory does not fail
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}
