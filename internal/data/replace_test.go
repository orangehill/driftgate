package data

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestReplacer(t *testing.T, key []byte) *Replacer {
	t.Helper()
	r, err := NewReplacer(key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// okf returns a helper that fails the test on an error and returns the value.
func okf(t *testing.T) func(string, error) string {
	return func(s string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

// class sorts a rune for shape comparisons.
func class(c rune) string {
	switch {
	case c >= 'a' && c <= 'z':
		return "ascii-lower"
	case c >= 'A' && c <= 'Z':
		return "ascii-upper"
	case strings.ContainsRune("čćđšž", c):
		return "sr-lower"
	case strings.ContainsRune("ČĆĐŠŽ", c):
		return "sr-upper"
	case unicode.Is(unicode.Cyrillic, c) && unicode.IsUpper(c):
		return "cyr-upper"
	case unicode.Is(unicode.Cyrillic, c):
		return "cyr-lower"
	case unicode.IsDigit(c):
		return "digit"
	}
	return string(c)
}

func TestUniversalKeepsShape(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	for _, in := range []string{
		"Petar Petrović", "Đorđe Šćepanović-Žižić", "ŠĐČĆŽ šđčćž", "Јован Јовановић", "ЉУБИЦА Њ. Ђурђевић",
		"+381 64 123-4567", "Bulevar kralja Aleksandra 73/II", "O'Brien, Jr.", "x",
	} {
		out := ok(r.Universal(in))
		if utf8.RuneCountInString(out) != utf8.RuneCountInString(in) || len(out) != len(in) {
			t.Errorf("%q → %q: length changed", in, out)
		}
		if out == in {
			t.Errorf("%q unchanged", in)
		}
		a, b := []rune(in), []rune(out)
		for i := range a {
			if class(a[i]) != class(b[i]) {
				t.Errorf("%q → %q: rune %d %q became %q", in, out, i, a[i], b[i])
			}
		}
	}
}

func TestUniversalDeterministicAndKeyed(t *testing.T) {
	ok := okf(t)
	a, b := newTestReplacer(t, testKey), newTestReplacer(t, testKey)
	other := newTestReplacer(t, []byte("another key, another key, 32 by"))
	for _, in := range []string{"Petar Petrović", "Јован", "064/123-456"} {
		x, y, z := ok(a.Universal(in)), ok(b.Universal(in)), ok(other.Universal(in))
		if x != y {
			t.Errorf("%q: %q vs %q under the same key", in, x, y)
		}
		if x == z {
			t.Errorf("%q: same stand-in %q under different keys", in, x)
		}
		if again := ok(a.Universal(in)); again != x {
			t.Errorf("%q: second call %q, first %q", in, again, x)
		}
	}
}

func TestUniversalCaseFolds(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	lower := ok(r.Universal("petrović"))
	upper := ok(r.Universal("PETROVIĆ"))
	mixed := ok(r.Universal("Petrović"))
	if strings.ToLower(upper) != lower || strings.ToLower(mixed) != lower {
		t.Fatalf("case variants differ beyond case: %q %q %q", lower, upper, mixed)
	}
	if !unicode.IsUpper([]rune(mixed)[0]) || unicode.IsUpper([]rune(mixed)[1]) {
		t.Fatalf("case pattern lost: %q", mixed)
	}
}

func TestUniversalStaysUnique(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	seen := map[string]string{}
	// All 676 two-letter values: the stand-ins must be a permutation.
	for a := 'a'; a <= 'z'; a++ {
		for b := 'a'; b <= 'z'; b++ {
			in := string([]rune{a, b})
			out := ok(r.Universal(in))
			if prev, ok := seen[out]; ok {
				t.Fatalf("%q and %q share %q", prev, in, out)
			}
			seen[out] = in
		}
	}
	// Asking again gives the same answers.
	for out, in := range seen {
		if again := ok(r.Universal(in)); again != out {
			t.Fatalf("%q: %q then %q", in, out, again)
		}
	}
}

func TestEmail(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	in := "Pera.Peric@Posta.example"
	out := ok(r.Email(in))
	if !strings.HasSuffix(out, ".invalid") || strings.Count(out, "@") != 1 {
		t.Fatalf("%q", out)
	}
	local, dom, _ := strings.Cut(out, "@")
	if len(local) != len("Pera.Peric") || local[4] != '.' || !unicode.IsUpper(rune(local[0])) {
		t.Fatalf("local part lost its shape: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "pera") || strings.Contains(strings.ToLower(out), "posta") {
		t.Fatalf("original parts in %q", out)
	}
	if again := ok(r.Email("pera.peric@posta.example")); !strings.EqualFold(again, out) {
		t.Fatalf("case variants: %q vs %q", again, out)
	}
	// The same domain gets the same stand-in.
	other := ok(r.Email("mika@posta.example"))
	_, dom2, _ := strings.Cut(other, "@")
	if !strings.EqualFold(dom, dom2) {
		t.Fatalf("domains %q and %q", dom, dom2)
	}
	// Many addresses that differ in one letter stay distinct.
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		e := ok(r.Email(fmt.Sprintf("u%d@x.rs", i)))
		if seen[strings.ToLower(e)] {
			t.Fatalf("duplicate %q", e)
		}
		seen[strings.ToLower(e)] = true
	}
}

func TestToken(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	for _, in := range []string{
		"8f14e45fceea167a5a36dedd4bea2543",
		"550e8400-e29b-41d4-a716-446655440000",
		"aB3dE5gH7jK9mN1pQ3sT5vX7zA9cE1gI3kM5oQ7sU9wY1",
		"tok_live+/==",
	} {
		out := ok(r.Token(in))
		if len(out) != len(in) || out == in {
			t.Errorf("%q → %q", in, out)
		}
		hexToken := strings.Trim(strings.ToLower(in), "0123456789abcdef-") == ""
		for i := range in {
			if !hexToken && (in[i] >= '0' && in[i] <= '9') != (out[i] >= '0' && out[i] <= '9') {
				t.Errorf("%q → %q: class changed at %d", in, out, i)
			}
			if !isAlnum(in[i]) && in[i] != out[i] {
				t.Errorf("%q → %q: punctuation changed", in, out)
			}
		}
		if hexValue(strings.ReplaceAll(in, "-", ""), len(strings.ReplaceAll(in, "-", ""))) && !hexValue(strings.ReplaceAll(out, "-", ""), len(strings.ReplaceAll(out, "-", ""))) {
			t.Errorf("%q → %q: no longer hexadecimal", in, out)
		}
		if again := ok(r.Token(in)); again != out {
			t.Errorf("%q not deterministic", in)
		}
	}
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func TestPasswordStrategy(t *testing.T) {
	r := newTestReplacer(t, testKey)
	cases := map[string]string{
		"$2y$10$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234": "$2y$10$UA27VjnZnIYNSwBEzD1bq.divaC6Z62cXSBxtud9Ddo7GSGMwtF02",
		"$2b$12$abcdefghijklmnopqrstuuAbCdEfGhIjKlMnOpQrStUvWxYz01234": "$2b$12$tvnlwwyQV3En98U1NRViKuHJGNWC2g8Dr6GVtTiiG/lIypy.z8IyC",
		"$argon2id$v=19$m=1024,t=2,p=2$c29tZXNhbHQ$abcdef":             argon2idHash,
	}
	for in, want := range cases {
		out, known, err := r.Password(in)
		if err != nil || !known || out != want {
			t.Errorf("%q → %q %v %v", in, out, known, err)
		}
	}
	md := md5.Sum([]byte(TestPassword))
	if out, _, _ := r.Password("5f4dcc3b5aa765d61d8327deb882cf99"); out != hex.EncodeToString(md[:]) {
		t.Errorf("md5: %q", out)
	}
	out, known, err := r.Password("pbkdf2_sha256$600000$oldsalt$AAAA")
	if err != nil || !known || !strings.HasPrefix(out, "pbkdf2_sha256$600000$driftgatesalt$") {
		t.Errorf("pbkdf2: %q %v %v", out, known, err)
	}
	// Django's documented test vector for its hasher would need its salt;
	// here: same iterations give the same (cached) hash.
	if again, _, _ := r.Password("pbkdf2_sha256$600000$x$y"); again != out {
		t.Errorf("pbkdf2 not stable")
	}
	in := "plain-text-password"
	out, known, _ = r.Password(in)
	if known || out == in || len(out) != len(in) {
		t.Errorf("unknown format: %q %v", out, known)
	}
	for cost, h := range bcryptHashes {
		if len(h) != 60 || h[4:6] != fmt.Sprintf("%02d", cost) {
			t.Errorf("bcrypt table entry %d: %q", cost, h)
		}
	}
}

func TestNumber(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	for _, in := range []string{"381641234567", "-42", "12.50", "7"} {
		out := ok(r.Number(in))
		if len(out) != len(in) || out == in {
			t.Errorf("%q → %q", in, out)
		}
		d := strings.TrimLeft(out, "-")
		if len(d) > 1 && d[0] == '0' && d[1] != '.' {
			t.Errorf("%q → %q: leading zero", in, out)
		}
	}
}

func TestNumberKeepsNaNAndInfinity(t *testing.T) {
	r := newTestReplacer(t, testKey)
	for _, v := range []string{"NaN", "Infinity", "-Infinity"} {
		if out, err := r.Number(v); err != nil || out != v {
			t.Errorf("%s: %q, %v", v, out, err)
		}
	}
}

func TestUniversalAndNumberAgree(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	for _, v := range []string{"12345", "1e5", "-42.50", "0123"} {
		if a, b := ok(r.Universal(v)), ok(r.Number(v)); a != b {
			t.Errorf("%s: universal %q, number %q", v, a, b)
		}
	}
}

func TestArrayElementByElement(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	col := &Column{Name: "cc", Type: "text[]", Category: CatOther}
	out := ok(r.ForColumn(col, StrategyEmail)(`{ana@x.rs,"Pera Perić <pera@y.rs>",NULL,""}`))
	if !strings.HasPrefix(out, "{"+ok(r.Email("ana@x.rs"))+",") || !strings.HasSuffix(out, `,NULL,""}`) || strings.Contains(out, "pera@y") {
		t.Fatalf("%s", out)
	}
	if got := arrayElements(out); len(got) != 3 {
		t.Fatalf("%q", got)
	}
}

// testCollation is utf8mb4_unicode_ci for a few letters, as the server
// reports it.
func testCollation() *collation {
	co := &collation{name: "test_ci", pad: true, ci: true, canon: map[rune]string{}}
	for _, c := range "abcdefghijklmnopqrstuvwxyz" {
		co.canon[c], co.canon[unicode.ToUpper(c)] = string(c), string(c)
	}
	for k, v := range map[rune]string{'é': "e", 'É': "e", 'č': "c", 'ć': "c", 'š': "s", 'ž': "z", 'ß': "ss", '­': ""} {
		co.canon[k] = v
	}
	return co
}

func TestStandInsFollowTheCollation(t *testing.T) {
	ok := okf(t)
	r := newTestReplacer(t, testKey)
	co := testCollation()
	r.colls[co.name] = co
	col := &Column{Name: "name", Type: "varchar(50)", Category: CatText, Collation: co.name}
	fn := r.ForColumn(col, StrategyUniversal)
	// Equal under the collation: equal stand-ins under it.
	for _, group := range [][]string{{"José", "Jose", "JOSE", "jose "}, {"Čolić", "Colic", "colić"}, {"Strauß", "Strauss"}} {
		first := co.key(ok(fn(group[0])))
		for _, v := range group[1:] {
			if got := co.key(ok(fn(v))); got != first {
				t.Errorf("%q and %q are equal under the collation, their stand-ins are not: %q, %q", group[0], v, first, got)
			}
		}
	}
	// Different under the collation: different under it, also among many.
	seen := map[string]string{}
	for _, a := range "abcdeéčs" {
		for _, b := range "abcdeéčs" {
			v := "k" + string(a) + string(b)
			k := co.key(ok(fn(v)))
			if prev, dup := seen[k]; dup && co.key(prev) != co.key(v) {
				t.Fatalf("%q and %q get stand-ins equal under the collation", prev, v)
			}
			seen[k] = v
		}
	}
	// Without accents and case the stand-in is the one Universal gives.
	if a, b := ok(fn("Petar")), ok(r.Universal("Petar")); a != b {
		t.Errorf("%q, %q", a, b)
	}
	e := r.ForColumn(col, StrategyEmail)
	if a, b := co.key(ok(e("Jose@Firma.example"))), co.key(ok(e("josé@firma.example "))); a != b {
		t.Errorf("e-mail: %q, %q", a, b)
	}
}

// TestStandInIsNoOtherOriginal: a column where one original is what another
// would be replaced by (a phone number in a dense number space): once the
// originals are reserved, no stand-in is any of them, and the result is
// the same whatever the order.
func TestStandInIsNoOtherOriginal(t *testing.T) {
	ok := okf(t)
	a := "064 111 2222"
	b := ok(newTestReplacer(t, testKey).Universal(a)) // what a becomes without b
	c := ok(newTestReplacer(t, testKey).Number("641112222"))
	run := func(order []string) map[string]string {
		r := newTestReplacer(t, testKey)
		for _, v := range []string{a, b, "641112222", c} {
			r.Reserve(PersonalValue{Kind: StrategyUniversal, Value: v, Category: CatText})
		}
		out := map[string]string{}
		for _, v := range order {
			if digitsOnly(v) {
				out[v] = ok(r.Number(v))
			} else {
				out[v] = ok(r.Universal(v))
			}
		}
		return out
	}
	first := run([]string{a, b, "641112222", c})
	for v, s := range first {
		for _, orig := range []string{a, b, "641112222", c} {
			if s == orig {
				t.Errorf("the stand-in of %q is the original %q", v, orig)
			}
		}
	}
	if first[a] == b || first["641112222"] == c {
		t.Fatalf("%v", first)
	}
	if again := run([]string{c, "641112222", b, a}); fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("order changes the stand-ins: %v, %v", first, again)
	}
}
