package capture

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// bigJSON is a JSON body of about n bytes with "id": 7 at its start.
func bigJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"id":7,"items":[`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"n":%d,"text":"lorem ipsum dolor sit amet %d"}`, i, i)
	}
	b.WriteString("]}")
	return b.String()
}

// queueProject is a project whose one target answers /big/… with a large
// body and anything else with a small one, and logs the paths it gets.
func queueProject(t *testing.T, concurrency int) (*config.Project, func() []string) {
	t.Helper()
	big := bigJSON(4 << 20)
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/big/") {
			fmt.Fprint(w, big)
			return
		}
		fmt.Fprintf(w, `{"path":%q}`, r.URL.RequestURI())
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL, Concurrency: config.Concurrency(concurrency)}}
	p, err := config.New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, paths...)
	}
}

// TestQueueKeepsCatalogOrder: with concurrency 1 the requests reach the
// server, and the progress lines come, in catalog order, although the
// records are written in the background (large bodies take longer).
func TestQueueKeepsCatalogOrder(t *testing.T) {
	p, paths := queueProject(t, 1)
	cat := &catalog.Catalog{Version: 1}
	var want []string
	for i := range 40 {
		path := fmt.Sprintf("/small/%d", i)
		if i%7 == 0 {
			path = fmt.Sprintf("/big/%d", i)
		}
		id := fmt.Sprintf("r%02d", i)
		cat.Requests = append(cat.Requests, catalog.Request{ID: id, Method: "GET", Path: path, Storage: snapshot.Public})
		want = append(want, id)
	}
	var out bytes.Buffer
	sum, err := Run(context.Background(), p, cat, Options{Tag: "t", Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != len(want) || len(sum.Failed) > 0 {
		t.Fatalf("summary %+v", sum)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "200" {
			got = append(got, f[1])
		}
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("progress lines out of catalog order:\n%s", out.String())
	}
	for i, path := range paths() {
		if want := cat.Requests[i].Path; path != want {
			t.Fatalf("request %d reached the server as %s, want %s", i, path, want)
		}
	}
}

// TestQueueParamsFromWaits: a read-only request whose parameter comes from
// the response just before it waits until that response is written, also
// when it is large and still being scanned for personal data.
func TestQueueParamsFromWaits(t *testing.T) {
	p, paths := queueProject(t, 1)
	cat := &catalog.Catalog{Version: 1}
	for i := range 5 {
		cat.Requests = append(cat.Requests,
			catalog.Request{ID: fmt.Sprintf("list%d", i), Method: "GET", Path: fmt.Sprintf("/big/%d", i), Storage: snapshot.Public},
			catalog.Request{ID: fmt.Sprintf("item%d", i), Method: "GET", Path: "/item/{id}", Storage: snapshot.Public,
				ParamsFrom: map[string]catalog.From{"id": {Request: fmt.Sprintf("list%d", i), Path: "$.id"}}})
	}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "t"}); err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture("t")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		rec, err := set.Record(fmt.Sprintf("item%d", i))
		if err != nil || rec == nil || rec.Error != "" || rec.Status != 200 {
			t.Fatalf("item%d: %+v %v", i, rec, err)
		}
	}
	n := 0
	for _, path := range paths() {
		if path == "/item/7" {
			n++
		}
	}
	if n != 5 {
		t.Fatalf("%d requests with the parameter from the earlier response: %v", n, paths())
	}
}

// TestQueueWriteError: a record that cannot be written stops the sending
// and its error is what the capture returns, with one worker or several.
func TestQueueWriteError(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				fmt.Fprint(w, `{"ok":true}`)
			}))
			t.Cleanup(srv.Close)
			p := &config.Project{Config: config.Config{Target: config.Target{BaseURL: srv.URL}}}
			var reqs []catalog.Request
			for i := range 200 {
				reqs = append(reqs, catalog.Request{ID: fmt.Sprintf("r%03d", i), Method: "GET", Path: "/x", Storage: snapshot.Public})
			}
			s, err := newService(context.Background(), p, reqs[0], false)
			if err != nil {
				t.Fatal(err)
			}
			detector, err := pii.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			// The set's folder is a file: no record can be written.
			dir := filepath.Join(t.TempDir(), "set")
			if err := os.WriteFile(dir, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			r := &runner{p: p, set: &snapshot.Set{Dir: dir}, services: map[string]*service{"": s}, detector: detector,
				out: &bytes.Buffer{}, log: &bytes.Buffer{}, timings: map[string]int64{}, sum: &Summary{Personal: map[string][]pii.Finding{}, Unscanned: map[string]string{}}}
			err = r.parallel(context.Background(), reqs, workers, newWriteQueue(r, workers, newBudget(queueBudget)))
			if err == nil || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("error %v", err)
			}
			if n := hits.Load(); n >= int64(len(reqs)) {
				t.Fatalf("all %d requests were sent after the first write failed", n)
			}
			if r.sum.Requests != 0 {
				t.Fatalf("%d records counted as written", r.sum.Requests)
			}
		})
	}
}

// TestQueueBudget: the bodies waiting to be written stay within the budget
// (a body larger than the whole budget goes alone).
func TestQueueBudget(t *testing.T) {
	b := newBudget(100)
	var inUse, peak atomic.Int64
	var wg sync.WaitGroup
	for _, n := range []int64{60, 30, 50, 500, 10, 90} {
		wg.Go(func() {
			b.acquire(n)
			w := min(n, 100)
			if now := inUse.Add(w); now > peak.Load() {
				peak.Store(now)
			}
			inUse.Add(-w)
			b.release(n)
		})
	}
	wg.Wait()
	if peak.Load() > 100 || b.free != 100 {
		t.Fatalf("peak %d, free %d", peak.Load(), b.free)
	}
}

