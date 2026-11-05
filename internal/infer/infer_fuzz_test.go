package infer

import (
	"regexp"
	"strings"
	"testing"
)

// FuzzPattern: a format compiles and accepts every value it was made from,
// and is "" only when the values are all equal; values are split at \x01.
func FuzzPattern(f *testing.F) {
	for _, s := range []string{"a1\x01b2", "v1.2.3\x01v1.2.4", "job_999\x01job_1000", "ERR_500\x01ERR_501", strings.Repeat("9", 1500) + "\x01" + strings.Repeat("8", 1200)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		vs := strings.Split(s, "\x01")
		p := Pattern(vs)
		if p == "" {
			for _, v := range vs {
				if v != vs[0] {
					t.Fatalf("empty for differing %q", vs)
				}
			}
			return
		}
		re, err := regexp.Compile("^(?:" + p + ")$")
		if err != nil {
			t.Fatalf("does not compile: %q -> %q: %v", vs, p, err)
		}
		for _, v := range vs {
			if !re.MatchString(v) {
				t.Fatalf("rejects seen %q (%q)", v, p)
			}
		}
	})
}
