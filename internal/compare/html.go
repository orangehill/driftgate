package compare

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// Drift kinds of an HTML body.
const (
	KindElementMissing   = "element-missing"
	KindElementExtra     = "element-extra"
	KindElementOrder     = "element-order"
	KindAttributeMissing = "attribute-missing"
	KindAttributeExtra   = "attribute-extra"
	KindAttributeValue   = "attribute-value"
	KindText             = "text"
	KindDoctype          = "doctype" // the document type declaration, at DoctypePath
)

// DoctypePath is where a drift of the <!DOCTYPE> declaration is reported.
const DoctypePath = "!doctype"

// FlightPath is where the React Server Components payload of a Next.js app
// router page (self.__next_f.push) is compared: FlightPath + a JSON path,
// one key per payload row (self.__next_f$.0.b is the build id).
const FlightPath = "self.__next_f"

// isHTML reports a response whose content type is text/html.
func isHTML(r *snapshot.Record) bool {
	for _, h := range r.Headers {
		if strings.EqualFold(h[0], "content-type") {
			mt, _, _ := strings.Cut(h[1], ";")
			return strings.EqualFold(strings.TrimSpace(mt), "text/html")
		}
	}
	return false
}

// element is one HTML element as the comparison sees it.
type element struct {
	tag, id string
	attrs   map[string]string
	text    []textRun // text between child elements, in order; whitespace collapsed unless kept
	kids    []*element
	flight  bool   // a script that pushes the RSC payload: its text is compared as the payload
	hash    string // of the whole subtree, to align children
}

// textRun is a text of an element and where it stands: after the first at
// child elements. lead and trail: whitespace stood between it and the
// inline element before or after it, which a browser shows as a space
// ("a <b>x</b>" reads "a x", "a<b>x</b>" "ax"); the text itself is
// written without it.
type textRun struct {
	at          int
	text        string
	lead, trail bool
}

// edged is the text with a space for each edge, as it reads next to its
// inline elements.
func (t textRun) edged() string {
	s := t.text
	if t.lead {
		s = " " + s
	}
	if t.trail {
		s += " "
	}
	return s
}

// key names an element among its siblings: tag and id.
func (e *element) key() string {
	if e.id != "" {
		return pathName(e.tag) + "#" + pathName(e.id)
	}
	return pathName(e.tag)
}

// pathSyntax are the characters that mean something in a path into a page
// (and in a glob over it).
const pathSyntax = "@>#[]$*?\"\\"

// pathName is a tag, an id or an attribute name as a step of a path: as it
// is, or quoted when it holds path syntax, whitespace or control characters,
// so that a path names one place only (div#"x@nonce" is not an attribute).
func pathName(s string) string {
	for _, r := range s {
		if strings.ContainsRune(pathSyntax, r) || r <= ' ' || r == 0x7f || !unicode.IsPrint(r) {
			return jsonx.Quote(s)
		}
	}
	return s
}

