package pii

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNumbers: personal data written as a JSON number is found like the same
// value in a string.
func TestNumbers(t *testing.T) {
	d, _ := New(nil)
	for body, kind := range map[string]string{
		`{"jmbg": 1505985710129}`:                    "jmbg",
		`{"card": 4111111111111111}`:                 "card",
		`{"cards": [5555555555554444]}`:              "card",
		`{"jmbg": 101990710008}`:                     "jmbg", // 0101990710008, the 0 lost
		`{"phone": 381641234567}`:                    "phone",
		`{"user": {"mobilePhone": 641234567}}`:       "phone",
		`{"contacts": {"tel": [[612345678]]}}`:       "phone",
		`{"x": {"y": [1, 2, {"z": 1505985710129}]}}`: "jmbg",
	} {
		fs := d.Scan([]byte(body))
		if len(fs) != 1 || fs[0].Kind != kind {
			t.Errorf("%s: %+v", body, fs)
			continue
		}
		if strings.Contains(fs[0].Sample, "1111111") || strings.Contains(fs[0].Sample, "985710") {
			t.Errorf("%s: sample is not masked: %s", body, fs[0].Sample)
		}
	}
	if fs := d.Scan([]byte(`{"x": {"y": [1, 2, {"z": 1505985710129}]}}`)); len(fs) != 1 || fs[0].Where != "$.x.y[2].z" {
		t.Errorf("path: %+v", fs)
	}
	for _, body := range []string{
		`{"id": 641234567}`,           // no phone key: an ID
		`{"amount": 1505985710129.5}`, // not an integer
		`{"n": -4111111111111111}`,
		`{"jmbg": 3213985710129}`,      // no such day
		`{"ts": 1730000000000}`,        // epoch millis
		`{"createdAt": 1505985710129}`, // epoch millis under a time key, even with a JMBG's shape
	} {
		if fs := d.Scan([]byte(body)); len(fs) != 0 {
			t.Errorf("%s: %+v", body, fs)
		}
	}
	a, _ := New([]string{`4111111111111111`})
	if fs := a.Scan([]byte(`{"card": 4111111111111111}`)); len(fs) != 0 {
		t.Errorf("allowed number reported: %+v", fs)
	}
}

// TestMobileWithoutSeparators: a Serbian mobile number written without
// separators, locally or with the country code.
func TestMobileWithoutSeparators(t *testing.T) {
	d, _ := New(nil)
	for _, s := range []string{"0641234567", "064123456", "+381641234567", "00381641234567", "zovite 0601234567.", "tel:+381601234567"} {
		if sp := d.Spans(s); len(sp) != 1 || sp[0].Kind != "phone" {
			t.Errorf("%q: %+v", s, sp)
		}
	}
	for _, s := range []string{"0012345678901", "2024-06-01", "06.12.2024", "0601", "0641234567890", "100641234567", "1064123456"} {
		if sp := d.Spans(s); len(sp) != 0 {
			t.Errorf("%q: %+v", s, sp)
		}
	}
}

// TestEncodedValues: a value URL-encoded, as an HTML entity or in base64 is
// found in its decoded form.
func TestEncodedValues(t *testing.T) {
	d, _ := New(nil)
	b64 := base64.StdEncoding.EncodeToString([]byte("jane@example.com"))
	jwt := "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"jane@example.com"}`)) + ".c2ln"
	for _, body := range []string{
		`{"u": "/x?e=jane%40example.com"}`,
		`{"u": "/x?e=jane%2540example.com"}`, // twice
		`<p>jane&#64;example.com</p>`,
		`<p>jane&amp;#64;example.com</p>`, // twice
		`<a href="mailto:jane&#x40;example&#46;com">`,
		`{"t": "` + b64 + `"}`,
		`{"token": "` + jwt + `"}`,
		`{"u": "100% sigurno: jane%40example.com"}`,
		`{"jmbg": "MDEwMTk5MDcxMDAwOA=="}`, // base64 of a JMBG
	} {
		fs := d.Scan([]byte(body))
		if len(fs) != 1 {
			t.Errorf("%s: %+v", body, fs)
			continue
		}
		if strings.Contains(fs[0].Sample, "example") || strings.Contains(fs[0].Sample, "199071") {
			t.Errorf("%s: sample is not masked: %s", body, fs[0].Sample)
		}
	}
	for _, body := range []string{
		`{"a": "100%", "b": "R&D", "c": "internationalization", "d": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`,
		`{"img": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="}`,
		`{"u": "/x?e=jane%40example"}`,
		`<p>R&amp;D &ndash; 011&nbsp;123&nbsp;4567 &quot;x&quot;</p>`, // NBSP is no separator
	} {
		if fs := d.Scan([]byte(body)); len(fs) != 0 {
			t.Errorf("%s: %+v", body, fs)
		}
	}
	// A value both plain and encoded is one finding.
	if fs := d.Scan([]byte(`{"u": "jane@example.com?e=jane%40example.com"}`)); len(fs) != 1 {
		t.Errorf("twice: %+v", fs)
	}
}

