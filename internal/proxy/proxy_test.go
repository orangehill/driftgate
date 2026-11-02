package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/httpx"
)

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8088": true, "127.0.0.2:1": true, "[::1]:8088": true, "localhost:8088": true,
		":8088": false, "0.0.0.0:8088": false, "[::]:8088": false, "192.168.1.5:8088": false, "example.com:80": false, "127.0.0.1": false,
	} {
		if Loopback(addr) != want {
			t.Errorf("Loopback(%q) = %v", addr, !want)
		}
	}
}

type recorder struct {
	mu   sync.Mutex
	seen []Exchange
}

func (r *recorder) add(e Exchange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, e)
}

func (r *recorder) all() []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Exchange{}, r.seen...)
}

func start(t *testing.T, app http.Handler, o Options) (*httptest.Server, *recorder, string) {
	t.Helper()
	target := httptest.NewServer(app)
	t.Cleanup(target.Close)
	rec := &recorder{}
	if o.Allow == nil {
		o.Allow, _ = httpx.NewAllow(httpx.LocalHosts, false)
	}
	p := httptest.NewUnstartedServer(nil)
	o.Target, o.Record = target.URL+"/base", rec.add
	if o.Listen == "" {
		o.Listen = p.Listener.Addr().String()
	} else if host, port, _ := net.SplitHostPort(o.Listen); port == "0" { // the port it got
		_, port, _ = net.SplitHostPort(p.Listener.Addr().String())
		o.Listen = net.JoinHostPort(host, port)
	}
	h, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	p.Config.Handler = h
	p.Start()
	t.Cleanup(p.Close)
	return p, rec, target.URL
}

func TestProxyRecords(t *testing.T) {
	var got []string
	var mu sync.Mutex
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(body)+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/base/redirect":
			http.Redirect(w, r, "http://"+r.Host+"/base/next", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"email":"pera.peric@example.com"}`)
		}
	})
	p, rec, target := start(t, app, Options{MaxBody: 64})
	req, _ := http.NewRequest("POST", p.URL+"/items/1?x=2", strings.NewReader(`{"a":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	big := strings.Repeat("x", 100)
	resp, _ = http.Post(p.URL+"/big", "application/json", strings.NewReader(big))
	resp.Body.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = client.Get(p.URL + "/redirect")
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != p.URL+"/base/next" {
		t.Errorf("redirect to the service is not pointed at the proxy: %s", loc)
	}
	resp, _ = http.Get(p.URL + "/app.js")
	resp.Body.Close()

	// The service gets every request as it was sent, the large body whole.
	if len(got) != 4 || got[0] != "POST /base/items/1?x=2 {\"a\":1} Bearer t" || got[1] != "POST /base/big "+big+" " {
		t.Fatalf("service got %q", got)
	}
	seen := rec.all()
	if len(seen) != 3 { // app.js is static
		t.Fatalf("recorded %+v", seen)
	}
	if e := seen[0]; e.Entry.URL != target+"/base/items/1?x=2" || e.Entry.Text != `{"a":1}` ||
		e.Entry.MimeType != "application/json" || e.Status != 200 || !strings.Contains(string(e.Entry.Response), "pera.peric") {
		t.Errorf("first: %+v", e)
	}
	if e := seen[1]; !e.Large || e.Entry.Text != "" || e.Entry.MimeType != "" {
		t.Errorf("large body: %+v", e)
	}
}

func TestProxyPassesStreamsThrough(t *testing.T) {
	release := make(chan struct{})
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release // the client reads the first event while the stream is open
		fmt.Fprint(w, "data: two\n\n")
	})
	p, rec, _ := start(t, app, Options{})
	resp, err := http.Get(p.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	close(release)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if line != "data: one\n" {
		t.Fatalf("first event: %q", line)
	}
	p.Close() // waits for the handler
	seen := rec.all()
	if len(seen) != 1 || seen[0].Streamed != "event stream" || seen[0].Entry.Response != nil {
		t.Fatalf("recorded %+v", seen)
	}
}

