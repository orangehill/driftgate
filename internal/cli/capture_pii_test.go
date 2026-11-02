package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
)

// TestCapturePersonalDataInHeadersAndNumbers: requests marked public whose
// personal data is only in a header or in a JSON number are stored as
// private and reported, and the values reach no file in plain text.
func TestCapturePersonalDataInHeadersAndNumbers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/doc", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Petar Petrovic 0101990710008.pdf"`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Location", "/users/jane%40example.com")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":12,"jmbg":1505985710129,"created":1730000000000}`)
	})
	mux.HandleFunc("/api/news", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":12,"amount":1505985,"ts":1730000000000}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: pii\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n", srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
 {"id":"doc","method":"GET","path":"/api/doc","storage":"public"},
 {"id":"me","method":"GET","path":"/api/me","storage":"public"},
 {"id":"profile","method":"GET","path":"/api/profile","storage":"public"},
 {"id":"news","method":"GET","path":"/api/news","storage":"public"}]}`)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\n")
	store := filepath.Join(t.TempDir(), "store")
	t.Setenv("DRIFTGATE_STORE", store)
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))

	code, out, errOut := runCLI(t, "capture", "c1", "--config", cfg)
	if code != ExitConfig {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"3 responses marked public", "doc: jmbg at header:content-disposition",
		"me: email at header:content-location", "profile: jmbg at $.jmbg"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("missing %q in:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "news") {
		t.Errorf("a response without personal data reported:\n%s", errOut)
	}
	public := false
	for _, dir := range []string{root, store} {
		filepath.Walk(dir, func(path string, info os.FileInfo, _ error) error {
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, v := range []string{"0101990710008", "jane%40example", "1505985710129"} {
				if strings.Contains(string(raw), v) {
					t.Errorf("%s in plain text: %s", v, path)
				}
			}
			if strings.HasSuffix(path, filepath.Join("responses", "news.json")) {
				public = strings.Contains(string(raw), `"storage": "public"`)
			}
			return nil
		})
	}
	if !public {
		t.Error("news is not stored as public")
	}
}
