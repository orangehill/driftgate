//go:build unix

package e2e

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// project is a checkout of a project on the demo API, with its local store.
type project struct {
	root, store string
	url         string // the API
	env         []string
	api         *demo.Server // the demo API, when the project made it
}

const allowDate = `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
`

// readOnly is a catalog of n reading requests of the demo API, public ones
// and a private one whose body holds personal data.
func readOnly(n int) string {
	var reqs []string
	for i := range n {
		reqs = append(reqs, fmt.Sprintf(`{"id":"news-%02d","method":"GET","path":"/api/news","storage":"public"}`, i))
	}
	reqs = append(reqs, `{"id":"contact","method":"GET","path":"/api/contact","storage":"private"}`)
	return `{"version":1,"requests":[` + strings.Join(reqs, ",\n") + `]}`
}

// newProject writes a project for the API at url into a new folder, with
// extra configuration (state:, guards:) and a store of its own unless
// store is given (two checkouts sharing one).
func newProject(t *testing.T, url, extra, catalog, store string) *project {
	t.Helper()
	root := t.TempDir()
	if store == "" {
		store = filepath.Join(t.TempDir(), "store")
	}
	write(t, filepath.Join(root, "driftgate.yaml"), fmt.Sprintf("version: 1\nproject: e2e\nmode: evolve\ntarget:\n  base_url: %s\n%s", url, extra))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), catalog)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), allowDate)
	return &project{root: root, store: store, url: url, env: []string{"DRIFTGATE_STORE=" + store}}
}

// memoryProject is a project of reading requests on the demo API with its
// data in memory.
func memoryProject(t *testing.T) *project {
	t.Helper()
	api := demo.New("v1")
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	p := newProject(t, srv.URL, "", readOnly(8), "")
	p.api = api
	return p
}

// run runs driftgate in the project; crash names the point to stop it at.
func (p *project) run(t *testing.T, crash string, args ...string) result {
	t.Helper()
	env := p.env
	if crash != "" {
		env = append(append([]string{}, env...), "DG_E2E_CRASH="+crash)
	}
	return dg(t, p.root, env, args...)
}

// must runs driftgate and fails the test on another exit code than want.
func (p *project) must(t *testing.T, want int, args ...string) result {
	t.Helper()
	r := p.run(t, "", args...)
	if r.code != want {
		t.Fatalf("driftgate %s: want exit %d, got %v", strings.Join(args, " "), want, r)
	}
	return r
}

func (p *project) capture(tag string) string { return filepath.Join(p.store, "captures", tag) }

func (p *project) baseline(name string) string {
	return filepath.Join(p.root, "driftgate", "baselines", name)
}

// clean fails the test when an interrupted write left something behind.
func (p *project) clean(t *testing.T) {
	t.Helper()
	if left := leftovers(t, p.store, filepath.Join(p.root, "driftgate")); len(left) > 0 {
		t.Errorf("left behind: %v", left)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
