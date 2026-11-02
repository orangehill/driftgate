package compare

import (
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/sheet"
)

// TestPlacesNameDrifts checks that Places names a place the way a drift
// there is named: every drift of a value, an array length or a cell is at
// a place of that kind.
func TestPlacesNameDrifts(t *testing.T) {
	jsonT := [][2]string{{"content-type", "application/json"}}
	cases := []struct {
		name      string
		base, cur Side
	}{
		{"json",
			side(200, jsonT, `{"data":[{"id":1,"e-mail":"a@x","n":null}],"meta":{"a.b":true,"list":[1,2]}}`),
			side(200, jsonT, `{"data":[{"id":2,"e-mail":"b@x","n":0}],"meta":{"a.b":false,"list":[1]}}`)},
		{"page",
			htmlSide(`<!DOCTYPE html><html><body class="a"><p>one</p><p>two</p>` +
				`<script id="__NEXT_DATA__" type="application/json">{"buildId":"x","props":{"l":[1]}}</script>` +
				`<script>self.__next_f.push([1,"0:{\"b\":\"x\"}\n"])</script></body></html>`),
			htmlSide(`<!DOCTYPE html><html><body class="b"><p>uno</p><p>dos</p>` +
				`<script id="__NEXT_DATA__" type="application/json">{"buildId":"y","props":{"l":[1,2]}}</script>` +
				`<script>self.__next_f.push([1,"0:{\"b\":\"y\"}\n"])</script></body></html>`)},
		{"xlsx",
			book(sheet.Rows{Name: "Q1 2026", Rows: [][]string{{"a", "b"}, {"c", "d"}}}),
			book(sheet.Rows{Name: "Q1 2026", Rows: [][]string{{"x", "b"}, {"c", "y"}}})},
	}
	kindOf := map[string]PlaceKind{
		KindValue: PlaceValue, KindType: PlaceValue, KindText: PlaceValue, KindAttributeValue: PlaceValue,
		KindArrayLength: PlaceArray, KindCell: PlaceCell,
	}
	for _, c := range cases {
		places := map[PlaceKind]map[string]bool{}
		Places(c.base, allowlist.Empty(), "req", func(k PlaceKind, path string) bool {
			if places[k] == nil {
				places[k] = map[string]bool{}
			}
			places[k][path] = true
			return true
		})
		ds := run(t, nil, LevelValues, c.base, c.cur)
		n := 0
		for _, d := range ds {
			k, ok := kindOf[d.Kind]
			if !ok {
				continue
			}
			n++
			if !places[k][d.Where] {
				t.Errorf("%s: drift %s at %s is no place of its kind; places %v", c.name, d.Kind, d.Where, places[k])
			}
		}
		if n < 2 {
			t.Errorf("%s: only %d drifts to check: %v", c.name, n, ds)
		}
		if body := places[PlaceBody][allowlist.RawPath]; body != (c.name != "json") {
			t.Errorf("%s: whole-body place %v", c.name, body)
		}
	}
	// fn returning false stops.
	n := 0
	Places(cases[0].base, allowlist.Empty(), "req", func(PlaceKind, string) bool { n++; return false })
	if n != 1 {
		t.Errorf("Places went on after false: %d calls", n)
	}
}
