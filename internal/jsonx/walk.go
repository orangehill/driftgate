package jsonx

// Visitor receives the values of a document from Walk, in source order.
type Visitor interface {
	// Member starts the value of an object member, Item the value of an
	// array item; End follows that value.
	Member(key string)
	Item(index int)
	End()
	// Scalar is a value that is not a container. Text is as in Value: the
	// literal of a number or a constant, the decoded string; "" for null
	// and booleans.
	Scalar(kind Kind, text string)
}

// Keys and texts a Visitor gets are valid until Walk returns, while the
// document is not changed: most share the document's memory instead of
// being copied out of it (a scan of a body of many megabytes would copy
// every string). A visitor that keeps one longer clones it.

// Walk reads a document as Parse does, with the same grammar and errors,
// but hands its values to v instead of building them: a scan of a large
// body then holds no tree, which takes many times the body's size. After a
// syntax error v has seen the values before it; the caller drops them.
func Walk(raw []byte, v Visitor) error {
	text, err := decodeText(raw)
	if err != nil {
		return err
	}
	p := &parser{s: text, view: true}
	p.skipSpace()
	if err := p.walk(0, v); err != nil {
		return err
	}
	p.skipSpace()
	if p.i != len(p.s) {
		return p.fail("extra data")
	}
	return nil
}

func (p *parser) walk(depth int, v Visitor) error {
	if depth > MaxDepth {
		return p.fail("nesting too deep")
	}
	if p.i >= len(p.s) {
		return p.fail("expecting value")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.walkObject(depth, v)
	case c == '[':
		return p.walkArray(depth, v)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return err
		}
		v.Scalar(String, s)
	case p.has("null"):
		p.i += 4
		v.Scalar(Null, "")
	case p.has("true"):
		p.i += 4
		v.Scalar(Bool, "")
	case p.has("false"):
		p.i += 5
		v.Scalar(Bool, "")
	case p.has("NaN"):
		p.i += 3
		v.Scalar(Const, "NaN")
	case p.has("Infinity"):
		p.i += 8
		v.Scalar(Const, "Infinity")
	case p.has("-Infinity"):
		p.i += 9
		v.Scalar(Const, "-Infinity")
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.i
		kind, err := p.numberEnd()
		if err != nil {
			return err
		}
		v.Scalar(kind, p.text(start, p.i))
	default:
		return p.fail("expecting value")
	}
	return nil
}

func (p *parser) walkObject(depth int, v Visitor) error {
	p.i++
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return nil
	}
	for {
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return p.fail("expecting property name enclosed in double quotes")
		}
		key, err := p.str()
		if err != nil {
			return err
		}
		p.skipSpace()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return p.fail("expecting ':' delimiter")
		}
		p.i++
		p.skipSpace()
		v.Member(key)
		if err := p.walk(depth+1, v); err != nil {
			return err
		}
		v.End()
		p.skipSpace()
		if p.i >= len(p.s) {
			return p.fail("expecting ',' delimiter")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return nil
		default:
			return p.fail("expecting ',' delimiter")
		}
	}
}

func (p *parser) walkArray(depth int, v Visitor) error {
	p.i++
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return nil
	}
	for i := 0; ; i++ {
		p.skipSpace()
		v.Item(i)
		if err := p.walk(depth+1, v); err != nil {
			return err
		}
		v.End()
		p.skipSpace()
		if p.i >= len(p.s) {
			return p.fail("expecting ',' delimiter")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return nil
		default:
			return p.fail("expecting ',' delimiter")
		}
	}
}

// Valid reports whether raw is one JSON document as Parse reads it, without
// building its values.
func Valid(raw []byte) bool { return Walk(raw, nop{}) == nil }

type nop struct{}

func (nop) Member(string)       {}
func (nop) Item(int)            {}
func (nop) End()                {}
func (nop) Scalar(Kind, string) {}
