// Package proxy is the recording proxy of driftgate record: a reverse proxy
// that forwards every request to one service and hands each finished
// exchange to a callback, which turns it into a catalog candidate. It is a
// reverse proxy only: whatever URL or Host a client sends, the request goes
// to the service's base URL, through a transport that connects only to
// allowed hosts, so it cannot be used to reach anything else.
//
// It answers only requests for its own address: a Host header that is not
// the listen address (in a loopback form, for a loopback listen address) is
// refused, which defeats DNS rebinding, where a page the developer has open
// points its own name at 127.0.0.1 to read the service through the proxy.
// A browser request from another site is refused too, but for a plain
// navigation (a link, the return from a sign-in provider), which is
// forwarded and not recorded; and a request that changes data, or opens a
// websocket, must come from a local origin. On an address that is not
// loopback, a browser sends no Sec-Fetch-Site over plain HTTP, so a request
// of another site's page cannot be told from the developer's: there a
// request is recorded only with a sign it comes from a page of the proxy's
// own address (Sec-Fetch-Site same-origin or none, or an Origin or Referer
// of that address), and any other is forwarded but not recorded.
//
// The request body is kept in memory up to a cap, only until the exchange is
// handed over; a larger one passes through unrecorded. A response body is
// never kept: it is read only to scan it for personal data (so the entry
// is stored private; one over the cap or cut off counts as personal, as
// does one that is not text, an image or a PDF, which is not read). A
// gzip or deflate body is decoded for the scan, within the same cap, and
// one in another encoding (br, zstd) counts as personal; the client gets the
// body as the service sent it. A stream (an event stream, a websocket)
// passes through untouched.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/orangehill/driftgate/internal/har"
	"github.com/orangehill/driftgate/internal/httpx"
)

// Exchange is one request that went through the proxy and its outcome.
type Exchange struct {
	// Entry is the request as the pipeline takes it, credentials included;
	// Response is filled only for a response that may hold text.
	Entry  har.Entry
	Status int
	// Streamed says why the exchange passed through unrecorded: "event
	// stream", "websocket", "stream" or "navigation from another site".
	// Entry has only the method and URL then.
	Streamed string
	// Large: the request body was over the cap and is not in Entry.
	Large bool
	// Unverified: the proxy listens beyond loopback and the request had no
	// sign it came from a page of the proxy's own address; it was forwarded,
	// not recorded. Entry has only the method and URL then.
	Unverified bool
}

// MaxRecorded caps the request and response bodies the proxy keeps in memory
// per exchange (the request to record it, the response to scan it for
// personal data). Without it every request in flight could hold up to
// httpx.MaxBody twice; a larger body still passes through, only unrecorded.
const MaxRecorded = 16 << 20

// Options configure the proxy.
type Options struct {
	Target string // the service's base URL
	// Listen is the address the proxy listens on, host:port with the port
	// it got. Requests are answered only for that Host: in any loopback form
	// (127.0.0.1, [::1], localhost) with that port, and for a host that is
	// not loopback, that host exactly (any IP address for an unspecified
	// one, such as 0.0.0.0: a rebound name is never an address).
	Listen  string
	Allow   *httpx.Allow
	MaxBody int64 // request and scanned response bodies; default and at most MaxRecorded
	// Skip names request paths that are forwarded but not recorded (static
	// files are never recorded).
	Skip   func(path string) bool
	Record func(Exchange) // called once the response is sent; may be called concurrently
}

// Proxy is the proxy handler.
type Proxy struct {
	o      Options
	target *url.URL
	rp     *httputil.ReverseProxy
	tr     *http.Transport
	// host and port of Options.Listen; host is lower case, empty for an
	// unspecified address.
	host, port string
	loopback   bool // Options.Listen is on the loopback interface only
}

type ctxKey struct{}

// exchange collects what the hooks of the reverse proxy see.
type exchange struct {
	url      string
	status   int
	streamed string
	failed   bool
	response *capped
	encoding string // the response's Content-Encoding
	binary   bool   // a response body that is not text, which is not read
	// unrecorded: the exchange is not recorded, so its response is not read
	unrecorded bool
}

// New builds the proxy handler. It checks that the target is an http or
// https URL; the caller checks it is on an allowed host before listening.
func New(o Options) (*Proxy, error) {
	target, err := url.Parse(strings.TrimRight(o.Target, "/"))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return nil, fmt.Errorf("target.base_url must be an http or https URL, got %q", o.Target)
	}
	host, port, err := net.SplitHostPort(o.Listen)
	if err != nil || port == "" || port == "0" {
		return nil, fmt.Errorf("proxy: the listen address must be host:port with the port it got, got %q", o.Listen)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = ""
	}
	if o.MaxBody <= 0 || o.MaxBody > MaxRecorded {
		o.MaxBody = MaxRecorded
	}
	p := &Proxy{o: o, target: target, host: strings.ToLower(host), port: port, loopback: Loopback(o.Listen)}
	p.tr = &http.Transport{
		Proxy:               nil, // never a proxy from the environment
		DialContext:         o.Allow.DialContext,
		DisableCompression:  true, // the client's Accept-Encoding goes through as it is
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 16,
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			if x, ok := pr.In.Context().Value(ctxKey{}).(*exchange); ok {
				x.url = pr.Out.URL.String()
			}
		},
		Transport:      p.tr,
		ModifyResponse: p.response,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if x, ok := r.Context().Value(ctxKey{}).(*exchange); ok {
				x.failed = true
			}
			msg := "driftgate record: the service did not answer: " + err.Error()
			if errors.Is(err, httpx.ErrHostNotAllowed) {
				msg = "driftgate record: " + err.Error() + " (target.allow_hosts)"
			}
			http.Error(w, msg, http.StatusBadGateway)
		},
	}
	return p, nil
}

