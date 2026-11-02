package glob

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		p, n string
		want bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"mut-api-*", "mut-api-login", true},
		{"mut-api-*", "pub-news", false},
		{"$.body.token.token", "$.body.token.token", true},
		{"$.data[0].id", "$.data[0].id", true},
		{"$.data[0].id", "$.data0.id", false},
		{"$.data[*].id", "$.data[12].id", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"*.created_at", "$.entry.created_at", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"é?", "éa", true},
	}
	for _, c := range cases {
		if got := Match(c.p, c.n); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.p, c.n, got, c.want)
		}
	}
}

// The byte matcher for ASCII gives the answers of the rune matcher.
func TestMatchASCII(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	text := func(alphabet string) string {
		b := make([]byte, rnd.IntN(7))
		for i := range b {
			b[i] = alphabet[rnd.IntN(len(alphabet))]
		}
		return string(b)
	}
	for range 200000 {
		p, n := text("ab*?."), text("ab.")
		if match(p, n) != matchRunes([]rune(p), []rune(n)) {
			t.Fatalf("Match(%q, %q) differs", p, n)
		}
	}
}

// A name a pattern matches starts and ends as the pattern does.
func TestLiteral(t *testing.T) {
	rnd := rand.New(rand.NewPCG(3, 4))
	text := func(alphabet string, max int) string {
		b := make([]rune, rnd.IntN(max))
		a := []rune(alphabet)
		for i := range b {
			b[i] = a[rnd.IntN(len(a))]
		}
		return string(b)
	}
	for range 200000 {
		p, n := text("ab*?é", 6), text("abé", 6)
		start, end := Literal(p)
		if Match(p, n) && (!strings.HasPrefix(n, start) || !strings.HasSuffix(n, end)) {
			t.Fatalf("%q matches %q, which does not start with %q and end with %q", p, n, start, end)
		}
	}
	for p, want := range map[string][2]string{"$.a.b": {"$.a.b", "$.a.b"}, "$.items[*].id": {"$.items[", "].id"}, "*.id": {"", ".id"}, "xlsx:S!?1*": {"xlsx:S!", ""}} {
		if start, end := Literal(p); start != want[0] || end != want[1] {
			t.Errorf("Literal(%q) = %q, %q", p, start, end)
		}
	}
}
