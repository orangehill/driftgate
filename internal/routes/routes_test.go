package routes

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
)

func TestPattern(t *testing.T) {
	cases := []struct {
		path  string
		match []string
		miss  []string
	}{
		{"/", []string{"/"}, []string{"/x"}},
		{"/api/news/{id}", []string{"/api/news/12", "/api/news/12/", "/api/news/{id}"}, []string{"/api/news", "/api/news/1/2"}},
		{"/search/{term?}", []string{"/search", "/search/x"}, []string{"/search/x/y"}},
		{"/docs/{path*}", []string{"/docs/a", "/docs/a/b/c"}, []string{"/docs"}},
		{"/docs/{path*?}", []string{"/docs", "/docs/a/b"}, []string{"/doc"}},
		{"/user-{group?}", []string{"/user-x", "/user-"}, []string{"/", "/users"}},
		{"/files/{name}.json", []string{"/files/a.json"}, []string{"/files/a.xml"}},
		{"/files/{name}-{ext}", []string{"/files/a-b"}, []string{"/files/a"}},
		{"/v{major}.{minor?}/x", []string{"/v1.2/x", "/v1./x"}, []string{"/v1/x"}},
	}
	for _, c := range cases {
		re := Route{Path: c.path}.Pattern()
		for _, p := range c.match {
			if !re.MatchString(p) {
				t.Errorf("%s should match %s (%s)", c.path, p, re)
			}
		}
		for _, p := range c.miss {
			if re.MatchString(p) {
				t.Errorf("%s should not match %s (%s)", c.path, p, re)
			}
		}
	}
}

func TestCoverage(t *testing.T) {
	inv := &Inventory{Version: 1, Routes: []Route{
		{Methods: []string{"GET"}, Path: "/api/news/{id}"},
		{Methods: []string{"GET"}, Path: "/api/news/latest"},
		{Methods: []string{"PATCH", "PUT"}, Path: "/api/faq/{id}"},
		{Methods: []string{"POST"}, Path: "/api/faq"},
		{Methods: []string{"GET"}, Path: "/_debugbar/assets"},
		{Methods: []string{"*"}, Path: "/api/legacy"},
	}}
	cat := &catalog.Catalog{Requests: []catalog.Request{
		{ID: "news", Method: "GET", Path: "/api/news/{id}", Params: map[string]string{"id": "12"}},
		{ID: "latest", Method: "GET", Path: "/api/news/latest?page=2"},
		{ID: "faq-update", Method: "PUT", Path: "/api/faq/3"},
		{ID: "legacy", Method: "DELETE", Path: "/api/legacy"},
		{ID: "gone", Method: "GET", Path: "/api/old"},
	}}
	tie := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/docs/{all*}"}, {Methods: []string{"GET"}, Path: "/docs/{id}"}}}
	if r := Match(tie, catalog.Request{Method: "GET", Path: "/docs/5"}); r == nil || r.Path != "/docs/{id}" {
		t.Errorf("a single parameter should win over a rest-of-path one: %+v", r)
	}
	excl := &Exclusions{Routes: []Exclusion{
		{Route: "GET _debugbar/*", Reason: "development only"},
		{Route: "/nothing/*", Reason: "stale"},
	}}
	c := Check(inv, cat, excl)
	if !reflect.DeepEqual(c.Requests["GET /api/news/latest"], []string{"latest"}) || !reflect.DeepEqual(c.Requests["GET /api/news/{id}"], []string{"news"}) {
		t.Errorf("most specific route not chosen: %v", c.Requests)
	}
	if !reflect.DeepEqual(c.Uncovered, []string{"POST /api/faq"}) {
		t.Errorf("uncovered %v", c.Uncovered)
	}
	if c.Excluded["GET /_debugbar/assets"] != "development only" {
		t.Errorf("excluded %v", c.Excluded)
	}
	if !reflect.DeepEqual(c.Unknown, []string{"gone"}) || !reflect.DeepEqual(c.Stale, []string{"/nothing/*"}) {
		t.Errorf("unknown %v stale %v", c.Unknown, c.Stale)
	}
	if !reflect.DeepEqual(c.NoBody, []string{"PUT /api/faq/{id}", "POST /api/faq"}) {
		t.Errorf("no body %v", c.NoBody)
	}
}

func TestInventoryFileAndDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	old := &Inventory{Routes: []Route{
		{Methods: []string{"GET"}, Path: "/b", Source: "laravel"},
		{Methods: []string{"GET"}, Path: "/a", Name: "a", Source: "laravel"},
	}}
	if err := old.Save(path); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	loaded, err := LoadInventory(path)
	if err != nil || loaded.Routes[0].Path != "/a" {
		t.Fatalf("load: %v %+v", err, loaded)
	}
	loaded.Save(path)
	if second, _ := os.ReadFile(path); string(first) != string(second) {
		t.Fatal("saving twice is not byte-identical")
	}
	cur := &Inventory{Routes: []Route{
		{Methods: []string{"GET"}, Path: "/a", Name: "renamed", Source: "laravel"},
		{Methods: []string{"POST"}, Path: "/c", Source: "laravel"},
	}}
	got := DiffInventories(old, cur)
	want := []Change{{Kind: "route-changed", Route: "GET /a", What: "name"}, {Kind: "route-removed", Route: "GET /b"}, {Kind: "route-added", Route: "POST /c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diff %+v", got)
	}
	if _, err := LoadExclusions(filepath.Join(t.TempDir(), "x.yaml")); err != nil {
		t.Fatal("missing exclusions file should be empty")
	}
	bad := filepath.Join(t.TempDir(), "exclusions.yaml")
	os.WriteFile(bad, []byte("version: 1\nroutes:\n  - route: GET /x\n"), 0o644)
	if _, err := LoadExclusions(bad); err == nil {
		t.Fatal("exclusion without a reason accepted")
	}
	// A second document would be ignored; a trailing --- is harmless.
	os.WriteFile(bad, []byte("version: 1\n---\nroutes:\n  - route: GET /x\n    reason: r\n"), 0o644)
	if _, err := LoadExclusions(bad); err == nil || !strings.Contains(err.Error(), "only one YAML document") {
		t.Fatalf("second document: %v", err)
	}
	os.WriteFile(bad, []byte("version: 1\n---\n"), 0o644)
	if _, err := LoadExclusions(bad); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageNeedsASucceedingRequest(t *testing.T) {
	inv := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/carts/{id}"}}}
	cat := &catalog.Catalog{Requests: []catalog.Request{
		{ID: "nf", Method: "GET", Path: "/carts/{id}", Params: map[string]string{"id": "999999999"}, Source: &catalog.Source{Route: "GET /carts/{id}", Template: "not-found"}},
	}}
	c := Check(inv, cat, &Exclusions{})
	if len(c.Covered) != 0 || !reflect.DeepEqual(c.Uncovered, []string{"GET /carts/{id}"}) || !reflect.DeepEqual(c.Negative, c.Uncovered) {
		t.Fatalf("covered %v uncovered %v negative %v", c.Covered, c.Uncovered, c.Negative)
	}
	cat.Requests = append(cat.Requests, catalog.Request{ID: "hand", Method: "GET", Path: "/carts/5"})
	if c := Check(inv, cat, &Exclusions{}); len(c.Covered) != 1 {
		t.Fatalf("a hand-written request should cover: %+v", c)
	}
}

func TestDiffIgnoresFile(t *testing.T) {
	old := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/a", Action: "A@show"}}}
	cur := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/a", Action: "A@show", File: "app/A.php:12"}}}
	if c := DiffInventories(old, cur); len(c) != 0 {
		t.Errorf("file filled for the first time: %v", c)
	}
	moved := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/a", Action: "A@show", File: "app/A.php:40"}}}
	if c := DiffInventories(cur, moved); len(c) != 0 {
		t.Errorf("line moved: %v", c)
	}
	renamed := &Inventory{Routes: []Route{{Methods: []string{"GET"}, Path: "/a", Action: "B@show", File: "app/B.php:3"}}}
	if c := DiffInventories(cur, renamed); len(c) != 1 || c[0].What != "action" {
		t.Errorf("action change: %v", c)
	}
}

// TestInventoryPathStartsWithSlash: routes.json is in the repository, and a
// path without a leading "/" would send generated requests to another host.
func TestInventoryPathStartsWithSlash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	os.WriteFile(path, []byte(`{"version": 1, "routes": [{"methods": ["GET"], "path": "@127.0.0.1:8906/steal", "source": "openapi"}]}`), 0o644)
	if _, err := LoadInventory(path); err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("load: %v", err)
	}
}
