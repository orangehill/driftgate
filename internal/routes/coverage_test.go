package routes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

func generated(id, method, path, tpl string, params map[string]string) catalog.Request {
	return catalog.Request{ID: id, Method: method, Path: path, Params: params, Source: &catalog.Source{Route: method + " " + path, Template: tpl}}
}

// TestCoverageByBaselineAnswers: a route is covered only by a request that
// did not fail in the baseline; what fails is listed with its requests.
func TestCoverageByBaselineAnswers(t *testing.T) {
	inv := &Inventory{Routes: []Route{
		{Methods: []string{"POST"}, Path: "/tree/reorder", Body: json.RawMessage(`{"order":[1]}`)},
		{Methods: []string{"GET"}, Path: "/items/{id}"},
		{Methods: []string{"POST"}, Path: "/login", Body: json.RawMessage(`{"email":"a"}`)},
		{Methods: []string{"GET"}, Path: "/new"},
		{Methods: []string{"GET"}, Path: "/slow"},
		{Methods: []string{"GET"}, Path: "/dashboard"},
		{Methods: []string{"POST"}, Path: "/notes"}, // no sample body
		{Methods: []string{"GET"}, Path: "/limited"},
		{Methods: []string{"GET"}, Path: "/home"},
	}}
	cat := &catalog.Catalog{Requests: []catalog.Request{
		generated("reorder", "POST", "/tree/reorder", "ok", nil),
		generated("item-1", "GET", "/items/{id}", "ok", map[string]string{"id": "1"}),
		generated("item-2", "GET", "/items/{id}", "ok", map[string]string{"id": "2"}),
		generated("item-nf", "GET", "/items/{id}", "not-found", map[string]string{"id": "999999999"}),
		{ID: "login-wrong", Method: "POST", Path: "/login"}, // by hand: a 422 may be meant
		generated("new", "GET", "/new", "ok", nil),
		generated("slow", "GET", "/slow", "ok", nil),
		generated("dashboard", "GET", "/dashboard", "ok", nil),
		generated("notes", "POST", "/notes", "ok", nil),
		{ID: "limited", Method: "GET", Path: "/limited", Repeat: 61},
		generated("home", "GET", "/home", "ok", nil),
		{ID: "step", Command: []string{"true"}},
	}}
	cat.Requests[0].Body = json.RawMessage(`{"order":[1]}`)
	cat.Requests[8].Body = json.RawMessage(`{}`)
	answers := map[string]Answer{
		"reorder":     {Status: 500},
		"item-1":      {Status: 200},
		"item-2":      {Status: 404},
		"item-nf":     {Status: 500},
		"login-wrong": {Status: 422},
		"slow":        {Error: "timeout after 30s"},
		"dashboard":   {Status: 302, Location: "http://localhost:8000/auth/Login"},
		"notes":       {Status: 422},
		"limited":     {Status: 429, Repeat: "200×59, 500×1, 429×1"},
		"home":        {Status: 302, Location: "/welcome"},
	}
	c := CheckWith(inv, cat, &Exclusions{}, answers)
	want := []FailedRoute{
		{"POST /tree/reorder", []Failure{{ID: "reorder", Status: 500}}},
		{"GET /slow", []Failure{{ID: "slow", Detail: "no answer (timeout after 30s)"}}},
		{"GET /dashboard", []Failure{{ID: "dashboard", Status: 302, Detail: "redirect to the sign-in page /auth/Login"}}},
		{"GET /limited", []Failure{{ID: "limited", Status: 429, Detail: "a server error among the repeated sends (200×59, 500×1, 429×1)"}}},
	}
	if !reflect.DeepEqual(c.Failed, want) {
		t.Errorf("failed routes\n got %+v\nwant %+v", c.Failed, want)
	}
	// Not new (no answer yet), items (item-1 succeeded), login (a hand-written
	// 4xx), notes (no sample body), home (an ordinary redirect).
	if wantCov := []string{"GET /items/{id}", "POST /login", "GET /new", "POST /notes", "GET /home"}; !reflect.DeepEqual(c.Covered, wantCov) {
		t.Errorf("covered %v", c.Covered)
	}
	if !reflect.DeepEqual(c.ErrorOnly, []string{"POST /login"}) || !reflect.DeepEqual(c.NoBody, []string{"POST /notes"}) {
		t.Errorf("error only %v, no body %v", c.ErrorOnly, c.NoBody)
	}
	ids := []string{}
	for _, f := range c.Health.Failed {
		ids = append(ids, f.ID)
	}
	if !reflect.DeepEqual(ids, []string{"reorder", "item-2", "slow", "dashboard", "limited"}) {
		t.Errorf("failed requests %v", ids)
	}
	if s := c.Failed[1].Requests[0].String(); s != "slow: no answer (timeout after 30s)" {
		t.Error(s)
	}

	// A known failure keeps the request compared and the route honest:
	// neither covered nor failed. One that no longer fails, or matches
	// nothing, is stale.
	excl := &Exclusions{KnownFailures: []KnownFailure{
		{Request: "reorder", Reason: "fixed in a later upgrade step", Ref: "T-1"},
		{Request: "home*", Reason: "was broken"},
		{Request: "gone", Reason: "renamed"},
	}}
	c = CheckWith(inv, cat, excl, answers)
	if len(c.Failed) != 3 || !reflect.DeepEqual(c.Known, []string{"POST /tree/reorder"}) {
		t.Errorf("known: failed %+v known %v", c.Failed, c.Known)
	}
	if len(c.Health.Known) != 1 || c.Health.Known[0].String() != "reorder: 500 (known: fixed in a later upgrade step, T-1)" {
		t.Errorf("known failures %+v", c.Health.Known)
	}
	if !reflect.DeepEqual(c.Health.Stale, []string{"home* (its requests succeed)", "gone (no request)"}) {
		t.Errorf("stale %v", c.Health.Stale)
	}
	// Without a baseline, the catalog alone decides, and nothing is stale for succeeding.
	if c := CheckWith(inv, cat, excl, nil); len(c.Covered) != 9 || len(c.Health.Failed) != 0 || len(c.Health.Stale) != 1 {
		t.Errorf("without a baseline: %+v", c)
	}
}