// CloseIdleConnections closes the idle connections to the service; call it
// once the server is shut down.
func (p *Proxy) CloseIdleConnections() { p.tr.CloseIdleConnections() }

// response notes the status, recognises a stream, points a redirect to the
// service back at the proxy, and reads a text body through a cap for the
// personal-data scan.
func (p *Proxy) response(res *http.Response) error {
	x, ok := res.Request.Context().Value(ctxKey{}).(*exchange)
	if !ok {
		return nil
	}
	x.status = res.StatusCode
	mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	switch {
	case res.StatusCode == http.StatusSwitchingProtocols:
		x.streamed = "websocket"
	case mt == "text/event-stream":
		x.streamed = "event stream"
	case mt == "application/x-ndjson" || mt == "application/stream+json" || mt == "application/jsonl":
		x.streamed = "stream"
	}
	if loc := res.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil && u.Host == p.target.Host && u.Scheme == p.target.Scheme {
			if origin, ok := res.Request.Context().Value(originKey{}).(string); ok {
				res.Header.Set("Location", origin+u.RequestURI())
			}
		}
	}
	if x.streamed == "" && !x.unrecorded && res.Body != nil && res.Body != http.NoBody && res.ContentLength != 0 {
		if !har.Textual(mt) {
			x.binary = true // an image, a PDF: not scanned, so not known to be clean
			return nil
		}
		x.response = &capped{r: res.Body, max: p.o.MaxBody}
		x.encoding = res.Header.Get("Content-Encoding")
		res.Body = x.response
	}
	return nil
}

type originKey struct{}

// own returns the Host of a request in lower case if it is an address of
// the proxy (see Options.Listen).
func (p *Proxy) own(hostport string) (string, bool) {
	hostport = strings.ToLower(hostport)
	host, port, err := net.SplitHostPort(hostport)
	if err != nil { // no port: the default one of http
		host, port, err = net.SplitHostPort(hostport + ":80")
	}
	if err != nil || port != p.port {
		return "", false
	}
	ip := net.ParseIP(host)
	switch {
	case host == "localhost" || (ip != nil && ip.IsLoopback()):
	case p.host != "" && host == p.host:
	case p.host == "" && ip != nil:
	default:
		return "", false
	}
	return hostport, true
}

// localOrigin reports whether an Origin header names a page served by the
// proxy or by another server on this machine (a frontend in development).
func (p *Proxy) localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false // "null" too: a sandboxed or file:// page
	}
	if _, ok := p.own(u.Host); ok {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
}

// ownPage reports whether a request carries a sign it comes from a page of
// the proxy's own address, or from the address bar: Sec-Fetch-Site
// same-origin or none, or an Origin or Referer of that address.
func (p *Proxy) ownPage(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	}
	for _, h := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if u, err := url.Parse(h); err == nil && u.Scheme == "http" {
			if _, ok := p.own(u.Host); ok {
				return true
			}
		}
	}
	return false
}

