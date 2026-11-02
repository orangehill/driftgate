package compare

import (
	"bytes"
	"fmt"
	"maps"
	"mime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/sheet"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// Drift kinds of command steps, repeated requests and spreadsheet bodies.
const (
	KindExit   = "exit-code" // a command step ended with another exit code
	KindRepeat = "repeat"    // the statuses of a request sent several times
	KindCell   = "cell"      // a cell of a CSV or XLSX body
	// The number format and the formula of an XLSX cell; its type is a
	// drift of KindType.
	KindCellFormat  = "cell-format"
	KindCellFormula = "cell-formula"

	// A worksheet of an XLSX body, matched by the name the workbook gives
	// it: missing now, new, or the worksheets both have in another order.
	KindSheetMissing = "sheet-missing"
	KindSheetExtra   = "sheet-extra"
	KindSheetOrder   = "sheet-order"
)

var cellRow = lazy.Regexp(`!([A-Z]+)\d+$`)

// steps compares what only command steps and repeated requests have.
func (c *comparer) steps(base, cur *snapshot.Record) {
	if b, v := exitText(base), exitText(cur); b != v {
		c.add(KindExit, "exit", b, v)
	}
	if base.Repeat != cur.Repeat {
		c.add(KindRepeat, "repeat", base.Repeat, cur.Repeat)
	}
}

func exitText(r *snapshot.Record) string {
	if r.Exit == nil {
		return ""
	}
	return strconv.Itoa(*r.Exit)
}

func mediaType(r *snapshot.Record) string {
	for _, h := range r.Headers {
		if strings.EqualFold(h[0], "content-type") {
			mt, _, _ := mime.ParseMediaType(h[1])
			return mt
		}
	}
	return ""
}

// sheetKind tells a spreadsheet body: xlsx by its content (or its media
// type), csv by its media type only, since any text may parse as CSV.
func sheetKind(s Side) string {
	switch mt := mediaType(s.Record); {
	case strings.Contains(mt, "spreadsheetml") || sheet.IsXLSX(s.Body):
		return "xlsx"
	case mt == "text/csv" || mt == "application/csv":
		return "csv"
	}
	return ""
}

// sheets compares two spreadsheets (an export) cell by cell, with the place
// of each difference: xlsx:Users!B3 or csv!B3 (sheet.PathName). The
// worksheets of two workbooks are matched by name: a renamed one is missing
// under its old name and new under the new one, and worksheets in another
// order are one drift. An XLSX cell's type is compared at LevelTypes, its
// value, number format, style, the formatting of a rich text and formula
// at LevelValues, also of an empty cell that has a style or a formula. Cell rules of the
// allowlist (paths xlsx:…!… and csv!…) apply to values. It returns false
// when the bodies are not both spreadsheets of one kind, or one cannot be
// read; they are then compared as bytes.
func (c *comparer) sheets(base, cur Side) bool {
	kind := sheetKind(base)
	if kind == "" || sheetKind(cur) != kind {
		return false
	}
	var bs, cs []sheet.Sheet
	var bw, cw *sheet.Workbook
	if kind == "csv" {
		b, err1 := sheet.ReadCSV(base.Body)
		v, err2 := sheet.ReadCSV(cur.Body)
		if err1 != nil || err2 != nil {
			return false
		}
		bs, cs = []sheet.Sheet{b}, []sheet.Sheet{v}
	} else {
		var err1, err2 error
		bw, err1 = sheet.ReadWorkbook(base.Body)
		cw, err2 = sheet.ReadWorkbook(cur.Body)
		if err1 != nil || err2 != nil {
			return false
		}
		bs, cs = bw.Sheets, cw.Sheets
		if c.origins != nil {
			c.origins.base.cells(bs)
			c.origins.cur.cells(cs)
		}
	}
	before := len(c.drifts)
	prefix := func(s sheet.Sheet) string {
		if kind == "csv" {
			return "csv"
		}
		return "xlsx:" + s.PathName()
	}
	rules := c.level >= LevelValues && c.allow.HasCellRules()
	allowed := 0
	pairs, missing, extra := pairSheets(bs, cs)
	for _, i := range missing {
		c.add(KindSheetMissing, prefix(bs[i]), cellCount(bs[i]), renamed(bs[i], cs, extra, prefix))
	}
	for _, j := range extra {
		c.add(KindSheetExtra, prefix(cs[j]), "", cellCount(cs[j]))
	}
	if !inOrder(pairs) {
		c.add(KindSheetOrder, "xlsx", listText(sheetNames(bs, pairs, 0)), listText(sheetNames(cs, pairs, 1)))
	}
	for _, p := range pairs {
		b, v := bs[p[0]], cs[p[1]]
		for _, ref := range sheet.Refs(b, v) {
			where := prefix(b) + "!" + ref
			bv, inB := b.Cells[ref]
			cv, inC := v.Cells[ref]
			bm, hasB := b.Meta[ref]
			cm, hasC := v.Meta[ref]
			if inB && inC && bm.Type != cm.Type {
				// Number 1 and text "1": the type changed, whatever the text.
				if c.level >= LevelTypes {
					c.add(KindType, where, bm.Type, cm.Type)
				}
				continue
			}
			if c.level < LevelValues {
				if inB != inC {
					c.add(KindCell, where, bv, cv)
				}
				continue
			}
			// What a cell has besides its value is compared when both sides
			// have the cell, or when neither has a value: an empty cell with a
			// style or a formula against one without, or none (the
			// defaults). A value only one side has is reported as the value.
			if hasB && hasC || bv == cv {
				switch {
				case bm.Format != cm.Format:
					c.add(KindCellFormat, where, jsonx.Quote(bm.Format), jsonx.Quote(cm.Format))
				case bm.Style != cm.Style:
					// Another font, fill or border: the style the cell points at.
					c.add(KindCellFormat, where, styleText(bm.Style), styleText(cm.Style))
				case b.Rich[ref] != v.Rich[ref]:
					// Runs of a text in another font or color.
					c.add(KindCellFormat, where, richText(b.Rich[ref]), richText(v.Rich[ref]))
				}
				if bm.Formula != cm.Formula {
					c.add(KindCellFormula, where, jsonx.Quote(bm.Formula), jsonx.Quote(cm.Formula))
				}
			}
			if bv == cv {
				continue
			}
			if rules { // a rule judges only a changed value
				whole, parts := c.allow.CellRules(c.id, where)
				if whole != nil && !whole.Matches(cv) {
					// An empty cell is the value "": the format must allow it.
					c.add(KindFormat, where, bv, cv)
					c.drifts[len(c.drifts)-1].Texts = [2]string{bv, cv}
					continue
				}
				if whole != nil || allowlist.SameParts(parts, bv, cv) {
					allowed++
					continue
				}
			}
			c.add(KindCell, where, bv, cv)
			c.drifts[len(c.drifts)-1].Texts = [2]string{bv, cv}
		}
	}
	if c.level < LevelValues {
		return true
	}
	if kind == "xlsx" {
		// The workbook apart from its cells: styles, column widths, merged
		// cells, the worksheets' settings. A cell a rule let change is no
		// part of it, so this holds also then. A worksheet missing, new or
		// moved changes the workbook's list of them, already reported.
		moved := len(missing) > 0 || len(extra) > 0 || !inOrder(pairs)
		if b, v, ok := c.rest(bw.Rest, cw.Rest); !ok && !moved {
			c.add(KindBody, "xlsx", b, v)
		}
		return true
	}
	// Same cells, other bytes in a CSV: quoting, line endings, a byte order
	// mark. A cell a rule let change makes other bytes by itself; then the
	// line endings and the byte order mark are still compared.
	switch {
	case len(c.drifts) > before:
	case allowed == 0:
		c.add(KindBody, kind, fmt.Sprintf("%d bytes", len(base.Body)), fmt.Sprintf("%d bytes", len(cur.Body)))
	case csvStyle(base.Body) != csvStyle(cur.Body):
		c.add(KindBody, kind, csvStyle(base.Body).String(), csvStyle(cur.Body).String())
	}
	return true
}

// rest compares two workbooks apart from their cells (sheet.Workbook.Rest),
// with the origins replaced; when they differ it names the first part that
// does, on each side.
func (c *comparer) rest(b, v []sheet.Part) (string, string, bool) {
	at := map[string][]byte{}
	for _, p := range v {
		at[p.Name] = p.Data
	}
	describe := func(p sheet.Part) string { return fmt.Sprintf("%s, %d bytes", p.Name, len(p.Data)) }
	for _, p := range b {
		d, ok := at[p.Name]
		if !ok {
			return describe(p), "no " + p.Name, false
		}
		x, y := p.Data, d
		if c.origins != nil {
			x, y = c.origins.base.replace(x), c.origins.cur.replace(y)
		}
		if !bytes.Equal(x, y) {
			return describe(p), describe(sheet.Part{Name: p.Name, Data: d}), false
		}
		delete(at, p.Name)
	}
	for _, p := range v {
		if _, ok := at[p.Name]; ok {
			return "no " + p.Name, describe(p), false
		}
	}
	return "", "", true
}

func richText(s string) string {
	if s == "" {
		return "plain text"
	}
	return "rich text " + s
}

func styleText(s string) string {
	if s == "" {
		return "style 0"
	}
	return "style " + s
}

// style is how a CSV file is written apart from its cells.
type style struct {
	bom, crlf, lf, final bool // byte order mark, CRLF and LF line ends, a line end at the end
}

func csvStyle(b []byte) style {
	crlf := bytes.Count(b, []byte("\r\n"))
	return style{
		bom:   bytes.HasPrefix(b, []byte("\xef\xbb\xbf")),
		crlf:  crlf > 0,
		lf:    bytes.Count(b, []byte("\n")) > crlf,
		final: bytes.HasSuffix(b, []byte("\n")),
	}
}

func (s style) String() string {
	var out []string
	if s.bom {
		out = append(out, "byte order mark")
	}
	switch {
	case s.crlf && s.lf:
		out = append(out, "CRLF and LF line ends")
	case s.crlf:
		out = append(out, "CRLF line ends")
	case s.lf:
		out = append(out, "LF line ends")
	}
	if !s.final {
		out = append(out, "no line end at the end")
	}
	return strings.Join(out, ", ")
}

// inOrder reports whether the paired worksheets come in the same order in
// both workbooks.
func inOrder(pairs [][2]int) bool {
	for k := 1; k < len(pairs); k++ {
		if pairs[k][1] < pairs[k-1][1] {
			return false
		}
	}
	return true
}

// sheetNames lists the paired worksheets of one side (0 baseline, 1
// current) in that side's order.
func sheetNames(sheets []sheet.Sheet, pairs [][2]int, side int) []string {
	idx := make([]int, len(pairs))
	for k, p := range pairs {
		idx[k] = p[side]
	}
	sort.Ints(idx)
	out := make([]string, len(idx))
	for k, i := range idx {
		out[k] = sheets[i].PathName()
	}
	return out
}

func cellCount(s sheet.Sheet) string {
	if len(s.Cells) == 1 {
		return "1 cell"
	}
	return strconv.Itoa(len(s.Cells)) + " cells"
}

// renamed tells, for a worksheet missing now, the new worksheet that holds
// exactly its cells: most likely the same worksheet under another name.
func renamed(s sheet.Sheet, cs []sheet.Sheet, extra []int, prefix func(sheet.Sheet) string) string {
	for _, j := range extra {
		if len(cs[j].Cells) == len(s.Cells) && maps.Equal(cs[j].Cells, s.Cells) {
			return "the same cells are in " + prefix(cs[j])
		}
	}
	return ""
}

// pairSheets matches the worksheets of two workbooks by name (part names
// with part names, workbook names with workbook names): the pairs (baseline
// index, current index) in the baseline's order, and the indexes of the
// worksheets only the baseline or only the current workbook has.
func pairSheets(bs, cs []sheet.Sheet) (pairs [][2]int, missing, extra []int) {
	at := map[string]int{}
	for j, s := range cs {
		at[s.PathName()] = j
	}
	used := map[int]bool{}
	for i, s := range bs {
		if j, ok := at[s.PathName()]; ok && !used[j] {
			used[j] = true
			pairs = append(pairs, [2]int{i, j})
			continue
		}
		missing = append(missing, i)
	}
	for j := range cs {
		if !used[j] {
			extra = append(extra, j)
		}
	}
	return pairs, missing, extra
}

// lines compares text that is not JSON (the output of a command step, a
// text/plain body) line by line and reports the first line that differs;
// the lines after an inserted one would all differ and say nothing new.
func (c *comparer) lines(base, cur Side) bool {
	text := func(s Side) bool {
		return (s.Record.Exit != nil || mediaType(s.Record) == "text/plain") && utf8.Valid(s.Body)
	}
	if !text(base) || !text(cur) {
		return false
	}
	bl := strings.Split(string(base.Body), "\n")
	cl := strings.Split(string(cur.Body), "\n")
	for i := 0; i < len(bl) || i < len(cl); i++ {
		var b, v string
		if i < len(bl) {
			b = bl[i]
		}
		if i < len(cl) {
			v = cl[i]
		}
		if i >= len(bl) || i >= len(cl) || b != v {
			c.add(KindBody, "line "+strconv.Itoa(i+1), b, v)
			return true
		}
	}
	return true // equal lines are equal bodies, which body() has ruled out
}
