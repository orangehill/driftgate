package allowlist

import (
	"sort"
	"strconv"
	"strings"
	"testing"
)

// lint parses rules and returns the findings as "kind rule check".
func lint(t *testing.T, yaml string) []string {
	t.Helper()
	l, err := Parse([]byte("version: 1\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range l.Lint() {
		out = append(out, f.Kind+" "+strconv.Itoa(f.Rule)+" "+f.Check)
	}
	sort.Strings(out)
	return out
}

func TestLint(t *testing.T) {
	cases := []struct {
		name, yaml string
		want       []string
	}{
		{"clean", `body:
  - path: $.meta.generated_at
    format: '\d{4}-\d{2}-\d{2}T[\d:]+Z'
    reason: r
  - path: $.data[*].updated_at
    format: '\d{4}-\d{2}-\d{2} [\d:]+'
    reason: r
  - request: get-feed
    path: $.items
    any_length: true
    reason: r
  - request: post-login
    path: $raw
    format: '(?s)<!DOCTYPE html>.*'
    reason: r
  - path: html*
    format: '/_next/static/[\w-]+/'
    parts: true
    reason: parts may reach the whole page
  - path: xlsx:*!F*
    format: '[0-9a-f-]{36}'
    reason: r
headers:
  - name: date
    format: '.+'
    reason: one header, by name
`, nil},
		// A cell glob's column and row are apart: *1 is row 1, so A11
		// is no part of it, and A1 is.
		{"row glob does not cover row 11", "body:\n  - path: 'xlsx:S!*1'\n    format: '\\d+'\n    reason: r\n  - path: 'xlsx:S!A11'\n    format: '\\d+'\n    reason: r\n", nil},
		{"column glob does not cover column BA", "body:\n  - path: 'xlsx:S!B*'\n    format: '\\d+'\n    reason: r\n  - path: 'xlsx:S!BA1'\n    format: '\\d+'\n    reason: r\n", nil},
		{"row glob covers its cells", "body:\n  - path: 'xlsx:*!*1'\n    format: '\\d+'\n    reason: r\n  - path: 'xlsx:S!B1'\n    format: '\\d+'\n    reason: r\n  - path: 'xlsx:S!B?1'\n    format: '\\d+'\n    reason: r\n", []string{"body 2 shadowed"}},
		{"raw without request", "body:\n  - path: $raw\n    format: '(?s)<html>.*'\n    reason: r\n", []string{"body 1 raw-without-request"}},
		{"leading wildcard", "body:\n  - path: '*updated_at'\n    format: '\\d+'\n    reason: r\n", []string{"body 1 broad"}},
		{"leading ?", "body:\n  - path: '?.a'\n    format: '\\d+'\n    reason: r\n", []string{"body 1 broad"}},
		{"whole document", "body:\n  - path: '$.*'\n    format: '\\d+'\n    reason: r\n  - path: 'html>*'\n    format: '\\d+'\n    reason: r\n  - path: 'xlsx:*!*'\n    format: '\\d+'\n    reason: r\n  - path: '$*'\n    any_length: true\n    reason: r\n",
			[]string{"body 1 broad", "body 2 broad", "body 3 broad", "body 4 broad"}},
		{"format accepts anything", "body:\n  - path: '$.data[*].token'\n    format: '.*'\n    reason: r\n  - request: x\n    path: $raw\n    format: '(?s).*'\n    reason: r\n  - path: '$.token'\n    format: '.*'\n    reason: one value by its path\n",
			[]string{"body 1 broad", "body 2 broad"}},
		{"shadowed and duplicate", `body:
  - path: $.data[*].at
    format: '\d+'
    reason: r
  - path: $.data[3].at
    format: '\w+'
    reason: later, narrower
  - path: $.data[*].at
    format: '\d+'
    reason: again
  - request: get-*
    path: $.data[*].at
    format: '\d+'
    parts: true
    reason: parts lose to any whole rule
  - path: $.data[*].at
    format: '\d+'
    target: api
    reason: other target is covered too
  - path: $.other
    format: '\d+'
    reason: not covered
`, []string{"body 2 shadowed", "body 3 duplicate", "body 4 shadowed", "body 5 shadowed"}},
		{"not shadowed", `body:
  - request: get-a
    path: $.x
    format: '\d+'
    reason: r
  - path: $.x
    format: '\d+'
    reason: applies to more requests
  - target: web
    path: $.y
    format: '\d+'
    reason: r
  - path: $.y
    format: '\d+'
    reason: applies to more targets
  - path: $.z?
    format: '\d+'
    reason: r
  - path: $.z*
    format: '\d+'
    reason: a question mark never covers a star
  - path: xlsx:S!A1
    format: '\d+'
    reason: cells and values are apart
  - path: $.w
    format: '\d+'
    reason: r
  - path: $.w
    format: '\d+'
    any_length: true
    reason: the length part is its own
`, nil},
		{"any_length shadowed", "body:\n  - path: $.items*\n    any_length: true\n    reason: r\n  - path: $.items[*].tags\n    any_length: true\n    reason: r\n", []string{"body 2 shadowed"}},
		{"arrays", `arrays:
  - path: $.data
    key: id
    reason: r
  - path: $.data
    key: id
    reason: again
  - path: $.data
    request: get-a
    key: slug
    reason: never decides
  - path: $.list
    key: id
    reason: r
`, []string{"array 2 duplicate", "array 3 shadowed"}},
		{"headers", `headers:
  - name: ETag
    format: 'W/"\w+"'
    reason: r
  - name: etag
    request: get-a
    format: '"\w+"'
    reason: never decides
  - name: etag
    format: 'W/"\w+"'
    reason: again
  - name: x-id
    request: get-a
    format: '\d+'
    reason: r
  - name: x-id
    format: '\d+'
    reason: applies to more
`, []string{"header 2 shadowed", "header 3 duplicate"}},
	}
	for _, c := range cases {
		got := lint(t, c.yaml)
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPathCovers(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"$.a*", "$.a[*].b", true},
		{"$.a*", "$.ab", true},
		{"$.a[*].b", "$.a[1].b", true},
		{"$.a[1].b", "$.a[*].b", false},
		{"$.a?", "$.a*", false},
		{"$.a*", "$.a?", true},
		{"*b", "$.a?b", true},
		{"$.x", "$.x", true},
	}
	for _, c := range cases {
		if got := pathCovers(c.a, c.b); got != c.want {
			t.Errorf("pathCovers(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestRuleRefString(t *testing.T) {
	r := RuleRef{Kind: "body", Rule: 3, Path: "$.token", Request: "get-*", Target: "api"}
	if got := r.String(); got != "body rule 3 ($.token, request get-*, target api)" {
		t.Error(got)
	}
	if got := (RuleRef{Kind: "header", Rule: 1, Name: "date"}).String(); got != "header rule 1 (date)" {
		t.Error(got)
	}
}
