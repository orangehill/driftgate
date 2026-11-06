package compare

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// The false-pass suite: realistic responses a frontend reads (a Laravel
// pagination envelope, a nested resource, a top-level list, an HTML page,
// an XLSX export), and one mutation of every class of change. Every
// mutation must be reported with its kind at its place from the lowest
// level that can see it, with no allowlist and with the rules a project
// typically has (time formats, any_length, a $raw rule, a cell rule, asset
// hashes, items paired by key): a rule may allow only what it names.
// Identical responses give no drift at any level. On failure the test
// prints the detection matrix (mutation class × level × allowlist).

// resp is a response of the corpus.
type resp struct {
	status  int
	headers [][2]string
	body    string
}

var (
	jsonHeaders = [][2]string{{"content-type", "application/json"}, {"cache-control", "no-cache, private"}, {"date", "Fri, 30 Oct 2026 10:00:00 GMT"}}
	pageHeaders = [][2]string{{"content-type", "text/html; charset=UTF-8"}, {"date", "Fri, 30 Oct 2026 10:00:00 GMT"}}
)

// Laravel's JsonResponse escapes slashes and non-ASCII characters.
const usersPage = `{"current_page":1,"data":[` +
	`{"id":1,"name":"Ana Jovanovi\u0107","email":"ana@example.com","email_verified_at":null,"external_id":12345678901234567890,"balance":1250.5,"score":10,"active":true,"roles":["admin","editor"],"profile":{"city":"Novi Sad","avatar":"https:\/\/cdn.example.com\/a\/1.png","bio":""},"created_at":"2026-01-02T10:00:00.000000Z","updated_at":"2026-10-30T08:15:00.000000Z"},` +
	`{"id":2,"name":"Marko Markovi\u0107","email":"marko@example.com","email_verified_at":"2026-02-01T09:00:00.000000Z","external_id":12345678901234567891,"balance":0,"score":7,"active":false,"roles":["viewer"],"profile":{"city":"Beograd","avatar":null,"bio":"Hi \ud83d\udc4b"},"created_at":"2026-01-03T10:00:00.000000Z","updated_at":"2026-10-29T08:15:00.000000Z"},` +
	`{"id":3,"name":"\u017deljko","email":"z@example.com","email_verified_at":null,"external_id":3,"balance":-12.75,"score":0,"active":true,"roles":[],"profile":{"city":"Ni\u0161","avatar":null,"bio":"<b>x<\/b>"},"created_at":"2026-01-04T10:00:00.000000Z","updated_at":"2026-10-28T08:15:00.000000Z"}],` +
	`"first_page_url":"http:\/\/localhost\/api\/users?page=1","from":1,"last_page":3,"links":[{"url":null,"label":"&laquo; Previous","active":false},{"url":"http:\/\/localhost\/api\/users?page=1","label":"1","active":true}],` +
	`"next_page_url":"http:\/\/localhost\/api\/users?page=2","path":"http:\/\/localhost\/api\/users","per_page":3,"prev_page_url":null,"to":3,"total":9}`

// A resource with nested objects, money as strings and floats, a
// pretty-printed layout (a Symfony or Django API).
const orderDoc = `{
    "order": {
        "number": "2026-000123",
        "total": "1499.90",
        "tax_rate": 0.2,
        "paid": true,
        "placed_at": "2026-10-30 08:15:00",
        "customer": {"id": 77, "name": "Ana"},
        "lines": [
            {"sku": "A-1", "qty": 2, "price": 499.95},
            {"sku": "B-2", "qty": 1, "price": 500.0}
        ]
    }
}
`

const tagList = `[{"id":1,"slug":"news","count":12},{"id":2,"slug":"sport","count":3}]`

const usersHTML = `<!DOCTYPE html>
<html lang="sr">
<head><meta charset="utf-8"><title>Users</title><script type="module" src="/build/assets/app-4f3a2b1c.js"></script></head>
<body>
<div id="app" class="container">
<h1 class="title">Users</h1>
<ul class="users"><li data-id="1">Ana</li><li data-id="2">Marko</li></ul>
<p>Total: <b>9</b> <i>members</i> today in Niš</p>
<script id="__NEXT_DATA__" type="application/json">{"props":{"total":9,"user":"ana"}}</script>
</div>
</body>
</html>
`

const xlsxStyles = `<numFmts><numFmt numFmtId="164" formatCode="0.00"/></numFmts><cellXfs><xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="14"/></cellXfs>`

