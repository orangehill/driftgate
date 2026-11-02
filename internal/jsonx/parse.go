package jsonx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// MaxDepth bounds nesting so hostile input cannot exhaust memory.
const MaxDepth = 10000

// SyntaxError reports where parsing failed.
type SyntaxError struct {
	Offset int
	Msg    string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("json: %s at offset %d", e.Msg, e.Offset) }

// Parse decodes one JSON document. The grammar matches Python's json module
// with its defaults (strict strings, NaN/Infinity accepted, encoding detected
// from the first bytes), because that is the reference implementation.
func Parse(raw []byte) (*Value, error) {
	text, err := decodeText(raw)
	if err != nil {
		return nil, err
	}
	p := &parser{s: text}
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.i != len(p.s) {
		return nil, p.fail("extra data")
	}
	return v, nil
}

var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16BE = []byte{0xFE, 0xFF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF32BE = []byte{0x00, 0x00, 0xFE, 0xFF}
	bomUTF32LE = []byte{0xFF, 0xFE, 0x00, 0x00}
)

// decodeText returns the document as UTF-8, detecting the encoding the way
// Python's json.detect_encoding does.
func decodeText(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, bomUTF32BE):
		return fromUTF32(b[4:], binary.BigEndian)
	case bytes.HasPrefix(b, bomUTF32LE):
		return fromUTF32(b[4:], binary.LittleEndian)
	case bytes.HasPrefix(b, bomUTF16BE):
		return fromUTF16(b[2:], binary.BigEndian)
	case bytes.HasPrefix(b, bomUTF16LE):
		return fromUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, bomUTF8):
		b = b[3:]
	case len(b) >= 4 && b[0] == 0:
		if b[1] != 0 {
			return fromUTF16If(b, b[1], binary.BigEndian)
		}
		return fromUTF32If(b, b[3], binary.BigEndian)
	case len(b) >= 4 && b[1] == 0:
		if b[2] != 0 || b[3] != 0 {
			return fromUTF16If(b, b[0], binary.LittleEndian)
		}
		return fromUTF32If(b, b[0], binary.LittleEndian)
	case len(b) == 2 && b[0] == 0:
		return fromUTF16If(b, b[1], binary.BigEndian)
	case len(b) == 2 && b[1] == 0:
		return fromUTF16If(b, b[0], binary.LittleEndian)
	}
	if !utf8.Valid(b) {
		return nil, &SyntaxError{0, "invalid UTF-8"}
	}
	return b, nil
}

// jsonStart reports a character a document may start with: white space or
// the first character of a value. The NUL bytes of UTF-16 and UTF-32
// without a byte order mark are also in many binary formats (an icon, a
// font); such a body is not decoded, whatever its size, when its first
// character could not start a document anyway: it fails as it would after
// the decoding, at offset 0.
func jsonStart(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '{', '[', '"', '-', 't', 'f', 'n', 'N', 'I':
		return true
	}
	return c >= '0' && c <= '9'
}

func fromUTF16If(b []byte, first byte, order binary.ByteOrder) ([]byte, error) {
	if !jsonStart(first) {
		return nil, &SyntaxError{0, "expecting value"}
	}
	return fromUTF16(b, order)
}

func fromUTF32If(b []byte, first byte, order binary.ByteOrder) ([]byte, error) {
	if !jsonStart(first) {
		return nil, &SyntaxError{0, "expecting value"}
	}
	return fromUTF32(b, order)
}

func fromUTF16(b []byte, order binary.ByteOrder) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, &SyntaxError{0, "truncated UTF-16"}
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = order.Uint16(b[2*i:])
	}
	var out []byte
	for i := 0; i < len(units); i++ {
		u := units[i]
		if utf16.IsSurrogate(rune(u)) {
			if u < 0xDC00 && i+1 < len(units) && units[i+1] >= 0xDC00 && units[i+1] <= 0xDFFF {
				out = utf8.AppendRune(out, utf16.DecodeRune(rune(u), rune(units[i+1])))
				i++
				continue
			}
			return nil, &SyntaxError{2 * i, "invalid UTF-16"}
		}
		out = utf8.AppendRune(out, rune(u))
	}
	return out, nil
}

