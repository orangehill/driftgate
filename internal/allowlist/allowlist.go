// Package allowlist holds the values that may change on every request (time,
// tokens). Their value is not compared, but their format still is, and every
// entry carries a reason.
package allowlist

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/sheet"
)

const Version = 1

// RawPath is the path of a body rule for the whole body: when the bytes of a
// body that is not JSON differ, the current body as text must match the
// format, and then it is not compared further. It is taken literally, not as
// a glob, so a wildcard path never stands for the whole body.
const RawPath = "$raw"

// HeaderRule allows a header's value to change while it keeps its format,
// or, with IgnoreCase, only in letter case (charset and media type names are
// case-insensitive, RFC 9110).
type HeaderRule struct {
	Target     string `yaml:"target,omitempty"`  // only requests of this service
	Request    string `yaml:"request,omitempty"` // glob over request IDs; empty means all
	Name       string `yaml:"name"`
	Format     string `yaml:"format,omitempty"`
	IgnoreCase bool   `yaml:"ignore_case,omitempty"`
	Reason     string `yaml:"reason"`

	re   *regexp.Regexp
	n    int  // position in the list from 1, once tracked (Track)
	used bool // applied to a change of the request (ForRequest)
}

// BodyRule allows a value inside a body to change while it keeps its format.
// The path is a JSON path ($.token), a path in an HTML page (html>body>p@class,
// html>body>script#__NEXT_DATA__$.buildId) or in its RSC payload
// (self.__next_f$.0.b), a cell of a spreadsheet (xlsx:Users!A1,
// csv!A1), or a glob over them. The path RawPath names the
// whole body of a response that is not JSON (see RawRule).
type BodyRule struct {
	Target    string `yaml:"target,omitempty"`  // only requests of this service
	Request   string `yaml:"request,omitempty"` // glob over request IDs; empty means all
	Path      string `yaml:"path"`              // glob over body paths
	Format    string `yaml:"format,omitempty"`
	AnyLength bool   `yaml:"any_length,omitempty"` // arrays at this path may change length; values need a format
	// Parts: the format matches parts of the value (a hash in an asset URL, a
	// build id inside a script); only those parts may change, the rest of
	// the value is still compared. Without it the format matches the whole
	// value.
	Parts  bool   `yaml:"parts,omitempty"`
	Reason string `yaml:"reason"`

	re     *regexp.Regexp
	part   *regexp.Regexp // the format anywhere in the value, for Parts
	prefix string         // every path the rule matches starts with it (glob.Literal)
	suffix string         // and ends with it
	cell   *cellGlob      // a path that names cells after a !, matched part by part
	n      int            // position in the list from 1, once tracked (Track)
	used   bool           // applied to a change of the request (ForRequest)
}

// cellGlob is a cell rule's path in its parts: before the cell (csv,
// xlsx:<worksheet>), the column and the row, each a glob. The column is
// the cell's part up to its last letter and the row the rest; without a
// letter, the column is what comes before the first digit, and a cell of
// * only is every cell. So B* is column B, *1 row 1 (not A11), B*1 the
// rows of column B that end in 1.
type cellGlob struct {
	sheet, col, row string
}