// TestPreviousCaptureMovedAside: --force replaces a capture through a
// folder moved aside, which is gone when the capture ends, as is one an
// interrupted capture left; names that only look alike stay.
func TestPreviousCaptureMovedAside(t *testing.T) {
	p, _ := queueProject(t, 1)
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public}}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run"}); err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture("run")
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(set.Dir)
	stale := filepath.Join(set.Dir, "responses", "stale.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".run.old-123", ".run.old-x", "run.old-1", ".runner.old-5"} {
		if err := os.MkdirAll(filepath.Join(parent, name, "responses"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run"}); err == nil {
		t.Fatal("an existing capture was replaced without --force")
	}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run", Force: true}); err != nil {
		t.Fatal(err)
	}
	Wait()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the previous capture's record is still there: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, " "); got != ".run.old-x .runner.old-5 run run.old-1" {
		t.Fatalf("captures folder: %s", got)
	}
}

// TestFailedCaptureKeepsPrevious: a capture that fails (here: cancelled
// before it sends anything, as when the stack is down) puts the previous
// capture back, record for record; a complete one replaces it.
func TestFailedCaptureKeepsPrevious(t *testing.T) {
	p, _ := queueProject(t, 1)
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public}}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run"}); err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture("run")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(set.Dir, "responses", "marker.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := set.Meta()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, p, cat, Options{Tag: "run", Force: true}); err == nil {
		t.Fatal("a cancelled capture succeeded")
	}
	Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the previous capture was not put back: %v", err)
	}
	after, err := set.Meta()
	if err != nil || after.Incomplete || after.Created != before.Created {
		t.Fatalf("meta after a failed capture: %+v, %v", after, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(set.Dir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), asidePrefix("run")) {
			t.Fatalf("left aside: %s", e.Name())
		}
	}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run", Force: true}); err != nil {
		t.Fatal(err)
	}
	Wait()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a complete capture kept the previous one's record: %v", err)
	}
}

// Once a large body on the heap is written the garbage is collected at
// once, so the next large body does not come on top of it.
func TestCollectAfterLargeBody(t *testing.T) {
	gcs := func() uint32 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.NumGC
	}
	before := gcs()
	collect(httpx.Result{Body: make([]byte, collectAfter)}, collectAfter)
	if gcs() == before {
		t.Fatal("no collection after a large body")
	}
}

// Large chunked bodies are read off the heap (httpx.Options.OffHeap): every
// one is released once stored or dropped, in the queue, in the requests that
// change data and in a repeated request, and stored whole, public and
// private.
func TestOffHeapBodiesReleased(t *testing.T) {
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
	p, _ := queueProject(t, 3)
	cat := &catalog.Catalog{Version: 1}
	for i := range 12 {
		storage := snapshot.Public
		if i%2 == 1 {
			storage = snapshot.Private
		}
		cat.Requests = append(cat.Requests, catalog.Request{ID: fmt.Sprintf("r%02d", i), Method: "GET", Path: fmt.Sprintf("/big/%d", i), Storage: storage})
	}
	yes := true
	cat.Requests = append(cat.Requests,
		catalog.Request{ID: "w1", Method: "POST", Path: "/big/w1", Mutates: &yes, Storage: snapshot.Private},
		catalog.Request{ID: "w2", Method: "GET", Path: "/big/w2", Repeat: 3, Mutates: &yes, Storage: snapshot.Public})
	sum, err := Run(context.Background(), p, cat, Options{Tag: "t", AllowMutatingWithoutRestore: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != len(cat.Requests) || len(sum.Failed) > 0 {
		t.Fatalf("summary %+v", sum)
	}
	if n := httpx.Unreleased(); n != 0 {
		t.Fatalf("%d bodies read off the heap were not released", n)
	}
	set, err := p.Capture("t")
	if err != nil {
		t.Fatal(err)
	}
	want := bigJSON(4 << 20)
	for _, req := range cat.Requests {
		rec, err := set.Record(req.ID)
		if err != nil {
			t.Fatal(err)
		}
		body, err := set.Body(rec)
		if err != nil || string(body) != want {
			t.Fatalf("%s: %d bytes stored, %v", req.ID, len(body), err)
		}
	}
}

// TestKilledCaptureKeepsPrevious: a capture killed after it moved the
// previous one aside (SIGKILL, out of memory, a CI timeout) cannot put it
// back itself; the next capture of the tag does, before anything else, so
// the last complete capture is never lost, also when the next one fails.
func TestKilledCaptureKeepsPrevious(t *testing.T) {
	p, _ := queueProject(t, 1)
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public}}}
	if _, err := Run(context.Background(), p, cat, Options{Tag: "run"}); err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture("run")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(set.Dir, "responses", "marker.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// What a killed capture leaves: the previous capture aside (and an
	// older one), the new one incomplete.
	parent := filepath.Dir(set.Dir)
	if err := os.Rename(set.Dir, filepath.Join(parent, ".run.old-2000")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(parent, ".run.old-1000", "responses"), 0o755)
	if err := set.WriteMeta(snapshot.Meta{Tag: "run", Source: "capture", Incomplete: true}); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(set.Dir, "responses"), 0o755)

	// The next capture fails at once, as when the stack is down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, p, cat, Options{Tag: "run", Force: true}); err == nil {
		t.Fatal("a cancelled capture succeeded")
	}
	Wait()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the last complete capture is lost: %v", err)
	}
	if m, err := set.Meta(); err != nil || m.Incomplete {
		t.Fatalf("meta: %+v, %v", m, err)
	}
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), asidePrefix("run")) {
			t.Fatalf("left aside: %s", e.Name())
		}
	}
}
