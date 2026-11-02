package compare

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

// KindItemOrder is a changed order of the items of an array paired by key
// (allowlist arrays:), unless the rule says any_order.
const KindItemOrder = "item-order"

// itemKeys reads the key of every item of an array: its identity (the
// text of an integer or a string, so 42 and "42" pair and the type change
// is reported inside the item) and its step in a path (id=42, slug="a").
// why says why the items cannot be paired by key: an item without the key,
// a key that is no integer or string, a key that two items share.
func itemKeys(r *allowlist.ArrayRule, a *jsonx.Value) (ids, steps []string, why string) {
	ids, steps = make([]string, len(a.Items)), make([]string, len(a.Items))
	seen := make(map[string]int, len(a.Items))
	name := keyName(r.Key)
	for i, item := range a.Items {
		v := item
		for _, name := range r.KeySteps() {
			if v = member(v, name); v == nil {
				return nil, nil, fmt.Sprintf("item %d has no %s", i, r.Key)
			}
		}
		switch v.Kind {
		case jsonx.Int:
			steps[i] = name + "=" + v.Text
		case jsonx.String:
			steps[i] = name + "=" + jsonx.Quote(v.Text)
		default:
			return nil, nil, fmt.Sprintf("the %s of item %d is %s, not an integer or a string", r.Key, i, v.Kind)
		}
		if j, dup := seen[v.Text]; dup {
			return nil, nil, fmt.Sprintf("items %d and %d have the same %s", j, i, r.Key)
		}
		seen[v.Text] = i
		ids[i] = v.Text
	}
	return ids, steps, ""
}

// keyName writes the key of an array rule in the step of an item: as it is
// when it is member names of letters, digits, _, $ and - joined by dots
// (id, user.id), quoted otherwise ("@id", "user id"), so that the step
// tells its key from its value whatever the key holds (a private body's
// paths hide the value: [id=(private)], ["@id"=(private)]).
func keyName(key string) string {
	for _, name := range strings.Split(key, ".") {
		if name == "" || strings.TrimFunc(name, func(r rune) bool {
			return r < 0x80 && (r == '_' || r == '$' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r))
		}) != "" {
			return jsonx.Quote(key)
		}
	}
	return key
}

// member is the first member of an object with the key, or nil.
func member(o *jsonx.Value, key string) *jsonx.Value {
	if o.Kind != jsonx.Object {
		return nil
	}
	for _, m := range o.Members {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// keyed compares two arrays whose items a rule pairs by key: items only
// one side has are missing or extra (the array may change length by an
// any_length rule), a changed order of the shared items is one drift
// unless the rule allows any order, and the pairs are compared at
// path[key=value]. It returns false, with a note, when the items cannot be
// paired by key; they are then paired by position.
func (c *comparer) keyed(path string, r *allowlist.ArrayRule, b, v *jsonx.Value) bool {
	bIDs, bSteps, why := itemKeys(r, b)
	var cIDs, cSteps []string
	side := "baseline"
	if why == "" {
		cIDs, cSteps, why = itemKeys(r, v)
		side = "current"
	}
	if why != "" {
		c.notes = append(c.notes, fmt.Sprintf("%s: items paired by position, not by %s: in the %s response %s", path, r.Key, side, why))
		return false
	}
	at := make(map[string]int, len(cIDs))
	for j, id := range cIDs {
		at[id] = j
	}
	var missing []int
	var pairs [][2]int
	moved, last := false, -1
	for i, id := range bIDs {
		j, ok := at[id]
		if !ok {
			missing = append(missing, i)
			continue
		}
		delete(at, id) // what is left is extra
		pairs = append(pairs, [2]int{i, j})
		moved = moved || j < last
		last = j
	}
	if len(missing) > 0 || len(at) > 0 {
		if c.allow.AnyLength(c.id, path) {
			c.lengths++
		} else {
			for _, i := range missing {
				c.add(KindKeyMissing, path+"["+bSteps[i]+"]", bSteps[i], "")
			}
			for j, step := range cSteps {
				if _, extra := at[cIDs[j]]; extra {
					c.add(KindKeyExtra, path+"["+step+"]", "", step)
				}
			}
		}
	}
	switch {
	case moved && r.AnyOrder:
		c.lengths++ // other bytes the rule explains
	case moved:
		// The shared items in the baseline's order and in the current one.
		bo, co := make([]string, len(pairs)), make([]int, len(pairs))
		for k, p := range pairs {
			bo[k], co[k] = bSteps[p[0]], p[1]
		}
		sort.Ints(co)
		cs := make([]string, len(co))
		for k, j := range co {
			cs[k] = cSteps[j]
		}
		c.add(KindItemOrder, path, keyList(bo), keyList(cs))
	}
	for _, p := range pairs {
		c.walk(path+"["+bSteps[p[0]]+"]", b.Items[p[0]], v.Items[p[1]])
	}
	return true
}

// keyList writes the keys of an order, at most 20 of them.
func keyList(steps []string) string {
	const most = 20
	if len(steps) > most {
		return strings.Join(steps[:most], ", ") + ", … " + strconv.Itoa(len(steps)-most) + " more"
	}
	return strings.Join(steps, ", ")
}
