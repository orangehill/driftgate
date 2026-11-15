package compare

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/sheet"
)

// canon writes what expectations digest (witnesses and the text of a
// drift) in one encoding that never gives two different contents the same
// bytes: every item starts with a tag, every text and byte string with its
// length, every list with its count. No part is joined by a separator a
// value could hold.
type canon struct {
	b []byte
	// own writes the origins of both sides neutrally in every text and
	// byte string, before its length is written.
	own *originSide
}

// normal is how the rules see a value for a digest: with the origins of
// both sides written neutrally first, so that it reads the same captured
// from another address.
func (c *comparer) normal(path, value string, cells bool) (allowlist.Normal, bool) {
	return c.allow.Normal(c.id, path, c.neutral(value), cells)
}

func (c *comparer) neutral(s string) string {
	if c.own == nil {
		return s
	}
	return c.own.text(s)
}

// enc is a canon of a comparison: with its own origins written neutrally.
func (c *comparer) enc() *canon { return &canon{own: c.own} }

// Tags of canon items.
const (
	cTagText   = 'S' // a text: length, bytes
	cTagBytes  = 'B' // bytes: length, bytes
	cTagInt    = 'I' // an integer
	cTagTrue   = 'T'
	cTagFalse  = 'F'
	cTagList   = 'L' // a list: count, then its items
	cTagObject = 'O' // a JSON object: count, then key and value of each member
	cTagArray  = 'A' // a JSON array: count, then its items
	cTagScalar = 'V' // a JSON scalar: its kind, then its text or its rule
	cTagRule   = 'R' // a value a whole-value rule accepts: the rule
	cTagParts  = 'P' // a value rules with parts see: its pieces
	cTagExact  = 'X' // a value as it is
	cTagNode   = 'E' // an element of a page
	cTagNone   = 'N' // nothing (a value left out below the values level)
)

func (e *canon) tag(t byte) *canon { e.b = append(e.b, t); return e }

func (e *canon) uint(n uint64) *canon { e.b = binary.AppendUvarint(e.b, n); return e }

func (e *canon) text(s string) *canon {
	if e.own != nil {
		s = e.own.text(s)
	}
	e.tag(cTagText).uint(uint64(len(s)))
	e.b = append(e.b, s...)
	return e
}

func (e *canon) bytes(b []byte) *canon {
	if e.own != nil {
		b = e.own.replace(b)
	}
	e.tag(cTagBytes).uint(uint64(len(b)))
	e.b = append(e.b, b...)
	return e
}

func (e *canon) int(n int) *canon {
	e.tag(cTagInt)
	e.b = binary.AppendVarint(e.b, int64(n))
	return e
}

func (e *canon) bool(v bool) *canon {
	if v {
		return e.tag(cTagTrue)
	}
	return e.tag(cTagFalse)
}

func (e *canon) list(n int) *canon { return e.tag(cTagList).uint(uint64(n)) }

func (e *canon) texts(list []string) *canon {
	e.list(len(list))
	for _, s := range list {
		e.text(s)
	}
	return e
}

// sum is the SHA-256 of what was written, in hex.
func (e *canon) sum() string {
	h := sha256.Sum256(e.b)
	return hex.EncodeToString(h[:])
}

// value writes a value (a JSON scalar's text, an attribute, a text, a
// cell) as the rules see it: as it is, as the rule that accepts it, or as
// the pieces rules with parts leave.
func (e *canon) value(n allowlist.Normal, ok bool, text string) *canon {
	switch {
	case !ok:
		return e.tag(cTagExact).text(text)
	case n.Pieces == nil:
		return e.tag(cTagRule).text(n.Rule).text(n.Format)
	}
	e.tag(cTagParts).list(len(n.Pieces))
	for _, p := range n.Pieces {
		if p.Part {
			e.tag(cTagTrue)
		} else {
			e.tag(cTagFalse).text(p.Text)
		}
	}
	return e
}

// json writes a JSON value at a path as the comparison sees it: a value
// an allowlist rule lets change as the rule; below the values level a
// scalar is its kind (types) or nothing (shape).
func (c *comparer) canonJSON(e *canon, path string, v *jsonx.Value, rules bool) {
	switch v.Kind {
	case jsonx.Object:
		e.tag(cTagObject).uint(uint64(len(v.Members)))
		for _, m := range v.Members {
			e.text(m.Key)
			c.canonJSON(e, keyPath(rules, path, m.Key), m.Value, rules)
		}
	case jsonx.Array:
		e.tag(cTagArray).uint(uint64(len(v.Items)))
		for i, item := range v.Items {
			c.canonJSON(e, itemPath(rules, path, i), item, rules)
		}
	default:
		e.tag(cTagScalar)
		switch {
		case c.level < LevelTypes:
			e.tag(cTagNone)
		case c.level < LevelValues:
			e.text(v.Kind.String())
		default:
			e.text(v.Kind.String())
			if !rules {
				e.tag(cTagExact).text(v.ScalarText())
				return
			}
			n, ok := c.normal(path, v.FormatText(), false)
			e.value(n, ok, v.ScalarText())
		}
	}
}