// parseCellGlob splits a cell rule's path; nil when it names no cell
// after a ! (xlsx:*, whose * stands for the rest) or no cell can match it.
func parseCellGlob(path string) *cellGlob {
	i := strings.LastIndexByte(path, '!')
	if i < 0 || !IsCellPath(path) {
		return nil
	}
	g := &cellGlob{sheet: path[:i]}
	ref := path[i+1:]
	last := strings.LastIndexFunc(ref, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	digit := strings.IndexAny(ref, "0123456789")
	switch {
	case last >= 0:
		g.col, g.row = ref[:last+1], ref[last+1:]
	case digit >= 0:
		g.col, g.row = ref[:digit], ref[digit:]
	case ref != "" && strings.Trim(ref, "*") == "":
		g.col, g.row = "*", "*"
	default:
		return nil
	}
	if g.col == "" || g.row == "" {
		return nil
	}
	return g
}

// match reports whether a cell's path (xlsx:Users!B2) matches.
func (g *cellGlob) match(path string) bool {
	i := strings.LastIndexByte(path, '!')
	if i < 0 || !glob.Match(g.sheet, path[:i]) {
		return false
	}
	ref := path[i+1:]
	j := strings.IndexFunc(ref, func(r rune) bool { return r < 'A' || r > 'Z' })
	if j <= 0 {
		return false
	}
	return glob.Match(g.col, ref[:j]) && glob.Match(g.row, ref[j:])
}

// ArrayRule pairs the items of a JSON array by a key instead of by
// position: an item inserted at the front is one new item, not a change of
// every item after it. Items that are new or gone are drifts (unless an
// any_length rule lets the array change length), and so is a changed order
// of the items both sides have, unless AnyOrder.
type ArrayRule struct {
	Target   string `yaml:"target,omitempty"`
	Request  string `yaml:"request,omitempty"`
	Path     string `yaml:"path"` // glob over JSON paths of arrays
	Key      string `yaml:"key"`  // a member of every item, or a path into it (user.id)
	AnyOrder bool   `yaml:"any_order,omitempty"`
	Reason   string `yaml:"reason"`

	steps          []string // Key split at the dots
	prefix, suffix string   // glob.Literal of Path
}

// List is the parsed allow-list file.
type List struct {
	Version int          `yaml:"version"`
	Headers []HeaderRule `yaml:"headers"`
	Body    []BodyRule   `yaml:"body"`
	Arrays  []ArrayRule  `yaml:"arrays,omitempty"`

	// request is set by ForRequest: every body rule applies to it.
	request string
	// The body rules of a list made for one request (ForRequest, Copy) by
	// kind: values (JSON, page texts and attributes), cells, the whole body.
	indexed       bool
	values, cells []*BodyRule
	raw           bool   // the list has a whole-body rule
	usage         *Usage // set by Track
}

// Empty returns a list with no rules.
func Empty() *List { return &List{Version: Version} }

// Load reads an allow-list; a missing file is an empty list.
func Load(path string) (*List, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Empty(), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes and validates an allow-list.
func Parse(raw []byte) (*List, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	l := Empty()
	if err := dec.Decode(l); err != nil {
		return nil, fmt.Errorf("allowlist: %w", err)
	}
	return l, l.Validate()
}

// Validate checks every rule and compiles its format.
func (l *List) Validate() error {
	if l.Version != Version {
		return fmt.Errorf("allowlist: unsupported version %d (this build reads version %d)", l.Version, Version)
	}
	for i := range l.Headers {
		r := &l.Headers[i]
		if r.Name == "" || strings.TrimSpace(r.Reason) == "" || (r.Format == "") == !r.IgnoreCase {
			return fmt.Errorf("allowlist: header rule %d needs name, reason and either format or ignore_case", i+1)
		}
		if r.IgnoreCase {
			continue
		}
		re, err := compile(r.Format)
		if err != nil {
			return fmt.Errorf("allowlist: header %s: %w", r.Name, err)
		}
		r.re = re
	}
	for i := range l.Body {
		r := &l.Body[i]
		if r.Path == "" || strings.TrimSpace(r.Reason) == "" {
			return fmt.Errorf("allowlist: body rule %d needs path and reason", i+1)
		}
		if r.Path == RawPath && (r.Format == "" || r.AnyLength || r.Parts) {
			return fmt.Errorf("allowlist: body rule %s is about the whole body: it needs a format and takes neither any_length nor parts", r.Path)
		}
		if r.Format == "" && !r.AnyLength {
			return fmt.Errorf("allowlist: body rule %s needs a format (or any_length)", r.Path)
		}
		if err := checkPath(r.Path); err != nil {
			return fmt.Errorf("allowlist: body rule %d: %w", i+1, err)
		}
		if IsCellPath(r.Path) && r.AnyLength {
			return fmt.Errorf("allowlist: body rule %s: any_length is about JSON arrays, and this path names cells of a spreadsheet; give a format", r.Path)
		}
		if r.Format == "" && strings.HasPrefix(r.Path, "html") && !reachesJSON(r.Path) {
			return fmt.Errorf("allowlist: body rule %s: any_length is about JSON arrays, and this path names an element or an attribute of a page; give a format", r.Path)
		}
		if r.Parts && (r.Format == "" || r.AnyLength) {
			return fmt.Errorf("allowlist: body rule %s: parts needs a format and cannot go with any_length", r.Path)
		}
		if r.Format != "" {
			re, err := compile(r.Format)
			if err != nil {
				return fmt.Errorf("allowlist: body %s: %w", r.Path, err)
			}
			r.re = re
		}
		r.prefix, r.suffix = glob.Literal(r.Path)
		r.cell = parseCellGlob(r.Path)
		if r.Parts {
			r.part = regexp.MustCompile(`(?:` + r.Format + `)`)
			if r.part.MatchString("") {
				return fmt.Errorf("allowlist: body %s: a parts format must not match an empty text", r.Path)
			}
		}
	}
	for i := range l.Arrays {
		r := &l.Arrays[i]
		if r.Path == "" || r.Key == "" || strings.TrimSpace(r.Reason) == "" {
			return fmt.Errorf("allowlist: array rule %d needs path, key and reason", i+1)
		}
		if err := checkPath(r.Path); err != nil {
			return fmt.Errorf("allowlist: array rule %d: %w", i+1, err)
		}
		if r.Path == RawPath || IsCellPath(r.Path) || strings.HasPrefix(r.Path, "html") && !reachesJSON(r.Path) {
			return fmt.Errorf("allowlist: array rule %s: the path names no JSON array", r.Path)
		}
		r.steps = strings.Split(r.Key, ".")
		if slices.Contains(r.steps, "") {
			return fmt.Errorf("allowlist: array rule %s: key %q: a member name, or names joined by dots (user.id)", r.Path, r.Key)
		}
		r.prefix, r.suffix = glob.Literal(r.Path)
	}
	return nil
}

// checkPath checks the paths into an HTML page: they start at html (or at
// self.__next_f$ for the RSC payload, or with a wildcard) and name an
// attribute after @. A JSON path goes on after $ with . or [ (or a
// wildcard): any other $… names no place and would never apply. Other paths
// are JSON paths, as before HTML existed.
func checkPath(path string) error {
	switch {
	case path == RawPath:
	case strings.HasPrefix(path, "$"):
		if len(path) > 1 && !strings.ContainsRune(".[*?", rune(path[1])) {
			return fmt.Errorf("path %q names no place: a JSON path goes on after $ with . or [ ($.token, $[0]), and %s is the whole body", path, RawPath)
		}
	case strings.HasPrefix(path, "*"), strings.HasPrefix(path, "?"):
	case path == "csv", path == "xlsx", strings.HasPrefix(path, "csv!"), strings.HasPrefix(path, "xlsx:"), strings.HasPrefix(path, "xlsx!"):
		return checkCellPath(path)
	case strings.HasPrefix(path, "html"), strings.HasPrefix(path, "self.__next_f$"):
		if strings.HasSuffix(path, "@") {
			return fmt.Errorf("path %q: no attribute name after @", path)
		}
	case strings.ContainsAny(path, ">@"):
		return fmt.Errorf("path %q: a path into an HTML page starts at html (html>body>…)", path)
	}
	return nil
}

// IsCellPath reports whether a rule's path names cells of a spreadsheet: it
// starts with xlsx: or csv!. Only such rules apply to cells; a path that
// starts with a wildcard (*) stays with JSON and HTML, as before cells had
// rules.
func IsCellPath(path string) bool {
	return strings.HasPrefix(path, "xlsx:") || strings.HasPrefix(path, "csv!")
}

// checkCellPath checks a path into a spreadsheet: csv!<cell> or
// xlsx:<worksheet>!<cell>, the worksheet written as in a drift
// (sheet.PathName) or a glob, and the cell a reference (B2) or a glob over
// one (B* for column B, *1 for row 1). A path that no cell can have is
// refused, and so is the bare part-name form (xlsx:sheet1!B2) of reports
// written before worksheets had their workbook names, whose meaning changed.
func checkCellPath(path string) error {
	cells := "a cell rule names cells: csv!A1, or xlsx:<worksheet>!A1 with the worksheet's name from the report (xlsx:Users!A1, xlsx:'Q1 2026'!B*)"
	var name, ref string
	switch {
	case strings.HasPrefix(path, "csv!"):
		ref = path[len("csv!"):]
	case strings.HasPrefix(path, "xlsx:"):
		rest := path[len("xlsx:"):]
		i := strings.LastIndexByte(rest, '!')
		if i < 0 && strings.Contains(rest, "*") {
			return nil // xlsx:* or xlsx:Users*: the * stands for the rest
		}
		if i < 0 {
			return fmt.Errorf("path %q names a whole worksheet; %s", path, cells)
		}
		name, ref = rest[:i], rest[i+1:]
		if err := checkSheetName(path, name); err != nil {
			return err
		}
	default:
		return fmt.Errorf("path %q names no cell; %s", path, cells)
	}
	return checkCellRef(path, ref)
}

// checkSheetName checks the worksheet of a cell path. A glob is taken as it
// is; a name must be written the one way a drift writes it.
func checkSheetName(path, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("path %q: no worksheet between xlsx: and the ! before the cell", path)
	case strings.ContainsAny(name, "*?"):
		return nil
	case sheet.IsPartLike(name):
		return fmt.Errorf("path %q names the worksheet by its part (%s), as reports did before worksheets had the names the workbook gives them; "+
			"write the worksheet's name from a new report (xlsx:Sheet1!A1, xlsx:Users!A1), or [%s] for a workbook that names no worksheet; "+
			"a worksheet the workbook really names %s is xlsx:'%s'!…", path, name, name, name, name)
	case strings.HasPrefix(name, "[") && strings.HasSuffix(name, "]"):
		if !sheet.IsPartLike(name[1 : len(name)-1]) {
			return fmt.Errorf("path %q: [ ] holds the part name of a worksheet the workbook does not name ([sheet1]); a workbook's name cannot contain [ or ]", path)
		}
		return nil
	case strings.HasPrefix(name, "'") && strings.HasSuffix(name, "'") && len(name) >= 2:
		inner := name[1 : len(name)-1]
		if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
			return fmt.Errorf("path %q: a quote inside a quoted worksheet name is doubled ('O''Brien')", path)
		}
		plain := strings.ReplaceAll(inner, "''", "'")
		if q := sheet.QuoteName(plain); q != name {
			return fmt.Errorf("path %q: drifts write this worksheet as %s, so the rule would never apply; write xlsx:%s!…", path, q, q)
		}
		return nil
	}
	if q := sheet.QuoteName(name); q != name {
		return fmt.Errorf("path %q: a worksheet name with spaces or signs is quoted, as drifts write it: xlsx:%s!…", path, q)
	}
	return nil
}

