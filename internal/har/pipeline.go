package har

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/routes"
)

// Entry is one request as the browser saved it in a HAR file or as the
// recording proxy saw it. Headers are as sent, credentials included: the
// pipeline reads them to recognise an identity and never keeps them.
type Entry struct {
	Method   string
	URL      string // absolute
	Headers  [][2]string
	MimeType string // of the body
	Text     string // the body
	// Response is the response body, only scanned for personal data when
	// Options.Personal is set; it is never kept.
	Response []byte
	// Unscanned: there was a response body, but not whole (over the cap, the
	// client went away, the HAR file left it out) or in an encoding that
	// cannot be read. It counts as personal data.
	Unscanned bool
	params    []harParam // the form fields a HAR file lists
}

// Options extend the pipeline beyond the plain HAR reading of Read.
type Options struct {
	// Routes turns a path into its route: a {name} segment of the route
	// becomes a parameter of the candidate, and requests are told apart by
	// method, route, the names of the query parameters, identity and the
	// shape of the body (its keys and value types), so /users/1 and
	// /users/2, or a poll with a new ?since=, are one candidate. An empty
	// inventory keeps the paths as they are but still compares by shape.
	// Without Routes a request is a duplicate only when it is the same.
	Routes *routes.Inventory
	Target string // the service, as the inventory names it
	// Identify returns the configured identity whose token the request
	// carries, or "".
	Identify func(authorization, cookie string) string
	// Personal scans Entry.Response for personal data (Candidate.Personal).
	Personal bool
}

// Builder turns entries into candidates, each once.
type Builder struct {
	base *url.URL
	o    Options
	// seen holds the candidates returned, by key: a later response of the
	// same request with personal data marks the candidate Personal there.
	seen map[string]*Candidate
}

// NewBuilder starts a pipeline for the requests to baseURL.
func NewBuilder(baseURL string, o Options) (*Builder, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("target.base_url %q is not a URL", baseURL)
	}
	return &Builder{base: base, o: o, seen: map[string]*Candidate{}}, nil
}

// Add takes one entry. It returns the candidate when the entry is a new
// request, a note when it is left out for a reason the user should know,
// and neither for another host, a static file or a duplicate. The candidate
// stays the Builder's: a duplicate whose response holds personal data sets
// its Personal, so read that once every entry is in.
func (b *Builder) Add(e Entry) (*Candidate, *Note) {
	u, err := url.Parse(e.URL)
	if err != nil || !strings.EqualFold(u.Host, b.base.Host) || !strings.HasPrefix(u.Path, b.base.Path) {
		return nil, nil
	}
	relPath := strings.TrimPrefix(u.Path, b.base.Path)
	if relPath == "" {
		relPath = "/"
	}
	if !strings.HasPrefix(relPath, "/") {
		return nil, nil // /apiv2 is not under /api
	}
	if Static(relPath) {
		return nil, nil
	}
	rel := relPath
	// A note names the request by its method and path, never its query, with
	// a token or personal data in the path masked.
	note := func(reason string) *Note {
		return &Note{strings.ToUpper(e.Method) + " " + MaskPath(relPath), reason}
	}
	if secretPath(rel) != "" {
		return nil, note("the path carries a token; add the request by hand")
	}
	if name := secretParam(u.Query()); name != "" {
		return nil, note("the query carries " + name + "; add the request by hand, with an identity")
	}
	if u.RawQuery != "" {
		rel += "?" + u.RawQuery
	}
	c := Candidate{Method: strings.ToUpper(e.Method), Path: rel}
	var authorization, cookie string
	for _, h := range e.Headers {
		switch name := strings.ToLower(h[0]); name {
		case "accept", "content-type":
			c.Headers = append(c.Headers, [2]string{name, h[1]})
		case "authorization":
			c.Auth, authorization = true, h[1]
		case "cookie":
			c.Auth, cookie = true, h[1]
		}
	}
	mimeType, text := e.MimeType, e.Text
	if isMultipart(mimeType) {
		mp, reason := formCandidate(mimeType, text, e.params)
		if reason != "" {
			return nil, note(reason)
		}
		c.Multipart, c.Headers, text = mp, withoutContentType(c.Headers), ""
	}
	if text != "" {
		if !strings.Contains(strings.ToLower(mimeType), "json") {
			return nil, note("body is " + mimeType + ", not JSON; only JSON bodies are supported")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, []byte(text)); err != nil {
			return nil, note("body is not valid JSON")
		}
		if name := secretKey(buf.Bytes()); name != "" {
			return nil, note("the body carries " + name + "; add the request by hand")
		}
		c.Body = buf.Bytes()
	}
	var queryText []string
	for _, k := range sortedKeys(u.Query()) {
		queryText = append(queryText, u.Query()[k]...)
	}
	if f := detector.Scan([]byte(rel + " " + strings.Join(queryText, " ") + " " + string(c.Body))); len(f) > 0 {
		return nil, note("it holds personal data (" + f[0].Kind + "); add the request by hand")
	}
	if c.Auth && b.o.Identify != nil {
		c.Identity = b.o.Identify(authorization, cookie)
	}
	if b.o.Routes != nil {
		b.template(&c, relPath, u.RawQuery)
	}
	key := b.Key(catalog.Request{Target: b.o.Target, Method: c.Method, Path: c.Path, Params: c.Params,
		Body: c.Body, Identity: c.Identity, Multipart: c.Multipart})
	prev := b.seen[key]
	if prev != nil && (prev.Personal || !b.o.Personal) {
		return nil, nil
	}
	personal := b.o.Personal && (e.Unscanned || len(e.Response) > 0 && len(detector.Scan(e.Response)) > 0)
	if prev != nil {
		prev.Personal = personal
		return nil, nil
	}
	c.Personal = personal
	b.seen[key] = &c
	return &c, nil
}