// shownText writes a text of an element with children (<b>) so that it
// never reads as one: its < and & as in HTML.
func (e *element) shownText(s string) string {
	if len(e.kids) == 0 || !strings.ContainsAny(s, "<&") {
		return s
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;").Replace(s)
}

// joined is the element's own text as one string.
func (e *element) joined() string {
	parts := make([]string, len(e.text))
	for i, t := range e.text {
		parts[i] = t.text
	}
	return strings.Join(parts, " ")
}

// layout is the element's content as it reads: its texts with a <key> in the
// place of every child element.
func (e *element) layout() string {
	var parts []string
	r := 0
	for k := 0; k <= len(e.kids); k++ {
		for ; r < len(e.text) && e.text[r].at == k; r++ {
			parts = append(parts, e.shownText(e.text[r].text))
		}
		if k < len(e.kids) {
			parts = append(parts, "<"+e.kids[k].key()+">")
		}
	}
	return strings.Join(parts, " ")
}

// spacing is layout as a browser reads it: a space only where the page has
// one between a text and an element (textRun), so "a <b>" and "a<b>"
// differ.
func (e *element) spacing() string {
	var b strings.Builder
	r := 0
	for k := 0; k <= len(e.kids); k++ {
		for ; r < len(e.text) && e.text[r].at == k; r++ {
			b.WriteString(e.shownText(e.text[r].edged()))
		}
		if k < len(e.kids) {
			b.WriteString("<" + e.kids[k].key() + ">")
		}
	}
	return b.String()
}

// keepSpace are the elements whose whitespace counts (§7.4): pre and the
// elements the parser treats like it (listing, xmp, plaintext), and raw text.
var keepSpace = map[string]bool{"pre": true, "listing": true, "xmp": true, "plaintext": true, "textarea": true, "script": true, "style": true}

// inline are the elements a browser lays out in a line by default (phrasing
// content that shows; br and wbr end or split a line). Whitespace between
// two of them shows as a space: <b>a</b> <i>b</i> reads "a b", without it
// "ab". Between other elements (blocks, scripts, unknown tags) it is only
// the page's indentation.
var inline = map[string]bool{
	"a": true, "abbr": true, "acronym": true, "audio": true, "b": true, "bdi": true, "bdo": true, "big": true,
	"button": true, "canvas": true, "cite": true, "code": true, "data": true, "del": true, "dfn": true, "em": true,
	"embed": true, "font": true, "i": true, "iframe": true, "img": true, "input": true, "ins": true, "kbd": true,
	"label": true, "mark": true, "math": true, "meter": true, "object": true, "output": true, "picture": true,
	"progress": true, "q": true, "ruby": true, "s": true, "samp": true, "select": true, "small": true, "span": true,
	"strike": true, "strong": true, "sub": true, "sup": true, "svg": true, "textarea": true, "time": true, "tt": true,
	"u": true, "var": true, "video": true,
}

// page is a parsed HTML body.
type page struct {
	root    *element
	doctype string   // the <!DOCTYPE> declaration as the parser reads it; "" for none
	flight  []string // arguments of self.__next_f.push, in page order
}

// parsePage parses an HTML body. The parser refuses a page nested deeper than
// 512 elements; that is the only error a bytes.Reader leaves.
func parsePage(body []byte) (*page, error) {
	p := &page{}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for n := doc.FirstChild; n != nil; n = n.NextSibling {
		switch n.Type {
		case html.ElementNode:
			p.root = p.element(n, false)
		case html.DoctypeNode:
			p.doctype = doctype(n)
		}
	}
	if p.root == nil {
		p.root = &element{tag: "html", attrs: map[string]string{}}
		p.root.hash = p.root.sum()
	}
	return p, nil
}

// doctype writes a document type declaration as the parser read it: the
// name in lower case, then its public and system identifiers, so
// <!doctype HTML> and <!DOCTYPE html> are one declaration.
func doctype(n *html.Node) string {
	s := "<!DOCTYPE " + n.Data
	for _, a := range n.Attr {
		switch a.Key {
		case "public":
			s += " PUBLIC " + jsonx.Quote(a.Val)
		case "system":
			s += " SYSTEM " + jsonx.Quote(a.Val)
		}
	}
	return s + ">"
}

func (p *page) element(n *html.Node, keep bool) *element {
	e := &element{tag: n.Data, attrs: map[string]string{}}
	for _, a := range n.Attr {
		name := a.Key
		if a.Namespace != "" {
			name = a.Namespace + ":" + a.Key
		}
		if _, dup := e.attrs[name]; !dup {
			e.attrs[name] = a.Val
		}
	}
	e.id = e.attrs["id"]
	keep = keep || keepSpace[e.tag]
	if arg, ok := flightPush(n); ok {
		// The script stays an element (its attributes and its place count);
		// what it pushes is compared as the payload.
		p.flight = append(p.flight, arg)
		e.flight = true
		e.hash = e.sum()
		return e
	}
	// Text nodes split only by comments (React writes <!-- --> between
	// adjacent texts) are one text. Whitespace alone is left out, but
	// between two inline elements it is one space (next is the element
	// after the text, nil at the end).
	var run strings.Builder
	flush := func(next *html.Node) {
		t := run.String()
		run.Reset()
		run := textRun{at: len(e.kids), text: t}
		if !keep && t != "" {
			afterInline := len(e.kids) > 0 && inline[e.kids[len(e.kids)-1].tag]
			beforeInline := next != nil && inline[next.Data]
			run.text = strings.Join(strings.FieldsFunc(t, isSpace), " ")
			switch {
			case run.text == "":
				if afterInline && beforeInline {
					run.text = " "
				}
			default:
				run.lead = afterInline && isSpace(rune(t[0]))
				run.trail = beforeInline && isSpace(rune(t[len(t)-1]))
			}
		}
		if run.text != "" {
			e.text = append(e.text, run)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			run.WriteString(c.Data)
		case html.ElementNode:
			flush(c)
			k := p.element(c, keep)
			// The payload comes in chunks cut where the server flushed, so
			// adjacent payload scripts that look the same are one element.
			if last := len(e.kids) - 1; k.flight && last >= 0 && e.kids[last].hash == k.hash &&
				(len(e.text) == 0 || e.text[len(e.text)-1].at <= last) {
				continue
			}
			e.kids = append(e.kids, k)
		}
	}
	flush(nil)
	e.hash = e.sum()
	return e
}

// sum hashes the element with its attributes and its content in order.
// shapeSum is the hash of what the shape and types levels compare of a
// subtree (tags, ids, attribute names, payload marks): below the values
// level children are aligned by it, not by content those levels ignore.
// It is made when first asked for, and kept in shapes.
func (e *element) shapeSum(shapes map[*element]string) string {
	if s, ok := shapes[e]; ok {
		return s
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s#%d:%s", len(e.tag), e.tag, len(e.id), e.id)
	if e.flight {
		fmt.Fprint(h, "f\x00")
	}
	names := make([]string, 0, len(e.attrs))
	for k := range e.attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(h, "@%d:%s", len(k), k)
	}
	for _, k := range e.kids {
		fmt.Fprintf(h, ">%s", k.shapeSum(shapes))
	}
	s := string(h.Sum(nil))
	shapes[e] = s
	return s
}

func (e *element) sum() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", e.tag)
	if e.flight {
		fmt.Fprint(h, "f\x00")
	}
	names := make([]string, 0, len(e.attrs))
	for k := range e.attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(h, "@%d:%s=%d:%s", len(k), k, len(e.attrs[k]), e.attrs[k])
	}
	r := 0
	for k := 0; k <= len(e.kids); k++ {
		for ; r < len(e.text) && e.text[r].at == k; r++ {
			t := e.text[r]
			fmt.Fprintf(h, "t%d:%s", len(t.text), t.text)
			if t.lead {
				h.Write([]byte("<"))
			}
			if t.trail {
				h.Write([]byte(">"))
			}
		}
		if k < len(e.kids) {
			fmt.Fprintf(h, ">%s", e.kids[k].hash)
		}
	}
	return string(h.Sum(nil))
}

