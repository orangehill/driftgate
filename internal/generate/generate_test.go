package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/routes"
)

func project(t *testing.T, yaml config.Config) *config.Project {
	t.Helper()
	yaml.Version, yaml.Project, yaml.Mode, yaml.Level = 1, "p", config.Freeze, "values"
	p, err := config.New(yaml, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerate(t *testing.T) {
	p := project(t, config.Config{
		State:     map[string]config.Component{"db": {Driver: "mysql", DSN: config.Secret{Env: "X"}, Working: "app"}},
		Templates: []string{"ok", "not-found", "json"},
		Params: []config.Param{
			{Name: "id", Route: "/api/news/*", SQL: "SELECT id FROM news ORDER BY id", Foreach: true},
			{Name: "id", Route: "/api/faq/*", From: &config.From{Request: "post-api-faq", Path: "$.entry.id"}},
			{Name: "path", Value: "guide/intro"},
		},
	})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"GET"}, Path: "/api/news/{id}"},
		{Methods: []string{"GET"}, Path: "/api/faq/{id}"},
		{Methods: []string{"POST"}, Path: "/api/faq", Body: json.RawMessage(`{"q":"x"}`)},
		{Methods: []string{"GET"}, Path: "/docs/{path*?}"},
		{Methods: []string{"GET"}, Path: "/search/{term?}"},
		{Methods: []string{"GET"}, Path: "/hand"},
	}}
	inv.Sort() // as routes.json is
	existing := &catalog.Catalog{Requests: []catalog.Request{{ID: "hand-made", Method: "GET", Path: "/hand", Storage: "public"}}}
	queries := 0
	opts := Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: existing,
		Query: func(ctx context.Context, state, sql string) ([]string, error) {
			queries++
			return []string{"7", "9"}, nil
		}}
	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]catalog.Request{}
	var ids []string
	for _, r := range res.Catalog.Requests {
		got[r.ID] = r
		ids = append(ids, r.ID)
	}
	want := []string{"hand-made",
		"post-api-faq", "post-api-faq-json",
		"get-api-faq-id", "get-api-faq-id-not-found", "get-api-faq-id-json",
		"get-api-news-id-7", "get-api-news-id-9", "get-api-news-id-not-found", "get-api-news-id-7-json", "get-api-news-id-9-json",
		"get-docs-path", "get-docs-path-json", // its only parameter is optional: no not-found
		"get-search-term", "get-search-term-json"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids\n got %v\nwant %v", ids, want)
	}
	if queries != 1 {
		t.Errorf("the same query ran %d times", queries)
	}
	if r := got["get-docs-path"]; r.Path != "/docs/guide/intro" {
		t.Errorf("rest-of-path value: %s", r.Path)
	}
	if r := got["get-search-term"]; r.Path != "/search" {
		t.Errorf("optional without a value: %s", r.Path)
	}
	if r := got["get-api-faq-id"]; r.ParamsFrom["id"].Path != "$.entry.id" || r.Path != "/api/faq/{id}" {
		t.Errorf("from: %+v", r)
	}
	if r := got["post-api-faq"]; !r.Mutating() || string(r.Body) != `{"q":"x"}` || r.Storage != "private" {
		t.Errorf("post: %+v", r)
	}
	if !reflect.DeepEqual(res.Catalog.Sequences, []catalog.Sequence{{ID: "gen-after-post-api-faq", Requests: []string{"post-api-faq", "get-api-faq-id", "get-api-faq-id-json"}}}) {
		t.Errorf("sequences %+v", res.Catalog.Sequences)
	}
	if err := res.Catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	// Generating again from the result changes nothing.
	opts.Existing = res.Catalog
	again, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(res.Catalog)
	b, _ := json.Marshal(again.Catalog)
	if string(a) != string(b) {
		t.Fatal("second run differs")
	}
	// A required parameter without a value: reported, not guessed.
	p.Params = nil
	res, err = Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: existing})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) == 0 || res.Skipped[0].Reason != "no value for {id}" {
		t.Errorf("skipped %+v", res.Skipped)
	}
}

