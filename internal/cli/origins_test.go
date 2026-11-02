package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// hostApp writes absolute URLs from the Host of the request, as Laravel does.
func hostApp(w http.ResponseWriter, r *http.Request) {
	origin := "http://" + r.Host
	switch r.URL.Path {
	case "/assets":
		w.Header().Set("Content-Type", "application/json")
		esc := strings.ReplaceAll(origin, "/", `\/`)
		fmt.Fprintf(w, `{"url":"%s/storage/assets/a.jpg","escaped":"%s\/storage\/b.jpg"}`, origin, esc)
	case "/me":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"email":"pera@example.com","avatar":"%s/storage/me.jpg"}`, origin)
	case "/page":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<html><body><a href="%s/news">news</a><img src="%s/i.png"></body></html>`, origin, origin)
	case "/old":
		http.Redirect(w, r, origin+"/new", http.StatusFound)
	}
}

const hostCatalog = `{"version":1,"requests":[
 {"id":"assets","method":"GET","path":"/assets","storage":"public"},
 {"id":"me","method":"GET","path":"/me","storage":"private"},
 {"id":"page","method":"GET","path":"/page","storage":"public"},
 {"id":"old","method":"GET","path":"/old","storage":"public"}]}`

// The same application captured at two addresses: no drift, the report says
// which origins were taken as the same, and nothing stored is changed.
func TestCompareOtherOrigin(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(hostApp))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(hostApp))
	defer second.Close()
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	configure := func(url, extra string) {
		write(t, cfg, "version: 1\nproject: hosts\nmode: freeze\nlevel: values\ntarget:\n  base_url: "+url+"\n"+extra)
	}
	write(t, filepath.Join(root, "driftgate", "catalog.json"), hostCatalog)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: time\n")
	configure(first.URL, "")
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", cfg); code != 0 {
		t.Fatalf("capture c1: %d\n%s\n%s", code, out, errOut)
	}
	configure(second.URL, "")
	if code, out, errOut := runCLI(t, "capture", "c2", "--config", cfg); code != 0 {
		t.Fatalf("capture c2: %d\n%s\n%s", code, out, errOut)
	}
	p, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := p.Capture("c1")
	meta, err := c1.Meta()
	if err != nil || meta.Origins[""] != first.URL {
		t.Fatalf("meta of c1: %+v %v", meta, err)
	}
	files := func() map[string]string {
		out := map[string]string{}
		for _, dir := range []string{root, os.Getenv("DRIFTGATE_STORE")} {
			filepath.WalkDir(dir, func(path string, d fs.DirEntry, _ error) error {
				if d != nil && !d.IsDir() && !strings.HasPrefix(d.Name(), "last-compare-") {
					raw, _ := os.ReadFile(path)
					out[path] = string(raw)
				}
				return nil
			})
		}
		return out
	}
	before := files()

	code, out, _ := runCLI(t, "compare", "c2", "--against", "c1", "--config", cfg)
	if code != ExitClean || !strings.Contains(out, "origins: "+first.URL+" ≙ "+second.URL) {
		t.Fatalf("compare: %d\n%s", code, out)
	}
	if after := files(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Error("the comparison changed stored files")
	}
	me, _ := c1.Record("me")
	body, err := c1.Body(me)
	if err != nil || me.Body.Storage != snapshot.Private || !bytes.Contains(body, []byte(first.URL+"/storage/me.jpg")) {
		t.Errorf("private body of c1: %s %v", body, err)
	}

	// A set that records no origin is not
	// compared with one that does: the error says how to capture it again.
	meta.Origins = nil
	if err := c1.WriteMeta(*meta); err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"c2 --against c1": "capture c1 records no origin: capture it again with driftgate capture c1 --force",
		"c1 --against c2": "capture c1 records no origin",
	} {
		code, out, errOut := runCLI(t, append(append([]string{"compare"}, strings.Fields(args)...), "--config", cfg)...)
		if code != ExitEnv || !strings.Contains(errOut, want) {
			t.Errorf("compare %s without an origin: %d\n%s\n%s", args, code, out, errOut)
		}
	}
	base, err := p.Baseline("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(base.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c1.Dir, base.Dir); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(base.Dir); err != nil {
		t.Fatal(err)
	}
	want := "baseline local records no origin: capture it again: remove " + base.Dir + " and run driftgate run --baseline local"
	if code, out, errOut := runCLI(t, "compare", "c2", "--config", cfg); code != ExitEnv || !strings.Contains(errOut, want) {
		t.Fatalf("baseline without an origin: %d\n%s\n%s", code, out, errOut)
	}
}

func TestOriginConfig(t *testing.T) {
	base := "version: 1\nproject: x\ntarget:\n  base_url: http://localhost:8081\n"
	for extra, want := range map[string]string{
		"  aliases: [\"http://app.test\", \"https://app.test:8443/\"]\n": "",
		"  aliases: [localhost:8080]\n":                                  "not an http or https URL",
		"  aliases: [HTTP://APP.TEST]\n":                                 "write it as http://app.test",
		// An unknown key is an error, not ignored.
		"  nosuch: x\n": "field nosuch not found",
	} {
		path := filepath.Join(t.TempDir(), "driftgate.yaml")
		write(t, path, base+extra)
		_, err := config.Load(path)
		if (err == nil) != (want == "") || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", extra, err)
		}
	}
}
