package files

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

// The PDF reader knows only enough of the format to find the page tree:
// objects (also inside object streams), dictionaries, arrays, names, numbers
// and references. Strings are skipped without keeping their content, and no
// stream is decoded except object streams. Encrypted object streams cannot
// be read, so such a file falls back to a single page.

type (
	pdfName string
	pdfRef  struct{ num, gen int }
	pdfDict map[pdfName]any // values: float64, bool, pdfName, pdfRef, []any, pdfDict or nil
)

// pdfPage is the geometry of one page.
type pdfPage struct {
	media, crop []float64
	rotate      int
}

var letter = []float64{0, 0, 612, 792}

const maxPages = 100000

// Limits on the work for one file, so that a small crafted file cannot cost
// more than a few passes over its bytes, however it is broken.
const (
	maxObjNum     = 1e10      // object and generation numbers have at most 10 digits
	maxInflate    = 256 << 20 // bytes all object streams of a file inflate to
	maxObjStm     = 64 << 20  // bytes one object stream inflates to
	scanPerByte   = 8         // bytes the parsers may go over per byte of the file or of an object stream
	scanAllowance = 1 << 20   // and in addition
)

// pdfWork is the work left for one file, shared by the parsers of the file
// and of its object streams.
type pdfWork struct {
	ctx     context.Context
	scan    int // bytes the parsers may still go over
	inflate int // bytes object streams may still inflate to
	err     error
}

var errPDFWork = errors.New("too much work to read")

// spend takes n bytes of scanning from the budget; false when the budget is
// spent or the context is done.
func (w *pdfWork) spend(n int) bool {
	if w.err != nil {
		return false
	}
	if w.scan -= n; w.scan < 0 {
		w.err = errPDFWork
	} else {
		w.err = w.ctx.Err()
	}
	return w.err == nil
}

func pdfPlan(ctx context.Context, r io.ReaderAt, size int64, info *Info) (plan, error) {
	data := make([]byte, size)
	if _, err := r.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	pages, err := pdfPageTree(ctx, data)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil || len(pages) == 0 {
		if err == nil {
			err = errors.New("no pages")
		}
		info.Fallback = fmt.Sprintf("pdf page tree not readable: %v", err)
		pages = []pdfPage{{media: letter}}
	}
	info.Pages = len(pages)
	return func(_ context.Context, w io.Writer) error {
		_, err := w.Write(writePDF(pages))
		return err
	}, nil
}

// pdfPages reads the page tree of a file, without a context.
func pdfPages(data []byte) ([]pdfPage, error) {
	return pdfPageTree(context.Background(), data)
}