func TestFromChainIsOneSequence(t *testing.T) {
	p := project(t, config.Config{Params: []config.Param{
		{Name: "a", From: &config.From{Request: "post-x", Path: "$.id"}},
		{Name: "b", From: &config.From{Request: "get-x-a", Path: "$.id"}},
	}})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"POST"}, Path: "/x"},
		{Methods: []string{"GET"}, Path: "/x/{a}"},
		{Methods: []string{"GET"}, Path: "/y/{b}"},
	}}
	inv.Sort()
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	want := []catalog.Sequence{{ID: "gen-after-post-x", Requests: []string{"post-x", "get-x-a", "get-y-b"}}}
	if !reflect.DeepEqual(res.Catalog.Sequences, want) {
		t.Fatalf("sequences %+v", res.Catalog.Sequences)
	}
	if err := res.Catalog.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStableIDs(t *testing.T) {
	p := project(t, config.Config{})
	gen := func(paths ...string) map[string]string {
		inv := &routes.Inventory{}
		for _, path := range paths {
			inv.Routes = append(inv.Routes, routes.Route{Methods: []string{"GET"}, Path: path})
		}
		inv.Sort()
		res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range res.Catalog.Requests {
			out[r.Path] = r.ID
		}
		return out
	}
	long := "/api/v1/organizations/settings/notifications/"
	before := gen(long+"email", long+"sms")
	after := gen(long+"email", long+"sms", long+"app")
	if before[long+"email"] == before[long+"sms"] || before[long+"email"] != after[long+"email"] || before[long+"sms"] != after[long+"sms"] {
		t.Fatalf("IDs not unique or not stable:\n%v\n%v", before, after)
	}
	for _, id := range after {
		if len(id) > 48 {
			t.Errorf("id too long: %s", id)
		}
	}
	twins := gen("/a/{id}", "/a/id")
	if twins["/a/{id}"] == twins["/a/id"] {
		t.Fatalf("colliding bases: %v", twins)
	}
}

func TestMethodsAndObserve(t *testing.T) {
	p := project(t, config.Config{Observe: []config.Observe{{Match: "get-items-not-found"}, {Match: "GET /slow"}}, Templates: []string{"ok", "not-found"},
		Params: []config.Param{{Name: "id", Value: "1"}}})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"GET", "POST"}, Path: "/items"},
		{Methods: []string{"PATCH", "PUT"}, Path: "/items/{id}"},
		{Methods: []string{"GET"}, Path: "/slow"},
		{Methods: []string{"*"}, Path: "/any"},
	}}
	inv.Sort()
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]catalog.Request{}
	for _, r := range res.Catalog.Requests {
		got[r.ID] = r
	}
	for _, id := range []string{"get-items", "post-items", "put-items-id", "get-slow", "get-any"} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing %s in %v", id, got)
		}
	}
	if _, ok := got["patch-items-id"]; ok {
		t.Error("PATCH generated next to PUT of the same route")
	}
	if !got["get-slow"].Observe || got["get-items"].Observe {
		t.Errorf("observe by route: slow %v items %v", got["get-slow"].Observe, got["get-items"].Observe)
	}
	p.Observe = []config.Observe{{Match: "put-items-id-not-found"}}
	res, _ = Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	for _, r := range res.Catalog.Requests {
		if r.Observe != (r.ID == "put-items-id-not-found") {
			t.Errorf("observe by ID: %s %v", r.ID, r.Observe)
		}
	}
}

