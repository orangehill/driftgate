package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

// TestMaskHeaderCredentials: headers that carry a credential outside
// Authorization and cookies are masked in the record, which goes into git,
// with or without X-, also in a URL's fragment; ordinary headers keep
// their values.
func TestMaskHeaderCredentials(t *testing.T) {
	for _, c := range [][3]string{
		{"x-csrf-token", "Xa8sd7F6a8sDf7as8d9Fq2w3", "Xx0xx0X0x0xXx0xx0x0Xx0x0"},
		{"x-xsrf-token", "abc", "xxx"},
		{"x-api-key", "sk_live_51Hx", "xx_xxxx_00Xx"},
		{"location", "/reset?token=8c2f3e1d&email=a", "/reset?token=0x0x0x0x&email=a"},
		{"location", "https://b.s3.amazonaws.com/a.pdf?X-Amz-Credential=AKIA%2F2024&X-Amz-Signature=fe5f80f7", "https://b.s3.amazonaws.com/a.pdf?X-Amz-Credential=XXXX%0X0000&X-Amz-Signature=xx0x00x0"},
		{"location", "https://app.test/reset-password/8c2f3e1d0a9b8c7d6e5f4a3b2c1d0e9f?email=a", "https://app.test/reset-password/0x0x0x0x0x0x0x0x0x0x0x0x0x0x0x0x?email=a"},
		{"link", `</items?page=2>; rel="next", </dl?signature=ab12>; rel="x"`, `</items?page=2>; rel="next", </dl?signature=xx00>; rel="x"`},
		{"location", "/items/550e8400-e29b-41d4-a716-446655440000?page=2#top", "/items/550e8400-e29b-41d4-a716-446655440000?page=2#top"},
		{"access-control-allow-credentials", "true", "true"},
		{"access-control-allow-headers", "Authorization, X-CSRF-Token", "Authorization, X-CSRF-Token"},
		{"etag", `"5d41402abc4b2a76b9719d911017c592"`, `"5d41402abc4b2a76b9719d911017c592"`},
		{"x-request-id", "Xa8sd7F6a8sDf7as8d9Fq2w3", "Xa8sd7F6a8sDf7as8d9Fq2w3"},
		// The fragment of an OAuth redirect or a route; a credential header
		// without X- (devise_token_auth); standard headers named like one.
		{"location", `https://app.test/cb#access_token=gho_16C7e42F292c6912E7710c838347Ae178B4a&token_type=bearer`, `https://app.test/cb#access_token=xxx_00X0x00X000x0000X0000x000000Xx000X0x&token_type=xxxxxx`},
		{"location", `/app#/reset?token=abc123&page=1`, `/app#/reset?token=xxx000&page=1`},
		{"location", `/cb#8c2f3e1d0a9b8c7d6e5f4a3b2c1d0e9f`, `/cb#0x0x0x0x0x0x0x0x0x0x0x0x0x0x0x0x`},
		{"access-token", `gho_16C7e42F292c6912E7710c838347Ae178B4a`, `xxx_00X0x00X000x0000X0000x000000Xx000X0x`},
		{"token", `Xa8sd7F6a8sDf7as8d9Fq2w3`, `Xx0xx0X0x0xXx0xx0x0Xx0x0`},
		{"api-key", `sk_live_51Hx`, `xx_xxxx_00Xx`},
		{"access-control-allow-origin", `https://app.test`, `https://app.test`},
		{"access-control-allow-credentials", `true`, `true`},
		{"public-key-pins", `pin-sha256="d6qzRu9zOECb90Uez27xWltNsj0e1Md7GkYYkVoZWmM="; max-age=5184000`, `pin-sha256="d6qzRu9zOECb90Uez27xWltNsj0e1Md7GkYYkVoZWmM="; max-age=5184000`},
		{"content-type", `application/json`, `application/json`},
	} {
		if got := MaskHeader(c[0], c[1]); got != c[2] {
			t.Errorf("%s: %s\n got %s\nwant %s", c[0], c[1], got, c[2])
		}
	}
}