// xlsxCells is the worksheet S of the export: A1 the time of the export,
// B1 a number, B2 a formula over it, C1 a number with a format, D1 an
// empty cell with a style, E1 a generated code and EA1 a number past its
// column, C2 a formula with an empty result and D2 one without a value,
// A3 a text, A5 a counter and A15 a number past its row.
const xlsxCells = `<row r="1"><c r="A1" t="inlineStr"><is><t>Exported 30.10.2026 10:00</t></is></c><c r="B1"><v>42</v></c><c r="C1" s="1"><v>3.5</v></c>` +
	`<c r="D1" s="1"/><c r="E1" t="inlineStr"><is><t>0badc0de</t></is></c><c r="EA1"><v>12345678</v></c></row>` +
	`<row r="2"><c r="A2" t="inlineStr"><is><t>Ana</t></is></c><c r="B2"><f>B1*2</f><v>84</v></c><c r="C2"><f>SUM(B1:B2)</f><v></v></c><c r="D2"><f>TODAY()</f></c></row>` +
	`<row r="3"><c r="A3" t="inlineStr"><is><t>Total</t></is></c></row>` +
	`<row r="5"><c r="A5"><v>1</v></c></row><row r="15"><c r="A15"><v>1</v></c></row>`

// typicalRules are the rules a project has: time formats, a list that may
// grow, a $raw rule for a framework's redirect page, the export's time
// cell, an asset hash in pages, the date header.
const typicalRules = `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: time
body:
  - path: $.data[*].updated_at
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z'
    reason: time
  - path: $.order.placed_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: time
  - path: $.data[*].roles
    any_length: true
    reason: roles may be added
  - path: $raw
    format: '(?s)<!DOCTYPE html>\s*<html>\s*<head>\s*<meta charset="UTF-8" />\s*<meta http-equiv="refresh" content="0;url=.*'
    reason: the framework's redirect page
  - path: xlsx:*!A1
    format: 'Exported \d{2}\.\d{2}\.\d{4} \d{2}:\d{2}'
    reason: time of the export
  - path: xlsx:*!E*
    format: '[0-9a-f]{8}'
    reason: generated codes in column E
  - path: xlsx:*!*5
    format: '\d+'
    reason: counters in row 5
  - path: html*
    format: '-[0-9a-f]{8}\.js'
    parts: true
    reason: asset hash
`

// keyedRules adds arrays paired by key.
const keyedRules = typicalRules + `arrays:
  - path: $.data
    key: id
    reason: users by id
  - path: $.order.lines
    key: sku
    reason: lines by sku
`

// fpCase is one mutation: what it does to a corpus response and the drift
// it must give (kind at where) from level on. ruled, when set, is the
// kind with the typical rules (a value a rule judges is a format drift);
// keyed the kind and place with the arrays paired by key ("" kind: the
// same kind).
type fpCase struct {
	class  string
	doc    string
	mutate func(t *testing.T, r resp) resp
	kind   string
	where  string
	level  Level
	ruled  string
	keyed  *[2]string
}

// replace changes the one occurrence of old in the body.
func replace(old, new string) func(*testing.T, resp) resp {
	return func(t *testing.T, r resp) resp {
		t.Helper()
		if n := strings.Count(r.body, old); n != 1 {
			t.Fatalf("mutation %q → %q: %d occurrences", old, new, n)
		}
		r.body = strings.Replace(r.body, old, new, 1)
		return r
	}
}

func setHeader(name, value string) func(*testing.T, resp) resp {
	return func(_ *testing.T, r resp) resp {
		var hs [][2]string
		set := false
		for _, h := range r.headers {
			if strings.EqualFold(h[0], name) {
				if value != "" && !set {
					hs = append(hs, [2]string{h[0], value})
				}
				set = true
				continue
			}
			hs = append(hs, h)
		}
		if !set {
			hs = append(hs, [2]string{name, value})
		}
		r.headers = hs
		return r
	}
}

func keyedAt(kind, where string) *[2]string { return &[2]string{kind, where} }

