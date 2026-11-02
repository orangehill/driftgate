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

// Drift is one difference.
type Drift struct {
	Request  string `json:"request"`
	Kind     string `json:"kind"`
	Where    string `json:"where"`
	Baseline string `json:"baseline,omitempty"`
	Current  string `json:"current,omitempty"`
	// Texts holds a changed scalar or single header value as the allowlist
	// sees it (strings without quotes), for proposing a format.
	Texts [2]string `json:"-"`
	// Values holds the values of a changed header (baseline, current).
	Values [2][]string `json:"-"`
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
	// The request's own list: it counts the rules that applied.
	if FastPaths {
		allow = allow.ForRequest(id)
	} else {
		allow = allow.Copy()
	}
	c := &comparer{id: id, allow: allow, level: level}
	if cur.Record == nil {
		c.add(KindNotCaptured, "", "", "")
		return c.drifts, nil
	}
	if c.origins = newOrigins(base.Origins, cur.Origins); c.origins != nil {
		c.compare(base, cur)
		c.origins.restore(c.drifts)
	} else {
		c.compare(base, cur)
	}
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
		if !noBody && !bytes.Equal(base.Body, cur.Body) {
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
			c.sideEffects(base.Record.SideEffects, cur.Record.SideEffects)
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
func (c *comparer) sideEffects(base, cur *snapshot.SideEffects) {
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
	rows := func(list []snapshot.RowChange) (map[string]string, []string) {
		m := map[string]string{}
		var keys []string
		for _, r := range list {
			k := r.Item + "[" + r.Key + "]"
			if _, ok := m[k]; !ok {
				keys = append(keys, k)
			}
			m[k] = describe(r)
		}
		return m, keys
	}
	bRows, bKeys := rows(base.Rows)
	vRows, vKeys := rows(cur.Rows)
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
	lengths int      // arrays the allowlist let change their length or order
	origins *origins // nil: the sides share their origins
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
			continue
		case !inBase:
			c.add(KindHeaderExtra, where, "", listText(v))
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
	before, allowed := len(c.drifts), c.allowed+c.lengths
	c.walk(root, jb, jc)
	if c.level < LevelValues {
		return
	}
	// Same values written differently: the layout (compact or
	// pretty-printed, the indentation, line ends) changed, or nothing else
	// explains the byte difference (escaping of keys, a byte order mark):
	// no drift, and no value or array length the allowlist let change.
	// Two documents of one encoder have the same layout however deep they
	// nest.
	if jsonx.StyleOf(rawB) != jsonx.StyleOf(rawC) {
		c.add(KindEncoding, root, "layout", "layout")
	} else if len(c.drifts) == before && c.allowed+c.lengths == allowed {
		c.add(KindEncoding, root, fmt.Sprintf("%d bytes", len(rawB)), fmt.Sprintf("%d bytes", len(rawC)))
	}
}

func isContainer(k jsonx.Kind) bool { return k == jsonx.Object || k == jsonx.Array }

func (c *comparer) walk(path string, b, v *jsonx.Value) {
	if b.Kind != v.Kind {
		// Below LevelTypes only a change of structure counts.
		if c.level >= LevelTypes || isContainer(b.Kind) || isContainer(v.Kind) {
			c.add(KindType, path, b.Kind.String(), v.Kind.String())
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
		if r := c.allow.ArrayKey(c.id, path); r != nil && c.keyed(path, r, b, v) {
			return
		}
		if len(b.Items) != len(v.Items) {
			if c.allow.AnyLength(c.id, path) {
				c.lengths++
			} else {
				c.add(KindArrayLength, path, strconv.Itoa(len(b.Items)), strconv.Itoa(len(v.Items)))
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
	case rule != nil:
		if same {
			return // written differently: the rule covers how it is written too
		}
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
	default:
		c.add(KindValue, path, b.ScalarText(), v.ScalarText())
		c.drifts[len(c.drifts)-1].Texts = [2]string{b.FormatText(), v.FormatText()}
	}
}

// object reports missing and extra keys and a change in the order of the keys
// both sides share, then compares the shared keys. Duplicate keys are paired
// by occurrence: the second "a" in the baseline with the second "a" now.
func (c *comparer) object(path string, b, v *jsonx.Value) {
	if FastPaths && sameKeys(b, v) {
		// Nothing is missing, extra or moved, and the n-th occurrence of a
		// key is at the same position on both sides.
		for i, m := range b.Members {
			c.walk(jsonx.KeyPath(path, m.Key), m.Value, v.Members[i].Value)
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
			c.add(KindKeyMissing, jsonx.KeyPath(path, m.Key), jsonx.Quote(m.Key), "")
		}
		seen[m.Key] = true
	}
	seen = map[string]bool{}
	for _, m := range v.Members {
		if !seen[m.Key] && nb[m.Key] < nc[m.Key] {
			c.add(KindKeyExtra, jsonx.KeyPath(path, m.Key), "", jsonx.Quote(m.Key))
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
		c.walk(jsonx.KeyPath(path, k), ob[k][i], oc[k][i])
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
