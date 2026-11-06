package docscheck

import (
	"bufio"
	"fmt"
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
	urlHost  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(?:[^@/\s` + "`" + `]*@)?([^/\s:?#)\]>'"` + "`" + `;,]+)`)
	mailHost = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@((?:[a-z0-9-]+\.)+[a-z]{2,})\b`)
	bareHost = regexp.MustCompile(`(?i)(?:^|\*\.|[^.\w/@$-])((?:[a-z0-9-]+\.)+[a-z]{2,})(?:$|[^.\w-])`)
	bareIP   = regexp.MustCompile(`(?:^|[^.\d])(\d{1,3}(?:\.\d{1,3}){3})(?:$|[^.\d])`)
	yamlProj = regexp.MustCompile(`^\s*project:\s*["']?([^\s"']+)`)
	flagProj = regexp.MustCompile(`--project[ =]+["']?([^\s"'` + "`" + `)\]]+)`)
	flagRef  = regexp.MustCompile(`(?:--ref|--drop)[ =]+["']?([^\s"'` + "`" + `)\]]+)`)
	yamlRef  = regexp.MustCompile(`^[\s-]*ref:\s*["']?([^\s"']+)`)
	goodRef  = regexp.MustCompile(`^(SHOP|API|APP|DEMO)-\d+$`)
)

// vendors are the sites the pages may name besides the reserved example
// names, and product names written like one.
var vendors = []string{"github.com", "pkg.go.dev", "token.actions.githubusercontent.com",
	"mcr.microsoft.com", "modernc.org", "coveralls.io", "img.shields.io", "honnef.co", "asp.net", "ado.net"}

// notHosts are keys of the pages that look like a host with a country's
// domain; a GitHub expression (${{ job.services.db.id }}) is not read.
var (
	notHosts = map[string]bool{"user.id": true, "key.op": true}
	ghaExpr  = regexp.MustCompile(`\$\{\{[^}]*\}\}`)
)

// fileExts end names that are files, not hosts (catalog.json, main.go).
var fileExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`md go mod sum sh ps1 bat js mjs cjs ts tsx jsx vue py rb cs csproj php
		java kt db sqlite sqlite3 sql dump bak json yaml yml toml ini conf cfg xml txt csv tsv xlsx html htm css
		env lock log har pem crt key gif png svg zip gz tar exe dll`) {
		fileExts[e] = true
	}
}

// genericTLDs are the generic top-level domains a host in the pages could
// have; every two-letter one (a country's) counts too.
var genericTLDs = map[string]bool{}

func init() {
	for _, d := range strings.Fields(`com net org info biz io co ai app dev cloud tech online site shop store
		xyz eu asia mobi pro tv me us local lan internal corp home intranet`) {
		genericTLDs[d] = true
	}
}

// hostTLD reports whether a dotted name written without a scheme ends in a
// top-level domain and not in a file extension (catalog.json, main.go) or a
// key (tokens.command).
func hostTLD(name string) bool {
	tld := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	return !fileExts[tld] && (len(tld) == 2 || genericTLDs[tld])
}

// exampleAddrs are the addresses the pages may name: loopback, unspecified,
// the documentation ranges and the bases of the private ranges that the
// default allow_hosts lists.
var exampleAddrs = []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("10.0.0.0/32"), netip.MustParsePrefix("172.16.0.0/32"), netip.MustParsePrefix("192.168.0.0/32")}

