package compare

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"slices"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/jsonx"
)

// spot is where a comparison stood, to go back to it: its drifts, notes,
// documents, counts, allowed orders and the allowlist rules it marked as
// used.
type spot struct {
	drifts, notes, docs         int
	allowed, rewritten, lengths int
	orders, uses                int
}

func (c *comparer) spot() spot {
	return spot{drifts: len(c.drifts), notes: len(c.notes), docs: len(c.docs), allowed: c.allowed, rewritten: c.rewritten, lengths: c.lengths,
		orders: len(c.orders), uses: c.allow.Uses()}
}

func (c *comparer) back(s spot) {
	c.drifts, c.notes, c.docs, c.orders = c.drifts[:s.drifts], c.notes[:s.notes], c.docs[:s.docs], c.orders[:s.orders]
	c.allowed, c.rewritten, c.lengths = s.allowed, s.rewritten, s.lengths
	c.allow.Forget(s.uses)
}

// order is another order of the items of an array that a rule allowed
// (any_order): at place k stands, in the baseline's order, the current
// item from[k]. base and cur are the two arrays.
type order struct {
	base, cur *jsonx.Value
	from      []int
}

// allowOrder counts an order a rule allowed, as other bytes the rule
// explains, and keeps it: the rest of the document is still compared as
// it is written (json, unorder). from is nil when not every item is
// paired (items came or went too).
func (c *comparer) allowOrder(base, cur *jsonx.Value, from []int) {
	c.lengths++
	if from != nil {
		c.orders = append(c.orders, order{base, cur, from})
	}
}

// unorder writes the text of a current JSON document with the items of
// the orders a rule allowed put back in the baseline's order, inner arrays
// before the arrays around them, so that what it writes besides the order
// compares with the baseline's text byte for byte. It reads both texts
// along their parsed values (jb, jc): the bytes between items stay where
// they stand. When the items of an array are, byte for byte, the
// baseline's in another order, they are put back by their bytes (items
// that compare equal but are written otherwise would otherwise trade
// places). false when the texts do not follow the values (a payload
// compared decoded): the caller then compares the texts as they are.
func unorder(tb, tc []byte, jb, jc *jsonx.Value, orders []order) ([]byte, bool) {
	want := make(map[*jsonx.Value]bool, len(orders))
	byCur := make(map[*jsonx.Value]*order, len(orders))
	for i := range orders {
		want[orders[i].base] = true
		byCur[orders[i].cur] = &orders[i]
	}
	b := &unorderer{doc: tb, items: map[*jsonx.Value][][]byte{}, want: want}
	if !b.document(jb) {
		return nil, false
	}
	c := &unorderer{doc: tc, orders: byCur, baseItems: b.items}
	if !c.document(jc) {
		return nil, false
	}
	return c.out, true
}

// unorderer reads a JSON text along its parsed value: of the arrays in
// want it keeps the bytes of every item (items); the arrays in orders it
// writes (out) with their items in the baseline's order.
type unorderer struct {
	doc       []byte
	out       []byte
	want      map[*jsonx.Value]bool
	items     map[*jsonx.Value][][]byte
	orders    map[*jsonx.Value]*order
	baseItems map[*jsonx.Value][][]byte
}

// document reads the whole text: what stands around the value too.
func (u *unorderer) document(v *jsonx.Value) bool {
	i := u.space(0)
	for i < len(u.doc) && !strings.ContainsRune("{[\"-0123456789tfnNI", rune(u.doc[i])) {
		u.out = append(u.out, u.doc[i]) // a byte order mark
		i++
	}
	end, ok := u.value(i, v)
	if !ok {
		return false
	}
	u.out = append(u.out, u.doc[end:]...)
	return true
}

// space writes the whitespace from i on and returns where it ends.
func (u *unorderer) space(i int) int {
	j := i
	for j < len(u.doc) && jsonSpace(u.doc[j]) {
		j++
	}
	u.out = append(u.out, u.doc[i:j]...)
	return j
}