// TestKnownFailureStatus: a known failure with a status acknowledges only
// that failure; another one fails, noted as changed. A glob that
// acknowledges failures on many routes is reported as broad.
func TestKnownFailureStatus(t *testing.T) {
	inv := &Inventory{}
	cat := &catalog.Catalog{}
	answers := map[string]Answer{}
	for i, a := range []Answer{
		{Status: 500},
		{Status: 404},
		{Error: "Get \"http://x/a\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"},
		{Error: "connection refused"},
		{Status: 429, Repeat: "200×59, 503×1, 429×1"},
		{Status: 500}, {Status: 500}, {Status: 500},
	} {
		path := "/r" + string(rune('a'+i))
		inv.Routes = append(inv.Routes, Route{Methods: []string{"GET"}, Path: path})
		r := generated("get-r"+string(rune('a'+i)), "GET", path, "ok", nil)
		cat.Requests = append(cat.Requests, r)
		answers[r.ID] = a
	}
	known := []KnownFailure{
		{Request: "get-ra", Reason: "r", Status: "500"},
		{Request: "get-rb", Reason: "r", Status: "500"}, // now a 404
		{Request: "get-rc", Reason: "r", Status: "timeout"},
		{Request: "get-rd", Reason: "r", Status: "timeout"}, // refused, not a timeout
		{Request: "get-re", Reason: "r", Status: "5xx"},
		{Request: "get-r*", Reason: "every other one"},
	}
	c := CheckWith(inv, cat, &Exclusions{KnownFailures: known}, answers)
	var failed, ok []string
	for _, f := range c.Health.Failed {
		failed = append(failed, f.String())
	}
	for _, f := range c.Health.Known {
		ok = append(ok, f.ID)
	}
	want := []string{"get-rb: 404 (its known failure is status 500: the failure changed)", "get-rd: no answer (connection refused) (its known failure is status timeout: the failure changed)"}
	if !reflect.DeepEqual(failed, want) {
		t.Errorf("failed\n got %q\nwant %q", failed, want)
	}
	if !reflect.DeepEqual(ok, []string{"get-ra", "get-rc", "get-re", "get-rf", "get-rg", "get-rh"}) {
		t.Errorf("acknowledged %v", ok)
	}
	if len(c.Health.Broad) != 0 || len(c.Health.Stale) != 0 {
		t.Errorf("broad %v stale %v", c.Health.Broad, c.Health.Stale)
	}
	// One glob over six failing routes is broad.
	c = CheckWith(inv, cat, &Exclusions{KnownFailures: []KnownFailure{{Request: "get-*", Reason: "all of them"}}}, answers)
	if !reflect.DeepEqual(c.Health.Broad, []string{"get-* (8 routes)"}) {
		t.Errorf("broad %v", c.Health.Broad)
	}
	for _, s := range []string{"500", "5xx", "4xx", "error", "timeout", ""} {
		if !validKnownStatus(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"50", "5XX", "oops", "700"} {
		if validKnownStatus(s) {
			t.Errorf("%q taken", s)
		}
	}
}

func TestLoadKnownFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exclusions.yaml")
	os.WriteFile(path, []byte("version: 1\nroutes: []\nknown_failures:\n  - request: get-tree\n    reason: r\n    status: 500\n"), 0o644)
	if e, err := LoadExclusions(path); err != nil || e.KnownFailures[0].Status != "500" {
		t.Fatalf("status as a number: %v %+v", err, e)
	}
	os.WriteFile(path, []byte("version: 1\nroutes: []\nknown_failures:\n  - request: get-tree\n    reason: r\n    status: broken\n"), 0o644)
	if _, err := LoadExclusions(path); err == nil || !strings.Contains(err.Error(), `status "broken" is not a status`) {
		t.Fatalf("bad status: %v", err)
	}
	os.WriteFile(path, []byte("version: 1\nroutes: []\nknown_failures:\n  - request: get-tree\n"), 0o644)
	if _, err := LoadExclusions(path); err == nil || !strings.Contains(err.Error(), "needs request and reason") {
		t.Fatalf("no reason: %v", err)
	}
}

