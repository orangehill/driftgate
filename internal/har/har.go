// Package har turns requests into catalog candidates: those of a HAR file
// (saved from the browser's developer tools while using the application)
// and those the recording proxy sees feed the same pipeline (Builder). Only
// requests to the target are taken, without static files, and of the headers
// only Accept and Content-Type: cookies and tokens never enter the catalog. A
// request whose query, body or path carries a credential or personal data is
// left out and reported, to be added by hand: a credential is a parameter or
// key named like one (password, currentPassword, authToken, jwt, ticket…) or
// a value shaped like one (a JWT, a long random token; see secret.go).
package har

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"path"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/pii"
)

var detector, _ = pii.New(nil)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Candidate is one request found in the file.
type Candidate struct {
	Method  string
	Path    string // relative to the target, query included; {name} where a route parameter is
	Params  map[string]string
	Headers [][2]string
	Body    json.RawMessage
	Auth    bool // the browser sent Authorization or a cookie
	// Identity is the configured identity whose token the browser sent.
	Identity string
	// Route is the inventory route the request reaches (its path), when
	// Options.Routes is given.
	Route string
	// Personal: the response held personal data, so its body must be kept
	// private. The response itself is never kept.
	Personal bool
	// Multipart is a form with files: the fields are kept, every file is a
	// synthetic one of the same kind.
	Multipart *catalog.Multipart
}

// Note is an entry that was left out, with the reason.
type Note struct {
	Request string
	Reason  string
}

type file struct {
	Log struct {
		Entries []struct {
			Request struct {
				Method  string `json:"method"`
				URL     string `json:"url"`
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
				PostData *struct {
					MimeType string     `json:"mimeType"`
					Text     string     `json:"text"`
					Params   []harParam `json:"params"`
				} `json:"postData"`
			} `json:"request"`
			Response struct {
				Content struct {
					Size     int64  `json:"size"`
					MimeType string `json:"mimeType"`
					Text     string `json:"text"`
					Encoding string `json:"encoding"`
				} `json:"content"`
			} `json:"response"`
		} `json:"entries"`
	} `json:"log"`
}

var static = lazy.Regexp(`(?i)\.(js|mjs|css|map|png|jpe?g|gif|svg|webp|avif|ico|woff2?|ttf|eot|otf|mp4|webm|mp3|pdf)$`)

// noise are paths of a development server and its tooling that are never
// catalog requests.
var noise = []string{
	"/_next/static/*", "/_next/image*", "/_next/webpack-hmr*", "/__nextjs*",
	"/_nuxt/*", "/build/*", "/@vite/*", "/@fs/*", "/@id/*", "/@react-refresh*", "/__vite_ping*",
	"/node_modules/*", "/sockjs-node/*", "/_debugbar/*",
	"/favicon*", "/apple-touch-icon*", "/robots.txt", "/site.webmanifest",
}

// Static reports a path that is never a catalog candidate: a static file
// (script, style, image, font, media) or the noise of a development server
// (/_next/static, /_nuxt, /build, Vite, the favicon).
func Static(p string) bool {
	if static().MatchString(path.Base(p)) {
		return true
	}
	for _, g := range noise {
		if glob.Match(g, p) {
			return true
		}
	}
	return false
}

// Read parses a HAR file and returns the requests to baseURL, each once.
func Read(raw []byte, baseURL string) ([]Candidate, []Note, error) {
	return ReadWith(raw, baseURL, Options{})
}

// ReadWith is Read with options: the same pipeline the proxy recording uses.
func ReadWith(raw []byte, baseURL string, o Options) ([]Candidate, []Note, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, fmt.Errorf("not a HAR file: %w", err)
	}
	b, err := NewBuilder(baseURL, o)
	if err != nil {
		return nil, nil, err
	}
	var out []*Candidate
	var notes []Note
	for _, e := range f.Log.Entries {
		req := e.Request
		entry := Entry{Method: req.Method, URL: req.URL}
		for _, h := range req.Headers {
			entry.Headers = append(entry.Headers, [2]string{h.Name, h.Value})
		}
		if req.PostData != nil {
			entry.MimeType, entry.Text, entry.params = req.PostData.MimeType, req.PostData.Text, req.PostData.Params
		}
		if content := e.Response.Content; o.Personal {
			entry.Response = []byte(content.Text)
			if content.Encoding == "base64" {
				var err error
				if entry.Response, err = base64.StdEncoding.DecodeString(content.Text); err != nil {
					entry.Response, entry.Unscanned = nil, true
				}
			}
			// A text body the file has a size for but left out (an export
			// without content) cannot be told clean.
			mt, _, _ := mime.ParseMediaType(content.MimeType)
			if content.Text == "" && content.Size > 0 && Textual(mt) {
				entry.Unscanned = true
			}
		}
		c, note := b.Add(entry)
		switch {
		case note != nil:
			notes = append(notes, *note)
		case c != nil:
			out = append(out, c)
		}
	}
	// Read only now: a later duplicate may have marked a candidate Personal.
	cands := make([]Candidate, len(out))
	for i, c := range out {
		cands[i] = *c
	}
	return cands, notes, nil
}

// Textual is a media type a scan for personal data can read.
func Textual(mt string) bool {
	return mt == "" || strings.HasPrefix(mt, "text/") || strings.Contains(mt, "json") || strings.Contains(mt, "xml")
}
