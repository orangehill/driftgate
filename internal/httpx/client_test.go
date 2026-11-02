package httpx

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

func client(t *testing.T, url string, allow []string, follow bool) *Client {
	a, err := NewAllow(allow, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{BaseURL: url, Allow: a, FollowRedirects: follow})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRequestAndResponse(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Set-Cookie", "laravel_session=AbC123; path=/; httponly")
		w.Header().Add("Vary", "b")
		w.Header().Add("Vary", "a")
		w.(http.Flusher).Flush() // forces chunked transfer encoding
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := client(t, srv.URL, nil, false)
	res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "POST", Path: "/news/{id}", Params: map[string]string{"id": "5"},
		Headers: [][2]string{{"Accept", "application/json"}}, Body: []byte(`{"a":1}`), Storage: "public"}, "tok")
	if res.Record.Error != "" {
		t.Fatal(res.Record.Error)
	}
	if got.URL.Path != "/news/5" || got.Header.Get("User-Agent") != DefaultUserAgent || got.Header.Get("Authorization") != "Bearer tok" ||
		got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Accept-Encoding") != "identity" || !got.Close {
		t.Fatalf("request: %s %v close=%v", got.URL.Path, got.Header, got.Close)
	}
	h := map[string][]string{}
	for _, p := range res.Record.Headers {
		h[p[0]] = append(h[p[0]], p[1])
	}
	if h["set-cookie"][0] != "laravel_session=XxX000; path=/; httponly" {
		t.Errorf("cookie not masked: %v", h["set-cookie"])
	}
	if strings.Join(h["vary"], ",") != "b,a" || h["transfer-encoding"] == nil || strings.Join(h["connection"], ",") != "close" {
		t.Errorf("headers: %v", h)
	}
	if string(res.Body) != `{"ok":true}` {
		t.Errorf("body %q", res.Body)
	}
}

func TestHostNotAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := client(t, srv.URL, []string{"10.0.0.0/8"}, false)
	res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/", Storage: "public"}, "")
	if !strings.Contains(res.Record.Error, ErrHostNotAllowed.Error()) {
		t.Fatalf("connection to a host outside the allow-list: %q", res.Record.Error)
	}
	if !errors.Is(ErrHostNotAllowed, ErrHostNotAllowed) {
		t.Fatal()
	}
}

func TestRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		w.Write([]byte("new"))
	}))
	defer srv.Close()
	req := catalog.Request{ID: "x", Method: "GET", Path: "/old", Storage: "public"}
	if res := client(t, srv.URL, nil, false).Do(context.Background(), req, ""); res.Record.Status != http.StatusFound {
		t.Fatalf("redirect followed by default: %d", res.Record.Status)
	}
	if res := client(t, srv.URL, nil, true).Do(context.Background(), req, ""); res.Record.Status != 200 {
		t.Fatalf("follow_redirects: %d", res.Record.Status)
	}
	// A redirect to a host outside the allow-list is refused.
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.1/", http.StatusFound) // TEST-NET-3, no DNS lookup
	}))
	defer evil.Close()
	if res := client(t, evil.URL, nil, true).Do(context.Background(), req, ""); !strings.Contains(res.Record.Error, "not allowed") {
		t.Fatalf("redirect to an outside host: %+v", res.Record)
	}
}

func TestUnfilledParameter(t *testing.T) {
	c := client(t, "http://localhost:1", nil, false)
	if res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/news/{id}", Storage: "public"}, ""); !strings.Contains(res.Record.Error, "unfilled") {
		t.Fatalf("got %+v", res.Record)
	}
}

// TestPathKeepsTheHost: a path that does not start with "/" would change the
// host of the URL, and the identity's token would go there.
func TestPathKeepsTheHost(t *testing.T) {
	var stolen string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen = r.Header.Get("Authorization")
	}))
	defer evil.Close()
	evilHost := strings.TrimPrefix(evil.URL, "http://")
	c := client(t, "http://127.0.0.1:1", nil, false)
	for _, p := range []string{"@" + evilHost + "/steal", ":" + evilHost[strings.LastIndex(evilHost, ":")+1:] + "/steal", ".example.test/x", "x"} {
		res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: p, Storage: "public"}, "tok")
		if res.Record.Error == "" || !strings.Contains(res.Record.Error, "must start with /") {
			t.Errorf("%s: %+v", p, res.Record)
		}
	}
	if stolen != "" {
		t.Fatalf("the token reached another host: %q", stolen)
	}
}

// TestErrorWithoutURL: a failed request is recorded without its URL, whose
// path may hold a value from a private body.
func TestErrorWithoutURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // the connection is refused
	c := client(t, base, nil, false)
	res := c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/u/{e}", Params: map[string]string{"e": "ana@example.com"}, Storage: "public"}, "")
	if res.Record.Error == "" || strings.Contains(res.Record.Error, "ana") || strings.Contains(res.Record.Error, "/u/") {
		t.Fatalf("error: %q", res.Record.Error)
	}
	if !strings.HasPrefix(res.Record.Error, "Get: ") {
		t.Errorf("error: %q", res.Record.Error)
	}
	// The unfilled parameter is named in the path as the catalog writes it.
	res = c.Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/u/{e}/{f}", Params: map[string]string{"e": "ana@example.com"}, Storage: "public"}, "")
	if !strings.Contains(res.Record.Error, "/u/{e}/{f}") || strings.Contains(res.Record.Error, "ana") {
		t.Errorf("unfilled: %q", res.Record.Error)
	}
}

// A body of known length is read into a buffer of its size, not one grown
// to up to twice it; a body without a length is read as well.
func TestReadBodySize(t *testing.T) {
	body := strings.Repeat("x", 3<<20+7)
	for _, chunked := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !chunked {
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			}
			w.Write([]byte(body))
		}))
		res := client(t, srv.URL, nil, false).Do(context.Background(), catalog.Request{ID: "x", Method: "GET", Path: "/"}, "")
		srv.Close()
		if res.Record.Error != "" || string(res.Body) != body {
			t.Fatalf("chunked %v: error %q, %d bytes", chunked, res.Record.Error, len(res.Body))
		}
		if !chunked && cap(res.Body) > len(body)+bytes.MinRead {
			t.Fatalf("a body of %d bytes held in %d", len(body), cap(res.Body))
		}
	}
}
