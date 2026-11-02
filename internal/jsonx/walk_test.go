package jsonx

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// events records what Walk hands a Visitor.
type events struct{ out []string }

func (e *events) Member(key string) { e.out = append(e.out, "member "+key) }
func (e *events) Item(index int)    { e.out = append(e.out, fmt.Sprint("item ", index)) }
func (e *events) End()              { e.out = append(e.out, "end") }
func (e *events) Scalar(kind Kind, text string) {
	e.out = append(e.out, kind.String()+" "+text)
}

// treeEvents lists the same events from a parsed tree.
func treeEvents(v *Value, out []string) []string {
	switch v.Kind {
	case Object:
		for _, m := range v.Members {
			out = append(out, "member "+m.Key)
			out = treeEvents(m.Value, out)
			out = append(out, "end")
		}
	case Array:
		for i, it := range v.Items {
			out = append(out, fmt.Sprint("item ", i))
			out = treeEvents(it, out)
			out = append(out, "end")
		}
	case Null, Bool:
		out = append(out, v.Kind.String()+" ")
	default:
		out = append(out, v.Kind.String()+" "+v.Text)
	}
	return out
}

// Walk accepts and rejects what Parse does, with the same error, and hands
// out the values of the tree Parse builds, in the same order.
func TestWalkMatchesParse(t *testing.T) {
	inputs := []string{
		`1`, `-0`, `1.5e3`, `1E+5`, `"a"`, `""`, `null`, `true`, `false`, `NaN`, `Infinity`, `-Infinity`,
		`{}`, `[]`, ` { "a" : [ 1 , 2.0 , { "b" : null } ] , "a" : "dup" } `,
		`{"x":"é\n\"\\\/","y":"😀","z":"\ud801x","é":[[],{}]}`,
		`[{"id":12,"mail":"pera@example.com","tags":["a","b"],"n":{"m":true}}]`,
		"\xEF\xBB\xBF[1]", string([]byte{'[', 0, '1', 0, ']', 0}),
		strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1),
		strings.Repeat("[", 50) + "1" + strings.Repeat("]", 50),
		// rejected
		"", "01", "1.", "[1,]", `{"a":1,}`, `{'a':1}`, "\"a\x01\"", `"\x"`, "tru", "1 2", "-", `"\u12"`, "\xff",
		`{"a"}`, `{"a":}`, `[1 2]`, `{"a":1 "b":2}`, `[`, `{"a":[1,{"b":"c"}`, `"abc`,
	}
	for _, in := range inputs {
		tree, perr := Parse([]byte(in))
		var e events
		werr := Walk([]byte(in), &e)
		if fmt.Sprint(perr) != fmt.Sprint(werr) {
			t.Errorf("%.40q: Parse error %v, Walk error %v", in, perr, werr)
			continue
		}
		if perr != nil {
			continue
		}
		if want := treeEvents(tree, nil); strings.Join(want, "|") != strings.Join(e.out, "|") {
			t.Errorf("%.40q: Walk %q, tree %q", in, e.out, want)
		}
	}
}

type discard struct{}

func (discard) Member(string)       {}
func (discard) Item(int)            {}
func (discard) End()                {}
func (discard) Scalar(Kind, string) {}

// Walk builds no tree: it allocates a small part of what Parse does.
func TestWalkMemory(t *testing.T) {
	doc := memDoc(5000)
	allocs := func(f func()) uint64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	walked := allocs(func() {
		if err := Walk(doc, discard{}); err != nil {
			t.Fatal(err)
		}
	})
	if walked > 2*uint64(len(doc)) {
		t.Fatalf("%d bytes allocated to walk %d bytes", walked, len(doc))
	}
}

// A binary body with the NUL bytes of UTF-16 or UTF-32 (an icon, a font)
// is not decoded into a copy several times its size before it fails: it
// fails at once, as it would after the decoding. UTF-16 and UTF-32 JSON
// without a byte order mark still decodes.
func TestBinaryWithNULs(t *testing.T) {
	body := make([]byte, 4<<20)
	for i := range body {
		body[i] = byte(i*7919>>3) | 0x80
	}
	for _, head := range [][]byte{{0x89, 0}, {0, 0x89}, {0x89, 0, 0, 0}, {0, 0, 0, 0x89}} {
		copy(body, head)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := Walk(body, nopVisitor{})
		runtime.ReadMemStats(&after)
		var se *SyntaxError
		if !errors.As(err, &se) || se.Offset != 0 {
			t.Fatalf("% x: %v", head, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<10 {
			t.Fatalf("% x: %d KiB allocated", head, grew>>10)
		}
	}
	for _, doc := range [][]byte{
		{'{', 0, '}', 0},
		{0, '[', 0, ']'},
		{' ', 0, 0, 0, '1', 0, 0, 0},
		{0, 0, 0, '"', 0, 0, 0, 'a', 0, 0, 0, '"'},
	} {
		if _, err := Parse(doc); err != nil {
			t.Fatalf("% x: %v", doc, err)
		}
	}
}

type nopVisitor struct{}

func (nopVisitor) Member(string)       {}
func (nopVisitor) Item(int)            {}
func (nopVisitor) End()                {}
func (nopVisitor) Scalar(Kind, string) {}
