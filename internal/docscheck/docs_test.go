package docscheck

import (
	"bufio"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// strict turns the style warnings (sentence length, line budgets) into
// failures.
var strict = os.Getenv("DRIFTGATE_DOCS_STRICT") == "1"

// warn reports a style finding: a failure in strict mode, a log line
// otherwise (go test -v shows it).
func warn(t *testing.T, format string, args ...any) {
	t.Helper()
	if strict {
		t.Errorf(format, args...)
	} else {
		t.Logf("warning: "+format, args...)
	}
}

func pages(t *testing.T) (*Pages, []string) {
	t.Helper()
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Markdown files found")
	}
	return NewPages(root), files
}

func docs(t *testing.T) (*Pages, []*Doc) {
	t.Helper()
	p, files := pages(t)
	var out []*Doc
	for _, f := range files {
		d, err := p.Get(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return p, out
}

// TestLinks: every relative link and image, and every link to a file of
// this repository on GitHub, names a file that exists and an anchor its
// page has.
func TestLinks(t *testing.T) {
	p, all := docs(t)
	for _, d := range all {
		for _, l := range d.Links {
			if err := p.Resolve(d.Path, l.Target); err != nil {
				t.Errorf("%s:%d: %v", d.Path, l.Line, err)
			}
		}
	}
}

// TestPublicAnchors: the anchors released binaries, code and other sites
// link to keep resolving (testdata/public-anchors.txt).
func TestPublicAnchors(t *testing.T) {
	p, _ := pages(t)
	f, err := os.Open(filepath.Join("testdata", "public-anchors.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	n := 0
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		if err := p.Resolve("", "/"+line); err != nil {
			t.Errorf("public anchor %s: %v", line, err)
		}
	}
	if n == 0 {
		t.Error("testdata/public-anchors.txt lists no anchor")
	}
}

// TestNoDuplicateHeadings: no heading repeats within a page, so no anchor
// needs a -1 that moves when a heading above it changes.
func TestNoDuplicateHeadings(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		first := map[string]int{}
		for _, h := range d.Headings {
			s := Slug(h.Text)
			if line, ok := first[s]; ok {
				t.Errorf("%s:%d: heading %q repeats the one on line %d (anchor #%s)", d.Path, h.Line, h.Text, line, s)
				continue
			}
			first[s] = h.Line
		}
	}
}

var (
	urlHost  = regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://(?:[^@/\s` + "`" + `]*@)?([^/\s:?#)\]>'"` + "`" + `;,]+)`)
	mailHost = regexp.MustCompile(`[A-Za-z0-9._%+-]+@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})\b`)
	bareHost = regexp.MustCompile(`\b((?:[a-z0-9-]+\.)+(?:com|org|net|io|dev|app|co|ai|cloud|internal))(?:$|[^.\w-])`)
	yamlProj = regexp.MustCompile(`^\s*project:\s*(\S+)`)
	flagRef  = regexp.MustCompile(`(?:--ref|--drop)[ =]+["']?([^\s"'` + "`" + `)\]]+)`)
	yamlRef  = regexp.MustCompile(`^[\s-]*ref:\s*["']?([^\s"']+)`)
	goodRef  = regexp.MustCompile(`^(SHOP|API|APP|DEMO)-\d+$`)
)

// vendors are the sites the pages may name besides the reserved example
// names.
var vendors = []string{"github.com", "pkg.go.dev", "token.actions.githubusercontent.com",
	"mcr.microsoft.com", "modernc.org", "coveralls.io", "img.shields.io", "honnef.co"}

// docNets are the address ranges reserved for examples and private use.
var docNets = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32")}

// exampleHost reports whether a host name may appear in the public pages:
// a name reserved for examples or tests (RFC 2606, RFC 6761), a loopback,
// private or documentation address, or a vendor's site.
func exampleHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if a, err := netip.ParseAddr(host); err == nil {
		for _, n := range docNets {
			if n.Contains(a) {
				return true
			}
		}
		return a.IsLoopback() || a.IsPrivate() || a.IsUnspecified()
	}
	if !strings.Contains(host, ".") || strings.ContainsAny(host, "…<>{}$%*") {
		return true // a placeholder or a single name such as db
	}
	for _, name := range append([]string{"example", "test", "invalid", "localhost", "example.com", "example.net", "example.org"}, vendors...) {
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// TestExampleNames: host names, e-mail domains, project names and ticket
// refs in the pages are the example names of the style guide, never a
// real customer's or an internal one.
func TestExampleNames(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		yamlLines := map[int]bool{}
		for _, b := range d.Blocks {
			if b.Lang == "yaml" || b.Lang == "yml" {
				for i := range strings.Count(b.Body, "\n") + 1 {
					yamlLines[b.Line+1+i] = true
				}
			}
		}
		for i, line := range d.Lines {
			n := i + 1
			seen := map[string]bool{}
			for _, re := range []*regexp.Regexp{urlHost, mailHost, bareHost} {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					if h := m[1]; !seen[h] && !exampleHost(h) {
						seen[h] = true
						t.Errorf("%s:%d: host %q is not an example name (.example, .test, .invalid, localhost) or a listed vendor", d.Path, n, h)
					}
				}
			}
			if m := yamlProj.FindStringSubmatch(line); m != nil && yamlLines[n] && m[1] != "shop" && !strings.HasPrefix(m[1], "<") {
				t.Errorf("%s:%d: project %q: examples use project: shop", d.Path, n, m[1])
			}
			refs := flagRef.FindAllStringSubmatch(line, -1)
			if m := yamlRef.FindStringSubmatch(line); m != nil && yamlLines[n] {
				refs = append(refs, m)
			}
			for _, m := range refs {
				if r := m[1]; !goodRef.MatchString(r) && !strings.HasPrefix(r, "<") {
					t.Errorf("%s:%d: ticket ref %q: examples use SHOP-<n>, API-<n>, APP-<n> or DEMO-<n>", d.Path, n, r)
				}
			}
		}
	}
}

func TestExampleHost(t *testing.T) {
	for host, want := range map[string]bool{
		"shop.example": true, "api.shop.example": true, "laravel.test": true, "localhost": true,
		"127.0.0.1": true, "203.0.113.10": true, "github.com": true, "db": true, "example.com": true,
		"shop.com": false, "db.internal": false, "notexample.com": false, "8.8.8.8": false, "examplecom.test.org": false,
	} {
		if got := exampleHost(host); got != want {
			t.Errorf("exampleHost(%q) = %v", host, got)
		}
	}
}

// releaseVersion finds the DriftGate release a page names: a tag of the
// action or a download, a version input, a version printed.
var releaseVersion = regexp.MustCompile(`(?:driftgate@|driftgate |tags/|version: |release download |Version=)(v\d+\.\d+\.\d+)\b`)

// TestVersionStrings: every page names the same DriftGate release, except
// the notes on upgrading from earlier ones.
func TestVersionStrings(t *testing.T) {
	_, all := docs(t)
	first, where := "", ""
	for _, d := range all {
		if d.Path == "docs/reference/upgrading-driftgate.md" {
			continue
		}
		for i, line := range d.Lines {
			for _, m := range releaseVersion.FindAllStringSubmatch(line, -1) {
				at := d.Path + ":" + strconv.Itoa(i+1)
				switch {
				case first == "":
					first, where = m[1], at
				case m[1] != first:
					t.Errorf("%s: %s, but %s names %s", at, m[1], where, first)
				}
			}
		}
	}
	if first == "" {
		t.Error("no page names a release")
	}
}

// sentenceLimit is the most words a sentence may have in a page, by its
// folder; 0 for pages without a limit.
func sentenceLimit(path string) int {
	switch {
	case strings.HasPrefix(path, "docs/start/"), strings.HasPrefix(path, "docs/concepts/"), strings.HasPrefix(path, "docs/guides/"):
		return 25
	case strings.HasPrefix(path, "docs/reference/"), path == "docs/security.md":
		return 35
	}
	return 0
}

var (
	listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+`)
	comment  = regexp.MustCompile(`<!--.*?-->`)
)

// TestSentenceLength (a warning unless DRIFTGATE_DOCS_STRICT=1): sentences
// of prose, outside code, tables and comments, within the limit of their
// page. A code span counts as one word.
func TestSentenceLength(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		limit := sentenceLimit(d.Path)
		if limit == 0 {
			continue
		}
		code := map[int]bool{}
		for _, b := range d.Blocks {
			for i := range strings.Count(b.Body, "\n") + 3 {
				code[b.Line+i] = true
			}
		}
		var para []string
		start := 0
		flush := func() {
			for _, s := range sentences(strings.Join(para, " ")) {
				if words := len(strings.Fields(s)); words > limit {
					warn(t, "%s:%d: a sentence of %d words in this paragraph (at most %d)", d.Path, start, words, limit)
				}
			}
			para = nil
		}
		for i, line := range d.Lines {
			trim := strings.TrimSpace(line)
			skip := code[i+1] || trim == "" || strings.HasPrefix(trim, "|") || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, "<!--")
			if skip || listItem.MatchString(line) {
				flush()
			}
			if skip {
				continue
			}
			if para == nil {
				start = i + 1
			}
			para = append(para, listItem.ReplaceAllString(comment.ReplaceAllString(trim, ""), ""))
		}
		flush()
	}
}

// sentences splits a paragraph at ., ! and ? followed by a space; a code
// span becomes one word.
func sentences(para string) []string {
	para = codeSpan.ReplaceAllString(para, "code")
	var out []string
	from := 0
	for i := 0; i < len(para); i++ {
		if strings.ContainsRune(".!?", rune(para[i])) && (i+1 == len(para) || para[i+1] == ' ') {
			out = append(out, para[from:i+1])
			from = i + 1
		}
	}
	if strings.TrimSpace(para[from:]) != "" {
		out = append(out, para[from:])
	}
	return out
}

// TestBudgets (a warning unless DRIFTGATE_DOCS_STRICT=1): every page within
// its line budget (testdata/budgets.txt: path and the most lines).
func TestBudgets(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "budgets.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		budget, err := strconv.Atoi(f[len(f)-1])
		if len(f) != 2 || err != nil {
			t.Fatalf("budgets.txt: %q is not <path> <lines>", line)
		}
		page, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f[0])))
		if os.IsNotExist(err) {
			continue // a page still to be written
		}
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(page), "\n"); n > budget {
			warn(t, "%s: %d lines (budget %d)", f[0], n, budget)
		}
	}
}
