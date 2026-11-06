package cli

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/secret"
)

// TestBaselineHoldsABug: a baseline made while a request meant to succeed
// answers 500 is made, but the run says so and fails; coverage then counts
// the route as not covered until the response succeeds, and an accept of
// the fixed response clears it.
func TestBaselineHoldsABug(t *testing.T) {
	var fixed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch {
		case r.URL.Path == "/api/tree" && !fixed.Load():
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"boom"}`)
		case strings.HasPrefix(r.URL.Path, "/api/items/") && r.URL.Path != "/api/items/1":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":"not found"}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, filepath.Join(root, "routes-src.json"), `[{"method":"GET","path":"/api/news"},{"method":"GET","path":"/api/tree"},{"method":"GET","path":"/api/items/{id}"}]`)
	write(t, cfg, fmt.Sprintf(`version: 1
project: health
mode: evolve
target:
  base_url: %s
discover:
  - adapter: command
    command: ["cat", "routes-src.json"]
templates: [ok, not-found]
params:
  - name: id
    values: ["1", "2"]
    foreach: true
storage:
  public: ["*"]
`, srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: Time of the response.\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	code, out, errOut := runCLI(t, "run", "--config", cfg)
	if code != ExitDrift || !strings.Contains(out, "baseline local made") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"failed in the baseline (an error the baseline keeps green: fix it and accept the new answer), baseline local", "get-api-tree: 500", "get-api-items-id-2: 404",
		"GET /api/tree ← get-api-tree: 500", "requests meant to succeed failed in the baseline"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "get-api-items-id-not-found:") {
		t.Errorf("an error variant was reported:\n%s", out)
	}

	// coverage reads the baseline folder alone: no local store is made.
	store := os.Getenv("DRIFTGATE_STORE")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "none-yet"))
	code, out, _ = runCLI(t, "coverage", "--config", cfg, "--format", "json")
	if _, err := os.Stat(os.Getenv("DRIFTGATE_STORE")); err == nil {
		t.Error("coverage made a local store")
	}
	t.Setenv("DRIFTGATE_STORE", store)
	var cov struct {
		Covered []string `json:"covered"`
		Checked bool     `json:"baseline_checked"`
		Failed  []struct {
			Route    string `json:"route"`
			Requests []struct {
				ID     string `json:"id"`
				Status int    `json:"status"`
			} `json:"requests"`
		} `json:"failed_in_baseline"`
		Requests []struct {
			ID     string `json:"id"`
			Status int    `json:"status"`
		} `json:"requests_failed_in_baseline"`
	}
	if err := json.Unmarshal([]byte(out), &cov); err != nil || code != ExitDrift {
		t.Fatalf("coverage: %d %v\n%s", code, err, out)
	}
	if !cov.Checked || len(cov.Failed) != 1 || cov.Failed[0].Route != "GET /api/tree" || cov.Failed[0].Requests[0].Status != 500 || len(cov.Requests) != 2 || len(cov.Covered) != 2 {
		t.Fatalf("coverage: %+v", cov)
	}
	// A baseline named must exist; none checks the catalog alone.
	if code, _, errOut = runCLI(t, "coverage", "--config", cfg, "--baseline", "other"); code != ExitConfig || !strings.Contains(errOut, "baseline other does not exist") {
		t.Fatalf("coverage --baseline other: %d %s", code, errOut)
	}
	if code, out, _ = runCLI(t, "coverage", "--config", cfg, "--baseline", "none"); code != ExitClean {
		t.Fatalf("coverage --baseline none: %d\n%s", code, out)
	}

	// A known failure: the run is clean, the request is still compared.
	excl := filepath.Join(root, "driftgate", "exclusions.yaml")
	write(t, excl, "version: 1\nroutes: []\nknown_failures:\n  - request: get-api-tree\n    reason: fixed in a later upgrade step\n    ref: T-1\n")
	code, out, _ = runCLI(t, "run", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "get-api-tree: 500 (known: fixed in a later upgrade step, T-1)") || !strings.Contains(out, "1 with known failures") || !strings.Contains(out, "No drift") {
		t.Fatalf("run with a known failure: %d\n%s", code, out)
	}
	fixed.Store(true)
	if code, out, _ = runCLI(t, "run", "--config", cfg); code != ExitDrift || !strings.Contains(out, "status 500 → 200") {
		t.Fatalf("a known failure fixed is drift: %d\n%s", code, out)
	}
	if code, out, errOut = runCLI(t, "accept", "get-api-tree", "--reason", "the tree answers again", "--config", cfg); code != ExitClean || strings.Contains(out, "failed in the baseline") {
		t.Fatalf("accept: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ = runCLI(t, "coverage", "--config", cfg); code != ExitClean || !strings.Contains(out, "get-api-items-id-2: 404") || !strings.Contains(out, "get-api-tree (its requests succeed)") {
		t.Fatalf("coverage after the fix: %d\n%s", code, out)
	}
	write(t, excl, "version: 1\nroutes: []\n")

	// Accepting an error is allowed (the reason may explain it), but said.
	fixed.Store(false)
	runCLI(t, "run", "--config", cfg)
	if code, out, _ = runCLI(t, "accept", "get-api-tree", "--reason", "broken again", "--config", cfg); code != ExitClean || !strings.Contains(out, "GET /api/tree ← get-api-tree: 500") || strings.Contains(out, "get-api-items-id-2") {
		t.Fatalf("accept of a 500: %d\n%s", code, out)
	}
}

// TestKnownFailuresOfOtherServices: coverage of one service does not call
// another service's known failures stale; one that matches no request of
// any service still is.
func TestKnownFailuresOfOtherServices(t *testing.T) {
	inv := &routes.Inventory{Routes: []routes.Route{
		{Target: "api", Methods: []string{"GET"}, Path: "/a"},
		{Target: "web", Methods: []string{"GET"}, Path: "/b"},
	}}
	cat := &catalog.Catalog{Requests: []catalog.Request{
		{ID: "get-a", Target: "api", Method: "GET", Path: "/a"},
		{ID: "get-b", Target: "web", Method: "GET", Path: "/b"},
	}}
	excl := &routes.Exclusions{KnownFailures: []routes.KnownFailure{
		{Request: "get-b", Reason: "web's"},
		{Request: "get-gone", Reason: "renamed"},
	}}
	var b strings.Builder
	c, _, err := checkCoverage(selection{"api": true}, cat, inv, excl, nil, "text", &b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Health.Stale, []string{"get-gone (no request)"}) {
		t.Errorf("stale %v\n%s", c.Health.Stale, b.String())
	}
}
