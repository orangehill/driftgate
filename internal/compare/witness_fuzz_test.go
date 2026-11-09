package compare

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

// The property expectations rest on: for a baseline X and two responses
// A and B, what an expectation digests of X→A equals that of X→B exactly
// when the comparison finds no drift between A and B. Equal digests for
// responses the comparison tells apart would let an expectation pass a
// drift it never saw (a false pass); different digests for responses it
// does not tell apart would make an expectation fail on the next capture
// (a false fail). FuzzWitness checks it on generated JSON, HTML, CSV and
// XLSX bodies, at every level, under allowlist rules with broad formats.

// stream reads the fuzzer's bytes as choices; past the end every choice is
// the first.
type stream struct {
	b []byte
	i int
}

func (s *stream) n(max int) int {
	if max <= 1 {
		return 0
	}
	if s.i >= len(s.b) {
		return 0
	}
	v := int(s.b[s.i])
	s.i++
	return v % max
}

func (s *stream) pick(list ...string) string { return list[s.n(len(list))] }

var fuzzKeys = []string{"a", "b", "t", "p", "x", "ts", "a b", "é"}

// genJSON writes a JSON value.
func genJSON(s *stream, depth int) *jsonx.Value {
	k := s.n(8)
	if depth > 2 && k >= 6 {
		k %= 6
	}
	switch k {
	case 0:
		return &jsonx.Value{Kind: jsonx.Null}
	case 1:
		return &jsonx.Value{Kind: jsonx.Bool, Bool: s.n(2) == 1}
	case 2:
		t := s.pick("0", "1", "5", "12", "-3")
		return &jsonx.Value{Kind: jsonx.Int, Text: t}
	case 3:
		t := s.pick("1.5", "1.0", "2e3")
		return &jsonx.Value{Kind: jsonx.Float, Text: t}
	case 4, 5:
		t := s.pick("", "5", "x", "null", "true", "1,\"b\":2", "2026-01-01", "a b", "<b>", "12x", "@O/a", "http://elsewhere.test/a", "é/1", "a\nb", "/a/", "–", "„x“")
		return &jsonx.Value{Kind: jsonx.String, Text: t, Raw: jsonx.Quote(t)}
	case 6:
		v := &jsonx.Value{Kind: jsonx.Object}
		n := s.n(4)
		for range n {
			v.Members = append(v.Members, jsonx.Member{Key: fuzzKeys[s.n(len(fuzzKeys))], Value: genJSON(s, depth+1)})
		}
		return v
	default:
		v := &jsonx.Value{Kind: jsonx.Array}
		n := s.n(4)
		for range n {
			v.Items = append(v.Items, genJSON(s, depth+1))
		}
		return v
	}
}

// mutateJSON changes one place of a copy of v.
func mutateJSON(s *stream, v *jsonx.Value) *jsonx.Value {
	out := v.Clone()
	var places []*jsonx.Value
	var walk func(*jsonx.Value)
	walk = func(x *jsonx.Value) {
		places = append(places, x)
		for _, m := range x.Members {
			walk(m.Value)
		}
		for _, it := range x.Items {
			walk(it)
		}
	}
	walk(out)
	p := places[s.n(len(places))]
	switch s.n(5) {
	case 0: // another value
		*p = *genJSON(s, 1)
	case 1: // a member more, or less
		if p.Kind == jsonx.Object {
			if len(p.Members) > 0 && s.n(2) == 0 {
				i := s.n(len(p.Members))
				p.Members = append(p.Members[:i], p.Members[i+1:]...)
			} else {
				p.Members = append(p.Members, jsonx.Member{Key: fuzzKeys[s.n(len(fuzzKeys))], Value: genJSON(s, 2)})
			}
		}
	case 2: // an item more, or less
		if p.Kind == jsonx.Array {
			if len(p.Items) > 0 && s.n(2) == 0 {
				p.Items = p.Items[:len(p.Items)-1]
			} else {
				p.Items = append(p.Items, genJSON(s, 2))
			}
		}
	case 3: // members in another order
		if p.Kind == jsonx.Object && len(p.Members) > 1 {
			p.Members[0], p.Members[1] = p.Members[1], p.Members[0]
		}
	default: // nothing here: only the encoding may change
	}
	return out
}

