package credential

import (
	"net/url"
	"strings"
)

// MaskURL masks the credentials in a URL, absolute or relative, keeping
// everything else as it is written: the value of every query parameter
// named as a credential (token=, signature=, X-Amz-Signature=) or shaped
// like one, the same in the fragment (an OAuth redirect's
// #access_token=, a route's #/reset?token=) or a fragment shaped like one
// as a whole, and with paths every path segment shaped like one (a reset
// link's token). mask turns a value into its masked form. changed reports
// whether anything was masked.
func MaskURL(u string, paths bool, mask func(string) string) (masked string, changed bool) {
	rest, frag, hasFrag := strings.Cut(u, "#")
	path, query, hasQuery := strings.Cut(rest, "?")
	if paths {
		start := 0
		if i := strings.Index(path, "://"); i >= 0 {
			if j := strings.IndexByte(path[i+3:], '/'); j >= 0 {
				start = i + 3 + j
			} else {
				start = len(path)
			}
		}
		segs := strings.Split(path[start:], "/")
		for i, s := range segs {
			if d, err := url.PathUnescape(s); err == nil && Value(d) {
				segs[i], changed = mask(s), true
			}
		}
		path = path[:start] + strings.Join(segs, "/")
	}
	if hasQuery {
		var ch bool
		query, ch = maskPairs(query, mask)
		changed = changed || ch
	}
	if hasFrag {
		var ch bool
		if route, q, ok := strings.Cut(frag, "?"); ok {
			q, ch = maskPairs(q, mask)
			frag = route + "?" + q
		} else if strings.IndexByte(frag, '=') >= 0 {
			frag, ch = maskPairs(frag, mask)
		} else if d, err := url.PathUnescape(frag); err == nil && Value(d) {
			frag, ch = mask(frag), true
		}
		changed = changed || ch
	}
	if !changed {
		return u, false
	}
	masked = path
	if hasQuery {
		masked += "?" + query
	}
	if hasFrag {
		masked += "#" + frag
	}
	return masked, true
}

// maskPairs masks the values of the name=value pairs of a query named or
// shaped as a credential.
func maskPairs(query string, mask func(string) string) (string, bool) {
	changed := false
	params := strings.Split(query, "&")
	for i, p := range params {
		name, value, ok := strings.Cut(p, "=")
		if !ok || value == "" {
			continue
		}
		if Name(unescape(name)) || Value(unescape(value)) {
			params[i], changed = name+"="+mask(value), true
		}
	}
	if !changed {
		return query, false
	}
	return strings.Join(params, "&"), true
}

// unescape is a query name or value as it is read; as written when it
// does not decode.
func unescape(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

// InQuery reports a URL with a query parameter named as a credential that
// has a value (a signed or presigned URL, a link with its token), also in
// the fragment (an OAuth redirect's #access_token=), or with a JWT
// anywhere. A value shaped like a token under another name, or in the
// path, is not taken: the random names of stored files look the same.
func InQuery(u string) bool {
	if JWT(u) {
		return true
	}
	rest, frag := u, ""
	if i := strings.IndexByte(u, '#'); i >= 0 {
		rest, frag = u[:i], u[i+1:]
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 && namedPair(rest[i+1:]) {
		return true
	}
	if i := strings.IndexByte(frag, '?'); i >= 0 {
		frag = frag[i+1:]
	}
	return namedPair(frag)
}

// namedPair reports a name=value pair of a query named as a credential.
func namedPair(query string) bool {
	start, eq, escaped := 0, -1, false
	for i := 0; i <= len(query); i++ {
		if i < len(query) && query[i] != '&' {
			switch c := query[i]; {
			case eq >= 0:
			case c == '=':
				eq = i
			case c == '%' || c == '+':
				escaped = true
			}
			continue
		}
		if eq >= 0 && eq+1 < i { // a pair with a value
			name := query[start:eq]
			if escaped {
				name = unescape(name)
			}
			if Name(name) {
				return true
			}
		}
		start, eq, escaped = i+1, -1, false
	}
	return false
}
