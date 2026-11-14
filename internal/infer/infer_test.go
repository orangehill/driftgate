package infer

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestPattern(t *testing.T) {
	cases := []struct {
		values []string
		want   string
	}{
		{[]string{"2026-10-29 10:00:01", "2026-10-29 10:00:07"}, `\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`},
		{[]string{"Wed, 29 Oct 2026 09:00:01 GMT", "Wed, 29 Oct 2026 09:00:02 GMT"}, `[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT`},
		{[]string{"9", "10"}, `\d+`},
		{[]string{"-5", "23"}, `-?\d+`},
		{[]string{"1.5", "12"}, `\d+(?:\.\d+)?`},
		{[]string{"3f9a.c01d", "77b2.e4ff"}, `[0-9a-f]{4}\.[0-9a-f]{4}`},
		{[]string{"XSRF-TOKEN=ab12; path=/", "XSRF-TOKEN=cd34; path=/"}, `XSRF-TOKEN=[0-9a-f]{4}; path=/`},
		{[]string{"2026-10-29T10:27:08.896Z", "2026-10-29T10:27:09.112Z"}, `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`},
		{[]string{"eyJ.a-b_c.x1", "eyJ.ab.c-d_e2"}, `[0-9A-Za-z\-._]+`},
		{[]string{"11bca30b-0308-4725-9d4e-6f1a2b3c4d5e", "7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d"}, `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`},
		{[]string{"11BCA30B-0308-4725-9D4E-6F1A2B3C4D5E", "7E2F0C41-5521-4A0B-8C3D-0E9F8A7B6C5D"}, `[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}`},
		{[]string{"/orders/11BCA30B-0308-4725-9D4E-6F1A2B3C4D5E?x=1", "/orders/7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d?x=2"}, `/orders/[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\?x=\d`},
		{[]string{"/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/1", "/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/2"}, `/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/\d+`},
		// A duration and a session cookie: neither keeps the width of
		// the samples (6ms then 12ms; a longer payload).
		{[]string{"Response rendered in 6ms at 1", "Response rendered in 6ms at 2"}, `Response rendered in \d+ms at \d`},
		{[]string{"x-time: 7ms", "x-time: 12ms"}, `x-time: \d+ms`},
		{[]string{"s=" + strings.Repeat("aB3", 113) + "; path=/", "s=" + strings.Repeat("Zx9", 113) + "; path=/"}, `s=[0-9A-Za-z]+; path=/`},
		{[]string{"1700000000", "1700000123"}, `\d+`},
		{[]string{"id ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34", "id 0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"}, `id [0-9a-f]{40}`},
		{[]string{"same", "same"}, ``},
		{[]string{"abc", ""}, `[a-z]*`},
		{[]string{"", "2024-01-01"}, `[0-9\-]*`},
		{nil, ``},
	}
	for _, c := range cases {
		got := Pattern(c.values)
		if got != c.want {
			t.Errorf("Pattern(%q) = %q, want %q", c.values, got, c.want)
			continue
		}
		if got == "" {
			continue
		}
		re := regexp.MustCompile("^(?:" + got + ")$")
		for _, v := range c.values {
			if !re.MatchString(v) {
				t.Errorf("%q does not match %q", got, v)
			}
		}
	}
}

// A UUID group that holds digits only in every sample must not become \d{4}:
// the next UUID has letters there. The case of the samples stays.
func TestPatternUUIDAnyGroup(t *testing.T) {
	got := Pattern([]string{"11bca30b-0308-4725-9d4e-6f1a2b3c4d5e", "7e2f0c41-5521-4725-8c3d-0e9f8a7b6c5d"})
	re := regexp.MustCompile("^(?:" + got + ")$")
	if v := "0b5c1e2a-af3d-4bc1-a9e8-77f1d2c3b4a5"; !re.MatchString(v) {
		t.Errorf("%q does not match %q", got, v)
	}
	if v := "0B5C1E2A-AF3D-4BC1-A9E8-77F1D2C3B4A5"; re.MatchString(v) {
		t.Errorf("%q matches %q, an uppercase UUID for lowercase samples", got, v)
	}
}

// A UUID that is the same in every sample (a tenant) is kept: another one is
// a change.
func TestPatternUUIDSame(t *testing.T) {
	got := Pattern([]string{"/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/1", "/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/2"})
	re := regexp.MustCompile("^(?:" + got + ")$")
	if v := "/t/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/jobs/3"; !re.MatchString(v) {
		t.Errorf("%q does not match %q", got, v)
	}
	if v := "/t/7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d/jobs/3"; re.MatchString(v) {
		t.Errorf("%q matches %q, another tenant", got, v)
	}
}

