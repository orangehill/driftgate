package jsonx

import "strings"

// Marshal writes v back as JSON, keeping key order, duplicates and number
// literals. With spaced set it adds a space after ':' and ',', which changes
// the bytes but not a single value.
func Marshal(v *Value, spaced bool) []byte {
	var b strings.Builder
	write(&b, v, spaced)
	return []byte(b.String())
}

func write(b *strings.Builder, v *Value, spaced bool) {
	sep, colon := ",", ":"
	if spaced {
		sep, colon = ", ", ": "
	}
	switch v.Kind {
	case Object:
		b.WriteByte('{')
		for i, m := range v.Members {
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(Quote(m.Key))
			b.WriteString(colon)
			write(b, m.Value, spaced)
		}
		b.WriteByte('}')
	case Array:
		b.WriteByte('[')
		for i, item := range v.Items {
			if i > 0 {
				b.WriteString(sep)
			}
			write(b, item, spaced)
		}
		b.WriteByte(']')
	default:
		b.WriteString(v.ScalarText())
	}
}

// Clone returns a deep copy of v.
func (v *Value) Clone() *Value {
	c := *v
	c.Src = nil // a copy is made to be changed: the source would not describe it
	if v.Items != nil {
		c.Items = make([]*Value, len(v.Items))
		for i, item := range v.Items {
			c.Items[i] = item.Clone()
		}
	}
	if v.Members != nil {
		c.Members = make([]Member, len(v.Members))
		for i, m := range v.Members {
			c.Members[i] = Member{Key: m.Key, Value: m.Value.Clone()}
		}
	}
	return &c
}
