package compare

import (
	"sort"
	"strconv"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/sheet"
)

// PlaceKind is what a rule of the allowlist can reach in a body.
type PlaceKind int

const (
	PlaceValue PlaceKind = iota // a JSON scalar, a text or an attribute of a page
	PlaceArray                  // a JSON array (any_length)
	PlaceCell                   // a cell of a spreadsheet
	PlaceBody                   // the whole body of a response that is not JSON (allowlist.RawPath)
)

// Places names every place of a body that a rule of the allowlist can
// reach, with the path a drift there would have (allowlist check uses it to
// find rules that reach nothing). The body is read as body() reads it: an
// HTML page, else a spreadsheet, else JSON; a body that is not JSON is also
// the place allowlist.RawPath. The items of an array that an array rule of
// allow pairs by key for the request id are named by their key. fn returns
// false to stop.
func Places(s Side, allow *allowlist.List, id string, fn func(kind PlaceKind, path string) bool) {
	p := &placer{fn: fn, allow: allow.ForRequest(id), id: id}
	if !jsonx.Valid(s.Body) && !p.emit(PlaceBody, allowlist.RawPath) {
		return
	}
	if isHTML(s.Record) {
		if pg, err := parsePage(s.Body); err == nil {
			p.element("html", pg.root)
			if len(pg.flight) > 0 {
				p.json(FlightPath+"$", flightValue(pg.flight))
			}
		}
		return
	}
	if p.sheets(s) {
		return
	}
	if v, err := jsonx.Parse(s.Body); err == nil {
		p.json("$", v)
	}
}

type placer struct {
	fn    func(PlaceKind, string) bool
	stop  bool
	allow *allowlist.List
	id    string
}

func (p *placer) emit(kind PlaceKind, path string) bool {
	if !p.stop && !p.fn(kind, path) {
		p.stop = true
	}
	return !p.stop
}

func (p *placer) json(path string, v *jsonx.Value) {
	switch v.Kind {
	case jsonx.Object:
		for _, m := range v.Members {
			if p.stop {
				return
			}
			p.json(jsonx.KeyPath(path, m.Key), m.Value)
		}
	case jsonx.Array:
		p.emit(PlaceArray, path)
		var steps []string
		if r := p.allow.ArrayKey(p.id, path); r != nil {
			_, steps, _ = itemKeys(r, v)
		}
		for i, item := range v.Items {
			if p.stop {
				return
			}
			if steps != nil {
				p.json(path+"["+steps[i]+"]", item)
			} else {
				p.json(path+"["+strconv.Itoa(i)+"]", item)
			}
		}
	default:
		p.emit(PlaceValue, path)
	}
}

// element names the attributes and the text of an element (or the JSON it
// embeds), then its children, as node() compares them.
func (p *placer) element(path string, e *element) {
	names := make([]string, 0, len(e.attrs))
	for k := range e.attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		p.emit(PlaceValue, path+"@"+pathName(k))
	}
	if v := embeddedValue(e); v != nil {
		p.json(path+"$", v)
	} else if len(e.text) > 0 {
		p.emit(PlaceValue, path)
	}
	for i, step := range steps(e.kids) {
		if p.stop {
			return
		}
		p.element(path+">"+step, e.kids[i])
	}
}

// embeddedValue is the JSON a script element holds, as embedded() reads it;
// nil when it is compared as text.
func embeddedValue(e *element) *jsonx.Value {
	kind := embeddedJSON(e)
	if kind == "" {
		return nil
	}
	v, err := jsonx.Parse([]byte(e.joined()))
	if err != nil {
		return nil
	}
	if kind == "devalue" {
		if d, ok := devalue(v); ok {
			return d
		}
	}
	return v
}

// sheets names the cells of a spreadsheet body; false when it is none.
func (p *placer) sheets(s Side) bool {
	var list []sheet.Sheet
	switch sheetKind(s) {
	case "csv":
		sh, err := sheet.ReadCSV(s.Body)
		if err != nil {
			return false
		}
		list = []sheet.Sheet{sh}
	case "xlsx":
		wb, err := sheet.ReadWorkbook(s.Body)
		if err != nil {
			return false
		}
		list = wb.Sheets
	default:
		return false
	}
	for _, sh := range list {
		prefix := "csv!"
		if sheetKind(s) == "xlsx" {
			prefix = "xlsx:" + sh.PathName() + "!"
		}
		refs := make([]string, 0, len(sh.Cells))
		for ref := range sh.Cells {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			if !p.emit(PlaceCell, prefix+ref) {
				return true
			}
		}
	}
	return true
}
