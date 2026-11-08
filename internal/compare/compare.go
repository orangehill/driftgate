// Package compare finds every difference (drift) between a baseline response
// and a current one.
package compare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// Level says how far a comparison goes; each level includes the previous ones.
type Level int

const (
	LevelStatus Level = iota // status code only
	LevelShape               // + header names, keys, key order, nesting, array lengths
	LevelTypes               // + the type of every value
	LevelValues              // + exact values, header values and body bytes
)

var levelNames = []string{"status", "shape", "types", "values"}

func (l Level) String() string { return levelNames[l] }

// ParseLevel reads a level name.
func ParseLevel(s string) (Level, error) {
	for i, n := range levelNames {
		if n == s {
			return Level(i), nil
		}
	}
	return 0, fmt.Errorf("unknown level %q (status, shape, types, values)", s)
}

// Drift kinds. The names are stable: reports and CI rely on them.
const (
	KindNotCaptured   = "not-captured"
	KindStatus        = "status"
	KindHeaderMissing = "header-missing"
	KindHeaderExtra   = "header-extra"
	KindHeaderValue   = "header-value"
	KindKeyMissing    = "key-missing"
	KindKeyExtra      = "key-extra"
	KindKeyOrder      = "key-order"
	KindArrayLength   = "array-length"
	KindType          = "type"
	KindNumberLiteral = "number-literal"
	KindFormat        = "format"
	KindValue         = "value"
	KindEncoding      = "encoding"
	KindBody          = "body"
	KindSideEffect    = "side-effect"
)

// KindInfo is a drift kind and what it means, in one line.
type KindInfo struct{ Name, Text string }

// kinds lists every drift kind with what it means: the text of driftgate
// help kinds and the Meaning column of docs/reference/drift-kinds.md, which
// the docs test checks against it and the Kind constants.
var kinds = []KindInfo{
	{KindStatus, "the HTTP status changed, or the request got no answer"},
	{KindHeaderMissing, "a header of the baseline is gone"},
	{KindHeaderExtra, "a header is new"},
	{KindHeaderValue, "a header's value changed"},
	{KindKeyMissing, "a JSON key, or an array item paired by key, is gone"},
	{KindKeyExtra, "a JSON key, or an array item paired by key, is new"},
	{KindKeyOrder, "the same keys in another order"},
	{KindArrayLength, "an array has more or fewer items"},
	{KindItemOrder, "the same items of an array in another order"},
	{KindType, "a value's type changed, in JSON or in a spreadsheet cell"},
	{KindNumberLiteral, "the same number written differently, such as 1.0 for 1"},
	{KindValue, "a JSON value changed"},
	{KindFormat, "a value an allowlist rule covers no longer matches its format"},
	{KindEncoding, "the same values in other bytes: layout, escapes, byte order mark, charset"},
	{KindBody, "a body that is not JSON changed, or JSON became non-JSON"},
	{KindElementMissing, "an HTML element is gone"},
	{KindElementExtra, "an HTML element is new"},
	{KindElementOrder, "the same HTML elements in another order"},
	{KindAttributeMissing, "an attribute of an HTML element is gone"},
	{KindAttributeExtra, "an attribute of an HTML element is new"},
	{KindAttributeValue, "an attribute's value changed"},
	{KindText, "the text of an HTML element changed"},
	{KindDoctype, "the document type declaration came, went or changed"},
	{KindCell, "a spreadsheet or CSV cell's value changed"},
	{KindCellFormat, "a cell's number format or style changed"},
	{KindCellFormula, "a cell's formula changed"},
	{KindSheetMissing, "a worksheet is gone"},
	{KindSheetExtra, "a worksheet is new"},
	{KindSheetOrder, "the worksheets are in another order"},
	{KindExit, "a command step ended with another exit code"},
	{KindRepeat, "the statuses of a repeated request changed"},
	{KindSideEffect, "the request changed other data than in the baseline"},
	{KindNotCaptured, "the baseline has a response, the capture has none"},
}

// Kinds returns a copy of the list of drift kinds.
func Kinds() []KindInfo { return slices.Clone(kinds) }

// Drift is one difference.
type Drift struct {
	Request  string `json:"request"`
	Kind     string `json:"kind"`
	Where    string `json:"where"`
	Baseline string `json:"baseline,omitempty"`
	Current  string `json:"current,omitempty"`
	// Digest names the drift with its values and witness, for the
	// expectations of a baseline (driftgate expect); Expected is the ref
	// of an expectation in force that names it.
	Digest   string `json:"digest,omitempty"`
	Expected string `json:"expected,omitempty"`
	// Texts holds a changed scalar or single header value as the allowlist
	// sees it (strings without quotes), for proposing a format.
	Texts [2]string `json:"-"`
	// Values holds the values of a changed header (baseline, current); of
	// a format drift of a header, only when their number changed.
	Values [2][]string `json:"-"`
	// Witness is the SHA-256 of what the description leaves out: the
	// source of a key, item, element or worksheet only one side has, the
	// items past the shorter array, both values of a type change, both
	// bodies of a drift that only names their sizes, both full texts of
	// an excerpt. An expectation digests it with the description, so a
	// drift is expected only with the same content. Empty when the
	// description says everything.
	Witness string `json:"-"`
	// Material is what an expectation digests (DigestText) of a drift that
	// names an origin of either side (or an alias): written with one
	// neutral origin, so that the drift reads the same captured from
	// another address (a worktree on another port); empty otherwise.
	Material string `json:"-"`
	// parted: rules with parts judge the values; what is digested of them
	// is the witness (their pieces), not the values as they are.
	parted bool
	// named: what is digested in place of the values of a key or item
	// only one side has, whose description shows its value (memberText):
	// its name. The witness digests its content, with what the allowlist
	// lets change in it written neutrally; the value as shown would not.
	named *[2]string
}

