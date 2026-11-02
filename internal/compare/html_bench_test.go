package compare

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// BenchmarkHTML compares each page of testdata/html (real Next.js and Nuxt
// pages and a handmade one) with itself plus a comment, so neither side is
// skipped as the same bytes: both pages are parsed and walked.
func BenchmarkHTML(b *testing.B) {
	files, err := filepath.Glob(filepath.Join("testdata", "html", "*.html"))
	if err != nil || len(files) == 0 {
		b.Fatal("no pages", err)
	}
	var pages [][2]Side
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			b.Fatal(err)
		}
		cur := strings.Replace(string(raw), "</body>", "<!-- x --></body>", 1)
		pages = append(pages, [2]Side{side(200, pageHeaders, string(raw)), side(200, pageHeaders, cur)})
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range pages {
			One("req", p[0], p[1], allowlist.Empty(), LevelValues)
		}
	}
}
