package compare

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/jsonx"
)

// residueDocs are a list of 5,000 items, pretty-printed and compact, with
// slashes escaped as PHP writes them.
func residueDocs() map[string][]byte {
	var items []any
	for i := range 5000 {
		items = append(items, map[string]any{"id": i, "name": fmt.Sprintf("n%d", i), "url": fmt.Sprintf("https://x.test/i/%d", i), "tags": []any{"a", "b"}, "o": map[string]any{"x": 1}})
	}
	pretty, _ := json.MarshalIndent(map[string]any{"data": items}, "", "    ")
	compact, _ := json.Marshal(map[string]any{"data": items})
	return map[string][]byte{"pretty": pretty, "compact": []byte(strings.ReplaceAll(string(compact), "/", `\/`))}
}

// BenchmarkSpacing reads the whitespace of a document against its layout.
func BenchmarkSpacing(b *testing.B) {
	for _, name := range []string{"pretty", "compact"} {
		doc := residueDocs()[name]
		style := jsonx.StyleOf(doc)
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			for b.Loop() {
				spacing(doc, style)
			}
		})
	}
}

// BenchmarkJSONResidue writes the whole residue of a document.
func BenchmarkJSONResidue(b *testing.B) {
	for _, name := range []string{"pretty", "compact"} {
		doc := residueDocs()[name]
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(doc)))
			for b.Loop() {
				jsonResidue(doc, nil)
			}
		})
	}
}
