package compare

import (
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/jsonx"
)

func str(s string) *jsonx.Value {
	return &jsonx.Value{Kind: jsonx.String, Text: s, Raw: jsonx.Quote(s)}
}

func object(members ...jsonx.Member) *jsonx.Value {
	return &jsonx.Value{Kind: jsonx.Object, Members: members}
}

// flightValue turns the chunks of a React Server Components payload
// (self.__next_f.push([1,"…"])) into one JSON object: a key per row, in
// payload order. A row is "<hex id>:<tag><JSON>" up to a newline; a tagged
// row (I for a client module, HL for a preload hint, …) becomes {"<tag>":
// <JSON>}, a text row (T<hex length>,<text>) {"T": "<text>"}. A row without an
// id has the key "_", and the same id may come back (a row that resolves
// later); the comparison pairs repeated keys in order. What does not parse
// stays text, so a change in it is still reported.
func flightValue(pushes []string) *jsonx.Value {
	out := object()
	add := func(key string, v *jsonx.Value) { out.Members = append(out.Members, jsonx.Member{Key: key, Value: v}) }
	var data strings.Builder
	for _, p := range pushes {
		arg, err := jsonx.Parse([]byte(p))
		switch {
		case err != nil || arg.Kind != jsonx.Array || len(arg.Items) == 0:
			add("push", str(p))
		case arg.Items[0].Kind == jsonx.Int && arg.Items[0].Text == "0" && len(arg.Items) == 1:
			// The bootstrap push carries no data.
		case arg.Items[0].Kind == jsonx.Int && arg.Items[0].Text == "1" && len(arg.Items) == 2 && arg.Items[1].Kind == jsonx.String:
			data.WriteString(arg.Items[1].Text)
		default:
			// Form state (2) and binary chunks (3) are compared as they are.
			add("push", arg)
		}
	}
	rows := data.String()
	for rows != "" {
		colon := strings.IndexByte(rows, ':')
		nl := strings.IndexByte(rows, '\n')
		if nl < 0 {
			nl = len(rows)
		}
		if colon < 0 || colon > nl || !isHex(rows[:colon]) {
			add("?", str(rows[:nl]))
			rows = strings.TrimPrefix(rows[nl:], "\n")
			continue
		}
		id := rows[:colon]
		if id == "" {
			id = "_"
		}
		rows = rows[colon+1:]
		nl -= colon + 1
		tag := 0
		for tag < len(rows) && tag < nl && rows[tag] >= 'A' && rows[tag] <= 'Z' {
			tag++
		}
		if tag == 1 && rows[0] == 'T' {
			// Text row: the length (hex, in bytes) says where it ends; it has
			// no newline of its own.
			if comma := strings.IndexByte(rows, ','); comma > 1 {
				// The length is checked against what is left before it is
				// added to anything: a negative or huge one does not cut.
				if n, err := strconv.ParseInt(rows[1:comma], 16, 64); err == nil && n >= 0 && n <= int64(len(rows)-comma-1) {
					add(id, object(jsonx.Member{Key: "T", Value: str(rows[comma+1 : comma+1+int(n)])}))
					rows = rows[comma+1+int(n):]
					continue
				}
			}
		}
		body := rows[tag:nl]
		var v *jsonx.Value
		if body == "" {
			v = &jsonx.Value{Kind: jsonx.Null}
		} else if parsed, err := jsonx.Parse([]byte(body)); err == nil {
			v = parsed
		} else {
			v = str(body)
		}
		if tag > 0 {
			v = object(jsonx.Member{Key: rows[:tag], Value: v})
		}
		add(id, v)
		rows = strings.TrimPrefix(rows[nl:], "\n")
	}
	return out
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// maxDevalue bounds the decoded size of a Nuxt payload: devalue shares
// repeated values, and expanding them could grow without end.
const maxDevalue = 1 << 20

// devalue decodes Nuxt's payload (__NUXT_DATA__, written by devalue): one flat
// array whose first item is the root and whose objects and arrays hold the
// indexes of their values. Decoding gives paths by key ($.data.page.title)
// instead of positions in the array, and a value added in one place does not
// move every index after it. Vue's wrappers (Reactive, ShallowReactive, Ref,
// ShallowRef) are left out of the path; other typed values become
// {"<type>": …} ({"Date": "2026-10-29T10:00:00.000Z"}). Special numbers
// (undefined, NaN, Infinity, -0, a hole) become constants, and a value that
// refers back to one that contains it becomes {"$ref": "<its path>"}, so a
// reference to another value is a drift at that place. ok is false when the
// array is not in this form, or when it decodes deeper than a JSON document
// may be nested (jsonx.MaxDepth) or larger than maxDevalue; it is then
// compared as plain JSON.
func devalue(root *jsonx.Value) (v *jsonx.Value, ok bool) {
	if root.Kind != jsonx.Array || len(root.Items) == 0 {
		return nil, false
	}
	arr := root.Items
	size := 0
	// steps is the path of the value being decoded; open holds, for every
	// index being decoded, how many steps its path has.
	var steps []string
	open := map[int]int{}
	special := map[string]string{"-1": "undefined", "-2": "hole", "-3": "NaN", "-4": "Infinity", "-5": "-Infinity", "-6": "-0"}
	var resolve func(ref *jsonx.Value) *jsonx.Value
	// at resolves a value one or more steps further down the path.
	at := func(ref *jsonx.Value, step ...string) *jsonx.Value {
		n := len(steps)
		steps = append(steps, step...)
		x := resolve(ref)
		steps = steps[:n]
		return x
	}
	fail := false
	depth := 0
	resolve = func(ref *jsonx.Value) *jsonx.Value {
		if fail {
			return nil
		}
		if size++; size > maxDevalue || depth >= jsonx.MaxDepth || ref.Kind != jsonx.Int {
			fail = true
			return nil
		}
		if s, ok := special[ref.Text]; ok {
			return &jsonx.Value{Kind: jsonx.Const, Text: s}
		}
		i, err := strconv.Atoi(ref.Text)
		if err != nil || i < 0 || i >= len(arr) {
			fail = true
			return nil
		}
		if n, ok := open[i]; ok {
			return object(jsonx.Member{Key: "$ref", Value: str("$" + strings.Join(steps[:n], ""))})
		}
		open[i] = len(steps)
		depth++
		defer func() {
			delete(open, i)
			depth--
		}()
		x := arr[i]
		switch x.Kind {
		case jsonx.Object:
			o := object()
			for _, m := range x.Members {
				o.Members = append(o.Members, jsonx.Member{Key: m.Key, Value: at(m.Value, keyStep(m.Key))})
			}
			return o
		case jsonx.Array:
			if len(x.Items) > 0 && x.Items[0].Kind == jsonx.String {
				return typed(x.Items[0].Text, x.Items[1:], resolve, at)
			}
			a := &jsonx.Value{Kind: jsonx.Array}
			for n, item := range x.Items {
				a.Items = append(a.Items, at(item, indexStep(n)))
			}
			return a
		}
		return x
	}
	v = resolve(&jsonx.Value{Kind: jsonx.Int, Text: "0"})
	return v, !fail
}

// keyStep and indexStep are steps of a JSON path, as the comparison writes
// them.
func keyStep(key string) string { return jsonx.KeyPath("", key) } // the form compare writes
func indexStep(i int) string    { return "[" + strconv.Itoa(i) + "]" }

// typed decodes one of devalue's typed values: ["<type>", args…]. resolve
// decodes a value in place (a wrapper that is not in the path), at one
// further down the path.
func typed(name string, args []*jsonx.Value, resolve func(*jsonx.Value) *jsonx.Value, at func(*jsonx.Value, ...string) *jsonx.Value) *jsonx.Value {
	wrap := func(v *jsonx.Value) *jsonx.Value { return object(jsonx.Member{Key: name, Value: v}) }
	list := func(vs []*jsonx.Value) *jsonx.Value { return &jsonx.Value{Kind: jsonx.Array, Items: vs} }
	switch name {
	case "Reactive", "ShallowReactive", "Ref", "ShallowRef":
		if len(args) == 1 {
			return resolve(args[0])
		}
	case "Date", "RegExp", "BigInt", "URL", "URLSearchParams", "Object":
		// Arguments are literal values, not indexes.
		if len(args) == 1 {
			return wrap(args[0])
		}
		return wrap(list(args))
	case "Set":
		items := make([]*jsonx.Value, len(args))
		for i, a := range args {
			items[i] = at(a, keyStep(name), indexStep(i))
		}
		return wrap(list(items))
	case "Map":
		var pairs []*jsonx.Value
		for i := 0; i+1 < len(args); i += 2 {
			n := indexStep(i / 2)
			pairs = append(pairs, list([]*jsonx.Value{at(args[i], keyStep(name), n, indexStep(0)), at(args[i+1], keyStep(name), n, indexStep(1))}))
		}
		return wrap(list(pairs))
	case "null":
		// An object without a prototype: key, index, key, index…
		o := object()
		for i := 0; i+1 < len(args); i += 2 {
			o.Members = append(o.Members, jsonx.Member{Key: args[i].Text, Value: at(args[i+1], keyStep(args[i].Text))})
		}
		return o
	}
	// A reviver of the application or of Nuxt (NuxtError, EmptyRef, Island):
	// one index is resolved, anything else is kept as written.
	if len(args) == 1 && args[0].Kind == jsonx.Int {
		return wrap(at(args[0], keyStep(name)))
	}
	return wrap(list(args))
}