func fromUTF32(b []byte, order binary.ByteOrder) ([]byte, error) {
	if len(b)%4 != 0 {
		return nil, &SyntaxError{0, "truncated UTF-32"}
	}
	var out []byte
	for i := 0; i < len(b); i += 4 {
		r := rune(order.Uint32(b[i:]))
		if !utf8.ValidRune(r) {
			return nil, &SyntaxError{i, "invalid UTF-32"}
		}
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}

// The children of the objects and arrays being parsed wait on the stacks
// members and items, and each container takes a slice of exactly its size:
// a slice grown by append keeps up to twice the room it needs. buf holds the
// string being decoded; strings are copied out of it at their size. After a
// syntax error the parser is not used again, so the stacks need no cleanup.
type parser struct {
	s       []byte
	i       int
	members []Member
	items   []*Value
	buf     []byte
	view    bool // texts share s (Walk) instead of being copied out of it
}

// text is s[i:j] as a string: a view of s for Walk, else a copy.
func (p *parser) text(i, j int) string {
	if p.view {
		return unsafe.String(unsafe.SliceData(p.s[i:j]), j-i)
	}
	return string(p.s[i:j])
}

func (p *parser) fail(msg string) error { return &SyntaxError{p.i, msg} }

func (p *parser) skipSpace() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) has(lit string) bool {
	return bytes.HasPrefix(p.s[p.i:], []byte(lit))
}

func (p *parser) value(depth int) (*Value, error) {
	if depth > MaxDepth {
		return nil, p.fail("nesting too deep")
	}
	if p.i >= len(p.s) {
		return nil, p.fail("expecting value")
	}
	switch c := p.s[p.i]; {
	case c == '{' || c == '[':
		start := p.i
		var v *Value
		var err error
		if c == '{' {
			v, err = p.object(depth)
		} else {
			v, err = p.array(depth)
		}
		if err != nil {
			return nil, err
		}
		v.Src = p.s[start:p.i:p.i]
		return v, nil
	case c == '"':
		start := p.i
		if end := p.plainEnd(); end >= 0 {
			// Without escapes the text is the source without its quotes:
			// one string serves both.
			raw := string(p.s[start : end+1])
			p.i = end + 1
			return &Value{Kind: String, Text: raw[1 : len(raw)-1], Raw: raw}, nil
		}
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Value{Kind: String, Text: s, Raw: string(p.s[start:p.i])}, nil
	case p.has("null"):
		p.i += 4
		return &Value{Kind: Null}, nil
	case p.has("true"):
		p.i += 4
		return &Value{Kind: Bool, Bool: true}, nil
	case p.has("false"):
		p.i += 5
		return &Value{Kind: Bool}, nil
	case p.has("NaN"):
		p.i += 3
		return &Value{Kind: Const, Text: "NaN"}, nil
	case p.has("Infinity"):
		p.i += 8
		return &Value{Kind: Const, Text: "Infinity"}, nil
	case p.has("-Infinity"):
		p.i += 9
		return &Value{Kind: Const, Text: "-Infinity"}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, p.fail("expecting value")
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// number follows -?(0|[1-9]\d*)(\.\d+)?([eE][-+]?\d+)?; an optional part that
// does not match completely is left unread, so the caller reports extra data.
func (p *parser) number() (*Value, error) {
	start := p.i
	kind, err := p.numberEnd()
	if err != nil {
		return nil, err
	}
	return &Value{Kind: kind, Text: string(p.s[start:p.i])}, nil
}

// numberEnd reads the number at p.i (see number) and returns its kind.
func (p *parser) numberEnd() (Kind, error) {
	j := p.i
	if j < len(p.s) && p.s[j] == '-' {
		j++
	}
	switch {
	case j < len(p.s) && p.s[j] == '0':
		j++
	case j < len(p.s) && p.s[j] >= '1' && p.s[j] <= '9':
		for j < len(p.s) && isDigit(p.s[j]) {
			j++
		}
	default:
		return 0, p.fail("expecting value")
	}
	kind := Int
	if j+1 < len(p.s) && p.s[j] == '.' && isDigit(p.s[j+1]) {
		j += 2
		for j < len(p.s) && isDigit(p.s[j]) {
			j++
		}
		kind = Float
	}
	if j < len(p.s) && (p.s[j] == 'e' || p.s[j] == 'E') {
		k := j + 1
		if k < len(p.s) && (p.s[k] == '+' || p.s[k] == '-') {
			k++
		}
		if k < len(p.s) && isDigit(p.s[k]) {
			for k < len(p.s) && isDigit(p.s[k]) {
				k++
			}
			j = k
			kind = Float
		}
	}
	p.i = j
	return kind, nil
}

var errUnterminated = errors.New("unterminated string")

// plainEnd is the index of the quote that closes the string at p.i when
// the string has no escape and no control character, else -1.
func (p *parser) plainEnd() int {
	for j := p.i + 1; j < len(p.s); j++ {
		switch c := p.s[j]; {
		case c == '"':
			return j
		case c == '\\' || c < 0x20:
			return -1
		}
	}
	return -1
}

func (p *parser) str() (string, error) {
	if end := p.plainEnd(); end >= 0 {
		s := p.text(p.i+1, end)
		p.i = end + 1
		return s, nil
	}
	p.i++ // opening quote
	out := p.buf[:0]
	for {
		if p.i >= len(p.s) {
			return "", &SyntaxError{p.i, errUnterminated.Error()}
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			p.buf = out
			return string(out), nil
		case c < 0x20:
			return "", p.fail("invalid control character in string")
		case c != '\\':
			out = append(out, c)
			p.i++
			continue
		}
		// escape
		if p.i+1 >= len(p.s) {
			return "", &SyntaxError{p.i, errUnterminated.Error()}
		}
		e := p.s[p.i+1]
		p.i += 2
		switch e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			u, ok := p.hex4(p.i)
			if !ok {
				return "", p.fail("invalid \\uXXXX escape")
			}
			p.i += 4
			r := rune(u)
			if u >= 0xD800 && u <= 0xDBFF && p.i+1 < len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
				if u2, ok := p.hex4(p.i + 2); ok && u2 >= 0xDC00 && u2 <= 0xDFFF {
					r = utf16.DecodeRune(rune(u), rune(u2))
					p.i += 6
				}
			}
			out = appendWTF8(out, r)
		default:
			p.i -= 2
			return "", p.fail("invalid escape")
		}
	}
}

