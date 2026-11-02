package compare

import (
	"github.com/orangehill/driftgate/internal/allowlist"
	"testing"
)

// An any_length rule lets arrays change their length and nothing else: the
// values its glob reaches are still compared (a price that changed under
// $.items* is a drift).
func TestAnyLengthIsNotAValueRule(t *testing.T) {
	allow := htmlAllow(t, "  - path: '$.items*'\n    any_length: true\n    reason: paging\n")
	ds := both(t, allow, side(200, jsonType, `{"items":[{"price":10,"name":"a"}]}`),
		side(200, jsonType, `{"items":[{"price":99,"name":"a"},{"price":1,"name":"c"}]}`))
	if len(ds) != 1 || !has(ds, KindValue, "$.items[0].price") {
		t.Fatalf("got %+v", ds)
	}
	allow = htmlAllow(t, "  - path: '$.items'\n    any_length: true\n    reason: paging\n")
	if ds := both(t, allow, side(200, jsonType, `{"items":"a"}`), side(200, jsonType, `{"items":"b"}`)); !has(ds, KindValue, "$.items") {
		t.Fatalf("scalar at the path: %+v", ds)
	}
	// With a format the same rule also covers the values.
	allow = htmlAllow(t, "  - path: '$.items*'\n    any_length: true\n    format: '\\d+|[a-z]'\n    reason: paging\n")
	if ds := both(t, allow, side(200, jsonType, `{"items":[{"price":10}]}`), side(200, jsonType, `{"items":[{"price":99},{"price":1}]}`)); len(ds) != 0 {
		t.Fatalf("with a format: %+v", ds)
	}
}

// A header rule judges only a header value that changed: an unchanged value
// outside the format is the baseline's, and no drift whatever else changed.
func TestHeaderRuleOnlyOnChange(t *testing.T) {
	l := htmlAllow(t, "  - path: $.x\n    format: x\n    reason: x\n")
	l.Headers = []allowlist.HeaderRule{{Name: "x-time", Format: `\d+`, Reason: "time"}}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	h := [][2]string{{"content-type", "application/json"}, {"x-time", "never"}}
	for _, cur := range []string{`{"a":1}`, `{"a":2}`} {
		if ds := run(t, l, LevelValues, side(200, h, `{"a":1}`), side(200, h, cur)); has(ds, KindFormat, "header:x-time") {
			t.Fatalf("%s: %+v", cur, ds)
		}
	}
	h2 := [][2]string{{"content-type", "application/json"}, {"x-time", "later"}}
	if ds := run(t, l, LevelValues, side(200, h, `{}`), side(200, h2, `{}`)); !has(ds, KindFormat, "header:x-time") {
		t.Fatalf("changed: %+v", ds)
	}
}

// The <!DOCTYPE> declaration is compared: added or removed at the shape
// level, changed at the values level; the same declaration written in
// another letter case is the same.
func TestDoctype(t *testing.T) {
	page := func(doctype string) Side { return htmlSide(doctype + "<html><body><p>a</p></body></html>") }
	if ds := run(t, nil, LevelShape, page("<!DOCTYPE html>"), page("")); len(ds) != 1 || !has(ds, KindDoctype, DoctypePath) || ds[0].Baseline != "<!DOCTYPE html>" {
		t.Fatalf("removed: %+v", ds)
	}
	if ds := run(t, nil, LevelShape, page(""), page("<!DOCTYPE html>")); !has(ds, KindDoctype, DoctypePath) {
		t.Fatalf("added: %+v", ds)
	}
	xhtml := `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">`
	if ds := run(t, nil, LevelShape, page("<!DOCTYPE html>"), page(xhtml)); len(ds) != 0 {
		t.Fatalf("changed at shape: %+v", ds)
	}
	if ds := both(t, nil, page("<!DOCTYPE html>"), page(xhtml)); !has(ds, KindDoctype, DoctypePath) {
		t.Fatalf("changed: %+v", ds)
	}
	if ds := both(t, nil, page("<!DOCTYPE html>"), page("<!doctype HTML>")); len(ds) != 0 {
		t.Fatalf("letter case: %+v", ds)
	}
}

// Pretty-printed JSON that nests one level deeper now is indented further,
// by the same encoder: no layout drift (an array the allowlist lets grow
// took a new object). Another indentation still is one.
func TestLayoutOfDeeperNesting(t *testing.T) {
	allow := htmlAllow(t, "  - path: '$.items'\n    any_length: true\n    reason: paging\n")
	b := "{\n  \"items\": [],\n  \"n\": 1\n}"
	c := "{\n  \"items\": [\n    {\n      \"a\": 1\n    }\n  ],\n  \"n\": 1\n}"
	if ds := both(t, allow, side(200, jsonType, b), side(200, jsonType, c)); len(ds) != 0 {
		t.Fatalf("deeper: %+v", ds)
	}
	four := "{\n    \"items\": [],\n    \"n\": 1\n}"
	if ds := both(t, allow, side(200, jsonType, b), side(200, jsonType, four)); len(ds) != 1 || !has(ds, KindEncoding, "$") || ds[0].Baseline != "layout" {
		t.Fatalf("indentation: %+v", ds)
	}
	// Keys escaped differently: nothing else explains the other bytes.
	if ds := both(t, nil, side(200, jsonType, `{"a":1}`), side(200, jsonType, `{"\u0061":1}`)); len(ds) != 1 || !has(ds, KindEncoding, "$") {
		t.Fatalf("escaped key: %+v", ds)
	}
}
