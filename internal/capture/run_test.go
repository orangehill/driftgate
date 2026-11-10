package capture

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// TestRunRefusals: a capture that may not run is refused with its message
// and error kind, the first refusal winning. One refused before the store
// is touched never reaches capture.started and sends nothing; one refused
// later, once the previous capture is moved aside, puts it back.
func TestRunRefusals(t *testing.T) {
	get := catalog.Request{ID: "get", Method: "GET", Path: "/", Storage: snapshot.Public}
	post := catalog.Request{ID: "post", Method: "POST", Path: "/", Storage: snapshot.Public}
	upload := catalog.Request{ID: "upload", Method: "POST", Path: "/", Storage: snapshot.Public,
		Multipart: &catalog.Multipart{Files: []catalog.File{{Field: "f", Path: "a.txt"}}}}
	burst := catalog.Request{ID: "burst", Method: "GET", Path: "/", Repeat: 3, Storage: snapshot.Public}
	step := catalog.Request{ID: "step", Command: []string{"true"}, Dir: "missing", Storage: snapshot.Public}
	hint := `; post change data only by their method (the catalog does not say mutates): say mutates: false for those that only read (a search sent as POST), under requests: in driftgate.yaml, then driftgate generate --write (an entry written by hand: "mutates": false in the catalog)`
	for _, tc := range []struct {
		name    string
		reqs    []catalog.Request
		opt     Options
		prepare bool
		kind    error
		want    string // the whole message; with a trailing "…", its start
		late    bool   // refused once the store is taken (after capture.started)
	}{
		{"restore", []catalog.Request{get}, Options{Restore: "never"}, false, ErrConfig,
			"configuration: --restore must be once or each", false},
		{"read-only prepare", []catalog.Request{get}, Options{ReadOnly: true}, true, ErrConfig,
			"configuration: --read-only covers the requests only, not the project's commands…", false},
		{"read-only none", []catalog.Request{post}, Options{ReadOnly: true}, false, ErrConfig,
			"configuration: no catalog request that leaves data unchanged is selected (--read-only)", false},
		{"only none", []catalog.Request{get}, Options{Only: "nope"}, false, ErrConfig,
			"configuration: no catalog request matches --only nope", false},
		{"empty", nil, Options{}, false, ErrConfig,
			"configuration: the catalog has no requests", false},
		{"upload before state", []catalog.Request{get, post, upload}, Options{}, false, ErrConfig,
			"configuration: upload uploads a file from the uploads folder, which needs a state with a restore command to take it out of the application again (also without --allow-mutating-without-restore)", false},
		{"no state", []catalog.Request{get, post}, Options{AllowRemote: true}, false, ErrConfig,
			"configuration: the catalog has requests that change data (post), but no state with a restore command is configured: configure state: in driftgate.yaml (see driftgate init)" + hint, false},
		{"remote", []catalog.Request{get, post, burst}, Options{AllowRemote: true, AllowMutatingWithoutRestore: true}, false, ErrConfig,
			"configuration: refusing to send requests that change data or are repeated (post, burst) to a service outside allow_hosts without --allow-remote-mutations" + hint, false},
		{"command folder", []catalog.Request{get, step}, Options{AllowMutatingWithoutRestore: true}, false, ErrEnv,
			"environment: step: …", false},
		// Refused after the previous capture was moved aside.
		{"service", []catalog.Request{get, {ID: "elsewhere", Method: "GET", Path: "/", Target: "other", Storage: snapshot.Public}}, Options{}, false, ErrConfig,
			`configuration: elsewhere: target "other": the configuration has one target and no targets: section`, true},
		{"identity", []catalog.Request{get, {ID: "mine", Method: "GET", Path: "/", Identity: "admin", Storage: snapshot.Public}}, Options{}, false, ErrEnv,
			"environment: no token for identity admin", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sent := queueProject(t, 1)
			ctx := context.Background()
			if _, err := Run(ctx, p, &catalog.Catalog{Version: 1, Requests: []catalog.Request{get}}, Options{Tag: "run"}); err != nil {
				t.Fatal(err)
			}
			set, err := p.Capture("run")
			if err != nil {
				t.Fatal(err)
			}
			before, err := set.Meta()
			if err != nil {
				t.Fatal(err)
			}
			if tc.prepare {
				p.Prepare = []config.Command{{Run: []string{"true"}}}
			}
			opt := tc.opt
			opt.Tag, opt.Force = "run", true
			var points []string
			chaos.At = func(point string) { points = append(points, point) }
			t.Cleanup(func() { chaos.At = nil })
			requests := len(sent())
			_, err = Run(ctx, p, &catalog.Catalog{Version: 1, Requests: tc.reqs}, opt)
			chaos.At = nil
			if err == nil {
				t.Fatal("the capture ran")
			}
			if started := slices.Contains(points, "capture.started"); started != tc.late {
				t.Errorf("capture.started reached: %v, want %v (points %v)", started, tc.late, points)
			}
			if n := len(sent()) - requests; n > 0 {
				t.Errorf("the refused capture sent %d requests", n)
			}
			if !errors.Is(err, tc.kind) {
				t.Errorf("kind of %v: want %v", err, tc.kind)
			}
			if start, cut := strings.CutSuffix(tc.want, "…"); cut && !strings.HasPrefix(err.Error(), start) || !cut && err.Error() != tc.want {
				t.Errorf("message:\n%s\nwant:\n%s", err, tc.want)
			}
			Wait()
			after, err := set.Meta()
			if err != nil || after == nil || after.Incomplete || after.Created != before.Created {
				t.Fatalf("the previous capture after a refusal: %+v %v", after, err)
			}
			entries, _ := os.ReadDir(filepath.Dir(set.Dir))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), asidePrefix("run")) {
					t.Fatalf("left aside: %s", e.Name())
				}
			}
		})
	}
}