// simpleParam is a route segment that is one required parameter.
var simpleParam = lazy.Regexp(`^\{([A-Za-z0-9_.-]+)\}$`)

// template names the route the request reaches and, where a whole segment
// of the route is one parameter, puts the parameter in the path and its
// value in Params. The request sent stays the same. A route with optional,
// rest-of-path or partial-segment parameters keeps the path as it is.
func (b *Builder) template(c *Candidate, relPath, rawQuery string) {
	r := routes.Match(b.o.Routes, catalog.Request{Target: b.o.Target, Method: c.Method, Path: relPath})
	if r == nil {
		return
	}
	c.Route = r.Path
	rs, ps := strings.Split(r.Path, "/"), strings.Split(relPath, "/")
	if len(rs) != len(ps) {
		return
	}
	params := map[string]string{}
	out := make([]string, len(rs))
	for i, seg := range rs {
		m := simpleParam().FindStringSubmatch(seg)
		if m == nil {
			if strings.Contains(seg, "{") {
				return
			}
			out[i] = ps[i]
			continue
		}
		v, err := url.PathUnescape(ps[i])
		if err != nil || v == "" {
			return
		}
		if prev, ok := params[m[1]]; ok && prev != v {
			return
		}
		params[m[1]], out[i] = v, seg
	}
	if len(params) == 0 {
		return
	}
	c.Params, c.Path = params, strings.Join(out, "/")
	if rawQuery != "" {
		c.Path += "?" + rawQuery
	}
}

// Key tells requests apart the way Add does, so a catalog request that is
// already there can be recognised: with Options.Routes by service, method,
// route, query parameter names, identity and body shape; without, by the
// service, method, filled path and body.
func (b *Builder) Key(r catalog.Request) string {
	filled := r.Path
	for name, v := range r.Params {
		filled = strings.ReplaceAll(filled, "{"+name+"}", v)
	}
	if b.o.Routes == nil {
		return r.Target + " " + r.Method + " " + filled + " " + string(r.Body)
	}
	p, query, _ := strings.Cut(filled, "?")
	if route := routes.Match(b.o.Routes, r); route != nil {
		p = route.Path
	}
	q, _ := url.ParseQuery(query)
	var form []string
	if r.Multipart != nil {
		for _, f := range r.Multipart.Fields {
			form = append(form, f[0])
		}
		for _, f := range r.Multipart.Files {
			form = append(form, f.Field+":"+f.Synthetic)
		}
	}
	return strings.Join([]string{r.Target, r.Method, p, strings.Join(sortedKeys(q), "&"), r.Identity, shape(r.Body), strings.Join(form, "&")}, " ")
}

// shape is a JSON body with every value replaced by a value of its type, and
// an array by its first element: what a body looks like, not what it holds.
func shape(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return string(body)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				x[k] = walk(val)
			}
			return x
		case []any:
			if len(x) == 0 {
				return x
			}
			return []any{walk(x[0])}
		case string:
			return ""
		case float64:
			return 0
		case bool:
			return false
		}
		return nil
	}
	out, _ := json.Marshal(walk(v))
	return string(out)
}
