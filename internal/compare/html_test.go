package compare

import (
	"fmt"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

var htmlType = [][2]string{{"content-type", "text/html; charset=utf-8"}}

func htmlSide(body string) Side { return side(200, htmlType, body) }

func runHTML(t *testing.T, allow *allowlist.List, base, cur string) []Drift {
	t.Helper()
	return run(t, allow, LevelValues, htmlSide(base), htmlSide(cur))
}

func TestHTMLDrifts(t *testing.T) {
	const list = `<div id="app"><ul><li>a</li><li class="x">b</li><li>c</li><li>d</li></ul></div>`
	cases := []struct {
		name, base, cur, kind, where string
	}{
		{"text", list, strings.Replace(list, "<li>d</li>", "<li>e</li>", 1), KindText, "html>body>div#app>ul>li[3]"},
		{"attribute value", list, strings.Replace(list, `class="x"`, `class="y"`, 1), KindAttributeValue, "html>body>div#app>ul>li[1]@class"},
		{"attribute missing", list, strings.Replace(list, ` class="x"`, ``, 1), KindAttributeMissing, "html>body>div#app>ul>li[1]@class"},
		{"attribute extra", list, strings.Replace(list, `<li>c</li>`, `<li hidden>c</li>`, 1), KindAttributeExtra, "html>body>div#app>ul>li[2]@hidden"},
		{"element missing", list, strings.Replace(list, `<li>c</li>`, ``, 1), KindElementMissing, "html>body>div#app>ul>li[2]"},
		{"element extra", list, strings.Replace(list, `<ul>`, `<ul><li>new</li>`, 1), KindElementExtra, "html>body>div#app>ul>li[0]"},
		{"element extra, other tag", list, strings.Replace(list, `</ul>`, `</ul><p>new</p>`, 1), KindElementExtra, "html>body>div#app>p"},
		{"element order", `<h1>t</h1><p>x</p><ul></ul>`, `<h1>t</h1><ul></ul><p>x</p>`, KindElementOrder, "html>body"},
		{"element order by id", `<div id="a">1</div><div id="b">2</div>`, `<div id="b">3</div><div id="a">1</div>`, KindElementOrder, "html>body"},
		{"text by id after a move", `<div id="a">1</div><div id="b">2</div>`, `<div id="b">3</div><div id="a">1</div>`, KindText, "html>body>div#b"},
		{"root attribute", `<html lang="en"><p>x</p></html>`, `<html lang="de"><p>x</p></html>`, KindAttributeValue, "html@lang"},
		{"head", `<title>A</title><p>x</p>`, `<title>B</title><p>x</p>`, KindText, "html>head>title"},
		{"svg", `<svg><circle r="4"/></svg>`, `<svg><circle r="5"/></svg>`, KindAttributeValue, "html>body>svg>circle@r"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := runHTML(t, nil, c.base, c.cur)
			if !has(ds, c.kind, c.where) {
				t.Fatalf("want %s at %s, got %+v", c.kind, c.where, ds)
			}
		})
	}
}

// An element added at the start of a list is one extra element, not a
// changed text in every item after it.
func TestHTMLInsertIsOneDrift(t *testing.T) {
	ds := runHTML(t, nil, `<ul><li>a</li><li>b</li><li>c</li></ul>`, `<ul><li>new</li><li>a</li><li>b</li><li>c</li></ul>`)
	if len(ds) != 1 || ds[0].Kind != KindElementExtra || ds[0].Where != "html>body>ul>li[0]" {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, `<ul><li>a</li><li>b</li><li>c</li></ul>`, `<ul><li>a</li><li>c</li></ul>`)
	if len(ds) != 1 || ds[0].Kind != KindElementMissing || ds[0].Where != "html>body>ul>li[1]" {
		t.Fatalf("got %+v", ds)
	}
}

