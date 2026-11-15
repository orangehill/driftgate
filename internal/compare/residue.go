package compare

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"strconv"
	"unicode/utf8"
	"unsafe"

	"github.com/orangehill/driftgate/internal/jsonx"
)

// What a body document writes beyond its values (how JSON strings are
// escaped, the whitespace, a CSV file's quotes and line ends) is a drift
// only in part: next to an allowed change or another drift the comparison
// does not report it. An expectation must still hold only for a response
// written the same way, wherever its drifts are (a header, the status,
// another part of a page). So for every document whose bytes differ
// between the sides, its residue (what it writes beyond its values) is
// taken on both sides, and when the residue of any document changed,
// all of them are in the witness of every drift of the response.
//
// A residue with the document's values gives the document back: when no
// drift and no allowed change tells two documents apart, their values
// are the same, and equal residues mean equal bytes. It is written so
// that an encoder writes the same residue for other values and lengths:
// the layout (jsonx.StyleOf), the whitespace where it is not what the
// layout says, and how characters are escaped by the document's rule
// for each character, by place only for a character it writes in more
// than one way.

// encDoc is a body document whose bytes differ between the sides.
type encDoc struct {
	csv, devalue bool // a CSV file; JSON compared as the payload it encodes
	where        string
	b, v         []byte
}

// document notes a body document whose bytes differ, when witnessing at
// the values level (below it, how a document is written is not
// compared).
func (c *comparer) document(csv bool, where string, b, v []byte) {
	if c.witnessing && c.level >= LevelValues {
		c.docs = append(c.docs, encDoc{csv: csv, devalue: c.devalue, where: where, b: b, v: v})
	}
}

// foldEncodings adds the residues of the response's documents to the
// witness of every drift, when the residue of one of them changed; it is
// taken only for a response with drifts, and hashed once.
func (c *comparer) foldEncodings() {
	docs := c.docs
	c.docs = nil
	if len(c.drifts) == 0 || len(docs) == 0 {
		return
	}
	e := c.enc()
	changed := false
	for _, d := range docs {
		b, v := d.b, d.v
		if c.own != nil { // read with the origins written neutrally
			b, v = c.own.replace(b), c.own.replace(v)
		}
		var rb, rv []byte
		switch {
		case d.csv:
			rb, rv = csvResidue(b), csvResidue(v)
		case d.devalue || !c.allow.HasValueRules():
			// A payload's paths are the decoded value's, not its JSON's.
			rb, rv = jsonResidue(b, nil), jsonResidue(v, nil)
		default:
			rules := c.ruled(d.where)
			rb, rv = jsonResidue(b, rules), jsonResidue(v, rules)
		}
		if d.devalue {
			rb, rv = (&canon{}).bytes(rb).bytes(slots(b)).b, (&canon{}).bytes(rv).bytes(slots(v)).b
		}
		if bytes.Equal(rb, rv) {
			continue
		}
		e.text(d.where).bytes(rb).bytes(rv)
		changed = true
	}
	if !changed {
		return
	}
	h := e.sum()
	for i := range c.drifts {
		c.drifts[i].Witness = (&canon{}).text(c.drifts[i].Witness).text(h).sum()
	}
}

// jsonResidue writes what a JSON document writes beyond its values: its
// layout, what stands before and after it, its spacing and its escapes.
// The strings of values a rule covers (rules) are left out: the rule
// covers how they are written as it lets them change.
func jsonResidue(raw []byte, rules *ruled) []byte {
	if !plainJSON(raw) {
		// Another encoding (UTF-16, UTF-32): the residue of its text, with
		// the encoding, which with the text gives the bytes back; bytes
		// that decode to no text: the document as it is.
		enc, text, ok := decoded(raw)
		if !ok {
			return (&canon{}).int(0).bytes(raw).b
		}
		return (&canon{}).int(2).text(enc).bytes(jsonResidue(text, rules)).b
	}
	before, after := edges(raw)
	doc := raw[len(before) : len(raw)-len(after)]
	style := jsonx.StyleOf(raw)
	strs := func(f func([]byte)) { eachString(doc, f) }
	if rules != nil {
		strs = func(f func([]byte)) { rules.strings(doc, f) }
	}
	e := (&canon{}).int(1)
	e.bytes(styleSource(style)).bytes(before).bytes(after).bytes(spacing(doc, style)).bytes(escapes(doc, strs))
	return e.b
}

