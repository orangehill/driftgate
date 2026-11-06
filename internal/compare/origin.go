package compare

import (
	"bytes"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/sheet"
)

// Applications build absolute URLs from the Host of the request (Laravel's
// url(), asset links of a CMS): the same code served at another address
// writes another origin into its bodies and its Location headers. When the
// two sides were captured from different origins, the origins of both (and
// the aliases) are replaced by one neutral origin on both sides before
// anything is compared, in memory only; a URL of another host, port, scheme
// or path still differs. Both sides' origins and not only each side's own:
// an application that writes a fixed address (APP_URL) writes the same one
// on both sides, and that is no change.
const (
	neutralHost   = "origin.invalid" // .invalid never names a real host (RFC 2606)
	neutralScheme = "origin"
)

// Origin returns the origin of an http or https URL: scheme://host[:port]
// in lower case, without the default port of the scheme.
func Origin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("%q is not an http or https URL", raw)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

// origin is one origin of a side, split for searching.
type origin struct {
	scheme string
	auth   []byte // host[:port]
	port   bool
}

// originSide holds the origins replaced in one side, and what a neutral
// origin in a drift is shown as again.
type originSide struct {
	list []origin
	// neutral: the origins do not share one scheme, so an origin becomes
	// origin://origin.invalid and not <scheme>://origin.invalid.
	neutral bool
	shown   string // the side's own first origin, shown in place of the neutral one
}

// origins are the two sides' origins; nil when there is nothing to replace:
// a side without a known origin, or the same origins on both.
type origins struct {
	base, cur originSide
}

func newOrigins(base, cur []string) *origins {
	if len(base) == 0 || len(cur) == 0 || slices.Equal(base, cur) {
		return nil
	}
	pb, sb := parseOrigins(base)
	pc, sc := parseOrigins(cur)
	if len(pb) == 0 || len(pc) == 0 || maps.Equal(sb, sc) {
		return nil
	}
	all, _ := parseOrigins(append(append([]string{}, base...), cur...))
	neutral := false
	for _, o := range all {
		neutral = neutral || o.scheme != all[0].scheme
	}
	return &origins{
		base: originSide{list: all, neutral: neutral, shown: pb[0].scheme + "://" + string(pb[0].auth)},
		cur:  originSide{list: all, neutral: neutral, shown: pc[0].scheme + "://" + string(pc[0].auth)},
	}
}

// ownOrigins writes both sides' origins as one neutral origin, for what
// expectations digest; nil when no side knows its origin.
func ownOrigins(base, cur []string) *originSide {
	all, _ := parseOrigins(append(append([]string{}, base...), cur...))
	if len(all) == 0 {
		return nil
	}
	neutral := false
	for _, o := range all {
		neutral = neutral || o.scheme != all[0].scheme
	}
	return &originSide{list: all, neutral: neutral}
}

// parseOrigins reads a side's origins, in order and without repeats;
// what is not an origin is left out.
func parseOrigins(list []string) ([]origin, map[string]bool) {
	var out []origin
	seen := map[string]bool{}
	for _, s := range list {
		o, err := Origin(s)
		if err != nil || seen[o] {
			continue
		}
		seen[o] = true
		u, _ := url.Parse(o)
		out = append(out, origin{scheme: u.Scheme, auth: []byte(u.Host), port: u.Port() != ""})
	}
	return out, seen
}

// replace returns b with the origins replaced; b itself is never
// changed.
func (s *originSide) replace(b []byte) []byte {
	for _, o := range s.list {
		b = o.replace(b, s.neutral)
	}
	return b
}

func (s *originSide) text(v string) string {
	for _, o := range s.list {
		if strings.Contains(v, string(o.auth)) {
			return string(s.replace([]byte(v)))
		}
	}
	return v
}

// body replaces the origins in a body. An XLSX file is a ZIP archive whose
// bytes must stay as they are; its cells are replaced after reading.
func (s *originSide) body(side Side) []byte {
	if sheetKind(side) == "xlsx" {
		return side.Body
	}
	return s.replace(side.Body)
}