// TestDecodingIsBounded: deep encodings stop after maxDecode layers, a text
// yields at most maxViews decoded forms, and a long text is decoded only
// around its escapes.
func TestDecodingIsBounded(t *testing.T) {
	s := "jane@example.com"
	for i := 0; i < maxDecode+2; i++ {
		s = strings.ReplaceAll(s, "%", "%25")
		s = strings.ReplaceAll(s, "@", "%40")
	}
	if len(decodings(s)) > maxDecode {
		t.Errorf("%d decodings", len(decodings(s)))
	}
	var many []string
	for i := 0; i < 100; i++ {
		many = append(many, base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("tekst broj %04d ovde", i))))
	}
	if n := len(decodeLayers(strings.Join(many, " "))); n > maxViews {
		t.Errorf("%d decodings", n)
	}
	long := strings.Repeat("News and announcements. ", 100)
	d, _ := New(nil)
	for _, enc := range []string{"jane%40example.com", "jane&#64;example.com", many[0] + " " + base64.StdEncoding.EncodeToString([]byte("jane@example.com"))} {
		body := long + "Write to " + enc + ". " + long
		if fs := d.Scan([]byte(body)); len(fs) != 1 || fs[0].Kind != "email" {
			t.Errorf("%s in a long text: %+v", enc, fs)
		}
		w := escapes(body)
		if n := w[len(w)-1][1] - w[0][0]; n > 4*window {
			t.Errorf("%s: %d bytes decoded of %d", enc, n, len(body))
		}
	}
}

// TestHeaders: personal data in a header value is found and hidden.
func TestHeaders(t *testing.T) {
	d, _ := New(nil)
	h := [][2]string{
		{"content-disposition", `attachment; filename="Petar Petrovic 0101990710008.pdf"`},
		{"content-type", "application/pdf"},
		{"location", "/users/jane.doe@example.com"},
		{"x-user", "jane%40example.com"},
	}
	fs, masked := d.Headers(h)
	want := []string{"jmbg header:content-disposition", "email header:location", "email header:x-user"}
	if len(fs) != len(want) {
		t.Fatalf("%+v", fs)
	}
	for i, f := range fs {
		if f.Kind+" "+f.Where != want[i] {
			t.Errorf("%d: %+v", i, f)
		}
	}
	if masked[0][1] != `attachment; filename="Petar Petrovic 0000000000000.pdf"` || masked[1][1] != "application/pdf" ||
		masked[2][1] != "/users/xxxx.xxx@xxxxxxx.xxx" || masked[3][1] != "xxxx%00xxxxxxx.xxx" {
		t.Errorf("masked: %q", masked)
	}
	if h[2][1] != "/users/jane.doe@example.com" {
		t.Error("the input was changed")
	}
	clean := [][2]string{{"content-type", "application/json"}}
	if fs, m := d.Headers(clean); fs != nil || &m[0] != &clean[0] {
		t.Errorf("clean headers: %+v %v", fs, m)
	}
}

