package compare

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/sheet"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const (
	baseOrigin = "http://localhost:8080"
	curOrigin  = "http://localhost:8081"
)

var jsonType = [][2]string{{"content-type", "application/json"}}

// at gives a side its own origins.
func at(s Side, origins ...string) Side {
	s.Origins = origins
	return s
}

// hosted compares a body served at baseOrigin with one served at curOrigin;
// {o} in either stands for the side's own origin.
func hosted(t *testing.T, headers [][2]string, base, cur string) []Drift {
	t.Helper()
	b := strings.ReplaceAll(base, "{o}", baseOrigin)
	c := strings.ReplaceAll(cur, "{o}", curOrigin)
	return both(t, nil, at(side(200, headers, b), baseOrigin), at(side(200, headers, c), curOrigin))
}

func TestOriginForms(t *testing.T) {
	side := originSide{list: []origin{{scheme: "http", auth: []byte("localhost:8080"), port: true}}}
	cases := [][2]string{
		{`"http://localhost:8080/storage/a.jpg"`, `"http://origin.invalid/storage/a.jpg"`},
		{`"http:\/\/localhost:8080\/storage\/a.jpg"`, `"http:\/\/origin.invalid\/storage\/a.jpg"`},
		{`http://localhost:8080`, `http://origin.invalid`},
		{`http://localhost:8080?x#y`, `http://origin.invalid?x#y`},
		{`<img src="//localhost:8080/i.png">`, `<img src="//origin.invalid/i.png">`},
		{`https://localhost:8080/x`, `https://origin.invalid/x`}, // another scheme stays
		{`ws://localhost:8080/socket`, `ws://origin.invalid/socket`},
		{`"host": "localhost:8080"`, `"host": "origin.invalid"`},
		{`http://user@localhost:8080/`, `http://user@origin.invalid/`},
		{`a http://localhost:8080/ and localhost:8080, twice`, `a http://origin.invalid/ and origin.invalid, twice`},
		// Not this origin.
		{`http://localhost:80801/x`, `http://localhost:80801/x`},
		{`http://localhost:8080.evil.test/x`, `http://localhost:8080.evil.test/x`},
		{`http://localhost:8080@evil.test/x`, `http://localhost:8080@evil.test/x`},
		{`http://a.localhost:8080/x`, `http://a.localhost:8080/x`},
		{`http://mylocalhost:8080/x`, `http://mylocalhost:8080/x`},
		{`http://localhost:8080:1/x`, `http://localhost:8080:1/x`},
		{`http://localhost/x`, `http://localhost/x`},
	}
	for _, c := range cases {
		if got := string(side.replace([]byte(c[0]))); got != c[1] {
			t.Errorf("%s: got %s, want %s", c[0], got, c[1])
		}
	}
	// Without a port only a host after // is the origin.
	side = originSide{list: []origin{{scheme: "https", auth: []byte("example.com")}}}
	for _, c := range [][2]string{
		{`https://example.com/x`, `https://origin.invalid/x`},
		{`//example.com/x`, `//origin.invalid/x`},
		{`info@example.com`, `info@example.com`},
		{`db: example.com`, `db: example.com`},
		{`https://example.com:8443/x`, `https://example.com:8443/x`},
		{`https://example.community/x`, `https://example.community/x`},
	} {
		if got := string(side.replace([]byte(c[0]))); got != c[1] {
			t.Errorf("%s: got %s, want %s", c[0], got, c[1])
		}
	}
}