// isSpace is ASCII whitespace as HTML defines it.
func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '\r' }

// flightPush returns the argument of an inline script that only pushes a
// chunk of the RSC payload: self.__next_f.push([1,"…"]).
func flightPush(n *html.Node) (string, bool) {
	if n.Data != "script" || n.FirstChild == nil || n.FirstChild != n.LastChild || n.FirstChild.Type != html.TextNode {
		return "", false
	}
	for _, a := range n.Attr {
		if a.Key == "src" {
			return "", false
		}
	}
	s := strings.TrimSpace(n.FirstChild.Data)
	for _, prefix := range []string{"self.__next_f.push(", "(self.__next_f=self.__next_f||[]).push("} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			rest = strings.TrimSuffix(rest, ";")
			if inner, ok := strings.CutSuffix(rest, ")"); ok {
				return inner, true
			}
		}
	}
	return "", false
}

// htmlBody compares two HTML bodies element by element. A body the parser
// refuses (nested deeper than 512 elements) has no tree to compare: the two
// bodies are then compared as bytes, like a body that is neither JSON nor
// HTML, and the drift says why.
func (c *comparer) htmlBody(base, cur []byte) {
	b, errB := parsePage(base)
	v, errV := parsePage(cur)
	if errB != nil || errV != nil {
		c.add(KindBody, "", unparsed(base, errB), unparsed(cur, errV))
		if c.witnessing {
			c.witness(base, cur)
		}
		return
	}
	// The declaration chooses how a browser renders the page (standards or
	// quirks mode): its presence is structure, its text a value.
	switch {
	case b.doctype == v.doctype:
	case b.doctype == "" || v.doctype == "":
		c.add(KindDoctype, DoctypePath, b.doctype, v.doctype)
	case c.level >= LevelValues:
		c.add(KindDoctype, DoctypePath, b.doctype, v.doctype)
	}
	c.node("html", b.root, v.root)
	switch {
	case len(b.flight) == 0 && len(v.flight) == 0:
	case len(v.flight) == 0:
		c.add(KindElementMissing, FlightPath, FlightPath, "")
		if c.witnessing {
			c.witness(c.source(FlightPath+"$", flightValue(b.flight)))
		}
	case len(b.flight) == 0:
		c.add(KindElementExtra, FlightPath, "", FlightPath)
		if c.witnessing {
			c.witness(c.source(FlightPath+"$", flightValue(v.flight)))
		}
	default:
		c.walk(FlightPath+"$", flightValue(b.flight), flightValue(v.flight))
	}
}

