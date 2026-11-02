package jsonx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// At finds the value at a simple JSON path: $ followed by .key, ["key"] and
// [index] steps ($.entry.id, $.entries[0].slug, $.users["a.b@c.d"]), the
// form KeyPath writes. With duplicate keys the first one counts.
func (v *Value) At(path string) (*Value, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("path %q must start with $", path)
	}
	cur, rest := v, path[1:]
	for rest != "" {
		switch rest[0] {
		case '.':
			end := strings.IndexAny(rest[1:], ".[")
			if end < 0 {
				end = len(rest) - 1
			}
			key := rest[1 : end+1]
			rest = rest[end+1:]
			if cur.Kind != Object {
				return nil, fmt.Errorf("%s: not an object at .%s", path, key)
			}
			next := cur.member(key)
			if next == nil {
				return nil, fmt.Errorf("%s: no key %q", path, key)
			}
			cur = next
		case '[':
			if strings.HasPrefix(rest, `["`) {
				key, n, err := quotedKey(rest[1:])
				if err != nil {
					return nil, fmt.Errorf("%s: %v", path, err)
				}
				rest = rest[1+n:]
				if !strings.HasPrefix(rest, "]") {
					return nil, fmt.Errorf("%s: unclosed [", path)
				}
				rest = rest[1:]
				next := cur.member(key)
				if next == nil {
					return nil, fmt.Errorf("%s: no key %q", path, key)
				}
				cur = next
				continue
			}
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("%s: unclosed [", path)
			}
			i, err := strconv.Atoi(rest[1:end])
			if err != nil || cur.Kind != Array || i < 0 || i >= len(cur.Items) {
				return nil, fmt.Errorf("%s: no item %s", path, rest[:end+1])
			}
			cur, rest = cur.Items[i], rest[end+1:]
		default:
			return nil, fmt.Errorf("%s: unexpected %q", path, rest[0])
		}
	}
	return cur, nil
}

// plainKey reports a key written as .key in a path ([A-Za-z0-9_$-]+); any
// other key is written as ["key"], so a key with a dot is not mistaken for
// two levels and a key that is data (an e-mail address used as a key) can
// be told apart.
func plainKey(key string) bool {
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || c == '$' || c == '-') {
			return false
		}
	}
	return key != ""
}

// KeyPath is the path of a member of the object at path.
func KeyPath(path, key string) string {
	if plainKey(key) {
		return path + "." + key
	}
	q, _ := json.Marshal(key)
	return path + "[" + string(q) + "]"
}

// member is the first member with the key, or nil (also when v is no object).
func (v *Value) member(key string) *Value {
	if v.Kind != Object {
		return nil
	}
	for _, m := range v.Members {
		if m.Key == key {
			return m.Value
		}
	}
	return nil
}

// quotedKey reads the JSON string s starts with, and how many bytes it took.
func quotedKey(s string) (string, int, error) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var key string
			if err := json.Unmarshal([]byte(s[:i+1]), &key); err != nil {
				return "", 0, err
			}
			return key, i + 1, nil
		}
	}
	return "", 0, fmt.Errorf("unclosed string in %s", s)
}
