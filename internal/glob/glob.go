// Package glob matches request IDs, JSON paths and routes against simple
// patterns: '*' matches any run of characters (including none), '?' matches
// one character, and everything else, brackets included, matches itself.
package glob

import "strings"

// Match reports whether name matches pattern.
func Match(pattern, name string) bool {
	// Paths and IDs are nearly always ASCII: then a byte is a character and
	// the strings are matched as they are, without converting them.
	if ascii(pattern) && ascii(name) {
		return match(pattern, name)
	}
	return matchRunes([]rune(pattern), []rune(name))
}

// Literal returns the start and the end of pattern around its wildcards:
// every name the pattern matches starts with prefix and ends with suffix.
// A pattern without wildcards is both.
func Literal(pattern string) (prefix, suffix string) {
	i := strings.IndexAny(pattern, "*?")
	if i < 0 {
		return pattern, pattern
	}
	return pattern[:i], pattern[strings.LastIndexAny(pattern, "*?")+1:]
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// match and matchRunes are the same matcher, over bytes and over runes.
func match(p, n string) bool {
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]) && p[pi] != '*':
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func matchRunes(p, n []rune) bool {
	pi, ni := 0, 0
	star, mark := -1, 0
	for ni < len(n) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]) && p[pi] != '*':
			pi++
			ni++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ni
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ni = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
