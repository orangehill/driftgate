package jsonx

import (
	"fmt"
	"strings"
	"testing"
)

// benchDoc is about 4 MB of API response.
func benchDoc() []byte {
	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; b.Len() < 4<<20; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"uuid":"%08x-1c2d-4e5f-8a9b-%012x","title":"Item \"%d\"","price":%d.5,"active":true,"tags":["a","b"],"body":"lorem ipsum dolor sit amet"}`, i, i, i, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func BenchmarkParse(b *testing.B) {
	doc := benchDoc()
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWalk(b *testing.B) {
	doc := benchDoc()
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		if err := Walk(doc, nopVisitor{}); err != nil {
			b.Fatal(err)
		}
	}
}