// TestRunBaselineRefusals: a capture whose start state differs from the one
// its baseline was captured from is refused with an environment error,
// once the previous capture is moved aside, which it puts back, and before
// any request: a baseline fingerprinted in an older format points to
// verify --migrate-state, items that are restore_only now to the changed
// configuration, and any other difference to the data; the command that
// makes the baseline again names a baseline of the repository's other than
// local. A baseline whose meta.json cannot be read is refused with the
// error of the read, as it is; with --skip-fingerprint-check the capture
// runs.
func TestRunBaselineRefusals(t *testing.T) {
	format := state.FingerprintFormat
	for _, tc := range []struct {
		name        string
		baseline    string // its name in the repository's baselines; "" is the capture base
		restoreOnly bool   // sessions is restore_only, not excluded, and only the baseline's state has it
		format      int    // of the baseline's state
		skip        bool   // --skip-fingerprint-check
		corrupt     bool   // the baseline's meta.json cannot be read
		want        string // <dir> is the baseline's folder; with a trailing "…", its start; "" is a capture that runs
	}{
		{"older format", "main", false, format - 1, false, false,
			"environment: the state differs from the one the baseline was captured from (1 items: [db/table:users]), and the baseline's fingerprint has an older format (the current format fingerprints these items another way): driftgate verify --baseline main --migrate-state checks that the data is the baseline's and records the fingerprint in the new format"},
		{"restore_only now", "main", true, format, false, false,
			"environment: the state differs from the one the baseline was captured from (1 items): [db/table:sessions]; they are restore_only now, which the baseline's state still holds: the configuration changed, so make the baseline again (move <dir> away; the next driftgate run --baseline main makes it)"},
		{"other data", "main", false, format, false, false,
			"environment: the state differs from the one the baseline was captured from (1 items): [db/table:users]; the pristine copy holds other data than the baseline's (a seed since, other seed data or migrations): seed the data the baseline was captured from, or make a new baseline of this data (move <dir> away; the next driftgate run --baseline main makes it); when the data changed on purpose, you can also capture with --skip-fingerprint-check and make that capture the baseline's content: driftgate baselines rebase main --from <tag> --reason \"…\""},
		{"other data, local", "local", false, format, false, false,
			"environment: the state differs from the one the baseline was captured from (1 items): [db/table:users]; the pristine copy holds other data than the baseline's (a seed since, other seed data or migrations): seed the data the baseline was captured from, or make a new baseline of this data (move <dir> away; the next driftgate run makes it); when the data changed on purpose, you can also capture with --skip-fingerprint-check and make that capture the baseline's content: driftgate baselines rebase local --from <tag> --reason \"…\""},
		{"other data, capture", "", false, format, false, false,
			"environment: the state differs from the one the baseline was captured from (1 items): [db/table:users]; the pristine copy holds other data than the baseline's (a seed since, other seed data or migrations): seed the data the baseline was captured from, or make a new baseline of this data (move <dir> away; the next driftgate run makes it); when the data changed on purpose, you can also capture with --skip-fingerprint-check and make that capture the baseline's content: driftgate baselines rebase base --from <tag> --reason \"…\""},
		{"skip fingerprint check", "main", false, format, true, false, ""},
		{"meta unreadable", "main", false, format, false, true, "<dir>/meta.json: invalid character…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			app := newSQLiteApp(t, filepath.Join(root, "app.sqlite"), "wal")
			srv := httptest.NewServer(app)
			t.Cleanup(srv.Close)
			c := config.Component{Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}
			if tc.restoreOnly {
				c.Exclude, c.RestoreOnly = nil, []string{"sessions"}
			}
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				Target: config.Target{BaseURL: srv.URL}, State: map[string]config.Component{"db": c}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := Seed(ctx, p, SeedOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "run", Only: "list"}); err != nil {
				t.Fatal(err)
			}
			before := readMeta(t, p, "run")
			// The baseline's state: the one this capture starts from, but
			// for one item.
			meta := snapshot.Meta{Tag: "base", Source: "capture", Created: before.Created, StateFormat: tc.format,
				State: map[string]map[string]string{"db": {}}}
			for k, v := range before.State["db"] {
				meta.State["db"][k] = v
			}
			if tc.restoreOnly {
				meta.State["db"]["table:sessions"] = "baseline"
			} else {
				meta.State["db"]["table:users"] = "baseline"
			}
			base, err := p.Capture("base")
			if tc.baseline != "" {
				base, err = p.Baseline(tc.baseline)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := base.WriteMeta(meta); err != nil {
				t.Fatal(err)
			}
			if tc.corrupt {
				if err := os.WriteFile(filepath.Join(base.Dir, "meta.json"), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			app.mu.Lock()
			clear(app.served)
			app.mu.Unlock()
			var points []string
			chaos.At = func(point string) { points = append(points, point) }
			t.Cleanup(func() { chaos.At = nil })
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "run", Only: "list", Force: true, Baseline: base, SkipFingerprintCheck: tc.skip})
			chaos.At = nil
			if tc.want == "" {
				if err != nil {
					t.Fatalf("capture with --skip-fingerprint-check: %v", err)
				}
				if meta := readMeta(t, p, "run"); meta.Incomplete || !meta.Unreliable {
					t.Fatalf("meta: %+v", meta)
				}
				return
			}
			if err == nil {
				t.Fatal("the capture ran")
			}
			if tc.corrupt {
				if errors.Is(err, ErrEnv) || errors.Is(err, ErrConfig) {
					t.Errorf("kind of %v: want the read's error as it is", err)
				}
			} else if !errors.Is(err, ErrEnv) {
				t.Errorf("kind of %v: want %v", err, ErrEnv)
			}
			want := strings.ReplaceAll(tc.want, "<dir>", base.Dir)
			if start, cut := strings.CutSuffix(want, "…"); cut && !strings.HasPrefix(err.Error(), start) || !cut && err.Error() != want {
				t.Errorf("message:\n%s\nwant:\n%s", err, want)
			}
			if !slices.Contains(points, "capture.started") {
				t.Errorf("capture.started not reached (points %v)", points)
			}
			app.mu.Lock()
			served := len(app.served)
			app.mu.Unlock()
			if served > 0 {
				t.Errorf("the refused capture sent %d requests", served)
			}
			Wait()
			set, err := p.Capture("run")
			if err != nil {
				t.Fatal(err)
			}
			after, err := set.Meta()
			if err != nil || after == nil || after.Incomplete || after.Created != before.Created {
				t.Fatalf("the previous capture after a refusal: %+v %v", after, err)
			}
			entries, _ := os.ReadDir(p.Layout.Captures)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), asidePrefix("run")) {
					t.Fatalf("left aside: %s", e.Name())
				}
			}
		})
	}
}

