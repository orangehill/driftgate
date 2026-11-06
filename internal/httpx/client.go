// Package httpx sends catalog requests and records the responses exactly:
// no automatic compression, no connection reuse, no proxies from the
// environment, and no connection to a host that is not allowed.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/credential"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// DefaultTimeout is the time limit of a request of a target that sets none.
const DefaultTimeout = 60 * time.Second

// MaxBody bounds a response body so a runaway endpoint cannot exhaust memory.
const MaxBody = 256 << 20

// DefaultUserAgent is sent unless the configuration sets another. It does not
// carry the DriftGate version, so captures stay comparable across releases.
const DefaultUserAgent = "driftgate/1"

// ErrHostNotAllowed is returned for a connection to a host outside the allow-list.
var ErrHostNotAllowed = errors.New("host not allowed")

var defaultNets = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// LocalHosts is an allow-list of the local system only.
var LocalHosts = []string{"localhost", "127.0.0.0/8", "::1/128"}

// Allow decides which hosts may be contacted.
type Allow struct {
	names map[string]bool
	nets  []*net.IPNet
	any   bool
	// LookupIP, when set, resolves names instead of the system resolver
	// (tests).
	LookupIP func(ctx context.Context, network, host string) ([]net.IP, error)
}

func (a *Allow) lookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if a.LookupIP != nil {
		return a.LookupIP(ctx, "ip", host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// NewAllow builds the allow-list from host names and CIDR ranges. An empty
// list means local and private addresses; anyHost lifts every restriction.
func NewAllow(entries []string, anyHost bool) (*Allow, error) {
	a := &Allow{names: map[string]bool{}, any: anyHost}
	if len(entries) == 0 {
		entries = append([]string{"localhost"}, defaultNets...)
	}
	for _, e := range entries {
		if _, n, err := net.ParseCIDR(e); err == nil {
			a.nets = append(a.nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			a.nets = append(a.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		a.names[strings.ToLower(e)] = true
	}
	return a, nil
}

func (a *Allow) ipAllowed(ip net.IP) bool {
	for _, n := range a.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Check reports an error unless the host is allowed: named in the list, or
// every address it resolves to inside an allowed range.
func (a *Allow) Check(ctx context.Context, host string) error {
	if a.any || a.names[strings.ToLower(host)] {
		return nil
	}
	ips, err := a.lookupIP(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if !a.ipAllowed(ip) {
			return fmt.Errorf("%w: %s (%s)", ErrHostNotAllowed, host, ip)
		}
	}
	return nil
}

// Dial connects only to allowed hosts. A host named in the allow-list may
// resolve anywhere; any other name must resolve to an allowed address, and the
// connection goes to that checked address (no second lookup to rebind).
func (a *Allow) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if a.any || a.names[strings.ToLower(host)] {
		return d.DialContext(ctx, network, addr)
	}
	ips, err := a.lookupIP(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if a.ipAllowed(ip) {
			return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
}

// DialContext connects only to allowed hosts, as the client does; for a
// transport of its own, such as the recording proxy's.
func (a *Allow) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return a.Dial(ctx, network, addr)
}

// Options configure a client.
type Options struct {
	BaseURL         string
	UserAgent       string
	Timeout         time.Duration
	FollowRedirects bool
	Allow           *Allow
	// OffHeap reads a body that is not small outside the Go heap
	// (readOffHeap); the caller then releases every Result.
	OffHeap bool
}

// Client sends catalog requests.
type Client struct {
	base    *url.URL
	ua      string
	http    *http.Client
	offHeap bool
}

// New builds a client.
func New(o Options) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(o.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("target.base_url must be an http or https URL, got %q", o.BaseURL)
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	transport := &http.Transport{
		Proxy:               nil, // never a proxy from the environment
		DialContext:         o.Allow.Dial,
		DisableCompression:  true,
		DisableKeepAlives:   true, // sends Connection: close, like the reference capture
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	c := &http.Client{Transport: transport, Timeout: o.Timeout, CheckRedirect: checkRedirect}
	if !o.FollowRedirects {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return &Client{base: base, ua: o.UserAgent, http: c, offHeap: o.OffHeap}, nil
}

// keptOnRedirect are the headers a redirect to another host keeps: the
// catalog's own headers and the token may be credentials for this target
// only (Go keeps them for a subdomain, and keeps every header it does not
// know).
var keptOnRedirect = map[string]bool{"User-Agent": true, "Accept": true, "Accept-Encoding": true, "Accept-Language": true}

// checkRedirect follows at most 10 redirects (as Go does), never from
// https to http, where the token and the catalog's headers would travel
// in the clear; to another host (or port) it sends only keptOnRedirect.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0]
	if first.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing a redirect from https to " + req.URL.Scheme)
	}
	if req.URL.Host != first.URL.Host {
		for name := range req.Header {
			if !keptOnRedirect[name] {
				delete(req.Header, name)
			}
		}
	}
	return nil
}

// URL builds the full URL of a request, filling {name} placeholders from
// params. The path must start with "/", and the URL must keep the scheme and
// host of the base URL: the identity's token goes with the request. An error
// names the path as the catalog writes it, never with values filled in (they
// may come from a private body).
func (c *Client) URL(r catalog.Request) (string, error) {
	if err := catalog.CheckPath(r.Path); err != nil {
		return "", fmt.Errorf("%s: %w", r.ID, err)
	}
	path := r.Path
	for name, value := range r.Params {
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
	}
	if strings.Contains(path, "{") && strings.Contains(path, "}") {
		if i, j := strings.Index(path, "{"), strings.Index(path, "}"); i < j {
			return "", fmt.Errorf("%s: unfilled parameter in %s", r.ID, r.Path)
		}
	}
	full := c.base.String() + path
	u, err := url.Parse(full)
	if err != nil || u.Scheme != c.base.Scheme || u.Host != c.base.Host || u.User != nil {
		return "", fmt.Errorf("%s: path %s does not make a URL on %s", r.ID, r.Path, c.base.Host)
	}
	return full, nil
}

// transportError is the text of a failed request without its URL: the path
// may hold a value taken from a private body (params_from), and the error is
// stored in the record, which may be committed.
func transportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// loadFailure names a failure a server under load gives now and then: a
// timeout, or a connection reset or closed before the response was whole.
// Empty for any other failure (a refused connection, a host not allowed)
// and when ctx itself ended (an interrupt is no failure of the server).
func loadFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return ""
	}
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection reset"
	}
	for _, r := range resetErrors {
		if errors.Is(err, r) {
			return "connection reset"
		}
	}
	return ""
}

// Result is one sent request.
type Result struct {
	Record   *snapshot.Record
	Body     []byte
	Duration time.Duration
	// LoadFailure is set when the request failed as a server under load
	// does now and then: "timeout" or "connection reset" (Record.Error
	// says more).
	LoadFailure string
	region      *region // the memory of a body read off the heap
}

// Release gives back the memory of a body read outside the Go heap
// (Options.OffHeap); the body must not be used after it. It does nothing
// for any other result.
func (r Result) Release() { r.region.free() }

// OffHeap reports a body read outside the Go heap: dropping it leaves no
// garbage to collect.
func (r Result) OffHeap() bool { return r.region != nil }

// Do sends a request. Transport failures (refused connection, timeout,
// host not allowed) are returned as a record with Error set, not as an error.
func (c *Client) Do(ctx context.Context, r catalog.Request, token string) Result {
	rec := &snapshot.Record{ID: r.ID, Headers: [][2]string{}, Body: snapshot.Body{Storage: r.Storage}}
	u, err := c.URL(r)
	if err != nil {
		rec.Error = err.Error()
		return Result{Record: rec}
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		rec.Error = transportError(err)
		return Result{Record: rec}
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept-Encoding", "identity")
	// The catalog's first header of a name replaces the default (a catalog
	// Accept-Encoding: gzip is not sent next to identity); more of the name
	// are added.
	set := map[string]bool{}
	for _, h := range r.Headers {
		if name := http.CanonicalHeaderKey(h[0]); set[name] {
			req.Header.Add(name, h[1])
		} else {
			req.Header.Set(name, h[1])
			set[name] = true
		}
	}
	if len(r.Body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		rec.Error = transportError(err)
		return Result{Record: rec, Duration: time.Since(start), LoadFailure: loadFailure(ctx, err)}
	}
	defer resp.Body.Close()
	length := resp.ContentLength
	if resp.Body == http.NoBody {
		length = 0 // HEAD, 204, 304: the header may name the length of a body not sent
	}
	if length > MaxBody {
		rec.Error = fmt.Sprintf("body larger than %d bytes", MaxBody)
		return Result{Record: rec, Duration: time.Since(start)}
	}
	raw, reg, err := readBody(resp.Body, length, c.offHeap)
	dur := time.Since(start)
	switch {
	case err != nil:
		rec.Error = "reading body: " + transportError(err)
		return Result{Record: rec, Duration: dur, LoadFailure: loadFailure(ctx, err)}
	case len(raw) > MaxBody:
		reg.free()
		rec.Error = fmt.Sprintf("body larger than %d bytes", MaxBody)
		return Result{Record: rec, Duration: dur}
	}
	rec.Status = resp.StatusCode
	// Go moves Transfer-Encoding out of the header map; the server did send it.
	if len(resp.TransferEncoding) > 0 {
		resp.Header["Transfer-Encoding"] = []string{strings.Join(resp.TransferEncoding, ", ")}
	}
	// It also drops "Connection: close" from an HTTP/1.1 response and sets Close.
	if resp.Close && resp.ProtoAtLeast(1, 1) && resp.Header.Get("Connection") == "" {
		resp.Header["Connection"] = []string{"close"}
	}
	rec.Headers = Headers(resp.Header)
	return Result{Record: rec, Body: raw, Duration: dur, region: reg}
}

// maxPrealloc caps the buffer made up front for a body of known length on
// the heap: the length is the server's word, and a body that is not as
// long as it says must not take that much memory before a byte is read.
const maxPrealloc = 4 << 20

// readBody reads a response body, at most MaxBody+1 bytes of it. A body of
// known length is read into a buffer of that size (up to maxPrealloc):
// grown as it is read, the buffer of a large body takes up to twice its
// size, and as much garbage. With offHeap, a body that is not small (of
// unknown length, chunked as PHP behind nginx sends every one, or of a
// known length from smallBody up) is read into a region outside the heap.
func readBody(r io.Reader, length int64, offHeap bool) ([]byte, *region, error) {
	r = io.LimitReader(r, MaxBody+1)
	switch {
	case length >= 0 && (!offHeap || length < smallBody):
		b := bytes.NewBuffer(make([]byte, 0, min(length, maxPrealloc)+bytes.MinRead))
		_, err := b.ReadFrom(r)
		return b.Bytes(), nil, err
	case !offHeap:
		b, err := io.ReadAll(r)
		return b, nil, err
	}
	return readOffHeap(r, length)
}

// Headers turns a header map into sorted, lower-case pairs with sensitive
// values masked. Go does not keep the received order of header names; the
// comparison ignores that order anyway.
func Headers(h http.Header) [][2]string {
	names := make([]string, 0, len(h))
	for n := range h {
		names = append(names, n)
	}
	sort.Strings(names)
	out := [][2]string{}
	for _, n := range names {
		lower := strings.ToLower(n)
		for _, v := range h[n] {
			out = append(out, [2]string{lower, MaskHeader(lower, v)})
		}
	}
	return out
}

// MaskHeader hides secrets in header values while keeping their format, so
// allow-list formats still match: letters become x, digits 0. Besides the
// headers that carry credentials by definition (Authorization, Cookie,
// Set-Cookie's value), a header named as a credential (X-CSRF-Token,
// X-Api-Key, devise_token_auth's Access-Token, Token, Api-Key) is masked,
// except the standard ones in plainHeaders, and in a header that holds a
// URL (Location, Content-Location, Link, Refresh) the values of query and
// fragment parameters named or shaped as credentials (token=, signature=,
// X-Amz-Signature=, #access_token=) and path segments shaped as one (a
// reset link's token).
func MaskHeader(name, value string) string {
	switch name {
	case "set-cookie":
		first, rest, _ := strings.Cut(value, ";")
		cookie, val, ok := strings.Cut(first, "=")
		if !ok {
			return value
		}
		masked := cookie + "=" + maskChars(val)
		if rest != "" || strings.Contains(value, ";") {
			masked += ";" + rest
		}
		return masked
	case "authorization", "cookie", "proxy-authorization":
		return maskChars(value)
	case "location", "content-location", "refresh":
		masked, _ := credential.MaskURL(value, true, maskChars)
		return masked
	case "link":
		// <url>; rel="next", <url>; rel="prev"
		var b strings.Builder
		for {
			start := strings.IndexByte(value, '<')
			end := strings.IndexByte(value[start+1:], '>')
			if start < 0 || end < 0 {
				b.WriteString(value)
				return b.String()
			}
			end += start + 1
			masked, _ := credential.MaskURL(value[start+1:end], true, maskChars)
			b.WriteString(value[:start+1])
			b.WriteString(masked)
			value = value[end:]
		}
	}
	if !plainHeaders[name] && !strings.HasPrefix(name, "access-control-") && credential.Name(name) {
		return maskChars(value)
	}
	return value
}

// plainHeaders are standard headers that hold no credential: some named
// like one (Access-Control-* starts with "access", as an access token
// does), the most common ones so that their names need no check.
var plainHeaders = map[string]bool{
	"public-key-pins": true, "public-key-pins-report-only": true, "sec-websocket-key": true,
	"signature": true, "signature-input": true, // RFC 9421: a signature anyone may check
	"content-type": true, "content-length": true, "content-encoding": true, "date": true, "server": true,
	"cache-control": true, "etag": true, "expires": true, "last-modified": true, "vary": true,
	"connection": true, "transfer-encoding": true, "strict-transport-security": true,
	"x-content-type-options": true, "x-frame-options": true, "referrer-policy": true,
	"content-security-policy": true, "x-request-id": true, "x-powered-by": true,
}

func maskChars(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'x'
		case r >= 'A' && r <= 'Z':
			return 'X'
		case r >= '0' && r <= '9':
			return '0'
		}
		return r
	}, s)
}