// The same UUID in another case is still the same UUID, in either case.
func TestPatternUUIDSameFold(t *testing.T) {
	got := Pattern([]string{"x/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/1", "x/11BCA30B-0308-4725-9D4E-6F1A2B3C4D5E/2"})
	re := regexp.MustCompile("^(?:" + got + ")$")
	for _, v := range []string{"x/11bca30b-0308-4725-9d4e-6f1a2b3c4d5e/3", "x/11BCA30B-0308-4725-9D4E-6F1A2B3C4D5E/3"} {
		if !re.MatchString(v) {
			t.Errorf("%q does not match %q", got, v)
		}
	}
	if v := "x/7e2f0c41-5521-4a0b-8c3d-0e9f8a7b6c5d/3"; re.MatchString(v) {
		t.Errorf("%q matches %q, another UUID", got, v)
	}
}

// TestPatternNarrow: a proposed format accepts what the samples' own form
// allows and drifts on what a reviewer would want to see: a new status
// word, a longer amount or code, a digit that agreed in every sample; a
// count, a duration and a long token still take any length.
func TestPatternNarrow(t *testing.T) {
	for _, c := range []struct {
		samples, accept, reject []string
	}{
		{[]string{"pending", "shipped"}, nil, []string{"failed", "cancelled", "error"}},
		{[]string{"status: pending", "status: shipped"}, nil, []string{"status: failed", "status: cancelled"}},
		{[]string{"Total: 12.50 EUR", "Total: 13.75 EUR"}, []string{"Total: 99.10 EUR"}, []string{"Total: 1312.50 EUR", "Total: 3.75 EUR", "Total: 12.5 EUR"}},
		{[]string{"page 1 of 3", "page 2 of 3"}, []string{"page 3 of 3"}, []string{"page 10 of 3", "page 1234567 of 3", "page 1 of 4"}},
		{[]string{"HTTP 200", "HTTP 201"}, []string{"HTTP 204"}, []string{"HTTP 2010", "HTTP 20"}},
		{[]string{"code=3", "code=4"}, []string{"code=5"}, []string{"code=500", "code=34"}},
		{[]string{"2026-11-04T10:00:00Z", "2026-11-04T11:22:33Z"}, []string{"2026-12-05T09:00:00Z"}, []string{"2026-11-04T1:2:3Z", "2026-11-04T10:00:00.123Z"}},
		{[]string{"ORD-12345", "ORD-12399"}, []string{"ORD-54321"}, []string{"ORD-1", "ORD-123456789"}},
		{[]string{"ab12cd34ef", "zz99yy88xx"}, nil, []string{"a1", "abcdefghij12345"}},
		{[]string{"v2beta1x9a", "v3beta2y8b"}, nil, []string{"v3beta2y8bzzzzz"}},
		{[]string{"1.2.3.4.5", "1.2.3.4.6"}, []string{"1.2.3.4.7"}, []string{"1.2.3.4.66666", "1.2.4.4.5"}},
		{[]string{"tok_abcdefgh", "tok_ijklmnop"}, []string{"tok_qrstuvwx"}, []string{"tok_a", "tok_abcdefghabcdefghabcdefgh"}},
		{[]string{"12:01", "12:59"}, []string{"13:15"}, []string{"12:5", "12:5999"}},
		{[]string{"AB", "CD"}, nil, []string{"ABC", "EF"}},
		{[]string{"count 7", "count 8"}, []string{"count 70000"}, []string{"count x"}},
		{[]string{"took 7ms", "took 12ms"}, []string{"took 1500ms"}, []string{"took 7s"}},
		{[]string{"/jobs/9", "/jobs/12"}, []string{"/jobs/100000"}, []string{"/jobs/x"}},
		// Page counts, suffixed ids, versions and ranges grow; a number
		// none of whose samples is negative stays non-negative.
		{[]string{"total_pages=3", "total_pages=4"}, []string{"total_pages=120"}, nil},
		{[]string{"user_42", "user_7"}, []string{"user_100000"}, []string{"user_x"}},
		{[]string{"v1.2.3", "v1.3.0"}, []string{"v1.20.30"}, []string{"v1.2"}},
		{[]string{"Showing 1-10 of 120", "Showing 11-20 of 120"}, []string{"Showing 101-110 of 120"}, []string{"Showing 1-10 of 119", "Showing 1-10 of 0"}},
		// Only digits that differ inside a version or a range grow; what
		// agrees in every sample, or lies outside them, keeps its form.
		{[]string{"app v1.2.3 build 7 status 200", "app v1.2.3 build 8 status 200"}, []string{"app v1.2.3 build 9 status 200"}, []string{"app v1.2.3 build 7 status 500", "app v9.9.9 build 7 status 200", "app v1.2.3 build 70 status 200"}},
		{[]string{"Laravel v10.2.3 errors: 0", "Laravel v10.2.4 errors: 0"}, []string{"Laravel v10.2.40 errors: 0"}, []string{"Laravel v10.2.4 errors: 5", "Laravel v10.3.4 errors: 0"}},
		{[]string{"Showing 1-10 of 120 (2 hidden)", "Showing 11-20 of 120 (2 hidden)"}, nil, []string{"Showing 1-10 of 120 (3 hidden)", "Showing 1-10 of 120 (200 hidden)"}},
		// A money total keeps its width; a code after a word keeps a width
		// of three or more that every sample shares, an id does not.
		{[]string{"total: 12.50", "total: 13.75"}, nil, []string{"total: 1312.50", "total: 0.01"}},
		{[]string{"HTTP_401", "HTTP_403"}, []string{"HTTP_500"}, []string{"HTTP_5000", "HTTP_50"}},
		{[]string{"error_code_401", "error_code_403"}, []string{"error_code_500"}, []string{"error_code_4031"}},
		{[]string{"order_12 shipped", "order_13 shipped"}, []string{"order_99999 shipped"}, nil},
		// What agrees inside a version stays; a counter after a lower-case
		// word grows past its width, a code after an upper-case word or a
		// word that names one keeps it.
		{[]string{"v1.2.3 v1.2.3", "v1.2.4 v1.2.4"}, []string{"v1.2.40 v1.2.5"}, []string{"v1.2.4 v9.2.4", "v9.2.4 v1.2.4"}},
		{[]string{"Laravel v10.48.2", "Laravel v10.48.3"}, []string{"Laravel v10.48.10"}, []string{"Laravel v11.48.3", "Laravel v10.49.3"}},
		{[]string{"job_998", "job_999"}, []string{"job_1000"}, nil},
		{[]string{"amount_401", "amount_402"}, []string{"amount_999"}, nil},
		{[]string{"ERR_500", "ERR_503"}, []string{"ERR_504"}, []string{"ERR_5000"}},
		{[]string{"status_201", "status_204"}, []string{"status_200"}, []string{"status_2010"}},
		// A run padded with zeros keeps its width; code and err are words
		// of their own, not part of one (cherry, terraform).
		{[]string{"ref_00123", "ref_00124"}, []string{"ref_00999"}, []string{"ref_100000", "ref_123"}},
		{[]string{"cherry_998", "cherry_999"}, []string{"cherry_1000"}, nil},
		{[]string{"terraform_100", "terraform_101"}, []string{"terraform_1000"}, nil},
		{[]string{"tmp_3", "tmp_9"}, []string{"tmp_123456"}, nil},
		{[]string{"page 1 of 3", "page 2 of 3"}, nil, []string{"page 1 of 4"}},
		{[]string{"5", "7"}, []string{"700000"}, []string{"-5"}},
		{[]string{"0.5", "1.25"}, nil, []string{"-0.5"}},
		{[]string{"balance: 5", "balance: 7"}, nil, []string{"balance: -5", "balance: 50"}},
		{[]string{"9", "10"}, []string{"123"}, []string{"-1"}},
		{[]string{"-1", "10"}, []string{"-123"}, nil},
	} {
		p := Pattern(c.samples)
		re := regexp.MustCompile("^(?:" + p + ")$")
		for _, v := range append(append([]string{}, c.samples...), c.accept...) {
			if !re.MatchString(v) {
				t.Errorf("%q → %s rejects %q", c.samples, p, v)
			}
		}
		for _, v := range c.reject {
			if re.MatchString(v) {
				t.Errorf("%q → %s accepts %q", c.samples, p, v)
			}
		}
	}
}