func unparsed(body []byte, err error) string {
	if err != nil {
		return fmt.Sprintf("%d bytes, not parsed as HTML (%v)", len(body), err)
	}
	return fmt.Sprintf("%d bytes", len(body))
}

// node compares two paired elements: attributes, text or embedded JSON, children.
func (c *comparer) node(path string, b, v *element) {
	c.attributes(path, b, v)
	pairs, missing, extra, moved := align(c.alignKey(), b.kids, v.kids)
	if kind := embeddedJSON(b); kind == "" || kind != embeddedJSON(v) || !c.embedded(path, kind, b.joined(), v.joined()) {
		c.text(path, b, v, pairs)
	}
	c.children(path, b, v, pairs, missing, extra, moved)
}

// text compares the texts of two paired elements and where they stand among
// the child elements both have: <p><b>5</b> EUR</p> and <p>EUR <b>5</b></p>
// differ. A child element that is missing or extra is reported by itself and
// does not move the texts around it.
func (c *comparer) text(path string, b, v *element, pairs [][2]int) {
	if c.level < LevelValues || (len(b.text) == 0 && len(v.text) == 0) {
		return
	}
	bAnchor, vAnchor := make([]int, len(b.kids)), make([]int, len(v.kids))
	for i := range bAnchor {
		bAnchor[i] = -1
	}
	for j := range vAnchor {
		vAnchor[j] = -1
	}
	for _, p := range pairs {
		bAnchor[p[0]], vAnchor[p[1]] = p[0], p[0]
	}
	gb, gv := textGroups(b, bAnchor), textGroups(v, vAnchor)
	same, placed := len(gb) == len(gv), len(gb) == len(gv)
	for i := 0; i < len(gb) && same; i++ {
		placed = placed && gb[i].anchor == gv[i].anchor
		// Where both sides have an element only they have between texts
		// (one replaced by another), the spacing next to it is compared
		// too: a client reads "aa" and "a a". An element only one side
		// has explains the texts around it coming together or apart.
		same = placed && gb[i].text == gv[i].text && (gb[i].from == gb[i].to || gv[i].from == gv[i].to || gb[i].edges(b) == gv[i].edges(v))
	}
	if same {
		return
	}
	var layout [2]string
	bj, vj := b.joined(), v.joined()
	switch {
	case !placed:
		layout = [2]string{b.layout(), v.layout()}
	case bj == vj:
		// Only a space between a text and an element came or went.
		layout = [2]string{b.spacing(), v.spacing()}
	}
	before := len(c.drifts)
	c.value(KindText, path, bj, vj, false, layout)
	if c.witnessing && len(c.drifts) > before {
		// The drift shows the joined texts or an excerpt: the witness has
		// where every text stands and the spacing around the elements;
		// under rules with parts the texts as those rules see them.
		c.witness(c.runsSource(path, b), c.runsSource(path, v))
		if _, parts := c.allow.TextRules(c.id, path); len(parts) > 0 {
			c.drifts[len(c.drifts)-1].parted = true
		}
	}
}