func (s *originSide) headers(hs [][2]string) [][2]string {
	var out [][2]string
	for i, h := range hs {
		if v := s.text(h[1]); v != h[1] {
			if out == nil {
				out = append([][2]string{}, hs...)
			}
			out[i][1] = v
		}
	}
	if out == nil {
		return hs
	}
	return out
}

// restore shows a neutral origin in a drift as the side's own first origin.
func (s *originSide) restore(v string) string {
	if !strings.Contains(v, neutralHost) {
		return v
	}
	scheme, auth, _ := strings.Cut(s.shown, "://")
	v = strings.ReplaceAll(v, neutralScheme+"://"+neutralHost, s.shown)
	v = strings.ReplaceAll(v, neutralScheme+`:\/\/`+neutralHost, scheme+`:\/\/`+auth)
	return strings.ReplaceAll(v, neutralHost, auth)
}

// restore shows the values of drifts with the sides' own origins.
func (o *origins) restore(drifts []Drift) {
	for i := range drifts {
		drifts[i].Baseline = o.base.restore(drifts[i].Baseline)
		drifts[i].Current = o.cur.restore(drifts[i].Current)
	}
}

// cells replaces the origins in the cells of read spreadsheets: their
// values and formulas.
func (s *originSide) cells(sheets []sheet.Sheet) {
	for _, sh := range sheets {
		for ref, v := range sh.Cells {
			if r := s.text(v); r != v {
				sh.Cells[ref] = r
			}
		}
		for ref, m := range sh.Meta {
			if r := s.text(m.Formula); r != m.Formula {
				m.Formula = r
				sh.Meta[ref] = m
			}
		}
	}
}

// replace replaces every occurrence of the origin in b. An occurrence is its
// host[:port] between boundaries (not inside a longer host name, port or
// user name: localhost:80801, a.localhost:8080, localhost:8080@evil), in
// one of three forms:
//
//   - <scheme>://host[:port], also escaped as in JSON (http:\/\/…), with the
//     origin's own scheme: the whole origin is replaced;
//   - //host[:port] after another scheme or none (a scheme-relative URL):
//     the host is replaced and the scheme kept, so a change of scheme
//     still shows;
//   - a bare host:port, only with a port: a host name alone is too common
//     a word (an e-mail domain, a database host) to be taken for the
//     application's address.
func (o origin) replace(b []byte, neutral bool) []byte {
	var out []byte
	last, from := 0, 0
	for {
		i := bytes.Index(b[from:], o.auth)
		if i < 0 {
			break
		}
		i += from
		end := i + len(o.auth)
		from = i + 1
		if end < len(b) && authByte(b[end]) {
			continue
		}
		start, repl := o.form(b, i, neutral)
		if start < 0 {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(b))
		}
		out = append(append(out, b[last:start]...), repl...)
		last, from = end, end
	}
	if out == nil {
		return b
	}
	return append(out, b[last:]...)
}

// form tells where the occurrence of the origin's host at i starts and what
// replaces it; start is -1 when it is not one of the forms.
func (o origin) form(b []byte, i int, neutral bool) (start int, repl string) {
	before := b[:i]
	for _, sep := range []string{"://", `:\/\/`} {
		if p := o.scheme + sep; bytes.HasSuffix(before, []byte(p)) {
			if k := i - len(p); k == 0 || !schemeByte(b[k-1]) {
				scheme := o.scheme
				if neutral {
					scheme = neutralScheme
				}
				return k, scheme + sep + neutralHost
			}
		}
	}
	if bytes.HasSuffix(before, []byte("//")) || bytes.HasSuffix(before, []byte(`\/\/`)) {
		return i, neutralHost
	}
	if o.port && (i == 0 || !hostByte(b[i-1])) {
		return i, neutralHost
	}
	return -1, ""
}

func hostByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
}

// authByte: a byte that would continue the host or port into another one.
func authByte(c byte) bool { return hostByte(c) || c == ':' || c == '@' }

func schemeByte(c byte) bool { return hostByte(c) || c == '+' }
