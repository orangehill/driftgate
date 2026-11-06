package cli

import (
	"regexp"
	"slices"
	"strings"
	"testing"

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
	if !strings.Contains(out, "\nDocs: "+docsBase()+"commands.md#run\n") {
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
	if _, out, _ := runCLI(t, "help", "run"); !strings.Contains(out, "/blob/v9.9.9/docs/commands.md#run") {
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