// TestRequestSettings: requests: in the configuration says which
// generated requests change data and gives their bodies, by route or by
// ID; the last entry that names a request wins, and empty-body keeps {}.
func TestRequestSettings(t *testing.T) {
	yes, no := true, false
	p := project(t, config.Config{Templates: []string{"ok", "empty-body"}, Params: []config.Param{{Name: "id", Value: "1"}},
		Requests: []config.RequestSetting{
			{Match: "GET /items/*", Mutates: &yes},
			{Match: "POST /search", Mutates: &no, Body: yamlBody(t, "{q: news, page: 1}")},
			{Match: "post-items", Body: yamlBody(t, "[a]")},
			{Match: "post-items", Body: yamlBody(t, "{name: x}")},
		}})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"GET"}, Path: "/items/{id}"},
		{Methods: []string{"GET", "POST"}, Path: "/items", BodyType: "application/x-www-form-urlencoded", Body: json.RawMessage(`{"name":"sample"}`)},
		{Methods: []string{"POST"}, Path: "/search", Body: json.RawMessage(`{"q":"sample"}`)},
	}}
	inv.Sort()
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.Catalog.Requests {
		m := "-"
		if r.Mutates != nil {
			m = fmt.Sprint(*r.Mutates)
		}
		got[r.ID] = fmt.Sprintf("%s %s %v", m, string(r.Body), r.Headers)
	}
	want := map[string]string{
		"get-items-id":           "true  []",
		"get-items":              "-  []",
		"post-items":             `true {"name":"x"} []`,
		"post-items-empty-body":  "true {} [[content-type application/x-www-form-urlencoded]]",
		"post-search":            `false {"q":"news","page":1} []`,
		"post-search-empty-body": "false {} []",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests:\n got %v\nwant %v", got, want)
	}
}

func yamlBody(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}
	return n.Content[0]
}

// TestRequestSettingsRefused: mutates: false of a DELETE, PUT or PATCH,
// or of requests of more than one route, is refused; an entry that names
// no generated request is listed.
func TestRequestSettingsRefused(t *testing.T) {
	no := false
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"POST", "PUT"}, Path: "/items"},
		{Methods: []string{"POST"}, Path: "/search"},
		{Methods: []string{"PURGE"}, Path: "/cache"},
	}}
	inv.Sort()
	for _, c := range []struct{ match, err string }{
		{"PURGE /cache", "mutates: false names a PURGE request"},
		{"PUT /items", "requests 1 (match \"PUT /items\"): mutates: false names a PUT request"},
		{"POST *", "requests 1 (match \"POST *\"): mutates: false names requests of 2 routes"},
		{"POST /search", ""},
	} {
		p := project(t, config.Config{Requests: []config.RequestSetting{{Match: c.match, Mutates: &no}, {Match: "GET /none", Mutates: &no}}})
		res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
		switch {
		case c.err != "" && (err == nil || !errors.Is(err, ErrSetting) || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v", c.match, err)
		case c.err == "" && (err != nil || !reflect.DeepEqual(res.Unused, []string{`requests 2 (match "GET /none")`})):
			t.Errorf("%s: %v %+v", c.match, err, res)
		}
	}
}

// TestPerMethod: a hand-written GET does not stop POST of the same route from
// being generated, and excluding POST does not exclude GET.
func TestPerMethod(t *testing.T) {
	p := project(t, config.Config{})
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"GET", "POST"}, Path: "/items"}}}
	manual := &catalog.Catalog{Requests: []catalog.Request{{ID: "hand", Method: "GET", Path: "/items", Storage: "public"}}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: manual})
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 1 || res.Catalog.Requests[1].ID != "post-items" {
		t.Fatalf("with a hand-written GET: %+v", res.Catalog.Requests)
	}
	excl := &routes.Exclusions{Routes: []routes.Exclusion{{Route: "POST /items", Reason: "x"}}}
	res, _ = Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: excl, Existing: &catalog.Catalog{}})
	if res.Generated != 1 || res.Catalog.Requests[0].ID != "get-items" {
		t.Fatalf("with POST excluded: %+v", res.Catalog.Requests)
	}
	c := routes.CheckWith(inv, res.Catalog, excl, nil)
	if !reflect.DeepEqual(c.Covered, []string{"GET /items"}) || c.Excluded["POST /items"] != "x" {
		t.Fatalf("coverage per method: %+v", c)
	}
}

func TestRequiredQuery(t *testing.T) {
	p := project(t, config.Config{Params: []config.Param{{Name: "currencyCode", Value: "USD"}}})
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"GET"}, Path: "/info", Query: map[string]string{"storeId": "1", "currencyCode": "driftgate"}}}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Catalog.Requests[0].Path; got != "/info?currencyCode=USD&storeId=1" {
		t.Fatalf("path %s", got)
	}
	if r := routes.Match(inv, res.Catalog.Requests[0]); r == nil {
		t.Fatal("a request with a query does not reach its route")
	}
}

