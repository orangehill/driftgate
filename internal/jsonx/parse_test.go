package jsonx

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

func mustParse(t *testing.T, s string) *Value {
	t.Helper()
	v, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return v
}

func TestNumbersKeepKindAndText(t *testing.T) {
	cases := map[string]Kind{"1": Int, "-0": Int, "1.0": Float, "1e5": Float, "1E+5": Float, "1.50": Float, "123456789012345678901234567890": Int}
	for in, kind := range cases {
		v := mustParse(t, in)
		if v.Kind != kind || v.Text != in {
			t.Errorf("%s: got %s %q", in, v.Kind, v.Text)
		}
	}
	if mustParse(t, "1").Equal(mustParse(t, "1.0")) {
		t.Error("1 and 1.0 must differ")
	}
	if mustParse(t, "1.0").Equal(mustParse(t, "1.00")) {
		t.Error("1.0 and 1.00 must differ")
	}
}

func TestConstants(t *testing.T) {
	for _, in := range []string{"NaN", "Infinity", "-Infinity"} {
		if v := mustParse(t, in); v.Kind != Const || v.Text != in {
			t.Errorf("%s: got %s %q", in, v.Kind, v.Text)
		}
	}
}

func TestObjectOrderAndDuplicates(t *testing.T) {
	v := mustParse(t, `{"b":1,"a":2,"b":3}`)
	keys := v.Keys()
	if len(keys) != 3 || keys[0] != "b" || keys[1] != "a" || keys[2] != "b" {
		t.Fatalf("keys: %v", keys)
	}
	if v.Members[2].Value.Text != "3" {
		t.Fatal("duplicate value lost")
	}
}

func TestStrings(t *testing.T) {
	if v := mustParse(t, `"a\/bé😀"`); v.Text != "a/bé😀" {
		t.Errorf("got %q", v.Text)
	}
	lone := mustParse(t, `"\ud800x"`)
	other := mustParse(t, `"\ud801x"`)
	if lone.Equal(other) || lone.Text == "�x" {
		t.Error("lone surrogates must stay distinct")
	}
}

func TestRejects(t *testing.T) {
	for _, in := range []string{"", "01", "1.", "[1,]", `{"a":1,}`, `{'a':1}`, "\"a\x01\"", `"\x"`, "tru", "1 2", "-", `"\u12"`, "\xff"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestEncodings(t *testing.T) {
	if v, err := Parse([]byte("\xEF\xBB\xBF[1]")); err != nil || v.Kind != Array {
		t.Errorf("UTF-8 BOM: %v", err)
	}
	if v, err := Parse([]byte{'[', 0, '1', 0, ']', 0}); err != nil || len(v.Items) != 1 {
		t.Errorf("UTF-16LE: %v", err)
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{"a\"b": `"a\"b"`, "é/\n": `"é/\n"`, "\x01": `"\u0001"`, "\x7f": "\"\x7f\""}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{`{"a":[1,2.5,"x",null,true]}`, `[]`, `"😀"`, `-1e-5`, `NaN`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Parse(data)
		if json.Valid(data) && utf8.Valid(data) && err != nil {
			t.Fatalf("valid JSON rejected: %q: %v", data, err)
		}
		if err == nil && v == nil {
			t.Fatal("nil value without error")
		}
		var e events
		werr := Walk(data, &e)
		if fmt.Sprint(err) != fmt.Sprint(werr) {
			t.Fatalf("%q: Parse error %v, Walk error %v", data, err, werr)
		}
		if err == nil && strings.Join(treeEvents(v, nil), "|") != strings.Join(e.out, "|") {
			t.Fatalf("%q: Walk differs from Parse", data)
		}
	})
}

// Objects and arrays keep their source text; strings with and without
// escapes decode as before.
func TestSourceAndStrings(t *testing.T) {
	const doc = ` {"a" : [1, {"x":"plain"}], "bé":"q\"é\n", "c":{}} `
	v, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a := v.Members[0].Value
	if string(v.Src) != `{"a" : [1, {"x":"plain"}], "bé":"q\"é\n", "c":{}}` || string(a.Src) != `[1, {"x":"plain"}]` || string(a.Items[1].Src) != `{"x":"plain"}` {
		t.Fatalf("source: %q %q", v.Src, a.Src)
	}
	if s := a.Items[1].Members[0].Value; s.Text != "plain" || s.Raw != `"plain"` {
		t.Fatalf("plain string: %q %q", s.Text, s.Raw)
	}
	if m := v.Members[1]; m.Key != "bé" || m.Value.Text != "q\"é\n" || m.Value.Raw != `"q\"é\n"` {
		t.Fatalf("escaped: %q %q %q", m.Key, m.Value.Text, m.Value.Raw)
	}
	if v.Clone().Src != nil {
		t.Fatal("a clone keeps the source")
	}
	if _, err := Parse([]byte("[\"a\tb\"]")); err == nil {
		t.Fatal("control character accepted")
	}
}

// memDoc is a body like an API's: a list of records with escaped strings
// (json_encode writes non-ASCII as \uXXXX), numbers and nested objects.
func memDoc(n int) []byte {
	var b []byte
	b = append(b, `{"data":[`...)
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, `{"id":%d,"name":"Petrović %d","city":"Niš","tags":["a","b\"c"],"score":%d.5,"meta":{"ok":true,"note":null}}`, i, i, i)
	}
	return append(b, `]}`...)
}

