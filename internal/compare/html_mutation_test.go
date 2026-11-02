package compare

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

// The HTML mutation test plants one change of every kind at every element it
// fits in every page of testdata/html (real Next.js and Nuxt pages and a
// handmade one) and requires DriftGate to report it at that element's path.
// The expected path is computed here from the parsed tree, independently of
// the comparison. DRIFTGATE_HTML_CORPUS adds a directory of real pages.

func htmlCorpus(t *testing.T) map[string][]byte {
	docs := map[string][]byte{}
	dirs := []string{filepath.Join("testdata", "html")}
	if extra := os.Getenv("DRIFTGATE_HTML_CORPUS"); extra != "" {
		dirs = append(dirs, extra)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("corpus %s: %v", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
				raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				docs[filepath.Join(dir, e.Name())] = raw
			}
		}
	}
	if len(docs) == 0 {
		t.Fatal("empty corpus")
	}
	return docs
}

func isFlightScript(n *html.Node) bool {
	return n.Type == html.ElementNode && n.Data == "script" && n.FirstChild != nil && strings.Contains(n.FirstChild.Data, "self.__next_f")
}

// joinedFlight reports a payload script that the comparison makes one
// element with the one before it: the previous element is a payload script
// with the same attributes, and only whitespace and comments lie between.
func joinedFlight(n *html.Node) bool {
	if !isFlightScript(n) {
		return false
	}
	for p := n.PrevSibling; p != nil; p = p.PrevSibling {
		switch {
		case p.Type == html.TextNode && strings.TrimSpace(p.Data) != "":
			return false
		case p.Type == html.ElementNode:
			return isFlightScript(p) && fmt.Sprint(attrMap(p)) == fmt.Sprint(attrMap(n))
		}
	}
	return false
}

func attrMap(n *html.Node) map[string]string {
	m := map[string]string{}
	for _, a := range n.Attr {
		m[a.Namespace+":"+a.Key] = a.Val
	}
	return m
}

// leadsFlight reports a payload script that later ones are joined to: a
// change of its attributes splits them off, a different drift.
func leadsFlight(n *html.Node) bool {
	for c := n.NextSibling; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return joinedFlight(c)
		}
	}
	return false
}

// elementSiblings are the element children of a parent as the comparison
// sees them: adjacent payload scripts of the RSC payload are one element.
func elementSiblings(parent *html.Node) []*html.Node {
	var out []*html.Node
	for c := parent.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && !joinedFlight(c) {
			out = append(out, c)
		}
	}
	return out
}

// quoteStep quotes a tag or an id the way paths do when it holds path syntax.
func quoteStep(s string) string {
	if strings.ContainsAny(s, "@>#[]$*?\"\\ \t\n") {
		return jsonx.Quote(s)
	}
	return s
}

func nodeKey(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "id" && a.Namespace == "" && a.Val != "" {
			return quoteStep(n.Data) + "#" + quoteStep(a.Val)
		}
	}
	return quoteStep(n.Data)
}

// expectedPath is the documented path of an element: keys from the root,
// with an index among siblings of the same key when there are several.
func expectedPath(n *html.Node) string {
	if n.Parent == nil || n.Parent.Type == html.DocumentNode {
		return n.Data
	}
	key, idx, count := nodeKey(n), 0, 0
	for _, s := range elementSiblings(n.Parent) {
		if nodeKey(s) == key {
			if s == n {
				idx = count
			}
			count++
		}
	}
	step := key
	if count > 1 {
		step = fmt.Sprintf("%s[%d]", key, idx)
	}
	return expectedPath(n.Parent) + ">" + step
}

func render(t *testing.T, doc *html.Node) string {
	var b bytes.Buffer
	if err := html.Render(&b, doc); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// elements lists the elements of a document in document order.
func elements(doc *html.Node) []*html.Node {
	var out []*html.Node
	var visit func(n *html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.ElementNode && !joinedFlight(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)
	return out
}

func inBody(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "body" {
			return true
		}
	}
	return false
}

// Elements whose content the parser rebuilds or moves (tables, selects, the
// document skeleton), raw text elements and foreign (SVG, MathML) content are
// left out where a mutation would not survive a round trip.
var restructured = map[string]bool{"html": true, "head": true, "body": true, "table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true,
	"select": true, "colgroup": true, "optgroup": true, "option": true, "script": true, "style": true, "textarea": true, "title": true, "pre": true, "p": true, "ul": true, "ol": true}

func ownText(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) != "" {
			return c
		}
	}
	return nil
}

func embeddedJSONNode(n *html.Node) bool {
	if n.Data != "script" || n.FirstChild == nil {
		return false
	}
	for _, a := range n.Attr {
		if a.Key == "type" && strings.HasSuffix(a.Val, "json") {
			return true
		}
	}
	return false
}

func hasNuxtData(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "data-nuxt-data" || (a.Key == "id" && a.Val == "__NUXT_DATA__") {
			return true
		}
	}
	return false
}

// unique reports whether no sibling renders exactly like n (a duplicate can
// be paired either way).
func unique(t *testing.T, n *html.Node) bool {
	var b bytes.Buffer
	html.Render(&b, n)
	for _, s := range elementSiblings(n.Parent) {
		if s == n {
			continue
		}
		var o bytes.Buffer
		html.Render(&o, s)
		if o.String() == b.String() {
			return false
		}
	}
	return true
}

type htmlMutation struct {
	name  string
	apply func(t *testing.T, n *html.Node) (kind, where string, ok bool)
}

