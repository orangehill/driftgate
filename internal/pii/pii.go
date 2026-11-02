// Package pii finds personal data in response bodies and headers before they
// are written as public (into git). It looks for e-mail addresses, phone
// numbers, Serbian personal numbers (JMBG), payment card numbers and IBANs;
// numbers are checked against their checksum to keep false alarms rare.
// Texts are also read URL-decoded, with HTML entities resolved and with base64
// runs decoded. Findings never carry the full value.
package pii

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/orangehill/driftgate/internal/credential"
	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/lazy"
)

// Finding is one value that looks like personal data.
type Finding struct {
	Kind   string `json:"kind"`
	Where  string `json:"where"`  // JSON path, "$" for a body that is not JSON, or header:<name>
	Sample string `json:"sample"` // masked
}

var (
	email = lazy.Regexp(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	// Phone numbers need a leading + / 00 or 0 and separators, so plain IDs
	// do not match; a Serbian mobile number (06x, +3816x, 003816x) matches
	// without separators too.
	phone = lazy.Regexp(`\+\d{1,3}[ \-/.]?\(?\d{1,4}\)?(?:[ \-/.]?\d{2,4}){2,4}\b` +
		`|\b00\d{1,3}[ \-/.]\(?\d{1,4}\)?(?:[ \-/.]?\d{2,4}){2,4}\b` +
		`|\b0\d{1,3}[ \-/]\d{2,4}[ \-/]?\d{2,4}(?:[ \-/]?\d{2,4})?\b` +
		`|(?:\+|\b00)3816\d{7,8}\b|\b06\d{7,8}\b`)
	jmbg = lazy.Regexp(`\b\d{13}\b`)
	card = lazy.Regexp(`\b\d(?:[ \-]?\d){12,18}\b`)
	iban = lazy.Regexp(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`)
	// dateTime is an ISO date (with a time), which a phone match may start at.
	dateTime = lazy.Regexp(`^\d{4}-(0\d|1[0-2])-([0-2]\d|3[01])([ T]\d{2}:\d{2}|\b)`)
	// uuid groups may be digits only: 11bca30b-0308-4725-… is no phone number.
	uuid = lazy.Regexp(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// mobile is a Serbian mobile number as a JSON number: with the country
	// code, or (under a phone key) without the leading 0.
	mobile      = lazy.Regexp(`^3816\d{7,8}$`)
	mobileLocal = lazy.Regexp(`^6\d{7,8}$`)
)

// Kinds are the kinds of personal data a Detector finds.
var Kinds = []string{"email", "phone", "jmbg", "card", "iban"}

// Kind reports whether k is one of Kinds.
func Kind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Detector finds personal data, except values matching an allow pattern.
type Detector struct {
	allow []*regexp.Regexp
}

// New builds a detector; patterns are regular expressions of allowed values.
func New(allow []string) (*Detector, error) {
	d := &Detector{}
	for _, p := range allow {
		re, err := regexp.Compile(`^(?:` + p + `)$`)
		if err != nil {
			return nil, err
		}
		d.allow = append(d.allow, re)
	}
	return d, nil
}

// step is one step of a JSON path: a key, or an index when index >= 0.
type step struct {
	key   string
	index int
}

// pathOf builds the path of a JSON value; Scan only builds it for a finding.
func pathOf(steps []step) string {
	p := "$"
	for _, s := range steps {
		if s.index >= 0 {
			p += "[" + strconv.Itoa(s.index) + "]"
		} else {
			p = jsonx.KeyPath(p, s.key)
		}
	}
	return p
}

// Scan looks at every string in a JSON body (keys included) and at its
// integers, or at the whole body when it is not JSON. It walks the body
// without building it (jsonx.Walk): the tree of a body of a few megabytes
// would take many times that. The texts it reads are views of body, not
// copies (a body read off the heap stays there); findings copy what they
// keep.
func (d *Detector) Scan(body []byte) []Finding { return d.scan(body, false) }

// ScanResponse is Scan for a response body that would be stored in git: it
// also finds credentials (kind "credential"), which must not be committed
// either. In JSON: a JWT anywhere, a string under a key named as a
// credential (access_token, csrf, api_key, signature) that is shaped like
// one, also as the value of an object whose name says so
// ({"name":"api_key","value":…}, AWS's Key and Value), and a URL with a
// credential in its query or fragment (a signed or presigned URL, a link
// with its token). In another body, see ScanText.
func (d *Detector) ScanResponse(body []byte) []Finding { return d.scan(body, true) }

func (d *Detector) scan(body []byte, creds bool) []Finding {
	text := unsafe.String(unsafe.SliceData(body), len(body)) // read only, never kept
	s := &scanner{d: d, creds: creds, jwt: creds && credential.MayHoldJWT(text)}
	if err := jsonx.Walk(body, s); err != nil {
		if creds {
			return d.ScanText("$", text)
		}
		return appendFindings(nil, "$", d.matches(text))
	}
	return s.out
}

// ScanText is ScanResponse for a text that is not JSON, such as a part of
// a document; where names it in the findings. Its credentials are a JWT, a
// URL with one in its query or fragment, and in a form body
// (access_token=…&token_type=bearer) or in XML a pair, element or
// attribute named as a credential that is shaped like one.
func (d *Detector) ScanText(where, text string) []Finding {
	out := appendFindings(nil, where, d.matches(text))
	if sample := textCredential(text); sample != "" {
		out = append(out, Finding{Kind: Credential, Where: where, Sample: sample})
	}
	return out
}

// textCredential finds a credential in a text that is not JSON (see
// ScanText) and returns it masked, or "". It reads the text a few times
// over at most: a URL is looked for only around a ? or #, a form only in
// a text without spaces, quotes or markup, and elements only in XML.
func textCredential(text string) string {
	if credential.MayHoldJWT(text) && credential.JWT(text) {
		return "JWT"
	}
	// A URL: the name=value pairs of its query and fragment
	// (credential.InQuery), read from each ? and # to where the URL ends
	// in one pass. A page repeats its parameter names.
	names := nameCache{decide: credential.Name}
	end, q, h := 0, -1, -1
	for {
		if q < end {
			if q = indexFrom(text, '?', end); q < 0 {
				q = len(text)
			}
		}
		if h < end {
			if h = indexFrom(text, '#', end); h < 0 {
				h = len(text)
			}
		}
		i := min(q, h)
		if i == len(text) {
			break
		}
		start, eq, escaped := i+1, -1, false
		for end = i + 1; ; end++ {
			last := end == len(text) || urlEnd[text[end]]
			if c := byte('&'); !last {
				if c = text[end]; c != '&' && c != '?' && c != '#' {
					if eq < 0 && c == '=' {
						eq = end
					} else if eq < 0 && (c == '%' || c == '+') {
						escaped = true
					}
					continue
				}
			}
			if eq >= start && eq+1 < end { // a pair with a value
				name := text[start:eq]
				if escaped {
					name = unescape(name)
				}
				if names.named(name) {
					return Mask(text[i:end])
				}
			}
			if last {
				break
			}
			start, eq, escaped = end+1, -1, false
		}
	}
	if strings.IndexByte(text, '=') >= 0 && !strings.ContainsAny(text, " \t\r\n\"'<>{") {
		for rest := text; rest != ""; {
			var p string
			p, rest, _ = strings.Cut(rest, "&")
			name, value, _ := strings.Cut(p, "=")
			if v := unescape(value); len(v) >= 20 && credential.Key(unescape(name)) && credential.Value(v) {
				return Mask(v)
			}
		}
		return ""
	}
	return xmlCredential(text)
}

// tokenRun reports a run of 20 characters a token is made of, without
// which credential.Value says no: most values (a date, a URL, words) fail
// it before their name is looked at.
func tokenRun(v string) bool {
	n := 0
	for i := 0; i < len(v); i++ {
		if c := v[i]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '+' || c == '=' || c == '-' {
			if n++; n == 20 {
				return true
			}
		} else {
			n = 0
		}
	}
	return false
}

// nameCache keeps what decide said of the names of a text, which repeats
// them (a page its query parameters, XML its elements): the last one, and
// the others in a map.
type nameCache struct {
	decide func(string) bool
	last   string
	isLast bool
	seen   map[string]bool
}

func (c *nameCache) named(name string) bool {
	if name == c.last && c.seen != nil {
		return c.isLast
	}
	n, ok := c.seen[name]
	if !ok {
		if c.seen == nil {
			c.seen = map[string]bool{}
		}
		n = c.decide(name)
		c.seen[name] = n
	}
	c.last, c.isLast = name, n
	return n
}

// closes reports a text that starts with the end tag of name.
func closes(t, name string) bool {
	return len(t) > len(name)+2 && t[1] == '/' && t[2:2+len(name)] == name && t[2+len(name)] == '>'
}

// urlEnd are the characters a URL in a text ends at.
var urlEnd = func() (t [256]bool) {
	for _, c := range []byte(" \t\r\n\"'<>()[]{}`") {
		t[c] = true
	}
	return t
}()

func indexFrom(s string, c byte, from int) int {
	if i := strings.IndexByte(s[from:], c); i >= 0 {
		return from + i
	}
	return -1
}

func unescape(s string) string {
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

// xmlCredential finds in an XML document (one that starts with a tag that
// is not html) an attribute or an element with text only whose name is a
// credential and whose value is shaped as one: <access_token>…</access_token>,
// <credentials token="…">. HTML is not read this way: a page names its CSRF
// token in a meta tag (name="csrf-token" content="…") and its CSP nonce, both
// public in the page by design.
func xmlCredential(text string) string {
	t := strings.TrimLeft(text, " \t\r\n\ufeff")
	if !strings.HasPrefix(t, "<") || len(t) > 1 && t[1] == '!' && !strings.HasPrefix(t, "<!--") ||
		len(t) >= 5 && strings.EqualFold(t[:5], "<html") {
		return ""
	}
	names := nameCache{decide: credential.Key}
	isNamed := names.named
	for i := strings.IndexByte(t, '<'); i >= 0; {
		j := strings.IndexByte(t[i:], '>')
		if j < 0 {
			break
		}
		tag := t[i+1 : i+j]
		next := indexFrom(t, '<', i+j)
		if tag == "" || tag[0] == '/' || tag[0] == '?' || tag[0] == '!' {
			i = next
			continue
		}
		n := 0 // the name ends at white space or /
		for n < len(tag) && tag[n] != ' ' && tag[n] != '/' && tag[n] != '\t' && tag[n] != '\r' && tag[n] != '\n' {
			n++
		}
		name, attrs := tag[:n], tag[n:]
		for attrs != "" {
			k := strings.IndexByte(attrs, '=')
			if k < 0 || k+1 >= len(attrs) || attrs[k+1] != '"' && attrs[k+1] != '\'' {
				break
			}
			attr := strings.TrimSpace(attrs[:k])
			v, rest, _ := strings.Cut(attrs[k+2:], attrs[k+1:k+2])
			if tokenRun(v) && isNamed(attr) && credential.Value(v) {
				return Mask(v)
			}
			attrs = rest
		}
		if next < 0 {
			break
		}
		if v := t[i+j+1 : next]; tokenRun(v) && closes(t[next:], name) && isNamed(name) && credential.Value(v) {
			return Mask(v)
		}
		i = next
	}
	return ""
}

// Credential is the kind of a credential ScanResponse finds.
const Credential = "credential"

// scanner collects the findings of Scan, in the order of the body.
type scanner struct {
	d     *Detector
	out   []Finding
	path  []step
	creds bool            // look for credentials too
	jwt   bool            // the body has the start of a JWT somewhere
	ended bool            // the last value was a member's or an item's: a Member now is its sibling
	keys  map[string]bool // credential.Key of the keys seen
	pairs []pair          // by depth: the name and value of the object being read there
}

// pair is the name and the value of an object that holds a setting, a
// parameter or a tag: {"name":"api_key","value":…}, AWS's Key and Value.
type pair struct {
	name, value, valueKey string
	checked               bool
}

func (s *scanner) Member(key string) {
	if d := len(s.path); !s.ended && d < len(s.pairs) {
		s.pairs[d] = pair{} // the first member of an object
	}
	s.ended = false
	s.path = append(s.path, step{key: key, index: -1})
	if ms := s.d.matches(key); len(ms) > 0 {
		s.out = appendFindings(s.out, pathOf(s.path), ms)
	}
}

func (s *scanner) Item(index int) {
	s.ended = false
	s.path = append(s.path, step{index: index})
}

func (s *scanner) End() {
	s.ended = true
	s.path = s.path[:len(s.path)-1]
}

func (s *scanner) Scalar(kind jsonx.Kind, text string) {
	switch kind {
	case jsonx.String:
		if ms := s.d.matches(text); len(ms) > 0 {
			s.out = appendFindings(s.out, pathOf(s.path), ms)
		}
		if s.creds && len(text) >= 20 && s.credential(text) {
			s.out = append(s.out, Finding{Kind: Credential, Where: pathOf(s.path), Sample: Mask(text)})
		}
		if s.creds && len(s.path) > 0 && s.path[len(s.path)-1].index < 0 {
			s.pair(s.path[len(s.path)-1].key, text)
		}
	case jsonx.Int:
		if k := s.d.number(text, lastKey(s.path)); k != "" {
			s.out = append(s.out, Finding{Kind: k, Where: pathOf(s.path), Sample: Mask(text)})
		}
	}
}

// credential reports a string of at least 20 bytes that holds a
// credential (see ScanResponse).
func (s *scanner) credential(text string) bool {
	if s.jwt && credential.JWT(text) {
		return true
	}
	if key := lastKey(s.path); key != "" && s.named(key) && credential.Value(text) {
		return true
	}
	return (strings.IndexByte(text, '?') >= 0 || strings.IndexByte(text, '#') >= 0) && credential.InQuery(text)
}

// named reports a key or a name named as a credential (credential.Key),
// decided once per key: an array of objects repeats its keys.
func (s *scanner) named(key string) bool {
	named, ok := s.keys[key]
	if !ok {
		if s.keys == nil {
			s.keys = map[string]bool{}
		}
		named = credential.Key(key)
		s.keys[key] = named // the key is a view of the body, which outlives the map
	}
	return named
}

// pair takes the string member key of the object being read when it is
// its name (name, key, Name, Key) or its value (value, Value): with both,
// a value shaped as a credential under a name of one is a credential,
// whichever comes first.
func (s *scanner) pair(key, text string) {
	switch key {
	case "name", "key", "Name", "Key", "value", "Value":
	default:
		return
	}
	d := len(s.path) - 1
	for len(s.pairs) <= d {
		s.pairs = append(s.pairs, pair{})
	}
	p := &s.pairs[d]
	if key[0] == 'v' || key[0] == 'V' {
		p.value, p.valueKey = text, key
	} else {
		p.name = text
	}
	if p.checked || p.name == "" || len(p.value) < 20 {
		return
	}
	p.checked = true
	if s.named(p.name) && credential.Value(p.value) {
		at := append(s.path[:d:d], step{key: p.valueKey, index: -1})
		s.out = append(s.out, Finding{Kind: Credential, Where: pathOf(at), Sample: Mask(p.value)})
	}
}

// Headers finds personal data in header values (a Location with an e-mail
// address, a Content-Disposition with a JMBG in the file name). It returns
// the findings and the headers with that data hidden, letters as x and
// digits as 0 (the whole value when only a decoded form held it), so the
// record that goes into git does not hold it either. h is not changed.
func (d *Detector) Headers(h [][2]string) ([]Finding, [][2]string) {
	var out []Finding
	masked := h
	for i, kv := range h {
		ms := d.matches(kv[1])
		if len(ms) == 0 {
			continue
		}
		if out == nil {
			masked = append([][2]string{}, h...)
		}
		out = appendFindings(out, "header:"+kv[0], ms)
		masked[i][1] = d.hide(kv[1])
	}
	return out, masked
}

// hide masks the personal data in s.
func (d *Detector) hide(s string) string {
	sp := d.Spans(s)
	if len(sp) == 0 {
		return maskChars(s)
	}
	b := []byte(s)
	for _, x := range sp {
		copy(b[x.Start:x.End], maskChars(s[x.Start:x.End]))
	}
	return string(b)
}

// maskChars turns ASCII letters into x and digits into 0, keeping the format.
func maskChars(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= '0' && c <= '9':
			b[i] = '0'
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			b[i] = 'x'
		}
	}
	return string(b)
}

