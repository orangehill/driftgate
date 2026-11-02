package pii

import (
	"reflect"
	"runtime"
	"testing"

	"github.com/orangehill/driftgate/internal/jsonx"
)

// treeScan is Scan as it was, over the parsed tree: the reference the
// streaming scan must match finding for finding.
func (d *Detector) treeScan(body []byte) []Finding {
	v, err := jsonx.Parse(body)
	if err != nil {
		return appendFindings(nil, "$", d.matches(string(body)))
	}
	var out []Finding
	var path []step
	var walk func(v *jsonx.Value)
	walk = func(v *jsonx.Value) {
		switch v.Kind {
		case jsonx.String:
			if ms := d.matches(v.Text); len(ms) > 0 {
				out = appendFindings(out, pathOf(path), ms)
			}
		case jsonx.Int:
			if kind := d.number(v.Text, lastKey(path)); kind != "" {
				out = append(out, Finding{Kind: kind, Where: pathOf(path), Sample: Mask(v.Text)})
			}
		case jsonx.Object:
			for _, m := range v.Members {
				path = append(path, step{key: m.Key, index: -1})
				if ms := d.matches(m.Key); len(ms) > 0 {
					out = appendFindings(out, pathOf(path), ms)
				}
				walk(m.Value)
				path = path[:len(path)-1]
			}
		case jsonx.Array:
			for i, item := range v.Items {
				path = append(path, step{index: i})
				walk(item)
				path = path[:len(path)-1]
			}
		}
	}
	walk(v)
	return out
}

// The scan that walks the body finds what the scan of the tree found, in
// the same order, also in a body that is not JSON after its start.
func TestScanMatchesTree(t *testing.T) {
	d, err := New([]string{`allowed@example\.com`})
	if err != nil {
		t.Fatal(err)
	}
	bodies := [][]byte{
		corpus(300, true), corpus(300, false), stamps(300), page(200),
		[]byte(`{"pera@example.com":{"jmbg":"0101990710006","phone":641234567,"list":[381641234567,"allowed@example.com"]}}`),
		[]byte(`{"a":"pera@example.com","b":`), // a finding before the syntax error
		[]byte(`[{"mail":"x@example.org"}] trailing`),
		[]byte(`"+381 64 123 4567"`), []byte(`4111111111111111`), []byte(``),
	}
	for i, body := range bodies {
		got, want := d.Scan(body), d.treeScan(body)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("body %d: Scan %v, tree %v", i, got, want)
		}
	}
}

// A scan holds no tree of the body: it allocates about the size of the
// body (its strings), where the tree took 13 to 17 times that.
func TestScanMemory(t *testing.T) {
	d, _ := New(nil)
	body := corpus(3000, false)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	d.Scan(body)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 4*uint64(len(body)) {
		t.Fatalf("%d bytes allocated to scan %d bytes", got, len(body))
	}
}