// textGroup is the text that follows a child element both sides have (anchor,
// its index in the baseline; -1 for the start of the element).
type textGroup struct {
	anchor int
	text   string
	// from and to are the group's runs (Element.text); more than one when
	// elements only this side has stood between them.
	from, to int
}

// collapseSpaces writes runs of spaces as one.
func collapseSpaces(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}

// textGroups places every text of an element after the last child before it
// that both sides have; anchor maps a child to its index in the baseline, or
// -1. Texts after the same child are one text.
func textGroups(e *element, anchor []int) []textGroup {
	var out []textGroup
	last := -1
	k := 0
	for i, t := range e.text {
		for ; k < t.at; k++ {
			if anchor[k] >= 0 {
				last = anchor[k]
			}
		}
		if n := len(out) - 1; n >= 0 && out[n].anchor == last {
			// An element only one side has stood between them: one space.
			out[n].text = strings.TrimRight(out[n].text, " ") + " " + strings.TrimLeft(t.edged(), " ")
			out[n].to = i
			continue
		}
		out = append(out, textGroup{anchor: last, text: t.edged(), from: i, to: i})
	}
	return out
}

// edges is a group's texts as they read with the elements only one side
// has left out: their own spacing next to inline elements, a space for an
// element that is not inline (a block, br, a script: a break), runs of
// spaces as one.
func (g textGroup) edges(e *element) string {
	var b strings.Builder
	for i := g.from; i <= g.to; i++ {
		t := e.text[i]
		if i > g.from {
			for k := e.text[i-1].at; k < t.at; k++ {
				if !inline[e.kids[k].tag] {
					b.WriteByte(' ')
					break
				}
			}
		}
		b.WriteString(t.edged())
	}
	return collapseSpaces(b.String())
}

func (c *comparer) attributes(path string, b, v *element) {
	names := map[string]bool{}
	for k := range b.attrs {
		names[k] = true
	}
	for k := range v.attrs {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		bv, inB := b.attrs[k]
		vv, inV := v.attrs[k]
		where := path + "@" + pathName(k)
		switch {
		case !inV:
			c.add(KindAttributeMissing, where, jsonx.Quote(bv), "")
		case !inB:
			c.add(KindAttributeExtra, where, "", jsonx.Quote(vv))
		case c.level >= LevelValues:
			c.value(KindAttributeValue, where, bv, vv, bv == vv, [2]string{})
		}
	}
}

// value compares a text or an attribute value. As for JSON, a rule judges
// only a value that changed; a rule with a whole-value format decides
// alone, rules with parts allow only their parts to change, and a rule
// without a format (any_length) says nothing about texts and attributes.
// layout, when set, is a text that moved past a child element (the
// element's content with <key> for every child): only a whole-value format
// allows that.
func (c *comparer) value(kind, where, b, v string, same bool, layout [2]string) {
	if same {
		return
	}
	whole, parts := c.allow.TextRules(c.id, where)
	if whole != nil {
		if !whole.Matches(v) {
			c.add(KindFormat, where, jsonx.Quote(b), jsonx.Quote(v))
		}
		return
	}
	if layout[0] != "" || layout[1] != "" {
		bs, vs := excerpt(layout[0], layout[1])
		c.add(kind, where, bs, vs)
		c.drifts[len(c.drifts)-1].Texts = [2]string{b, v}
		if c.witnessing {
			c.witness([]byte(layout[0]), []byte(layout[1]), []byte(b), []byte(v))
		}
		return
	}
	if allowlist.SameParts(parts, b, v) {
		return
	}
	bs, vs := excerpt(b, v)
	c.add(kind, where, bs, vs)
	c.drifts[len(c.drifts)-1].Texts = [2]string{b, v}
	if c.witnessing {
		c.witness([]byte(b), []byte(v)) // the excerpt shows part of them
	}
	if c.witnessing {
		c.parted(where, b, v, len(parts) > 0, false)
	}
}