// TestJSONErrors: json-errors adds an Accept: application/json twin after
// each error variant, never after the ok request, and the twins do not
// cover a route.
func TestJSONErrors(t *testing.T) {
	p := project(t, config.Config{
		Templates:  []string{"ok", "not-found", "anonymous", "empty-body", "json-errors"},
		Identities: map[string]config.Identity{"admin": {Env: "X"}},
		Generate:   config.Generate{Identity: "admin"},
		Params:     []config.Param{{Name: "id", Value: "1"}},
	})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"PUT"}, Path: "/api/items/{id}", Auth: true, Body: json.RawMessage(`{"name":"x"}`), BodyType: "application/json"},
	}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	got := map[string]catalog.Request{}
	for _, r := range res.Catalog.Requests {
		ids, got[r.ID] = append(ids, r.ID), r
	}
	want := []string{"put-api-items-id",
		"put-api-items-id-not-found", "put-api-items-id-not-found-json",
		"put-api-items-id-anonymous", "put-api-items-id-anonymous-json",
		"put-api-items-id-empty-body", "put-api-items-id-empty-body-json"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids\n got %v\nwant %v", ids, want)
	}
	j := got["put-api-items-id-anonymous-json"]
	if j.Identity != "" || j.Source.Template != "json-errors" || string(j.Body) != `{"name":"x"}` ||
		!reflect.DeepEqual(j.Headers, [][2]string{{"accept", "application/json"}, {"content-type", "application/json"}}) {
		t.Errorf("anonymous json twin: %+v", j)
	}
	if got["put-api-items-id-not-found"].Headers[0][0] == "accept" {
		t.Error("the error variant itself changed")
	}
	// A route that takes files: the empty-body twin sends an empty form too.
	inv.Routes[0].Files = []routes.FileField{{Field: "photo", Kind: "png"}}
	withFiles, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range withFiles.Catalog.Requests {
		if r.Multipart == nil || strings.Contains(r.ID, "empty-body") != (len(r.Multipart.Files) == 0) {
			t.Errorf("multipart of %s: %+v", r.ID, r.Multipart)
		}
	}
	// Only the ok request covers the route.
	res.Catalog.Requests = res.Catalog.Requests[1:]
	if c := routes.CheckWith(inv, res.Catalog, &routes.Exclusions{}, nil); len(c.Covered) != 0 {
		t.Errorf("an error variant covers: %+v", c)
	}
}

// TestJSONErrorsWithoutValue: a route without a parameter value skips the
// templates that need one, but not json-errors, whose twins of not-found
// and invalid-param are still made.
func TestJSONErrorsWithoutValue(t *testing.T) {
	p := project(t, config.Config{Templates: []string{"ok", "not-found", "invalid-param", "json", "json-errors"}})
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"GET"}, Path: "/api/items/{id}"}}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	var skipped, ids []string
	for _, s := range res.Skipped {
		skipped = append(skipped, s.Template)
	}
	for _, r := range res.Catalog.Requests {
		ids = append(ids, r.ID)
	}
	if want := []string{"ok", "json"}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped %v, want %v", skipped, want)
	}
	if want := []string{"get-api-items-id-not-found", "get-api-items-id-not-found-json", "get-api-items-id-invalid-param", "get-api-items-id-invalid-param-json"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids\n got %v\nwant %v", ids, want)
	}
}

// TestJSONErrorsWithoutTwins: without a parameter value and without
// not-found or invalid-param, json-errors makes nothing, and is listed as
// skipped, with the reason.
func TestJSONErrorsWithoutTwins(t *testing.T) {
	p := project(t, config.Config{Templates: []string{"ok", "json-errors"}})
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"GET"}, Path: "/api/items/{id}"}}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
	if err != nil {
		t.Fatal(err)
	}
	var skipped []string
	for _, s := range res.Skipped {
		skipped = append(skipped, s.Template+": "+s.Reason)
	}
	if want := []string{"ok: no value for {id}", "json-errors: no value for {id}"}; !reflect.DeepEqual(skipped, want) || len(res.Catalog.Requests) != 0 {
		t.Errorf("skipped %v, want %v; requests %v", skipped, want, res.Catalog.Requests)
	}
}