// plainJSON reports a JSON document written in UTF-8, which the residue
// reads as bytes: UTF-8 throughout and no NUL, which a UTF-8 document
// never holds and UTF-16 and UTF-32 ones do.
func plainJSON(raw []byte) bool {
	return bytes.IndexByte(raw, 0) < 0 && utf8.Valid(raw)
}

// decoded is a JSON document in UTF-16 or UTF-32 as UTF-8, with its
// encoding; ok is false when it decodes to no plain text.
func decoded(raw []byte) (encoding string, text []byte, ok bool) {
	encoding, text, err := jsonx.Decode(raw)
	return encoding, text, err == nil && plainJSON(text)
}

// ruled tells the values of a JSON document at root that a rule covers:
// at a path the comparison gives them, where a rule judges them
// (allowlist.List.CoversValue). Inside an array whose items are paired
// by key the comparison names items by their key, not their place, so
// no value there is left out.
type ruled struct {
	root   string
	covers func(path string) bool
	keyed  func(path string) bool
}

// ruled is the values a rule covers of the JSON document at root.
func (c *comparer) ruled(root string) *ruled {
	return &ruled{root: root,
		covers: func(path string) bool { return c.allow.CoversValue(c.id, path) },
		keyed:  func(path string) bool { r := c.allow.ArrayKey(c.id, path); return r != nil && r.Key != "" }}
}

// strings calls f with the source of every string of a JSON document,
// keys and values, without its quotes, apart from the values a rule
// covers.
func (r *ruled) strings(doc []byte, f func([]byte)) { r.walk(doc, f, func(int, int) {}) }

// masked is a JSON document with the strings of the values a rule covers
// written as "": what is left of it besides how they are written.
func (r *ruled) masked(doc []byte) []byte {
	var out []byte
	last := 0
	r.walk(doc, func([]byte) {}, func(i, end int) {
		out = append(append(out, doc[last:i]...), '"', '"')
		last = end
	})
	if out == nil {
		return doc
	}
	return append(out, doc[last:]...)
}

// walk calls f with the source of every string of a JSON document, keys
// and values, without its quotes, and covered with where each string of
// a value a rule covers stands instead. Paths are written into one buffer
// and read in place.
func (r *ruled) walk(doc []byte, f func([]byte), covered func(start, end int)) {
	type frame struct {
		n           int  // the length of the container's path in buf
		object, key bool // an object, and its next string is a key
		at          int  // the item of an array
		name        []byte
		keyed       bool // in an array whose items are paired by key
	}
	var stack []frame
	buf := append([]byte(nil), r.root...)
	// place writes the path of the next value into buf.
	place := func() (string, bool) {
		if len(stack) == 0 {
			buf = append(buf[:0], r.root...)
			return unsafe.String(unsafe.SliceData(buf), len(buf)), false
		}
		top := stack[len(stack)-1]
		buf = buf[:top.n]
		switch {
		case !top.object:
			buf = append(strconv.AppendInt(append(buf, '['), int64(top.at), 10), ']')
		case plainName(top.name):
			buf = append(append(buf, '.'), top.name...)
		default:
			buf = append(buf, jsonx.KeyPath("", string(top.name))...)
		}
		return unsafe.String(unsafe.SliceData(buf), len(buf)), top.keyed
	}
	for i := 0; i < len(doc); {
		if jsonSpace(doc[i]) {
			i++
			continue
		}
		kind, end := token(doc, i)
		switch {
		case kind == '{' || kind == '[':
			path, keyed := place()
			keyed = keyed || kind == '[' && r.keyed(path)
			stack = append(stack, frame{n: len(buf), object: kind == '{', key: kind == '{', keyed: keyed})
		case kind == '}' || kind == ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case kind == ',':
			if len(stack) > 0 {
				top := &stack[len(stack)-1]
				top.at++
				top.key = top.object
			}
		case doc[i] == '"':
			s := doc[i+1 : max(i+1, end-1)]
			if len(stack) > 0 && stack[len(stack)-1].key {
				top := &stack[len(stack)-1]
				top.key, top.name = false, s
				if bytes.IndexByte(s, '\\') >= 0 {
					if v, err := jsonx.Parse(doc[i:end]); err == nil {
						top.name = []byte(v.Text)
					}
				}
				f(s)
				break
			}
			if path, keyed := place(); keyed || !r.covers(path) {
				f(s)
			} else {
				covered(i, end)
			}
		}
		i = end
	}
}

