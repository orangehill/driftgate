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
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
)

// TestNotCoveredSummary: run and compare end with what the result does not
// cover (an error the baseline holds, a route without a request, a request
// that changes data only observed), the catalog by what its requests do,
// and the one change almost every drifted request has with its rule; none
// of it changes the exit code.
func TestNotCoveredSummary(t *testing.T) {
	server := "Apache/2.4"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("server", server)
		if r.URL.Path == "/api/tree" {
			w.WriteHeader(500)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	var paths []string
	for i := range 6 {
		paths = append(paths, fmt.Sprintf(`{"method":"GET","path":"/api/news%d"}`, i))
	}
	write(t, filepath.Join(root, "routes-src.json"), `[`+strings.Join(paths, ",")+`,{"method":"GET","path":"/api/tree"},{"method":"POST","path":"/api/import"}]`)
	write(t, cfg, fmt.Sprintf(`version: 1
project: gaps
mode: freeze
target:
  base_url: %s
discover:
  - adapter: command
    command: ["cat", "routes-src.json"]
templates: [ok]
storage:
  public: ["*"]
`, srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: Time of the response.\n")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	if code, out, errOut := runCLI(t, "run", "--config", cfg, "--allow-mutating-without-restore"); code != ExitDrift || !strings.Contains(out, "baseline local made") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	// The import is measured only; the other server answers every request.
	catPath := filepath.Join(root, "driftgate", "catalog.json")
	raw, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatal(err)
	}
	write(t, catPath, strings.Replace(string(raw), `"method": "POST",`, `"method": "POST", "observe": true,`, 1))
	server = "nginx/1.25"

	code, out, errOut := runCLI(t, "run", "--config", cfg, "--allow-mutating-without-restore")
	rule := `- {name: 'server', format: 'nginx/1\.25', reason: '…'}`
	for _, want := range []string{
		"catalog: 8 requests: 7 reads, 1 mutating, 0 command steps; 0 of them multipart",
		"Not covered by this result (driftgate coverage lists the routes):",
		"1 request meant to succeed that failed in baseline:local: get-api-tree",
		"1 request observed (measured, not compared): post-api-import",
		"one change in 7 of 7 drifted requests: header server changed value",
		"set aside, 0 of 7 drift in something else",
		"when the other stack is meant to answer so, add under headers: in allowlist.yaml " + rule,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run lacks %q:\n%s\n%s", want, out, errOut)
		}
	}
	if code != ExitDrift {
		t.Errorf("run: exit %d", code)
	}

	// compare reads the routes itself: a route the catalog has no request
	// for is named; the JSON report holds it all.
	invPath := filepath.Join(root, "driftgate", "routes.json")
	var inv map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, invPath)), &inv); err != nil {
		t.Fatal(err)
	}
	inv["routes"] = append(inv["routes"].([]any), map[string]any{"methods": []string{"GET"}, "path": "/api/extra"})
	raw, _ = json.Marshal(inv)
	write(t, invPath, string(raw))
	code, out, _ = runCLI(t, "compare", runTagFor(t, cfg), "--config", cfg, "--format", "json")
	var r struct {
		NotCovered struct {
			Catalog       map[string]int `json:"catalog"`
			RoutesChecked bool           `json:"routes_checked"`
			NoRequest     struct {
				Count int      `json:"count"`
				Names []string `json:"names"`
			} `json:"no_request"`
			Failed struct {
				Count int `json:"count"`
			} `json:"failed_in_baseline"`
			Observed struct {
				Names []string `json:"names"`
			} `json:"observed"`
		} `json:"not_covered"`
		Dominant struct {
			Requests, Elsewhere int
			Rule                string
		} `json:"dominant"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != ExitDrift {
		t.Fatalf("compare: %d %v\n%s", code, err, out)
	}
	n := r.NotCovered
	if !n.RoutesChecked || n.Catalog["multipart"] != 0 || n.Catalog["requests"] != 8 || n.NoRequest.Count != 1 || n.NoRequest.Names[0] != "GET /api/extra" ||
		n.Failed.Count != 1 || len(n.Observed.Names) != 1 || r.Dominant.Requests != 7 || r.Dominant.Rule != rule {
		t.Errorf("not covered: %+v %+v", n, r.Dominant)
	}

	// The rule pastes under headers:, passes allowlist check and ends the
	// drift of the server header alone.
	allowPath := filepath.Join(root, "driftgate", "allowlist.yaml")
	write(t, allowPath, mustRead(t, allowPath)+"  "+strings.Replace(rule, "…", "The new stack runs nginx.", 1)+"\n")
	if code, out, errOut := runCLI(t, "allowlist", "check", "--config", cfg); code != ExitClean {
		t.Fatalf("allowlist check: %d\n%s\n%s", code, out, errOut)
	}
	// Two captures: the line names neither accept nor expect.
	if code, out, _ := runCLI(t, "capture", "again", "--config", cfg, "--allow-mutating-without-restore"); code != ExitClean {
		t.Fatalf("capture again: %d\n%s", code, out)
	}
	server = "caddy"
	if code, out, _ := runCLI(t, "capture", "third", "--config", cfg, "--allow-mutating-without-restore"); code != ExitClean {
		t.Fatalf("capture third: %d\n%s", code, out)
	}
	_, out, _ = runCLI(t, "compare", "third", "--against", "again", "--config", cfg)
	if !strings.Contains(out, "name it in the pull request") || strings.Contains(out, "driftgate accept group") {
		t.Errorf("two captures:\n%s", out)
	}
	// A capture made with --skip-fingerprint-check against the baseline:
	// captured again without the flag first.
	if code, out, _ := runCLI(t, "capture", "unreliable", "--skip-fingerprint-check", "--config", cfg, "--allow-mutating-without-restore"); code != ExitClean {
		t.Fatalf("capture unreliable: %d\n%s", code, out)
	}
	_, out, _ = runCLI(t, "compare", "unreliable", "--skip-fingerprint-check", "--config", cfg)
	if !strings.Contains(out, "capture again without --skip-fingerprint-check") || strings.Contains(out, "driftgate accept group") {
		t.Errorf("unreliable capture:\n%s", out)
	}
	server = "nginx/1.25"
	if code, out, _ = runCLI(t, "compare", runTagFor(t, cfg), "--config", cfg); code != ExitClean || strings.Contains(out, "one change in") {
		t.Errorf("with the rule: exit %d\n%s", code, out)
	}
	// --only counts the compared requests and judges no route.
	_, out, _ = runCLI(t, "compare", runTagFor(t, cfg), "--config", cfg, "--only", "get-api-news*")
	if !strings.Contains(out, "compared (--only): 6 requests: 6 reads, 0 mutating, 0 command steps; 0 of them multipart") || strings.Contains(out, "route method") {
		t.Errorf("--only:\n%s", out)
	}
	// One baseline against another: neither, as accept and expect take
	// only a capture against a baseline.
	server = "caddy"
	if code, out, errOut := runCLI(t, "run", "--baseline", "other", "--config", cfg, "--allow-mutating-without-restore"); !strings.Contains(out, "baseline other made") {
		t.Fatalf("run --baseline other: %d\n%s\n%s", code, out, errOut)
	}
	_, out, _ = runCLI(t, "compare", "baseline:other", "--against", "local", "--config", cfg)
	if !strings.Contains(out, "one change in") || !strings.Contains(out, "name it in the pull request") || strings.Contains(out, "driftgate accept group") {
		t.Errorf("baseline against baseline:\n%s", out)
	}
}

// Of a change between builds no rule is proposed: a header of the
// application (content-type) or a value. The line still says how many
// requests drift in something else, and what to do instead.
func TestDominantProposesNoGuess(t *testing.T) {
	r := report.Build("capture:c", "baseline:local", nil)
	for i := range 6 {
		id := fmt.Sprintf("get-%d", i)
		r.Results = append(r.Results, report.Result{ID: id, Outcome: report.Drift, Drifts: []compare.Drift{
			{Request: id, Kind: compare.KindHeaderValue, Where: "header:content-type", Values: [2][]string{{"application/json"}, {"text/html"}}}}})
	}
	r = report.Build("capture:c", "baseline:local", r.Results)
	d := dominant(r)
	if d == nil || d.Rule != "" || !strings.Contains(dominantNote(r, d), "driftgate accept group:1") {
		t.Errorf("content-type: %+v", d)
	}
	for i := range r.Results {
		r.Results[i].Drifts[0] = compare.Drift{Request: r.Results[i].ID, Kind: compare.KindValue, Where: "$.price", Baseline: "10", Current: "0", Texts: [2]string{"10", "0"}}
	}
	r = report.Build("capture:c", "baseline:local", r.Results)
	if d := dominant(r); d == nil || d.Rule != "" {
		t.Errorf("value: %+v", d)
	}
	// A server header with other values on other requests: no rule.
	for i := range r.Results {
		r.Results[i].Drifts[0] = compare.Drift{Request: r.Results[i].ID, Kind: compare.KindHeaderValue, Where: "header:server", Values: [2][]string{{"a"}, {fmt.Sprintf("b%d", i)}}}
	}
	r = report.Build("capture:c", "baseline:local", r.Results)
	if d := dominant(r); d == nil || d.Rule != "" {
		t.Errorf("server of several values: %+v", d)
	}
}

func dominantNote(r *report.Report, d *report.Dominant) string {
	r.Dominant = d
	return r.DominantNote()
}