// checkCellRef checks the cell of a cell path: a reference such as B2, or a
// glob that can match one (letters before digits, a row from 1), whose
// column and row parts are told apart (cellGlob).
func checkCellRef(path, ref string) error {
	never := func(why string) error {
		return fmt.Errorf("path %q: %s, so no cell can match it; a cell is a column and a row (B2), B* is column B and *2 row 2", path, why)
	}
	if ref == "" {
		return never("no cell after !")
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z':
			return fmt.Errorf("path %q: columns are written in capitals (B2, not b2)", path)
		case !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '*' || r == '?'):
			return never("a cell is letters and digits")
		}
	}
	isLetter := func(b byte) bool { return b >= 'A' && b <= 'Z' }
	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }
	switch {
	case isDigit(ref[0]):
		return never("it starts with a digit, not a column")
	case isLetter(ref[len(ref)-1]):
		return never("it ends with a letter, not a row")
	case !strings.Contains(ref, "*") && len(ref) < 2:
		return never("it is shorter than a column and a row")
	}
	if strings.Trim(ref, "*?") == "" && strings.Contains(ref, "?") {
		return never("with no letter or digit in it, ? cannot tell the column from the row (* alone is every cell)")
	}
	digit := false
	for i := 0; i < len(ref); i++ {
		switch {
		case isDigit(ref[i]):
			if ref[i] == '0' && i > 0 && isLetter(ref[i-1]) {
				return never("rows count from 1")
			}
			digit = true
		case isLetter(ref[i]) && digit:
			return never("a letter comes after a digit")
		}
	}
	return nil
}

