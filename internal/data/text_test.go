package data

import (
	"hash/maphash"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/jsonx"
	"github.com/orangehill/driftgate/internal/pii"
)

func newText(t *testing.T) *textReplacer {
	t.Helper()
	d, _ := pii.New(nil)
	return &textReplacer{r: newTestReplacer(t, testKey), pii: d}
}

func TestFreeTextReplacesOnlyPersonalParts(t *testing.T) {
	ok := okf(t)
	x := newText(t)
	in := "Dear Petre, write to pera.peric@posta.example or call +381 64 123 4567. Thanks, Management."
	needles := []needle{{text: "Petar Petrović", fn: x.r.Universal, fold: true}, {text: "Petre", fn: x.r.Universal, fold: true}}
	out, n, err := x.freeText(in, needles)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("%d parts: %q", n, out)
	}
	for _, gone := range []string{"pera.peric", "posta", "123 4567", "Petre"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q still in %q", gone, out)
		}
	}
	for _, kept := range []string{"Dear ", ", write to ", " or call +", ". Thanks, Management."} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q lost from %q", kept, out)
		}
	}
	email := ok(x.r.Email("pera.peric@posta.example"))
	if !strings.Contains(out, email) {
		t.Errorf("the e-mail in the text is not the column's stand-in %q: %q", email, out)
	}
	if len([]rune(out)) != len([]rune(in))+len(".invalid")-len(".example") {
		t.Errorf("length changed beyond the domain: %q", out)
	}
}

func TestFreeTextRowValuesAndWords(t *testing.T) {
	ok := okf(t)
	x := newText(t)
	needles := []needle{{text: "Jovana Marković", fn: x.r.Universal, fold: true}}
	for _, w := range words("Jovana Marković") {
		needles = append(needles, needle{text: w, fn: x.r.Universal, fold: true})
	}
	out, _, err := x.freeText("Author: Jovana Marković. Contact: JOVANA. Markovićeva ulica stays.", needles)
	if err != nil {
		t.Fatal(err)
	}
	whole := ok(x.r.Universal("Jovana Marković"))
	if !strings.Contains(out, whole) || strings.Contains(out, "Jovana") || strings.Contains(out, "JOVANA") {
		t.Fatalf("%q (whole %q)", out, whole)
	}
	if !strings.Contains(out, "Markovićeva") {
		t.Fatalf("a longer word was replaced: %q", out)
	}
}

func TestHTMLKeepsMarkup(t *testing.T) {
	x := newText(t)
	in := `<p class="lead">Write <a href="mailto:pera@firma.example" title='Pera Perić'>Peri</a> &amp; <b>us</b>: 011/3300-900</p><!-- pera@firma.example --> a < b`
	needles := []needle{{text: "Pera Perić", fn: x.r.Universal, fold: true}}
	out, n, err := x.html(in, needles)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("%d parts: %s", n, out)
	}
	for _, kept := range []string{`<p class="lead">Write <a href="mailto:`, `" title='`, `'>Peri</a> &amp; <b>us</b>: `, `</p><!-- `, ` --> a < b`} {
		if !strings.Contains(out, kept) {
			t.Errorf("markup %q lost: %s", kept, out)
		}
	}
	for _, gone := range []string{"pera@firma.example", "Pera Perić", "3300-900"} {
		if strings.Contains(out, gone) {
			t.Errorf("%q still in %s", gone, out)
		}
	}
}

