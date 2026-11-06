package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/docscheck"
)

func docPages(t *testing.T) *docscheck.Pages {
	t.Helper()
	root, err := docscheck.Root()
	if err != nil {
		t.Fatal(err)
	}
	return docscheck.NewPages(root)
}

// TestHelpDocLinks: the section every command and topic links to exists
// in the docs of this checkout.
func TestHelpDocLinks(t *testing.T) {
	p := docPages(t)
	links := []string{tasksDoc}
	for _, c := range commands {
		if c.doc == "" {
			t.Errorf("%s: no doc", c.name)
		}
		links = append(links, c.doc)
	}
	for _, tp := range topics {
		links = append(links, tp.doc)
	}
	for _, l := range links {
		if err := p.Resolve("docs/README.md", l); err != nil {
			t.Errorf("help link %s: %v", l, err)
		}
	}
	_, out, _ := runCLI(t, "help", "run")
	if !strings.Contains(out, "\nDocs: "+docsBase()+"reference/commands/check.md#driftgate-run\n") {
		t.Errorf("help run has no Docs line:\n%s", out)
	}
}

// TestDocsBase: a release links the pages of its own tag, any other build
// those of main.
func TestDocsBase(t *testing.T) {
	defer func(v string) { Version = v }(Version)
	for v, want := range map[string]string{
		"v9.9.9":     "https://github.com/orangehill/driftgate/blob/v9.9.9/docs/",
		"v1.2.3":     "https://github.com/orangehill/driftgate/blob/v1.2.3/docs/",
		"dev":        "https://github.com/orangehill/driftgate/blob/main/docs/",
		"v1.2.3-rc1": "https://github.com/orangehill/driftgate/blob/main/docs/",
	} {
		Version = v
		if got := docsBase(); got != want {
			t.Errorf("Version %s: docsBase() = %s, want %s", v, got, want)
		}
	}
	Version = "v9.9.9"
	if _, out, _ := runCLI(t, "help", "run"); !strings.Contains(out, "/blob/v9.9.9/docs/reference/commands/check.md#driftgate-run") {
		t.Errorf("help run of v9.9.9:\n%s", out)
	}
}

var docFlag = regexp.MustCompile("`--([a-z0-9-]+)")