// value writes the value at doc[i] and returns where it ends.
func (u *unorderer) value(i int, v *jsonx.Value) (int, bool) {
	if i >= len(u.doc) {
		return i, false
	}
	kind, end := token(u.doc, i)
	switch kind {
	case '{':
		if v.Kind != jsonx.Object {
			return i, false
		}
		u.out = append(u.out, '{')
		i = u.space(end)
		for k := range v.Members {
			if k > 0 {
				if i >= len(u.doc) || u.doc[i] != ',' {
					return i, false
				}
				u.out = append(u.out, ',')
				i = u.space(i + 1)
			}
			if i >= len(u.doc) || u.doc[i] != '"' {
				return i, false
			}
			_, kend := token(u.doc, i)
			u.out = append(u.out, u.doc[i:kend]...)
			i = u.space(kend)
			if i >= len(u.doc) || u.doc[i] != ':' {
				return i, false
			}
			u.out = append(u.out, ':')
			var ok bool
			if i, ok = u.value(u.space(i+1), v.Members[k].Value); !ok {
				return i, false
			}
			i = u.space(i)
		}
		if i >= len(u.doc) || u.doc[i] != '}' {
			return i, false
		}
		u.out = append(u.out, '}')
		return i + 1, true
	case '[':
		if v.Kind != jsonx.Array {
			return i, false
		}
		return u.array(end, v)
	case 'v':
		if isContainer(v.Kind) {
			return i, false
		}
		u.out = append(u.out, u.doc[i:end]...)
		return end, true
	}
	return i, false
}

// array writes the items of an array from doc[i] on (after its [) and the
// ], in the baseline's order when a rule allowed another one.
func (u *unorderer) array(i int, v *jsonx.Value) (int, bool) {
	o := u.orders[v]
	var gaps, items [][]byte // the bytes before every item and the ], and every item
	outer := u.out
	u.out = nil
	i = u.space(i)
	for k, item := range v.Items {
		if k > 0 {
			if i >= len(u.doc) || u.doc[i] != ',' {
				return i, false
			}
			u.out = append(u.out, ',')
			i = u.space(i + 1)
		}
		gaps, u.out = append(gaps, u.out), nil
		start := i
		var ok bool
		if i, ok = u.value(i, item); !ok {
			return i, false
		}
		if u.want[v] {
			u.items[v] = append(u.items[v], u.doc[start:i])
		}
		items, u.out = append(items, u.out), nil
		i = u.space(i)
	}
	if i >= len(u.doc) || u.doc[i] != ']' {
		return i, false
	}
	gaps, u.out = append(gaps, u.out), outer
	from := []int(nil)
	if o != nil {
		from = o.from
		if base := u.baseItems[o.base]; len(base) == len(items) {
			if f, ok := byBytes(base, items); ok {
				from = f
			}
		}
	}
	u.out = append(u.out, '[')
	for k := range items {
		u.out = append(u.out, gaps[k]...)
		if from != nil {
			u.out = append(u.out, items[from[k]]...)
		} else {
			u.out = append(u.out, items[k]...)
		}
	}
	u.out = append(append(u.out, gaps[len(items)]...), ']')
	return i + 1, true
}

// byBytes pairs every baseline item with a current item of the same bytes,
// the n-th with the n-th; false when the items are not the same bytes.
func byBytes(base, cur [][]byte) ([]int, bool) {
	waiting := make(map[string][]int, len(cur))
	for j, item := range cur {
		waiting[string(item)] = append(waiting[string(item)], j)
	}
	from := make([]int, len(base))
	for k, item := range base {
		q := waiting[string(item)]
		if len(q) == 0 {
			return nil, false
		}
		from[k], waiting[string(item)] = q[0], q[1:]
	}
	return from, true
}

