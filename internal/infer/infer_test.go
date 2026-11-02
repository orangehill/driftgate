package infer

import (
	"regexp"
	"testing"
)

func TestPattern(t *testing.T) {
	cases := []struct {
		values []string
		want   string
	}{
		{[]string{"2026-10-29 10:00:01", "2026-10-29 10:00:07"}, `\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`},
		{[]string{"Wed, 29 Oct 2026 09:00:01 GMT", "Wed, 29 Oct 2026 09:00:02 GMT"}, `[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT`},
		{[]string{"9", "10"}, `-?\d+`},
		{[]string{"-5", "23"}, `-?\d+`},
		{[]string{"1.5", "12"}, `-?\d+(?:\.\d+)?`},
		{[]string{"3f9a.c01d", "77b2.e4ff"}, `[0-9a-f]{4}\.[0-9a-f]{4}`},
		{[]string{"XSRF-TOKEN=ab12; path=/", "XSRF-TOKEN=cd34; path=/"}, `XSRF-TOKEN=[0-9a-f]{4}; path=/`},
		{[]string{"2026-10-29T10:27:08.896Z", "2026-10-29T10:27:09.112Z"}, `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`},
		{[]string{"eyJ.a-b_c.x1", "eyJ.ab.c-d_e2"}, `[0-9A-Za-z\-._]+`},
		{[]string{"11bca30b-0308-4725-9d4e-6f1a2b3c4d5e", "7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d"}, uuidFormat},
		{[]string{"/orders/11BCA30B-0308-4725-9D4E-6F1A2B3C4D5E?x=1", "/orders/7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d?x=2"}, `/orders/` + uuidFormat + `\?x=\d`},
		{[]string{"same", "same"}, ``},
		{[]string{"abc", ""}, `[a-z]*`},
		{[]string{"", "2024-01-01"}, `[0-9\-]*`},
		{nil, ``},
	}
	for _, c := range cases {
		got := Pattern(c.values)
		if got != c.want {
			t.Errorf("Pattern(%q) = %q, want %q", c.values, got, c.want)
			continue
		}
		if got == "" {
			continue
		}
		re := regexp.MustCompile("^(?:" + got + ")$")
		for _, v := range c.values {
			if !re.MatchString(v) {
				t.Errorf("%q does not match %q", got, v)
			}
		}
	}
}

// A UUID group that holds digits only in every sample must not become \d{4}:
// the next UUID has letters there.
func TestPatternUUIDAnyGroup(t *testing.T) {
	got := Pattern([]string{"11bca30b-0308-4725-9d4e-6f1a2b3c4d5e", "7e2f0c41-5521-4725-8c3d-0e9f8a7b6c5d"})
	re := regexp.MustCompile("^(?:" + got + ")$")
	for _, v := range []string{"0b5c1e2a-af3d-4bc1-a9e8-77f1d2c3b4a5", "0B5C1E2A-AF3D-4BC1-A9E8-77F1D2C3B4A5"} {
		if !re.MatchString(v) {
			t.Errorf("%q does not match %q", got, v)
		}
	}
}