var fpCases = []fpCase{
	// JSON values.
	{class: "type: int → string", doc: "users", mutate: replace(`"score":10,`, `"score":"10",`), kind: KindType, where: "$.data[0].score", level: LevelTypes, keyed: keyedAt("", "$.data[id=1].score")},
	{class: "type: int → float literal", doc: "users", mutate: replace(`"score":7,`, `"score":7.0,`), kind: KindType, where: "$.data[1].score", level: LevelTypes, keyed: keyedAt("", "$.data[id=2].score")},
	{class: "type: object → array", doc: "order", mutate: replace(`"customer": {"id": 77, "name": "Ana"}`, `"customer": []`), kind: KindType, where: "$.order.customer", level: LevelShape},
	{class: "type: bool → int", doc: "order", mutate: replace(`"paid": true`, `"paid": 1`), kind: KindType, where: "$.order.paid", level: LevelTypes},
	{class: "value: string", doc: "users", mutate: replace(`"email":"ana@example.com"`, `"email":"ana@example.org"`), kind: KindValue, where: "$.data[0].email", level: LevelValues, keyed: keyedAt("", "$.data[id=1].email")},
	{class: "value: bool", doc: "users", mutate: replace(`"active":false,"roles"`, `"active":true,"roles"`), kind: KindValue, where: "$.data[1].active", level: LevelValues, keyed: keyedAt("", "$.data[id=2].active")},
	{class: "value: money string", doc: "order", mutate: replace(`"1499.90"`, `"1499.9"`), kind: KindValue, where: "$.order.total", level: LevelValues},
	{class: "value: big int last digit", doc: "users", mutate: replace(`12345678901234567890`, `12345678901234567899`), kind: KindValue, where: "$.data[0].external_id", level: LevelValues, keyed: keyedAt("", "$.data[id=1].external_id")},
	{class: "value: float", doc: "order", mutate: replace(`"price": 499.95`, `"price": 499.94`), kind: KindValue, where: "$.order.lines[0].price", level: LevelValues, keyed: keyedAt("", `$.order.lines[sku="A-1"].price`)},
	{class: "value: negative zero", doc: "users", mutate: replace(`"score":0,`, `"score":-0,`), kind: KindNumberLiteral, where: "$.data[2].score", level: LevelValues, keyed: keyedAt("", "$.data[id=3].score")},
	{class: "value: unicode NFC → NFD", doc: "users", mutate: replace(`Ana Jovanovi\u0107`, `Ana Jovanovic\u0301`), kind: KindValue, where: "$.data[0].name", level: LevelValues, keyed: keyedAt("", "$.data[id=1].name")},
	{class: "value: allowed time in another format", doc: "users", mutate: replace(`"2026-10-30T08:15:00.000000Z"`, `"2026-10-30 08:15:00"`), kind: KindValue, where: "$.data[0].updated_at", level: LevelValues, ruled: KindFormat, keyed: keyedAt("", "$.data[id=1].updated_at")},
	{class: "number literal: 1.0 vs 1.00", doc: "order", mutate: replace(`"price": 500.0`, `"price": 500.00`), kind: KindNumberLiteral, where: "$.order.lines[1].price", level: LevelValues, keyed: keyedAt("", `$.order.lines[sku="B-2"].price`)},
	{class: "number literal: 1250.5 vs 1.2505e3", doc: "users", mutate: replace(`1250.5`, `1.2505e3`), kind: KindNumberLiteral, where: "$.data[0].balance", level: LevelValues, keyed: keyedAt("", "$.data[id=1].balance")},
	{class: "null → missing", doc: "users", mutate: replace(`"email":"z@example.com","email_verified_at":null,`, `"email":"z@example.com",`), kind: KindKeyMissing, where: "$.data[2].email_verified_at", level: LevelShape, keyed: keyedAt("", "$.data[id=3].email_verified_at")},
	{class: `null → ""`, doc: "users", mutate: replace(`"prev_page_url":null`, `"prev_page_url":""`), kind: KindType, where: "$.prev_page_url", level: LevelTypes},
	{class: `"" → null`, doc: "users", mutate: replace(`"bio":""`, `"bio":null`), kind: KindType, where: "$.data[0].profile.bio", level: LevelTypes, keyed: keyedAt("", "$.data[id=1].profile.bio")},
	{class: "missing → null (key added)", doc: "users", mutate: replace(`"to":3,`, `"to":3,"deleted_at":null,`), kind: KindKeyExtra, where: "$.deleted_at", level: LevelShape},
	{class: "key removed", doc: "users", mutate: replace(`"city":"Beograd",`, ``), kind: KindKeyMissing, where: "$.data[1].profile.city", level: LevelShape, keyed: keyedAt("", "$.data[id=2].profile.city")},
	{class: "key renamed", doc: "order", mutate: replace(`"tax_rate"`, `"taxRate"`), kind: KindKeyExtra, where: "$.order.taxRate", level: LevelShape},
	{class: "key order", doc: "users", mutate: replace(`"from":1,"last_page":3,`, `"last_page":3,"from":1,`), kind: KindKeyOrder, where: "$", level: LevelShape},
	{class: "item added", doc: "users", mutate: replace(`],"first_page_url"`, `,{"id":4,"name":"Novi"}],"first_page_url"`), kind: KindArrayLength, where: "$.data", level: LevelShape, keyed: keyedAt(KindKeyExtra, "$.data[id=4]")},
	{class: "item removed", doc: "tags", mutate: replace(`,{"id":2,"slug":"sport","count":3}`, ``), kind: KindArrayLength, where: "$", level: LevelShape},
	{class: "any_length list: item added, one changed", doc: "users", mutate: replace(`"roles":["viewer"]`, `"roles":["admin","viewer"]`), kind: KindValue, where: "$.data[1].roles[0]", level: LevelValues, keyed: keyedAt("", "$.data[id=2].roles[0]")},
	{class: "items reordered", doc: "users", mutate: replace(`"roles":["admin","editor"]`, `"roles":["editor","admin"]`), kind: KindItemOrder, where: "$.data[0].roles", level: LevelValues, keyed: keyedAt("", "$.data[id=1].roles")},
	{class: "keyed items reordered", doc: "order", mutate: replace(`{"sku": "A-1", "qty": 2, "price": 499.95},
            {"sku": "B-2", "qty": 1, "price": 500.0}`, `{"sku": "B-2", "qty": 1, "price": 500.0},
            {"sku": "A-1", "qty": 2, "price": 499.95}`), kind: KindItemOrder, where: "$.order.lines", level: LevelValues, keyed: keyedAt(KindItemOrder, "$.order.lines")},
	{class: "keyed item replaced", doc: "order", mutate: replace(`"sku": "B-2"`, `"sku": "B-3"`), kind: KindValue, where: "$.order.lines[1].sku", level: LevelValues, keyed: keyedAt(KindKeyMissing, `$.order.lines[sku="B-2"]`)},
	// Encoding of the same values.
	{class: "escaping: slash unescaped", doc: "users", mutate: replace(`"https:\/\/cdn.example.com\/a\/1.png"`, `"https://cdn.example.com/a/1.png"`), kind: KindEncoding, where: "$.data[0].profile.avatar", level: LevelValues, keyed: keyedAt("", "$.data[id=1].profile.avatar")},
	{class: "escaping: unicode unescaped", doc: "users", mutate: replace(`"Ni\u0161"`, `"Niš"`), kind: KindEncoding, where: "$.data[2].profile.city", level: LevelValues, keyed: keyedAt("", "$.data[id=3].profile.city")},
	{class: "escaping: surrogate pair as UTF-8", doc: "users", mutate: replace(`"Hi \ud83d\udc4b"`, `"Hi 👋"`), kind: KindEncoding, where: "$.data[1].profile.bio", level: LevelValues, keyed: keyedAt("", "$.data[id=2].profile.bio")},
	{class: "layout: pretty → compact", doc: "order", mutate: func(t *testing.T, r resp) resp {
		r.body = strings.Join(strings.Fields(r.body), "")
		return r
	}, kind: KindEncoding, where: "$", level: LevelValues},
	{class: "escaping: key escaped", doc: "tags", mutate: replace(`"slug":"news"`, `"sl\u0075g":"news"`), kind: KindEncoding, where: "$", level: LevelValues},
	{class: "layout: newline at the end", doc: "tags", mutate: replace(`"count":3}]`, "\"count\":3}]\n"), kind: KindEncoding, where: "$", level: LevelValues},
	{class: "byte order mark", doc: "tags", mutate: replace(`[{"id":1`, "\ufeff"+`[{"id":1`), kind: KindEncoding, where: "$", level: LevelValues},
	// The response around the body.
	{class: "status", doc: "users", mutate: func(_ *testing.T, r resp) resp { r.status = 201; return r }, kind: KindStatus, where: "", level: LevelStatus},
	{class: "header value", doc: "users", mutate: setHeader("cache-control", "public, max-age=60"), kind: KindHeaderValue, where: "header:cache-control", level: LevelValues},
	{class: "header removed", doc: "users", mutate: setHeader("cache-control", ""), kind: KindHeaderMissing, where: "header:cache-control", level: LevelShape},
	{class: "header added", doc: "users", mutate: setHeader("x-debug", "1"), kind: KindHeaderExtra, where: "header:x-debug", level: LevelShape},
	{class: "allowed header off format", doc: "users", mutate: setHeader("date", "yesterday"), kind: KindHeaderValue, where: "header:date", level: LevelValues, ruled: KindFormat},
	{class: "content type", doc: "users", mutate: setHeader("content-type", "text/plain"), kind: KindHeaderValue, where: "header:content-type", level: LevelValues},
	{class: "JSON → HTML error page", doc: "users", mutate: func(_ *testing.T, r resp) resp {
		r.headers = pageHeaders
		r.body = "<!DOCTYPE html>\n<html>\n    <head>\n        <meta charset=\"UTF-8\" />\n        <meta http-equiv=\"refresh\" content=\"0;url='http://localhost/login'\" />\n    </head>\n</html>"
		return r
	}, kind: KindBody, where: "", level: LevelShape},
	{class: "JSON → empty body", doc: "tags", mutate: func(_ *testing.T, r resp) resp { r.body = ""; return r }, kind: KindBody, where: "", level: LevelShape},
	{class: "JSON truncated", doc: "tags", mutate: replace(`"count":3}]`, `"count":3}`), kind: KindBody, where: "", level: LevelShape},
	// HTML.
	{class: "html: element removed", doc: "page", mutate: replace(`<li data-id="2">Marko</li>`, ``), kind: KindElementMissing, where: "html>body>div#app>ul>li[1]", level: LevelShape},
	{class: "html: element added", doc: "page", mutate: replace(`<p>Total`, `<div class="alert">Error</div><p>Total`), kind: KindElementExtra, where: "html>body>div#app>div", level: LevelShape},
	{class: "html: attribute value", doc: "page", mutate: replace(`class="title"`, `class="title big"`), kind: KindAttributeValue, where: "html>body>div#app>h1@class", level: LevelValues},
	{class: "html: attribute removed", doc: "page", mutate: replace(` class="container"`, ``), kind: KindAttributeMissing, where: "html>body>div#app@class", level: LevelShape},
	{class: "html: text", doc: "page", mutate: replace(`<b>9</b>`, `<b>10</b>`), kind: KindText, where: "html>body>div#app>p>b", level: LevelValues},
	{class: "html: text next to an asset hash", doc: "page", mutate: replace(`app-4f3a2b1c.js`, `main-4f3a2b1c.js`), kind: KindAttributeValue, where: "html>head>script@src", level: LevelValues},
	{class: "html: embedded JSON value", doc: "page", mutate: replace(`"user":"ana"`, `"user":"marko"`), kind: KindValue, where: "html>body>div#app>script#__NEXT_DATA__$.props.user", level: LevelValues},
	{class: "html: doctype removed", doc: "page", mutate: replace("<!DOCTYPE html>\n", ""), kind: KindDoctype, where: DoctypePath, level: LevelShape},
	{class: "html: space between inline elements", doc: "page", mutate: replace(`</b> <i>`, `</b><i>`), kind: KindText, where: "html>body>div#app>p", level: LevelValues},
	{class: "html: space before an inline element", doc: "page", mutate: replace(`Total: <b>`, `Total:<b>`), kind: KindText, where: "html>body>div#app>p", level: LevelValues},
	{class: "html: space after an inline element", doc: "page", mutate: replace(`</i> today`, `</i>today`), kind: KindText, where: "html>body>div#app>p", level: LevelValues},
	{class: "html: doctype changed", doc: "page", mutate: replace("<!DOCTYPE html>", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN">`), kind: KindDoctype, where: DoctypePath, level: LevelValues},
	// XLSX.
	{class: "xlsx: cell value", doc: "xlsx", mutate: replace(`<v>42</v>`, `<v>43</v>`), kind: KindCell, where: "xlsx:S!B1", level: LevelValues},
	{class: "xlsx: cell type", doc: "xlsx", mutate: replace(`<c r="B1"><v>42</v></c>`, `<c r="B1" t="inlineStr"><is><t>42</t></is></c>`), kind: KindType, where: "xlsx:S!B1", level: LevelTypes},
	{class: "xlsx: allowed cell type", doc: "xlsx", mutate: replace(`<c r="A1" t="inlineStr"><is><t>Exported 30.10.2026 10:00</t></is></c>`, `<c r="A1"><v>1</v></c>`), kind: KindType, where: "xlsx:S!A1", level: LevelTypes},
	{class: "xlsx: cell format", doc: "xlsx", mutate: replace(`<c r="C1" s="1">`, `<c r="C1" s="2">`), kind: KindCellFormat, where: "xlsx:S!C1", level: LevelValues},
	{class: "xlsx: formula", doc: "xlsx", mutate: replace(`<f>B1*2</f>`, `<f>B1+B1</f>`), kind: KindCellFormula, where: "xlsx:S!B2", level: LevelValues},
	{class: "xlsx: formula of an empty result", doc: "xlsx", mutate: replace(`SUM(B1:B2)`, `SUM(B1:B9)`), kind: KindCellFormula, where: "xlsx:S!C2", level: LevelValues},
	{class: "xlsx: formula without a value", doc: "xlsx", mutate: replace(`TODAY()`, `NOW()`), kind: KindCellFormula, where: "xlsx:S!D2", level: LevelValues},
	{class: "xlsx: style of an empty cell", doc: "xlsx", mutate: replace(`<c r="D1" s="1"/>`, `<c r="D1" s="2"/>`), kind: KindCellFormat, where: "xlsx:S!D1", level: LevelValues},
	{class: "xlsx: rich text, same text", doc: "xlsx", mutate: replace(`<is><t>Total</t></is>`, `<is><r><rPr><b/><color rgb="FFFF0000"/></rPr><t>Total</t></r></is>`), kind: KindCellFormat, where: "xlsx:S!A3", level: LevelValues},
	{class: "xlsx: cell past a column rule (EA)", doc: "xlsx", mutate: replace(`<v>12345678</v>`, `<v>87654321</v>`), kind: KindCell, where: "xlsx:S!EA1", level: LevelValues},
	{class: "xlsx: cell past a row rule (15)", doc: "xlsx", mutate: replace(`<c r="A15"><v>1</v>`, `<c r="A15"><v>2</v>`), kind: KindCell, where: "xlsx:S!A15", level: LevelValues},
	{class: "xlsx: cell removed", doc: "xlsx", mutate: replace(`<c r="A2" t="inlineStr"><is><t>Ana</t></is></c>`, ``), kind: KindCell, where: "xlsx:S!A2", level: LevelShape},
}

// docs are the corpus responses by name.
func fpDocs(t *testing.T) map[string]func(cells string) resp {
	return map[string]func(string) resp{
		"users": func(string) resp { return resp{200, jsonHeaders, usersPage} },
		"order": func(string) resp { return resp{200, jsonHeaders, orderDoc} },
		"tags":  func(string) resp { return resp{200, jsonHeaders, tagList} },
		"page":  func(string) resp { return resp{200, pageHeaders, usersHTML} },
		"xlsx": func(cells string) resp {
			s := workbook(t, cells, xlsxStyles, "2026-10-30")
			return resp{200, s.Record.Headers, string(s.Body)}
		},
	}
}

func (r resp) side() Side { return side(r.status, r.headers, r.body) }

func TestFalsePassSuite(t *testing.T) {
	modes := []struct {
		name  string
		rules string
	}{{"none", "version: 1\n"}, {"typical", typicalRules}, {"keyed", keyedRules}}
	lists := make([]*allowlist.List, len(modes))
	for i, m := range modes {
		l, err := allowlist.Parse([]byte(m.rules))
		if err != nil {
			t.Fatal(err)
		}
		lists[i] = l
	}
	docs := fpDocs(t)
	levels := []Level{LevelStatus, LevelShape, LevelTypes, LevelValues}

	// Identical responses: nothing at any level, with any rules.
	for name, doc := range docs {
		r := doc(xlsxCells)
		for i, l := range lists {
			for _, level := range levels {
				if ds := One("req", r.side(), r.side(), l, level); len(ds) != 0 {
					t.Errorf("%s identical (%s, %s): %v", name, modes[i].name, level, ds)
				}
			}
		}
	}

	// matrix[class][mode] is one mark per level: + reported as expected,
	// . not required at this level, X missed.
	var rows []string
	failed := false
	for _, c := range fpCases {
		doc := docs[c.doc]
		var base, cur resp
		if c.doc == "xlsx" {
			// The mutation applies to the cells; the workbook is built around them.
			cells := c.mutate(t, resp{body: xlsxCells}).body
			base, cur = doc(xlsxCells), doc(cells)
		} else {
			base = doc("")
			cur = c.mutate(t, base)
		}
		row := fmt.Sprintf("%-40s", c.class)
		for i, l := range lists {
			kind, where := c.kind, c.where
			if modes[i].name != "none" && c.ruled != "" {
				kind = c.ruled
			}
			if modes[i].name == "keyed" && c.keyed != nil {
				if c.keyed[0] != "" {
					kind = c.keyed[0]
				}
				where = c.keyed[1]
			}
			marks := ""
			for _, level := range levels {
				ds := One("req", base.side(), cur.side(), l, level)
				switch {
				case level < c.level:
					marks += "."
				case has(ds, kind, where):
					marks += "+"
				default:
					marks += "X"
					failed = true
					t.Errorf("%s (%s, %s): no %s at %q; got %v", c.class, modes[i].name, level, kind, where, ds)
				}
			}
			row += " " + modes[i].name + " " + marks
		}
		rows = append(rows, row)
	}
	if failed {
		sort.Strings(rows)
		t.Logf("detection matrix (levels status, shape, types, values; + reported, . below its level, X missed):\n%s", strings.Join(rows, "\n"))
	}
}

// TestFalsePassSuiteAllowed: what the typical rules allow gives no drift
// with them, and is reported without them.
func TestFalsePassSuiteAllowed(t *testing.T) {
	typical, err := allowlist.Parse([]byte(typicalRules))
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := allowlist.Parse([]byte(keyedRules))
	if err != nil {
		t.Fatal(err)
	}
	docs := fpDocs(t)
	cases := []struct {
		name, doc string
		mutate    func(*testing.T, resp) resp
		keyedOnly bool
	}{
		{"time in its format", "users", replace(`"2026-10-30T08:15:00.000000Z"`, `"2026-10-31T09:00:00.123456Z"`), false},
		{"date header", "users", setHeader("date", "Sat, 31 Oct 2026 11:12:13 GMT"), false},
		{"role added", "users", replace(`"roles":["viewer"]`, `"roles":["viewer","editor"]`), false},
		{"export time", "xlsx", replace(`Exported 30.10.2026 10:00`, `Exported 31.10.2026 11:30`), false},
		{"generated code", "xlsx", replace(`0badc0de`, `deadbeef`), false},
		{"counter", "xlsx", replace(`<c r="A5"><v>1</v>`, `<c r="A5"><v>7</v>`), false},
		{"asset hash", "page", replace(`app-4f3a2b1c.js`, `app-0badc0de.js`), false},
		{"redirect page", "redirect", replace(`/login`, `/login?next=%2F`), false},
		{"user inserted at the front, by key", "users", replace(`"data":[`, `"data":[{"id":0,"name":"x"},`), true},
	}
	redirect := resp{302, pageHeaders, "<!DOCTYPE html>\n<html>\n    <head>\n        <meta charset=\"UTF-8\" />\n        <meta http-equiv=\"refresh\" content=\"0;url='http://localhost/login'\" />\n    </head>\n</html>"}
	for _, c := range cases {
		var base, cur resp
		switch c.doc {
		case "xlsx":
			base, cur = docs["xlsx"](xlsxCells), docs["xlsx"](c.mutate(t, resp{body: xlsxCells}).body)
		case "redirect":
			base, cur = redirect, c.mutate(t, redirect)
		default:
			base = docs[c.doc]("")
			cur = c.mutate(t, base)
		}
		allow := typical
		if c.keyedOnly {
			allow = keyed
		}
		ds := One("req", base.side(), cur.side(), allow, LevelValues)
		if c.keyedOnly {
			// A new item is a drift; the items after it are not.
			if len(ds) != 1 || !has(ds, KindKeyExtra, "$.data[id=0]") {
				t.Errorf("%s: %v", c.name, ds)
			}
		} else if len(ds) != 0 {
			t.Errorf("%s: allowed, yet %v", c.name, ds)
		}
		if ds := One("req", base.side(), cur.side(), allowlist.Empty(), LevelValues); len(ds) == 0 {
			t.Errorf("%s: not reported without rules", c.name)
		}
	}
}