// exampleHost reports whether a host name may appear in the public pages:
// a name reserved for examples or tests (RFC 2606, RFC 6761), a loopback or
// documentation address, a placeholder, or a vendor's site. A wildcard is
// judged by the name after *.
func exampleHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	host = strings.TrimPrefix(host, "*.")
	if a, err := netip.ParseAddr(host); err == nil {
		for _, n := range exampleAddrs {
			if n.Contains(a) {
				return true
			}
		}
		return a.IsLoopback() || a.IsUnspecified()
	}
	if !strings.Contains(host, ".") || strings.ContainsAny(host, "…<>{}$%") {
		return true // a placeholder or a single name such as db
	}
	for _, name := range append([]string{"example", "test", "invalid", "localhost", "example.com", "example.net", "example.org"}, vendors...) {
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// nameFindings lists what in one line of a page is not an example name:
// a host, an e-mail domain, a project (yaml: the line is in a yaml block)
// or a ticket ref.
func nameFindings(line string, yaml bool) []string {
	var out []string
	seen := map[string]bool{}
	hosts := ghaExpr.ReplaceAllString(line, "")
	for _, re := range []*regexp.Regexp{urlHost, mailHost, bareHost, bareIP} {
		for _, m := range re.FindAllStringSubmatch(hosts, -1) {
			h := m[1]
			if re == bareHost && (!hostTLD(h) || notHosts[h]) {
				continue
			}
			if !seen[h] && !exampleHost(h) {
				seen[h] = true
				out = append(out, fmt.Sprintf("host %q is not an example name (.example, .test, .invalid, localhost) or a listed vendor", h))
			}
		}
	}
	projects := flagProj.FindAllStringSubmatch(line, -1)
	if m := yamlProj.FindStringSubmatch(line); m != nil && yaml {
		projects = append(projects, m)
	}
	for _, m := range projects {
		if p := m[1]; p != "shop" && !strings.HasPrefix(p, "<") {
			out = append(out, fmt.Sprintf("project %q: examples use the project shop", p))
		}
	}
	refs := flagRef.FindAllStringSubmatch(line, -1)
	if m := yamlRef.FindStringSubmatch(line); m != nil && yaml {
		refs = append(refs, m)
	}
	for _, m := range refs {
		if r := m[1]; !goodRef.MatchString(r) && !strings.HasPrefix(r, "<") {
			out = append(out, fmt.Sprintf("ticket ref %q: examples use SHOP-<n>, API-<n>, APP-<n> or DEMO-<n>", r))
		}
	}
	return out
}

// TestExampleNames: host names, e-mail domains, project names and ticket
// refs in the pages are the example names of the style guide.
func TestExampleNames(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		yamlLines := map[int]bool{}
		for _, b := range d.Blocks {
			if b.Lang == "yaml" || b.Lang == "yml" {
				for n := b.Line + 1; n < b.End; n++ {
					yamlLines[n] = true
				}
			}
		}
		for i, line := range d.Lines {
			for _, f := range nameFindings(line, yamlLines[i+1]) {
				t.Errorf("%s:%d: %s", d.Path, i+1, f)
			}
		}
	}
}

func TestNameFindings(t *testing.T) {
	for line, want := range map[string]int{
		"base_url: https://api.shop.example/v1":          0,
		"see `driftgate.yaml`, catalog.json and main.go": 0,
		"cp .env.local .env; DB_HOST=127.0.0.1":          0,
		"allow_hosts: [\"*.shop.example\", localhost]":   0,
		"--project shop --ref SHOP-12":                   0,
		"`10.0.0.0/8`, `192.168.0.0/16`":                 0,
		"staging.acme.rs":                                1,
		"go to acme.de now":                              1,
		"api.acme.co.uk":                                 1,
		"Shop.ACME.com":                                  1,
		"DB_HOST=db.acme.local driftgate run":            1,
		"allow_hosts: [\"db.acme.rs\"]":                  1,
		"allow_hosts: [\"*.acme.rs\"]":                   1,
		"driftgate init --project acmecorp":              1,
		"https://ops@Corp.Example.org.uk/":               1,
		"connect to 10.1.2.3 or 192.168.1.5":             2,
		"mail admin@acme.de":                             1,
		"driftgate expect --ref JIRA-1":                  1,
	} {
		if got := nameFindings(line, true); len(got) != want {
			t.Errorf("%q: %d findings %v, want %d", line, len(got), got, want)
		}
	}
	if got := nameFindings("project: acme", true); len(got) != 1 {
		t.Errorf("project: acme in yaml: %v", got)
	}
	if got := nameFindings("the project: acme", false); len(got) != 0 {
		t.Errorf("prose: %v", got)
	}
}

func TestExampleHost(t *testing.T) {
	for host, want := range map[string]bool{
		"shop.example": true, "api.shop.example": true, "laravel.test": true, "localhost": true, "*.shop.example": true,
		"127.0.0.1": true, "203.0.113.10": true, "github.com": true, "db": true, "example.com": true, "10.0.0.0": true,
		"shop.com": false, "db.internal": false, "notexample.com": false, "8.8.8.8": false, "examplecom.test.org": false,
		"10.1.2.3": false, "192.168.1.5": false, "*.acme.rs": false, "SHOP.ACME.COM": false,
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
			for n := b.Line; n <= b.End; n++ {
				code[n] = true
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
