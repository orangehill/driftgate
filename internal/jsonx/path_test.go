package jsonx

import "testing"

func TestAt(t *testing.T) {
	v, err := Parse([]byte(`{"entry":{"id":42,"tags":["a",{"slug":"x-1"}]},"entry":{"id":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"$.entry.id": "42", "$.entry.tags[0]": "a", "$.entry.tags[1].slug": "x-1"} {
		got, err := v.At(path)
		if err != nil || got.FormatText() != want {
			t.Errorf("%s: %v %v", path, got, err)
		}
	}
	for _, path := range []string{"$.missing", "$.entry.tags[5]", "$.entry.id.x", "entry", "$.entry.tags[x]"} {
		if _, err := v.At(path); err == nil {
			t.Errorf("%s: no error", path)
		}
	}
}

// TestKeyPath: plain keys stay .key, any other key is ["key"], and At reads
// both back.
func TestKeyPath(t *testing.T) {
	for _, c := range [][3]string{
		{"$", "id", "$.id"},
		{"$", "content-type", "$.content-type"},
		{"$", "2024", "$.2024"},
		{"$", "a.b", `$["a.b"]`},
		{"$.users", "pera.peric@example.com", `$.users["pera.peric@example.com"]`},
		{"$", `say "hi"`, `$["say \"hi\""]`},
		{"$", "", `$[""]`},
	} {
		if got := KeyPath(c[0], c[1]); got != c[2] {
			t.Errorf("KeyPath(%q, %q) = %s, want %s", c[0], c[1], got, c[2])
		}
	}
	v, err := Parse([]byte(`{"a.b":{"x y":[1,{"say \"hi\"":"ok"}]},"a":{"b":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		KeyPath(KeyPath("$", "a.b"), "x y") + "[1]" + `["say \"hi\""]`: `"ok"`,
		"$.a.b": "2",
	} {
		got, err := v.At(path)
		if err != nil || string(Marshal(got, false)) != want {
			t.Errorf("At(%s) = %v, %v; want %s", path, got, err, want)
		}
	}
}