// witness sets the witness of the drift added last, from parts with the
// origins written neutrally.
func (c *comparer) witness(parts ...[]byte) {
	if c.own != nil {
		for i, p := range parts {
			parts[i] = c.own.replace(p)
		}
	}
	c.drifts[len(c.drifts)-1].Witness = witness(parts...)
}

// digestAs sets what expectations digest in place of the values of the
// drift added last (Drift.named).
func (c *comparer) digestAs(base, cur string) {
	if c.witnessing {
		c.drifts[len(c.drifts)-1].named = &[2]string{base, cur}
	}
}

// sources writes the members of an object with a key, for a witness.
func (c *comparer) sources(path string, o *jsonx.Value, key string) []byte {
	var list [][]byte
	for _, m := range o.Members {
		if m.Key == key {
			list = append(list, c.source(jsonx.KeyPath(path, m.Key), m.Value))
		}
	}
	e := c.enc()
	e.list(len(list))
	for _, b := range list {
		e.bytes(b)
	}
	return e.b
}

// material sets, when witnessing, what expectations digest of a drift that names an own
// origin: the same with the origins written neutrally. Of every other
// drift it is its fields as they are (Drift.DigestText), which costs
// nothing to keep.
func (c *comparer) material() {
	if !c.witnessing {
		return
	}
	for i, d := range c.drifts {
		if c.own == nil && c.level >= LevelValues && !d.parted && d.named == nil {
			continue
		}
		where, base, cur := d.Where, d.Baseline, d.Current
		if d.named != nil {
			base, cur = d.named[0], d.named[1]
		}
		if c.own != nil {
			where, base, cur = c.own.text(where), c.own.text(base), c.own.text(cur)
		}
		if d.parted {
			base, cur = "", ""
		}
		if c.level < LevelValues && !structural[d.Kind] {
			// The values the drift shows are not compared at this level
			// (a new attribute's value at types): only that it is there.
			base, cur = presence(base), presence(cur)
		}
		if c.level < LevelTypes && d.Kind == KindType {
			base, cur = shapeKind(base), shapeKind(cur) // a scalar's kind is no shape
		}
		if where != d.Where || base != d.Baseline || cur != d.Current || d.named != nil {
			c.drifts[i].Material = digestText(d.Kind, where, base, cur, d.Witness)
		}
	}
}

// structural are the kinds whose values the comparison checks below the
// values level too: a status, types, lengths, keys, orders, names.
var structural = map[string]bool{
	KindNotCaptured: true, KindStatus: true, KindType: true, KindArrayLength: true, KindKeyOrder: true, KindItemOrder: true,
	KindKeyMissing: true, KindKeyExtra: true, KindElementMissing: true, KindElementExtra: true, KindElementOrder: true,
	KindSheetMissing: true, KindSheetExtra: true, KindSheetOrder: true, KindExit: true, KindRepeat: true, KindBody: true,
}

func shapeKind(k string) string {
	if k == "object" || k == "array" {
		return k
	}
	return "scalar"
}

// presence is a value below the values level: whether there is one.
func presence(v string) string {
	if v == "" {
		return ""
	}
	return "\x00present"
}

// DigestText is what an expectation digests of a drift: its kind, place,
// both values and its witness, with own origins written neutrally.
func (d Drift) DigestText() string {
	if d.Material != "" {
		return d.Material
	}
	return digestText(d.Kind, d.Where, d.Baseline, d.Current, d.Witness)
}

// indexRE is an array index in a path, or the key of an item of an array
// paired by key ([id=42], [slug="a-b"], ["@id"=42]: keyName).
var indexRE = lazy.Regexp(`\[(?:\d+|(?:[\w.$-]+|"(?:[^"\\]|\\.)*")=(?:-?\d+|"(?:[^"\\]|\\.)*"))\]`)

// AnyItem writes every array index and item key of a path as [*].
func AnyItem(path string) string { return indexRE().ReplaceAllString(path, "[*]") }

// Signature groups drifts that come from the same change, for example one
// date format change across hundreds of list items.
func (d Drift) Signature() string {
	sig := d.Kind + " " + AnyItem(d.Where)
	if d.Kind == KindCell || d.Kind == KindCellFormat || d.Kind == KindCellFormula || d.Kind == KindType && cellRow().MatchString(d.Where) {
		sig = d.Kind + " " + cellRow().ReplaceAllString(d.Where, "!$1*") // one column of an export: B2, B3… → B*
	}
	if d.Kind == KindType {
		sig += " " + d.Baseline + "→" + d.Current
	}
	return sig
}