func TestHTMLWhitespace(t *testing.T) {
	compact := `<!DOCTYPE html><html><head><title>T</title></head><body><div id="app"><p>Hello <b>big</b> world</p></div></body></html>`
	pretty := "<!DOCTYPE html>\n<html>\n  <head>\n    <title>T</title>\n  </head>\n  <body>\n    <div id=\"app\">\n      <p>Hello\n        <b>big</b>   world</p>\n    </div>\n  </body>\n</html>\n"
	if ds := runHTML(t, nil, compact, pretty); len(ds) != 0 {
		t.Fatalf("whitespace between elements reported: %+v", ds)
	}
	for _, tag := range []string{"pre", "textarea", "script", "style"} {
		b := "<" + tag + ">a  b</" + tag + ">"
		c := "<" + tag + ">a b</" + tag + ">"
		parent := "html>body>"
		if tag == "script" || tag == "style" {
			parent = "html>head>"
		}
		if ds := runHTML(t, nil, b, c); !has(ds, KindText, parent+tag) {
			t.Errorf("%s: whitespace inside must count, got %+v", tag, ds)
		}
	}
	// Inside pre, whitespace of nested elements counts too.
	if ds := runHTML(t, nil, "<pre><code>a  b</code></pre>", "<pre><code>a b</code></pre>"); !has(ds, KindText, "html>body>pre>code") {
		t.Fatalf("got %+v", ds)
	}
}

// Whitespace between two inline elements shows as a space ("a b", not
// "ab"); between blocks, and at the start or end of an element, it is
// indentation.
func TestHTMLWhitespaceBetweenInlineElements(t *testing.T) {
	ds := runHTML(t, nil, `<p><b>a</b> <i>b</i></p>`, `<p><b>a</b><i>b</i></p>`)
	if len(ds) != 1 || !has(ds, KindText, "html>body>p") {
		t.Fatalf("space removed: %+v", ds)
	}
	ds = runHTML(t, nil, `<p><a href="/">x</a><span>y</span></p>`, "<p><a href=\"/\">x</a>\n  <span>y</span></p>")
	if len(ds) != 1 || !has(ds, KindText, "html>body>p") {
		t.Fatalf("space added: %+v", ds)
	}
	// Text next to an inline element: the space between them shows too.
	for _, c := range [][4]string{
		{`<p>a<b>x</b></p>`, `<p>a <b>x</b></p>`, `"a<b>"`, `"a <b>"`},
		{`<p><b>x</b> a</p>`, `<p><b>x</b>a</p>`, `"<b> a"`, `"<b>a"`},
		{`<p>Price: <b>9</b> EUR</p>`, `<p>Price:<b>9</b>EUR</p>`, `"Price: <b> EUR"`, `"Price:<b>EUR"`},
	} {
		ds := runHTML(t, nil, c[0], c[1])
		if len(ds) != 1 || !has(ds, KindText, "html>body>p") || ds[0].Baseline != c[2] || ds[0].Current != c[3] {
			t.Errorf("%s vs %s: %+v", c[0], c[1], ds)
		}
	}
	for _, same := range [][2]string{
		{`<p><b>a</b> <i>b</i></p>`, "<p><b>a</b>\n\t  <i>b</i></p>"},               // any whitespace is one space
		{`<p><b>a</b> <!-- --> <i>b</i></p>`, `<p><b>a</b> <i>b</i></p>`},           // split by a comment
		{`<div><p>a</p> <p>b</p></div>`, `<div><p>a</p><p>b</p></div>`},             // blocks
		{`<ul><li>a</li> <li>b</li></ul>`, "<ul>\n<li>a</li>\n<li>b</li>\n</ul>"},   // list items
		{`<p> <b>a</b> </p>`, `<p><b>a</b></p>`},                                    // start and end of a line
		{`<p><b>a</b> <div>b</div></p>`, `<p><b>a</b><div>b</div></p>`},             // inline then block
		{`<p><b>a</b> <my-tag>b</my-tag></p>`, `<p><b>a</b><my-tag>b</my-tag></p>`}, // unknown element
		{`<p><b>a</b> <br> <i>b</i></p>`, `<p><b>a</b><br><i>b</i></p>`},            // a line break
		{`<pre><b>a</b> <i>b</i></pre>`, `<pre><b>a</b> <i>b</i></pre>`},            // kept as it is
		{`<p>a <b>x</b> b</p>`, "<p>a\n  <b>x</b>\n  b</p>"},                        // any whitespace next to an element
		{`<div>a <div>x</div> b</div>`, `<div>a<div>x</div>b</div>`},                // next to a block
		{`<p> a <b>x</b></p>`, `<p>a <b>x</b></p>`},                                 // start of an element
		{`<p>a <br>b</p>`, `<p>a<br> b</p>`},                                        // around a line break
	} {
		if ds := runHTML(t, nil, same[0], same[1]); len(ds) != 0 {
			t.Errorf("%s vs %s: %+v", same[0], same[1], ds)
		}
	}
}