func TestOriginOf(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:8080/api/":  "http://localhost:8080",
		"HTTPS://Example.COM:443/x?y": "https://example.com",
		"http://example.com:80":       "http://example.com",
		"http://[::1]:8080":           "http://[::1]:8080",
	} {
		if got, err := Origin(in); err != nil || got != want {
			t.Errorf("%s: got %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "localhost:8080", "ftp://x", "http://u:p@x"} {
		if _, err := Origin(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

// The same response served at another address does not drift: absolute URLs
// in a JSON body (also escaped), in HTML and in the Location header.
func TestOriginsSameApplication(t *testing.T) {
	cases := []struct {
		name      string
		headers   [][2]string
		base, cur string
	}{
		{"json", jsonType, `{"url":"{o}/storage/assets/a.jpg","n":1}`, `{"url":"{o}/storage/assets/a.jpg","n":1}`},
		{"escaped json", jsonType, `{"url":"{o}/storage\/a.jpg"}`, `{"url":"{o}/storage\/a.jpg"}`},
		{"key", jsonType, `{"{o}/a":1}`, `{"{o}/a":1}`},
		{"html", htmlType, `<a href="{o}/news">{o}/news</a><img src="{o}/i.png">`, `<a href="{o}/news">{o}/news</a><img src="{o}/i.png">`},
		{"next data", htmlType, `<script id="__NEXT_DATA__" type="application/json">{"u":"{o}/x"}</script>`,
			`<script id="__NEXT_DATA__" type="application/json">{"u":"{o}/x"}</script>`},
		{"text", [][2]string{{"content-type", "text/plain"}}, "see {o}/x\n", "see {o}/x\n"},
		{"csv", [][2]string{{"content-type", "text/csv"}}, "a,{o}/x\n", "a,{o}/x\n"},
	}
	for _, c := range cases {
		if ds := hosted(t, c.headers, c.base, c.cur); len(ds) != 0 {
			t.Errorf("%s: %+v", c.name, ds)
		}
	}
	// Escaped by one side only is a change of encoding, as without origins.
	ds := hosted(t, jsonType, `{"url":"{o}/a"}`, `{"url":"`+strings.ReplaceAll(curOrigin, "/", `\/`)+`\/a"}`)
	if len(ds) != 1 || ds[0].Kind != KindEncoding {
		t.Errorf("escaped by one side: %+v", ds)
	}

	loc := func(o string) Side {
		return side(302, [][2]string{{"location", o + "/login"}, {"content-length", "0"}}, "")
	}
	if ds := both(t, nil, at(loc(baseOrigin), baseOrigin), at(loc(curOrigin), curOrigin)); len(ds) != 0 {
		t.Errorf("location: %+v", ds)
	}
	// Without origins the same responses drift.
	if ds := both(t, nil, loc(baseOrigin), loc(curOrigin)); len(ds) != 1 || ds[0].Kind != KindHeaderValue {
		t.Errorf("location without origins: %+v", ds)
	}
	if ds := both(t, nil, side(200, jsonType, `{"u":"`+baseOrigin+`/a"}`), side(200, jsonType, `{"u":"`+curOrigin+`/a"}`)); len(ds) != 1 {
		t.Errorf("json without origins: %+v", ds)
	}

	// A body of another length: Content-Length says nothing new.
	long := "http://app.localhost:18081"
	b := at(side(200, append([][2]string{{"content-length", "18"}}, jsonType...), `{"u":"`+baseOrigin+`"}`), baseOrigin)
	c := at(side(200, append([][2]string{{"content-length", "23"}}, jsonType...), `{"u":"`+long+`"}`), long)
	if ds := both(t, nil, b, c); len(ds) != 0 {
		t.Errorf("content-length: %+v", ds)
	}
}

// A URL of another host, port, scheme or path still drifts, and the report
// shows each side's real URL.
func TestOriginsKeepRealChanges(t *testing.T) {
	cases := []struct{ name, base, cur, wantB, wantC string }{
		{"path", `{"u":"{o}/storage/a.jpg"}`, `{"u":"{o}/storage/b.jpg"}`, `"` + baseOrigin + `/storage/a.jpg"`, `"` + curOrigin + `/storage/b.jpg"`},
		{"another host", `{"u":"{o}/a.jpg"}`, `{"u":"http://cdn.test/a.jpg"}`, `"` + baseOrigin + `/a.jpg"`, `"http://cdn.test/a.jpg"`},
		{"scheme", `{"u":"{o}/a"}`, `{"u":"https://localhost:8081/a"}`, `"` + baseOrigin + `/a"`, `"https://localhost:8081/a"`},
		{"longer port", `{"u":"http://localhost:80801/a"}`, `{"u":"http://localhost:80811/a"}`, `"http://localhost:80801/a"`, `"http://localhost:80811/a"`},
	}
	for _, c := range cases {
		ds := hosted(t, jsonType, c.base, c.cur)
		if len(ds) != 1 || ds[0].Kind != KindValue || ds[0].Baseline != c.wantB || ds[0].Current != c.wantC {
			t.Errorf("%s: %+v", c.name, ds)
		}
	}
	ds := hosted(t, htmlType, `<a href="{o}/a">x</a>`, `<a href="{o}/b">x</a>`)
	if len(ds) != 1 || ds[0].Kind != KindAttributeValue || !strings.Contains(ds[0].Current, curOrigin+"/b") {
		t.Errorf("html: %+v", ds)
	}
	loc := func(u string) Side { return side(302, [][2]string{{"location", u}}, "") }
	ds = both(t, nil, at(loc(baseOrigin+"/a"), baseOrigin), at(loc("http://evil.test/a"), curOrigin))
	if len(ds) != 1 || ds[0].Kind != KindHeaderValue || ds[0].Baseline != `["`+baseOrigin+`/a"]` {
		t.Errorf("location: %+v", ds)
	}
}

// Cells of an XLSX export are compared with the origins replaced; the
// archive's bytes are left as they are.
func TestOriginsXLSX(t *testing.T) {
	xlsxType := [][2]string{{"content-type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}}
	x := func(o, file string) Side {
		return at(side(200, xlsxType, string(sheet.WriteXLSX([][]string{{"a", o + "/" + file}}))), o)
	}
	if ds := both(t, nil, x(baseOrigin, "a.pdf"), x(curOrigin, "a.pdf")); len(ds) != 0 {
		t.Errorf("same: %+v", ds)
	}
	ds := both(t, nil, x(baseOrigin, "a.pdf"), x(curOrigin, "b.pdf"))
	if len(ds) != 1 || !has(ds, KindCell, "xlsx:Sheet1!B1") || ds[0].Current != curOrigin+"/b.pdf" {
		t.Errorf("changed: %+v", ds)
	}
}

// An address both sides write as it is (a fixed APP_URL) is no change, also
// next to a change elsewhere in the body.
func TestOriginsFixedAddress(t *testing.T) {
	ds := hosted(t, jsonType, `{"app":"`+baseOrigin+`/a","u":"{o}/x","n":1}`, `{"app":"`+baseOrigin+`/a","u":"{o}/x","n":2}`)
	if len(ds) != 1 || !has(ds, KindValue, "$.n") {
		t.Errorf("got %+v", ds)
	}
}

// Sides of different schemes: the own origin of each becomes one neutral
// origin, and a URL with the other scheme is not taken for it.
func TestOriginsAcrossSchemes(t *testing.T) {
	const prod = "https://example.com"
	b := at(side(200, jsonType, `{"u":"`+baseOrigin+`/a","v":"`+baseOrigin+`/a"}`), baseOrigin)
	c := at(side(200, jsonType, `{"u":"`+prod+`/a","v":"http://example.com/a"}`), prod)
	ds := both(t, nil, b, c)
	if len(ds) != 1 || !has(ds, KindValue, "$.v") || ds[0].Baseline != `"`+baseOrigin+`/a"` || ds[0].Current != `"http://example.com/a"` {
		t.Errorf("got %+v", ds)
	}
}

// Aliases are origins of the same application.
func TestOriginsAliases(t *testing.T) {
	const alias = "http://app.test"
	b := at(side(200, jsonType, `{"u":"`+alias+`/a","v":"`+baseOrigin+`/b"}`), baseOrigin, alias)
	c := at(side(200, jsonType, `{"u":"`+curOrigin+`/a","v":"`+curOrigin+`/b"}`), curOrigin, alias)
	if ds := both(t, nil, b, c); len(ds) != 0 {
		t.Errorf("got %+v", ds)
	}
}

// A failed request names its URL in the error.
func TestOriginsInErrors(t *testing.T) {
	fail := func(o string) Side {
		return Side{Record: &snapshot.Record{Error: `Get "` + o + `/a": EOF`}}
	}
	if ds := both(t, nil, at(fail(baseOrigin), baseOrigin), at(fail(curOrigin), curOrigin)); len(ds) != 0 {
		t.Errorf("got %+v", ds)
	}
}

// The bodies and records the comparison is given are never changed: they
// may be a cached record or a decrypted private body.
func TestOriginsLeaveInputs(t *testing.T) {
	body := `{"u":"` + baseOrigin + `/a"}`
	b := at(side(200, [][2]string{{"location", baseOrigin + "/x"}}, body), baseOrigin)
	b.Record.Body.Storage = snapshot.Private
	c := at(side(200, [][2]string{{"location", curOrigin + "/x"}}, `{"u":"`+curOrigin+`/b"}`), curOrigin)
	both(t, nil, b, c)
	if string(b.Body) != body || b.Record.Headers[0][1] != baseOrigin+"/x" {
		t.Errorf("input changed: %s %v", b.Body, b.Record.Headers)
	}
}

// With the same origins on both sides, or a side without one, nothing is
// replaced: the drifts are exactly those of a comparison without origins.
func TestOriginsSameGolden(t *testing.T) {
	docs := corpus(t)
	pairs := [][2]Side{}
	for _, raw := range docs {
		doc := string(raw)
		pairs = append(pairs, [2]Side{side(200, jsonType, doc), side(200, jsonType, strings.Replace(doc, `"`, `"x`, 3))})
	}
	u := `{"a":"` + baseOrigin + `/a","b":"` + curOrigin + `/b","c":"localhost:8080"}`
	pairs = append(pairs,
		[2]Side{side(200, jsonType, u), side(200, jsonType, strings.ReplaceAll(u, "8080", "8081"))},
		[2]Side{side(302, [][2]string{{"location", baseOrigin}}, ""), side(302, [][2]string{{"location", curOrigin}}, "")},
		[2]Side{htmlSide(`<a href="` + baseOrigin + `/a">x</a>`), htmlSide(`<a href="` + curOrigin + `/a">x</a>`)})
	for i, p := range pairs {
		want := both(t, nil, p[0], p[1])
		for _, o := range [][2][]string{
			{{baseOrigin}, {baseOrigin}},
			{{baseOrigin, curOrigin}, {curOrigin, baseOrigin + "/"}},
			{nil, {curOrigin}},
			{{baseOrigin}, nil},
		} {
			got := both(t, nil, at(p[0], o[0]...), at(p[1], o[1]...))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("pair %d, origins %v: %+v, want %+v", i, o, got, want)
			}
		}
	}
	if newOrigins([]string{baseOrigin}, []string{baseOrigin + "/"}) != nil {
		t.Error("the same origin written differently")
	}
}