// Side is one response with its body.
type Side struct {
	Record *snapshot.Record
	Body   []byte
	// NoBody marks a side whose body is not available (a private body not
	// on this machine): everything but the body is compared.
	NoBody bool
	// Origins are the side's own origins (scheme://host[:port]): the one it
	// was captured from and its aliases. When the two sides have different
	// ones, all of them are taken as the same origin (origin.go).
	Origins []string
	// Roots are the folders of the checkout the side was captured in (the
	// project folder and its real path): when the two sides have
	// different ones, each is taken as the same folder (roots.go).
	Roots []string
	// RowTables are the items whose rows the side's capture tracked
	// (row_tracking); nil when it does not say (a set of an earlier
	// release). Rows of an item the other side did not track are not
	// compared: a note says so.
	RowTables []string
	// Folders are the forms of the two sides' Roots, made once for many
	// requests (NewFolders, the same on both sides); nil: made from Roots.
	Folders *Folders
}

// FastPaths turns on the shortcuts that only save work: rules narrowed to
// the request, identical parts of two bodies passed over and objects with
// the same keys paired by position. Tests turn it off to check that the
// result stays the same.
var FastPaths = true

// One compares a current response with its baseline.
// The rules that applied to a change count into the allowlist's Usage
// (allowlist.List.Track).
func One(id string, base, cur Side, allow *allowlist.List, level Level) []Drift {
	drifts, _ := OneNotes(id, base, cur, allow, level)
	return drifts
}

// OneNotes is One with the notes on how the comparison went where that is
// not a drift: an array the allowlist pairs by key whose items had to be
// paired by position.
func OneNotes(id string, base, cur Side, allow *allowlist.List, level Level) ([]Drift, []string) {
	return oneNotes(id, base, cur, allow, level, false)
}

// OneWitnessed is OneNotes that also writes what expectations digest of
// each drift (Drift.Witness, Drift.Material, DigestText): for a
// comparison with a baseline, whose expectations may apply.
func OneWitnessed(id string, base, cur Side, allow *allowlist.List, level Level) ([]Drift, []string) {
	return oneNotes(id, base, cur, allow, level, true)
}

func oneNotes(id string, base, cur Side, allow *allowlist.List, level Level, witnessing bool) ([]Drift, []string) {
	// The request's own list: it counts the rules that applied.
	if FastPaths {
		allow = allow.ForRequest(id)
	} else {
		allow = allow.Copy()
	}
	c := &comparer{id: id, allow: allow, level: level, witnessing: witnessing}
	if witnessing {
		c.own = ownOrigins(base.Origins, cur.Origins)
	}
	if cur.Record == nil {
		c.add(KindNotCaptured, "", "", "")
		c.material()
		return c.drifts, nil
	}
	r := base.Folders
	if r == nil {
		r = newRoots(base.Roots, cur.Roots)
	}
	if r != nil {
		// Bodies that differ only in the folders are equal now: their
		// Content-Length still differs, which is no drift either. Equal
		// bodies stay equal, both sides' folders being replaced on both:
		// they are not read for them.
		c.rawDiffer = !bytes.Equal(base.Body, cur.Body)
		base, cur = r.side(base, r.base, c.rawDiffer), r.side(cur, r.cur, c.rawDiffer)
	}
	if c.origins = newOrigins(base.Origins, cur.Origins); c.origins != nil {
		c.compare(base, cur)
		c.origins.restore(c.drifts)
	} else {
		c.compare(base, cur)
	}
	c.foldEncodings()
	c.material()
	failed := base.Record.Error != "" || cur.Record.Error != ""
	allow.Done(id, level >= LevelValues && !failed, level >= LevelShape && !failed && !base.NoBody && !cur.NoBody)
	return c.drifts, c.notes
}

func (c *comparer) compare(base, cur Side) {
	if base.Record.Error != "" || cur.Record.Error != "" {
		// A failed request has no status or body to compare.
		b, v := outcome(base.Record), outcome(cur.Record)
		if c.origins != nil {
			b, v = c.origins.base.text(b), c.origins.cur.text(v)
		}
		if b != v {
			c.add(KindStatus, "", b, v)
		}
		return
	}
	if base.Record.Status != cur.Record.Status {
		c.add(KindStatus, "", strconv.Itoa(base.Record.Status), strconv.Itoa(cur.Record.Status))
	}
	c.steps(base.Record, cur.Record)
	if c.level >= LevelShape {
		bh, ch := base.Record.Headers, cur.Record.Headers
		if c.origins != nil {
			bh, ch = c.origins.base.headers(bh), c.origins.cur.headers(ch)
		}
		c.headers(bh, ch)
		// A different body has a different length: Content-Length then says
		// nothing new, and the body itself is compared below.
		noBody := base.NoBody || cur.NoBody
		if !noBody && (c.rawDiffer || !bytes.Equal(base.Body, cur.Body)) {
			kept := c.drifts[:0]
			for _, d := range c.drifts {
				if !(d.Kind == KindHeaderValue && d.Where == "header:content-length") {
					kept = append(kept, d)
				}
			}
			c.drifts = kept
		}
		if !noBody && !c.rawAllowed(base, cur) {
			if c.origins != nil && !bytes.Equal(base.Body, cur.Body) {
				// New bodies: the read ones may be shared, and nothing
				// replaced is ever stored.
				base.Body, cur.Body = c.origins.base.body(base), c.origins.cur.body(cur)
			}
			c.body(base, cur)
		}
		// Side effects are compared only when both captures tracked the state.
		if base.Record.SideEffects != nil && cur.Record.SideEffects != nil {
			c.sideEffects(base.Record.SideEffects, cur.Record.SideEffects, base.RowTables, cur.RowTables)
		}
	}
}