func TestHTMLSameTreeWrittenDifferently(t *testing.T) {
	// Attribute order, quotes, character references, comments and React's
	// text separators do not change the tree.
	base := `<p class="a" id="x" title="Tom &amp; Jerry">Hello world</p><!-- note -->`
	cur := `<p title='Tom &#38; Jerry' id=x class=a>Hello <!-- -->world</p>`
	if ds := runHTML(t, nil, base, cur); len(ds) != 0 {
		t.Fatalf("got %+v", ds)
	}
}

func TestHTMLTextAroundChildren(t *testing.T) {
	ds := runHTML(t, nil, `<p>Hello <b>big</b> world</p>`, `<p>Hello <b>big</b> there</p>`)
	if len(ds) != 1 || ds[0].Kind != KindText || ds[0].Where != "html>body>p" || ds[0].Baseline != `"Hello world"` {
		t.Fatalf("got %+v", ds)
	}
	// Text that moved past a child element is a change, too.
	if ds := runHTML(t, nil, `<p>a <b>x</b> b</p>`, `<p>a b <b>x</b></p>`); !has(ds, KindText, "html>body>p") {
		t.Fatalf("got %+v", ds)
	}
}

func TestHTMLLongTextExcerpt(t *testing.T) {
	long := strings.Repeat("x", 300)
	ds := runHTML(t, nil, "<script>"+long+"var a=1;"+long+"</script>", "<script>"+long+"var a=2;"+long+"</script>")
	if len(ds) != 1 || !strings.Contains(ds[0].Baseline, "a=1") || !strings.HasPrefix(ds[0].Current, "…") || len(ds[0].Current) > 120 {
		t.Fatalf("got %+v", ds)
	}
	if ds[0].Texts[1] != long+"var a=2;"+long {
		t.Fatal("the whole text is kept for a format proposal")
	}
}

func TestHTMLLevels(t *testing.T) {
	base, cur := htmlSide(`<p class="a">x</p>`), htmlSide(`<p class="b">y</p><p>z</p>`)
	ds := run(t, nil, LevelShape, base, cur)
	if len(ds) != 1 || ds[0].Kind != KindElementExtra {
		t.Fatalf("shape: %+v", ds)
	}
	if ds := run(t, nil, LevelValues, base, cur); len(ds) != 3 {
		t.Fatalf("values: %+v", ds)
	}
}

func TestHTMLOnlyByContentType(t *testing.T) {
	// Without text/html on both sides the body is compared as before.
	ds := run(t, nil, LevelValues, side(200, nil, `<p>a</p>`), side(200, nil, `<p>b</p>`))
	if !has(ds, KindBody, "") {
		t.Fatalf("got %+v", ds)
	}
}

func TestEmbeddedJSON(t *testing.T) {
	next := func(title string) string {
		return `<div id="__next"><h1>x</h1></div><script id="__NEXT_DATA__" type="application/json">{"props":{"pageProps":{"title":"` + title + `"}},"buildId":"b1"}</script>`
	}
	ds := runHTML(t, nil, next("Products"), next("Goods"))
	if len(ds) != 1 || !has(ds, KindValue, "html>body>script#__NEXT_DATA__$.props.pageProps.title") {
		t.Fatalf("got %+v", ds)
	}
	ld := func(s string) string { return `<script type="application/ld+json">` + s + `</script>` }
	if ds := runHTML(t, nil, ld(`{"@type":"Organization","name":"A"}`), ld(`{"@type":"Organization","name":"B"}`)); !has(ds, KindValue, "html>head>script$.name") {
		t.Fatalf("ld+json: %+v", ds)
	}
	// The same values written differently.
	if ds := runHTML(t, nil, ld(`{"a":1}`), ld(`{"a": 1}`)); len(ds) != 1 || !has(ds, KindEncoding, "html>head>script$") {
		t.Fatalf("layout: %+v", ds)
	}
	// Not JSON on one side: compared as text.
	if ds := runHTML(t, nil, ld(`{"a":1}`), ld(`{"a":`)); !has(ds, KindText, "html>head>script") {
		t.Fatalf("broken JSON: %+v", ds)
	}
	// A script that is not JSON is text.
	if ds := runHTML(t, nil, `<script>var a=1</script>`, `<script>var a=2</script>`); !has(ds, KindText, "html>head>script") {
		t.Fatalf("script: %+v", ds)
	}
}

