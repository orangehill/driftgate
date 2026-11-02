package pii

import "testing"

func kinds(fs []Finding) map[string]bool {
	m := map[string]bool{}
	for _, f := range fs {
		m[f.Kind] = true
	}
	return m
}

func TestFinds(t *testing.T) {
	d, _ := New(nil)
	body := `{"contact":{"email":"pera.peric@example.com","phone":"+381 64 123 4567","local":"011/3300-900",
		"jmbg":"0101990710008","card":"4111 1111 1111 1111","iban":"RS35260005601001611379"}}`
	got := kinds(d.Scan([]byte(body)))
	for _, k := range []string{"email", "phone", "jmbg", "card", "iban"} {
		if !got[k] {
			t.Errorf("%s not found in %v", k, got)
		}
	}
	for _, f := range d.Scan([]byte(body)) {
		if f.Sample == "pera.peric@example.com" {
			t.Fatal("finding carries the full value")
		}
	}
}

func TestIgnoresOrdinaryData(t *testing.T) {
	d, _ := New(nil)
	body := `{"id":1234567890123,"price":"1999.99","date":"2024-03-01 10:00:00","year":2026,"slug":"news-2024-03","order":"0012345678901","ids":[101,102,103],"tariff":"0101210000","uuid":"11bca30b-0308-4725-a2c4-dce7ea7a7ee1"}`
	if fs := d.Scan([]byte(body)); len(fs) != 0 {
		t.Fatalf("false alarms: %+v", fs)
	}
}

func TestAllow(t *testing.T) {
	d, _ := New([]string{`[a-z.]+@shop\.example\.com`})
	if fs := d.Scan([]byte(`{"e":"info@shop.example.com"}`)); len(fs) != 0 {
		t.Fatalf("allowed value reported: %+v", fs)
	}
	if fs := d.Scan([]byte(`{"e":"pera@posta.example"}`)); len(fs) != 1 {
		t.Fatalf("got %+v", fs)
	}
}

func TestNotJSON(t *testing.T) {
	d, _ := New(nil)
	if fs := d.Scan([]byte("<p>Write to kontakt@example.org</p>")); len(fs) != 1 || fs[0].Where != "$" {
		t.Fatalf("got %+v", fs)
	}
}

func TestIgnoresNumbersInMarkup(t *testing.T) {
	d, _ := New(nil)
	page := `<style>:root{--color-slate-50:oklch(98.4% .003 247.858);--a:1.5 .002 264.542}</style>` +
		`<svg><path d="M340.797 89.6597 356.094 104 378.661 104C391.29 104 0.5,0011 222 333"/></svg>`
	if f := d.Scan([]byte(page)); len(f) > 0 {
		t.Fatalf("numbers in CSS and SVG reported as personal data: %+v", f)
	}
	if f := d.Scan([]byte(`<p>Pozovite 011/123-4567.</p>`)); len(f) != 1 || f[0].Kind != "phone" {
		t.Fatalf("phone at the end of a sentence: %+v", f)
	}
}

func TestDatesAreNoPhoneNumbers(t *testing.T) {
	d, _ := New(nil)
	for _, s := range []string{"DEFAULT '0000-00-00 00:00:00'", "0000-00-00", "datum 0012-05-01T10:00"} {
		if sp := d.Spans(s); len(sp) != 0 {
			t.Errorf("%q: %+v", s, sp)
		}
	}
	if sp := d.Spans("zovite 011/123-4567"); len(sp) != 1 {
		t.Errorf("%+v", sp)
	}
}
