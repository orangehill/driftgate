package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// TestUploadWithoutMutatesIsSequential: a request that uploads a real file
// runs in the sequential phase even without mutates, so the responses after
// it until the next restore are private; a read-only request that would
// echo the file runs before it and sees nothing of it.
func TestUploadWithoutMutatesIsSequential(t *testing.T) {
	var mu sync.Mutex
	var stored string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/upload":
			raw, _ := io.ReadAll(r.Body)
			stored = string(raw)
			fmt.Fprint(w, `{"ok":true}`)
		case "/reset":
			stored = ""
		default:
			fmt.Fprintf(w, `{"stored":%q}`, stored)
		}
	}))
	t.Cleanup(srv.Close)
	root, uploads := t.TempDir(), t.TempDir()
	const content = "name\nMarko Markovic Uploaded\n"
	write(t, filepath.Join(uploads, "people.csv"), content)
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: up\ntarget:\n  base_url: %s\nuploads:\n  dir: %s\nstate:\n  app:\n    restore: [\"curl\", \"-sf\", \"%s/reset\"]\n", srv.URL, uploads, srv.URL))
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: time\n")
	write(t, filepath.Join(root, "driftgate", "catalog.json"), fmt.Sprintf(`{"version":1,"requests":[
 {"id":"upload","method":"POST","path":"/upload","storage":"private","multipart":{"files":[{"field":"file","path":"people.csv","ref":"%s"}]}},
 {"id":"echo","method":"GET","path":"/echo","storage":"public"},
 {"id":"echo-after","method":"GET","path":"/echo-after","storage":"public","mutates":true}]}`, secret.Key(k).Ref([]byte(content))))
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", cfg); code != ExitClean {
		t.Fatalf("capture: %d\n%s\n%s", code, out, errOut)
	}
	filepath.Walk(os.Getenv("DRIFTGATE_STORE"), func(path string, info os.FileInfo, _ error) error {
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "Uploaded") {
			t.Errorf("content of the uploaded file in plain text: %s", path)
		}
		return nil
	})
}