// TestCommandParity: the section each command's doc names is an H2 with
// the command's name whose first table lists exactly the command's flags
// (--config, common to all, is described once); every H2 of those pages
// that names a command (lower case, or "driftgate …") is a command's
// section.
func TestCommandParity(t *testing.T) {
	p := docPages(t)
	sections := map[string]bool{} // page#anchor of every command
	for _, c := range commands {
		page, anchor, _ := strings.Cut(c.doc, "#")
		page = "docs/" + page
		sections[page+"#"+anchor] = true
		d, err := p.Get(page)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(d.Headings, func(h docscheck.Heading) bool { return docscheck.Slug(h.Text) == anchor })
		if i < 0 {
			t.Errorf("%s: %s has no #%s", c.name, page, anchor)
			continue
		}
		h := d.Headings[i]
		if h.Level != 2 || h.Text != c.name && h.Text != "driftgate "+c.name {
			t.Errorf("%s: %s:%d: the section is %q (H%d), not an H2 named after the command", c.name, page, h.Line, h.Text, h.Level)
			continue
		}
		documented := tableFlags(d, h.Line)
		var want []string
		for _, f := range c.flags {
			want = append(want, f.name)
		}
		slices.Sort(want)
		if !slices.Equal(documented, want) {
			t.Errorf("%s: %s:%d lists the flags %v, the command has %v", c.name, page, h.Line, documented, want)
		}
	}
	pagesSeen := map[string]bool{}
	for s := range sections {
		page, _, _ := strings.Cut(s, "#")
		if pagesSeen[page] {
			continue
		}
		pagesSeen[page] = true
		d, err := p.Get(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range d.Headings {
			names := strings.HasPrefix(h.Text, "driftgate ") || h.Text != "" && h.Text[0] >= 'a' && h.Text[0] <= 'z'
			if h.Level == 2 && names && !sections[page+"#"+docscheck.Slug(h.Text)] {
				t.Errorf("%s:%d: section %q names no command of driftgate help", page, h.Line, h.Text)
			}
		}
	}
}

// tableFlags lists the flags in the first column of the first table after
// the heading on line, up to the next H2, sorted.
func tableFlags(d *docscheck.Doc, line int) []string {
	var out []string
	inTable := false
	for _, l := range d.Lines[line:] {
		if strings.HasPrefix(l, "## ") || inTable && !strings.HasPrefix(l, "|") {
			break
		}
		if !strings.HasPrefix(l, "|") {
			continue
		}
		inTable = true
		first, _, _ := strings.Cut(strings.TrimPrefix(l, "|"), "|")
		for _, m := range docFlag.FindAllStringSubmatch(first, -1) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestDriftKinds: the drift kinds of internal/compare (its constants named
// Kind…), compare.Kinds() (the text of driftgate help kinds), the kinds
// docs/reference/drift-kinds.md describes (the first column of its kind
// tables) and the output of driftgate help kinds are one set; the page's
// Meaning of each kind is its text in compare.Kinds().
func TestDriftKinds(t *testing.T) {
	root, err := docscheck.Root()
	if err != nil {
		t.Fatal(err)
	}
	code := codeKinds(t, filepath.Join(root, "internal", "compare"))
	if len(code) == 0 {
		t.Fatal("internal/compare has no Kind constants")
	}
	d, err := docPages(t).Get("docs/reference/drift-kinds.md")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	page, meanings := tableKinds(d)
	for _, k := range compare.Kinds() {
		listed = append(listed, k.Name)
		if k.Text == "" {
			t.Errorf("compare.Kinds(): %s has no text", k.Name)
		}
		if m, ok := meanings[k.Name]; ok && m != k.Text {
			t.Errorf("docs/reference/drift-kinds.md: the meaning of %s is %q, compare.Kinds() has %q", k.Name, m, k.Text)
		}
	}
	_, out, _ := runCLI(t, "help", "kinds")
	var help []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && len(f) > 0 {
			help = append(help, f[0])
		}
	}
	for _, c := range []struct {
		where string
		kinds []string
	}{{"compare.Kinds()", listed}, {"docs/reference/drift-kinds.md", page}, {"driftgate help kinds", help}} {
		for _, k := range code {
			if !slices.Contains(c.kinds, k) {
				t.Errorf("%s does not list the drift kind %s", c.where, k)
			}
		}
		for _, k := range c.kinds {
			if !slices.Contains(code, k) {
				t.Errorf("%s lists %s, which is no drift kind of internal/compare", c.where, k)
			}
		}
		if dup := len(c.kinds) - len(slices.Compact(slices.Sorted(slices.Values(c.kinds)))); dup > 0 {
			t.Errorf("%s lists %d kinds twice", c.where, dup)
		}
	}
}

// codeKinds lists the values of the string constants named Kind… in the
// package in dir, without its tests.
func codeKinds(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Kind") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						out = append(out, v)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

var codeWord = regexp.MustCompile("^`([a-z-]+)`$")

// tableKinds lists the kinds in the first column of the kind tables of a
// page, those whose header row starts with "| Kind |", outside fenced
// blocks: one kind, a code span, per row; and the second column of each
// (its meaning), without the backquotes of code spans.
func tableKinds(d *docscheck.Doc) ([]string, map[string]string) {
	code := map[int]bool{}
	for _, b := range d.Blocks {
		for n := b.Line; n <= b.End; n++ {
			code[n] = true
		}
	}
	var out []string
	meanings := map[string]string{}
	inTable := false
	for i, l := range d.Lines {
		if code[i+1] || !strings.HasPrefix(l, "|") {
			inTable = false
			continue
		}
		if strings.HasPrefix(l, "| Kind |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		cells := strings.Split(strings.TrimPrefix(l, "|"), "|")
		if m := codeWord.FindStringSubmatch(strings.TrimSpace(cells[0])); m != nil {
			out = append(out, m[1])
			if len(cells) > 1 {
				meanings[m[1]] = strings.ReplaceAll(strings.TrimSpace(cells[1]), "`", "")
			}
		}
	}
	return out, meanings
}
