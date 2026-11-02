//go:build unix

package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/demo"
)

// TestEndToEnd is the whole workflow on the demo API, as a user runs it:
// its data in MySQL/MariaDB, an audit log in PostgreSQL (two state
// components), seed, the first run that makes the baseline, runs on the
// same code, planted drift found by run and compare, accepted, and the
// baseline verified. DRIFTGATE_E2E=1 with both databases runs it (tools/e2e/docker.sh).
func TestEndToEnd(t *testing.T) {
	if os.Getenv("DRIFTGATE_E2E") == "" {
		t.Skip("DRIFTGATE_E2E=1 runs the end-to-end test")
	}
	myDSN, my := mysqlDatabase(t, "e2e")
	pgDSN, pg := postgresDatabase(t, "e2e")
	// The API: the demo, and an audit log in PostgreSQL written by a POST.
	api := demo.NewSQL("v1", my)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/audit" {
			if _, err := pg.Exec("INSERT INTO audit (what) VALUES ('audit')"); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	catalog := strings.Replace(stateCatalog, "]}", `,
 {"id":"audit","method":"POST","path":"/api/audit","mutates":true,"storage":"public"}]}`, 1)
	p := newProject(t, srv.URL, "state:\n"+mysqlState("e2e")+"    row_tracking: [faq]\n"+postgresState("e2e"), catalog, "")
	write(t, filepath.Join(p.root, "driftgate", "allowlist.yaml"), allowDate+allowCreated)
	p.env = append(p.env, "DG_E2E_MYSQL="+myDSN, "DG_E2E_POSTGRES="+pgDSN)

	p.must(t, 0, "seed")
	if r := p.must(t, 0, "run"); !strings.Contains(r.out, "baseline local made") {
		t.Fatalf("first run: %v", r)
	}
	p.must(t, 0, "run")
	p.must(t, 0, "verify")
	if n := count(t, pg, "audit"); n != 1 {
		t.Errorf("%d audit rows after the runs, want the pristine 1", n)
	}

	api.SetVersion("v2") // planted drift: types, a date format, a header, a side effect
	r := p.must(t, 1, "run")
	for _, want := range []string{"news", "x-powered-by", "faq_log"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("run on v2 does not report %s:\n%s", want, r.out)
		}
	}
	p.must(t, 0, "capture", "v2")
	p.must(t, 1, "compare", "v2")
	p.must(t, 0, "accept", "*", "--reason", "v2 is intended")
	p.must(t, 0, "run")
	if r := p.must(t, 0, "verify"); !strings.Contains(r.out, "1 acceptances") {
		t.Errorf("verify: %v", r)
	}
	p.clean(t)
}
