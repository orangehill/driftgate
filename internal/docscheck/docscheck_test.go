package docscheck

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const sample = "# Page\n" +
	"\n" +
	"Text with [a link that\n" +
	"wraps](missing-page.md) and `[code](no.md)`, then\n" +
	"<a href=\"other.md#Top\">a tag</a>.\n" +
	"\n" +
	"<!--\n" +
	"## Hidden\n" +
	"[hidden](hidden.md)\n" +
	"-->\n" +
	"<a name=\"Kept-As-Written\"></a>\n" +
	"\n" +
	"- item\n" +
	"\n" +
	"  <!-- check: config-fragment -->\n" +
	"  ```yaml{title=x}\n" +
	"  mode: evolve\n" +
	"    level: values\n" +
	"  ```\n" +
	"\n" +
	"## Last\n"

// TestParseText: links that wrap over lines are found on their first line;
// code spans, comments and their headings are not read; a fence's
// indentation is not part of the block.
func TestParseText(t *testing.T) {
	d, err := ParseText("docs/page.md", sample)
	if err != nil {
		t.Fatal(err)
	}
	want := []Link{{3, "missing-page.md"}, {5, "other.md#Top"}}
	if !reflect.DeepEqual(d.Links, want) {
		t.Errorf("links %v, want %v", d.Links, want)
	}
	var headings []string
	for _, h := range d.Headings {
		headings = append(headings, h.Text)
	}
	if !reflect.DeepEqual(headings, []string{"Page", "Last"}) {
		t.Errorf("headings %v", headings)
	}
	if len(d.Blocks) != 1 {
		t.Fatalf("blocks %+v", d.Blocks)
	}
	b := d.Blocks[0]
	if b.Lang != "yaml" || b.Marker != "config-fragment" || b.Body != "mode: evolve\n  level: values" || b.Line != 16 || b.End != 19 {
		t.Errorf("block %+v", b)
	}
	for anchor, want := range map[string]bool{"page": true, "last": true, "Kept-As-Written": true, "kept-as-written": false, "Last": false, "hidden": false} {
		if d.HasAnchor(anchor) != want {
			t.Errorf("HasAnchor(%q) = %v", anchor, !want)
		}
	}
}

// TestResolve: a wrapped link to a missing page fails like any other; a
// query is not part of the file name.
func TestResolve(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "page.md"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPages(root)
	d, err := p.Get("docs/page.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Resolve(d.Path, d.Links[0].Target); err == nil {
		t.Error("the wrapped link to a missing page resolves")
	}
	for target, ok := range map[string]bool{"page.md?plain=1#last": true, "#last": true, "#Last": false, "page.md#nope": false} {
		if err := p.Resolve(d.Path, target); (err == nil) != ok {
			t.Errorf("Resolve(%q): %v", target, err)
		}
	}
}