// jsonDoc writes a JSON value with an encoding: compact or pretty, keys
// "a" with escapes, a byte order mark, a final line end, strings (keys and
// values, by their number in the document, or all) with escapes or with
// every character escaped, whitespace more between tokens (by the number
// of the place), in UTF-16.
type jsonDoc struct {
	v                         *jsonx.Value
	pretty, escape, bom, line bool
	escaped                   map[int]int // 1 escapes, 2 every character escaped; -1 all strings
	spaced                    map[int]string
	utf16                     int // 1 UTF-16 little endian, 2 big endian
}

// encoder reports a document an encoder writes: the same way everywhere,
// so that only its values and lengths tell it from another one.
func (d jsonDoc) encoder() bool {
	return !d.escape && len(d.spaced) == 0 && (len(d.escaped) == 0 || len(d.escaped) == 1 && d.escaped[-1] != 0)
}

func (d jsonDoc) sameEncoding(o jsonDoc) bool {
	return d.pretty == o.pretty && d.bom == o.bom && d.line == o.line && d.escaped[-1] == o.escaped[-1] && d.utf16 == o.utf16
}

// escapeEvery writes every character of a string as \uXXXX.
func escapeEvery(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		if r >= 0x10000 {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	b.WriteByte('"')
	return b.String()
}

// escapeAll writes a string with every escape it may have: \/, \u000a,
// \uXXXX for every character past ASCII.
func escapeAll(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"' || r == '\\':
			b.WriteString(`\` + string(r))
		case r == '/':
			b.WriteString(`\/`)
		case r < 0x20 || r >= 0x80 && r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r >= 0x10000:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (d jsonDoc) bytes() []byte {
	var b strings.Builder
	if d.bom {
		b.WriteString("\ufeff")
	}
	strs, places := 0, 0
	str := func(text string) string {
		strs++
		way, ok := d.escaped[strs-1]
		if !ok {
			way = d.escaped[-1] // every string, as an encoder escapes
		}
		switch way {
		case 1:
			return escapeAll(text)
		case 2:
			return escapeEvery(text)
		}
		return jsonx.Quote(text)
	}
	place := func() {
		places++
		b.WriteString(d.spaced[places-1])
	}
	var write func(*jsonx.Value, int)
	write = func(v *jsonx.Value, depth int) {
		nl := func(depth int) {
			if d.pretty {
				b.WriteString("\n" + strings.Repeat("  ", depth))
			}
			place()
		}
		switch v.Kind {
		case jsonx.Object:
			b.WriteByte('{')
			for i, m := range v.Members {
				if i > 0 {
					b.WriteByte(',')
				}
				nl(depth + 1)
				key := str(m.Key)
				if d.escape && m.Key == "a" {
					key = `"\u0061"`
				}
				b.WriteString(key + ":")
				if d.pretty {
					b.WriteByte(' ')
				}
				place()
				write(m.Value, depth+1)
			}
			if len(v.Members) > 0 {
				nl(depth)
			}
			b.WriteByte('}')
		case jsonx.Array:
			b.WriteByte('[')
			for i, it := range v.Items {
				if i > 0 {
					b.WriteByte(',')
				}
				nl(depth + 1)
				write(it, depth+1)
			}
			if len(v.Items) > 0 {
				nl(depth)
			}
			b.WriteByte(']')
		case jsonx.String:
			b.WriteString(str(v.Text))
		default:
			b.WriteString(v.ScalarText())
		}
	}
	write(d.v, 0)
	if d.line {
		b.WriteByte('\n')
	}
	if d.utf16 > 0 {
		return []byte(inUTF16(b.String(), d.utf16 == 2))
	}
	return []byte(b.String())
}

func mutateDoc(s *stream, d jsonDoc) jsonDoc {
	d.v = mutateJSON(s, d.v)
	d.escaped, d.spaced = maps.Clone(d.escaped), maps.Clone(d.spaced)
	switch s.n(8) {
	case 0:
		d.pretty = !d.pretty
	case 1:
		d.escape = !d.escape
	case 2:
		d.bom = !d.bom
	case 3:
		d.line = !d.line
	case 4: // escapes in a string, or in all of them, of one way or another
		if d.escaped == nil {
			d.escaped = map[int]int{}
		}
		i := s.n(6) - 1
		d.escaped[i] = s.n(3)
		if d.escaped[i] == 0 {
			delete(d.escaped, i)
		}
	case 6: // another encoding
		d.utf16 = s.n(3)
	case 5: // whitespace more at a place, or less
		if d.spaced == nil {
			d.spaced = map[int]string{}
		}
		i := s.n(6)
		if d.spaced[i] == "" {
			d.spaced[i] = s.pick(" ", "\n", "\t ")
		} else {
			delete(d.spaced, i)
		}
	}
	return d
}

// fuzzNode is an element of a generated page.
type fuzzNode struct {
	tag   string
	attrs [][2]string
	texts []string // before each child, and after the last
	kids  []*fuzzNode
}

func genNode(s *stream, depth int) *fuzzNode {
	n := &fuzzNode{tag: s.pick("div", "p", "b", "span")}
	for range s.n(3) {
		n.attrs = append(n.attrs, [2]string{s.pick("a", "b", "data-x", "q", "id"), s.pick("1", "2", "1@b=2", "", "x y", "12")})
	}
	kids := 0
	if depth < 3 {
		kids = s.n(3)
	}
	for range kids {
		n.kids = append(n.kids, genNode(s, depth+1))
	}
	for range kids + 1 {
		n.texts = append(n.texts, s.pick("", "a", "a ", " a", "x  y", "&lt;b&gt;", "12", " "))
	}
	return n
}

func (n *fuzzNode) clone() *fuzzNode {
	c := *n
	c.attrs = slices.Clone(n.attrs)
	c.texts = slices.Clone(n.texts)
	c.kids = make([]*fuzzNode, len(n.kids))
	for i, k := range n.kids {
		c.kids[i] = k.clone()
	}
	return &c
}

func (n *fuzzNode) html(b *strings.Builder) {
	b.WriteString("<" + n.tag)
	seen := map[string]bool{}
	for _, a := range n.attrs {
		if seen[a[0]] {
			continue
		}
		seen[a[0]] = true
		b.WriteString(" " + a[0] + `="` + a[1] + `"`)
	}
	b.WriteString(">")
	for i, t := range n.texts {
		b.WriteString(t)
		if i < len(n.kids) {
			n.kids[i].html(b)
		}
	}
	b.WriteString("</" + n.tag + ">")
}

type fuzzPage struct {
	root   *fuzzNode
	flight string // the RSC payload pushed by a script; "" for none
}

func (p fuzzPage) bytes() []byte {
	var b strings.Builder
	b.WriteString("<html><body>")
	p.root.html(&b)
	if p.flight != "" {
		b.WriteString(`<script>self.__next_f.push([1,"` + p.flight + `"])</script>`)
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

func mutatePage(s *stream, p fuzzPage) fuzzPage {
	p.root = p.root.clone()
	var all []*fuzzNode
	var walk func(*fuzzNode)
	walk = func(n *fuzzNode) {
		all = append(all, n)
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(p.root)
	n := all[s.n(len(all))]
	switch s.n(6) {
	case 0:
		if len(n.attrs) > 0 {
			n.attrs[s.n(len(n.attrs))][1] = s.pick("1", "2", "1@b=2", "x y", "13")
		} else {
			n.attrs = append(n.attrs, [2]string{"a", "1"})
		}
	case 1:
		n.texts[s.n(len(n.texts))] = s.pick("", "a", "a ", " a", "b", "13", "x  y")
	case 2:
		n.kids = append(n.kids, genNode(s, 2))
		n.texts = append(n.texts, s.pick("", " ", "c"))
	case 3:
		if len(n.kids) > 0 {
			n.kids = n.kids[:len(n.kids)-1]
			n.texts = n.texts[:len(n.texts)-1]
		}
	case 4:
		p.flight = s.pick("", "1:a", "1:b", "2:c")
	}
	return p
}

// fuzzTable is a generated CSV file.
type fuzzTable struct {
	rows      [][]string
	quote     map[[2]int]bool // fields written in quotes
	lazy      map[[2]int]bool // fields with a quote written without quotes
	crlf, bom bool
	other     map[int]bool // records ending with the other line end
	trail     map[int]bool // records with an empty field more
	blank     map[int]bool // records after a blank line
}

func genTable(s *stream) fuzzTable {
	t := fuzzTable{quote: map[[2]int]bool{}}
	for range 1 + s.n(3) {
		var row []string
		for range 1 + s.n(3) {
			row = append(row, s.pick("1", "2", "a", "", "x,y", "2026-01-01", "12", `q"r`))
		}
		t.rows = append(t.rows, row)
	}
	return t
}