// excerpt quotes two long texts around the place where they differ, so a
// changed word in a long inline script is visible in the report.
func excerpt(b, v string) (string, string) {
	const long, context = 120, 40
	if len(b) <= long && len(v) <= long {
		return jsonx.Quote(b), jsonx.Quote(v)
	}
	start := 0
	for start < len(b) && start < len(v) && b[start] == v[start] {
		start++
	}
	end := 0
	for end < len(b)-start && end < len(v)-start && b[len(b)-1-end] == v[len(v)-1-end] {
		end++
	}
	cut := func(s string) string {
		from, to := max(0, start-context), min(len(s), len(s)-end+context)
		for from > 0 && !utf8.RuneStart(s[from]) {
			from--
		}
		for to < len(s) && !utf8.RuneStart(s[to]) {
			to++
		}
		out := jsonx.Quote(s[from:to])
		if from > 0 {
			out = "…" + out
		}
		if to < len(s) {
			out += "…"
		}
		return out
	}
	return cut(b), cut(v)
}

// children reports the child elements that are missing, extra or moved, and
// compares the pairs.
func (c *comparer) children(path string, be, ve *element, pairs [][2]int, missing, extra []int, moved bool) {
	b, v := be.kids, ve.kids
	if len(b) == 0 && len(v) == 0 {
		return
	}
	bs, vs := steps(b), steps(v)
	for _, i := range missing {
		c.add(KindElementMissing, path+">"+bs[i], b[i].key(), "")
		if c.witnessing {
			c.witness(c.elementSource(path+">"+bs[i], b[i]), c.around(path, be), c.around(path, ve))
		}
	}
	for _, j := range extra {
		c.add(KindElementExtra, path+">"+vs[j], "", v[j].key())
		// And where it stands, with the texts around it.
		if c.witnessing {
			c.witness(c.elementSource(path+">"+vs[j], v[j]), c.textsSource(keys(v)), c.around(path, be), c.around(path, ve))
		}
	}
	if moved {
		c.add(KindElementOrder, path, listText(keys(b)), listText(keys(v)))
		// Which element went where: keys may repeat; the content of each
		// pair is compared by itself.
		if c.witnessing {
			c.witness(pairsSource(pairs))
		}
	}
	for _, p := range pairs {
		c.node(path+">"+bs[p[0]], b[p[0]], v[p[1]])
	}
}

func keys(list []*element) []string {
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.key()
	}
	return out
}

// steps names every element in a list of siblings: its key, and its index
// among the siblings with the same key when there is more than one.
func steps(list []*element) []string {
	count := map[string]int{}
	for _, e := range list {
		count[e.key()]++
	}
	seen := map[string]int{}
	out := make([]string, len(list))
	for i, e := range list {
		k := e.key()
		out[i] = k
		if count[k] > 1 {
			out[i] = fmt.Sprintf("%s[%d]", k, seen[k])
		}
		seen[k]++
	}
	return out
}

// maxLCS bounds the table of the longest common subsequence; longer lists of
// siblings are paired by key only.
const maxLCS = 1 << 22

// align pairs the children of two elements. Identical subtrees are paired
// first, keeping their order (longest common subsequence); between them the
// n-th remaining element of a key is paired with the n-th of the same key.
// An element that turns up elsewhere (the same subtree, or the same tag and
// id) is paired too and makes the order a drift.
// alignKey is what children are aligned by: their whole subtree at the
// values level, below it what that level compares.
func (c *comparer) alignKey() func(*element) string {
	if c.level >= LevelValues {
		return func(e *element) string { return e.hash }
	}
	if c.shapes == nil {
		c.shapes = map[*element]string{}
	}
	return func(e *element) string { return e.shapeSum(c.shapes) }
}