func nuxtPage(payload string) string {
	return `<div id="__nuxt"></div><script type="application/json" data-nuxt-data="nuxt-app" data-ssr="true" id="__NUXT_DATA__">` + payload + `</script>`
}

func TestNuxtPayload(t *testing.T) {
	const base = `[["ShallowReactive",1],{"data":2,"state":7},["ShallowReactive",3],{"page":4},{"title":5,"at":6},"Home",["Date","2026-10-29T10:00:00.000Z"],{"open":-1}]`
	// The title changes, and a new value moves every index after it.
	const cur = `[["ShallowReactive",1],{"data":2,"state":8},["ShallowReactive",3],{"page":4},{"title":5,"lead":7,"at":6},"About",["Date","2026-10-29T10:00:00.000Z"],"x",{"open":-1}]`
	ds := runHTML(t, nil, nuxtPage(base), nuxtPage(cur))
	const root = "html>body>script#__NUXT_DATA__$"
	if !has(ds, KindValue, root+".data.page.title") || !has(ds, KindKeyExtra, root+".data.page.lead") || len(ds) != 2 {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, nuxtPage(base), nuxtPage(strings.Replace(base, "10:00", "11:00", 1)))
	if !has(ds, KindValue, root+".data.page.at.Date") {
		t.Fatalf("date: %+v", ds)
	}
	// Special numbers, a set, a map and a value that refers back to the root.
	v, ok := devalue(mustJSON(t, `[{"a":-3,"b":1,"c":2,"self":0},["Set",3],["Map",3,-1],"k"]`))
	if !ok {
		t.Fatal("not decoded")
	}
	if got := string(jsonx.Marshal(v, false)); got != `{"a":NaN,"b":{"Set":["k"]},"c":{"Map":[["k",undefined]]},"self":{"$ref":"$"}}` {
		t.Fatalf("decoded %s", got)
	}
	// Not in devalue's form: compared as a plain array.
	if _, ok := devalue(mustJSON(t, `[{"a":"x"}]`)); ok {
		t.Fatal("an object of strings decoded")
	}
	ds = runHTML(t, nil, nuxtPage(`[{"a":"x"}]`), nuxtPage(`[{"a":"y"}]`))
	if !has(ds, KindValue, root+"[0].a") {
		t.Fatalf("plain: %+v", ds)
	}
}