func TestProxyRefuses(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request reached the service") })
	// The service is on 127.0.0.1, which this allow-list leaves out: the
	// transport refuses to connect even when a caller skipped the check.
	none, _ := httpx.NewAllow([]string{"10.0.0.0/8"}, false)
	p, rec, _ := start(t, app, Options{Allow: none})
	resp, err := http.Get(p.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "host not allowed") {
		t.Errorf("not allowed: %d %s", resp.StatusCode, body)
	}
	// It is not a forward proxy: CONNECT names another host, and is refused
	// for its own too.
	for host, want := range map[string]int{"example.com:443": http.StatusMisdirectedRequest, p.Listener.Addr().String(): http.StatusMethodNotAllowed} {
		req, _ := http.NewRequest(http.MethodConnect, p.URL, nil)
		req.Host = host
		if resp := do(t, req); resp.StatusCode != want {
			t.Errorf("CONNECT %s: %d", host, resp.StatusCode)
		}
	}
	if len(rec.all()) != 0 {
		t.Errorf("recorded %+v", rec.all())
	}
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// TestProxyAnswersOnlyItsAddress: DNS rebinding. A page of attacker.example
// that points its name at 127.0.0.1 sends its own Host; the proxy refuses it,
// so the page reads nothing and records nothing.
func TestProxyAnswersOnlyItsAddress(t *testing.T) {
	var reached []string
	var mu sync.Mutex
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/base/redir" {
			http.Redirect(w, r, "http://"+r.Host+"/base/next", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	p, rec, _ := start(t, app, Options{})
	_, port, _ := net.SplitHostPort(p.Listener.Addr().String())
	for _, host := range []string{"attacker.example:" + port, "attacker.example", "127.0.0.1.nip.io:" + port, "127.0.0.1:1", "localhost.:" + port} {
		req, _ := http.NewRequest("POST", p.URL+"/api/me/password", strings.NewReader(`{"newPassword":"x"}`))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		if resp := do(t, req); resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("Host %s: %d", host, resp.StatusCode)
		}
		req, _ = http.NewRequest("GET", p.URL+"/redir", nil)
		req.Host = host
		if resp := do(t, req); resp.StatusCode != http.StatusMisdirectedRequest || resp.Header.Get("Location") != "" {
			t.Errorf("redirect for Host %s: %d %s", host, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if len(reached) != 0 || len(rec.all()) != 0 {
		t.Fatalf("a foreign Host reached the service %q or was recorded %+v", reached, rec.all())
	}
	// Every loopback form of its own address works, and a redirect comes
	// back to the form the client used (a cookie for localhost is not sent
	// to 127.0.0.1).
	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port, "[::1]:" + port} {
		req, _ := http.NewRequest("GET", p.URL+"/redir", nil)
		req.Host = host
		resp := do(t, req)
		if want := "http://" + strings.ToLower(host) + "/base/next"; resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("Host %s: %d %s", host, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestProxyOwnAddress(t *testing.T) {
	for listen, hosts := range map[string]map[string]bool{
		"127.0.0.1:8088": {"127.0.0.1:8088": true, "localhost:8088": true, "[::1]:8088": true, "127.0.0.2:8088": true,
			"192.168.1.10:8088": false, "example.com:8088": false, "127.0.0.1:8089": false, "127.0.0.1": false},
		"127.0.0.1:80":      {"127.0.0.1": true, "localhost": true, "localhost:80": true, "example.com": false},
		"192.168.1.10:8088": {"192.168.1.10:8088": true, "127.0.0.1:8088": true, "192.168.1.11:8088": false, "dev.lan:8088": false},
		"dev.lan:8088":      {"dev.lan:8088": true, "DEV.LAN:8088": true, "localhost:8088": true, "evil.lan:8088": false},
		// Any interface: an address is fine, a name is what rebinding uses.
		"0.0.0.0:8088": {"10.1.2.3:8088": true, "[fe80::1]:8088": true, "localhost:8088": true, "attacker.example:8088": false},
		"[::]:8088":    {"10.1.2.3:8088": true, "attacker.example:8088": false},
	} {
		p, err := New(Options{Target: "http://127.0.0.1:1", Listen: listen})
		if err != nil {
			t.Fatal(err)
		}
		for host, want := range hosts {
			if _, ok := p.own(host); ok != want {
				t.Errorf("listen %s, Host %s: %v", listen, host, ok)
			}
		}
	}
	for _, listen := range []string{"", "127.0.0.1", "127.0.0.1:0"} {
		if _, err := New(Options{Target: "http://127.0.0.1:1", Listen: listen}); err == nil {
			t.Errorf("listen %q accepted", listen)
		}
	}
}

// TestProxyRefusesOtherSites: a page of another site may make the browser
// send a request to the proxy; it must not reach the service as the
// developer, nor be recorded. The developer's own pages still work.
func TestProxyRefusesOtherSites(t *testing.T) {
	var reached []string
	var mu sync.Mutex
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = append(reached, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	p, rec, _ := start(t, app, Options{})
	type request struct {
		method, path string
		headers      map[string]string
	}
	send := func(r request) int {
		var body io.Reader
		if r.method == "POST" {
			body = strings.NewReader(`{"a":1}`)
		}
		req, _ := http.NewRequest(r.method, p.URL+r.path, body)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		return do(t, req).StatusCode
	}
	for _, r := range []request{
		{"POST", "/x", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Origin": "https://evil.example"}},
		{"POST", "/x", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}}, // a form
		{"GET", "/x", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}},   // an image, a script
		{"POST", "/x", map[string]string{"Origin": "https://evil.example"}},                             // no fetch metadata
		{"POST", "/x", map[string]string{"Origin": "null"}},
		{"DELETE", "/x", map[string]string{"Origin": "http://127.0.0.1.evil.example"}},
		{"GET", "/socket", map[string]string{"Origin": "https://evil.example", "Connection": "Upgrade", "Upgrade": "websocket"}},
	} {
		if code := send(r); code != http.StatusForbidden {
			t.Errorf("%+v: %d", r, code)
		}
	}
	if len(reached) != 0 || len(rec.all()) != 0 {
		t.Fatalf("a request from another site reached the service %q or was recorded %+v", reached, rec.all())
	}

	// The application's own pages, a frontend in development on another
	// local port, a typed URL and a client that is not a browser.
	own := "http://" + p.Listener.Addr().String()
	for _, r := range []request{
		{"POST", "/same", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Origin": own}},
		{"POST", "/dev", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors", "Origin": "http://localhost:3000"}},
		{"GET", "/typed", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate"}},
		{"PUT", "/e2e", nil},
	} {
		if code := send(r); code != http.StatusOK {
			t.Errorf("%+v: %d", r, code)
		}
	}
	// A link or a sign-in provider sending the browser back works, but is
	// not recorded.
	if code := send(request{"GET", "/callback", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}}); code != http.StatusOK {
		t.Errorf("navigation from another site: %d", code)
	}
	seen := rec.all()
	if len(seen) != 5 {
		t.Fatalf("recorded %+v", seen)
	}
	for i, path := range []string{"/same", "/dev", "/typed", "/e2e"} {
		if e := seen[i]; !strings.HasSuffix(e.Entry.URL, "/base"+path) || e.Streamed != "" {
			t.Errorf("%s: %+v", path, e)
		}
	}
	if e := seen[4]; e.Streamed != "navigation from another site" || e.Entry.Text != "" || len(e.Entry.Headers) != 0 {
		t.Errorf("navigation from another site: %+v", e)
	}
}

// TestProxyRecordingCap: a body over MaxRecorded passes through whole and
// is not kept, however large the cap asked for.
func TestProxyRecordingCap(t *testing.T) {
	var size int64
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/base/big" {
			size = n
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	p, rec, _ := start(t, app, Options{MaxBody: 1 << 30})
	if h := p.Config.Handler.(*Proxy); h.o.MaxBody != MaxRecorded {
		t.Errorf("MaxBody %d", h.o.MaxBody)
	}
	big := bytes.Repeat([]byte("x"), MaxRecorded+1)
	resp, err := http.Post(p.URL+"/big", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, _ = http.Post(p.URL+"/small", "application/json", strings.NewReader(`{"a":1}`))
	resp.Body.Close()
	seen := rec.all()
	if size != MaxRecorded+1 || len(seen) != 2 {
		t.Fatalf("service got %d bytes; recorded %d", size, len(seen))
	}
	if e := seen[0]; !e.Large || e.Entry.Text != "" {
		t.Errorf("large body kept: %v %d", e.Large, len(e.Entry.Text))
	}
	if e := seen[1]; e.Large || e.Entry.Text != `{"a":1}` {
		t.Errorf("small body: %+v", e)
	}
}

// TestProxyIncompleteResponse: a response body over the cap, one the
// client stopped reading, or one that is not text (an image) is not known
// to be clean: Unscanned. An empty one is.
func TestProxyIncompleteResponse(t *testing.T) {
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/base/avatar":
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprint(w, "\x89PNG\r\n\x1a\n")
		case "/base/empty":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusNoContent)
		case "/base/big":
			fmt.Fprint(w, `{"items":"`+strings.Repeat("x", 100)+`"}`)
		case "/base/cut":
			fmt.Fprint(w, `{"items":[`)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // the rest never comes: the client went away
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	p, rec, _ := start(t, app, Options{MaxBody: 64})
	for _, path := range []string{"/big", "/small", "/avatar", "/empty"} {
		resp, err := http.Get(p.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	resp, err := http.Get(p.URL + "/cut")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Read(make([]byte, 4))
	resp.Body.Close() // the client goes away mid-body
	var seen []Exchange
	for range 200 {
		if seen = rec.all(); len(seen) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(seen) != 5 {
		t.Fatalf("recorded %+v", seen)
	}
	for _, e := range seen {
		want := !strings.HasSuffix(e.Entry.URL, "/small") && !strings.HasSuffix(e.Entry.URL, "/empty")
		if e.Entry.Unscanned != want {
			t.Errorf("%s: Unscanned %v", e.Entry.URL, e.Entry.Unscanned)
		}
	}
}

// TestProxyScansEncodedResponses: a gzip or deflate response is decoded for
// the scan, within the cap, while the client gets the bytes the service
// sent; one in an encoding the proxy cannot decode is Unscanned.
func TestProxyScansEncodedResponses(t *testing.T) {
	const doc = `{"email":"pera.peric@example.com"}`
	compress := func(w io.WriteCloser, buf *bytes.Buffer, s string) []byte {
		io.WriteString(w, s)
		w.Close()
		return buf.Bytes()
	}
	var gz, zl, raw, bomb bytes.Buffer
	bodies := map[string][2]string{ // path: encoding, body
		"/gzip":     {"gzip", string(compress(gzip.NewWriter(&gz), &gz, doc))},
		"/zlib":     {"deflate", string(compress(zlib.NewWriter(&zl), &zl, doc))},
		"/br":       {"br", "\x1b\x21\x00\xf8\x25\x00"},
		"/identity": {"identity", doc},
		"/bomb":     {"gzip", string(compress(gzip.NewWriter(&bomb), &bomb, strings.Repeat("0", 1<<20)))},
		"/broken":   {"gzip", "not gzip"},
	}
	fw, _ := flate.NewWriter(&raw, flate.BestCompression)
	bodies["/flate"] = [2]string{"deflate", string(compress(fw, &raw, doc))}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := bodies[strings.TrimPrefix(r.URL.Path, "/base")]
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", b[0])
		io.WriteString(w, b[1])
	})
	p, rec, _ := start(t, app, Options{MaxBody: 4096})
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	for path, b := range bodies {
		req, _ := http.NewRequest("GET", p.URL+path, nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != b[1] || resp.Header.Get("Content-Encoding") != b[0] {
			t.Errorf("%s: the client got %q (%s), not the bytes the service sent", path, got, resp.Header.Get("Content-Encoding"))
		}
	}
	seen := rec.all()
	if len(seen) != len(bodies) {
		t.Fatalf("recorded %d", len(seen))
	}
	for _, e := range seen {
		path := e.Entry.URL[strings.LastIndex(e.Entry.URL, "/"):]
		switch path {
		case "/gzip", "/zlib", "/flate", "/identity":
			if string(e.Entry.Response) != doc || e.Entry.Unscanned {
				t.Errorf("%s: scanned %q, unscanned %v", path, e.Entry.Response, e.Entry.Unscanned)
			}
		default: // br, a body that expands past the cap, one that does not decode
			if !e.Entry.Unscanned || len(e.Entry.Response) != 0 {
				t.Errorf("%s: scanned %q, unscanned %v", path, e.Entry.Response, e.Entry.Unscanned)
			}
		}
	}
}

// TestProxyBeyondLoopback: on an address that is not loopback a browser
// sends no Sec-Fetch-Site over plain HTTP, so a request is recorded only
// with a sign it comes from a page of the proxy's own address; any other is
// forwarded but not recorded.
func TestProxyBeyondLoopback(t *testing.T) {
	var mu sync.Mutex
	var reached []string
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reached = append(reached, r.Method+" "+r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"email":"pera.peric@example.com"}`)
	})
	p, rec, _ := start(t, app, Options{Listen: "0.0.0.0:0"})
	own := p.URL
	_, port, _ := net.SplitHostPort(p.Listener.Addr().String())
	type request struct {
		method, path string
		headers      map[string]string
	}
	send := func(r request) {
		var body io.Reader
		if r.method == "POST" {
			body = strings.NewReader(`{"a":1}`)
		}
		req, _ := http.NewRequest(r.method, own+r.path, body)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		if resp := do(t, req); resp.StatusCode != http.StatusOK {
			t.Errorf("%+v: %d", r, resp.StatusCode)
		}
	}
	for _, r := range []request{
		{"GET", "/plain", nil}, // a link or an image of another site, or a client that is not a browser
		{"GET", "/image", map[string]string{"Referer": "http://evil.example/page"}},
		{"GET", "/other", map[string]string{"Origin": "http://localhost:3000"}},
		{"GET", "/https", map[string]string{"Referer": "https://127.0.0.1:" + port + "/"}},
		{"POST", "/form", nil},
		{"GET", "/page", map[string]string{"Referer": own + "/app?tab=1"}},
		{"POST", "/fetch", map[string]string{"Origin": own}},
		{"GET", "/same", map[string]string{"Sec-Fetch-Site": "same-origin"}},
		{"GET", "/typed", map[string]string{"Sec-Fetch-Site": "none"}},
	} {
		send(r)
	}
	if len(reached) != 9 || reached[4] != "POST /base/form {\"a\":1}" {
		t.Fatalf("the service got %q", reached)
	}
	seen := rec.all()
	if len(seen) != 9 {
		t.Fatalf("recorded %+v", seen)
	}
	for i, e := range seen {
		want := i < 5
		if e.Unverified != want || (want && (e.Entry.Text != "" || len(e.Entry.Headers) != 0 || e.Entry.Response != nil)) {
			t.Errorf("%s: %+v", e.Entry.URL, e)
		}
	}
	if e := seen[6]; e.Entry.Text != `{"a":1}` || len(e.Entry.Response) == 0 {
		t.Errorf("a request of its own page: %+v", e)
	}
}

// A body read whole is handed over, not copied once more: the proxy holds
// a recorded body once, not twice.
func TestCappedHandsOver(t *testing.T) {
	c := &capped{r: io.NopCloser(strings.NewReader(strings.Repeat("x", 1<<20))), max: MaxRecorded}
	io.Copy(io.Discard, c)
	kept := c.buf.Bytes()
	got, complete := c.bytes()
	if !complete || len(got) != 1<<20 || &got[0] != &kept[0] {
		t.Fatalf("complete %v, %d bytes, copied %v", complete, len(got), len(got) > 0 && &got[0] != &kept[0])
	}
}