// items compares two arrays of one length that no rule pairs by key.
//
// When the current items are the baseline's, byte for byte, in another
// order, and every item that moved differs from the one now at its place
// in a way the comparison always reports (permuted), the array is one
// item-order drift and nothing else: no comparison by position, no hash.
//
// Otherwise the items are compared by position first; only when that
// finds drifts are the items with drifts taken as a multiset (each hashed
// once, as the comparison sees it: at its level, a value a rule lets
// change as the rule), and the items without stay where they are. When
// those items are the same ones in another order, or all but one item of
// each side, the comparison is redone with them paired by content (what
// the comparison by position did is undone, the allowlist rules it marked
// as used too): one item-order drift at the array (unless an array rule
// without a key allows any order), with a note that names the rule to
// add, and every pair compared at the baseline item's path, so that what
// differs within a pair (a value written otherwise, the one changed item)
// is still reported. Otherwise (two or more items changed: which went
// where is ambiguous) the drifts by position stand.
func (c *comparer) items(path string, b, v *jsonx.Value) {
	pairs, alone := c.permuted(path, b, v)
	if pairs != nil {
		c.ordered(path, b, v, pairs, true)
		return
	}
	at := c.spot()
	// The items with drifts by position, as runs of positions on a stack
	// the comparer keeps (arrays inside push theirs above and take them
	// off again): items that changed in place are one run.
	base := len(c.moved)
	defer func() { c.moved = c.moved[:base] }()
	count := 0
	for i := range b.Items {
		before := len(c.drifts)
		c.walk(path+"["+strconv.Itoa(i)+"]", b.Items[i], v.Items[i])
		if len(c.drifts) == before {
			continue
		}
		count++
		if top := len(c.moved) - 1; top >= base && c.moved[top][1] == i {
			c.moved[top][1] = i + 1
		} else {
			c.moved = append(c.moved, [2]int{i, i + 1})
		}
	}
	if count < 2 {
		return // one item cannot have changed places
	}
	runs := c.moved[base:]
	pairs, same := c.reordered(path, b, v, runs, count, alone)
	if pairs == nil {
		return
	}
	c.back(at)
	c.ordered(path, b, v, pairs, same)
}

// ordered reports two arrays whose items are paired by content (pairs[i]
// is the current item of baseline item i) and compares the pairs.
func (c *comparer) ordered(path string, b, v *jsonx.Value, pairs []int, same bool) {
	switch r := c.allow.ArrayKey(c.id, path); {
	case r != nil && r.Key == "" && r.AnyOrder:
		from := make([]int, len(pairs))
		copy(from, pairs)
		c.allowOrder(b, v, from)
	default:
		// For every item now, the baseline's item it is paired with.
		from := make([]string, len(pairs))
		for i, j := range pairs {
			from[j] = strconv.Itoa(i)
		}
		n := len(pairs)
		cur := fmt.Sprintf("same %d items, other order: %s", n, keyList(from))
		if !same {
			cur = fmt.Sprintf("%d of %d items the same, other order: %s", n-1, n, keyList(from))
		}
		c.add(KindItemOrder, path, plural(n, "item"), cur)
		if c.witnessing {
			// Both arrays as compared: the drift digests which items stand
			// where, not only the positions it shows.
			c.witness(c.source(path, b), c.source(path, v))
		}
		if hint := c.orderHint(path, b, v, same); r == nil && !slices.Contains(c.notes, hint) {
			c.notes = append(c.notes, hint)
		}
	}
	for i, j := range pairs {
		c.walk(path+"["+strconv.Itoa(i)+"]", b.Items[i], v.Items[j])
	}
}

// seed hashes items for pairing; a hash only finds candidates, which are
// then compared byte for byte.
var seed = maphash.MakeSeed()

