package jsonx

import (
	"bytes"
	"fmt"
)

// ScalarText is the text of a scalar as Python's json.dumps(v, ensure_ascii=False)
// writes it: numbers by their literal text, strings quoted and minimally escaped.
// Byte-length bookkeeping in the comparison depends on exactly this form.
func (v *Value) ScalarText() string {
	switch v.Kind {
	case Null:
		return "null"
	case Bool:
		if v.Bool {
			return "true"
		}
		return "false"
	case Int, Float, Const:
		return v.Text
	case String:
		return Quote(v.Text)
	}
	return ""
}

// FormatText is what an allow-list format is matched against: strings without
// their quotes, every other scalar as its ScalarText.
func (v *Value) FormatText() string {
	if v.Kind == String {
		return v.Text
	}
	return v.ScalarText()
}

// Quote escapes s like Python's json encoder with ensure_ascii=False.
func Quote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		default:
			if c < 0x20 {
				out = append(out, fmt.Sprintf("\\u%04x", c)...)
			} else {
				out = append(out, c)
			}
		}
	}
	return string(append(out, '"'))
}

// Layout returns the distinct runs of whitespace outside strings. Two
// documents written by the same encoder have the same layout even when they
// hold different data; compact and pretty-printed output do not.
func Layout(raw []byte) map[string]bool {
	runs := map[string]bool{}
	inString, escaped := false, false
	start := -1
	for i, c := range raw {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		isSpace := c == ' ' || c == '\t' || c == '\n' || c == '\r'
		if isSpace && start < 0 {
			start = i
		}
		if !isSpace && start >= 0 {
			runs[string(raw[start:i])] = true
			start = -1
		}
		if c == '"' {
			inString = true
		}
	}
	if start >= 0 {
		runs[string(raw[start:])] = true
	}
	return runs
}

// Style is how a document is laid out, apart from what it holds: how lines
// end, what one level of indentation is, and the spaces around ':' and
// after ','. Two documents written by the same encoder have the same style,
// however deep they nest and however much they hold; compact and
// pretty-printed output, or another indentation, do not.
type Style struct {
	Lines  uint8 // line ends: 0 none, '\n', '\r' for CRLF, '?' for a mix or a lone CR
	Indent uint8 // the indentation character (' ', '\t'; '?' for a mix); 0 for none
	Unit   int   // the first indentation's width: one level
	// What follows ':' and what precedes it, and what follows ',' on the
	// same line, the first time: 0 nothing, ' ' one space, '?' anything else.
	Colon, BeforeColon, Comma uint8
	Outside                   uint8 // whitespace before or after the document: 0 none, else as Lines/space
}

// StyleOf reads the style of a document in one pass, without allocating.
func StyleOf(raw []byte) Style {
	var s Style
	seenColon, seenBefore, seenComma := false, false, false
	kind := func(run []byte) uint8 {
		switch {
		case len(run) == 0:
			return 0
		case len(run) == 1 && run[0] == ' ':
			return ' '
		}
		return '?'
	}
	inString, escaped := false, false
	var prev byte // the last byte outside whitespace and strings
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
				prev = '"'
			}
			continue
		}
		if !isSpace(c) {
			if c == '"' {
				inString = true
			}
			switch {
			case c == ':':
				seenBefore = true
			case prev == ':':
				seenColon = true // the first ':' is behind
			case prev == ',':
				seenComma = true
			}
			prev = c
			continue
		}
		start := i
		for i < len(raw) && isSpace(raw[i]) {
			i++
		}
		run := raw[start:i]
		i--
		switch {
		case start == 0 || i == len(raw)-1:
			if s.Outside == 0 {
				s.Outside = lineEnd(run, ' ')
			}
			continue
		case !seenBefore && i+1 < len(raw) && raw[i+1] == ':':
			s.BeforeColon, seenBefore = kind(run), true
		}
		nl := bytes.IndexByte(run, '\n')
		if nl < 0 && bytes.IndexByte(run, '\r') < 0 {
			switch {
			case prev == ':' && !seenColon:
				s.Colon = kind(run)
			case prev == ',' && !seenComma:
				s.Comma = kind(run)
			}
			continue
		}
		if e := lineEnd(run, 0); s.Lines == 0 {
			s.Lines = e
		} else if e != s.Lines {
			s.Lines = '?'
		}
		indent := run[bytes.LastIndexAny(run, "\r\n")+1:]
		if len(indent) == 0 || s.Indent == '?' {
			continue
		}
		ch := indent[0]
		for _, b := range indent {
			if b != ch {
				ch = '?'
			}
		}
		switch {
		case s.Indent == 0:
			s.Indent, s.Unit = ch, len(indent)
		case ch != s.Indent:
			s.Indent = '?'
		}
	}
	return s
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// lineEnd tells how a run of whitespace ends its lines ('\n', '\r' for
// CRLF, '?' for a mix or a lone CR); without one, other.
func lineEnd(run []byte, other uint8) uint8 {
	var e uint8
	for i, b := range run {
		var k uint8
		switch {
		case b == '\r' && i+1 < len(run) && run[i+1] == '\n':
			k = '\r'
		case b == '\r':
			k = '?'
		case b == '\n' && (i == 0 || run[i-1] != '\r'):
			k = '\n'
		default:
			continue
		}
		if e != 0 && e != k {
			return '?'
		}
		e = k
	}
	if e == 0 {
		return other
	}
	return e
}
