package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareThatWritesWarns: capture warns when prepare changed the
// restored state, names what it changed and advises seed; the exit code
// stays.
func TestPrepareThatWritesWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf(`version: 1
project: prep
target:
  base_url: %s
state:
  files:
    restore: ["sh", "-c", "printf restored > data.txt"]
    fingerprint: ["cat", "data.txt"]
prepare:
  - ["sh", "-c", "date +%%s%%N >> data.txt"]
`, srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[{"id":"a","method":"GET","path":"/a","storage":"public"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	code, out, errOut := runCLI(t, "capture", "c", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "warning: the state changed between the restore and the end of prepare: files/files") ||
		!strings.Contains(out, "move it to seed:") {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
}