// TestNoInvalidBody: a covered route whose body is validated (it has a
// sample, from its rules) is listed until a request sends it an invalid
// body: an empty-body request, or one written by hand that the baseline
// recorded as refused (400, 422). Other error cases do not count: they are
// answered before the body is validated.
func TestNoInvalidBody(t *testing.T) {
	inv := &Inventory{Routes: []Route{
		{Methods: []string{"POST"}, Path: "/posts", Body: json.RawMessage(`{"title":"a"}`)},
		{Methods: []string{"POST"}, Path: "/login", Body: json.RawMessage(`{"email":"a"}`)},
		{Methods: []string{"PUT"}, Path: "/notes/{id}", Body: json.RawMessage(`{"text":"a"}`)},
		{Methods: []string{"POST"}, Path: "/upload"}, // no sample: listed under NoBody instead
		{Methods: []string{"GET"}, Path: "/items"},
		{Methods: []string{"POST"}, Path: "/orders", Body: json.RawMessage(`{"sku":"a"}`)},
	}}
	body := func(r catalog.Request, b string) catalog.Request { r.Body = json.RawMessage(b); return r }
	cat := &catalog.Catalog{Requests: []catalog.Request{
		body(generated("posts", "POST", "/posts", "ok", nil), `{"title":"a"}`),
		body(generated("posts-anonymous", "POST", "/posts", "anonymous", nil), `{"title":"a"}`),
		body(generated("login", "POST", "/login", "ok", nil), `{"email":"a"}`),
		body(generated("login-empty-body", "POST", "/login", "empty-body", nil), `{}`),
		body(generated("note", "PUT", "/notes/{id}", "ok", map[string]string{"id": "1"}), `{"text":"a"}`),
		body(generated("note-not-found", "PUT", "/notes/{id}", "not-found", map[string]string{"id": "999999999"}), `{"text":"a"}`),
		{ID: "note-too-long", Method: "PUT", Path: "/notes/1", Body: json.RawMessage(`{"text":"…"}`)}, // by hand
		body(generated("upload", "POST", "/upload", "ok", nil), `{}`),
		generated("items", "GET", "/items", "ok", nil),
		body(generated("order", "POST", "/orders", "ok", nil), `{"sku":"a"}`),
		{ID: "order-wrong", Method: "POST", Path: "/orders", Body: json.RawMessage(`{"sku":"x"}`)}, // by hand, a 401
	}}
	answers := map[string]Answer{
		"posts": {Status: 201}, "posts-anonymous": {Status: 401}, "login": {Status: 200}, "login-empty-body": {Status: 422},
		"note": {Status: 200}, "note-not-found": {Status: 404}, "note-too-long": {Status: 422}, "upload": {Status: 422},
		"items": {Status: 200}, "order": {Status: 201}, "order-wrong": {Status: 401},
	}
	c := CheckWith(inv, cat, &Exclusions{}, answers)
	if want := []string{"POST /posts", "POST /orders"}; !reflect.DeepEqual(c.NoInvalidBody, want) {
		t.Errorf("no invalid body %v, want %v", c.NoInvalidBody, want)
	}
	// Without a baseline a request written by hand is not known to be
	// refused: the route is listed.
	c = CheckWith(inv, cat, &Exclusions{}, nil)
	if want := []string{"POST /posts", "PUT /notes/{id}", "POST /orders"}; !reflect.DeepEqual(c.NoInvalidBody, want) {
		t.Errorf("without a baseline: no invalid body %v, want %v", c.NoInvalidBody, want)
	}
	// An excluded or uncovered route is not listed: coverage says so already.
	excl := &Exclusions{Routes: []Exclusion{{Route: "/posts", Reason: "x"}}}
	cat.Requests = cat.Requests[2:]
	if c := CheckWith(inv, cat, excl, answers); !reflect.DeepEqual(c.NoInvalidBody, []string{"POST /orders"}) {
		t.Errorf("excluded: %v", c.NoInvalidBody)
	}
}
