package compare

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/jsonx"
)

// The mutation test plants one change of every kind at every place it fits in
// every corpus document and requires DriftGate to report it at that place.
// The built-in corpus is testdata/corpus; DRIFTGATE_MUTATION_CORPUS adds a
// directory of real response bodies (kept outside the repository).

type node struct {
	path  string
	steps []int // member or item index from the root
	value *jsonx.Value
}

func nodes(v *jsonx.Value) []node {
	var out []node
	var visit func(path string, steps []int, v *jsonx.Value)
	visit = func(path string, steps []int, v *jsonx.Value) {
		out = append(out, node{path, append([]int{}, steps...), v})
		for i, m := range v.Members {
			visit(jsonx.KeyPath(path, m.Key), append(steps, i), m.Value)
		}
		for i, item := range v.Items {
			visit(fmt.Sprintf("%s[%d]", path, i), append(steps, i), item)
		}
	}
	visit("$", nil, v)
	return out
}

func locate(root *jsonx.Value, steps []int) *jsonx.Value {
	v := root
	for _, s := range steps {
		if v.Kind == jsonx.Object {
			v = v.Members[s].Value
		} else {
			v = v.Items[s]
		}
	}
	return v
}

func hasDuplicateKeys(v *jsonx.Value) bool {
	seen := map[string]bool{}
	for _, m := range v.Members {
		if seen[m.Key] {
			return true
		}
		seen[m.Key] = true
	}
	return false
}

var sqlDateTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)

// mutation changes the node in place and returns the expected drift, or
// ok=false when it does not apply to this node.
type mutation struct {
	name  string
	apply func(n node, v *jsonx.Value) (kind, where string, ok bool)
}

var mutations = []mutation{
	{"type", func(n node, v *jsonx.Value) (string, string, bool) {
		switch v.Kind {
		case jsonx.String:
			*v = jsonx.Value{Kind: jsonx.Int, Text: "7"}
		case jsonx.Int, jsonx.Float, jsonx.Bool, jsonx.Null:
			*v = jsonx.Value{Kind: jsonx.String, Text: "7", Raw: `"7"`}
		default:
			return "", "", false
		}
		return KindType, n.path, true
	}},
	{"int-to-float", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Int {
			return "", "", false
		}
		*v = jsonx.Value{Kind: jsonx.Float, Text: v.Text + ".0"}
		return KindType, n.path, true
	}},
	{"number-literal", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Float || strings.ContainsAny(v.Text, "eE") {
			return "", "", false
		}
		v.Text += "0"
		return KindNumberLiteral, n.path, true
	}},
	{"value", func(n node, v *jsonx.Value) (string, string, bool) {
		switch v.Kind {
		case jsonx.String:
			v.Text += "!"
			v.Raw = jsonx.Quote(v.Text)
		case jsonx.Int:
			if v.Text = strings.TrimPrefix(v.Text, "-") + "1"; v.Text == "01" {
				v.Text = "1"
			}
		case jsonx.Bool:
			v.Bool = !v.Bool
		default:
			return "", "", false
		}
		return KindValue, n.path, true
	}},
	{"date-format", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.String || !sqlDateTime.MatchString(v.Text) {
			return "", "", false
		}
		v.Text = strings.Replace(v.Text, " ", "T", 1) + ".000000Z"
		v.Raw = jsonx.Quote(v.Text)
		return KindValue, n.path, true
	}},
	{"key-missing", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Object || len(v.Members) == 0 || hasDuplicateKeys(v) {
			return "", "", false
		}
		key := v.Members[len(v.Members)-1].Key
		v.Members = v.Members[:len(v.Members)-1]
		return KindKeyMissing, n.path + "." + key, true
	}},
	{"key-extra", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Object {
			return "", "", false
		}
		v.Members = append(v.Members, jsonx.Member{Key: "__driftgate_extra", Value: &jsonx.Value{Kind: jsonx.Null}})
		return KindKeyExtra, n.path + ".__driftgate_extra", true
	}},
	{"key-order", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Object || len(v.Members) < 2 || v.Members[0].Key == v.Members[1].Key {
			return "", "", false
		}
		v.Members[0], v.Members[1] = v.Members[1], v.Members[0]
		return KindKeyOrder, n.path, true
	}},
	{"array-grow", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Array {
			return "", "", false
		}
		v.Items = append(v.Items, &jsonx.Value{Kind: jsonx.Null})
		return KindArrayLength, n.path, true
	}},
	{"array-shrink", func(n node, v *jsonx.Value) (string, string, bool) {
		if v.Kind != jsonx.Array || len(v.Items) == 0 {
			return "", "", false
		}
		v.Items = v.Items[:len(v.Items)-1]
		return KindArrayLength, n.path, true
	}},
	{"container-swap", func(n node, v *jsonx.Value) (string, string, bool) {
		switch v.Kind {
		case jsonx.Object:
			*v = jsonx.Value{Kind: jsonx.Array, Items: []*jsonx.Value{}}
		case jsonx.Array:
			*v = jsonx.Value{Kind: jsonx.Object, Members: []jsonx.Member{}}
		default:
			return "", "", false
		}
		return KindType, n.path, true
	}},
}

