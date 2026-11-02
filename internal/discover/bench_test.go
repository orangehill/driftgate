package discover

import (
	"context"
	"testing"
)

// BenchmarkFileRoutes reads the Next.js and Nuxt sample projects, the
// adapters that match file and segment names against patterns.
func BenchmarkFileRoutes(b *testing.B) {
	adapters := []Adapter{{Adapter: "nextjs", Dir: "testdata/next"}, {Adapter: "nuxt", Dir: "testdata/nuxt"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Run(context.Background(), ".", adapters, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
