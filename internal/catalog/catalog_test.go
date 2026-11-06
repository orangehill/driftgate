package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSequences(t *testing.T) {
	base := func() *Catalog {
		return &Catalog{Version: Version, Requests: []Request{
			{ID: "a", Method: "POST", Path: "/a", Storage: "public"},
			{ID: "b", Method: "GET", Path: "/a/{id}", Storage: "public", ParamsFrom: map[string]From{"id": {Request: "a", Path: "$.id"}}},
		}}
	}
	c := base()
	c.Sequences = []Sequence{{ID: "s", Requests: []string{"a", "b"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, seqs := range map[string][]Sequence{
		"no sequence":    nil,
		"giver after":    {{ID: "s", Requests: []string{"b", "a"}}},
		"other sequence": {{ID: "s", Requests: []string{"a"}}, {ID: "t", Requests: []string{"b"}}},
		"in two":         {{ID: "s", Requests: []string{"a", "b"}}, {ID: "t", Requests: []string{"a"}}},
	} {
		c := base()
		c.Sequences = seqs
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "in two" && !strings.Contains(err.Error(), "runs once") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestPathStartsWithSlash: the path is appended to the base URL, so one
// without a leading "/" could name another host.
func TestPathStartsWithSlash(t *testing.T) {
	for _, p := range []string{"@127.0.0.1:8906/steal", ".evil.example/x", "api/x", ""} {
		c := &Catalog{Version: Version, Requests: []Request{{ID: "a", Method: "GET", Path: p, Storage: "public"}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must start with /") {
			t.Errorf("%q: %v", p, err)
		}
	}
}

// TestIDsAreFileNamesEverywhere: an ID names the file of its record, so
// IDs that differ only in case (one file on macOS and Windows) and names
// Windows reserves for devices are refused; so are such sequence IDs.
func TestIDsAreFileNamesEverywhere(t *testing.T) {
	req := func(id string) Request { return Request{ID: id, Method: "GET", Path: "/" + id, Storage: "public"} }
	for name, c := range map[string]*Catalog{
		"case":          {Requests: []Request{req("get-X"), req("get-x")}},
		"con":           {Requests: []Request{req("con")}},
		"NUL.json":      {Requests: []Request{req("NUL.json")}},
		"com1.x":        {Requests: []Request{req("com1.x")}},
		"Lpt9":          {Requests: []Request{req("Lpt9")}},
		"trailing dot":  {Requests: []Request{req("a.")}},
		"sequence aux":  {Requests: []Request{req("a")}, Sequences: []Sequence{{ID: "aux", Requests: []string{"a"}}}},
		"sequence case": {Requests: []Request{req("a"), req("b")}, Sequences: []Sequence{{ID: "S", Requests: []string{"a"}}, {ID: "s", Requests: []string{"b"}}}},
	} {
		c.Version = Version
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "case" && !strings.Contains(err.Error(), `"get-X" and "get-x" differ only in case`) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := &Catalog{Version: Version, Requests: []Request{req("console"), req("com10"), req("a.b"), req("nul-x")}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

// TestMethodDecidesMutates: a request with a method that is not safe changes
// data unless the catalog says mutates: false; the method is checked and
// written in upper case.
func TestMethodDecidesMutates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	write := func(reqs string) (*Catalog, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(`{"version":1,"requests":[`+reqs+`]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	c, err := write(`{"id":"del","method":"delete","path":"/api/users/1","storage":"public"},
		{"id":"get","method":"GET","path":"/a","storage":"public"},
		{"id":"head","method":"HEAD","path":"/a","storage":"public"},
		{"id":"none","path":"/a","storage":"public"},
		{"id":"search","method":"POST","path":"/search","mutates":false,"storage":"public"},
		{"id":"touch","method":"GET","path":"/touch","mutates":true,"storage":"public"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"del": true, "get": false, "head": false, "none": false, "search": false, "touch": true}
	for _, r := range c.Requests {
		if r.Mutating() != want[r.ID] || r.Sequential() != want[r.ID] {
			t.Errorf("%s: mutating %v, sequential %v", r.ID, r.Mutating(), r.Sequential())
		}
		// Only del changes data because of its method alone.
		if r.MutatingByMethod() != (r.ID == "del") {
			t.Errorf("%s: mutating by method %v", r.ID, r.MutatingByMethod())
		}
	}
	if c.Requests[0].Method != "DELETE" {
		t.Errorf("method %q, want DELETE", c.Requests[0].Method)
	}
	// What was written stays: unset, true and false.
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if got := strings.Count(string(raw), `"mutates"`); got != 2 || !strings.Contains(string(raw), `"mutates": false`) {
		t.Errorf("saved catalog has %d mutates fields:\n%s", got, raw)
	}
	for _, m := range []string{"GE T", "GET\n", "PO/ST", "ÜBER"} {
		b, _ := json.Marshal(m)
		if _, err := write(`{"id":"a","method":` + string(b) + `,"path":"/a","storage":"public"}`); err == nil || !strings.Contains(err.Error(), "invalid method") {
			t.Errorf("method %q: %v", m, err)
		}
	}
}

// LiteralID: a number written as a segment of the path, not a parameter
// nor a part of a segment, and not in the query.
func TestLiteralID(t *testing.T) {
	for path, want := range map[string]string{"/api/users/17": "17", "/api/users/17/edit": "17", "/api/v2/users": "", "/api/users/{id}": "",
		"/reports?year=2024": "", "/a/0": "0", "/2fa": "", "/": ""} {
		if got := LiteralID(path); got != want {
			t.Errorf("%s: %q", path, got)
		}
	}
}