func corpus(t *testing.T) map[string][]byte {
	docs := map[string][]byte{}
	dirs := []string{filepath.Join("testdata", "corpus")}
	if extra := os.Getenv("DRIFTGATE_MUTATION_CORPUS"); extra != "" {
		dirs = append(dirs, extra)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("corpus %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jsonx.Parse(raw); err == nil {
				docs[filepath.Join(dir, e.Name())] = raw
			}
		}
	}
	if len(docs) == 0 {
		t.Fatal("empty corpus")
	}
	return docs
}

// maxPerKind bounds the work on large documents: at most this many places per
// mutation kind and document, spread evenly over the document.
const maxPerKind = 200

func TestMutationsAreCaught(t *testing.T) {
	allow := allowlist.Empty()
	planted, missed := map[string]int{}, map[string]int{}
	for name, raw := range corpus(t) {
		root, _ := jsonx.Parse(raw)
		base := jsonx.Marshal(root, false)
		all := nodes(root)
		if ds := One("doc", side(200, nil, string(base)), side(200, nil, string(base)), allow, LevelValues); len(ds) != 0 {
			t.Fatalf("%s: unchanged document reported: %+v", name, ds)
		}
		for _, m := range mutations {
			var fits []node
			for _, n := range all {
				probe := n.value.Clone()
				if _, _, ok := m.apply(n, probe); ok {
					fits = append(fits, n)
				}
			}
			step := 1
			if len(fits) > maxPerKind {
				step = len(fits) / maxPerKind
			}
			for i := 0; i < len(fits); i += step {
				n := fits[i]
				mutated := root.Clone()
				kind, where, _ := m.apply(n, locate(mutated, n.steps))
				cur := jsonx.Marshal(mutated, false)
				planted[m.name]++
				ds := One("doc", side(200, nil, string(base)), side(200, nil, string(cur)), allow, LevelValues)
				if !has(ds, kind, where) {
					missed[m.name]++
					if missed[m.name] <= 3 {
						t.Errorf("%s: %s at %s not caught as %s; got %+v", name, m.name, n.path, kind, ds)
					}
				}
			}
		}
		// Same values, different layout.
		planted["layout"]++
		spaced := jsonx.Marshal(root, true)
		if ds := One("doc", side(200, nil, string(base)), side(200, nil, string(spaced)), allow, LevelValues); !has(ds, KindEncoding, "$") && string(spaced) != string(base) {
			missed["layout"]++
			t.Errorf("%s: layout change not caught", name)
		}
	}
	for _, m := range append(mutations, mutation{name: "layout"}) {
		t.Logf("%-15s planted %6d  missed %d", m.name, planted[m.name], missed[m.name])
	}
}