func mustJSON(t *testing.T, s string) *jsonx.Value {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func flightPage(build string, chunks ...string) string {
	var b strings.Builder
	b.WriteString(`<div>x</div><script>(self.__next_f=self.__next_f||[]).push([0])</script>`)
	b.WriteString(`<script>self.__next_f.push([1,"0:{\"P\":null,\"b\":\"` + build + `\"}\n"])</script>`)
	for _, c := range chunks {
		b.WriteString(`<script>self.__next_f.push(` + c + `)</script>`)
	}
	return b.String()
}

func TestFlightPayload(t *testing.T) {
	base := flightPage("A", `[1,"1:I[123,[\"/_next/static/chunks/a1.js\"],\"default\"]\n2:T5,hello3:[\"$\",\"p\",null,{\"children\":\"Hi\"}]\n"]`)
	// Same payload, cut into chunks at other places.
	same := flightPage("A", `[1,"1:I[123,[\"/_next/static/chunks/a1.js\"],\"defa"]`, `[1,"ult\"]\n2:T5,hel"]`, `[1,"lo3:[\"$\",\"p\",null,{\"children\":\"Hi\"}]\n"]`)
	if ds := runHTML(t, nil, base, same); len(ds) != 0 {
		t.Fatalf("chunking reported: %+v", ds)
	}
	cur := flightPage("B", `[1,"1:I[123,[\"/_next/static/chunks/b2.js\"],\"default\"]\n2:T5,howdy3:[\"$\",\"p\",null,{\"children\":\"Ho\"}]\n"]`)
	ds := runHTML(t, nil, base, cur)
	for _, w := range []string{"self.__next_f$.0.b", "self.__next_f$.1.I[1][0]", "self.__next_f$.2.T", "self.__next_f$.3[3].children"} {
		if !has(ds, KindValue, w) {
			t.Errorf("no value drift at %s: %+v", w, ds)
		}
	}
	// The text of the scripts that carry the payload is compared as the
	// payload only.
	for _, d := range ds {
		if strings.HasPrefix(d.Where, "html") {
			t.Errorf("payload compared as text of an element: %+v", d)
		}
	}
	if ds := runHTML(t, nil, base, `<div>x</div>`); !has(ds, KindElementMissing, FlightPath) {
		t.Fatalf("payload gone: %+v", ds)
	}
	// A row that does not parse is kept as text.
	v := flightValue([]string{`[1,"0:{broken\n5:\"ok\"\n"]`, `not json`})
	if got := string(jsonx.Marshal(v, false)); got != `{"push":"not json","0":"{broken","5":"ok"}` {
		t.Fatalf("got %s", got)
	}
}

func htmlAllow(t *testing.T, rules string) *allowlist.List {
	t.Helper()
	l, err := allowlist.Parse([]byte("version: 1\nbody:\n" + rules))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestHTMLAllowlist(t *testing.T) {
	allow := htmlAllow(t, `  - path: html>body>script#__NEXT_DATA__$.buildId
    format: '[\w-]+'
    reason: build id
  - path: html*
    format: '/_next/static/[\w/.~-]+'
    parts: true
    reason: hashed assets
  - path: html>body>footer>p
    format: '\d+ ms'
    parts: true
    reason: render time
  - path: html>*@nonce
    format: '[A-Za-z0-9+/=]+'
    reason: nonce
`)
	pg := func(build, hash, ms, nonce, title string) string {
		return `<script src="/_next/static/chunks/` + hash + `.js" nonce="` + nonce + `"></script>` +
			`<link rel="preload" href="/_next/static/` + build + `/_buildManifest.js">` +
			`<h1>` + title + `</h1><footer><p>Rendered in ` + ms + ` ms</p></footer>` +
			`<script id="__NEXT_DATA__" type="application/json">{"props":{"title":"` + title + `"},"buildId":"` + build + `"}</script>`
	}
	base := pg("b1", "a1b2", "12", "n0nce", "Home")
	if ds := runHTML(t, allow, base, pg("b2", "c3d4", "7", "other", "Home")); len(ds) != 0 {
		t.Fatalf("build noise reported: %+v", ds)
	}
	ds := runHTML(t, allow, base, pg("b2", "c3d4", "7", "other", "About"))
	if len(ds) != 2 || !has(ds, KindText, "html>body>h1") || !has(ds, KindValue, "html>body>script#__NEXT_DATA__$.props.title") {
		t.Fatalf("real change: %+v", ds)
	}
	// A parts rule still sees what is around the parts.
	ds = runHTML(t, allow, base, strings.Replace(base, "/_next/static/chunks/a1b2.js", "/cdn/x.js", 1))
	if !has(ds, KindAttributeValue, "html>head>script@src") {
		t.Fatalf("src outside _next: %+v", ds)
	}
	ds = runHTML(t, allow, base, strings.Replace(base, "Rendered in 12 ms", "Rendered in 12 s", 1))
	if !has(ds, KindText, "html>body>footer>p") {
		t.Fatalf("render time: %+v", ds)
	}
	// A whole-value format decides alone; it judges only a value that
	// changed.
	ds = runHTML(t, allow, base, strings.Replace(base, `"buildId":"b1"`, `"buildId":"b 1"`, 1))
	if !has(ds, KindFormat, "html>body>script#__NEXT_DATA__$.buildId") {
		t.Fatalf("format: %+v", ds)
	}
	ds = runHTML(t, allow, strings.Replace(base, "n0nce", "n0nce!", 1), strings.Replace(base, "n0nce", "n0nce!", 1)+" ")
	if len(ds) != 0 {
		t.Fatalf("unchanged nonce: %+v", ds)
	}
	ds = runHTML(t, allow, base, strings.Replace(base, "n0nce", "n0nce!", 1))
	if !has(ds, KindFormat, "html>head>script@nonce") {
		t.Fatalf("nonce: %+v", ds)
	}
}

// A parts rule works on JSON bodies too, and a whole-value rule for the same
// path wins over it wherever it stands in the file.
func TestPartsRuleInJSON(t *testing.T) {
	allow := htmlAllow(t, `  - path: $.*
    format: '[0-9a-f]{8}'
    parts: true
    reason: hashes
  - path: $.b
    format: 'x'
    reason: fixed
`)
	ds := run(t, allow, LevelValues, side(200, nil, `{"a":"app.0011aabb.js","b":"x","c":"v1"}`), side(200, nil, `{"a":"app.ccdd2233.js","b":"y","c":"v2"}`))
	if len(ds) != 2 || !has(ds, KindFormat, "$.b") || !has(ds, KindValue, "$.c") {
		t.Fatalf("got %+v", ds)
	}
}

// A page nested deeper than the parser accepts (512 elements) is compared as
// bytes, and the drift says why; it must not crash.
func TestHTMLTooDeep(t *testing.T) {
	deep := strings.Repeat("<div>", 600) + "x"
	ds := runHTML(t, nil, deep, strings.Replace(deep, "x", "y", 1))
	if len(ds) != 1 || ds[0].Kind != KindBody || !strings.Contains(ds[0].Current, "not parsed as HTML") {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, `<p>x</p>`, deep)
	if len(ds) != 1 || ds[0].Kind != KindBody || strings.Contains(ds[0].Baseline, "not parsed") || !strings.Contains(ds[0].Current, "not parsed") {
		t.Fatalf("one side too deep: %+v", ds)
	}
}

// Where a text stands among the child elements counts: text that moved past
// a child is a drift, while an inserted element is still one drift.
func TestHTMLTextPosition(t *testing.T) {
	ds := runHTML(t, nil, `<p><b>5</b> EUR</p>`, `<p>EUR <b>5</b></p>`)
	if len(ds) != 1 || ds[0].Kind != KindText || ds[0].Where != "html>body>p" || ds[0].Baseline != `"<b> EUR"` || ds[0].Current != `"EUR <b>"` {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, `<label><input type="checkbox">Accept terms</label>`, `<label>Accept terms<input type="checkbox"></label>`)
	if !has(ds, KindText, "html>body>label") {
		t.Fatalf("label: %+v", ds)
	}
	// A parts rule allows its parts, not a move.
	allow := htmlAllow(t, "  - path: html>body>p\n    format: '\\d+'\n    parts: true\n    reason: x\n")
	if ds := runHTML(t, allow, `<p><b>x</b> 10 EUR</p>`, `<p>11 EUR <b>x</b></p>`); !has(ds, KindText, "html>body>p") {
		t.Fatalf("parts: %+v", ds)
	}
	if ds := runHTML(t, allow, `<p><b>x</b> 10 EUR</p>`, `<p><b>x</b> 11 EUR</p>`); len(ds) != 0 {
		t.Fatalf("parts in place: %+v", ds)
	}
	// An element added or removed between texts does not move them.
	ds = runHTML(t, nil, `<p>Hello <b>big</b> world</p>`, `<p><i>new</i>Hello <b>big</b> world</p>`)
	if len(ds) != 1 || ds[0].Kind != KindElementExtra || ds[0].Where != "html>body>p>i" {
		t.Fatalf("inserted: %+v", ds)
	}
	ds = runHTML(t, nil, `<p>Hello <i>x</i> big <b>y</b> world</p>`, `<p>Hello big <b>y</b> world</p>`)
	if len(ds) != 1 || ds[0].Kind != KindElementMissing || ds[0].Where != "html>body>p>i" {
		t.Fatalf("removed: %+v", ds)
	}
}

// listing and xmp keep their whitespace, like pre.
func TestHTMLWhitespaceListingXmp(t *testing.T) {
	for _, tag := range []string{"listing", "xmp"} {
		if ds := runHTML(t, nil, "<"+tag+">a  b</"+tag+">", "<"+tag+">a b</"+tag+">"); !has(ds, KindText, "html>body>"+tag) {
			t.Errorf("%s: got %+v", tag, ds)
		}
	}
}

// A script that pushes the RSC payload is still an element: its attributes
// and its place are compared, its text only as the payload.
func TestFlightScriptAttributes(t *testing.T) {
	const push = `self.__next_f.push([1,"0:{}\n"])`
	ds := runHTML(t, nil, `<p>x</p><script>`+push+`</script>`, `<p>x</p><script type="text/plain" data-x="1">`+push+`</script>`)
	if len(ds) != 2 || !has(ds, KindAttributeExtra, "html>body>script@type") || !has(ds, KindAttributeExtra, "html>body>script@data-x") {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, `<script nonce="a">`+push+`</script><script nonce="a">`+push+`</script>`, `<script nonce="a">`+push+`</script><script nonce="b">`+push+`</script>`)
	if !has(ds, KindElementExtra, "html>head>script[1]") {
		t.Fatalf("one script of the payload changed: %+v", ds)
	}
	ds = runHTML(t, nil, `<p>x</p><div id="a"></div><script>`+push+`</script>`, `<p>x</p><script>`+push+`</script><div id="a"></div>`)
	if !has(ds, KindElementOrder, "html>body") {
		t.Fatalf("moved: %+v", ds)
	}
}

// Ids and attribute names that hold path syntax are quoted, so a path names
// one place and a rule for attributes (html>*@nonce) does not match a text.
func TestHTMLPathQuoting(t *testing.T) {
	allow := htmlAllow(t, "  - path: html>*@nonce\n    format: '[A-Za-z0-9+/=_-]+'\n    reason: nonce\n")
	ds := runHTML(t, allow, `<div id="x@nonce">Price 100</div>`, `<div id="x@nonce">Price200</div>`)
	if len(ds) != 1 || ds[0].Kind != KindText || ds[0].Where != `html>body>div#"x@nonce"` {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, `<button @click="a" id="b">x</button>`, `<button @click="c" id="b">x</button>`)
	if len(ds) != 1 || ds[0].Where != `html>body>button#b@"@click"` {
		t.Fatalf("attribute: %+v", ds)
	}
	ds = runHTML(t, nil, `<div id="a b">x</div>`, `<div id="a b">y</div>`)
	if !has(ds, KindText, `html>body>div#"a b"`) {
		t.Fatalf("space: %+v", ds)
	}
	if ds := runHTML(t, nil, `<div id="app-1">x</div>`, `<div id="app-1">y</div>`); !has(ds, KindText, "html>body>div#app-1") {
		t.Fatalf("plain id: %+v", ds)
	}
}

// any_length is about arrays: it does not allow a text or an attribute of a
// page, and on JSON in a page it still allows the length.
func TestHTMLAnyLengthRule(t *testing.T) {
	allow := htmlAllow(t, "  - path: html>body>ul*\n    any_length: true\n    reason: x\n")
	ds := runHTML(t, allow, `<ul><li title="a">Price 100</li></ul>`, `<ul><li title="b">Price 999</li></ul>`)
	if !has(ds, KindText, "html>body>ul>li") || !has(ds, KindAttributeValue, "html>body>ul>li@title") {
		t.Fatalf("got %+v", ds)
	}
	// A later rule with a format still applies.
	allow = htmlAllow(t, "  - path: html*\n    any_length: true\n    reason: x\n  - path: html>body>p\n    format: '\\d+'\n    reason: y\n")
	if ds := runHTML(t, allow, `<p>1</p>`, `<p>2</p>`); len(ds) != 0 {
		t.Fatalf("later rule: %+v", ds)
	}
	ld := func(s string) string { return `<script type="application/ld+json">` + s + `</script>` }
	allow = htmlAllow(t, "  - path: html>head>script$.items\n    any_length: true\n    reason: x\n")
	if ds := runHTML(t, allow, ld(`{"items":[1]}`), ld(`{"items":[1,2]}`)); len(ds) != 0 {
		t.Fatalf("JSON in a page: %+v", ds)
	}
}

func TestFlightTextRowLength(t *testing.T) {
	for _, row := range []string{"0:T-5,hello\n", "0:T7fffffffffffffff,hello\n", "0:Tffffffffffffffff,hello\n"} {
		v := flightValue([]string{`[1,` + jsonx.Quote(row) + `]`})
		if got := string(jsonx.Marshal(v, false)); !strings.Contains(got, "hello") {
			t.Errorf("%q: got %s", row, got)
		}
		page := func(s string) string {
			return `<p>x</p><script>self.__next_f.push([1,` + jsonx.Quote(s) + `])</script>`
		}
		if ds := runHTML(t, nil, page(row), page(strings.Replace(row, "hello", "hellx", 1))); len(ds) != 1 {
			t.Errorf("%q: got %+v", row, ds)
		}
	}
}

// A payload that decodes deeper than JSON may be nested, or larger than the
// bound, is compared as the plain array.
func TestNuxtPayloadTooDeep(t *testing.T) {
	chain := func(n int, end string) string {
		var b strings.Builder
		b.WriteString("[")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "[%d],", i+1)
		}
		return b.String() + `"` + end + `"]`
	}
	if _, ok := devalue(mustJSON(t, chain(jsonx.MaxDepth+10, "end"))); ok {
		t.Fatal("decoded deeper than jsonx.MaxDepth")
	}
	if _, ok := devalue(mustJSON(t, chain(100, "end"))); !ok {
		t.Fatal("a short chain not decoded")
	}
	const n = 200000
	ds := runHTML(t, nil, nuxtPage(chain(n, "end")), nuxtPage(chain(n, "enx")))
	if len(ds) != 1 || !has(ds, KindValue, fmt.Sprintf("html>body>script#__NUXT_DATA__$[%d]", n)) {
		t.Fatalf("got %+v", ds)
	}
	// Shared values that would expand without end.
	var dag strings.Builder
	dag.WriteString("[")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&dag, "[%d,%d],", i+1, i+1)
	}
	dag.WriteString(`"end"]`)
	if ds := runHTML(t, nil, nuxtPage(dag.String()), nuxtPage(strings.Replace(dag.String(), "end", "enx", 1))); len(ds) != 1 {
		t.Fatalf("dag: %+v", ds)
	}
}

// A value that refers back to one that contains it is the path of that
// value, so another target is a drift at that place.
func TestNuxtPayloadCycles(t *testing.T) {
	const root = "html>body>script#__NUXT_DATA__$"
	ds := runHTML(t, nil, nuxtPage(`[{"self":0,"child":1},{"p":0}]`), nuxtPage(`[{"self":0,"child":1},{"p":1}]`))
	if len(ds) != 1 || !has(ds, KindValue, root+".child.p.$ref") || ds[0].Baseline != `"$"` || ds[0].Current != `"$.child"` {
		t.Fatalf("got %+v", ds)
	}
	ds = runHTML(t, nil, nuxtPage(`[{"a":1},{"x":0,"y":2},{"z":1}]`), nuxtPage(`[{"a":1},{"x":2,"y":2},{"z":1}]`))
	if !has(ds, KindKeyMissing, root+".a.x.$ref") {
		t.Fatalf("other target: %+v", ds)
	}
	v, ok := devalue(mustJSON(t, `[{"m":1},["Map",2,0],"k"]`))
	if got := string(jsonx.Marshal(v, false)); !ok || got != `{"m":{"Map":[["k",{"$ref":"$"}]]}}` {
		t.Fatalf("map: %s", got)
	}
}
