package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestRetriedPassNeverPlain: a read-only request whose first send fails
// under load passes after a retry, but run says so when it makes the
// baseline from it, and every later comparison and report shows it as
// passed after a retry, with the first status. A request that changes
// data is never sent twice.
func TestRetriedPassNeverPlain(t *testing.T) {
	var newsHits, postHits atomic.Int64
	srv := httptest.NewServer(noDate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/news":
			// Every capture's first send fails: odd hits get 503.
			if newsHits.Add(1)%2 == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case "/api/vote":
			postHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: retry\ntarget:\n  base_url: %s\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
 {"id":"news","method":"GET","path":"/api/news","storage":"public"},
 {"id":"faq","method":"GET","path":"/api/faq","storage":"public"},
 {"id":"vote","method":"POST","path":"/api/vote","mutates":true,"storage":"public"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	code, out, errOut := runCLI(t, "run", "--config", cfg, "--allow-mutating-without-restore")
	if code != ExitClean || !strings.Contains(out, "1 request was answered only after a retry") ||
		!strings.Contains(out, "news: first 503 after") ||
		!strings.Contains(out, "warning: baseline local holds 1 response that came only after a retry: news") {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	if postHits.Load() != 2 { // once per capture
		t.Fatalf("the request that changes data was sent %d times in two captures", postHits.Load())
	}

	code, out, errOut = runCLI(t, "run", "--config", cfg, "--allow-mutating-without-restore")
	for _, want := range []string{
		// The server fails the first send of every capture: in the
		// baseline and now, which is said louder.
		"RETRY  news (passed after a retry: the first send gave 503; it needed a retry in the baseline too (first 503): not load, check it)",
		"1 request passed only after a retry",
		"the baseline's response of 1 request came only after a retry: news",
		"No drift ✓ (1 passed only after a retry, 1 of them in the baseline too: check them)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run lacks %q", want)
		}
	}
	if code != ExitClean {
		t.Fatalf("second run: %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ = runCLI(t, "review", "--config", cfg); code != ExitClean ||
		!strings.Contains(out, "sent again after a load failure") ||
		!strings.Contains(out, "news                     after a retry: the first send gave 503; it needed a retry in the baseline too") {
		t.Fatalf("review: %d\n%s", code, out)
	}
	if code, out, _ = runCLI(t, "report", "--config", cfg, "--format", "json"); code != ExitClean ||
		!strings.Contains(out, `"passed_after_retry": [`) || !strings.Contains(out, `"retry": {`) {
		t.Fatalf("json report: %d\n%s", code, out)
	}
	if code, out, _ = runCLI(t, "report", "--config", cfg, "--format", "markdown"); code != ExitClean ||
		!strings.Contains(out, "no drift in 3 requests (1 passed only after a retry, 1 of them in the baseline too") {
		t.Fatalf("markdown report: %d\n%s", code, out)
	}
}

// TestRetriedNoteCapped: the capture names at most ten requests answered
// after a retry, and how many more there are.
func TestRetriedNoteCapped(t *testing.T) {
	sum := &capture.Summary{Retried: map[string]*snapshot.Retry{}}
	for i := range 12 {
		sum.Retried[fmt.Sprintf("r%02d", i)] = &snapshot.Retry{Sends: 2, Status: 503, Recovered: true}
	}
	var out strings.Builder
	retriedNote(&out, sum)
	if !strings.Contains(out.String(), "12 requests were answered only after a retry") || !strings.Contains(out.String(), "  r09: first 503") ||
		strings.Contains(out.String(), "r10:") || !strings.Contains(out.String(), "  and 2 more\n") {
		t.Fatalf("%s", out.String())
	}
}

// TestObservedKeepsRetry: an observed request (measured, never compared)
// keeps its retry in the result.
func TestObservedKeepsRetry(t *testing.T) {
	dir := t.TempDir()
	base, cur := &snapshot.Set{Dir: filepath.Join(dir, "b")}, &snapshot.Set{Dir: filepath.Join(dir, "c")}
	rec := func(rt *snapshot.Retry) *snapshot.Record {
		return &snapshot.Record{ID: "x", Status: 200, Headers: [][2]string{}, Body: snapshot.Body{Storage: snapshot.Public}, Retry: rt}
	}
	if err := base.Write(rec(nil), []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := cur.Write(rec(&snapshot.Retry{Sends: 2, Status: 503, Recovered: true}), []byte("a")); err != nil {
		t.Fatal(err)
	}
	res, err := compareOne(catalog.Request{ID: "x", Observe: true}, base, cur, nil, &allowlist.List{}, compare.LevelValues, false)
	if err != nil || res.Outcome != report.Observed || !res.Retry.Passed() {
		t.Fatalf("%+v %v", res, err)
	}
}