// Allowed reports whether s matches an allow pattern: a value the project
// declared not personal.
func (d *Detector) Allowed(s string) bool { return d.allowed(s) }

func (d *Detector) allowed(s string) bool {
	for _, re := range d.allow {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// match is a value found in a text or in one of its decoded forms.
type match struct{ kind, value string }

func appendFindings(out []Finding, where string, ms []match) []Finding {
	for _, m := range ms {
		out = append(out, Finding{Kind: m.kind, Where: where, Sample: Mask(m.value)})
	}
	return out
}

// matches finds the personal data in a text and in its decoded forms, each
// value once.
func (d *Detector) matches(s string) []match {
	var out []match
	for _, sp := range d.Spans(s) {
		out = append(out, match{sp.Kind, s[sp.Start:sp.End]})
	}
	if !encoded(s) {
		return out
	}
	for _, t := range decodings(s) {
	next:
		for _, sp := range d.Spans(t) {
			m := match{sp.Kind, t[sp.Start:sp.End]}
			for _, x := range out {
				if x == m {
					continue next
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// lastKey is the key of the innermost object member on the path ("" at the
// top), skipping array indexes: phones[0] is under phones.
func lastKey(path []step) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].index < 0 {
			return path[i].key
		}
	}
	return ""
}

// number decides whether a JSON integer is personal data. Integers are mostly
// amounts, IDs and times, so only a shape that is rare by chance counts: a
// JMBG (a plausible birth date and the check digit) except under a time key,
// where epoch milliseconds sometimes have that shape, and with 12 digits (the
// day's leading 0 lost) only under a JMBG key; a card number (known prefix and length, Luhn); a
// Serbian mobile number with the country code (3816…), and one without it
// (6…, the 0 lost) only under a phone key.
func (d *Detector) number(n, key string) string {
	if len(n) < 8 || len(n) > 19 || n[0] == '-' {
		return ""
	}
	kind := ""
	switch {
	case (len(n) == 13 && validJMBG(n) || len(n) == 12 && keyWord(key, jmbgWords) && validJMBG("0"+n)) && !keyWord(key, timeWords):
		kind = "jmbg"
	case len(n) >= 13 && cardPrefix(n) && luhn(n):
		kind = "card"
	case mobile().MatchString(n), mobileLocal().MatchString(n) && keyWord(key, phoneWords):
		kind = "phone"
	}
	if kind == "" || d.allowed(n) {
		return ""
	}
	return kind
}

var (
	// phoneWords name a phone number in a key: phone, mobile_phone, phoneNumber.
	phoneWords = map[string]bool{"phone": true, "mobile": true, "mob": true, "tel": true, "telephone": true,
		"telefon": true, "mobilni": true, "fax": true, "gsm": true, "msisdn": true}
	jmbgWords = map[string]bool{"jmbg": true, "embg": true, "emso": true, "umcn": true, "maticni": true}
	// timeWords name a time: ts, created_at, updatedAt, expires, timestamp.
	timeWords = map[string]bool{"ts": true, "time": true, "timestamp": true, "date": true, "at": true, "on": true,
		"created": true, "updated": true, "modified": true, "deleted": true, "expires": true, "expiry": true,
		"exp": true, "iat": true, "nbf": true, "since": true, "until": true, "millis": true, "ms": true, "epoch": true}
)

// keyWord reports a key with a word of words, the words split at _ - . and
// space and in camelCase.
func keyWord(key string, words map[string]bool) bool {
	start := 0
	for i := 0; i <= len(key); i++ {
		end := i == len(key) || key[i] == '_' || key[i] == '-' || key[i] == '.' || key[i] == ' '
		camel := !end && i > start && key[i] >= 'A' && key[i] <= 'Z' && key[i-1] >= 'a' && key[i-1] <= 'z'
		if end || camel {
			if words[strings.ToLower(key[start:i])] {
				return true
			}
			start = i
			if end {
				start++
			}
		}
	}
	return false
}

// Span is where a value that looks like personal data sits in a text: the
// byte offsets of the match and its kind.
type Span struct {
	Kind       string
	Start, End int
}

// Spans finds the personal data in a text, by kind (e-mail, JMBG, card, IBAN,
// phone) and then by position; matches of different kinds may overlap. The
// text is read as it is, not decoded.
func (d *Detector) Spans(s string) []Span { return d.spans(s, true) }

// spans is Spans; without the prefilter every pattern runs. The prefilter
// only skips patterns that cannot match (a test compares both on a corpus).
func (d *Detector) spans(s string, prefilter bool) []Span {
	f := filter{at: true, iban: true, jmbg: 13, card: 13, phone: 8, runs: [][2]int{{0, len(s)}}}
	if prefilter {
		if f = scanFilter(s); !f.at && !f.iban && f.card < 13 && f.phone < 8 {
			return nil
		}
	}
	var out []Span
	add := func(kind string, ix []int) {
		if !d.allowed(s[ix[0]:ix[1]]) {
			out = append(out, Span{Kind: kind, Start: ix[0], End: ix[1]})
		}
	}
	if f.at {
		for _, ix := range email().FindAllStringIndex(s, -1) {
			add("email", ix)
		}
	}
	if f.jmbg >= 13 {
		for _, ix := range jmbg().FindAllStringIndex(s, -1) {
			if validJMBG(s[ix[0]:ix[1]]) {
				add("jmbg", ix)
			}
		}
	}
	if f.card >= 13 {
		for _, ix := range card().FindAllStringIndex(s, -1) {
			if n := digitsOf(s[ix[0]:ix[1]]); len(n) >= 13 && cardPrefix(n) && luhn(n) {
				add("card", ix)
			}
		}
	}
	if f.iban {
		for _, ix := range iban().FindAllStringIndex(s, -1) {
			if validIBAN(strings.ReplaceAll(s[ix[0]:ix[1]], " ", "")) {
				add("iban", ix)
			}
		}
	}
	if f.phone < 8 {
		return out
	}
	var uuids [][]int
	found := false
	for _, r := range f.runs {
		// The run with the character on each side, so \b reads as in s.
		lo, hi := max(r[0]-1, 0), min(r[1]+1, len(s))
		for _, ix := range phone().FindAllStringIndex(s[lo:hi], -1) {
			ix[0], ix[1] = ix[0]+lo, ix[1]+lo
			if !found {
				uuids, found = uuid().FindAllStringIndex(s, -1), true
			}
			if phoneNumber(s, ix, uuids) {
				add("phone", ix)
			}
		}
	}
	return out
}

// phoneNumber reports a match of the phone pattern that is a phone number:
// not in a UUID, a list of decimals or a date, and of 8 to 15 digits.
func phoneNumber(s string, ix []int, uuids [][]int) bool {
	if within(uuids, ix) {
		return false
	}
	if partOfDecimal(s, ix[0], ix[1]) {
		return false // CSS and SVG numbers: oklch(98.4% .003 247.858), 356.094 104 378.661
	}
	if dateTime().MatchString(s[ix[0]:]) {
		return false // a date: 0000-00-00 00:00:00 (MySQL's zero date) has the shape of a phone number
	}
	n := digitsOf(s[ix[0]:ix[1]])
	return len(n) >= 8 && len(n) <= 15
}

// filter is what a text holds that each pattern needs: an @ for an e-mail
// address; two upper-case letters and two digits in a row for an IBAN; the
// most digits in a run of the characters a JMBG (digits) or a card number
// (digits, space, -) is made of; and for a phone number, the most digits in a
// run of digits, space, +()-/. counted from the first place a phone number
// can start (card >= jmbg, as its runs break less often).
type filter struct {
	at, iban          bool
	jmbg, card, phone int
	runs              [][2]int // the phone runs with 8 digits from a start
}

// scanFilter reads s once; a text that can hold no match needs no regular
// expression, and most texts are such.
//
// Every character of a phone match is a digit or one of space +()-/., so a
// match lies in one such run; and it starts at a + or at a 0 after a word
// boundary, that is a 0 not right after a digit (and not 0, a digit and a .
// as in the seconds 05.123456, which no alternative of the pattern starts
// with). A reported number has at least 8 digits, so a run with fewer than 8
// digits from its first such start holds none, and the pattern only reads the
// other runs (runs). A UUID that stands alone (no word or run character next
// to it) is one the phone loop skips matches in, and every run that touches
// it lies inside it, so it counts no start. That skips ISO dates and times
// (2024-01-15T10:30:00Z, 2024-01-15 10:30:00, 2024-01-15T10:30:05.000000Z)
// and UUIDs, most of the phone pattern's work on API responses.
func scanFilter(s string) filter {
	var f filter
	run, card, phone := 0, 0, 0
	start := false // the phone run has a place where a phone number can start
	from := 0      // where the phone run began
	uuidEnd := 0   // the end of a UUID that stands alone, where no start counts
	if soleUUID(s, 0) {
		uuidEnd = 36
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if c == '0' && i >= uuidEnd && (i == 0 || s[i-1] < '0' || s[i-1] > '9') &&
				(i+2 >= len(s) || s[i+1] < '0' || s[i+1] > '9' || s[i+2] != '.') {
				start = true
			}
			run, card = run+1, card+1
			if start {
				phone++
			}
			f.jmbg, f.card, f.phone = max(f.jmbg, run), max(f.card, card), max(f.phone, phone)
			if !f.iban && run == 2 && i >= 3 && upper(s[i-2]) && upper(s[i-3]) {
				f.iban = true
			}
			continue
		case c == ' ' || c == '-':
		case c == '+':
			card, start = 0, start || i >= uuidEnd
		case c == '(' || c == ')' || c == '/' || c == '.':
			card = 0
		default:
			if phone >= 8 {
				f.runs = append(f.runs, [2]int{from, i})
			}
			card, phone, start = 0, 0, false
			if letter(c) {
				// A run of letters only ends the other runs.
				for i+1 < len(s) && letter(s[i+1]) {
					i++
				}
			} else if c == '@' {
				f.at = true
			} else if c != '_' && i+1 >= uuidEnd && soleUUID(s, i+1) {
				uuidEnd = i + 37 // c is no word or run character
			}
			from = i + 1
		}
		run = 0
	}
	if phone >= 8 {
		f.runs = append(f.runs, [2]int{from, len(s)})
	}
	return f
}

func upper(c byte) bool { return c >= 'A' && c <= 'Z' }

func letter(c byte) bool { return c|0x20-'a' < 26 }

// phoneOrWord reports a character of a phone run or of a regexp word (\w).
func phoneOrWord(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' ||
		c == ' ' || c == '-' || c == '+' || c == '(' || c == ')' || c == '/' || c == '.'
}

// soleUUID reports a UUID at s[i:] with no word or phone run character right
// after it; with none right before it either (the caller checks), uuid finds
// it and every phone match that touches it lies inside it. Most texts fail
// at the first check, a - at s[i+8].
func soleUUID(s string, i int) bool {
	if len(s)-i < 36 || s[i+8] != '-' || len(s)-i > 36 && phoneOrWord(s[i+36]) {
		return false
	}
	for j := 0; j < 36; j++ {
		c := s[i+j]
		if j == 8 || j == 13 || j == 18 || j == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func digitsOf(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b = append(b, s[i])
		}
	}
	return string(b)
}

// within reports a match inside one of spans.
func within(spans [][]int, ix []int) bool {
	for _, sp := range spans {
		if sp[0] <= ix[0] && ix[1] <= sp[1] {
			return true
		}
	}
	return false
}

// partOfDecimal reports a match that starts right after a decimal point or
// comma, or runs into one ("1.5"): a list of numbers, not a phone number.
func partOfDecimal(s string, start, end int) bool {
	if start > 0 && (s[start-1] == '.' || s[start-1] == ',') {
		return true
	}
	return end+1 < len(s) && (s[end] == '.' || s[end] == ',') && s[end+1] >= '0' && s[end+1] <= '9'
}

// Mask keeps the first character and the length class of a value.
func Mask(s string) string {
	r := []rune(s)
	if len(r) <= 2 {
		return strings.Repeat("*", len(r))
	}
	return string(r[0]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1])
}

// validJMBG checks the check digit of a JMBG and that it starts with a
// plausible birth date (DDMMYYY: a real day of a year from 1900 until now).
func validJMBG(s string) bool {
	var d [13]int
	for i, c := range s {
		d[i] = int(c - '0')
	}
	day, month, y := d[0]*10+d[1], d[2]*10+d[3], d[4]*100+d[5]*10+d[6]
	switch {
	case y >= 900:
		y += 1000
	case y <= time.Now().Year()-2000:
		y += 2000
	default:
		return false
	}
	if month < 1 || month > 12 || day < 1 || day > time.Date(y, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return false
	}
	w := []int{7, 6, 5, 4, 3, 2, 7, 6, 5, 4, 3, 2}
	sum := 0
	for i, x := range w {
		sum += x * d[i]
	}
	k := 11 - sum%11
	if k > 9 {
		k = 0
	}
	return k == d[12]
}

func cardPrefix(n string) bool {
	switch {
	case n[0] == '4':
		return len(n) == 13 || len(n) == 16 || len(n) == 19
	case n[0] == '5' && n[1] >= '1' && n[1] <= '5':
		return len(n) == 16
	case n[0] == '3' && (n[1] == '4' || n[1] == '7'):
		return len(n) == 15
	case n[0] == '6':
		return len(n) == 16
	}
	return false
}

func luhn(n string) bool {
	sum, double := 0, false
	for i := len(n) - 1; i >= 0; i-- {
		x := int(n[i] - '0')
		if double {
			if x *= 2; x > 9 {
				x -= 9
			}
		}
		sum += x
		double = !double
	}
	return sum%10 == 0
}

func validIBAN(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	// The number (letters as 10…35) modulo 97, a digit at a time.
	r := s[4:] + s[:4]
	m := 0
	for i := 0; i < len(r); i++ {
		switch c := r[i]; {
		case c >= '0' && c <= '9':
			m = (m*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			m = (m*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return m == 1
}
