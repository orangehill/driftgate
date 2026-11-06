package capture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestPrepareThatWrites: a prepare that changes the restored state is
// named with what it changed, from the fingerprints the capture takes
// anyway; a tokens command that writes (a login) is not taken for it: with
// both, one fingerprint between them tells them apart.
func TestPrepareThatWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public}}}
	const (
		writes = "printf ' stamped' >> data.txt"
		login  = "printf ' token' >> tokens.txt; echo '{}'"
	)
	run := func(prepare, tokens string) *Summary {
		t.Helper()
		t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		cfg := config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			Target: config.Target{BaseURL: srv.URL},
			State: map[string]config.Component{
				"files":  {Restore: []string{"sh", "-c", "printf restored > data.txt"}, Fingerprint: []string{"cat", "data.txt"}},
				"tokens": {Restore: []string{"sh", "-c", "printf none > tokens.txt"}, Fingerprint: []string{"cat", "tokens.txt"}},
			}}
		if prepare != "" {
			cfg.Prepare = []config.Command{{Run: []string{"sh", "-c", prepare}}}
		}
		if tokens != "" {
			cfg.Tokens.Command = []string{"sh", "-c", tokens}
		}
		p, err := config.New(cfg, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		sum, err := Run(context.Background(), p, cat, Options{Tag: "c"})
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	wrote := run(writes, "")
	clean := run("true", "")
	if strings.Join(wrote.Prepared, ",") != "files/files" || len(clean.Prepared) != 0 {
		t.Fatalf("prepared %v; by a prepare that writes nothing %v", wrote.Prepared, clean.Prepared)
	}
	if wrote.Time.Fingerprints != clean.Time.Fingerprints {
		t.Fatalf("fingerprints: %d with a prepare that writes, %d without", wrote.Time.Fingerprints, clean.Time.Fingerprints)
	}
	if only := run("", login); len(only.Prepared) != 0 {
		t.Fatalf("a tokens command alone: %v", only.Prepared)
	}
	if both := run("true", login); len(both.Prepared) != 0 || both.Time.Fingerprints != clean.Time.Fingerprints+1 {
		t.Fatalf("a tokens command that writes after a prepare that does not: %v, %d fingerprints", both.Prepared, both.Time.Fingerprints)
	}
	if both := run(writes, login); strings.Join(both.Prepared, ",") != "files/files" {
		t.Fatalf("both write: %v", both.Prepared)
	}
}

// TestPrepareThatWritesLater: a prepare that writes only after a later
// restore (before the requests that change data) is named too.
func TestPrepareThatWritesLater(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State:  map[string]config.Component{"files": {Restore: []string{"sh", "-c", "printf restored > data.txt"}, Fingerprint: []string{"cat", "data.txt"}}},
		// The second run of prepare (after the second restore) writes.
		Prepare: []config.Command{{Run: []string{"sh", "-c", "if [ -f ran ]; then printf ' later' >> data.txt; fi; touch ran"}}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public},
		{ID: "w", Method: "POST", Path: "/w", Storage: snapshot.Public},
	}}
	sum, err := Run(context.Background(), p, cat, Options{Tag: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sum.Prepared, ",") != "files/files" {
		t.Fatalf("prepared %v", sum.Prepared)
	}
}