// reachesJSON reports whether a path into a page (a glob) can name a place
// in JSON, where arrays are: JSON in the page (…$…) or a wildcard that can
// stand for that.
func reachesJSON(path string) bool { return strings.ContainsAny(path, "$*?") }

func compile(format string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(`^(?:` + format + `)$`)
	if err != nil {
		return nil, fmt.Errorf("invalid format %q: %w", format, err)
	}
	return re, nil
}

// Header returns the first rule for a header name (case-insensitive) that
// applies to every request.
func (l *List) Header(name string) *HeaderRule { return l.HeaderFor("", name) }

// HeaderFor returns the first rule for a header name that applies to the
// request: a rule without request applies to all. In a list for one
// request the rule counts as used (Track).
func (l *List) HeaderFor(request, name string) *HeaderRule {
	for i := range l.Headers {
		r := &l.Headers[i]
		if !strings.EqualFold(r.Name, name) {
			continue
		}
		if r.Request == "" || (request != "" && glob.Match(r.Request, request)) {
			if l.indexed {
				r.used = true
			}
			return r
		}
	}
	return nil
}

// ForTarget is the list of rules that apply to requests of one service: the
// rules of that service and the rules that name none.
func (l *List) ForTarget(target string) *List {
	out := &List{Version: l.Version, usage: l.usage}
	for _, r := range l.Headers {
		if r.Target == "" || r.Target == target {
			out.Headers = append(out.Headers, r)
		}
	}
	for _, r := range l.Body {
		if r.Target == "" || r.Target == target {
			out.Body = append(out.Body, r)
		}
	}
	for _, r := range l.Arrays {
		if r.Target == "" || r.Target == target {
			out.Arrays = append(out.Arrays, r)
		}
	}
	return out
}

