package compare

import (
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
)

// redirectPage is Symfony's "Redirecting to" page; quote is "'" from
// Symfony 7 on, which quotes the meta refresh URL.
func redirectPage(origin, quote string) string {
	return "<!DOCTYPE html>\n<html>\n    <head>\n        <meta charset=\"UTF-8\" />\n" +
		"        <meta http-equiv=\"refresh\" content=\"0;url=" + quote + origin + quote + "\" />\n\n" +
		"        <title>Redirecting to " + origin + "</title>\n    </head>\n    <body>\n" +
		"        Redirecting to <a href=\"" + origin + "\">" + origin + "</a>.\n    </body>\n</html>"
}

// redirectFormat is the format of an allowlist rule for that page.
const redirectFormat = `<!DOCTYPE html>\n<html>\n    <head>\n        <meta charset="UTF-8" />\n` +
	`        <meta http-equiv="refresh" content="0;url='?http://localhost:\d+'?" />\n\n` +
	`        <title>Redirecting to http://localhost:\d+</title>\n    </head>\n    <body>\n` +
	`        Redirecting to <a href="http://localhost:\d+">http://localhost:\d+</a>.\n    </body>\n</html>`

func rawAllow(t *testing.T, request, format string) *allowlist.List {
	t.Helper()
	return htmlAllow(t, "  - request: "+request+"\n    path: $raw\n    format: '"+strings.ReplaceAll(format, "'", "''")+"'\n    reason: Symfony 7 quotes the URL\n")
}

func redirect(origin, quote string) Side {
	return side(302, [][2]string{{"content-type", "text/html; charset=utf-8"}, {"location", origin}}, redirectPage(origin, quote))
}

// TestRawRule: a whole-body rule lets a page that is not JSON change as its
// format allows; a page it does not describe is still compared element by
// element.
func TestRawRule(t *testing.T) {
	const o = "http://localhost:8080"
	base, cur := redirect(o, ""), redirect(o, "'")
	if ds := both(t, nil, base, cur); !has(ds, KindAttributeValue, "html>head>meta[1]@content") {
		t.Fatalf("without the rule: %+v", ds)
	}
	allow := rawAllow(t, "req", redirectFormat)
	if ds := both(t, allow, base, cur); len(ds) != 0 {
		t.Fatalf("quoted URL: %+v", ds)
	}
	if ds := both(t, rawAllow(t, "r*", redirectFormat), base, cur); len(ds) != 0 {
		t.Fatalf("request glob: %+v", ds)
	}
	// Another page: the format does not match, and the drifts are listed.
	other := redirect(o, "'")
	other.Body = []byte(strings.Replace(string(other.Body), "Redirecting to <a", "Moved to <a", 1))
	if ds := both(t, allow, base, other); !has(ds, KindAttributeValue, "html>head>meta[1]@content") || !has(ds, KindText, "html>body") {
		t.Fatalf("another page: %+v", ds)
	}
	// A rule for another request does not apply.
	if ds := both(t, rawAllow(t, "other-*", redirectFormat), base, cur); !has(ds, KindAttributeValue, "html>head>meta[1]@content") {
		t.Fatalf("rule of another request: %+v", ds)
	}
	// Status and headers are still compared.
	moved := redirect(o, "'")
	moved.Record.Status = 301
	moved.Record.Headers = [][2]string{{"content-type", "text/html; charset=utf-8"}, {"location", o + "/login"}}
	ds := both(t, allow, base, moved)
	if len(ds) != 2 || !has(ds, KindStatus, "") || !has(ds, KindHeaderValue, "header:location") {
		t.Fatalf("status and location: %+v", ds)
	}
}

// TestRawRuleNotForJSON: two JSON bodies are compared value by value, also
// with a whole-body rule that matches anything, and a JSON body that is
// not JSON now (an error page), or the other way round, is a body drift: the
// rule covers only bodies that are not JSON on both sides. A rule without
// a request is no exception.
func TestRawRuleNotForJSON(t *testing.T) {
	for _, allow := range []*allowlist.List{rawAllow(t, "req", `(?s).*`), htmlAllow(t, "  - path: $raw\n    format: '(?s).*'\n    reason: any page\n")} {
		ds := both(t, allow, side(200, jsonType, `{"a":1}`), side(200, jsonType, `{"a":2}`))
		if len(ds) != 1 || !has(ds, KindValue, "$.a") {
			t.Fatalf("JSON: %+v", ds)
		}
		page := [][2]string{{"content-type", "text/html; charset=utf-8"}}
		for _, pair := range [][2]Side{
			{side(200, jsonType, `{"users":[1,2,3]}`), side(200, jsonType, `Server Error`)},
			{side(200, jsonType, `{"users":[1,2,3]}`), side(200, page, "<html><body>Error. Generated at 123</body></html>")},
			{side(200, page, "<html><body>Error</body></html>"), side(200, jsonType, `{"users":[]}`)},
		} {
			for _, level := range []Level{LevelShape, LevelValues} {
				if ds := run(t, allow, level, pair[0], pair[1]); !has(ds, KindBody, "") {
					t.Fatalf("%s: one side JSON: %+v", level, ds)
				}
			}
		}
		if ds := both(t, allow, side(200, jsonType, `Bad Gateway`), side(200, jsonType, `Server Error`)); len(ds) != 0 {
			t.Fatalf("neither side JSON: %+v", ds)
		}
	}
}

// TestRawRuleWithOrigins: with different origins the rule is matched against
// the current body as served (its own origin), not against the body with the
// neutral origin; the rest of the comparison still replaces the origins.
func TestRawRuleWithOrigins(t *testing.T) {
	base, cur := at(redirect(baseOrigin, ""), baseOrigin), at(redirect(curOrigin, "'"), curOrigin)
	if ds := both(t, rawAllow(t, "req", redirectFormat), base, cur); len(ds) != 0 {
		t.Fatalf("quoted URL at another origin: %+v", ds)
	}
	exact := strings.ReplaceAll(redirectFormat, `http://localhost:\d+`, curOrigin)
	if ds := both(t, rawAllow(t, "req", exact), base, cur); len(ds) != 0 {
		t.Fatalf("format with the current origin: %+v", ds)
	}
	neutral := strings.ReplaceAll(redirectFormat, `http://localhost:\d+`, "http://origin.invalid")
	ds := both(t, rawAllow(t, "req", neutral), base, cur)
	if len(ds) != 1 || !has(ds, KindAttributeValue, "html>head>meta[1]@content") {
		t.Fatalf("format with the neutral origin: %+v", ds)
	}
	if !strings.Contains(ds[0].Current, curOrigin) || !strings.Contains(ds[0].Baseline, baseOrigin) {
		t.Errorf("drift shows the neutral origin: %+v", ds[0])
	}
	// Without the rule only the quotes differ: the origins are replaced.
	if ds := both(t, nil, base, at(redirect(curOrigin, ""), curOrigin)); len(ds) != 0 {
		t.Fatalf("same page at another origin: %+v", ds)
	}
}
