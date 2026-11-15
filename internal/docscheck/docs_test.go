package docscheck

import (
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

// TestNoDuplicateHeadings: no heading repeats within a page, so no anchor
// needs a -1 that moves when a heading above it changes. The changelog
// repeats its section headings (Added, Fixed, …) under every release; only
// its release headings must be unique there.
func TestNoDuplicateHeadings(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		first := map[string]int{}
		for _, h := range d.Headings {
			if d.Path == "CHANGELOG.md" && h.Level > 2 {
				continue
			}
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
	urlHost  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(?:[^@/\s` + "`" + `]*@)?(\[[0-9a-f:.]+\]|[^/\s:?#)\]>'"` + "`" + `;,\[]+)`)
	mailHost = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@((?:[a-z0-9-]+\.)+[a-z]{2,})\b`)
	bareHost = regexp.MustCompile(`(?i)(?:^|\*\.|[>}]\.|[^.\w/@$-])((?:[a-z0-9-]+\.)+[a-z]{2,})`)
	bareIP   = regexp.MustCompile(`(?:^|[^.\d])(\d{1,3}(?:\.\d{1,3}){3})`)
	yamlProj = regexp.MustCompile(`^\s*project:\s*["']?([^\s"']+)`)
	flagProj = regexp.MustCompile(`--project[ =]+["']?([^\s"'` + "`" + `)\]]+)`)
	flagRef  = regexp.MustCompile(`(?:--ref|--drop)[ =]+["']?([^\s"'` + "`" + `)\]]+)`)
	yamlRef  = regexp.MustCompile(`^[\s-]*ref:\s*["']?([^\s"']+)`)
	ghOwner  = regexp.MustCompile(`(?i)github\.com[/:]([a-z0-9_.-]+)`)
	goodRef  = regexp.MustCompile(`^(SHOP|API|APP|DEMO)-\d+$`)
)

// vendors are the sites the pages may name besides the reserved example
// names, and product names written like one.
var vendors = []string{"github.com", "pkg.go.dev", "token.actions.githubusercontent.com",
	"mcr.microsoft.com", "modernc.org", "coveralls.io", "img.shields.io", "honnef.co"}

// products are product names written like a host; only the name itself.
var products = map[string]bool{"asp.net": true, "ado.net": true}

// githubOwners are the accounts whose repositories the pages may link.
var githubOwners = map[string]bool{"orangehill": true, "microsoft": true, "slavenkobozic-ohd": true}

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
		xyz eu asia mobi pro tv me us local lan internal corp home intranet agency studio digital group
		solutions software systems network company live club page ltd gmbh global consulting media design
		team works world website space top one inc services partners center today email host hosting tools
		technology marketing edu gov`) {
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

// hostName is the part of a host that names it, in lower case: a wildcard
// or a placeholder label ({tenant}, <tenant>, ${TENANT}) says nothing.
func hostName(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	var labels []string
	for _, l := range strings.Split(host, ".") {
		if l != "*" && !strings.ContainsAny(l, "…<>{}$%") {
			labels = append(labels, l)
		}
	}
	return strings.Join(labels, ".")
}

// exampleHost reports whether a host name may appear in the public pages:
// a name reserved for examples or tests (RFC 2606, RFC 6761), a loopback or
// documentation address, a placeholder, or a vendor's site. A wildcard is
// judged by the name after *.
func exampleHost(host string) bool {
	host = hostName(host)
	if a, err := netip.ParseAddr(host); err == nil {
		for _, n := range exampleAddrs {
			if n.Contains(a) {
				return true
			}
		}
		return a.IsLoopback() || a.IsUnspecified()
	}
	if !strings.Contains(host, ".") || products[host] {
		return true // a single name such as db, or none
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
		for _, m := range re.FindAllStringSubmatchIndex(hosts, -1) {
			h := hosts[m[2]:m[3]]
			// A bare name or address ends where no name character follows;
			// the pattern cannot look ahead without taking that character.
			// A dot goes on with the name only when a name character follows
			// it; otherwise it ends a sentence.
			next := byte(' ')
			if m[3] < len(hosts) {
				next = hosts[m[3]]
			}
			goesOn := next == '.' && m[3]+1 < len(hosts) && isAlnum(hosts[m[3]+1])
			switch {
			case re == bareHost && (goesOn || next == '-' || next == '_' || isAlnum(next) || !hostTLD(h) || notHosts[h]),
				re == bareIP && (goesOn || next >= '0' && next <= '9'):
				continue
			}
			if name := hostName(h); !seen[name] && !exampleHost(h) {
				seen[name] = true
				out = append(out, fmt.Sprintf("host %q is not an example name (.example, .test, .invalid, localhost) or a listed vendor", h))
			}
		}
	}
	for _, m := range ghOwner.FindAllStringSubmatch(line, -1) {
		if o := strings.ToLower(m[1]); !githubOwners[o] {
			out = append(out, fmt.Sprintf("GitHub account %q: the pages link only orangehill and the listed vendors", m[1]))
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

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
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
		"go to acme.de.":                                 1,
		"api.acme.rs. Next":                              1,
		"at 10.1.2.3.":                                   1,
		"Connect to staging.acme.rs.":                    1,
		"version 1.2.3.4.5":                              0,
		"a.acme.rs b.acme.de":                            2,
		"[a.acme.rs,b.acme.de]":                          2,
		"10.1.2.3 192.168.1.5":                           2,
		"10.1.2.3,192.168.1.5":                           2,
		"http://[2606:4700::1]/":                         1,
		"http://[::1]:8080/":                             0,
		"see acme.edu or acme.gov":                       2,
		"REDIS_HOST=cache.acme.agency":                   1,
		`{"host": "db.acme.agency"}`:                     1,
		"tcp(db.acme.agency:3306)/":                      1,
		"https://{tenant}.acme.rs/":                      1,
		"https://{tenant}.shop.example/":                 0,
		"<tenant>.acme.rs":                               1,
		"${TENANT}.acme.rs":                              1,
		"https://github.com/acme-client/app":             1,
		"git@github.com:acme-client/app.git":             1,
		"https://github.com/orangehill/driftgate":        0,
		"https://github.com/microsoft/go-mssqldb":        0,
		"ASP.NET Core and ADO.NET":                       0,
		"https://docs.asp.net/x":                         1,
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
		"{tenant}.acme.rs": false, "<tenant>.acme.rs": false, "${tenant}.acme.rs": false, "{tenant}.shop.example": true,
		"asp.net": true, "docs.asp.net": false, "<host>": true, "…": true,
	} {
		if got := exampleHost(host); got != want {
			t.Errorf("exampleHost(%q) = %v", host, got)
		}
	}
}

// releaseVersion finds the DriftGate release a page names: a tag of the
// action or a download, a version input, a version printed.
var releaseVersion = regexp.MustCompile(`(?:driftgate@|driftgate |tags/|version: |release download |Version=)(v\d+\.\d+\.\d+)\b`)

// TestVersionStrings: every page names the same DriftGate release.
func TestVersionStrings(t *testing.T) {
	_, all := docs(t)
	first, where := "", ""
	for _, d := range all {
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

// sentenceLimit is the most words a sentence may have in a page: 25 in
// the documentation (docs/, README.md) and the style guide; 0 for pages
// without a limit.
func sentenceLimit(path string) int {
	if strings.HasPrefix(path, "docs/") || path == "README.md" || path == ".github/docs-style.md" {
		return 25
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

// TestBudgets (a warning unless DRIFTGATE_DOCS_STRICT=1): every page of
// the documentation (docs/ and README.md) has a line budget in
// testdata/budgets.txt (path and the most lines) and stays within it, and
// all pages together stay within TOTAL. A budget for a page that does not
// exist fails.
func TestBudgets(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "budgets.txt"))
	if err != nil {
		t.Fatal(err)
	}
	budgeted, total := map[string]bool{}, false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		budget, err := strconv.Atoi(f[len(f)-1])
		if len(f) != 2 || err != nil {
			t.Fatalf("budgets.txt: %q is not <path> <lines>", line)
		}
		if f[0] == "TOTAL" {
			total = true
			n := totalLines(t, root)
			t.Logf("docs: %d lines (budget %d)", n, budget)
			if n > budget {
				warn(t, "docs: %d lines in all (budget %d)", n, budget)
			}
			continue
		}
		budgeted[f[0]] = true
		page, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f[0])))
		if err != nil {
			t.Errorf("budgets.txt: %v", err)
			continue
		}
		if n := strings.Count(string(page), "\n"); n > budget {
			warn(t, "%s: %d lines (budget %d)", f[0], n, budget)
		}
	}
	if !total {
		t.Error("budgets.txt has no TOTAL")
	}
	for _, f := range docPages(t, root) {
		if !budgeted[f] {
			warn(t, "%s: no line budget in testdata/budgets.txt", f)
		}
	}
}

// bannedWords are words the documentation does not use, in lower case,
// with what to say instead: the style guide (.github/docs-style.md) lists
// them. Words wrong only in one sense ("target" for a running service,
// "test" for a request, "recording" for a baseline) are left to review.
var bannedWords = map[string]string{
	"local mode":   `"the local layout", or "the baseline local"`,
	"snapshot":     `"baseline"`,
	"golden file":  `"baseline"`,
	"mutating":     `"a request that changes data"`,
	"note that":    "nothing",
	"in order to":  `"to"`,
	"basically":    "nothing",
	"simply":       "nothing",
	"as mentioned": "nothing",
	"please":       "nothing",
	"actually":     "nothing",
	"of course":    "nothing",
}

// proseFindings lists the banned words in the prose of a page: outside
// code blocks and code spans.
func proseFindings(d *Doc) []string {
	code := map[int]bool{}
	for _, b := range d.Blocks {
		for n := b.Line; n <= b.End; n++ {
			code[n] = true
		}
	}
	var out []string
	for i, line := range d.Lines {
		if code[i+1] {
			continue
		}
		prose := strings.ToLower(codeSpan.ReplaceAllString(line, ""))
		for w, say := range bannedWords {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(w) + `\b`).MatchString(prose) {
				out = append(out, fmt.Sprintf("%s:%d: %q (say %s)", d.Path, i+1, w, say))
			}
		}
	}
	return out
}

// TestBannedWords: the prose of the documentation (docs/ and README.md)
// uses none of bannedWords, outside code.
func TestBannedWords(t *testing.T) {
	_, all := docs(t)
	for _, d := range all {
		if !strings.HasPrefix(d.Path, "docs/") && d.Path != "README.md" {
			continue
		}
		for _, f := range proseFindings(d) {
			t.Errorf("%s is a word the documentation does not use", f)
		}
	}
	// The test finds them.
	probe, err := ParseText("docs/probe.md", "A mutating GET and a snapshot.\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := proseFindings(probe); len(got) != 2 {
		t.Errorf("the probe gives %v, want mutating and snapshot", got)
	}
}

// docPages lists the pages of the documentation: docs/ and README.md.
func docPages(t *testing.T, root string) []string {
	t.Helper()
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, "docs/") || f == "README.md" {
			out = append(out, f)
		}
	}
	return out
}

// totalLines counts the lines of every page of the documentation.
func totalLines(t *testing.T, root string) int {
	t.Helper()
	n := 0
	for _, f := range docPages(t, root) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(b), "\n")
	}
	return n
}
