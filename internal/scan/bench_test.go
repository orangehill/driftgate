package scan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/pii"
)

func benchPublic(b *testing.B, ct string, body []byte) {
	d, _ := pii.New(nil)
	headers := [][2]string{{"content-type", ct}}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		Public(d, headers, body)
	}
}

// BenchmarkPublicJSON is about 2 MB of API response, as most public
// bodies are; HTML is a page of about 2 MB.
func BenchmarkPublicJSON(b *testing.B) {
	var s strings.Builder
	s.WriteString(`{"data":[`)
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&s, `{"id":%d,"title":"News issue %d – announcement from Niš","price":%d.%02d,"created_at":"2024-01-%02dT10:30:00Z","tags":["a","b"]},`, i, i, i*7%1000, i%100, i%28+1)
	}
	s.WriteString(`{}]}`)
	benchPublic(b, "application/json", []byte(s.String()))
}

func BenchmarkPublicHTML(b *testing.B) {
	var s strings.Builder
	s.WriteString(`<html><body>`)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&s, `<p class="lead">News &amp; announcements from Čačak, issue %d – <a href="/news/%d?page=2">more</a></p>`, i, i)
	}
	s.WriteString(`</body></html>`)
	benchPublic(b, "text/html; charset=utf-8", []byte(s.String()))
}