// TestRunMeta: the meta.json a capture writes last, with the services named
// as the configuration names them (the empty name of a request is the only
// service's), their origins and connect_to, and what a read-only capture
// left out of what --only selected, in name order; and, with a state, the
// fingerprint it started from.
func TestRunMeta(t *testing.T) {
	t.Run("services", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(srv.Close)
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		base := srv.URL
		p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			Targets: map[string]config.Target{"api": {BaseURL: base, ConnectTo: "127.0.0.1"}}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
			{ID: "list", Method: "GET", Path: "/", Storage: snapshot.Public},
			{ID: "post", Method: "POST", Path: "/", Storage: snapshot.Public},
			{ID: "save", Method: "PUT", Path: "/", Storage: snapshot.Public},
			{ID: "drop", Method: "DELETE", Path: "/", Storage: snapshot.Public},
			{ID: "kill", Method: "DELETE", Path: "/", Storage: snapshot.Public},
			{ID: "move", Method: "PATCH", Path: "/", Storage: snapshot.Public},
			{ID: "edit", Method: "PATCH", Path: "/", Storage: snapshot.Public},
			{ID: "add", Method: "POST", Path: "/", Storage: snapshot.Public},
		}}
		began := time.Now().UTC().Truncate(time.Second)
		sum, err := Run(context.Background(), p, cat, Options{Tag: "ro", ReadOnly: true, Only: "????", SkipFingerprintCheck: true})
		if err != nil {
			t.Fatal(err)
		}
		if sum.Requests != 1 || sum.Tracked {
			t.Fatalf("summary: %+v", sum)
		}
		meta := readMeta(t, p, "ro")
		origin, err := compare.Origin(base)
		if err != nil {
			t.Fatal(err)
		}
		if created, err := time.Parse(time.RFC3339, meta.Created); err != nil || created.Before(began) {
			t.Errorf("created %q: %v", meta.Created, err)
		}
		if meta.Tag != "ro" || meta.Source != "capture" || meta.Incomplete || !meta.Unreliable || !meta.ReadOnly ||
			!slices.Equal(meta.ReadOnlySkipped, []string{"drop", "edit", "kill", "move", "post", "save"}) ||
			len(meta.Origins) != 1 || meta.Origins["api"] != origin ||
			len(meta.ConnectTo) != 1 || meta.ConnectTo["api"] != "127.0.0.1" ||
			!slices.Equal(meta.Roots, projectRoots(p.Root)) || len(meta.RowTracking) != 0 ||
			meta.State != nil || meta.StateFormat != 0 {
			t.Errorf("meta: %+v", meta)
		}
	})
	t.Run("state", func(t *testing.T) {
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		root := t.TempDir()
		srv := httptest.NewServer(newSQLiteApp(t, filepath.Join(root, "app.sqlite"), "wal"))
		t.Cleanup(srv.Close)
		p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			Target: config.Target{BaseURL: srv.URL},
			State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "app.sqlite", Exclude: []string{"sessions"}}}}, root)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := Seed(ctx, p, SeedOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "all"}); err != nil {
			t.Fatal(err)
		}
		meta := readMeta(t, p, "all")
		if len(meta.State["db"]) == 0 || meta.StateFormat != state.FingerprintFormat || meta.Unreliable || meta.ReadOnly ||
			meta.ReadOnlySkipped != nil || len(meta.Origins) != 1 || meta.Origins[""] == "" || meta.ConnectTo != nil {
			t.Errorf("meta: %+v", meta)
		}
	})
}

func readMeta(t *testing.T, p *config.Project, tag string) *snapshot.Meta {
	t.Helper()
	set, err := p.Capture(tag)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := set.Meta()
	if err != nil || meta == nil {
		t.Fatalf("meta of %s: %v", tag, err)
	}
	return meta
}
