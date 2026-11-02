package credential

import (
	"strings"
	"testing"
)

// TestASCIIWords: an ASCII name is split as nameWords splits it, without
// allocating, so a check of every header name and key costs nothing.
func TestASCIIWords(t *testing.T) {
	for _, name := range []string{"content-type", "X-CSRF-Token", "XSRFToken", "currentPassword", "newPassword2",
		"access_token", "api-key", "v2ApiUserProfile3Settings", "__a__b__", "", "A", "AB", "ABc", "a1B2c3",
		"sec-websocket-key", "x-amz-server-side-encryption-customer-key-md5", "a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q.r"} {
		var buf [64]byte
		var ends [16]uint8
		n, ok := asciiWords(name, &buf, &ends)
		var got []string
		for i, start := 0, 0; i < n; i++ {
			got = append(got, string(buf[start:ends[i]]))
			start = int(ends[i])
		}
		if want := nameWords(name); ok && strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if n := testing.AllocsPerRun(100, func() { Name("content-type"); Key("XSRFToken"); Name("access_token") }); n != 0 {
		t.Errorf("%v allocations", n)
	}
}