// TestJSONErrorsNothingToFollow: a route where no error template makes a
// request (no parameter, as GET /api/news with init's templates) lists
// nothing as skipped; json-errors without any error template is noted once,
// for the configuration.
func TestJSONErrorsNothingToFollow(t *testing.T) {
	inv := &routes.Inventory{Routes: []routes.Route{{Methods: []string{"GET"}, Path: "/api/news"}, {Methods: []string{"GET"}, Path: "/api/items"}}}
	for _, c := range []struct {
		templates []string
		notes     int
	}{{[]string{"ok", "not-found", "json", "json-errors"}, 0}, {[]string{"ok", "json-errors"}, 1}} {
		p := project(t, config.Config{Templates: c.templates})
		res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: &catalog.Catalog{}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Skipped) != 0 || len(res.Notes) != c.notes || c.notes == 1 && !strings.Contains(res.Notes[0], "json-errors repeats the requests of not-found, invalid-param, anonymous, empty-body") {
			t.Errorf("%v: skipped %+v, notes %q", c.templates, res.Skipped, res.Notes)
		}
	}
}

// TestReplacedByHand: a route method an entry written by hand reaches gets
// no generated requests, and the result names them with the entries that
// take their place; the IDs and the rest of the catalog are as without
// the note. --all generates them and replaces nothing.
func TestReplacedByHand(t *testing.T) {
	p := project(t, config.Config{Templates: []string{"ok", "not-found", "json", "json-errors"},
		Params:   []config.Param{{Name: "id", Value: "1"}},
		Requests: []config.RequestSetting{{Match: "GET /limited", Mutates: new(bool)}}})
	inv := &routes.Inventory{Routes: []routes.Route{
		{Methods: []string{"GET"}, Path: "/limited"},
		{Methods: []string{"GET"}, Path: "/items/{id}"},
		{Methods: []string{"GET"}, Path: "/nothing/{slug}"}, // no value: generate makes only not-found…
	}}
	inv.Sort()
	manual := &catalog.Catalog{Requests: []catalog.Request{
		{ID: "limited-throttle", Method: "GET", Path: "/limited", Repeat: 6, Storage: "public"},
		{ID: "limited-throttle-json", Method: "GET", Path: "/limited", Repeat: 6, Storage: "public"},
		{ID: "item-by-hand", Method: "GET", Path: "/items/{id}", Params: map[string]string{"id": "2"}, Storage: "public"},
	}}
	res, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: manual})
	if err != nil {
		t.Fatal(err)
	}
	want := []Replaced{
		{Route: "GET /items/{id}", ByHand: []string{"item-by-hand"}, Generated: []string{"get-items-id", "get-items-id-not-found", "get-items-id-not-found-json", "get-items-id-json"}},
		{Route: "GET /limited", ByHand: []string{"limited-throttle", "limited-throttle-json"}, Generated: []string{"get-limited", "get-limited-json"}},
	}
	if !reflect.DeepEqual(res.Replaced, want) {
		t.Errorf("replaced\n got %+v\nwant %+v", res.Replaced, want)
	}
	var ids []string
	for _, r := range res.Catalog.Requests {
		ids = append(ids, r.ID)
	}
	if want := []string{"limited-throttle", "limited-throttle-json", "item-by-hand", "get-nothing-slug-not-found", "get-nothing-slug-not-found-json"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("catalog %v, want %v", ids, want)
	}
	// The setting that names the replaced route only is still reported as unused.
	if len(res.Unused) != 1 {
		t.Errorf("unused %v", res.Unused)
	}
	all, err := Run(context.Background(), Options{Project: p, Inventory: inv, Exclusions: &routes.Exclusions{}, Existing: manual, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Replaced) != 0 || all.Generated != 8 {
		t.Errorf("--all: replaced %v, generated %d", all.Replaced, all.Generated)
	}
}
