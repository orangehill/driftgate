package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSteps(t *testing.T) {
	upload := func(f File) *Multipart { return &Multipart{Fields: [][2]string{{"kind", "x"}}, Files: []File{f}} }
	ok := []Request{
		{ID: "u", Method: "POST", Path: "/u", Storage: "private", Multipart: upload(File{Field: "file", Path: "orders/2024.xlsx", Ref: "abc"})},
		{ID: "s", Method: "POST", Path: "/u", Storage: "public", Mutates: new(bool), Multipart: upload(File{Field: "file", Synthetic: "png"})}, // a synthetic file alone is not sequential
		{ID: "r", Method: "PUT", Path: "/r", Storage: "public", Repeat: 61},
		{ID: "c", Command: []string{"php", "artisan", "queue:work", "--stop-when-empty"}, Output: true, OutputIgnore: []string{`\d+%`}, Storage: "public"},
	}
	c := &Catalog{Version: Version, Requests: ok, Sequences: []Sequence{{ID: "q", Requests: []string{"u", "c"}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !ok[2].Sequential() || !ok[3].Sequential() || ok[1].Sequential() || !ok[0].UsesFiles() || ok[1].UsesFiles() {
		t.Fatal("Sequential or UsesFiles")
	}
	for want, r := range map[string]Request{
		"must have storage private":      {Method: "POST", Path: "/u", Storage: "public", Multipart: upload(File{Field: "file", Path: "a.csv"})},
		"either path":                    {Method: "POST", Path: "/u", Storage: "private", Multipart: upload(File{Field: "file"})},
		"unknown synthetic kind":         {Method: "POST", Path: "/u", Storage: "public", Multipart: upload(File{Field: "file", Synthetic: "exe"})},
		"without . or ..":                {Method: "POST", Path: "/u", Storage: "private", Multipart: upload(File{Field: "file", Path: "../secret.csv"})},
		"field is empty":                 {Method: "POST", Path: "/u", Storage: "public", Multipart: upload(File{Synthetic: "csv"})},
		"body and multipart":             {Method: "POST", Path: "/u", Storage: "public", Body: json.RawMessage(`{}`), Multipart: &Multipart{}},
		"repeat must be":                 {Method: "GET", Path: "/r", Storage: "public", Repeat: MaxRepeat + 1},
		"no method or path":              {Command: []string{"true"}, Method: "GET", Storage: "public"},
		"no body, headers":               {Command: []string{"true"}, Identity: "admin", Storage: "public"},
		"output_ignore needs output":     {Command: []string{"true"}, OutputIgnore: []string{"x"}, Storage: "public"},
		"output_ignore: error":           {Command: []string{"true"}, Output: true, OutputIgnore: []string{"("}, Storage: "public"},
		"belong to command steps":        {Method: "GET", Path: "/", Output: true, Storage: "public"},
		"stdin belong to command steps":  {Method: "GET", Path: "/", Dir: "sub", Stdin: "in.txt", Storage: "public"},
		"a command step is not repeated": {Command: []string{"true"}, Repeat: 2, Storage: "public"},
	} {
		r.ID = "x"
		err := (&Catalog{Version: Version, Requests: []Request{r}}).Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

// TestUnknownFieldsAreErrors: a misspelled field is an error, not a
// request that silently runs with the field's default.
func TestUnknownFieldsAreErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"requests":[{"id":"a","method":"POST","path":"/a","storage":"public","mutate":true}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "mutate") {
		t.Fatalf("a misspelled field was accepted: %v", err)
	}
}
