// Package jsonx parses JSON without losing anything a regression gate must see:
// key order, duplicate keys and the literal text of every number. Strings are
// decoded; lone UTF-16 surrogates are kept (encoded as WTF-8) so that two
// different inputs never decode to the same value.
package jsonx

// Kind is the type of a JSON value. Integers and non-integers are separate
// kinds, so 1 and 1.0 differ in type, as in the reference implementation.
// One byte: next to Bool it keeps a Value in a smaller size class.
type Kind uint8

const (
	Null Kind = iota
	Bool
	Int
	Float
	Const // NaN, Infinity, -Infinity (accepted by the reference parser)
	String
	Array
	Object
)

func (k Kind) String() string {
	switch k {
	case Null:
		return "null"
	case Bool:
		return "bool"
	case Int:
		return "int"
	case Float:
		return "float"
	case Const:
		return "const"
	case String:
		return "string"
	case Array:
		return "array"
	case Object:
		return "object"
	}
	return "unknown"
}

// Member is one key/value pair of an object, in source order.
type Member struct {
	Key   string
	Value *Value
}

// Value is a parsed JSON value.
type Value struct {
	Kind    Kind
	Bool    bool
	Text    string // number literal text (Int, Float, Const) or decoded string (String)
	Raw     string // source text of a string, quotes and escapes included
	Items   []*Value
	Members []Member
	// Src is the source text of a parsed object or array (a slice of the
	// document, not a copy); nil for a value made by hand.
	Src []byte
}

// Equal reports whether two scalars are identical: same kind and, for numbers,
// the same literal text. Containers are never equal here; callers walk them.
func (v *Value) Equal(o *Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case Null:
		return true
	case Bool:
		return v.Bool == o.Bool
	case Int, Float, Const, String:
		return v.Text == o.Text
	}
	return false
}

// Keys returns the object's keys in source order, duplicates included.
func (v *Value) Keys() []string {
	keys := make([]string, len(v.Members))
	for i, m := range v.Members {
		keys[i] = m.Key
	}
	return keys
}
