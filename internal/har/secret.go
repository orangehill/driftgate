package har

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/orangehill/driftgate/internal/credential"
)

// nameMatcher tells the names of parameters, keys and form fields that hold
// credentials (credential.Name).
type nameMatcher struct{}

func (nameMatcher) MatchString(name string) bool { return credential.Name(name) }

// secretName matches parameter and key names that hold credentials.
var secretName nameMatcher

// secretValue reports a value shaped like a credential (credential.Value).
func secretValue(v string) bool { return credential.Value(v) }

// secretParam finds a query parameter that carries a credential, by its
// name or its value; it returns the name, or what to say about it, never
// the value.
func secretParam(q url.Values) string {
	for _, name := range sortedKeys(q) {
		if secretName.MatchString(name) {
			return name
		}
		for _, v := range q[name] {
			if secretValue(v) {
				return "a token in " + name
			}
		}
	}
	return ""
}

// secretKey finds a key that holds a credential, or a string value shaped
// like one, anywhere in a JSON body.
func secretKey(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	var walk func(key string, v any) string
	walk = func(key string, v any) string {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(x) {
				if secretName.MatchString(k) {
					return k
				}
				if s := walk(k, x[k]); s != "" {
					return s
				}
			}
		case []any:
			for _, val := range x {
				if s := walk(key, val); s != "" {
					return s
				}
			}
		case string:
			if secretValue(x) {
				if key == "" {
					return "a token"
				}
				return "a token in " + key
			}
		}
		return ""
	}
	return walk("", v)
}

// secretPath returns the path with every segment shaped like a credential
// (a reset link's token, a JWT) replaced by {token}, or "" when it has
// none. The masked path names the request in a note without the token.
func secretPath(p string) string {
	segs := strings.Split(p, "/")
	found := false
	for i, s := range segs {
		if d, err := url.PathUnescape(s); err == nil {
			s = d
		}
		if secretValue(s) {
			segs[i], found = "{token}", true
		}
	}
	if !found {
		return ""
	}
	return strings.Join(segs, "/")
}

// MaskPath is a path as a note or the live output may print it: every
// segment shaped like a credential replaced by {token} and every one that
// holds personal data by {personal}; a path whose personal data no single
// segment holds is printed as /{personal}.
func MaskPath(p string) string {
	if len(detector.Scan([]byte(p))) == 0 {
		if masked := secretPath(p); masked != "" {
			return masked
		}
		return p
	}
	segs := strings.Split(p, "/")
	found := false
	for i, s := range segs {
		if d, err := url.PathUnescape(s); err == nil {
			s = d
		}
		switch {
		case secretValue(s):
			segs[i] = "{token}"
		case len(detector.Scan([]byte(s))) > 0:
			segs[i], found = "{personal}", true
		}
	}
	if !found {
		return "/{personal}"
	}
	return strings.Join(segs, "/")
}