// TestPrefilterKeepsMatches: Spans with the prefilter finds exactly what
// every pattern finds, on the strings of a corpus and on random texts made
// of digits, separators, letters and @.
func TestPrefilterKeepsMatches(t *testing.T) {
	d, _ := New([]string{`user1\d@example\.com`})
	texts := strings.Split(string(corpus(2000, true)), `"`)
	rnd := rand.New(rand.NewPCG(5, 6))
	alphabet := "0123456789000000666 -/.+()@abcXYZRS"
	for i := 0; i < 200000; i++ {
		b := make([]byte, 1+rnd.IntN(40))
		for j := range b {
			b[j] = alphabet[rnd.IntN(len(alphabet))]
		}
		texts = append(texts, string(b))
	}
	for i := 0; i < 2000; i++ {
		texts = append(texts, fmt.Sprintf("RS35260005601001611379 %d 0101990710008 +381 64 %d x@y.rs 4111-1111-1111-1111", i, 1000000+i))
	}
	found := 0
	for _, s := range texts {
		a, b := d.spans(s, true), d.spans(s, false)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("%q: prefilter %v, all patterns %v", s, a, b)
		}
		found += len(a)
	}
	if found < 10000 {
		t.Errorf("only %d matches: the corpus does not exercise the patterns", found)
	}
}

// TestPrefilterDatesAndUUIDs: the phone part of the prefilter (a run needs 8
// digits from a + or a 0 that starts a group) keeps every match on texts made
// of times, UUIDs, phone numbers and IDs glued with separators, and lets ISO
// times and typical UUIDs skip the phone pattern.
func TestPrefilterDatesAndUUIDs(t *testing.T) {
	d, _ := New(nil)
	rnd := rand.New(rand.NewPCG(7, 8))
	hex := func(n int, digits bool) string {
		const h = "0123456789abcdefABCDEF"
		b := make([]byte, n)
		for i := range b {
			if digits {
				b[i] = h[rnd.IntN(10)]
			} else {
				b[i] = h[rnd.IntN(len(h))]
			}
		}
		return string(b)
	}
	t0 := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05.000000Z07:00",
		"2006-01-02", "20060102T150405Z", "0000-00-00 00:00:00"}
	tokens := []func() string{
		func() string {
			at := t0.Add(time.Duration(rnd.Int64N(60 * 365 * 24 * 3600 * 1e9)))
			if rnd.IntN(3) == 0 {
				at = at.In(time.FixedZone("", (rnd.IntN(27)-12)*3600))
			}
			return at.Format(layouts[rnd.IntN(len(layouts))])
		},
		func() string {
			all := rnd.IntN(2) == 0
			return hex(8, all) + "-" + hex(4, all || rnd.IntN(2) == 0) + "-" + hex(4, all) + "-" +
				hex(4, all || rnd.IntN(2) == 0) + "-" + hex(12, all || rnd.IntN(2) == 0)
		},
		func() string {
			return []string{"+381 64 ", "+3816", "00381 ", "064/", "011 ", "06", "0", "+"}[rnd.IntN(8)]
		},
		func() string { return strconv.Itoa(rnd.IntN(1 << uint(rnd.IntN(40)+1))) },
		func() string { return hex(1+rnd.IntN(12), rnd.IntN(2) == 0) },
		func() string { return string(" -/.+()@:TZ,\"x_"[rnd.IntN(15)]) },
	}
	// Whole bodies too: a text with many runs, read run by run.
	texts := []string{string(page(500)), string(corpus(100, true)), string(stamps(300))}
	for i := 0; i < 60000; i++ {
		var b strings.Builder
		for k := 1 + rnd.IntN(6); k > 0; k-- {
			b.WriteString(tokens[rnd.IntN(len(tokens))]())
		}
		texts = append(texts, b.String())
	}
	found := 0
	for _, s := range texts {
		a, b := d.spans(s, true), d.spans(s, false)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Fatalf("%q: prefilter %v, all patterns %v", s, a, b)
		}
		for _, x := range a {
			if x.Kind == "phone" {
				found++
			}
		}
	}
	if found < 5000 {
		t.Errorf("only %d phone matches: the texts do not exercise the pattern", found)
	}
	for _, s := range []string{"2024-01-15T10:30:00Z", "2024-01-15 10:30:00", "2024-01-15T10:30:00.123456+02:00",
		"1999-12-31", "550e8400-e29b-41d4-a716-446655440000", "11bca30b-0308-4725-a123-4b5c6d7e8f90"} {
		if f := scanFilter(s); f.phone >= 8 {
			t.Errorf("%q: the phone pattern still runs (%d digits)", s, f.phone)
		}
	}
}

