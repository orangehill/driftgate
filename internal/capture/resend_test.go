package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

// TestEndedCarriedOver: the budget the last restore of a capture leaves
// goes to the read-only phase of the next capture, once, and only to a
// capture of the same databases soon after: the local store is shared by
// every checkout of the project.
func TestEndedCarriedOver(t *testing.T) {
	db := endedDatabase{Component: "db", Working: "Shop", Server: serverHash("127.0.0.1:1433")}
	newTracker := func(n int64, ended ...endedDatabase) *tracker {
		tr := &tracker{databases: map[string]endedDatabase{"db": db}, endedBy: ended}
		tr.resends.Store(n)
		if n > 0 {
			tr.resentAfter = fmt.Sprintf("the restore of db ended %d database sessions of the application", n)
		}
		return tr
	}
	p := &config.Project{Layout: config.Layout{Pristine: t.TempDir()}}
	saveEnded(p, newTracker(3, db))
	next := newTracker(1, db) // its own first restore ended one
	takeEnded(p, next)
	if next.resends.Load() != 4 || !strings.Contains(next.resentAfter, "at the end of the previous capture") {
		t.Fatalf("%d %q", next.resends.Load(), next.resentAfter)
	}
	again := newTracker(0)
	takeEnded(p, again)
	if again.resends.Load() != 0 {
		t.Fatalf("taken twice: %d", again.resends.Load())
	}

	// A record without sessions is never written, and one written by hand
	// is not taken.
	saveEnded(p, again)
	if _, err := os.Stat(endedFile(p)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a record without sessions: %v", err)
	}
	write := func(e endedRecord) {
		if err := snapshot.WriteJSON(endedFile(p), e); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for name, e := range map[string]endedRecord{
		"no sessions":                              {Sessions: 0, Databases: []endedDatabase{db}, Saved: now},
		"another component":                        {Sessions: 3, Databases: []endedDatabase{{Component: "other", Working: "Shop", Server: db.Server}}, Saved: now},
		"another working db":                       {Sessions: 3, Databases: []endedDatabase{{Component: "db", Working: "Shop2", Server: db.Server}}, Saved: now},
		"another server":                           {Sessions: 3, Databases: []endedDatabase{{Component: "db", Working: "Shop", Server: serverHash("10.0.0.5:1433")}}, Saved: now},
		"older than endedFor":                      {Sessions: 3, Databases: []endedDatabase{db}, Saved: now.Add(-endedFor - time.Minute)},
		"saved in the future":                      {Sessions: 3, Databases: []endedDatabase{db}, Saved: now.Add(time.Hour)},
		"of an earlier release, without databases": {Sessions: 3, Saved: now},
	} {
		write(e)
		tr := newTracker(0)
		takeEnded(p, tr)
		if tr.resends.Load() != 0 {
			t.Errorf("%s: taken: %d", name, tr.resends.Load())
		}
		if _, err := os.Stat(endedFile(p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the record stays: %v", name, err)
		}
	}

	// A matching record is capped.
	write(endedRecord{Sessions: 1 << 40, Databases: []endedDatabase{db}, Saved: now})
	tr := newTracker(0)
	takeEnded(p, tr)
	if tr.resends.Load() != endedMax {
		t.Fatalf("not capped: %d", tr.resends.Load())
	}
}

// TestResentThenRetried: a request sent again after a restore that ended
// the application's sessions keeps that second answer, unless it is a
// load failure (a 503): then it is put aside for a retry as any other, and
// its record will keep both. A 500 that stays after the resend is the
// application's and is not retried.
func TestResentThenRetried(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		n := calls[r.URL.Path]
		mu.Unlock()
		switch {
		case n == 1 && r.URL.Path == "/a": // a pooled connection the restore ended
			w.WriteHeader(500)
		case n <= 2: // then the server is busy
			w.WriteHeader(503)
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := &config.Project{Config: config.Config{Targets: map[string]config.Target{"app": {BaseURL: srv.URL}}}}
	ctx := context.Background()
	reqs := []catalog.Request{{ID: "a", Method: "GET", Path: "/a", Target: "app"}, {ID: "b", Method: "GET", Path: "/b", Target: "app"}}
	r := &runner{p: p, services: map[string]*service{}, log: io.Discard, tr: &tracker{}, retry: newRetrying(reqs)}
	s, err := newService(ctx, p, reqs[0], false)
	if err != nil || s.retries != 1 {
		t.Fatalf("%v, retries %d", err, s.retries)
	}
	r.services["app"] = s
	r.tr.resends.Store(1)
	r.tr.resentAfter = "the restore of db ended 1 database session of the application"

	_, ok := r.sendRead(ctx, reqs[0])
	if ok || len(r.retry.deferred) != 1 {
		t.Fatalf("resent, then a 503: %v, deferred %d", ok, len(r.retry.deferred))
	}
	if d := r.retry.deferred[0]; d.first.Record.Resent == nil || d.first.Record.Resent.First != 500 || d.retry.Status != 503 {
		t.Fatalf("deferred: %+v %+v", d.first.Record, d.retry)
	}
	// The retry recovers; the record keeps the resend too.
	d := r.retry.deferred[0]
	r.retry.deferred = nil
	res := r.resend(ctx, d.req, d.first, d.retry)
	if rec := res.Record; rec.Status != 200 || rec.Resent == nil || rec.Resent.First != 500 || !rec.Retry.Passed() || rec.Retry.Status != 503 {
		t.Fatalf("retried: %+v, resent %+v, retry %+v", rec, rec.Resent, rec.Retry)
	}
	res.Release()
	// The budget is spent: b's 503 is a load failure, put aside.
	if _, ok := r.sendRead(ctx, reqs[1]); ok || len(r.retry.deferred) != 1 {
		t.Fatalf("b: %v, deferred %d", ok, len(r.retry.deferred))
	}
	r.retry.release()
	mu.Lock()
	defer mu.Unlock()
	if calls["/a"] != 3 || calls["/b"] != 1 {
		t.Errorf("sent %v", calls)
	}
}