// plainName is a key jsonx.KeyPath writes after a dot.
func plainName(key []byte) bool {
	for _, c := range key {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '$' || c == '-') {
			return false
		}
	}
	return len(key) > 0
}

// slots writes how a devalue payload (__NUXT_DATA__) lays out the value it
// encodes, which the comparison reads decoded: every slot's kind, and of
// a slot that holds an array or an object its references, keys and type
// tag (the first text of an array); a slot no other one refers to with
// its value, since it is in no decoded value. The values of the slots
// are compared decoded, so a value a rule lets change leaves it as it is,
// while the same values in other slots, which development reports as an
// encoding drift, change it.
func slots(raw []byte) []byte {
	root, err := jsonx.Parse(raw)
	if err != nil || root.Kind != jsonx.Array {
		return nil
	}
	used := map[string]bool{"0": true}
	ref := func(v *jsonx.Value) {
		if v.Kind == jsonx.Int {
			used[v.Text] = true
		}
	}
	for _, it := range root.Items {
		for _, x := range it.Items {
			ref(x)
		}
		for _, m := range it.Members {
			ref(m.Value)
		}
	}
	e := &canon{}
	e.list(len(root.Items))
	item := func(i int, v *jsonx.Value) {
		switch {
		case v.Kind == jsonx.Int || i == 0 && v.Kind == jsonx.String:
			e.text(v.ScalarText())
		default:
			e.text(v.Kind.String())
		}
	}
	for i, it := range root.Items {
		e.text(it.Kind.String())
		switch it.Kind {
		case jsonx.Array:
			e.list(len(it.Items))
			for j, x := range it.Items {
				item(j, x)
			}
		case jsonx.Object:
			e.list(len(it.Members))
			for _, m := range it.Members {
				e.text(m.Key)
				item(-1, m.Value)
			}
		default:
			if !used[strconv.Itoa(i)] {
				e.text(it.ScalarText())
			}
		}
	}
	return e.b
}

// csvResidue writes what a CSV file writes beyond its cells.
func csvResidue(body []byte) []byte {
	e := &canon{}
	e.bool(bytes.HasPrefix(body, []byte("\xef\xbb\xbf"))).bytes(csvLayout(body))
	return e.b
}