// TestMasksWholeMatchesMaskHeader: MasksWhole(name) is true exactly for
// the headers whose every value MaskHeader masks whole (maskChars of the
// value); a header masked in part or not at all keeps some value as sent.
func TestMasksWholeMatchesMaskHeader(t *testing.T) {
	// Every name MaskHeader or MasksWhole names; a new case there goes
	// here too.
	names := []string{
		"authorization", "cookie", "proxy-authorization",
		"set-cookie", "location", "content-location", "refresh", "link",
		"x-csrf-token", "x-xsrf-token", "x-api-key", "api-key", "access-token", "token", "x-auth-token",
		"access-control-allow-origin", "access-control-allow-headers", "x-request-id", "accept", "x-custom",
	}
	for n := range plainHeaders {
		names = append(names, n)
	}
	values := []string{
		"Bearer abcDEF123",
		"laravel_session=eyJpdiI6Ik1; expires=Tue, 10-Nov-2026 12:00:00 GMT; Max-Age=7200; path=/; httponly; samesite=lax",
		"https://app.test/reset?token=8c2f3e1d&page=2",
		`<https://app.test/items?page=2>; rel="next"`,
		"",
	}
	for _, n := range names {
		whole := true
		for _, v := range values {
			whole = whole && MaskHeader(n, v) == maskChars(v)
		}
		if whole != MasksWhole(n) {
			t.Errorf("%s: MaskHeader masks every value whole: %v, MasksWhole: %v", n, whole, MasksWhole(n))
		}
	}
}

// TestSplitSetCookie: the name is up to the first = before the first ;,
// attrs keep that ; and everything after it as sent, and the three parts
// joined with = are the value again; without = before the first ; it is
// not a cookie.
func TestSplitSetCookie(t *testing.T) {
	for _, c := range []struct {
		in, name, value, attrs string
		ok                     bool
	}{
		{"a=b", "a", "b", "", true},
		{"a=b; Path=/; HttpOnly", "a", "b", "; Path=/; HttpOnly", true},
		{"a=; Max-Age=0", "a", "", "; Max-Age=0", true},
		{"a=b=c==;", "a", "b=c==", ";", true},
		{` a = "q;r"`, " a ", ` "q`, `;r"`, true},
		{"=b", "", "b", "", true},
		{"flag; a=b", "", "", "", false},
		{"", "", "", "", false},
	} {
		name, value, attrs, ok := SplitSetCookie(c.in)
		if name != c.name || value != c.value || attrs != c.attrs || ok != c.ok {
			t.Errorf("%q: got %q %q %q %v", c.in, name, value, attrs, ok)
		}
		if ok && name+"="+value+attrs != c.in {
			t.Errorf("%q: parts do not join back", c.in)
		}
	}
}

// TestCatalogHeaderReplacesDefault: a catalog header replaces the default
// of its name instead of going next to it (Accept-Encoding: gzip and
// identity at once); more of one name in the catalog are all sent.
func TestCatalogHeaderReplacesDefault(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	c, err := New(Options{BaseURL: srv.URL, Allow: mustAllow(t)})
	if err != nil {
		t.Fatal(err)
	}
	c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/", Headers: [][2]string{
		{"accept-encoding", "gzip"}, {"User-Agent", "app/2"}, {"X-Tag", "a"}, {"x-tag", "b"}}}, "")
	if ae, ua, tags := got.Values("Accept-Encoding"), got.Values("User-Agent"), got.Values("X-Tag"); strings.Join(ae, ",") != "gzip" ||
		strings.Join(ua, ",") != "app/2" || strings.Join(tags, ",") != "a,b" {
		t.Fatalf("Accept-Encoding %q, User-Agent %q, X-Tag %q", ae, ua, tags)
	}
}

// TestCheckRedirect: a redirect never goes from https to http, and one to
// another host takes neither the token nor the catalog's headers along.
func TestCheckRedirect(t *testing.T) {
	first := &http.Request{URL: &url.URL{Scheme: "https", Host: "api.test"}}
	next := func(raw string) *http.Request {
		u, _ := url.Parse(raw)
		return &http.Request{URL: u, Header: http.Header{"Authorization": {"Bearer t"}, "X-Api-Key": {"k"}, "Accept": {"*/*"}, "User-Agent": {"driftgate/1"}}}
	}
	if err := checkRedirect(next("http://api.test/x"), []*http.Request{first}); err == nil {
		t.Fatal("followed https → http")
	}
	same := next("https://api.test/y")
	if err := checkRedirect(same, []*http.Request{first}); err != nil || same.Header.Get("Authorization") == "" || same.Header.Get("X-Api-Key") == "" {
		t.Fatalf("same host: %v %v", err, same.Header)
	}
	for _, raw := range []string{"https://cdn.test/z", "https://sub.api.test/z", "https://api.test:8443/z"} {
		other := next(raw)
		if err := checkRedirect(other, []*http.Request{first}); err != nil {
			t.Fatal(err)
		}
		if other.Header.Get("Authorization") != "" || other.Header.Get("X-Api-Key") != "" || other.Header.Get("User-Agent") == "" || other.Header.Get("Accept") == "" {
			t.Fatalf("%s: headers %v", raw, other.Header)
		}
	}
	var via []*http.Request
	for range 10 {
		via = append(via, first)
	}
	if err := checkRedirect(next("https://api.test/"), via); err == nil {
		t.Fatal("more than 10 redirects")
	}
}
