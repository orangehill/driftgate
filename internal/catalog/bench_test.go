package catalog

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkLoad reads a catalog of 5,000 requests, a quarter of them POST.
func BenchmarkLoad(b *testing.B) {
	c := &Catalog{Version: Version}
	for i := range 5000 {
		r := Request{ID: fmt.Sprintf("req-%d", i), Method: "GET", Path: fmt.Sprintf("/api/items/%d", i), Storage: "public",
			Headers: [][2]string{{"Accept", "application/json"}}}
		if i%4 == 0 {
			r.Method, r.Body = "POST", []byte(`{"q":"x"}`)
		}
		c.Requests = append(c.Requests, r)
	}
	path := filepath.Join(b.TempDir(), "catalog.json")
	if err := c.Save(path); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load(path); err != nil {
			b.Fatal(err)
		}
	}
}