// ForRequest is the list of rules that can apply to one request: the body
// rules for other requests are left out once, instead of being matched
// against the request at every value of its body, and the rest are sorted
// by kind. The list is the request's own: it counts which rules applied to
// a change (Done).
func (l *List) ForRequest(request string) *List {
	out := &List{Version: l.Version, Headers: append([]HeaderRule(nil), l.Headers...), request: request, usage: l.usage}
	for _, r := range l.Body {
		if r.Request == "" || (l.request != "" && l.request == request) || glob.Match(r.Request, request) {
			out.Body = append(out.Body, r)
		}
	}
	for _, r := range l.Arrays {
		if r.Request == "" || (l.request != "" && l.request == request) || glob.Match(r.Request, request) {
			out.Arrays = append(out.Arrays, r)
		}
	}
	out.index()
	return out
}

// Copy is ForRequest without leaving out the rules of other requests.
func (l *List) Copy() *List {
	out := &List{Version: l.Version, Headers: append([]HeaderRule(nil), l.Headers...), Body: append([]BodyRule(nil), l.Body...), Arrays: l.Arrays, request: l.request, usage: l.usage}
	out.index()
	return out
}

func (l *List) index() {
	l.indexed = true
	for i := range l.Body {
		r := &l.Body[i]
		switch {
		case r.Path == RawPath:
			l.raw = true
		case IsCellPath(r.Path):
			l.cells = append(l.cells, r)
		default:
			l.values = append(l.values, r)
		}
	}
}

