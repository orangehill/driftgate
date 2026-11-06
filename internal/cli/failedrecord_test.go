package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// TestCompareFailedRequest: a request that failed at the transport (no
// status, no body) on either side is a drift of its status, public or
// private, never a comparison that stops.
func TestCompareFailedRequest(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: failed\ntarget:\n  base_url: %s\n  retry: 0\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
 {"id":"pub","method":"GET","path":"/pub","storage":"public"},
 {"id":"priv","method":"GET","path":"/priv","storage":"private"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	if code, out, errOut := runCLI(t, "capture", "good", "--config", cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	up = false
	if code, out, errOut := runCLI(t, "capture", "bad", "--config", cfg); code != ExitEnv {
		t.Fatalf("capture of a server that drops connections: %d\n%s\n%s", code, out, errOut)
	}
	for _, pair := range [][2]string{{"bad", "good"}, {"good", "bad"}} {
		code, out, errOut := runCLI(t, "compare", pair[0], "--against", pair[1], "--config", cfg)
		if code != ExitDrift || !strings.Contains(out, "DRIFT  pub") || !strings.Contains(out, "DRIFT  priv") || !strings.Contains(out, "status") {
			t.Fatalf("compare %s against %s: %d\n%s\n%s", pair[0], pair[1], code, out, errOut)
		}
	}
}
