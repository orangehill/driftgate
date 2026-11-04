package capture

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestResendAfterRestore: after a restore that ended the application's
// database sessions, a GET or HEAD of the read-only phase answered with
// 5xx is sent once more, within the budget (the sessions ended) across
// requests, and the record says so. A POST, a repeat, a known writer, a
// request to a remote target, one past the budget and one after no such
// restore are never sent again; a 5xx that stays is kept.
func TestResendAfterRestore(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method+" "+r.URL.Path]++
		n := calls[r.Method+" "+r.URL.Path]
		mu.Unlock()
		switch {
		case r.URL.Path == "/always": // the application's own 500
			w.WriteHeader(500)
		case n == 1: // a pooled connection the restore ended: fails once
			w.WriteHeader(500)
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := &config.Project{Config: config.Config{Targets: map[string]config.Target{
		"app":    {BaseURL: srv.URL},
		"remote": {BaseURL: srv.URL},
	}}}
	ctx := context.Background()
	r := &runner{p: p, services: map[string]*service{}, log: io.Discard, tr: &tracker{}}
	for _, name := range []string{"app", "remote"} {
		s, err := newService(ctx, p, catalog.Request{ID: name, Target: name}, false)
		if err != nil {
			t.Fatal(err)
		}
		s.remote = name == "remote"
		r.services[name] = s
	}
	get := func(id, method, path, target string, repeat int) *snapshot.Record {
		t.Helper()
		req := catalog.Request{ID: id, Method: method, Path: path, Target: target, Repeat: repeat}
		res := r.resendAfterRestore(ctx, req, r.send(ctx, req))
		defer res.Release()
		return res.Record
	}
	sent := func(key string) int { mu.Lock(); defer mu.Unlock(); return calls[key] }

	// No restore ended sessions: nothing is sent again.
	if rec := get("cold", "GET", "/cold", "app", 0); rec.Status != 500 || rec.Resent != nil || sent("GET /cold") != 1 {
		t.Fatalf("without a restore: %d %+v, sent %d times", rec.Status, rec.Resent, sent("GET /cold"))
	}

	r.tr.resends.Store(3)
	r.tr.resentAfter = "the restore of db ended 3 database sessions of the application"
	if rec := get("a", "GET", "/a", "app", 0); rec.Status != 200 || rec.Resent == nil || rec.Resent.First != 500 ||
		!strings.Contains(rec.Resent.After, "ended 3 database sessions") || sent("GET /a") != 2 {
		t.Fatalf("GET: %d %+v, sent %d times", rec.Status, rec.Resent, sent("GET /a"))
	}
	if rec := get("h", "HEAD", "/h", "app", 0); rec.Status != 200 || rec.Resent == nil || sent("HEAD /h") != 2 {
		t.Fatalf("HEAD: %d %+v, sent %d times", rec.Status, rec.Resent, sent("HEAD /h"))
	}
	for _, c := range []struct {
		id, method, path, target string
		repeat                   int
	}{
		{"post", "POST", "/post", "app", 0},
		{"rep", "GET", "/rep", "app", 3},
		{"far", "GET", "/far", "remote", 0},
	} {
		if rec := get(c.id, c.method, c.path, c.target, c.repeat); rec.Status != 500 || rec.Resent != nil || sent(c.method+" "+c.path) != 1 {
			t.Errorf("%s: %d %+v, sent %d times", c.id, rec.Status, rec.Resent, sent(c.method+" "+c.path))
		}
	}
	// A GET the baseline knows writes is never sent twice.
	base := &snapshot.Set{Dir: t.TempDir()}
	if err := base.Write(&snapshot.Record{ID: "writes", Status: 200, Headers: [][2]string{}, Body: snapshot.Body{Storage: snapshot.Public},
		SideEffects: &snapshot.SideEffects{Items: []string{"db/table:dbo.views"}}}, nil); err != nil {
		t.Fatal(err)
	}
	r.base = base
	if rec := get("writes", "GET", "/writes", "app", 0); rec.Status != 500 || rec.Resent != nil || sent("GET /writes") != 1 {
		t.Fatalf("a known writer: %d %+v, sent %d times", rec.Status, rec.Resent, sent("GET /writes"))
	}
	// A 5xx that stays is the application's: kept, sent twice at most.
	if rec := get("always", "GET", "/always", "app", 0); rec.Status != 500 || rec.Resent == nil || sent("GET /always") != 2 {
		t.Fatalf("a 500 that stays: %d %+v, sent %d times", rec.Status, rec.Resent, sent("GET /always"))
	}
	// The budget (3) is spent: the next one is not sent again.
	if rec := get("late", "GET", "/late", "app", 0); rec.Status != 500 || rec.Resent != nil || sent("GET /late") != 1 {
		t.Fatalf("past the budget: %d %+v, sent %d times", rec.Status, rec.Resent, sent("GET /late"))
	}
}