// rules returns the body rules of a kind: from the index of a list for one
// request, else picked from all.
func (l *List) rules(cells bool) []*BodyRule {
	if l.indexed {
		if cells {
			return l.cells
		}
		return l.values
	}
	var out []*BodyRule
	for i := range l.Body {
		if r := &l.Body[i]; r.Path != RawPath && IsCellPath(r.Path) == cells {
			out = append(out, r)
		}
	}
	return out
}

// HasCellRules reports whether a rule can apply to cells of a spreadsheet:
// a body rule whose path starts with xlsx: or csv! (IsCellPath).
func (l *List) HasCellRules() bool { return len(l.rules(true)) > 0 }

// RawRule returns the first whole-body rule (path RawPath) that applies to
// the request, or nil. The caller asks only when the rule would apply (no
// body is JSON): in a list for one request it then counts as used.
func (l *List) RawRule(request string) *BodyRule {
	for i := range l.Body {
		if r := &l.Body[i]; r.Path == RawPath && l.applies(r, request) {
			l.use(r)
			return r
		}
	}
	return nil
}

// HasRawRule reports whether a whole-body rule applies to the request,
// without counting it as used.
func (l *List) HasRawRule(request string) bool {
	if l.indexed && !l.raw {
		return false
	}
	for i := range l.Body {
		if r := &l.Body[i]; r.Path == RawPath && l.applies(r, request) {
			return true
		}
	}
	return false
}

// ValueRules returns the rules for a value (a JSON scalar, a text or an
// attribute of an HTML page) at a body path of a request: the first
// matching rule with a whole-value format, which then decides alone, and
// otherwise every matching rule with parts. A rule without a format
// (any_length) is about the length of JSON arrays and says nothing about a
// value, so it is passed over. The caller asks for a value that changed:
// in a list for one request the rules returned count as used.
func (l *List) ValueRules(request, path string) (*BodyRule, []*BodyRule) {
	return l.match(l.rules(false), request, path)
}

// TextRules is ValueRules; texts and attributes of a page are values.
func (l *List) TextRules(request, path string) (*BodyRule, []*BodyRule) {
	return l.ValueRules(request, path)
}

// CellRules is ValueRules for a cell of a spreadsheet: only rules whose path
// names cells (IsCellPath) apply to them.
func (l *List) CellRules(request, path string) (*BodyRule, []*BodyRule) {
	return l.match(l.rules(true), request, path)
}

func (l *List) match(rules []*BodyRule, request, path string) (*BodyRule, []*BodyRule) {
	var parts []*BodyRule
	for _, r := range rules {
		if r.re == nil || !r.Reaches(path) || !l.applies(r, request) {
			continue
		}
		if !r.Parts {
			l.use(r)
			return r, nil
		}
		parts = append(parts, r)
	}
	for _, r := range parts {
		l.use(r)
	}
	return nil, parts
}

// Reaches reports whether the rule's path matches a body path; the start
// and end of its glob rule most paths out before the glob is matched. A
// cell rule matches the worksheet, the column and the row apart
// (cellGlob): what it matches is always a part of what its glob does.
func (r *BodyRule) Reaches(path string) bool {
	if !strings.HasPrefix(path, r.prefix) || !strings.HasSuffix(path, r.suffix) {
		return false
	}
	if r.cell != nil {
		return r.cell.match(path)
	}
	return glob.Match(r.Path, path)
}