// TestPatternLong: a run longer than a regular expression can count (1000)
// takes as many as it needs; Pattern never panics on what responses hold.
func TestPatternLong(t *testing.T) {
	for name, vals := range map[string][]string{
		"hex":           {"sig " + strings.Repeat("a1", 600), "sig " + strings.Repeat("b2", 600)},
		"digits":        {"n " + strings.Repeat("1", 1200), "n " + strings.Repeat("2", 1200)},
		"letters":       {strings.Repeat("aZ", 600) + "-x", strings.Repeat("bY", 600) + "-x"},
		"digits 5-1200": {"x 1 " + strings.Repeat("1", 1200), "x 2 " + strings.Repeat("2", 5)},
		"equal digits":  {"a" + strings.Repeat("7", 1200) + "Z x", "a" + strings.Repeat("7", 1200) + "Z y"},
	} {
		p := Pattern(vals)
		re, err := regexp.Compile("^(?:" + p + ")$")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, v := range vals {
			if !re.MatchString(v) {
				t.Errorf("%s: %s rejects a sample", name, p)
			}
		}
	}
}

// jwt is a token with the header and claims given as JSON and a signature
// of the text given (base64url, so - and _ fall where the text puts them).
func jwt(header, claims, sig string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(header)) + "." + enc([]byte(claims)) + "." + sig
}