// keyPath and itemPath name a place for the rules; without rules no path
// is built.
func keyPath(rules bool, path, key string) string {
	if !rules {
		return ""
	}
	return jsonx.KeyPath(path, key)
}

func itemPath(rules bool, path string, i int) string {
	if !rules {
		return ""
	}
	return path + "[" + strconv.Itoa(i) + "]"
}

// source writes a JSON value at a path for a witness.
func (c *comparer) source(path string, v *jsonx.Value) []byte {
	e := c.enc()
	c.canonJSON(e, path, v, c.level >= LevelValues && c.allow.HasValueRules())
	return e.b
}

// element writes an element only one side has, with everything the
// comparison looks at: tag, id, attributes, texts with where they stand
// and the spacing around them, whether it carries the RSC payload, and its
// children. An attribute or text a rule lets change is written as the
// rule; below the values level texts and attribute values are left out.
func (c *comparer) element(e *canon, path string, el *element) {
	e.tag(cTagNode).text(el.tag).text(el.id).bool(el.flight)
	names := slices.Sorted(maps.Keys(el.attrs))
	e.list(len(names))
	for _, k := range names {
		e.text(k)
		if c.level < LevelValues {
			e.tag(cTagNone)
			continue
		}
		n, ok := c.normal(path+"@"+pathName(k), el.attrs[k], false)
		e.value(n, ok, el.attrs[k])
	}
	if c.level < LevelValues {
		e.tag(cTagNone)
	} else {
		// A rule of the element's text sees it joined; where the runs
		// stand and the spacing around them are written either way.
		n, ok := c.normal(path, el.joined(), false)
		e.list(len(el.text))
		for _, t := range el.text {
			e.int(t.at).bool(t.lead).bool(t.trail)
			if !ok {
				e.text(t.text)
			}
		}
		if ok {
			e.value(n, ok, "")
		}
	}
	steps := steps(el.kids)
	e.list(len(el.kids))
	for i, k := range el.kids {
		c.element(e, path+">"+steps[i], k)
	}
}

// runsSource writes an element's texts as rules with parts see them, with
// where each stands among the children and the spacing around it.
func (c *comparer) runsSource(path string, el *element) []byte {
	e := c.enc()
	e.list(len(el.text))
	for _, t := range el.text {
		n, ok := c.normal(path, t.text, false)
		e.int(t.at).bool(t.lead).bool(t.trail).value(n, ok, t.text)
	}
	e.texts(keys(el.kids))
	return e.b
}

// around is the texts of a parent as they stand, for the witness of a
// child only one side has: the spacing next to it is part of how the page
// reads. Below the values level nothing.
func (c *comparer) around(path string, e *element) []byte {
	if c.level < LevelValues {
		return nil
	}
	return c.runsSource(path, e)
}

func (c *comparer) elementSource(path string, el *element) []byte {
	e := c.enc()
	c.element(e, path, el)
	return e.b
}

// sheetSource writes a worksheet for a witness: its name, every cell with
// its value (as the rules see it) and what it has besides; below the values
// level only which cells there are and their types.
func (c *comparer) sheetSource(prefix string, s sheet.Sheet) []byte {
	refs := slices.Sorted(maps.Keys(s.Cells))
	for ref := range s.Meta {
		if _, ok := s.Cells[ref]; !ok {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs)
	e := c.enc()
	e.text(s.Name).bool(s.Named).list(len(refs))
	for _, ref := range refs {
		v, has := s.Cells[ref]
		m := s.Meta[ref]
		e.text(ref).bool(has).text(m.Type)
		if c.level < LevelValues {
			continue
		}
		n, ok := c.normal(prefix+"!"+ref, v, true)
		e.value(n, ok, v)
		e.text(m.Format).text(m.Formula).text(m.Style).text(s.Rich[ref])
	}
	return e.b
}

// partsSource writes the parts of a workbook apart from its cells.
func (c *comparer) partsSource(parts []sheet.Part) []byte {
	e := c.enc()
	e.list(len(parts))
	for _, p := range parts {
		e.text(p.Name).bytes(p.Data)
	}
	return e.b
}

// styleSource writes the layout of a JSON document.
func styleSource(s jsonx.Style) []byte {
	e := &canon{}
	e.int(int(s.Lines)).int(int(s.Indent)).int(s.Unit).int(int(s.Colon)).int(int(s.BeforeColon)).int(int(s.Comma))
	return e.b
}

// pairsSource writes which item of one side went where on the other.
func pairsSource(pairs [][2]int) []byte {
	e := &canon{}
	e.list(len(pairs))
	for _, p := range pairs {
		e.int(p[0]).int(p[1])
	}
	return e.b
}

func (c *comparer) textsSource(list []string) []byte { return c.enc().texts(list).b }

// digestText writes a drift for its digest.
func digestText(kind, where, base, cur, witness string) string {
	return string((&canon{}).text(kind).text(where).text(base).text(cur).text(witness).b)
}

// witness is the SHA-256 of parts, as a list.
func witness(parts ...[]byte) string {
	e := &canon{}
	e.list(len(parts))
	for _, p := range parts {
		e.bytes(p)
	}
	return e.sum()
}
