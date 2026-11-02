package capture

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/upload"
)

// TestCaptureKeepsEncodedPersonalDataPrivate: through a capture, a public
// request that asks for gzip gets it alone (not next to identity), and its
// body with an e-mail address is stored private, as is a public image;
// a plain JSON body without personal data stays public.
func TestCaptureKeepsEncodedPersonalDataPrivate(t *testing.T) {
	png, _ := upload.Synthetic(upload.PNG)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gz":
			if r.Header.Get("Accept-Encoding") != "gzip" {
				http.Error(w, "Accept-Encoding "+r.Header.Get("Accept-Encoding"), 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			g := gzip.NewWriter(w)
			g.Write([]byte(`{"email":"marko@example.rs"}`))
			g.Close()
		case "/logo.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":1}`))
		}
	}))
	defer srv.Close()
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=")
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values", Target: config.Target{BaseURL: srv.URL}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "gz", Method: "GET", Path: "/gz", Storage: snapshot.Public, Headers: [][2]string{{"Accept-Encoding", "gzip"}}},
		{ID: "logo", Method: "GET", Path: "/logo.png", Storage: snapshot.Public},
		{ID: "plain", Method: "GET", Path: "/plain", Storage: snapshot.Public},
	}}
	sum, err := Run(context.Background(), p, cat, Options{Tag: "t"})
	if err != nil {
		t.Fatal(err)
	}
	set, _ := p.Capture("t")
	for id, want := range map[string]string{"gz": snapshot.Private, "logo": snapshot.Private, "plain": snapshot.Public} {
		rec, err := set.Record(id)
		if err != nil || rec.Status != 200 || rec.Body.Storage != want {
			t.Errorf("%s: %+v %v", id, rec, err)
		}
	}
	if len(sum.Personal["gz"]) == 0 || sum.Unscanned["logo"] == "" || len(sum.Personal) != 1 || len(sum.Unscanned) != 1 {
		t.Fatalf("personal %v, unscanned %v", sum.Personal, sum.Unscanned)
	}
}
