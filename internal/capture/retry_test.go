package capture

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// flakyServer answers every path but fails the first send of some: /flaky
// and /search (POST) and /write (POST) with 503, /gateway with 502, /reset
// by closing the connection, /slow by answering after the timeout; /down
// fails every time with 503, /broken with 500.
func flakyServer(t *testing.T) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		n := hits[r.URL.Path]
		mu.Unlock()
		switch p := r.URL.Path; {
		case p == "/broken":
			w.WriteHeader(http.StatusInternalServerError)
			return
		case p == "/down", n == 1 && (p == "/flaky" || p == "/search" || p == "/write"):
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case n == 1 && p == "/gateway":
			w.WriteHeader(http.StatusBadGateway)
			return
		case n == 1 && p == "/reset":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		case n == 1 && p == "/slow":
			time.Sleep(600 * time.Millisecond)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return hits[path]
	}
}

func retryCatalog() *catalog.Catalog {
	f := false
	return &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "flaky", Method: "GET", Path: "/flaky", Storage: snapshot.Public},
		{ID: "gateway", Method: "GET", Path: "/gateway", Storage: snapshot.Public},
		{ID: "reset", Method: "GET", Path: "/reset", Storage: snapshot.Public},
		{ID: "slow", Method: "GET", Path: "/slow", Storage: snapshot.Public},
		{ID: "down", Method: "GET", Path: "/down", Storage: snapshot.Public},
		{ID: "broken", Method: "GET", Path: "/broken", Storage: snapshot.Public},
		{ID: "search", Method: "POST", Path: "/search", Mutates: &f, Storage: snapshot.Public},
		{ID: "write", Method: "POST", Path: "/write", Storage: snapshot.Public},
	}}
}

// TestRetryOnLoadFailures: a read-only GET whose first send timed out,
// lost its connection or got a 502, 503 or 504 is sent once more, and its
// record keeps the first outcome. One that fails in every send is marked
// as not recovered, never as answered after a retry. A 500 (an exception
// of the application) is not retried, nor a request that is not GET, HEAD
// or OPTIONS, also with mutates: false.
func TestRetryOnLoadFailures(t *testing.T) {
	srv, hits := flakyServer(t)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL, Timeout: config.Duration{Duration: 300 * time.Millisecond}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	sum, err := Run(context.Background(), p, retryCatalog(), Options{Tag: "c", AllowMutatingWithoutRestore: true, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture("c")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, first string
		status    int
		recovered bool
	}{
		{"flaky", "503", 200, true},
		{"gateway", "502", 200, true},
		{"reset", "connection reset", 200, true},
		{"slow", "timeout", 200, true},
		{"down", "503", 503, false},
	} {
		rec, err := set.Record(tc.id)
		if err != nil || rec == nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if rec.Retry == nil || rec.Retry.First() != tc.first || rec.Retry.Sends != 2 || rec.Retry.Recovered != tc.recovered ||
			rec.Status != tc.status || rec.Error != "" {
			t.Fatalf("%s: %+v, retry %+v", tc.id, rec, rec.Retry)
		}
		if (sum.Retried[tc.id] != nil) != tc.recovered || hits("/"+tc.id) != 2 {
			t.Fatalf("%s: summary %v, %d sends", tc.id, sum.Retried, hits("/"+tc.id))
		}
		what := "after a retry"
		if !tc.recovered {
			what = "failed in all 2 sends"
		}
		if !strings.Contains(out.String(), tc.id+"  ") || !strings.Contains(out.String(), what+": the first send gave "+tc.first) {
			t.Fatalf("%s: progress %q", tc.id, out.String())
		}
	}
	for _, tc := range []struct {
		id     string
		status int
	}{{"broken", 500}, {"search", 503}, {"write", 503}} {
		rec, err := set.Record(tc.id)
		if err != nil || rec == nil || rec.Retry != nil || rec.Status != tc.status || hits("/"+tc.id) != 1 {
			t.Fatalf("%s was sent again: %d sends, %+v, %v", tc.id, hits("/"+tc.id), rec, err)
		}
	}
	if len(sum.Retried) != 4 {
		t.Fatalf("retried: %v", sum.Retried)
	}
}

// TestRetryZeroDisables: retry: 0 records the first send as it came.
func TestRetryZeroDisables(t *testing.T) {
	srv, hits := flakyServer(t)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	zero := 0
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL, Retry: &zero}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "flaky", Method: "GET", Path: "/flaky", Storage: snapshot.Public}}}
	sum, err := Run(context.Background(), p, cat, Options{Tag: "c"})
	if err != nil {
		t.Fatal(err)
	}
	set, _ := p.Capture("c")
	rec, err := set.Record("flaky")
	if err != nil || rec.Status != http.StatusServiceUnavailable || rec.Retry != nil || hits("/flaky") != 1 || len(sum.Retried) != 0 {
		t.Fatalf("%+v, %d sends, %v", rec, hits("/flaky"), err)
	}
}

// TestRetryDeferred: a load failure is sent again after the other
// read-only requests, one at a time.
func TestRetryDeferred(t *testing.T) {
	var mu sync.Mutex
	var order []string
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		n := hits[r.URL.Path]
		order = append(order, r.URL.Path)
		mu.Unlock()
		if n == 1 && (r.URL.Path == "/flaky" || r.URL.Path == "/list") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"id":7}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "flaky", Method: "GET", Path: "/flaky", Storage: snapshot.Public},
		{ID: "list", Method: "GET", Path: "/list", Storage: snapshot.Public},
		{ID: "last", Method: "GET", Path: "/last", Storage: snapshot.Public},
	}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "c"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, " "); got != "/flaky /list /last /flaky /list" {
		t.Fatalf("order of sends: %s", got)
	}
	set, _ := p.Capture("c")
	for _, id := range []string{"flaky", "list"} {
		if rec, _ := set.Record(id); rec.Status != 200 || !rec.Retry.Passed() {
			t.Fatalf("%s: %+v", id, rec)
		}
	}
}

