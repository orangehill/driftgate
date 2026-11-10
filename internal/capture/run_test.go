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
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// TestRunRefusals: a capture that may not run is refused with its message
// and error kind, the first refusal winning, before anything is touched (a
// refusal before the capture folder is taken leaves the previous capture
// where it was); one refused later, once the previous capture is moved
// aside, puts it back.
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
	}{
		{"restore", []catalog.Request{get}, Options{Restore: "never"}, false, ErrConfig,
			"configuration: --restore must be once or each"},
		{"read-only prepare", []catalog.Request{get}, Options{ReadOnly: true}, true, ErrConfig,
			"configuration: --read-only covers the requests only, not the project's commands…"},
		{"read-only none", []catalog.Request{post}, Options{ReadOnly: true}, false, ErrConfig,
			"configuration: no catalog request that leaves data unchanged is selected (--read-only)"},
		{"only none", []catalog.Request{get}, Options{Only: "nope"}, false, ErrConfig,
			"configuration: no catalog request matches --only nope"},
		{"empty", nil, Options{}, false, ErrConfig,
			"configuration: the catalog has no requests"},
		{"upload before state", []catalog.Request{get, post, upload}, Options{}, false, ErrConfig,
			"configuration: upload uploads a file from the uploads folder, which needs a state with a restore command to take it out of the application again (also without --allow-mutating-without-restore)"},
		{"no state", []catalog.Request{get, post}, Options{AllowRemote: true}, false, ErrConfig,
			"configuration: the catalog has requests that change data (post), but no state with a restore command is configured: configure state: in driftgate.yaml (see driftgate init)" + hint},
		{"remote", []catalog.Request{get, post, burst}, Options{AllowRemote: true, AllowMutatingWithoutRestore: true}, false, ErrConfig,
			"configuration: refusing to send requests that change data or are repeated (post, burst) to a service outside allow_hosts without --allow-remote-mutations" + hint},
		{"command folder", []catalog.Request{get, step}, Options{AllowMutatingWithoutRestore: true}, false, ErrEnv,
			"environment: step: …"},
		// Refused after the previous capture was moved aside.
		{"service", []catalog.Request{get, {ID: "elsewhere", Method: "GET", Path: "/", Target: "other", Storage: snapshot.Public}}, Options{}, false, ErrConfig,
			`configuration: elsewhere: target "other": the configuration has one target and no targets: section`},
		{"identity", []catalog.Request{get, {ID: "mine", Method: "GET", Path: "/", Identity: "admin", Storage: snapshot.Public}}, Options{}, false, ErrEnv,
			"environment: no token for identity admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := queueProject(t, 1)
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
			_, err = Run(ctx, p, &catalog.Catalog{Version: 1, Requests: tc.reqs}, opt)
			if err == nil {
				t.Fatal("the capture ran")
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
			{ID: "drop", Method: "DELETE", Path: "/", Storage: snapshot.Public},
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
			!slices.Equal(meta.ReadOnlySkipped, []string{"drop", "post"}) ||
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