// spacing writes the whitespace between the tokens of a JSON document
// (what stands around it apart) where it is not what the document's
// layout says (spaced): every such place by its number, with its run.
// With the layout and the tokens it gives the document back; an encoder
// writes what its layout says everywhere, so other values and array
// lengths leave it empty. Nil when every place is as the layout says.
func spacing(doc []byte, s jsonx.Style) []byte {
	if !bytes.ContainsAny(doc, " \t\r\n") && s.Colon == 0 && s.BeforeColon == 0 && s.Comma == 0 {
		return nil // no whitespace, which is what a compact layout says
	}
	var e *canon
	n := 0
	depth, place := 0, 0
	for i := 0; i < len(doc); place++ {
		prev, end := token(doc, i)
		switch prev {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
		j := end
		for j < len(doc) && jsonSpace(doc[j]) {
			j++
		}
		if j >= len(doc) {
			break
		}
		next, _ := token(doc, j)
		if !spaced(doc[end:j], s, prev, next, depth) {
			if e == nil {
				e = &canon{}
			}
			e.int(place).bytes(doc[end:j])
			n++
		}
		i = j
	}
	if e == nil {
		return nil
	}
	return (&canon{}).list(n).bytes(e.b).b
}

// spaced reports whether a run between two tokens at a depth is what a
// layout writes there: around ':' and after ',' on one line its spaces,
// nothing inside an empty object or array, and in a document of several
// lines a line end and the indentation of the depth after an opening
// bracket or a comma and before a closing one.
func spaced(run []byte, s jsonx.Style, prev, next byte, depth int) bool {
	switch {
	case next == ':':
		return spaces(run, s.BeforeColon)
	case prev == ':':
		return spaces(run, s.Colon)
	case (prev == '{' || prev == '[') && (next == '}' || next == ']'):
		return len(run) == 0
	case s.Lines == 0 && prev == ',':
		return spaces(run, s.Comma)
	case s.Lines == 0:
		return len(run) == 0
	case prev == '{' || prev == '[' || prev == ',':
		return line(run, s, depth)
	case next == '}' || next == ']':
		return line(run, s, depth-1)
	}
	return len(run) == 0
}

// spaces reports a run that is what a Style says of the spaces at a
// place: nothing, or one space.
func spaces(run []byte, k uint8) bool {
	switch k {
	case 0:
		return len(run) == 0
	case ' ':
		return len(run) == 1 && run[0] == ' '
	}
	return false
}

// line reports a run that is a line end of the layout and the
// indentation of a depth.
func line(run []byte, s jsonx.Style, depth int) bool {
	switch {
	case s.Lines == '\n' && len(run) > 0 && run[0] == '\n':
		run = run[1:]
	case s.Lines == '\r' && len(run) > 1 && run[0] == '\r' && run[1] == '\n':
		run = run[2:]
	default:
		return false
	}
	if s.Indent == 0 || s.Indent == '?' {
		return len(run) == 0 && s.Indent == 0
	}
	if len(run) != s.Unit*depth {
		return false
	}
	for _, c := range run {
		if c != s.Indent {
			return false
		}
	}
	return true
}

// token reads the JSON token at doc[i]: what it is ({, [, }, ], ',', ':'
// or v for a value) and where it ends.
func token(doc []byte, i int) (byte, int) {
	switch c := doc[i]; c {
	case '{', '[', '}', ']', ',', ':':
		return c, i + 1
	case '"':
		return 'v', min(stringEnd(doc, i+1)+1, len(doc))
	}
	j := i + 1
	for j < len(doc) && !jsonSpace(doc[j]) && doc[j] != ',' && doc[j] != ':' && doc[j] != ']' && doc[j] != '}' {
		j++
	}
	return 'v', j
}

// stringEnd is where the quote that ends the string from doc[i] on
// stands; len(doc) without one.
func stringEnd(doc []byte, i int) int {
	for ; i < len(doc); i++ {
		switch doc[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return len(doc)
}

func jsonSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// escapes writes how the strings of a JSON document (keys and values)
// are escaped: for every character the document escapes the same way
// wherever it stands, that escape; for a character it writes in more
// than one way (escaped and not, é and é), the way of every
// occurrence, by the text of its string, the how-manieth of the strings
// with that text it is, and the how-manieth occurrence in it. With the
// texts of the strings it gives their sources back. Characters are told
// as the parser decodes them: a surrogate pair is one, a lone surrogate
// its three bytes. Nil for a document without escapes.
func escapes(doc []byte, each func(func([]byte))) []byte {
	if bytes.IndexByte(doc, '\\') < 0 {
		return nil
	}
	type way struct {
		escape string
		mixed  bool
	}
	ways := map[rune]way{}
	each(func(s []byte) {
		for i := bytes.IndexByte(s, '\\'); i >= 0 && i < len(s); {
			n, r := escapeAt(s, i)
			if w, ok := ways[r]; !ok {
				ways[r] = way{escape: string(s[i : i+n])}
			} else if !w.mixed && w.escape != string(s[i:i+n]) {
				ways[r] = way{w.escape, true}
			}
			i += n
			if k := bytes.IndexByte(s[i:], '\\'); k >= 0 {
				i += k
			} else {
				break
			}
		}
	})
	if len(ways) == 0 {
		return nil
	}
	// A character also written as it is: in more than one way.
	var ascii [utf8.RuneSelf]bool
	wide := false
	for r := range ways {
		if r >= 0 && r < utf8.RuneSelf {
			ascii[r] = true
		} else {
			wide = true
		}
	}
	mixed := false
	each(func(s []byte) {
		for i := 0; i < len(s); {
			if s[i] == '\\' {
				n, _ := escapeAt(s, i)
				i += n
				continue
			}
			if s[i] < utf8.RuneSelf {
				if ascii[s[i]] {
					if w := ways[rune(s[i])]; !w.mixed {
						ways[rune(s[i])], mixed = way{w.escape, true}, true
					}
				}
				i++
				continue
			}
			r, n := literalAt(s, i)
			if wide {
				if w, ok := ways[r]; ok && !w.mixed {
					ways[r], mixed = way{w.escape, true}, true
				}
			}
			i += n
		}
	})
	runes := make([]rune, 0, len(ways))
	for r, w := range ways {
		mixed = mixed || w.mixed
		if !w.mixed {
			runes = append(runes, r)
		}
	}
	slices.Sort(runes)
	e := &canon{}
	e.list(len(runes))
	for _, r := range runes {
		e.int(int(r)).text(ways[r].escape)
	}
	if !mixed {
		return e.list(0).b
	}
	// The characters written in more than one way, by place.
	type place struct {
		r      rune
		text   string
		nth, k int
		way    []byte
	}
	var places []place
	seen := map[string]int{} // strings by their text, how many so far
	each(func(s []byte) {
		var text string
		counts := map[rune]int{}
		for i := 0; i < len(s); {
			var r rune
			var n int
			if s[i] == '\\' {
				n, r = escapeAt(s, i)
			} else {
				r, n = literalAt(s, i)
			}
			if w, ok := ways[r]; ok && w.mixed {
				if text == "" {
					text = stringText(s)
					seen[text]++
				}
				places = append(places, place{r, text, seen[text], counts[r], s[i : i+n]})
			}
			counts[r]++
			i += n
		}
	})
	e.list(len(places))
	for _, p := range places {
		e.int(int(p.r)).text(p.text).int(p.nth).int(p.k).bytes(p.way)
	}
	return e.b
}

// stringText is the text of a JSON string's source (without its
// quotes), as a hash.
func stringText(s []byte) string {
	v, err := jsonx.Parse(append(append([]byte{'"'}, s...), '"'))
	text := string(s)
	if err == nil {
		text = v.Text
	}
	h := sha256.Sum256([]byte(text))
	return string(h[:])
}

// eachString calls f with the source of every string of a JSON document,
// keys and values, without its quotes.
func eachString(doc []byte, f func([]byte)) {
	for i := 0; i < len(doc); {
		q := bytes.IndexByte(doc[i:], '"')
		if q < 0 {
			return
		}
		start := i + q + 1
		end := stringEnd(doc, start)
		f(doc[start:end])
		i = end + 1
	}
}

// escapeAt reads the escape at s[i]: its length and the character it
// stands for, as the parser decodes it: a pair of surrogates one
// character, a lone surrogate its own.
func escapeAt(s []byte, i int) (n int, r rune) {
	if i+1 >= len(s) {
		return len(s) - i, '\\'
	}
	switch c := s[i+1]; c {
	case 'b':
		return 2, '\b'
	case 'f':
		return 2, '\f'
	case 'n':
		return 2, '\n'
	case 'r':
		return 2, '\r'
	case 't':
		return 2, '\t'
	case 'u':
	default:
		return 2, rune(c)
	}
	u, ok := hex4(s, i+2)
	if !ok {
		return 2, 'u'
	}
	if u >= 0xD800 && u <= 0xDBFF && i+12 <= len(s) && s[i+6] == '\\' && s[i+7] == 'u' {
		if u2, ok := hex4(s, i+8); ok && u2 >= 0xDC00 && u2 <= 0xDFFF {
			return 12, 0x10000 + (u-0xD800)<<10 + (u2 - 0xDC00)
		}
	}
	return 6, u
}

// literalAt reads the character written as it is at s[i], as the parser
// keeps it: a surrogate's three bytes (WTF-8) are one character, a byte
// that is no UTF-8 one of its own (told apart from every character).
func literalAt(s []byte, i int) (rune, int) {
	if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
		return 0xD000 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F), 3
	}
	r, n := utf8.DecodeRune(s[i:])
	if r == utf8.RuneError && n == 1 {
		return -1 - rune(s[i]), 1
	}
	return r, n
}

func hex4(s []byte, at int) (rune, bool) {
	if at+4 > len(s) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(s[at:at+4]), 16, 32)
	return rune(n), err == nil
}