var htmlMutations = []htmlMutation{
	{"text", func(t *testing.T, n *html.Node) (string, string, bool) {
		tn := ownText(n)
		if tn == nil || embeddedJSONNode(n) || isFlightScript(n) {
			return "", "", false
		}
		tn.Data = strings.TrimRight(tn.Data, " \t\n") + "!" + tn.Data[len(strings.TrimRight(tn.Data, " \t\n")):]
		return KindText, expectedPath(n), true
	}},
	{"attribute-value", func(t *testing.T, n *html.Node) (string, string, bool) {
		if leadsFlight(n) {
			return "", "", false
		}
		for i, a := range n.Attr {
			if a.Key != "id" && a.Key != "type" && a.Namespace == "" {
				n.Attr[i].Val += "!"
				return KindAttributeValue, expectedPath(n) + "@" + a.Key, true
			}
		}
		return "", "", false
	}},
	{"attribute-missing", func(t *testing.T, n *html.Node) (string, string, bool) {
		if leadsFlight(n) {
			return "", "", false
		}
		for i, a := range n.Attr {
			if a.Key != "id" && a.Key != "type" && a.Namespace == "" {
				n.Attr = append(n.Attr[:i:i], n.Attr[i+1:]...)
				return KindAttributeMissing, expectedPath(n) + "@" + a.Key, true
			}
		}
		return "", "", false
	}},
	{"attribute-extra", func(t *testing.T, n *html.Node) (string, string, bool) {
		if n.Namespace != "" || leadsFlight(n) {
			return "", "", false
		}
		n.Attr = append(n.Attr, html.Attribute{Key: "data-driftgate", Val: "x"})
		return KindAttributeExtra, expectedPath(n) + "@data-driftgate", true
	}},
	{"element-missing", func(t *testing.T, n *html.Node) (string, string, bool) {
		if restructured[n.Data] || restructured[n.Parent.Data] || n.Namespace != "" || !unique(t, n) {
			return "", "", false
		}
		where := expectedPath(n)
		n.Parent.RemoveChild(n)
		return KindElementMissing, where, true
	}},
	{"element-extra", func(t *testing.T, n *html.Node) (string, string, bool) {
		if restructured[n.Data] || n.Namespace != "" || !inBody(n) || voidElement[n.Data] {
			return "", "", false
		}
		count := 0
		for _, s := range elementSiblings(n) {
			if nodeKey(s) == "span" {
				count++
			}
		}
		where := expectedPath(n) + ">span"
		if count > 0 {
			where = fmt.Sprintf("%s[%d]", where, count)
		}
		n.AppendChild(&html.Node{Type: html.ElementNode, Data: "span", Attr: []html.Attribute{{Key: "data-driftgate", Val: ""}}})
		return KindElementExtra, where, true
	}},
	{"element-order", func(t *testing.T, n *html.Node) (string, string, bool) {
		next := n.NextSibling
		for next != nil && (next.Type != html.ElementNode || joinedFlight(next)) {
			next = next.NextSibling
		}
		if next == nil || nodeKey(next) == nodeKey(n) || restructured[n.Parent.Data] || n.Namespace != "" || !unique(t, n) || !unique(t, next) {
			return "", "", false
		}
		parent := n.Parent
		parent.RemoveChild(next)
		parent.InsertBefore(next, n)
		return KindElementOrder, expectedPath(parent), true
	}},
	{"json-value", func(t *testing.T, n *html.Node) (string, string, bool) {
		if !embeddedJSONNode(n) || hasNuxtData(n) {
			return "", "", false
		}
		root, err := jsonx.Parse([]byte(n.FirstChild.Data))
		if err != nil {
			return "", "", false
		}
		for _, nd := range nodes(root) {
			if nd.value.Kind == jsonx.String {
				where := expectedPath(n) + nd.path
				v := locate(root, nd.steps)
				v.Text += "!"
				v.Raw = jsonx.Quote(v.Text)
				n.FirstChild.Data = string(jsonx.Marshal(root, false))
				return KindValue, where, true
			}
		}
		return "", "", false
	}},
}

var voidElement = map[string]bool{"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true, "input": true,
	"link": true, "meta": true, "source": true, "track": true, "wbr": true, "noscript": true, "template": true, "a": true, "button": true, "label": true}

func TestHTMLMutationsAreCaught(t *testing.T) {
	allow := allowlist.Empty()
	planted, missed := map[string]int{}, map[string]int{}
	for name, raw := range htmlCorpus(t) {
		doc, err := html.Parse(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		base := render(t, doc)
		if ds := runHTML(t, allow, string(raw), base); len(ds) != 0 {
			t.Fatalf("%s: rendering the parsed page reported: %+v", name, ds)
		}
		count := len(elements(doc))
		for _, m := range htmlMutations {
			for i := 0; i < count; i++ {
				mutated, _ := html.Parse(strings.NewReader(base))
				list := elements(mutated)
				if i >= len(list) {
					break
				}
				kind, where, ok := m.apply(t, list[i])
				if !ok {
					continue
				}
				planted[m.name]++
				ds := runHTML(t, allow, base, render(t, mutated))
				if !has(ds, kind, where) {
					missed[m.name]++
					if missed[m.name] <= 3 {
						t.Errorf("%s: %s not caught as %s at %s; got %+v", name, m.name, kind, where, ds)
					}
				}
			}
		}
	}
	for _, m := range htmlMutations {
		t.Logf("%-18s planted %5d  missed %d", m.name, planted[m.name], missed[m.name])
		if planted[m.name] == 0 {
			t.Errorf("%s: never planted", m.name)
		}
	}
}