func (t fuzzTable) bytes() []byte {
	var b strings.Builder
	if t.bom {
		b.WriteString("\ufeff")
	}
	for i, row := range t.rows {
		if t.blank[i] {
			b.WriteByte('\n')
		}
		for j, f := range row {
			if j > 0 {
				b.WriteByte(',')
			}
			switch at := [2]int{i, j}; {
			case t.lazy[at] && !t.quote[at] && !strings.ContainsAny(f, ",\r\n") && !strings.HasPrefix(f, `"`):
				b.WriteString(f)
			case t.quote[at] || strings.ContainsAny(f, ",\"\r\n"):
				b.WriteString(`"` + strings.ReplaceAll(f, `"`, `""`) + `"`)
			default:
				b.WriteString(f)
			}
		}
		if t.trail[i] {
			b.WriteByte(',')
		}
		if t.crlf != t.other[i] {
			b.WriteString("\r\n")
		} else {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// sameLayout reports two files written the same way, by place: an
// encoder's files whose cells differ.
func (t fuzzTable) sameLayout(o fuzzTable) bool {
	if t.crlf != o.crlf || t.bom != o.bom || len(t.rows) != len(o.rows) {
		return false
	}
	for i := range t.rows {
		if len(t.rows[i]) != len(o.rows[i]) {
			return false
		}
	}
	return maps.Equal(t.quote, o.quote) && maps.Equal(t.lazy, o.lazy) && maps.Equal(t.other, o.other) && maps.Equal(t.trail, o.trail) && maps.Equal(t.blank, o.blank)
}

func mutateTable(s *stream, t fuzzTable) fuzzTable {
	out := fuzzTable{quote: maps.Clone(t.quote), lazy: maps.Clone(t.lazy), crlf: t.crlf, bom: t.bom,
		other: maps.Clone(t.other), trail: maps.Clone(t.trail), blank: maps.Clone(t.blank)}
	if out.quote == nil {
		out.quote = map[[2]int]bool{}
	}
	flip := func(m *map[int]bool, i int) {
		if *m == nil {
			*m = map[int]bool{}
		}
		(*m)[i] = !(*m)[i]
	}
	for _, row := range t.rows {
		out.rows = append(out.rows, slices.Clone(row))
	}
	i := s.n(len(out.rows))
	j := s.n(len(out.rows[i]))
	switch s.n(9) {
	case 0:
		out.rows[i][j] = s.pick("1", "3", "a", "", "x,y", "2026-02-02", `q"r`)
	case 1:
		out.quote[[2]int{i, j}] = !out.quote[[2]int{i, j}]
	case 2:
		out.crlf = !out.crlf
	case 3:
		out.bom = !out.bom
	case 4:
		flip(&out.other, i)
	case 5:
		flip(&out.trail, i)
	case 6:
		flip(&out.blank, i)
	case 7:
		if out.lazy == nil {
			out.lazy = map[[2]int]bool{}
		}
		out.lazy[[2]int{i, j}] = !out.lazy[[2]int{i, j}]
	default:
		out.rows = append(out.rows, []string{s.pick("1", "a")})
	}
	return out
}

// fuzzAllow has rules with broad formats, parts and narrow ones. A rule
// for a page's p names it alone (p) and among siblings (p[*]): a rule for
// one only would judge a text against X and not between A and B.
func fuzzAllow(t testing.TB) *allowlist.List {
	l, err := allowlist.Parse([]byte(`version: 1
headers:
  - name: set-cookie
    format: 'a=\d'
    reason: r
  - name: vary
    ignore_case: true
    reason: r
body:
  - path: $.t
    format: '\d+'
    reason: r
  - path: $.ts
    format: '.*'
    reason: r
  - path: $.p
    format: '\d+'
    parts: true
    reason: r
  - path: $.x.*
    format: '.+'
    reason: r
  - path: html>body>div@data-x
    format: '.*'
    reason: r
  - path: html>body>div>p
    format: '\d+'
    reason: r
  - path: html>body>div>p[*]
    format: '\d+'
    reason: r
  - path: csv!A*
    format: '\d+'
    reason: r
`))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func digests(ds []Drift) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = strconv.Quote(d.DigestText())
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

// checkWitness checks the property for one case; what it returns is
// empty when it holds.
func checkWitness(t testing.TB, data []byte) string {
	s := &stream{b: data}
	jsonT := [][2]string{{"content-type", "application/json"}}
	htmlT := [][2]string{{"content-type", "text/html"}}
	csvT := [][2]string{{"content-type", "text/csv"}}
	var x, a, b Side
	// encoder: A and B are written by one encoder the same way everywhere,
	// so that only their values and lengths tell them apart.
	encoder := true
	bommed := false  // JSON A and B without a byte order mark, which may get one
	recoded := false // JSON A and B in UTF-8, which may be written in UTF-16
	switch s.n(4) {
	case 3:
		type book struct {
			style string
			names []string
		}
		gen := func() book {
			b := book{style: s.pick("0.0", "0.00")}
			for _, n := range []string{"A", "B", "C"} {
				if s.n(2) == 1 {
					b.names = append(b.names, n)
				}
			}
			if len(b.names) == 0 {
				b.names = []string{"A"} // a workbook has a worksheet
			}
			return b
		}
		mutate := func(b book) book {
			out := book{style: b.style, names: slices.Clone(b.names)}
			switch s.n(4) {
			case 0:
				out.style = s.pick("0.0", "0.00", "0.000")
			case 1:
				out.names = append(out.names, s.pick("D", "E"))
			case 2:
				if len(out.names) > 1 {
					out.names = out.names[1:]
				}
			default:
				if len(out.names) > 1 {
					out.names[0], out.names[1] = out.names[1], out.names[0]
				}
			}
			return out
		}
		styles := func(n string) string {
			return `<numFmts count="1"><numFmt numFmtId="164" formatCode="` + n + `"/></numFmts>`
		}
		bx := gen()
		ba := mutate(bx)
		bb := mutate(ba)
		x, a, b = sheets(t, styles(bx.style), bx.names...), sheets(t, styles(ba.style), ba.names...), sheets(t, styles(bb.style), bb.names...)
	case 0:
		dx := jsonDoc{v: genJSON(s, 0), pretty: s.n(2) == 1}
		da := mutateDoc(s, dx)
		db := mutateDoc(s, da)
		if s.n(2) == 0 {
			db = mutateDoc(s, dx)
		}
		// Each side is captured from an origin, which its URLs name (@O):
		// X and A from one, B from it or from another of another length.
		oa := "http://localhost:8080"
		ob := s.pick(oa, "http://127.0.0.1:61212", "http://localhost:8081")
		withOrigin := func(raw []byte, o string) string { return strings.ReplaceAll(string(raw), "@O", o) }
		x, a, b = at(side(200, jsonT, withOrigin(dx.bytes(), oa)), oa), at(side(200, jsonT, withOrigin(da.bytes(), oa)), oa), at(side(200, jsonT, withOrigin(db.bytes(), ob)), ob)
		// Headers: values in another order, a value a rule accepts, a
		// header more or less.
		gen := func() [][2]string {
			hs := [][2]string{{"content-type", "application/json"}}
			for range s.n(4) {
				hs = append(hs, [2]string{s.pick("x-a", "set-cookie", "vary", "link"), s.pick("1", "2", "a=1", "a=2", "Accept", "@O/next")})
			}
			return hs
		}
		hx := gen()
		ha := slices.Clone(hx)
		if s.n(2) == 1 {
			ha = gen()
		}
		hb := slices.Clone(ha)
		switch s.n(3) {
		case 0:
			slices.Reverse(hb[1:])
		case 1:
			hb = gen()
		}
		origin := func(hs [][2]string, o string) [][2]string {
			out := slices.Clone(hs)
			for i := range out {
				out[i][1] = strings.ReplaceAll(out[i][1], "@O", o)
			}
			return out
		}
		x.Record.Headers, a.Record.Headers, b.Record.Headers = origin(hx, oa), origin(ha, oa), origin(hb, ob)
		// A status that changes next to the body.
		a.Record.Status = []int{200, 201}[s.n(2)]
		b.Record.Status = []int{a.Record.Status, 200, 500}[s.n(3)]
		encoder = da.encoder() && db.encoder() && da.sameEncoding(db)
		bommed = !da.bom && !db.bom && da.utf16 == 0 && db.utf16 == 0
		// Origins are replaced in the bytes of a body, which in UTF-16
		// they are not (as on development): from one origin only.
		recoded = da.utf16 == 0 && db.utf16 == 0 && oa == ob
	case 1:
		px := fuzzPage{root: genNode(s, 0)}
		pa := mutatePage(s, px)
		pb := mutatePage(s, pa)
		x, a, b = side(200, htmlT, string(px.bytes())), side(200, htmlT, string(pa.bytes())), side(200, htmlT, string(pb.bytes()))
	default:
		tx := genTable(s)
		ta := mutateTable(s, tx)
		tb := mutateTable(s, ta)
		x, a, b = side(200, csvT, string(tx.bytes())), side(200, csvT, string(ta.bytes())), side(200, csvT, string(tb.bytes()))
		encoder = ta.sameLayout(tb)
	}
	level := []Level{LevelShape, LevelTypes, LevelValues, LevelValues}[s.n(4)]
	allow, rules := allowlist.Empty(), s.n(2) == 1
	if rules {
		allow = fuzzAllow(t)
	}
	// A rule judges only the current value, so A→B and B→A may disagree
	// (B's value fits the format, A's does not): the property holds for
	// pairs the comparison sees alike both ways.
	ab, ba := witnessed("req", a, b, allow, level), witnessed("req", b, a, allow, level)
	xa, xb := witnessed("req", x, a, allow, level), witnessed("req", x, b, allow, level)
	da, db := digests(xa), digests(xb)
	// allowed: a rule let something between A and B change (the drifts
	// differ from those without rules).
	allowed := rules && places(ab) != places(witnessed("req", a, b, allowlist.Empty(), level))
	if bommed {
		// A byte order mark on both sides changes nothing else the
		// comparison sees.
		ma, mb := a, b
		ma.Body, mb.Body = append([]byte("\ufeff"), a.Body...), append([]byte("\ufeff"), b.Body...)
		if got := places(witnessed("req", ma, mb, allow, level)); got != places(ab) {
			return fmt.Sprintf("a byte order mark on both sides changes the drifts\nA %q\nB %q\nlevel %s\n%s\n--\n%s", a.Body, b.Body, level, places(ab), got)
		}
	}
	if recoded {
		// Both sides in UTF-16: the same drifts, layout included.
		big := len(a.Body)%2 == 0
		ua, ub := a, b
		ua.Body, ub.Body = []byte(inUTF16(string(a.Body), big)), []byte(inUTF16(string(b.Body), big))
		if got := places(witnessed("req", ua, ub, allow, level)); got != places(ab) {
			return fmt.Sprintf("UTF-16 on both sides changes the drifts\nA %q\nB %q\nlevel %s\n%s\n--\n%s", a.Body, b.Body, level, places(ab), got)
		}
	}
	switch {
	case (len(ab) == 0) != (len(ba) == 0):
	case len(ab) == 0 && da != db && (!rules || encoder && !alone(xa) && !alone(xb)):
		// Without rules A and B are the same. With rules they differ in
		// values and lengths the rules allow, written by one encoder; a
		// byte difference X→A shows when nothing else changed (alone) may
		// be missing from X→B next to an allowed change, as before
		// expectations.
		return fmt.Sprintf("false fail: A and B do not drift, and the digests differ\nX %q\nA %q\nB %q\nlevel %s\n%s\n--\n%s", x.Body, a.Body, b.Body, level, da, db)
	case len(ab) > 0 && len(xa) > 0 && da == db && !allowed:
		// An expectation of X→A (it has drifts) taken for B.
		return fmt.Sprintf("false pass: A and B drift (%s), and the digests are the same\nX %q\nA %q\nB %q\nlevel %s\n%s", driftList(ab), x.Body, a.Body, b.Body, level, da)
	case len(ab) > 0 && da == db && described(xa, level) != described(xb, level):
		// With rules, X→A and X→B may both be allowed where A→B is not (a
		// rule judges the changed value against its format, not against
		// the other response). Equal digests must still mean the same
		// drifts, apart from what rules let vary and what this level
		// does not compare.
		return fmt.Sprintf("false pass: the digests are the same, the drifts are not\nX %q\nA %q\nB %q\nlevel %s\n%s\n--\n%s", x.Body, a.Body, b.Body, level, described(xa, level), described(xb, level))
	}
	return ""
}

// places lists the kinds and places of drifts, sorted.
func places(ds []Drift) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Kind + " " + d.Where
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

// alone reports a drift of a body's bytes that is one only when nothing
// else changed.
func alone(ds []Drift) bool {
	for _, d := range ds {
		if (d.Kind == KindEncoding && d.Where == "$") || (d.Kind == KindBody && d.Where == "csv") {
			return true
		}
	}
	return false
}

// described writes drifts as a report shows them, without what rules let
// vary (values under rules with parts) and what the level does not
// compare.
func described(ds []Drift, level Level) string {
	out := make([]string, len(ds))
	for i, d := range ds {
		base, cur := d.Baseline, d.Current
		if d.parted || level < LevelValues && !structural[d.Kind] || level < LevelTypes && d.Kind == KindType {
			base, cur = "", ""
		}
		out[i] = strconv.Quote(d.Kind + " " + d.Where + " " + base + " → " + cur)
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

func FuzzWitness(f *testing.F) {
	for i := range 64 {
		f.Add([]byte(fmt.Sprintf("%c%c%c%c%c%c%c%c%c%c%c%c", i, i*7, i*13, i*3, i*5, i*11, i*17, i*19, i*23, i*29, i*31, i*37)))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if msg := checkWitness(t, data); msg != "" {
			t.Fatal(msg)
		}
	})
}

// TestWitnessProperty runs the property over many generated cases, as a
// test (FuzzWitness searches further).
func TestWitnessProperty(t *testing.T) {
	failures := 0
	for seed := range 20000 {
		r := rand.New(rand.NewPCG(uint64(seed), 7))
		data := make([]byte, 96)
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		if msg := checkWitness(t, data); msg != "" {
			t.Errorf("seed %d: %s", seed, msg)
			if failures++; failures == 5 {
				return
			}
		}
	}
}

func witnessed(id string, base, cur Side, allow *allowlist.List, level Level) []Drift {
	ds, _ := OneWitnessed(id, base, cur, allow, level)
	return ds
}
