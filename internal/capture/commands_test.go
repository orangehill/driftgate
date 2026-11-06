package capture

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestCommandsWithDirAndStdin: prepare and a command step run in their
// dir with their stdin file (both relative to the project root) as
// standard input, without a shell; a stdin file that is missing stops the
// capture before anything runs.
func TestCommandsWithDirAndStdin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "input.txt"), []byte("from stdin; $HOME stays as written\n"), 0o644)
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	newProject := func(stdin string) *config.Project {
		p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
			Target:  config.Target{BaseURL: srv.URL},
			Prepare: []config.Command{{Run: []string{"tee", "prepared.txt"}, Dir: "sub", Stdin: stdin}}}, root)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "a", Method: "GET", Path: "/a", Storage: snapshot.Public},
		{ID: "step", Command: []string{"cat"}, Dir: "sub", Stdin: "input.txt", Output: true, Storage: snapshot.Public},
	}}
	p := newProject("input.txt")
	if _, err := Run(context.Background(), p, cat, Options{Tag: "c", AllowMutatingWithoutRestore: true}); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "sub", "prepared.txt")); err != nil || string(raw) != "from stdin; $HOME stays as written\n" {
		t.Fatalf("prepare: %q %v", raw, err)
	}
	set, _ := p.Capture("c")
	rec, _ := set.Record("step")
	body, err := set.Body(rec)
	if err != nil || rec.Exit == nil || *rec.Exit != 0 || string(body) != "from stdin; $HOME stays as written\n" {
		t.Fatalf("step: %+v %q %v", rec, body, err)
	}

	os.Remove(filepath.Join(root, "sub", "prepared.txt"))
	_, err = Run(context.Background(), newProject("missing.txt"), cat, Options{Tag: "d", AllowMutatingWithoutRestore: true})
	if !errors.Is(err, ErrEnv) || !strings.Contains(err.Error(), "prepare: tee prepared.txt: stdin") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing stdin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "prepared.txt")); err == nil {
		t.Fatal("prepare ran although its stdin file is missing")
	}
}