// use counts a rule of a list for one request as used.
func (l *List) use(r *BodyRule) {
	if l.indexed {
		r.used = true
	}
}

// AnyLength reports whether a rule with any_length lets the JSON array at a
// body path of a request change its length (asked for an array that did).
func (l *List) AnyLength(request, path string) bool {
	for _, r := range l.rules(false) {
		if r.AnyLength && r.Reaches(path) && l.applies(r, request) {
			l.use(r)
			return true
		}
	}
	return false
}

// ArrayKey returns the first array rule that applies to the JSON array at a
// body path of a request, or nil: its items are then paired by key.
func (l *List) ArrayKey(request, path string) *ArrayRule {
	for i := range l.Arrays {
		r := &l.Arrays[i]
		if r.Reaches(path) && (r.Request == "" || (l.request != "" && l.request == request) || glob.Match(r.Request, request)) {
			return r
		}
	}
	return nil
}

// KeySteps is the key of an array rule as member names (user.id: user, id).
func (r *ArrayRule) KeySteps() []string { return r.steps }

// Reaches reports whether the rule's path matches the path of an array.
func (r *ArrayRule) Reaches(path string) bool {
	return strings.HasPrefix(path, r.prefix) && strings.HasSuffix(path, r.suffix) && glob.Match(r.Path, path)
}

// applies reports whether a body rule applies to a request: a rule without
// request applies to all.
func (l *List) applies(r *BodyRule, request string) bool {
	return r.Request == "" || (l.request != "" && l.request == request) || glob.Match(r.Request, request)
}

// Usage counts, over one comparison, the compared requests every rule
// applies to and those where it applied to a change. Requests are compared
// in parallel; each counts in its own list (ForRequest) and adds once, at
// the end (Done).
type Usage struct {
	headers, body []use
}

type use struct{ requests, used atomic.Int64 }

// Track returns the list with a new Usage and its rules numbered: lists
// made from it (ForTarget, ForRequest) count into it.
func (l *List) Track() *List {
	out := *l
	out.Headers = append([]HeaderRule(nil), l.Headers...)
	out.Body = append([]BodyRule(nil), l.Body...)
	for i := range out.Headers {
		out.Headers[i].n = i + 1
	}
	for i := range out.Body {
		out.Body[i].n = i + 1
	}
	out.usage = &Usage{headers: make([]use, len(l.Headers)), body: make([]use, len(l.Body))}
	return &out
}

// Done adds what the rules of a list for one request did to the Usage of
// the list it was made from. values is whether the comparison went as far
// as values (where header and value rules apply), bodies whether the
// request's bodies were compared (not when a private body is not on this
// machine): a rule that had no chance is not counted for the request.
func (l *List) Done(request string, values, bodies bool) {
	if l.usage == nil || !l.indexed {
		return
	}
	for i := range l.Headers {
		r := &l.Headers[i]
		if r.n > 0 && values && (r.Request == "" || glob.Match(r.Request, request)) {
			u := &l.usage.headers[r.n-1]
			u.requests.Add(1)
			if r.used {
				u.used.Add(1)
			}
		}
	}
	for i := range l.Body {
		r := &l.Body[i]
		if r.n > 0 && bodies && (values || r.AnyLength) && l.applies(r, request) {
			u := &l.usage.body[r.n-1]
			u.requests.Add(1)
			if r.used {
				u.used.Add(1)
			}
		}
	}
}

// RuleRef names a rule of the list in a report: its kind, its position and
// what it is about.
type RuleRef struct {
	Kind    string `json:"kind"` // header, body or array
	Rule    int    `json:"rule"` // its position among the rules of its kind, from 1
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Request string `json:"request,omitempty"`
	Target  string `json:"target,omitempty"`
}