// pdfPageTree reads the page tree, from the document catalog; when there is
// no readable catalog, the page objects in file order.
func pdfPageTree(ctx context.Context, data []byte) ([]pdfPage, error) {
	work := &pdfWork{ctx: ctx, scan: scanPerByte*len(data) + scanAllowance, inflate: maxInflate}
	objs, root := pdfObjects(data, work)
	if work.err != nil {
		return nil, work.err
	}
	resolve := func(v any) any {
		for i := 0; i < 32; i++ {
			ref, ok := v.(pdfRef)
			if !ok {
				return v
			}
			v = objs.byNum[ref.num]
		}
		return nil
	}
	if root == nil {
		// No trailer: the last catalog in the file.
		for _, n := range objs.order {
			if d, ok := objs.byNum[n].(pdfDict); ok && d["Type"] == pdfName("Catalog") {
				root = d
			}
		}
	}
	box := func(v any) []float64 {
		a, ok := resolve(v).([]any)
		if !ok || len(a) != 4 {
			return nil
		}
		out := make([]float64, 4)
		for i, x := range a {
			f, ok := resolve(x).(float64)
			if !ok {
				return nil
			}
			out[i] = f
		}
		return out
	}
	var pages []pdfPage
	if catalog, ok := resolve(root).(pdfDict); ok {
		seen := map[int]bool{}
		var walk func(v any, inherited pdfPage, depth int) error
		walk = func(v any, inherited pdfPage, depth int) error {
			if ref, ok := v.(pdfRef); ok {
				if seen[ref.num] {
					return errors.New("page tree has a cycle")
				}
				seen[ref.num] = true
			}
			node, ok := resolve(v).(pdfDict)
			if !ok || depth > 64 {
				return errors.New("bad page tree node")
			}
			p := inherited
			if b := box(node["MediaBox"]); b != nil {
				p.media = b
			}
			if b := box(node["CropBox"]); b != nil {
				p.crop = b
			}
			if rot, ok := resolve(node["Rotate"]).(float64); ok {
				p.rotate = int(math.Mod(rot, 360))
			}
			kids, isNode := resolve(node["Kids"]).([]any)
			if node["Type"] == pdfName("Page") || !isNode {
				if len(pages) >= maxPages {
					return errors.New("too many pages")
				}
				pages = append(pages, p)
				return nil
			}
			for _, k := range kids {
				if err := walk(k, p, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(catalog["Pages"], pdfPage{}, 0); err == nil && len(pages) > 0 {
			return normalize(pages), nil
		}
		pages = nil
	}
	for _, n := range objs.order {
		d, ok := objs.byNum[n].(pdfDict)
		if !ok || d["Type"] != pdfName("Page") {
			continue
		}
		p := pdfPage{media: box(d["MediaBox"]), crop: box(d["CropBox"])}
		if rot, ok := resolve(d["Rotate"]).(float64); ok {
			p.rotate = int(math.Mod(rot, 360))
		}
		pages = append(pages, p)
		if len(pages) > maxPages {
			return nil, errors.New("too many pages")
		}
	}
	return normalize(pages), nil
}

func normalize(pages []pdfPage) []pdfPage {
	for i := range pages {
		p := &pages[i]
		if p.media == nil {
			p.media = letter
		}
		p.rotate = (p.rotate%360 + 360) % 360 / 90 * 90
	}
	return pages
}

// pdfObjs holds the objects of a file; a later definition of a number
// replaces an earlier one, as an incremental update does.
type pdfObjs struct {
	byNum map[int]any
	order []int
}

func (o *pdfObjs) put(num int, v any) {
	if _, ok := o.byNum[num]; !ok {
		o.order = append(o.order, num)
	}
	o.byNum[num] = v
}

// pdfObjects scans the file for "n g obj", parses each object and the
// objects of object streams, and returns them with the root of the last
// trailer (a trailer dictionary or a cross-reference stream). The scan goes
// forward only; a broken object is parsed again from the next "obj", which
// the work budget bounds.
func pdfObjects(data []byte, work *pdfWork) (*pdfObjs, any) {
	objs := &pdfObjs{byNum: map[int]any{}}
	var root any
	marks := &pdfMarks{}
	trailer := -1 // position of the next "trailer" keyword, searched again once passed
	for pos := 0; pos < len(data); {
		if trailer < pos {
			if trailer = bytes.Index(data[pos:], []byte("trailer")); trailer >= 0 {
				trailer += pos
			} else {
				trailer = len(data)
			}
		}
		i := bytes.Index(data[pos:], []byte("obj"))
		if trailer < len(data) && (i < 0 || trailer < pos+i) {
			p := &pdfParser{data: data, pos: trailer + len("trailer"), marks: marks}
			if d, ok := p.value(0).(pdfDict); ok && d["Root"] != nil {
				root = d["Root"]
			}
			if !work.spend(p.pos - trailer + p.searched) {
				break
			}
			pos = max(p.pos, trailer+1)
			continue
		}
		if i < 0 {
			break
		}
		at := pos + i
		pos = at + 3
		num, _, ok := objHeader(data, at)
		if !ok || pos < len(data) && !isDelim(data[pos]) && !isSpace(data[pos]) {
			continue
		}
		p := &pdfParser{data: data, pos: pos, marks: marks}
		v := p.value(0)
		if p.err != nil {
			if !work.spend(p.pos - pos + p.searched) {
				break
			}
			continue
		}
		objs.put(num, v)
		d, isDict := v.(pdfDict)
		var stream []byte
		end := 0
		if isDict {
			stream, end = p.stream(d)
		}
		if !work.spend(p.pos - pos + p.searched) {
			break
		}
		if stream == nil {
			pos = p.pos
			continue
		}
		pos = end
		switch d["Type"] {
		case pdfName("XRef"):
			if d["Root"] != nil {
				root = d["Root"]
			}
		case pdfName("ObjStm"):
			objStream(objs, d, stream, work)
		}
	}
	return objs, root
}

// objHeader reads "num gen " backwards from the "obj" keyword at at.
func objHeader(data []byte, at int) (num, gen int, ok bool) {
	i := at
	digits := func() (int, bool) {
		end := i
		for i > 0 && data[i-1] >= '0' && data[i-1] <= '9' {
			i--
		}
		if i == end || end-i > 10 {
			return 0, false
		}
		n, err := strconv.Atoi(string(data[i:end]))
		return n, err == nil
	}
	spaces := func() bool {
		end := i
		for i > 0 && isSpace(data[i-1]) {
			i--
		}
		return i < end
	}
	if !spaces() {
		return 0, 0, false
	}
	if gen, ok = digits(); !ok || !spaces() {
		return 0, 0, false
	}
	if num, ok = digits(); !ok {
		return 0, 0, false
	}
	return num, gen, i == 0 || isSpace(data[i-1]) || isDelim(data[i-1])
}

// objStream reads the objects of an object stream compressed with Flate.
func objStream(objs *pdfObjs, d pdfDict, stream []byte, work *pdfWork) {
	if d["Filter"] != pdfName("FlateDecode") && !(len(asArray(d["Filter"])) == 1 && asArray(d["Filter"])[0] == pdfName("FlateDecode")) {
		return
	}
	if d["DecodeParms"] != nil {
		return // predictors are not used for object streams in practice
	}
	n, _ := d["N"].(float64)
	first, _ := d["First"].(float64)
	z, err := zlib.NewReader(bytes.NewReader(stream))
	if err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(z, int64(min(maxObjStm, work.inflate))))
	work.inflate -= len(data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return
	}
	work.scan += scanPerByte * len(data)
	// Compared as numbers before any conversion: a number of the file may be
	// out of the range of int.
	if first < 0 || first >= float64(len(data)+1) || n < 0 || n > 1e6 {
		return
	}
	p := &pdfParser{data: data[:int(first)], marks: &pdfMarks{}}
	marks := &pdfMarks{} // of the objects, after the header
	for k := 0; k < int(n); k++ {
		start := p.pos
		num, ok1 := p.value(0).(float64)
		off, ok2 := p.value(0).(float64)
		if !ok1 || !ok2 || p.err != nil || !work.spend(p.pos-start+p.searched) {
			return
		}
		p.searched = 0
		if num < 0 || num >= maxObjNum || off <= -1 || off >= float64(len(data)-int(first)) {
			continue
		}
		q := &pdfParser{data: data, pos: int(first) + int(off), marks: marks}
		v := q.value(0)
		if !work.spend(q.pos - int(first) - int(off) + q.searched) {
			return
		}
		if q.err == nil {
			objs.put(int(num), v)
		}
	}
}

func asArray(v any) []any {
	a, _ := v.([]any)
	return a
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '\f' || c == 0
}

func isDelim(c byte) bool {
	return bytes.IndexByte([]byte("()<>[]{}/%"), c) >= 0
}

// pdfParser parses PDF values; strings come back as nil, their content is
// not kept.
type pdfParser struct {
	data     []byte
	pos      int
	err      error
	marks    *pdfMarks // shared by the parsers of the same data
	searched int       // bytes searched for a terminator, beyond pos
}

// pdfMarks remembers the terminators found in the data, so that a broken
// file does not make the parsers search the same bytes for one again.
type pdfMarks struct {
	gt, endstream pdfMark
}

// pdfMark is a search result: no terminator in [from, at), and the next one
// at at (len(data) when there is none). The zero value knows nothing.
type pdfMark struct {
	from, at int
	known    bool
}

// next returns the position of the first sep at or after start, -1 when
// there is none.
func (p *pdfParser) next(m *pdfMark, start int, sep string) int {
	if !m.known || start < m.from || start > m.at {
		*m = pdfMark{from: start, at: len(p.data), known: true}
		if i := bytes.Index(p.data[start:], []byte(sep)); i >= 0 {
			m.at = start + i
		}
		p.searched += m.at - start
	}
	if m.at == len(p.data) {
		return -1
	}
	return m.at
}

var errPDFSyntax = errors.New("pdf syntax")

func (p *pdfParser) fail() any {
	p.err = errPDFSyntax
	return nil
}

func (p *pdfParser) skipSpace() {
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		switch {
		case isSpace(c):
			p.pos++
		case c == '%':
			for p.pos < len(p.data) && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *pdfParser) value(depth int) any {
	if p.err != nil || depth > 64 {
		return p.fail()
	}
	p.skipSpace()
	if p.pos >= len(p.data) {
		return p.fail()
	}
	c := p.data[p.pos]
	switch {
	case c == '<' && p.pos+1 < len(p.data) && p.data[p.pos+1] == '<':
		p.pos += 2
		d := pdfDict{}
		for {
			p.skipSpace()
			if p.pos+1 < len(p.data) && p.data[p.pos] == '>' && p.data[p.pos+1] == '>' {
				p.pos += 2
				return d
			}
			k, ok := p.value(depth + 1).(pdfName)
			if !ok {
				return p.fail()
			}
			v := p.value(depth + 1)
			if p.err != nil {
				return nil
			}
			d[k] = v
		}
	case c == '<': // hex string
		end := p.next(&p.marks.gt, p.pos, ">")
		if end < 0 {
			return p.fail()
		}
		p.pos = end + 1
		return nil
	case c == '(':
		p.pos++
		for nest := 1; nest > 0; p.pos++ {
			if p.pos >= len(p.data) {
				return p.fail()
			}
			switch p.data[p.pos] {
			case '\\':
				p.pos++
			case '(':
				nest++
			case ')':
				nest--
			}
		}
		return nil
	case c == '[':
		p.pos++
		a := []any{}
		for {
			p.skipSpace()
			if p.pos < len(p.data) && p.data[p.pos] == ']' {
				p.pos++
				return a
			}
			v := p.value(depth + 1)
			if p.err != nil {
				return nil
			}
			a = append(a, v)
		}
	case c == '/':
		start := p.pos + 1
		p.pos++
		for p.pos < len(p.data) && !isSpace(p.data[p.pos]) && !isDelim(p.data[p.pos]) {
			p.pos++
		}
		return pdfName(p.data[start:p.pos])
	case c == '+' || c == '-' || c == '.' || c >= '0' && c <= '9':
		f := p.number()
		if p.err != nil {
			return nil
		}
		// "num gen R" is a reference.
		if f >= 0 && f < maxObjNum && f == math.Trunc(f) {
			save := p.pos
			p.skipSpace()
			if p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
				g := p.number()
				p.skipSpace()
				if p.err == nil && g < maxObjNum && p.pos < len(p.data) && p.data[p.pos] == 'R' &&
					(p.pos+1 == len(p.data) || isSpace(p.data[p.pos+1]) || isDelim(p.data[p.pos+1])) {
					p.pos++
					return pdfRef{int(f), int(g)}
				}
			}
			p.pos, p.err = save, nil
		}
		return f
	default:
		start := p.pos
		for p.pos < len(p.data) && !isSpace(p.data[p.pos]) && !isDelim(p.data[p.pos]) {
			p.pos++
		}
		switch string(p.data[start:p.pos]) {
		case "true":
			return true
		case "false":
			return false
		case "null":
			return nil
		}
		return p.fail()
	}
}

func (p *pdfParser) number() float64 {
	start := p.pos
	for p.pos < len(p.data) && bytes.IndexByte([]byte("+-.0123456789"), p.data[p.pos]) >= 0 {
		p.pos++
	}
	f, err := strconv.ParseFloat(string(p.data[start:p.pos]), 64)
	if err != nil || math.IsInf(f, 0) {
		p.fail()
		return 0
	}
	return f
}

// stream returns the data of the stream that follows a dictionary, and the
// position after "endstream"; nil when no stream follows.
func (p *pdfParser) stream(d pdfDict) ([]byte, int) {
	p.skipSpace()
	if !bytes.HasPrefix(p.data[p.pos:], []byte("stream")) {
		return nil, 0
	}
	start := p.pos + len("stream")
	if bytes.HasPrefix(p.data[start:], []byte("\r\n")) {
		start += 2
	} else if start < len(p.data) && (p.data[start] == '\n' || p.data[start] == '\r') {
		start++
	}
	// Compared as numbers before the conversion: the length may be out of the
	// range of int.
	if n, ok := d["Length"].(float64); ok && n >= 0 && n < float64(len(p.data)-start+1) {
		end := start + int(n)
		rest := bytes.TrimLeft(p.data[end:min(end+16, len(p.data))], " \r\n\t")
		if bytes.HasPrefix(rest, []byte("endstream")) {
			return p.data[start:end], end + bytes.Index(p.data[end:], []byte("endstream")) + len("endstream")
		}
	}
	// An indirect or wrong length: up to "endstream".
	at := p.next(&p.marks.endstream, start, "endstream")
	if at < 0 {
		return nil, 0
	}
	end := at
	for end > start && (p.data[end-1] == '\n' || p.data[end-1] == '\r') {
		end--
	}
	return p.data[start:end], at + len("endstream")
}

// writePDF writes a PDF of blank pages with the given geometry, without an
// information dictionary or document ID.
func writePDF(pages []pdfPage) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	offsets := []int{}
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	num := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	arr := func(a []float64) string {
		return "[" + num(a[0]) + " " + num(a[1]) + " " + num(a[2]) + " " + num(a[3]) + "]"
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	var kids bytes.Buffer
	for i := range pages {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", i+4)
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), len(pages)))
	// One empty content stream for every page: optional in the format, but
	// some readers want it.
	obj("<< /Length 0 >>\nstream\n\nendstream")
	for _, p := range pages {
		s := "<< /Type /Page /Parent 2 0 R /MediaBox " + arr(p.media)
		if p.crop != nil {
			s += " /CropBox " + arr(p.crop)
		}
		if p.rotate != 0 {
			s += " /Rotate " + strconv.Itoa(p.rotate)
		}
		obj(s + " /Resources << >> /Contents 3 0 R >>")
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