// TestPatternNext: a format learned from two samples takes the next value
// of the same kind (the next day, the next token), and still rejects a
// value that changed in what the samples agree on.
func TestPatternNext(t *testing.T) {
	hs256 := `{"typ":"JWT","alg":"HS256"}`
	cases := []struct {
		name         string
		values       []string
		next, change []string
	}{
		{
			name:   "a file name with the date and time of its export",
			values: []string{"attachment; filename=report-20261114-101530.json", "attachment; filename=report-20261114-101612.json"},
			next:   []string{"attachment; filename=report-20261115-090000.json", "attachment; filename=report-20270101-000001.json"},
			change: []string{"attachment; filename=summary-20261115-090000.json", "attachment; filename=report-20261115-090000.csv"},
		},
		{
			// Eight digits that are not a date stay as they are.
			name:   "a number of eight digits that is no date",
			values: []string{"ref 20261399 build 7", "ref 20261399 build 8"},
			change: []string{"ref 20261398 build 9"},
		},
		{
			// The signatures happen to have a - at the same place.
			name: "a JWT",
			values: []string{
				jwt(hs256, `{"sub":1,"iat":1763114130}`, "Ab3dE5gH9jK-mN0pQ1sT2vW3yZ4aB5cD6eF7gH8iJ9k"),
				jwt(hs256, `{"sub":1,"iat":1763114191}`, "Zx9wV8uT7sR-qP6oN5mL4kJ3iH2gF1eD0cB9aZ8yX7w"),
			},
			next: []string{
				jwt(hs256, `{"sub":1,"iat":1763117000}`, "a_b-c-dEfGhIjKlMnOpQrStUvWxYz0123456789AbCd"),
				jwt(hs256, `{"sub":12,"iat":1763117000,"exp":1763120600}`, "NoDashOrUnderscoreHere0123456789abcdefghijk"),
			},
			change: []string{
				jwt(`{"typ":"JWT","alg":"HS512"}`, `{"sub":1,"iat":1763117000}`, "Ab3dE5gH9jK-mN0pQ1sT2vW3yZ4aB5cD6eF7gH8iJ9k"),
				"not a token",
			},
		},
		{
			name: "a JWT inside a text",
			values: []string{
				"Bearer " + jwt(hs256, `{"sub":1,"iat":1763114130}`, "Ab3dE5gH9jK-mN0pQ1sT2vW3yZ4aB5cD6eF7gH8iJ9k"),
				"Bearer " + jwt(hs256, `{"sub":1,"iat":1763114191}`, "Zx9wV8uT7sR-qP6oN5mL4kJ3iH2gF1eD0cB9aZ8yX7w"),
			},
			next:   []string{"Bearer " + jwt(hs256, `{"sub":1,"iat":1763117000}`, "a_b-c-dEfGhIjKlMnOpQrStUvWxYz0123456789AbCd")},
			change: []string{"Basic " + jwt(hs256, `{"sub":1,"iat":1763117000}`, "a_b-c-dEfGhIjKlMnOpQrStUvWxYz0123456789AbCd")},
		},
	}
	for _, c := range cases {
		p := Pattern(c.values)
		re := regexp.MustCompile("^(?:" + p + ")$")
		for _, v := range c.values {
			if !re.MatchString(v) {
				t.Errorf("%s: %q rejects the sample %q", c.name, p, v)
			}
		}
		for _, v := range c.next {
			if !re.MatchString(v) {
				t.Errorf("%s: %q rejects the next value %q", c.name, p, v)
			}
		}
		for _, v := range c.change {
			if re.MatchString(v) {
				t.Errorf("%s: %q takes the change %q", c.name, p, v)
			}
		}
	}
}