// permuted pairs the items of two arrays of one length when the current
// ones are the baseline's, byte for byte, in another order: an item stays
// with the item at its place when they are the same bytes, the others go
// the n-th with the n-th of the same bytes. It answers only where that is
// the whole result: at the values level, with no body or array rule that
// can reach inside the array (nothing the comparison would let change or
// pair otherwise), and when every
// item that moved differs from the item now at its place in what the
// comparison always reports (differs: at the values level without rules
// that is a drift), so that the comparison by position would have drifted
// there. nil otherwise.
//
// alone is a current item that has no equal among the baseline's items
// even as compared (reordered need not look again), or -1.
func (c *comparer) permuted(path string, b, v *jsonx.Value) (pairs []int, alone int) {
	if c.level < LevelValues || c.allow.RulesUnder(c.id, path, false) {
		return nil, -1
	}
	// One look first: the first item that moved must be one of the
	// baseline's now (none is, when the items changed in place). It looks
	// for an equal as compared, which every item of the same bytes is.
	first := -1
	for i, item := range b.Items {
		if !bytes.Equal(item.Src, v.Items[i].Src) {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, -1
	}
	if c.unequal(b, v, first) {
		return nil, first
	}
	pairs = make([]int, len(b.Items))
	for i, item := range b.Items {
		if item.Src == nil || v.Items[i].Src == nil {
			return nil, -1 // values a decoder made have no source
		}
		pairs[i] = i
		if !bytes.Equal(item.Src, v.Items[i].Src) {
			pairs[i] = -1
		}
	}
	waiting := map[uint64][]int{}
	for i, j := range pairs {
		if j < 0 {
			h := maphash.Bytes(seed, b.Items[i].Src)
			waiting[h] = append(waiting[h], i)
		}
	}
	for j, item := range v.Items {
		if pairs[j] == j {
			continue
		}
		h := maphash.Bytes(seed, item.Src)
		q := waiting[h]
		k := slices.IndexFunc(q, func(i int) bool { return bytes.Equal(b.Items[i].Src, item.Src) })
		if k < 0 {
			return nil, -1
		}
		pairs[q[k]] = j
		if k == 0 {
			waiting[h] = q[1:] // the n-th with the n-th: the first that waits
		} else {
			waiting[h] = slices.Delete(q, k, k+1) // another item of the same hash first
		}
	}
	for i, j := range pairs {
		if j != i && !c.differs(b.Items[i], v.Items[i]) {
			return nil, -1
		}
	}
	return pairs, -1
}

// reordered pairs the items of two arrays of one length at the positions
// that moved (runs of them, count in all) by their content: pairs[i] is the current item of baseline item i;
// of equal items, the n-th with the n-th in order, and every other item
// with the item at its position. same reports that every moved item has
// its equal; otherwise exactly one item of each side had none, and they
// are paired. nil when the pairing is the order by position, or more than
// one item of a side has no equal.
//
// Items are equal when their forms are (form): as the comparison sees
// them. The two sides are taken in turn, and an item is paired with the
// first waiting equal item of the other side (a hash finds the candidates,
// the forms are compared); it stops as soon as more than one item of a
// side is sure to stay without an equal (items changed in place).
func (c *comparer) reordered(path string, b, v *jsonx.Value, runs [][2]int, count, alone int) (pairs []int, same bool) {
	first, second := runs[0][0], runs[0][0]+1
	if runs[0][1] == second {
		second = runs[1][0]
	}
	own := c.own // the items as compared, the same whether witnessing or not
	c.own = nil
	defer func() { c.own = own }()
	// Paths are built when a rule can reach inside: a value's rule, or an
	// array's that lets its items come in any order (form).
	rules := c.level >= LevelValues && c.allow.RulesUnder(c.id, path, false)
	if !rules && (first == alone || c.unequal(b, v, first)) && c.unequal(b, v, second) {
		return nil, false // two current items with no equal: changed in place
	}
	moved := make([]int, 0, count)
	for _, r := range runs {
		for i := r[0]; i < r[1]; i++ {
			moved = append(moved, i)
		}
	}
	var buf []byte // the forms of the waiting items
	type item struct {
		at, start, end int
		next           int // the next waiting item of the same hash, -1
	}
	items := make([]item, 0, 2*len(moved))
	type queue struct{ head, tail int }
	var waiting [2]map[uint64]queue
	var left [2]int
	for s := range waiting {
		waiting[s] = make(map[uint64]queue, len(moved))
	}
	pairs = make([]int, len(b.Items))
	for i := range pairs {
		pairs[i] = i
	}
	sides := [2]*jsonx.Value{b, v}
	for k, at := range moved {
		for s := range sides {
			start := len(buf)
			buf = c.form(buf, itemPath(rules, path, at), sides[s].Items[at], rules)
			form := buf[start:]
			h := maphash.Bytes(seed, form)
			q, ok := waiting[1-s][h]
			prev, x := -1, -1
			for i := q.head; ok && i >= 0; prev, i = i, items[i].next {
				if bytes.Equal(buf[items[i].start:items[i].end], form) {
					x = i
					break
				}
			}
			if x < 0 {
				items = append(items, item{at: at, start: start, end: len(buf), next: -1})
				n := len(items) - 1
				if w, ok := waiting[s][h]; ok {
					items[w.tail].next = n
					waiting[s][h] = queue{w.head, n}
				} else {
					waiting[s][h] = queue{n, n}
				}
				left[s]++
				continue
			}
			buf = buf[:start] // paired: its form is not needed
			if s == 0 {
				pairs[at] = items[x].at
			} else {
				pairs[items[x].at] = at
			}
			switch {
			case prev < 0 && items[x].next < 0:
				delete(waiting[1-s], h)
			case prev < 0:
				waiting[1-s][h] = queue{items[x].next, q.tail}
			default:
				items[prev].next = items[x].next
				if q.tail == x {
					q.tail = prev
				}
				waiting[1-s][h] = q
			}
			left[1-s]--
		}
		// Each item still to come pairs one waiting item at most.
		if rest := len(moved) - k - 1; left[0]-rest > 1 || left[1]-rest > 1 {
			return nil, false
		}
	}
	if left[0] > 1 || left[1] > 1 {
		return nil, false
	}
	if left[0] == 1 {
		var lone [2]int
		for s := range waiting {
			for _, q := range waiting[s] {
				lone[s] = items[q.head].at
			}
		}
		pairs[lone[0]] = lone[1] // the one item of each side with no equal
	}
	for i, j := range pairs {
		if i != j {
			return pairs, left[0] == 0
		}
	}
	return nil, false
}

// anyOrder reports an array whose items a rule lets come in any order
// (with rules: paths are built).
func (c *comparer) anyOrder(rules bool, path string) bool {
	if !rules {
		return false
	}
	r := c.allow.ArrayKey(c.id, path)
	return r != nil && r.AnyOrder
}

// unequal reports that the current item at j has no equal among the
// baseline's items, by a look at each that stops at the first difference
// (differs); without rules that can reach inside the array.
func (c *comparer) unequal(b, v *jsonx.Value, j int) bool {
	x := v.Items[j]
	// The first member of an object (or a scalar item) is looked at in
	// place: most items differ there.
	first := x
	key, keyed := "", x.Kind == jsonx.Object && len(x.Members) > 0
	if keyed {
		key, first = x.Members[0].Key, x.Members[0].Value
	}
	quick := c.level >= LevelValues && !isContainer(first.Kind) && (keyed || x.Kind != jsonx.Object)
	for _, item := range b.Items {
		if quick {
			f := item
			if keyed {
				if item.Kind != jsonx.Object || len(item.Members) != len(x.Members) || item.Members[0].Key != key {
					continue
				}
				f = item.Members[0].Value
			}
			if f.Kind != first.Kind || f.Text != first.Text || f.Bool != first.Bool {
				continue
			}
		}
		if !c.differs(item, x) {
			return false
		}
	}
	return true
}

// differs reports two values whose forms differ at the comparison's level
// with no rule: another kind (a scalar's from the types level), other keys
// or key order, another array length, another scalar at the values level,
// or such a difference inside.
func (c *comparer) differs(b, v *jsonx.Value) bool {
	if b.Kind != v.Kind {
		return c.level >= LevelTypes || isContainer(b.Kind) || isContainer(v.Kind)
	}
	switch b.Kind {
	case jsonx.Object:
		if len(b.Members) != len(v.Members) {
			return true
		}
		for i, m := range b.Members { // key and value together: most differ at once
			if m.Key != v.Members[i].Key || c.differs(m.Value, v.Members[i].Value) {
				return true
			}
		}
		return false
	case jsonx.Array:
		if len(b.Items) != len(v.Items) {
			return true
		}
		for i, item := range b.Items {
			if c.differs(item, v.Items[i]) {
				return true
			}
		}
		return false
	}
	return c.level >= LevelValues && (b.Text != v.Text || b.Bool != v.Bool)
}

// form writes a JSON value as the comparison sees it, for pairing items:
// keys in order, lengths, kinds from the types level, values at the
// values level, a value a rule lets change as the rule (rules: whether a
// rule can reach inside; paths are built only then). Two values with one
// form compare without a drift by position. It is the canon of a witness
// (canonJSON) written without its tags and quoting.
func (c *comparer) form(b []byte, path string, v *jsonx.Value, rules bool) []byte {
	b = append(b, byte(v.Kind))
	switch v.Kind {
	case jsonx.Object:
		b = binary.AppendUvarint(b, uint64(len(v.Members)))
		for _, m := range v.Members {
			b = binary.AppendUvarint(b, uint64(len(m.Key)))
			b = append(b, m.Key...)
			b = c.form(b, keyPath(rules, path, m.Key), m.Value, rules)
		}
		return b
	case jsonx.Array:
		b = binary.AppendUvarint(b, uint64(len(v.Items)))
		if r := c.anyOrder(rules, path); r {
			// Items a rule lets come in any order: in the order of their
			// forms, as the comparison pairs them whatever their order.
			forms := make([][]byte, len(v.Items))
			for i, item := range v.Items {
				forms[i] = c.form(nil, itemPath(rules, path, i), item, rules)
			}
			slices.SortFunc(forms, bytes.Compare)
			for _, f := range forms {
				b = append(b, f...)
			}
			return b
		}
		for i, item := range v.Items {
			b = c.form(b, itemPath(rules, path, i), item, rules)
		}
		return b
	}
	switch {
	case c.level < LevelTypes:
		return b[:len(b)-1] // no kind below the types level
	case c.level < LevelValues:
		return b
	}
	text := v.Text
	if v.Kind == jsonx.Bool && v.Bool {
		text = "true"
	}
	if rules {
		if n, ok := c.normal(path, v.FormatText(), false); ok {
			e := &canon{b: b}
			e.value(n, true, "")
			return e.b
		}
	}
	b = append(b, 'X')
	b = binary.AppendUvarint(b, uint64(len(text)))
	return append(b, text...)
}

// idNames are the members an item's identity is looked for in, in order.
var idNames = []string{"id", "uuid", "_id", "key", "slug", "code"}

// orderHint is the note on an array whose items came in another order
// (once per request for arrays of one path): the array rule to add under
// arrays: in allowlist.yaml, written to paste, that allows any order. It
// pairs the items by a member every item of both sides has with a value of
// its own (an id) when there is one, which still reports an item that
// changed, came or went; otherwise it keeps them by position.
func (c *comparer) orderHint(path string, b, v *jsonx.Value, same bool) string {
	how := "any_order: true"
	if key := idName(b, v); key != "" {
		how = "key: " + key + ", any_order: true"
	}
	what := "the same items in another order"
	if !same {
		what = "the items in another order, one of them changed"
	}
	return fmt.Sprintf("%s: %s; if the order does not matter, add under arrays: in allowlist.yaml: - {request: %s, path: %s, %s, reason: '…'}",
		AnyItem(path), what, YAMLQuote(c.id), YAMLQuote(AnyItem(path)), how)
}

// YAMLQuote writes a text as a single-quoted YAML scalar.
func YAMLQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// idName is the first of idNames that every item of both arrays has as an
// integer or a string, different in every item of a side; "" if none.
func idName(arrays ...*jsonx.Value) string {
next:
	for _, name := range idNames {
		for _, a := range arrays {
			seen := make(map[string]bool, len(a.Items))
			for _, item := range a.Items {
				m := member(item, name)
				if m == nil || m.Kind != jsonx.Int && m.Kind != jsonx.String || seen[m.Text] {
					continue next
				}
				seen[m.Text] = true
			}
		}
		return name
	}
	return ""
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