func (p *parser) hex4(at int) (int, bool) {
	if at+4 > len(p.s) {
		return 0, false
	}
	n := 0
	for _, c := range p.s[at : at+4] {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return n, true
}

// appendWTF8 encodes r as UTF-8, and a lone surrogate as its 3-byte form.
func appendWTF8(b []byte, r rune) []byte {
	if r >= 0xD800 && r <= 0xDFFF {
		return append(b, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

func (p *parser) object(depth int) (*Value, error) {
	p.i++
	v := &Value{Kind: Object, Members: []Member{}}
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return v, nil
	}
	base := len(p.members)
	for {
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail("expecting property name enclosed in double quotes")
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail("expecting ':' delimiter")
		}
		p.i++
		p.skipSpace()
		item, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		p.members = append(p.members, Member{Key: key, Value: item})
		p.skipSpace()
		if p.i >= len(p.s) {
			return nil, p.fail("expecting ',' delimiter")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			v.Members = append(make([]Member, 0, len(p.members)-base), p.members[base:]...)
			p.members = p.members[:base]
			return v, nil
		default:
			return nil, p.fail("expecting ',' delimiter")
		}
	}
}

func (p *parser) array(depth int) (*Value, error) {
	p.i++
	v := &Value{Kind: Array, Items: []*Value{}}
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return v, nil
	}
	base := len(p.items)
	for {
		p.skipSpace()
		item, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		p.items = append(p.items, item)
		p.skipSpace()
		if p.i >= len(p.s) {
			return nil, p.fail("expecting ',' delimiter")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			v.Items = append(make([]*Value, 0, len(p.items)-base), p.items[base:]...)
			p.items = p.items[:base]
			return v, nil
		default:
			return nil, p.fail("expecting ',' delimiter")
		}
	}
}
