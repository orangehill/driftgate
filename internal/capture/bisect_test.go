package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// findWritersHalving is the search before the baseline's writers were tried
// first: the whole list split in halves, each group sent one request at a
// time. The new search must find the same.
func (r *runner) findWritersHalving(ctx context.Context, reqs []catalog.Request, before states, changed map[string][]string) error {
	covered := map[string]bool{}
	run := func(group []catalog.Request) (map[string][]string, []snapshot.RowChange, error) {
		now, err := r.reset(ctx)
		if err != nil {
			return nil, nil, err
		}
		if d := flatten(diffStates(before, now)); len(d) > 0 {
			return nil, nil, envErr("prepare is not repeatable: %v", d)
		}
		var rowsBefore map[string]state.Rows
		if len(group) == 1 {
			if rowsBefore, err = r.tr.readRows(ctx); err != nil {
				return nil, nil, err
			}
		}
		for _, req := range group {
			r.send(ctx, req)
		}
		after, err := r.tr.fingerprint(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		d := diffStates(before, after)
		r.tr.markDirty(d)
		var rows []snapshot.RowChange
		if len(d) > 0 && rowsBefore != nil {
			rowsAfter, err := r.tr.readRows(ctx)
			if err != nil {
				return nil, nil, err
			}
			rows = rowChanges(rowsBefore, rowsAfter)
		}
		return d, rows, nil
	}
	var search func(group []catalog.Request) error
	search = func(group []catalog.Request) error {
		d, rows, err := run(group)
		if err != nil || len(d) == 0 {
			return err
		}
		if len(group) == 1 {
			items := flatten(d)
			for _, it := range items {
				covered[it] = true
			}
			r.sum.Writers[group[0].ID] = items
			return r.addSideEffects(group[0].ID, &snapshot.SideEffects{Items: items, Rows: rows})
		}
		mid := len(group) / 2
		if err := search(group[:mid]); err != nil {
			return err
		}
		return search(group[mid:])
	}
	var err error
	if len(reqs) == 1 {
		err = search(reqs)
	} else if err = search(reqs[:len(reqs)/2]); err == nil {
		err = search(reqs[len(reqs)/2:])
	}
	if err != nil {
		return err
	}
	for _, it := range flatten(changed) {
		if !covered[it] {
			r.sum.Unattributed = append(r.sum.Unattributed, it)
		}
	}
	return nil
}

// lab is an application whose GET requests may write: a request appends its
// ID to the file of a table, which a command component fingerprints. One
// request may arm a flag that makes another one write (an order effect).
type lab struct {
	dir  string
	hits atomic.Int64

	mu     sync.Mutex
	writes map[string]string // request → table it writes
	arms   string            // request that arms the flag
	armed  map[string]string // request → table it writes once armed
}

func (l *lab) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	l.hits.Add(1)
	id := strings.TrimPrefix(req.URL.Path, "/r/")
	l.mu.Lock()
	defer l.mu.Unlock()
	appendTo := func(table string) {
		f, err := os.OpenFile(filepath.Join(l.dir, table), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, id)
			f.Close()
		}
	}
	if t, ok := l.writes[id]; ok {
		appendTo(t)
	}
	if id == l.arms {
		appendTo("flag")
	}
	if t, ok := l.armed[id]; ok {
		if _, err := os.Stat(filepath.Join(l.dir, "flag")); err == nil {
			appendTo(t)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

func (l *lab) set(writes map[string]string, arms string, armed map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes, l.arms, l.armed = writes, arms, armed
}

type labRun struct {
	sum   *Summary
	side  map[string]string // request → its side effects as JSON
	extra int               // requests the search sent
}

func newLab(t *testing.T, n, concurrency int) (*lab, *config.Project, *catalog.Catalog) {
	t.Helper()
	l := &lab{dir: t.TempDir()}
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	comps := map[string]config.Component{}
	for _, table := range []string{"t1", "t2", "t3"} {
		comps[table] = config.Component{
			Restore:     []string{"sh", "-c", "rm -f " + l.dir + "/*"},
			Fingerprint: []string{"sh", "-c", "cat " + l.dir + "/" + table + " 2>/dev/null; true"},
		}
	}
	cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL, Concurrency: config.Concurrency(concurrency)}, State: comps}
	p, err := config.New(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1}
	for i := range n {
		id := fmt.Sprintf("r%03d", i)
		cat.Requests = append(cat.Requests, catalog.Request{ID: id, Method: "GET", Path: "/r/" + id, Storage: snapshot.Public})
	}
	return l, p, cat
}

func (l *lab) capture(t *testing.T, p *config.Project, cat *catalog.Catalog, tag string, base *snapshot.Set, halving bool) labRun {
	t.Helper()
	if halving {
		writerSearch = (*runner).findWritersHalving
		defer func() { writerSearch = (*runner).findWriters }()
	}
	l.hits.Store(0)
	sum, err := Run(context.Background(), p, cat, Options{Tag: tag, Baseline: base})
	if err != nil {
		t.Fatal(err)
	}
	set, err := p.Capture(tag)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := set.Records()
	if err != nil {
		t.Fatal(err)
	}
	side := map[string]string{}
	for _, rec := range recs {
		b, _ := json.Marshal(rec.SideEffects)
		side[rec.ID] = string(b)
	}
	return labRun{sum: sum, side: side, extra: int(l.hits.Load()) - len(cat.Requests)}
}

