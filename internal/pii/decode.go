package pii

import (
	"encoding/base64"
	"html"
	"slices"
	"strings"
	"unicode/utf8"
)

// maxDecode is how many layers of encoding a text is decoded through
// (jane%2540example.com is URL-encoded twice); maxViews bounds the decoded
// forms of one text.
const (
	maxDecode = 3
	maxViews  = 16
)

// window is how much text around an escape a long text is decoded with: in a
// page only the parts near an escape are decoded and read again, not the
// whole page once per decoding.
const window = 128

// minBase64 is the shortest run of base64 characters that is decoded, as in
// data verify.
const minBase64 = 16

// encoded reports a text that may hold an encoded value: a percent sign, an
// ampersand (an HTML entity) or a base64 run. Most texts hold none, and are
// not decoded.
func encoded(s string) bool {
	return strings.ContainsAny(s, "%&") || hasBase64(s)
}

func isBase64(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '-' || c == '_'
}

// hasBase64 reports a run of at least minBase64 base64 characters with upper-
// and lower-case letters (encoded text has both; a long word or a hex hash
// does not).
func hasBase64(s string) bool {
	run, upper, lower := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isBase64(c) {
			run, upper, lower = 0, false, false
			continue
		}
		run++
		upper = upper || c >= 'A' && c <= 'Z'
		lower = lower || c >= 'a' && c <= 'z'
		if run >= minBase64 && upper && lower {
			return true
		}
	}
	return false
}

// decodings are the decoded forms of s (not s itself): URL-decoded, with HTML
// entities resolved and its base64 runs that decode to text, each again up to
// maxDecode layers. Only a text with an escape that may matter is decoded,
// and a long one in parts: window bytes around each such escape, and each
// base64 run.
func decodings(s string) []string {
	ws := escapes(s)
	if len(ws) == 0 {
		return nil
	}
	if len(s) <= 4*window {
		return decodeLayers(s)
	}
	var out []string
	for _, w := range ws {
		out = append(out, decodeLayers(s[w[0]:w[1]])...)
	}
	return out
}

// escapes are the parts of s to decode, merged where they overlap: around an
// escape that may matter (see relevant), and base64 runs.
func escapes(s string) [][2]int {
	var all [][2]int
	add := func(start, end int) { all = append(all, [2]int{max(start, 0), min(end, len(s))}) }
	run, upper, lower := 0, false, false
	for i := 0; i <= len(s); i++ {
		if i < len(s) && isBase64(s[i]) {
			run++
			upper = upper || s[i] >= 'A' && s[i] <= 'Z'
			lower = lower || s[i] >= 'a' && s[i] <= 'z'
			continue
		}
		if run >= minBase64 && upper && lower {
			end := i
			for end < len(s) && s[end] == '=' {
				end++
			}
			add(i-run, end)
		}
		run, upper, lower = 0, false, false
		if i < len(s) && (s[i] == '%' || s[i] == '&') && relevant(s, i) {
			add(i-window, i+window)
		}
	}
	slices.SortFunc(all, func(a, b [2]int) int { return a[0] - b[0] })
	var out [][2]int
	for _, w := range all {
		if n := len(out); n > 0 && w[0] <= out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], w[1])
			continue
		}
		out = append(out, w)
	}
	return out
}

// relevant reports an escape at s[i] that stands for a character a pattern is
// made of (an ASCII letter or digit, or one of " .-_+@/()"), or for % or & of
// another layer of encoding. Others, &amp; &nbsp; &ndash; &quot; %C5%A0,
// change no match, and most escapes in a page are such.
func relevant(s string, i int) bool {
	if s[i] == '%' {
		if i+2 >= len(s) || unhex(s[i+1]) < 0 || unhex(s[i+2]) < 0 {
			return false
		}
		return patternChar(byte(unhex(s[i+1])<<4 | unhex(s[i+2])))
	}
	chunk := s[i:min(i+16, len(s))]
	u := html.UnescapeString(chunk)
	if u == chunk {
		return false
	}
	if strings.HasPrefix(chunk, "&amp;") {
		// Encoded twice (&amp;#64;) only when an entity follows.
		return len(chunk) > 5 && (chunk[5] == '#' || chunk[5] >= 'a' && chunk[5] <= 'z' || chunk[5] >= 'A' && chunk[5] <= 'Z')
	}
	return patternChar(u[0])
}

func patternChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(" .-_+@/()%&", c) >= 0
}

// decodeLayers decodes s through up to maxDecode layers, at most maxViews
// forms.
func decodeLayers(s string) []string {
	var out []string
	seen := map[string]bool{s: true}
	var add func(t string, depth int)
	add = func(t string, depth int) {
		if depth >= maxDecode {
			return
		}
		var next []string
		if strings.IndexByte(t, '%') >= 0 {
			next = append(next, percentDecode(t))
		}
		if strings.IndexByte(t, '&') >= 0 {
			next = append(next, html.UnescapeString(t))
		}
		if hasBase64(t) {
			next = append(next, base64Runs(t)...)
		}
		for _, u := range next {
			if seen[u] || len(out) >= maxViews {
				continue
			}
			seen[u] = true
			out = append(out, u)
			add(u, depth+1)
		}
	}
	add(s, 0)
	return out
}

// percentDecode resolves the valid %XX escapes of a text and keeps the rest
// ("100% of jane%40example.com"): a text is often only partly encoded.
func percentDecode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && unhex(s[i+1]) >= 0 && unhex(s[i+2]) >= 0 {
			b.WriteByte(byte(unhex(s[i+1])<<4 | unhex(s[i+2])))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// base64Runs decodes the runs of base64 (standard or URL alphabet) of at
// least minBase64 characters that decode to printable UTF-8 text.
func base64Runs(t string) []string {
	var out []string
	for i := 0; i < len(t); {
		if !isBase64(t[i]) {
			i++
			continue
		}
		j := i
		for j < len(t) && isBase64(t[j]) {
			j++
		}
		run := t[i:j]
		for j < len(t) && t[j] == '=' {
			j++
		}
		i = j
		if len(run) < minBase64 {
			continue
		}
		if len(run)%4 == 1 {
			run = run[:len(run)-1] // no base64 has this length: the run ends in a word
		}
		enc := base64.RawStdEncoding
		if strings.ContainsAny(run, "-_") {
			enc = base64.RawURLEncoding
		}
		if dec, err := enc.DecodeString(run); err == nil && printable(dec) {
			out = append(out, string(dec))
		}
	}
	return out
}

// printable reports UTF-8 text without control characters but line breaks
// and tabs.
func printable(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range string(b) {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}