// Every object and array holds its children in a slice of exactly their
// number, and decoded strings do not share the buffer they were decoded in.
func TestParseExactSlices(t *testing.T) {
	v := mustParse(t, string(memDoc(50)))
	var walk func(v *Value)
	walk = func(v *Value) {
		if cap(v.Items) != len(v.Items) || cap(v.Members) != len(v.Members) {
			t.Fatalf("%s: %d/%d items, %d/%d members", v.Kind, len(v.Items), cap(v.Items), len(v.Members), cap(v.Members))
		}
		for _, it := range v.Items {
			walk(it)
		}
		for _, m := range v.Members {
			walk(m.Value)
		}
	}
	walk(v)
	list := v.Members[0].Value.Items
	for i, it := range list {
		if want := fmt.Sprintf("Petrović %d", i); it.Members[1].Value.Text != want || it.Members[2].Value.Text != "Niš" {
			t.Fatalf("record %d: %q %q", i, it.Members[1].Value.Text, it.Members[2].Value.Text)
		}
		if tags := it.Members[3].Value; len(tags.Items) != 2 || tags.Items[1].Text != `b"c` {
			t.Fatalf("record %d: tags %v", i, tags.Items)
		}
	}
	if s := mustParse(t, `[[], {}, [[]], {"a": {}}]`); len(s.Items) != 4 || s.Items[0].Items == nil || s.Items[1].Members == nil {
		t.Fatalf("empty containers: %+v", s.Items)
	}
}

// A parsed document takes a bounded multiple of its size (a memory
// regression check; tree nodes and their slices dominate).
func TestParseMemory(t *testing.T) {
	doc := memDoc(5000)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	v, err := Parse(doc)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(v)
	// About 13.5 times the size of this document, whose values are small;
	// 17 times with slices grown by append and a 128-byte Value.
	if got := after.TotalAlloc - before.TotalAlloc; got > 15*uint64(len(doc)) {
		t.Fatalf("%d bytes allocated to parse %d bytes", got, len(doc))
	}
}

// The style of a document does not depend on how deep it nests or what it
// holds; another encoder's output has another style.
func TestStyleOf(t *testing.T) {
	pretty := "{\n  \"items\": [],\n  \"n\": 1\n}"
	deeper := "{\n  \"items\": [\n    {\n      \"a\": \"x: y, z\"\n    }\n  ],\n  \"n\": 1\n}"
	if StyleOf([]byte(pretty)) != StyleOf([]byte(deeper)) {
		t.Fatalf("%+v %+v", StyleOf([]byte(pretty)), StyleOf([]byte(deeper)))
	}
	if StyleOf([]byte(`{"a":1,"b":[1,2]}`)) != StyleOf([]byte(`{"a":{"c":[]},"b":"x : y"}`)) {
		t.Fatal("compact")
	}
	others := []string{
		pretty,
		"{\n    \"items\": [],\n    \"n\": 1\n}",   // another indentation
		"{\n\t\"items\": [],\n\t\"n\": 1\n}",       // tabs
		"{\r\n  \"items\": [],\r\n  \"n\": 1\r\n}", // CRLF
		"{\n  \"items\" : [],\n  \"n\" : 1\n}",     // a space before ':'
		"{\n  \"items\":[],\n  \"n\":1\n}",         // none after it
		pretty + "\n",                              // a line end after the document
		`{"items": [], "n": 1}`,                    // Python's default separators
		`{"items":[],"n":1}`,                       // compact
		`{"items":[], "n":1}`,                      // a space after ','
		"{\"items\":[],\n\"n\":1}",                 // a line break, no indentation
	}
	seen := map[Style]string{}
	for _, d := range others {
		st := StyleOf([]byte(d))
		if prev, ok := seen[st]; ok {
			t.Errorf("%q and %q have one style %+v", prev, d, st)
		}
		seen[st] = d
	}
	raw := []byte(deeper)
	if testing.AllocsPerRun(10, func() { StyleOf(raw) }) != 0 {
		t.Error("StyleOf allocates")
	}
}