// TestFindWritersSameAsHalving runs the previous and the new search on the
// same scenarios: the same writers, side effects and unattributed items, and
// far fewer requests once a baseline names the writers.
func TestFindWritersSameAsHalving(t *testing.T) {
	const n = 550
	six := map[string]string{"r017": "t1", "r101": "t2", "r233": "t1", "r310": "t2", "r402": "t1", "r544": "t2"}
	eight := map[string]string{"r050": "t1", "r470": "t2"}
	for id, table := range six {
		eight[id] = table
	}
	five := map[string]string{}
	for id, table := range six {
		if id != "r310" {
			five[id] = table
		}
	}
	cases := []struct {
		name          string
		base          map[string]string // writers when the baseline was captured; nil: no baseline
		writes        map[string]string
		arms          string
		armed         map[string]string
		writers       int
		unattr        []string
		sent, sentNew int // requests the previous and the new search send; -1: fewer than the previous
	}{
		{name: "no writers", base: map[string]string{}, writes: map[string]string{}},
		{name: "first run", writes: six, writers: 6, sent: 2468, sentNew: 2088},
		{name: "baseline without writers", base: map[string]string{}, writes: six, writers: 6, sent: 2468, sentNew: 2088},
		// The known writers one by one, then the other 544 requests once.
		{name: "known writers only", base: six, writes: six, writers: 6, sent: 2468, sentNew: n},
		{name: "new writers", base: six, writes: eight, writers: 8, sent: 2679, sentNew: 1935},
		{name: "known writer stopped", base: six, writes: five, writers: 5, sent: 2335, sentNew: n},
		// The order of the parallel sends decides which groups change.
		{name: "order effect", base: six, writes: six, arms: "r020", armed: map[string]string{"r400": "t3"},
			writers: 6, unattr: []string{"t3/t3"}, sent: 2468, sentNew: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, p, cat := newLab(t, n, 4)
			var base *snapshot.Set
			if c.base != nil {
				l.set(c.base, "", nil)
				l.capture(t, p, cat, "base", nil, false)
				var err error
				if base, err = p.Capture("base"); err != nil {
					t.Fatal(err)
				}
			}
			l.set(c.writes, c.arms, c.armed)
			old := l.capture(t, p, cat, "old", base, true)
			cur := l.capture(t, p, cat, "new", base, false)
			if len(cur.sum.Writers) != c.writers || !reflect.DeepEqual(cur.sum.Writers, old.sum.Writers) {
				t.Errorf("writers: new %v, previous %v, want %d", cur.sum.Writers, old.sum.Writers, c.writers)
			}
			if !reflect.DeepEqual(cur.sum.Unattributed, old.sum.Unattributed) || !reflect.DeepEqual(cur.sum.Unattributed, c.unattr) {
				t.Errorf("unattributed: new %v, previous %v, want %v", cur.sum.Unattributed, old.sum.Unattributed, c.unattr)
			}
			if !reflect.DeepEqual(cur.side, old.side) {
				for id := range old.side {
					if cur.side[id] != old.side[id] {
						t.Errorf("%s side effects: new %s, previous %s", id, cur.side[id], old.side[id])
					}
				}
			}
			if old.extra != c.sent || (c.sentNew >= 0 && cur.extra != c.sentNew) || (c.sentNew < 0 && cur.extra >= old.extra) {
				t.Errorf("requests sent by the search: new %d, previous %d; want %d, %d", cur.extra, old.extra, c.sentNew, c.sent)
			}
			t.Logf("requests sent by the search: previous %d, new %d (read-only requests %d); fingerprints: previous %d, new %d",
				old.extra, cur.extra, n, old.sum.Time.Fingerprints, cur.sum.Time.Fingerprints)
		})
	}
}

// TestFindWritersUnrepeatableTokens: a tokens command that writes a new
// token on every run leaves the state after each restore other than the
// read-only phase started from. The search compares each run with the
// state it started from and still finds the request that wrote; a write
// that the other starting state hides stops the capture, naming both the
// change and what is not repeatable, instead of passing as an order effect.
func TestFindWritersUnrepeatableTokens(t *testing.T) {
	for _, c := range []struct {
		name    string
		writes  map[string]string
		armed   map[string]string // writes only while the flag the first tokens run makes exists
		writers map[string][]string
		err     string
	}{
		{name: "writer found", writes: map[string]string{"r007": "t1"}, writers: map[string][]string{"r007": {"t1/t1"}}},
		{name: "write hidden by the other start", armed: map[string]string{"r010": "t2"},
			err: "read-only requests changed [t2/t2], and no single one of them does it when sent again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, p, cat := newLab(t, 40, 1)
			ctr := filepath.Join(t.TempDir(), "counter")
			// Each run appends another token to t3; the first one also sets
			// the flag, which the restores remove.
			p.Tokens = config.Tokens{Command: []string{"sh", "-c", fmt.Sprintf(
				`n=$(cat %[1]s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %[1]s; echo token$n >> %[2]s/t3; [ $n = 1 ] && touch %[2]s/flag; echo '{}'`, ctr, l.dir)}}
			l.set(c.writes, "", c.armed)
			var out strings.Builder
			sum, err := Run(context.Background(), p, cat, Options{Tag: "c", Out: &out})
			if c.err != "" {
				if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), c.err) || !strings.Contains(err.Error(), "leave [t3/t3] different each time") {
					t.Fatalf("want an environment error %q, got %v", c.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			if !reflect.DeepEqual(sum.Writers, c.writers) || len(sum.Unattributed) > 0 {
				t.Errorf("writers %v, unattributed %v; want %v", sum.Writers, sum.Unattributed, c.writers)
			}
			if !strings.Contains(out.String(), "note: after a restore, prepare and the tokens command the state differs from the one before the read-only requests ([t3/t3])") {
				t.Errorf("no note:\n%s", out.String())
			}
		})
	}
}