func align(key func(*element) string, b, v []*element) (pairs [][2]int, missing, extra []int, moved bool) {
	pb, pv := make([]int, len(b)), make([]int, len(v))
	for i := range pb {
		pb[i] = -1
	}
	for j := range pv {
		pv[j] = -1
	}
	link := func(i, j int) { pb[i], pv[j] = j, i }
	// Common prefix and suffix, then the longest common subsequence.
	lo := 0
	for lo < len(b) && lo < len(v) && key(b[lo]) == key(v[lo]) {
		link(lo, lo)
		lo++
	}
	hiB, hiV := len(b), len(v)
	for hiB > lo && hiV > lo && key(b[hiB-1]) == key(v[hiV-1]) {
		hiB--
		hiV--
		link(hiB, hiV)
	}
	n, m := hiB-lo, hiV-lo
	if n > 0 && m > 0 && (n+1)*(m+1) <= maxLCS {
		dp := make([]int32, (n+1)*(m+1))
		at := func(i, j int) *int32 { return &dp[i*(m+1)+j] }
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				switch {
				case key(b[lo+i]) == key(v[lo+j]):
					*at(i, j) = *at(i+1, j+1) + 1
				case *at(i+1, j) >= *at(i, j+1):
					*at(i, j) = *at(i+1, j)
				default:
					*at(i, j) = *at(i, j+1)
				}
			}
		}
		for i, j := 0, 0; i < n && j < m; {
			switch {
			case key(b[lo+i]) == key(v[lo+j]):
				link(lo+i, lo+j)
				i++
				j++
			case *at(i+1, j) >= *at(i, j+1):
				i++
			default:
				j++
			}
		}
	}
	// Between two paired elements: pair by key, in order.
	i, j := 0, 0
	for i <= len(b) && j <= len(v) {
		ei, ej := i, j
		for ei < len(b) && pb[ei] < 0 {
			ei++
		}
		for ej < len(v) && pv[ej] < 0 {
			ej++
		}
		free := map[string][]int{}
		for y := j; y < ej; y++ {
			free[v[y].key()] = append(free[v[y].key()], y)
		}
		for x := i; x < ei; x++ {
			if q := free[b[x].key()]; len(q) > 0 {
				link(x, q[0])
				free[b[x].key()] = q[1:]
			}
		}
		i, j = ei+1, ej+1
	}
	// Elements that moved: the same subtree, or the same tag and id.
	byHash, byID := map[string][]int{}, map[string][]int{}
	for y := range v {
		if pv[y] < 0 {
			byHash[key(v[y])] = append(byHash[key(v[y])], y)
			if v[y].id != "" {
				byID[v[y].key()] = append(byID[v[y].key()], y)
			}
		}
	}
	take := func(q []int) int {
		for _, y := range q {
			if pv[y] < 0 {
				return y
			}
		}
		return -1
	}
	for x := range b {
		if pb[x] >= 0 {
			continue
		}
		y := take(byHash[key(b[x])])
		if y < 0 && b[x].id != "" {
			y = take(byID[b[x].key()])
		}
		if y >= 0 {
			link(x, y)
		}
	}
	last := -1
	for x := range b {
		if pb[x] < 0 {
			missing = append(missing, x)
			continue
		}
		pairs = append(pairs, [2]int{x, pb[x]})
		if pb[x] < last {
			moved = true
		}
		last = pb[x]
	}
	for y := range v {
		if pv[y] < 0 {
			extra = append(extra, y)
		}
	}
	return pairs, missing, extra, moved
}

// embeddedJSON says how a script element's content is compared: "json"
// (application/json, application/ld+json and other +json types), "devalue"
// (Nuxt's payload) or "" (as text).
func embeddedJSON(e *element) string {
	if e.tag != "script" || e.attrs["src"] != "" {
		return ""
	}
	if e.id == "__NUXT_DATA__" || hasAttr(e, "data-nuxt-data") {
		return "devalue"
	}
	t, _, _ := strings.Cut(e.attrs["type"], ";")
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "application/json" || strings.HasSuffix(t, "+json") || e.id == "__NEXT_DATA__" {
		return "json"
	}
	return ""
}

func hasAttr(e *element, name string) bool {
	_, ok := e.attrs[name]
	return ok
}

// embedded compares the JSON inside two script elements at path$…; false
// when one of them is not JSON, so the scripts are compared as text.
func (c *comparer) embedded(path, kind, b, v string) bool {
	rb, rv := []byte(b), []byte(v)
	jb, errB := jsonx.Parse(rb)
	jv, errV := jsonx.Parse(rv)
	if errB != nil || errV != nil {
		return false
	}
	decoded := false
	if kind == "devalue" {
		db, okB := devalue(jb)
		dv, okV := devalue(jv)
		if okB && okV {
			jb, jv, decoded = db, dv, true
		}
	}
	if b != v {
		c.devalue = decoded
		c.json(path+"$", rb, rv, jb, jv)
		c.devalue = false
	}
	return true
}
