//go:build unix

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// TestSetupInterrupted: a stop signal while setup captures (SIGTERM, as
// Ctrl-C) ends it with how to go on, not with the error of what was cut
// short.
func TestSetupInterrupted(t *testing.T) {
	api := demo.New("v1")
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			once.Do(func() { syscall.Kill(syscall.Getpid(), syscall.SIGTERM) })
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	setupDemo(t, srv.URL)
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != 128+int(syscall.SIGTERM) || !strings.Contains(errOut, "interrupted (terminated); run driftgate setup --yes: it goes on from here") ||
		strings.Contains(errOut, "context canceled") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
}