// safeMethod is a method that does not change data.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == http.MethodTrace
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, ok := p.own(r.Host)
	if !ok {
		// Not echoed: it is whatever the client sent.
		http.Error(w, "driftgate record answers only for its own address: open http://"+net.JoinHostPort(p.listenHost(), p.port)+"/", http.StatusMisdirectedRequest)
		return
	}
	if r.Method == http.MethodConnect {
		http.Error(w, "driftgate record is a reverse proxy, not a forward proxy: point the client at http://"+host+" itself (HTTPS from the client is not supported)", http.StatusMethodNotAllowed)
		return
	}
	upgrade := r.Header.Get("Upgrade") != ""
	// A page of another site may send a request here, not read the answer;
	// it must not reach the service as the developer, nor be recorded. A
	// navigation from another site (a link, a sign-in provider sending the
	// browser back) is let through so the application keeps working, but is
	// not recorded. Browsers send Sec-Fetch-Site; Origin covers one that
	// does not, for what changes data or opens a websocket.
	crossNav := false
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		if r.Header.Get("Sec-Fetch-Mode") != "navigate" || !safeMethod(r.Method) || upgrade {
			http.Error(w, "driftgate record refuses a request from another site", http.StatusForbidden)
			return
		}
		crossNav = true
	}
	if origin := r.Header.Get("Origin"); origin != "" && (!safeMethod(r.Method) || upgrade) && !p.localOrigin(origin) {
		http.Error(w, "driftgate record refuses a request from another site", http.StatusForbidden)
		return
	}
	// Beyond loopback a page of another site sends no Sec-Fetch-Site over
	// plain HTTP, and needs no Origin for a GET (a link, an image): what has
	// no sign of the proxy's own pages is forwarded, not recorded.
	unverified := !crossNav && !p.loopback && !p.ownPage(r)
	skip := har.Static(r.URL.Path) || (p.o.Skip != nil && p.o.Skip(r.URL.Path))
	x := &exchange{unrecorded: skip || crossNav || unverified}
	ctx := context.WithValue(r.Context(), ctxKey{}, x)
	// A redirect to the service comes back to the address the client used:
	// a session cookie set for localhost is not sent to 127.0.0.1. It is an
	// address of the proxy, checked above.
	ctx = context.WithValue(ctx, originKey{}, "http://"+host)
	r = r.WithContext(ctx)
	var body *capped
	if !x.unrecorded && !upgrade && r.Body != nil && r.Body != http.NoBody {
		body = &capped{r: r.Body, max: p.o.MaxBody}
		r.Body = body
	}
	if v := p.forward(w, r); v != nil {
		defer panic(v) // once the exchange is handed over
	}
	if skip || x.failed || p.o.Record == nil {
		return
	}
	if upgrade && x.streamed == "" {
		x.streamed = "websocket"
	}
	if crossNav {
		x.streamed = "navigation from another site"
	}
	if unverified {
		p.o.Record(Exchange{Status: x.status, Unverified: true, Entry: har.Entry{Method: r.Method, URL: x.url}})
		return
	}
	if x.streamed != "" {
		p.o.Record(Exchange{Status: x.status, Streamed: x.streamed, Entry: har.Entry{Method: r.Method, URL: x.url}})
		return
	}
	e := Exchange{Status: x.status, Entry: har.Entry{Method: r.Method, URL: x.url}}
	for name, values := range r.Header {
		for _, v := range values {
			e.Entry.Headers = append(e.Entry.Headers, [2]string{name, v})
		}
	}
	if body != nil {
		text, complete := body.bytes()
		if complete {
			// The copy is no one else's: the text shares it.
			e.Entry.MimeType, e.Entry.Text = r.Header.Get("Content-Type"), unsafe.String(unsafe.SliceData(text), len(text))
		} else {
			e.Large = true
			var kept [][2]string
			for _, h := range e.Entry.Headers {
				if !strings.EqualFold(h[0], "Content-Type") {
					kept = append(kept, h)
				}
			}
			e.Entry.Headers = kept
		}
	}
	e.Entry.Unscanned = x.binary
	if x.response != nil {
		// A body not read whole (over the cap, the client went away), or in
		// an encoding it cannot be decoded from, is not known to be clean.
		text, complete := x.response.bytes()
		if complete {
			text, complete = httpx.Decode(text, x.encoding, p.o.MaxBody)
		}
		if complete {
			e.Entry.Response = text
		} else {
			e.Entry.Unscanned = true
		}
	}
	p.o.Record(e)
}

// forward runs the reverse proxy. A response cut off on the way (the client
// or the service went away) aborts the handler with http.ErrAbortHandler:
// forward returns it, to raise again once the exchange, whose response is
// then not whole, is handed over.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) (aborted any) {
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			aborted = v
		}
	}()
	p.rp.ServeHTTP(w, r)
	return nil
}

// listenHost is the host to show for the proxy's address.
func (p *Proxy) listenHost() string {
	if p.host == "" || Loopback(net.JoinHostPort(p.host, p.port)) {
		return "127.0.0.1"
	}
	return p.host
}

// capped reads a body through and keeps a copy of it up to max bytes. The
// transport may still be reading it when the handler is done, hence the lock.
type capped struct {
	r    io.ReadCloser
	max  int64
	mu   sync.Mutex
	buf  bytes.Buffer
	over bool
	done bool
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.over {
		if int64(c.buf.Len()+n) > c.max {
			c.over, c.buf = true, bytes.Buffer{}
		} else {
			c.buf.Write(p[:n])
		}
	}
	if errors.Is(err, io.EOF) {
		c.done = true
	}
	return n, err
}

func (c *capped) Close() error { return c.r.Close() }

// bytes returns the copy, and whether it is the whole body. A whole body
// is read to its end, so nothing writes to the copy any more: it is handed
// over, not copied again.
func (c *capped) bytes() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.over || !c.done {
		return nil, false
	}
	b := c.buf.Bytes()
	c.buf = bytes.Buffer{}
	return b, true
}

// Loopback reports whether a listen address is on the loopback interface
// only: 127.0.0.0/8, ::1 or localhost. An empty host means every interface.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