// TestNumberFalsePositives measures how often ordinary integers pass as
// personal data (amounts, epoch times, IDs) and keeps the rates below the
// measured ones. A false alarm stores the body as private and asks for
// pii.allow; it never leaks.
func TestNumberFalsePositives(t *testing.T) {
	d, _ := New(nil)
	rnd := rand.New(rand.NewPCG(7, 8))
	from := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	to := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	for _, c := range []struct {
		name string
		gen  func() int64
		max  float64 // share of values reported
	}{
		{"amount in cents below 10^9", func() int64 { return rnd.Int64N(1e9) }, 0},
		{"amount in cents below 10^11", func() int64 { return rnd.Int64N(1e11) }, 0.0002}, // 3816xxxxxxx
		{"sequential ID below 10^12", func() int64 { return rnd.Int64N(1e12) }, 0.0002},
		{"epoch seconds 2000-2035", func() int64 { return from/1000 + rnd.Int64N((to-from)/1000) }, 0},
		{"epoch millis 2000-2035", func() int64 { return from + rnd.Int64N(to-from) }, 0.002}, // DDMM of a JMBG
		{"epoch micros 2000-2035", func() int64 { return (from + rnd.Int64N(to-from)) * 1000 }, 0},
		{"order ID YYYYMMDD + 6 digits", func() int64 { return 20240101*1e6 + rnd.Int64N(1e10) }, 0},
		{"snowflake ID (19 digits)", func() int64 { return 1e18 + rnd.Int64N(8e17) }, 0},
		{"9-digit ID starting with 6", func() int64 { return 6e8 + rnd.Int64N(1e8) }, 0},
		{"12-digit ID starting with 3816", func() int64 { return 3816e8 + rnd.Int64N(1e8) }, 1},
		{"random 13-digit number", func() int64 { return 1e12 + rnd.Int64N(9e12) }, 0.015},
		{"random 16-digit number", func() int64 { return 1e15 + rnd.Int64N(8e15) }, 0.035},
	} {
		const n = 500000
		hits := 0
		for i := 0; i < n; i++ {
			if d.number(strconv.FormatInt(c.gen(), 10), "id") != "" {
				hits++
			}
		}
		r := float64(hits) / n
		t.Logf("%-32s %8.4f %%", c.name, 100*r)
		if r > c.max {
			t.Errorf("%s: %.4f %% reported, at most %.4f %% expected", c.name, 100*r, 100*c.max)
		}
	}
	if fs := d.Scan(corpus(5000, false)); len(fs) != 0 {
		t.Errorf("false alarms in the corpus: %+v", fs[0])
	}
}

// TestIBANChecksum: the digit-at-a-time modulo 97 agrees with the whole
// number in math/big on random strings of digits and letters.
func TestIBANChecksum(t *testing.T) {
	ref := func(s string) bool {
		var b strings.Builder
		for _, c := range s[4:] + s[:4] {
			switch {
			case c >= '0' && c <= '9':
				b.WriteRune(c)
			case c >= 'A' && c <= 'Z':
				b.WriteString(strconv.Itoa(int(c-'A') + 10))
			default:
				return false
			}
		}
		n, ok := new(big.Int).SetString(b.String(), 10)
		return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
	}
	rnd := rand.New(rand.NewPCG(11, 12))
	valid := 0
	for i := 0; i < 50000; i++ {
		b := make([]byte, 15+rnd.IntN(20))
		for j := range b {
			if j < 2 || rnd.IntN(8) == 0 {
				b[j] = byte('A' + rnd.IntN(26))
			} else {
				b[j] = byte('0' + rnd.IntN(10))
			}
		}
		if rnd.IntN(1000) == 0 {
			b[rnd.IntN(len(b))] = 'a'
		}
		s := string(b)
		if got, want := validIBAN(s), ref(s); got != want {
			t.Fatalf("%s: %v, math/big %v", s, got, want)
		}
		if ref(s) {
			valid++
		}
	}
	if !validIBAN("RS35260005601001611379") || valid < 300 {
		t.Errorf("valid %d", valid)
	}
}
