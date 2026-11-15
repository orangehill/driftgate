package capture

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// touchApp updates one row per request, POST /<table>/<id>: v = v + 1,
// on an SQLite file with the tables t1 and t2, rows 1 to 3 each. It notes,
// in order, when each request arrived and when its response was written.
type touchApp struct {
	db            *sql.DB
	mu            sync.Mutex
	arrived, done []time.Time
}

func newTouchApp(t *testing.T, file string) *touchApp {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteTestURI(file, "_pragma=busy_timeout(10000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, tb := range []string{"t1", "t2"} {
		for _, s := range []string{"CREATE TABLE " + tb + " (id INTEGER PRIMARY KEY, v INTEGER NOT NULL)", "INSERT INTO " + tb + " (id, v) VALUES (1, 0), (2, 0), (3, 0)"} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &touchApp{db: db}
}

func (a *touchApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.arrived = append(a.arrived, time.Now())
	table, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if _, err := a.db.Exec("UPDATE "+table+" SET v = v + 1 WHERE id = ?", id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{}`)
	a.done = append(a.done, time.Now())
}

// TestWriteGapExpected: against a baseline, a request waits only for the
// gap after the last change of what its baseline record changed: the rows
// of a tracked table, else the table; so does the second run of a repeated
// capture (Expect). Without either, every write waits.
func TestWriteGapExpected(t *testing.T) {
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
	defer func(d time.Duration) { WriteGap = d }(WriteGap)
	WriteGap = 300 * time.Millisecond
	for _, rows := range []bool{true, false} {
		t.Run(fmt.Sprintf("rows %v", rows), func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app := newTouchApp(t, filepath.Join(root, "app.sqlite"))
			srv := httptest.NewServer(app)
			t.Cleanup(srv.Close)
			sc := config.Component{Driver: config.SQLite, Working: "app.sqlite"}
			if rows {
				sc.RowTracking = config.Tables("t1", "t2")
			}
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				Target: config.Target{BaseURL: srv.URL}, State: map[string]config.Component{"db": sc}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var out bytes.Buffer
			if err := Seed(ctx, p, SeedOptions{Out: &out}); err != nil {
				t.Fatal(err)
			}
			yes := true
			req := func(id, path string) catalog.Request {
				return catalog.Request{ID: id, Method: "POST", Path: path, Mutates: &yes, Storage: snapshot.Public}
			}
			cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
				req("a", "/t1/1"), req("b", "/t2/1"), req("c", "/t1/2"), req("d", "/t1/1")}}
			// after is how long after the response of request i request j came.
			after := func(i, j int) time.Duration {
				app.mu.Lock()
				defer app.mu.Unlock()
				return app.arrived[j].Sub(app.done[i])
			}
			const a, b, c, d = 0, 1, 2, 3
			if _, err := Run(ctx, p, cat, Options{Tag: "base", Out: &out}); err != nil {
				t.Fatalf("baseline: %v\n%s", err, out.String())
			}
			if w := after(a, b); w < WriteGap {
				t.Errorf("without a baseline, b came %v after a, want %v or more", w, WriteGap)
			}
			base, err := p.Capture("base")
			if err != nil {
				t.Fatal(err)
			}
			// Against the baseline, and in the second run of a repeated
			// capture, which expects what the first run changed.
			for _, opt := range []Options{{Tag: "run", Baseline: base}, {Tag: "repeat", Expect: base}} {
				app.mu.Lock()
				app.arrived, app.done = nil, nil
				app.mu.Unlock()
				opt.Out = &out
				if _, err := Run(ctx, p, cat, opt); err != nil {
					t.Fatalf("%s: %v\n%s", opt.Tag, err, out.String())
				}
				if w := after(a, b); w >= WriteGap/2 {
					t.Errorf("%s: b, another table, waited %v", opt.Tag, w)
				}
				if w := after(b, c); rows && w >= WriteGap/2 || !rows && w < WriteGap/2 {
					t.Errorf("%s: c, another row of t1 (rows tracked: %v), came %v after b", opt.Tag, rows, w)
				}
				if w := after(a, d); w < WriteGap {
					t.Errorf("%s: d, the row of a, came %v after a, want %v or more", opt.Tag, w, WriteGap)
				}
			}
		})
	}
}