// rawAllowed reports whether a whole-body rule (allowlist.RawPath) lets the
// current body differ: the bytes differ, neither body is JSON and the
// current body as text matches the rule's format. Otherwise the body is
// compared as usual, so a body the format does not describe still shows its
// drifts. A JSON body on either side is never covered: two JSON bodies are
// left to the rules for their values, and a JSON body that turned into a
// page (an error page) or back is a body drift. JSON is told by a scan that
// builds no tree. The rule sees the bodies as captured, before origins are
// replaced: its format is written from the body as it is served.
func (c *comparer) rawAllowed(base, cur Side) bool {
	if !c.allow.HasRawRule(c.id) || bytes.Equal(base.Body, cur.Body) || jsonx.Valid(base.Body) || jsonx.Valid(cur.Body) {
		return false
	}
	return c.allow.RawRule(c.id).Matches(string(cur.Body))
}

// sideEffects compares what two requests changed in the state: the changed
// items, and for tracked tables the changed rows and columns.
// sideEffects compares the items a request changed and, of the tables
// both sides tracked rows of (bTables, vTables; nil: every table), its
// rows.
func (c *comparer) sideEffects(base, cur *snapshot.SideEffects, bTables, vTables []string) {
	b, v := map[string]bool{}, map[string]bool{}
	for _, it := range base.Items {
		b[it] = true
	}
	for _, it := range cur.Items {
		v[it] = true
	}
	for _, it := range base.Items {
		if !v[it] {
			c.add(KindSideEffect, "side-effects:"+it, "changed", "unchanged")
		}
	}
	for _, it := range cur.Items {
		if !b[it] {
			c.add(KindSideEffect, "side-effects:"+it, "unchanged", "changed")
		}
	}
	describe := func(r snapshot.RowChange) string {
		if len(r.Columns) > 0 {
			return r.Change + " " + strings.Join(r.Columns, ",")
		}
		return r.Change
	}
	// untracked notes, once per item, the rows of a table the other side
	// did not track: row_tracking came or went after the baseline.
	noted := map[string]bool{}
	untracked := func(item string, other []string, why string) bool {
		if other == nil || tracks(other, item) {
			return false
		}
		if !noted[item] {
			noted[item] = true
			c.notes = append(c.notes, fmt.Sprintf("side-effects:%s: rows not compared: %s; make the baseline again to compare them (accept keeps the baseline's tracking)", item, why))
		}
		return true
	}
	rows := func(list []snapshot.RowChange, other []string, why string) (map[string]string, []string) {
		m := map[string]string{}
		var keys []string
		for _, r := range list {
			if untracked(r.Item, other, why) {
				continue
			}
			k := r.Item + "[" + r.Key + "]"
			if _, ok := m[k]; !ok {
				keys = append(keys, k)
			}
			m[k] = describe(r)
		}
		return m, keys
	}
	bRows, bKeys := rows(base.Rows, vTables, "row_tracking no longer covers the table")
	vRows, vKeys := rows(cur.Rows, bTables, "not tracked in the baseline (row_tracking came after it)")
	for _, k := range bKeys {
		if vRows[k] != bRows[k] {
			c.add(KindSideEffect, "side-effects:"+k, bRows[k], orNone(vRows[k]))
		}
	}
	for _, k := range vKeys {
		if _, ok := bRows[k]; !ok {
			c.add(KindSideEffect, "side-effects:"+k, "unchanged", vRows[k])
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "unchanged"
	}
	return s
}

func outcome(r *snapshot.Record) string {
	if r.Error != "" {
		return "error: " + r.Error
	}
	return strconv.Itoa(r.Status)
}

type comparer struct {
	id      string
	allow   *allowlist.List
	level   Level
	drifts  []Drift
	notes   []string // how the comparison went where that is not a drift
	allowed int      // allow-listed values that changed
	// rewritten counts values a rule covers that are written otherwise.
	rewritten int
	lengths   int      // arrays the allowlist let change their length or order
	origins   *origins // nil: the sides share their origins
	// witnessing: write what expectations digest of the drifts (Witness,
	// Material); a comparison no expectation applies to costs nothing
	// more for it (OneWitnessed).
	witnessing bool
	shapes     map[*element]string // shape hashes of elements (shapeSum), below the values level
	moved      [][2]int            // runs of the items of arrays with drifts by position (items), a stack
	orders     []order             // orders of items a rule allowed, in the document being compared
	// docs are the body documents whose bytes differ, for what they write
	// beyond their values (foldEncodings).
	docs []encDoc
	// devalue: the JSON being compared is a devalue payload, compared
	// decoded.
	devalue bool
	// own writes the origins of both sides as one neutral origin, for what
	// expectations digest (also when the sides share their origins).
	own *originSide
	// rawDiffer: the bodies differed before the project folders were
	// replaced (roots.go).
	rawDiffer bool
}

// memberTextMax bounds the text of a key's value in a key-missing or
// key-extra drift.
const memberTextMax = 200

// memberText is the value of a key that is missing or new, as JSON (the
// key itself is in the drift's place): an object or array cut to
// memberTextMax characters.
func memberText(v *jsonx.Value) string {
	if v.Kind != jsonx.Object && v.Kind != jsonx.Array {
		return v.ScalarText()
	}
	t := string(jsonx.Marshal(v, false))
	if r := []rune(t); len(r) > memberTextMax {
		t = string(r[:memberTextMax]) + "…"
	}
	return t
}

func (c *comparer) add(kind, where, base, cur string) {
	c.drifts = append(c.drifts, Drift{Request: c.id, Kind: kind, Where: where, Baseline: base, Current: cur})
}

func group(headers [][2]string) (names []string, values map[string][]string) {
	values = map[string][]string{}
	for _, h := range headers {
		n := strings.ToLower(h[0])
		if _, ok := values[n]; !ok {
			names = append(names, n)
		}
		values[n] = append(values[n], h[1])
	}
	return names, values
}

// headerWitness makes the drift added last digest a header's values as
// the comparison sees them: in any order, a value the header's rule
// accepts as the rule, in any letter case under an ignore_case rule;
// below the values level not at all (only whether the header is there).
func (c *comparer) headerWitness(name string, b, v []string) {
	rule := c.allow.HeaderFor(c.id, name)
	side := func(vs []string) []byte {
		if c.level < LevelValues {
			return nil
		}
		out := make([]string, len(vs))
		for i, x := range vs {
			x = c.neutral(x)
			switch {
			case rule != nil && rule.IgnoreCase:
				out[i] = "\x00lower " + strings.ToLower(x)
			case rule != nil && rule.Matches(x):
				out[i] = "\x00rule"
			default:
				out[i] = "\x00exact " + x
			}
		}
		slices.Sort(out)
		return c.textsSource(out)
	}
	if c.witnessing {
		c.witness(side(b), side(v))
	}
	c.drifts[len(c.drifts)-1].parted = true
}

func listText(vs []string) string {
	b, _ := json.Marshal(vs)
	return string(b)
}

func (c *comparer) headers(base, cur [][2]string) {
	bn, bv := group(base)
	cn, cv := group(cur)
	all := map[string]bool{}
	for _, n := range append(bn, cn...) {
		all[n] = true
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b, inBase := bv[n]
		v, inCur := cv[n]
		where := "header:" + n
		switch {
		case !inCur:
			c.add(KindHeaderMissing, where, listText(b), "")
			if c.witnessing {
				c.headerWitness(n, b, v)
			}
			continue
		case !inBase:
			c.add(KindHeaderExtra, where, "", listText(v))
			if c.witnessing {
				c.headerWitness(n, b, v)
			}
			continue
		case c.level < LevelValues:
			continue
		}
		sb, sc := append([]string{}, b...), append([]string{}, v...)
		sort.Strings(sb)
		sort.Strings(sc)
		if slices.Equal(sb, sc) {
			continue // an unchanged value is never a drift, rule or not
		}
		rule := c.allow.HeaderFor(c.id, n)
		if rule != nil && !rule.IgnoreCase {
			ok := len(b) == len(v)
			for _, x := range v {
				ok = ok && rule.Matches(x)
			}
			if !ok {
				c.add(KindFormat, where, listText(b), listText(v))
				if len(b) != len(v) {
					// A value came or went (a cookie no longer set): the
					// explanation says so rather than call it a format.
					c.drifts[len(c.drifts)-1].Values = [2][]string{b, v}
				}
				if c.witnessing {
					c.headerWitness(n, b, v)
				}
			}
			continue
		}
		if rule != nil { // IgnoreCase: the same values in any letter case
			for i := range sb {
				sb[i] = strings.ToLower(sb[i])
			}
			for i := range sc {
				sc[i] = strings.ToLower(sc[i])
			}
			sort.Strings(sb)
			sort.Strings(sc)
			if slices.Equal(sb, sc) {
				continue
			}
		}
		c.add(KindHeaderValue, where, listText(b), listText(v))
		if c.witnessing {
			c.headerWitness(n, b, v)
		}
		c.drifts[len(c.drifts)-1].Values = [2][]string{b, v}
		if len(b) == 1 && len(v) == 1 {
			c.drifts[len(c.drifts)-1].Texts = [2]string{b[0], v[0]}
		}
	}
}

// body compares the raw bodies; both are always at hand, private ones too
// (decrypted from the local store or the baseline).
func (c *comparer) body(base, cur Side) {
	if bytes.Equal(base.Body, cur.Body) {
		return
	}
	if isHTML(base.Record) && isHTML(cur.Record) {
		c.htmlBody(base.Body, cur.Body)
		return
	}
	if c.sheets(base, cur) {
		return
	}
	jb, errB, jc, errC := parseBoth(base.Body, cur.Body)
	if (errB != nil || errC != nil) && c.lines(base, cur) {
		return
	}
	if errB != nil || errC != nil {
		c.add(KindBody, "", fmt.Sprintf("%d bytes", len(base.Body)), fmt.Sprintf("%d bytes", len(cur.Body)))
		if c.witnessing {
			c.witness(base.Body, cur.Body)
		}
		return
	}
	c.json("$", base.Body, cur.Body, jb, jc)
}

// parseBoth parses two bodies; large ones at the same time, as a large
// pair is compared while the smaller ones wait (cli.parallel).
func parseBoth(b, c []byte) (jb *jsonx.Value, errB error, jc *jsonx.Value, errC error) {
	if len(b)+len(c) < 1<<20 {
		jb, errB = jsonx.Parse(b)
		jc, errC = jsonx.Parse(c)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		jb, errB = jsonx.Parse(b)
	}()
	jc, errC = jsonx.Parse(c)
	<-done
	return
}

// json compares two JSON documents whose root has the path root: a body ($)
// or JSON inside an HTML page (html>body>script#__NEXT_DATA__$).
func (c *comparer) json(root string, rawB, rawC []byte, jb, jc *jsonx.Value) {
	before, allowed, rewritten, orders := len(c.drifts), c.allowed+c.lengths, c.rewritten, len(c.orders)
	c.walk(root, jb, jc)
	if c.level < LevelValues {
		c.orders = c.orders[:orders]
		return
	}
	c.document(false, root, rawB, rawC)
	// Same values written differently: the layout (compact or
	// pretty-printed, the indentation, line ends) changed, or nothing else
	// explains the byte difference (escaping of keys, other whitespace):
	// no drift, and no value or array length the allowlist let change.
	// Two documents of one encoder have the same layout however deep they
	// nest. A byte order mark is a drift whatever else changed.
	// A document in UTF-16 or UTF-32 is read as its text, and its
	// encoding is compared as a byte order mark is: whatever else changed.
	tb, eb := textOf(rawB)
	tc, ec := textOf(rawC)
	bb, ba := edges(tb)
	cb, ca := edges(tc)
	if eb != ec {
		c.add(KindEncoding, root, encodingText(eb), encodingText(ec))
		if c.witnessing {
			c.witness([]byte(eb), []byte(ec))
		}
	} else if mb, mc := marks(bb, ba), marks(cb, ca); mb != mc {
		c.add(KindEncoding, root, edgesText(bb, ba), edgesText(cb, ca))
		if c.witnessing {
			c.witness([]byte(mb), []byte(mc))
		}
	}
	if sb, sc := jsonx.StyleOf(tb), jsonx.StyleOf(tc); sb != sc {
		c.add(KindEncoding, root, "layout", "layout")
		if c.witnessing {
			c.witness(styleSource(sb), styleSource(sc))
		}
	} else if len(c.drifts) == before {
		// Only orders a rule allowed: the document with those items in the
		// baseline's order is written as the baseline's, or that differs.
		// A document that cannot be written so is compared as it is: a
		// drift is then reported that may be the order alone, never an
		// order that hides another change.
		base, cur, plain := rawB, rawC, c.allowed+c.lengths == allowed
		if n := len(c.orders) - orders; !plain && n > 0 && c.allowed+c.lengths-allowed == n {
			plain = true
			if u, ok := unorder(tb, tc, jb, jc, c.orders[orders:]); ok {
				base, cur, plain = tb, u, !bytes.Equal(tb, u)
			}
		}
		// Values a rule covers written otherwise are no drift; the rest
		// of the document is compared without them.
		if plain && (c.rewritten == rewritten || c.beyondRules(root, base, cur)) {
			c.add(KindEncoding, root, fmt.Sprintf("%d bytes", len(rawB)), fmt.Sprintf("%d bytes", len(rawC)))
			if c.witnessing {
				c.witness(rawB, rawC)
			}
		}
	}
	c.orders = c.orders[:orders]
}

// textOf is a JSON document as the comparison reads its layout: a
// document in UTF-8 as it is, one in UTF-16 or UTF-32 as its text, with
// its encoding (jsonx.Decode); "" for UTF-8.
func textOf(raw []byte) ([]byte, string) {
	if plainJSON(raw) {
		return raw, ""
	}
	if enc, text, ok := decoded(raw); ok {
		return text, enc
	}
	return raw, ""
}

// encodingText names an encoding of a document in a drift.
func encodingText(enc string) string {
	if enc == "" {
		return "UTF-8"
	}
	if name, ok := strings.CutSuffix(enc, "-bom"); ok {
		return strings.ToUpper(name) + " with a byte order mark"
	}
	return strings.ToUpper(enc)
}

// beyondRules reports whether two JSON documents differ apart from how
// the values a rule covers are written.
func (c *comparer) beyondRules(root string, b, v []byte) bool {
	if c.devalue {
		// A payload's values are read at the paths of the value it
		// decodes to, not of its JSON: its bytes are compared as they are.
		return true
	}
	if !plainJSON(b) || !plainJSON(v) {
		// UTF-16 or UTF-32: their texts in one encoding.
		eb, tb, okB := decoded(b)
		ev, tv, okV := decoded(v)
		if !okB || !okV || eb != ev {
			return true
		}
		b, v = tb, tv
	}
	// The document between what stands around it (a byte order mark,
	// whitespace), which is compared as it is.
	bb, ba := edges(b)
	vb, va := edges(v)
	if !bytes.Equal(bb, vb) || !bytes.Equal(ba, va) {
		return true
	}
	r := c.ruled(root)
	return !bytes.Equal(r.masked(b[len(bb):len(b)-len(ba)]), r.masked(v[len(vb):len(v)-len(va)]))
}

// marks are what stands around a JSON document that is not whitespace (a
// byte order mark).
func marks(before, after []byte) string {
	trim := func(b []byte) []byte { return bytes.Trim(b, " \t\r\n") }
	return string((&canon{}).bytes(trim(before)).bytes(trim(after)).b)
}

// edges are the bytes of a JSON document before and after its value.
func edges(raw []byte) (before, after []byte) {
	start := 0
	for start < len(raw) && !strings.ContainsRune("{[\"-0123456789tfnNI", rune(raw[start])) {
		start++
	}
	end := len(raw)
	for end > start && strings.ContainsRune(" \t\r\n", rune(raw[end-1])) {
		end--
	}
	return raw[:start], raw[end:]
}

// edgesText says what stands before and after a document.
func edgesText(before, after []byte) string {
	var parts []string
	if rest, ok := bytes.CutPrefix(before, []byte("\xef\xbb\xbf")); ok {
		parts = append(parts, "byte order mark")
		before = rest
	}
	if len(before) > 0 {
		parts = append(parts, fmt.Sprintf("%d bytes before", len(before)))
	}
	switch string(after) {
	case "":
	case "\n":
		parts = append(parts, "final line end")
	case "\r\n":
		parts = append(parts, "final CRLF line end")
	default:
		parts = append(parts, fmt.Sprintf("%d bytes after", len(after)))
	}
	if len(parts) == 0 {
		return "nothing before or after the value"
	}
	return strings.Join(parts, ", ")
}

func isContainer(k jsonx.Kind) bool { return k == jsonx.Object || k == jsonx.Array }

func (c *comparer) walk(path string, b, v *jsonx.Value) {
	if b.Kind != v.Kind {
		// Below LevelTypes only a change of structure counts.
		if c.level >= LevelTypes || isContainer(b.Kind) || isContainer(v.Kind) {
			c.add(KindType, path, b.Kind.String(), v.Kind.String())
			if c.witnessing {
				c.witness(c.source(path, b), c.source(path, v))
			}
		}
		return
	}
	// The same source text holds the same values all the way down, and no
	// drift can come from it: rules judge only values that changed. Values
	// a payload decoder made have no source and are always walked.
	if FastPaths && isContainer(b.Kind) && b.Src != nil && v.Src != nil && bytes.Equal(b.Src, v.Src) {
		return
	}
	switch b.Kind {
	case jsonx.Object:
		c.object(path, b, v)
	case jsonx.Array:
		if r := c.allow.ArrayKey(c.id, path); r != nil && r.Key != "" && c.keyed(path, r, b, v) {
			return
		}
		if len(b.Items) == len(v.Items) {
			c.items(path, b, v)
			return
		}
		if c.allow.AnyLength(c.id, path) {
			c.lengths++
		} else {
			c.add(KindArrayLength, path, strconv.Itoa(len(b.Items)), strconv.Itoa(len(v.Items)))
			// The items past the shorter array are not walked.
			n := min(len(b.Items), len(v.Items))
			tail := func(items []*jsonx.Value) []byte {
				e := c.enc()
				e.list(len(items) - n)
				for i := n; i < len(items); i++ {
					e.bytes(c.source(path+"["+strconv.Itoa(i)+"]", items[i]))
				}
				return e.b
			}
			if c.witnessing {
				c.witness(tail(b.Items), tail(v.Items))
			}
		}
		for i := 0; i < len(b.Items) && i < len(v.Items); i++ {
			c.walk(path+"["+strconv.Itoa(i)+"]", b.Items[i], v.Items[i])
		}
	default:
		if c.level < LevelValues {
			return
		}
		c.scalar(path, b, v)
	}
}

// scalar compares two scalars of one kind. A rule judges only a value that
// changed: an unchanged value is the baseline's, never a drift, whatever
// else in the body changed. A rule with a whole-value format decides alone;
// rules with parts let only their parts change. A rule without a format
// (any_length) is about the length of arrays and says nothing here.
func (c *comparer) scalar(path string, b, v *jsonx.Value) {
	same := b.Equal(v)
	if same && (b.Kind != jsonx.String || b.Raw == v.Raw) {
		return
	}
	rule, parts := c.allow.ValueRules(c.id, path)
	switch {
	case same && (rule != nil || len(parts) > 0):
		// Written differently: a rule covers how its value is written
		// too, as it lets the value change (json compares the rest).
		c.rewritten++
	case rule != nil:
		if !rule.Matches(v.FormatText()) {
			c.add(KindFormat, path, b.ScalarText(), v.ScalarText())
		} else {
			c.allowed++
		}
	case same:
		c.add(KindEncoding, path, b.Raw, v.Raw)
	case allowlist.SameParts(parts, b.FormatText(), v.FormatText()):
		c.allowed++
	case sameNumber(b, v):
		c.add(KindNumberLiteral, path, b.ScalarText(), v.ScalarText())
		if c.witnessing {
			c.parted(path, b.FormatText(), v.FormatText(), len(parts) > 0, false)
		}
	default:
		c.add(KindValue, path, b.ScalarText(), v.ScalarText())
		c.drifts[len(c.drifts)-1].Texts = [2]string{b.FormatText(), v.FormatText()}
		if c.witnessing {
			c.parted(path, b.FormatText(), v.FormatText(), len(parts) > 0, false)
		}
	}
}

// parted makes the drift added last digest two values by the pieces
// rules with parts leave of them (what the comparison judges), when such
// rules apply.
func (c *comparer) parted(path, b, v string, parts, cells bool) {
	if !parts {
		return
	}
	nb, okB := c.normal(path, b, cells)
	nv, okV := c.normal(path, v, cells)
	e := c.enc()
	e.value(nb, okB, b).value(nv, okV, v)
	d := &c.drifts[len(c.drifts)-1]
	d.Witness, d.parted = e.sum(), true
}

// object reports missing and extra keys and a change in the order of the keys
// both sides share, then compares the shared keys. Duplicate keys are paired
// by occurrence: the second "a" in the baseline with the second "a" now.
func (c *comparer) object(path string, b, v *jsonx.Value) {
	if FastPaths && sameKeys(b, v) {
		// Nothing is missing, extra or moved, and the n-th occurrence of a
		// key is at the same position on both sides.
		for i, m := range b.Members {
			before := len(c.drifts)
			c.walk(jsonx.KeyPath(path, m.Key), m.Value, v.Members[i].Value)
			if len(c.drifts) > before {
				if c.witnessing {
					c.occurrence(b, m.Key, i, before)
				}
			}
		}
		return
	}
	count := func(o *jsonx.Value) map[string]int {
		m := map[string]int{}
		for _, mem := range o.Members {
			m[mem.Key]++
		}
		return m
	}
	nb, nc := count(b), count(v)
	seen := map[string]bool{}
	for _, m := range b.Members {
		if !seen[m.Key] && nc[m.Key] < nb[m.Key] {
			c.add(KindKeyMissing, jsonx.KeyPath(path, m.Key), memberText(m.Value), "")
			c.digestAs(jsonx.Quote(m.Key), "")
			if c.witnessing {
				c.witness(c.sources(path, b, m.Key), c.sources(path, v, m.Key))
			}
		}
		seen[m.Key] = true
	}
	seen = map[string]bool{}
	for _, m := range v.Members {
		if !seen[m.Key] && nb[m.Key] < nc[m.Key] {
			c.add(KindKeyExtra, jsonx.KeyPath(path, m.Key), "", memberText(m.Value))
			c.digestAs("", jsonx.Quote(m.Key))
			// Where the new key stands: the keys of the object in order.
			if c.witnessing {
				c.witness(c.sources(path, b, m.Key), c.sources(path, v, m.Key), c.textsSource(v.Keys()))
			}
		}
		seen[m.Key] = true
	}
	shared := func(o *jsonx.Value, other map[string]int) []string {
		used := map[string]int{}
		var keys []string
		for _, m := range o.Members {
			if used[m.Key] < other[m.Key] {
				keys = append(keys, m.Key)
				used[m.Key]++
			}
		}
		return keys
	}
	kb, kc := shared(b, nc), shared(v, nb)
	if strings.Join(kb, "\x00") != strings.Join(kc, "\x00") {
		c.add(KindKeyOrder, path, listText(b.Keys()), listText(v.Keys()))
	}
	// Pair the n-th occurrence of every shared key.
	occurrences := func(o *jsonx.Value) map[string][]*jsonx.Value {
		m := map[string][]*jsonx.Value{}
		for _, mem := range o.Members {
			m[mem.Key] = append(m[mem.Key], mem.Value)
		}
		return m
	}
	ob, oc := occurrences(b), occurrences(v)
	idx := map[string]int{}
	for _, k := range kb {
		i := idx[k]
		idx[k]++
		before := len(c.drifts)
		c.walk(jsonx.KeyPath(path, k), ob[k][i], oc[k][i])
		if len(c.drifts) > before && len(ob[k]) > 1 {
			if c.witnessing {
				c.mark(before, i)
			}
		}
	}
}

// occurrence marks the drifts from before on as of the n-th occurrence of
// a key that an object has more than once: they share one path.
func (c *comparer) occurrence(o *jsonx.Value, key string, at, before int) {
	n, total := 0, 0
	for i, m := range o.Members {
		if m.Key == key {
			if i < at {
				n++
			}
			total++
		}
	}
	if total > 1 {
		if c.witnessing {
			c.mark(before, n)
		}
	}
}

// mark adds to the witness of the drifts from before on which occurrence
// of a repeated key they are of.
func (c *comparer) mark(before, n int) {
	for i := before; i < len(c.drifts); i++ {
		e := c.enc()
		e.text(c.drifts[i].Witness).int(n)
		c.drifts[i].Witness = e.sum()
	}
}

// sameKeys reports two objects with the same keys in the same order.
func sameKeys(b, v *jsonx.Value) bool {
	if len(b.Members) != len(v.Members) {
		return false
	}
	for i := range b.Members {
		if b.Members[i].Key != v.Members[i].Key {
			return false
		}
	}
	return true
}

// sameNumber reports two literals of the same numeric kind with the same value
// written differently (1.0 and 1.00, 1e2 and 100.0).
func sameNumber(a, b *jsonx.Value) bool {
	if a.Kind != b.Kind || (a.Kind != jsonx.Int && a.Kind != jsonx.Float) {
		return false
	}
	x, ok1 := new(big.Float).SetPrec(4096).SetString(a.Text)
	y, ok2 := new(big.Float).SetPrec(4096).SetString(b.Text)
	return ok1 && ok2 && x.Cmp(y) == 0
}

// tracks reports whether the tables another side tracked (meta.RowTracking:
// <component>/table:<name>, from the configuration) hold item, the table
// a row change names as the driver read it. The drivers match a table
// more loosely than its configured name: SQL Server and others without
// letter case, PostgreSQL a name without a schema in any schema. So does
// this, without letter case and by the name without its schema too: a
// table taken for tracked keeps its rows compared, and a looser match can
// only show a difference, never hide one.
func tracks(other []string, item string) bool {
	comp, name, ok := strings.Cut(item, "/table:")
	if !ok {
		return slices.Contains(other, item)
	}
	bare := func(n string) string {
		if i := strings.LastIndexByte(n, '.'); i >= 0 {
			return n[i+1:]
		}
		return n
	}
	for _, o := range other {
		oc, on, ok := strings.Cut(o, "/table:")
		if !ok || oc != comp {
			continue
		}
		if strings.EqualFold(on, name) || strings.EqualFold(bare(on), bare(name)) {
			return true
		}
	}
	return false
}
