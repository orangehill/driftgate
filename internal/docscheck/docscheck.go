// Package docscheck reads the Markdown files of the repository for the docs
// tests: their headings, links, fenced blocks and check markers. The tests
// of this package check the pages; internal/cli checks its help links with
// Resolve.
package docscheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Doc is one Markdown file.
type Doc struct {
	Path     string // relative to the repository root, with slashes
	Lines    []string
	Headings []Heading
	Blocks   []Block
	Links    []Link
	Markers  []Marker // check markers not followed by a fenced block
	anchors  map[string]bool
}

// Heading is an ATX heading outside fenced blocks.
type Heading struct {
	Line  int // 1-based
	Level int
	Text  string // as written, without the #s
}

// Block is a fenced code block.
type Block struct {
	Line   int    // the line of the opening fence, 1-based
	Lang   string // the info string's first word
	Body   string
	Marker string // the check marker on the line before: "config-fragment files=a", or ""
}

// Marker is a check marker; in Doc.Markers, one that has no fenced block
// on the next line.
type Marker struct {
	Line int
	Text string
}

// Link is a link or image target outside code.
type Link struct {
	Line   int
	Target string
}

var (
	fence      = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([^\\s`]*)")
	atx        = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)(\s+#+)?\s*$`)
	marker     = regexp.MustCompile(`^\s*<!--\s*check:\s*(.*?)\s*-->\s*$`)
	codeSpan   = regexp.MustCompile("`+[^`]*`+")
	inlineLink = regexp.MustCompile(`!?\[(?:[^\]\[]|\[[^\]]*\])*\]\(\s*<?([^)\s>]*)>?(?:\s+"[^"]*")?\s*\)`)
	refDef     = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:\s*<?(\S+?)>?(\s+".*")?\s*$`)
	htmlID     = regexp.MustCompile(`<a\s+(?:name|id)="([^"]+)"`)
	htmlTag    = regexp.MustCompile(`<[^>]+>`)
	mdLinkText = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
)

// Parse reads the Markdown file rel (slash-separated) under root.
func Parse(root, rel string) (*Doc, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	d := &Doc{Path: rel, Lines: strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")}
	var open *Block
	var closer string
	var body []string
	for i, line := range d.Lines {
		n := i + 1
		if open != nil {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, closer) && strings.Trim(t, closer[:1]) == "" {
				open.Body = strings.Join(body, "\n")
				d.Blocks = append(d.Blocks, *open)
				open, body = nil, nil
				continue
			}
			body = append(body, line)
			continue
		}
		if m := fence.FindStringSubmatch(line); m != nil {
			open, closer = &Block{Line: n, Lang: strings.ToLower(m[2])}, m[1]
			if i > 0 {
				if mm := marker.FindStringSubmatch(d.Lines[i-1]); mm != nil {
					open.Marker = mm[1]
				}
			}
			continue
		}
		if mm := marker.FindStringSubmatch(line); mm != nil {
			if i+1 >= len(d.Lines) || !fence.MatchString(d.Lines[i+1]) {
				d.Markers = append(d.Markers, Marker{Line: n, Text: mm[1]})
			}
			continue
		}
		if m := atx.FindStringSubmatch(line); m != nil {
			d.Headings = append(d.Headings, Heading{Line: n, Level: len(m[1]), Text: m[2]})
		}
		prose := codeSpan.ReplaceAllString(line, "")
		for _, m := range inlineLink.FindAllStringSubmatch(prose, -1) {
			d.Links = append(d.Links, Link{Line: n, Target: m[1]})
		}
		if m := refDef.FindStringSubmatch(prose); m != nil {
			d.Links = append(d.Links, Link{Line: n, Target: m[1]})
		}
	}
	if open != nil {
		return nil, fmt.Errorf("%s:%d: the fenced block is not closed", rel, open.Line)
	}
	d.anchors = map[string]bool{}
	seen := map[string]int{}
	for _, h := range d.Headings {
		s := Slug(h.Text)
		if k := seen[s]; k > 0 {
			d.anchors[s+"-"+strconv.Itoa(k)] = true
		} else {
			d.anchors[s] = true
		}
		seen[s]++
	}
	for _, line := range d.Lines {
		for _, m := range htmlID.FindAllStringSubmatch(line, -1) {
			d.anchors[m[1]] = true
		}
	}
	return d, nil
}

// HasAnchor reports whether #anchor exists in the page.
func (d *Doc) HasAnchor(anchor string) bool { return d.anchors[strings.ToLower(anchor)] }

// Slug is the anchor GitHub gives a heading: the text as shown, lower case,
// without characters other than letters, digits, spaces, - and _, with
// spaces as -. A repeated slug gets -1, -2 (Parse).
func Slug(heading string) string {
	s := mdLinkText.ReplaceAllString(heading, "$1")
	s = htmlTag.ReplaceAllString(s, "")
	s = strings.NewReplacer("`", "", "*", "").Replace(s)
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Root is the repository root: the nearest folder upwards with go.mod.
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("go.mod not found above the working folder")
		}
		dir = up
	}
}

// Files lists every Markdown file under root (slash-separated, relative),
// outside .git and testdata.
func Files(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && (e.Name() == ".git" || e.Name() == "testdata" || e.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out, err
}

// Pages parses the Markdown files of a repository once.
type Pages struct {
	Root string
	mu   sync.Mutex
	docs map[string]*Doc
}

// NewPages reads pages under root as they are asked for.
func NewPages(root string) *Pages { return &Pages{Root: root, docs: map[string]*Doc{}} }

// Get parses the page rel, once.
func (p *Pages) Get(rel string) (*Doc, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.docs[rel]; ok {
		return d, nil
	}
	d, err := Parse(p.Root, rel)
	if err != nil {
		return nil, err
	}
	p.docs[rel] = d
	return d, nil
}

// repoURL is where the repository's files are on GitHub; links to a file of
// any ref are checked as local paths.
var repoURL = regexp.MustCompile(`^https://github\.com/orangehill/driftgate/(?:blob|tree)/[^/]+/(.*)$`)

// Local is the local target of a link from the page from: the path relative
// to the root and the anchor. ok is false for a link that leaves the
// repository (another site, mailto:).
func Local(from, target string) (rel, anchor string, ok bool) {
	if m := repoURL.FindStringSubmatch(target); m != nil {
		target, from = "/"+m[1], ""
	} else if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "//") {
		return "", "", false
	}
	target, anchor, _ = strings.Cut(target, "#")
	switch {
	case target == "":
		rel = from
	case strings.HasPrefix(target, "/"):
		rel = path.Clean(strings.TrimPrefix(target, "/"))
	default:
		rel = path.Join(path.Dir(from), target)
	}
	return rel, anchor, true
}

// Resolve checks that the link target, written in the page from, names a
// file or folder of the repository and, for a Markdown file, an anchor it
// has.
func (p *Pages) Resolve(from, target string) error {
	rel, anchor, ok := Local(from, target)
	if !ok {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s leaves the repository", target)
	}
	fi, err := os.Stat(filepath.Join(p.Root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("%s: no file %s", target, rel)
	}
	if anchor == "" || fi.IsDir() || !strings.HasSuffix(rel, ".md") {
		return nil
	}
	d, err := p.Get(rel)
	if err != nil {
		return err
	}
	if !d.HasAnchor(anchor) {
		return fmt.Errorf("%s: %s has no heading #%s", target, rel, anchor)
	}
	return nil
}