// String is "body rule 3 ($.token, request get-*)".
func (r RuleRef) String() string {
	what := r.Path
	if r.Kind == "header" {
		what = r.Name
	}
	if r.Request != "" {
		what += ", request " + r.Request
	}
	if r.Target != "" {
		what += ", target " + r.Target
	}
	return fmt.Sprintf("%s rule %d (%s)", r.Kind, r.Rule, what)
}

func arrayRef(i int, r *ArrayRule) RuleRef {
	return RuleRef{Kind: "array", Rule: i + 1, Path: r.Path, Request: r.Request, Target: r.Target}
}

func headerRef(i int, r *HeaderRule) RuleRef {
	return RuleRef{Kind: "header", Rule: i + 1, Name: r.Name, Request: r.Request, Target: r.Target}
}

func bodyRef(i int, r *BodyRule) RuleRef {
	return RuleRef{Kind: "body", Rule: i + 1, Path: r.Path, Request: r.Request, Target: r.Target}
}

// Unused is a rule that did nothing in a comparison.
type Unused struct {
	RuleRef
	// Why: "no request" (it applies to none of the compared requests) or
	// "no change" (nothing it applies to changed).
	Why string `json:"why"`
}

// Unused lists the rules of a tracked list (Track) that applied to no
// change in the comparison; with selected, only those that apply to a
// compared request (a comparison of some requests says nothing about the
// rules of the others).
func (l *List) Unused(selected bool) []Unused {
	if l.usage == nil {
		return nil
	}
	var out []Unused
	add := func(u Unused, n *use) {
		switch {
		case n.used.Load() > 0:
			return
		case n.requests.Load() == 0:
			if selected {
				return
			}
			u.Why = "no request"
		default:
			u.Why = "no change"
		}
		out = append(out, u)
	}
	for i, r := range l.Headers {
		add(Unused{RuleRef: headerRef(i, &r)}, &l.usage.headers[i])
	}
	for i, r := range l.Body {
		add(Unused{RuleRef: bodyRef(i, &r)}, &l.usage.body[i])
	}
	return out
}

// SameParts reports whether two values are the same once every part a rule
// with parts matches is set aside. Each value is cut into literal pieces and
// matched parts (rule by rule, a later rule only within the literal pieces an
// earlier one left); the literal pieces must be equal, and a matched part
// stands for any text the rule matches.
func SameParts(rules []*BodyRule, base, cur string) bool {
	if len(rules) == 0 {
		return false
	}
	pb, pc := pieces(rules, base), pieces(rules, cur)
	if len(pb) != len(pc) {
		return false
	}
	for i := range pb {
		if pb[i] != pc[i] {
			return false
		}
	}
	return true
}

// piece is a literal text of a value or, when part is set, a part a rule
// matched (its text does not count).
type piece struct {
	text string
	part bool
}

func pieces(rules []*BodyRule, value string) []piece {
	list := []piece{{text: value}}
	for _, r := range rules {
		var next []piece
		for _, p := range list {
			if p.part {
				next = append(next, p)
				continue
			}
			at := 0
			for _, m := range r.part.FindAllStringIndex(p.text, -1) {
				if m[0] > at {
					next = append(next, piece{text: p.text[at:m[0]]})
				}
				next = append(next, piece{part: true})
				at = m[1]
			}
			if at < len(p.text) {
				next = append(next, piece{text: p.text[at:]})
			}
		}
		list = next
	}
	return list
}

// Matches reports whether a header value keeps the rule's format.
func (r *HeaderRule) Matches(value string) bool { return r.re != nil && r.re.MatchString(value) }

// Matches reports whether a value keeps the rule's format; a rule without a
// format (any_length) matches no value.
func (r *BodyRule) Matches(text string) bool { return r.re != nil && r.re.MatchString(text) }

// Marshal writes the list as YAML.
func (l *List) Marshal() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(l); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}