func TestJSONPerPath(t *testing.T) {
	x := newText(t)
	rule := newJSONRule(&ColumnRule{Kind: KindPersonal, Strategy: StrategyJSON, Paths: map[string]string{
		"$.phone":             StrategyUniversal,
		"$.contacts[*].email": StrategyEmail,
		"$.secret":            StrategyNull,
		"$.theme":             KindKeep,
	}})
	in := `{"phone": "064 123 456", "contacts": [{"email": "a@b.rs", "n": 1}, {"email": "c@d.rs"}], "secret": "x", "theme": "pera@kept.rs", "note": "javite se na mika@x.rs", "id": 7}`
	out, nulled, n, err := x.json(in, rule, nil)
	if err != nil || nulled {
		t.Fatal(err)
	}
	v, err := jsonx.Parse([]byte(out))
	if err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	get := func(p string) *jsonx.Value {
		got, err := v.At(p)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if s := get("$.phone").Text; s == "064 123 456" || len(s) != len("064 123 456") || s[3] != ' ' {
		t.Errorf("phone %q", s)
	}
	if s := get("$.contacts[0].email").Text; !strings.HasSuffix(s, ".invalid") {
		t.Errorf("email %q", s)
	}
	if get("$.secret").Kind != jsonx.Null || get("$.theme").Text != "pera@kept.rs" || get("$.id").Text != "7" || get("$.contacts[0].n").Text != "1" {
		t.Errorf("%s", out)
	}
	if s := get("$.note").Text; strings.Contains(s, "mika@x.rs") || !strings.HasPrefix(s, "javite se na ") {
		t.Errorf("note %q", s)
	}
	if n != 5 || !strings.HasPrefix(out, `{"phone": "`) {
		t.Errorf("%d parts, layout: %s", n, out)
	}
	// Unchanged JSON keeps its exact bytes.
	same := `{"a":1,  "b":"plain"}`
	if out, _, _, _ := x.json(same, rule, nil); out != same {
		t.Errorf("%q → %q", same, out)
	}
	// Not JSON: free text.
	if out, _, n, _ := x.json("write to mika@x.rs", rule, nil); n != 1 || strings.Contains(out, "mika") {
		t.Errorf("not JSON: %q", out)
	}
}

func TestSerializedPHPProposal(t *testing.T) {
	d, _ := pii.New(nil)
	samples := []string{`a:2:{s:5:"email";s:6:"a@b.rs";i:0;b:1;}`, `O:8:"stdClass":1:{s:1:"a";i:1;}`, `a:0:{}`}
	r := propose(&Table{Name: "cache"}, &Column{Name: "value", Type: "mediumtext", Category: CatText}, samples, d)
	if r.Kind != KindSerializedPHP {
		t.Fatalf("%+v", r)
	}
}

func TestProposals(t *testing.T) {
	d, _ := pii.New(nil)
	users := &Table{Name: "users"}
	cases := []struct {
		col     *Column
		samples []string
		want    string
	}{
		{&Column{Name: "email", Category: CatText}, nil, StrategyEmail},
		{&Column{Name: "contact", Category: CatText}, []string{"a@b.rs", "c@d.com", "e@f.rs"}, StrategyEmail},
		{&Column{Name: "password", Category: CatText}, nil, StrategyPassword},
		{&Column{Name: "remember_token", Category: CatText}, nil, StrategyToken},
		{&Column{Name: "name", Category: CatText}, nil, StrategyUniversal},
		{&Column{Name: "body", Type: "text", Category: CatText}, []string{"<p>Zdravo</p>"}, StrategyHTML},
		{&Column{Name: "notes", Type: "text", Category: CatText}, []string{"no personal data, only a long product description that repeats in the catalog with no names or addresses"}, StrategyFreeText},
		{&Column{Name: "meta", Category: CatJSON}, nil, StrategyJSON},
		{&Column{Name: "created_at", Type: "datetime", Category: CatOther}, nil, KindKeep},
		{&Column{Name: "status", Type: "varchar(10)", Category: CatText}, []string{"active", "inactive"}, KindKeep},
		{&Column{Name: "ip_address", Type: "varchar(45)", Category: CatText, Nullable: true}, nil, StrategyNull},
	}
	for _, c := range cases {
		r := propose(users, c.col, c.samples, d)
		got := r.Strategy
		if r.Kind != KindPersonal {
			got = r.Kind
		}
		if got != c.want {
			t.Errorf("%s: %s (%s), want %s", c.col.Name, got, r.Reason, c.want)
		}
		for _, s := range c.samples {
			if strings.Contains(r.Reason, s) {
				t.Errorf("%s: the reason carries a value: %q", c.col.Name, r.Reason)
			}
		}
	}
	if r := propose(&Table{Name: "categories"}, &Column{Name: "name", Category: CatText}, []string{"News"}, d); r.Kind != KindKeep {
		t.Errorf("category name: %+v", r)
	}
}

// nameNeedles are the needles anonymize builds for a name of the row.
func nameNeedles(x *textReplacer, name string) []needle {
	nd := []needle{{text: name, fn: x.r.Universal, fold: true}}
	for _, w := range words(name) {
		nd = append(nd, needle{text: w, fn: x.r.Universal, fold: true})
	}
	return nd
}

func TestFreeTextFindsNamesWhateverTheText(t *testing.T) {
	x := newText(t)
	nd := nameNeedles(x, "Ana Petrović")
	for _, c := range []struct{ in, gone string }{
		// A character whose lower case has another length (İ, the Kelvin
		// sign) once turned off every case-insensitive needle.
		{"Call Ana Petrović in İstanbul", "Petrović"},
		{"Ana Petrović, 5 K", "Petrović"},
		{"ANA PETROVIĆ", "PETROVIĆ"},
		{"Ana Petrovic without diacritics", "Petrovic"},
		{"Ана Петровић in Cyrillic", "Петровић"},
		{"Greetings to Petroviću and Petrovićima", "Petrović"},
		{"AnaPetrović in the account", "AnaPetrović"},
		{`{"name":"Ana Petrović"}`, `Petrović`},
		{`{"name":"Ana Petrović"}`, `Ana`},
	} {
		out, n, err := x.freeText(c.in, nd)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 || strings.Contains(out, c.gone) {
			t.Errorf("%q → %q (%d parts)", c.in, out, n)
		}
	}
	for _, kept := range []string{"Markovićeva ulica", "Petrovićevo", "Anapolis"} {
		if out, _, _ := x.freeText(kept, append(nd, nameNeedles(x, "Jovana Marković")...)); out != kept {
			t.Errorf("another word was replaced: %q → %q", kept, out)
		}
	}
	// An escaped name gets an escaped stand-in: the text stays valid JSON.
	out, _, _ := x.freeText(`{"name":"Ana Petrović"}`, nd)
	v, err := jsonx.Parse([]byte(out))
	if err != nil || strings.Contains(v.Members[0].Value.Text, "Petrovi") {
		t.Errorf("%s: %v", out, err)
	}
	// HTML character references in HTML.
	out, _, _ = x.html(`<p title="Petrovi&#263;">Ana Petrovi&cacute;</p>`, nd)
	if strings.Contains(out, "Petrovi") || !strings.HasPrefix(out, `<p title="`) || !strings.HasSuffix(out, "</p>") {
		t.Errorf("html: %s", out)
	}
}

func TestFreeTextFindsPhoneNumbersWithTheirPrefix(t *testing.T) {
	x := newText(t)
	nd := []needle{{text: "641234567", fn: x.r.Number, fold: true}}
	for _, in := range []string{"tel 0641234567", "tel +381641234567", "tel 00381 641234567", "tel 641234567"} {
		out, n, _ := x.freeText(in, nd)
		if n != 1 || strings.Contains(out, "1234567") || !strings.HasPrefix(out, "tel ") {
			t.Errorf("%q → %q", in, out)
		}
	}
	if out, _, _ := x.freeText("racun 9641234567", nd); out != "racun 9641234567" {
		t.Errorf("a longer number was replaced: %q", out)
	}
}

func TestJSONKeysAndNumbersUnderPaths(t *testing.T) {
	x := newText(t)
	rule := newJSONRule(&ColumnRule{Kind: KindPersonal, Strategy: StrategyJSON, Paths: map[string]string{
		"$.contacts": StrategyEmail,
		"$.phone":    StrategyFreeText,
		"$.secret":   StrategyNull,
	}})
	in := `{"contacts":{"ana.petrovic@posta.example":"Ana","pera@firma.example":"Pera"},"phone":381641234567,"jmbg":1501990710008,` +
		`"secret":{"pera@firma.example":1},"amount":1501990710008,"id":12}`
	out, _, _, err := x.json(in, rule, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonx.Parse([]byte(out))
	if err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	contacts, _ := v.At("$.contacts")
	for _, m := range contacts.Members {
		if !strings.HasSuffix(m.Key, ".invalid") {
			t.Errorf("a key under an e-mail path stays: %s", out)
		}
	}
	if len(contacts.Members) != 2 || contacts.Members[0].Key == contacts.Members[1].Key {
		t.Errorf("keys: %s", out)
	}
	get := func(p string) string { j, _ := v.At(p); return j.ScalarText() }
	if get("$.phone") == "381641234567" || get("$.jmbg") == "1501990710008" {
		t.Errorf("a number under a free-text path or named jmbg stays: %s", out)
	}
	if get("$.amount") != "1501990710008" || get("$.id") != "12" || get("$.secret") != "null" {
		t.Errorf("%s", out)
	}
}

// BenchmarkFreeTextRow replaces the free text and the HTML of a seeded user
// with the needles of its row, as anonymize does.
func BenchmarkFreeTextRow(b *testing.B) {
	r, _ := NewReplacer(testKey)
	det, _ := pii.New(nil)
	x := &textReplacer{r: r, pii: det}
	var ps []person
	for i := 1; i <= 5000; i++ {
		ps = append(ps, newPerson(i))
	}
	b.ResetTimer()
	for i := range b.N {
		p := ps[i%len(ps)]
		var needles []needle
		needles = append(needles, needle{text: p.name, fn: r.Universal, fold: true})
		for _, w := range words(p.name) {
			needles = append(needles, needle{text: w, fn: r.Universal, fold: true})
		}
		needles = append(needles, needle{text: p.email, fn: r.Email, fold: true})
		needles = append(needles, needle{text: p.phone, fn: r.Universal, fold: true})
		for _, w := range words(p.phone) {
			needles = append(needles, needle{text: w, fn: r.Universal, fold: true})
		}
		needles = append(needles, needle{text: "tok0000000000000000000000000000000000019918", fn: r.Token})
		if _, _, err := x.freeText(p.bio, needles); err != nil {
			b.Fatal(err)
		}
		if _, _, err := x.html(p.html, needles); err != nil {
			b.Fatal(err)
		}
	}
}

// TestSpansFoundAhead: spans found ahead (record) and used later (replay)
// replace exactly what spans found at the time replace; spans of another
// text are not used.
func TestSpansFoundAhead(t *testing.T) {
	live, ahead := newText(t), newText(t)
	for i := 1; i <= 200; i++ {
		p := newPerson(i)
		needles := func() []needle {
			return []needle{{text: p.name, fn: live.r.Universal, fold: true}, {text: p.email, fn: live.r.Email, fold: true}, {text: p.phone, fn: live.r.Universal, fold: true}}
		}
		var bio, html []foundSpans
		rec := &textReplacer{r: ahead.r, pii: ahead.pii, record: &bio}
		rec.freeText(p.bio, needles())
		rec.record = &html
		rec.html(p.html, needles())
		other := p.bio
		if i > 1 {
			other = newPerson(i - 1).bio // spans of another text
		}
		for _, c := range []struct {
			text   string
			replay []foundSpans
			html   bool
		}{{p.bio, bio, false}, {p.html, html, true}, {p.bio, []foundSpans{{n: len(other), sum: maphash.String(spanSeed, other), spans: bio[0].spans}}, false}} {
			ahead.replay = c.replay
			var want, got string
			var err error
			if c.html {
				want, _, err = live.html(c.text, needles())
				if err == nil {
					got, _, err = ahead.html(c.text, needles())
				}
			} else {
				want, _, err = live.freeText(c.text, needles())
				if err == nil {
					got, _, err = ahead.freeText(c.text, needles())
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("person %d: %q, want %q", i, got, want)
			}
		}
	}
}