// TestRetryNotSpentOnWhatCannotRecover: a request whose baseline response
// failed the same way is not sent again, and after timeoutBudget resends
// of a service timed out again its other timeouts are not either.
func TestRetryNotSpentOnWhatCannotRecover(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch {
		case r.URL.Path == "/down":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case strings.HasPrefix(r.URL.Path, "/hang"):
			time.Sleep(400 * time.Millisecond)
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL, Timeout: config.Duration{Duration: 100 * time.Millisecond}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "down", Method: "GET", Path: "/down", Storage: snapshot.Public}}}
	for i := range 5 {
		cat.Requests = append(cat.Requests, catalog.Request{ID: fmt.Sprintf("hang%d", i), Method: "GET", Path: fmt.Sprintf("/hang%d", i), Storage: snapshot.Public})
	}
	var out strings.Builder
	if _, err := Run(context.Background(), p, cat, Options{Tag: "first", Out: &out}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	resent := 0
	for i := range 5 {
		if hits[fmt.Sprintf("/hang%d", i)] == 2 {
			resent++
		}
	}
	if hits["/down"] != 2 || resent != timeoutBudget || !strings.Contains(out.String(), "4 timeouts not sent again: 1 resent alone timed out again") {
		t.Fatalf("down sent %d times, %d hanging requests sent again:\n%s", hits["/down"], resent, out.String())
	}

	// With the first capture as the baseline, nothing failed differently.
	base, _ := p.Capture("first")
	for k := range hits {
		delete(hits, k)
	}
	mu.Unlock()
	_, err = Run(context.Background(), p, cat, Options{Tag: "second", Baseline: base})
	mu.Lock()
	if err != nil {
		t.Fatal(err)
	}
	for path, n := range hits {
		if n != 1 {
			t.Fatalf("%s sent %d times, although its baseline failed the same way", path, n)
		}
	}
}

// TestRetryStatusBudget: an application that answers 503 to everything
// (maintenance mode) gets statusBudget resends, not one per request.
func TestRetryStatusBudget(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1}
	for i := range 20 {
		cat.Requests = append(cat.Requests, catalog.Request{ID: fmt.Sprintf("r%02d", i), Method: "GET", Path: fmt.Sprintf("/r/%d", i), Storage: snapshot.Public})
	}
	var out strings.Builder
	if _, err := Run(context.Background(), p, cat, Options{Tag: "c", Out: &out}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 20+statusBudget || !strings.Contains(out.String(), "18 502, 503 or 504 answers not sent again: 2 resent alone got one again") {
		t.Fatalf("%d sends:\n%s", hits, out.String())
	}
}

// TestRetryNotRecoveredByOtherFailure: a resend that fails in another way
// than load (the server is gone, an interrupt) did not recover.
func TestRetryNotRecoveredByOtherFailure(t *testing.T) {
	// Answers once, then is gone: the resend is refused. The listener is
	// closed before the answer, and the answer closes its connection, so
	// no resend can reach the server however soon it comes (closing the
	// server in the background after the answer raced the resend).
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Listener.Close()
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "x", Method: "GET", Path: "/x", Storage: snapshot.Public}}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "c"}); err != nil {
		t.Fatal(err)
	}
	set, _ := p.Capture("c")
	rec, _ := set.Record("x")
	if rec.Error == "" || rec.Retry == nil || rec.Retry.Recovered || rec.Retry.Status != 503 {
		t.Fatalf("a refused resend: %+v, retry %+v", rec, rec.Retry)
	}

	// An interrupt during the resend.
	block := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	t.Cleanup(func() { close(block); hang.Close() })
	q, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: hang.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := catalog.Request{ID: "x", Method: "GET", Path: "/x", Storage: snapshot.Public}
	s, err := newService(ctx, q, req, false)
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{services: map[string]*service{"": s}, retry: newRetrying(nil)}
	first := &snapshot.Retry{Status: 503}
	time.AfterFunc(100*time.Millisecond, cancel)
	res := r.resend(ctx, req, httpx.Result{Record: &snapshot.Record{ID: "x", Status: 503}}, first)
	if res.Record.Error == "" || first.Recovered {
		t.Fatalf("an interrupted resend: %+v, retry %+v", res.Record, first)
	}
}

// TestRetryNotForKnownWriters: a GET whose baseline response changed the
// state (a GET can write) is never sent twice, also after a 504.
func TestRetryNotForKnownWriters(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 2 { // the first send of the second capture
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values", Target: config.Target{BaseURL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "visit", Method: "GET", Path: "/visit", Storage: snapshot.Public}}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "first"}); err != nil {
		t.Fatal(err)
	}
	base, _ := p.Capture("first")
	rec, err := base.Record("visit")
	if err != nil || rec == nil {
		t.Fatal(err)
	}
	rec.SideEffects = &snapshot.SideEffects{Items: []string{"db/table:visits"}}
	if err := base.Write(rec, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "second", Baseline: base}); err != nil {
		t.Fatal(err)
	}
	cur, _ := p.Capture("second")
	got, err := cur.Record("visit")
	mu.Lock()
	defer mu.Unlock()
	if err != nil || got == nil || got.Status != http.StatusGatewayTimeout || got.Retry != nil || hits != 2 {
		t.Fatalf("a known writer: %+v %v, sent %d times in all", got, err, hits)
	}
}
