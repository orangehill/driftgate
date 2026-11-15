package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/demo"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const stateCatalog = `{"version":1,"requests":[
 {"id":"news","method":"GET","path":"/api/news","storage":"public"},
 {"id":"news-item","method":"GET","path":"/api/news/12","storage":"public"},
 {"id":"news-missing","method":"GET","path":"/api/news/{id}","params":{"id":"999"},"storage":"public"},
 {"id":"faq-a","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"},
 {"id":"faq-b","method":"POST","path":"/api/faq","body":{"q":"?"},"mutates":true,"storage":"public"}]}`

const stateAllowlist = `version: 1
headers:
  - name: date
    format: '[A-Z][a-z]{2}, \d{2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}:\d{2} GMT'
    reason: Time of the response.
body:
  - request: faq-*
    path: $.entry.created_at
    format: '\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}'
    reason: Time of creation.
`

type dbProject struct {
	cfg  string
	api  *demo.Server
	db   *sql.DB
	root string
}

// newDBProject runs the demo API on a MySQL/MariaDB database from
// DRIFTGATE_TEST_MYSQL, with that database as the project's state.
func newDBProject(t *testing.T) *dbProject {
	t.Helper()
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=") // row_tracking names row values with the key; never the Keychain
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	open := func(db string) *sql.DB {
		cfg := mysql.NewConfig()
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
		cfg.Net, cfg.Addr, cfg.DBName = "tcp", u.Host, db
		conn, err := mysql.NewConnector(cfg)
		if err != nil {
			t.Fatal(err)
		}
		d := sql.OpenDB(conn)
		t.Cleanup(func() { d.Close() })
		return d
	}
	admin := open("")
	for _, s := range []string{"DROP DATABASE IF EXISTS dgt_demo", "DROP DATABASE IF EXISTS dgt_demo_pristine", "CREATE DATABASE dgt_demo"} {
		if _, err := admin.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		admin.Exec("DROP DATABASE IF EXISTS dgt_demo")
		admin.Exec("DROP DATABASE IF EXISTS dgt_demo_pristine")
	})
	db := open("dgt_demo")
	for _, s := range demo.Schema {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	api := demo.NewSQL("v1", db)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf(`version: 1
project: demo
target:
  base_url: %s
state:
  db:
    driver: mysql
    dsn: { env: DG_TEST_DSN }
    working: dgt_demo
    pristine: dgt_demo_pristine
    row_tracking: [faq: { ignore: [created_at] }] # the tests' captures may share a second (runGap 0)
guards:
  - name: always
    command: ["true"]
`, srv.URL))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), stateCatalog)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), stateAllowlist)
	t.Setenv("DG_TEST_DSN", dsn)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	return &dbProject{cfg: cfg, api: api, db: db, root: root}
}

func (p *dbProject) record(t *testing.T, tag, id string) *snapshot.Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", tag, "responses", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r snapshot.Record
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func (p *dbProject) capture(t *testing.T, tag string, args ...string) string {
	t.Helper()
	code, out, errOut := runCLI(t, append([]string{"capture", tag, "--config", p.cfg}, args...)...)
	if code != ExitClean {
		t.Fatalf("capture %s: exit %d\n%s\n%s", tag, code, out, errOut)
	}
	return out
}

func TestStateCapture(t *testing.T) {
	p := newDBProject(t)
	if code, _, errOut := runCLI(t, "capture", "c0", "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "driftgate seed") {
		t.Fatalf("capture before seed: exit %d %s", code, errOut)
	}
	if code, out, errOut := runCLI(t, "seed", "--config", p.cfg); code != ExitClean || !strings.Contains(out, "pristine copy of db") {
		t.Fatalf("seed: exit %d\n%s\n%s", code, out, errOut)
	}

	// The GET of a news item is not marked as changing data; the capture
	// finds out that it does.
	out := p.capture(t, "c1")
	t.Logf("capture output:\n%s", out)
	if !strings.Contains(out, "news-item → db/table:news") || strings.Contains(out, "news-missing →") {
		t.Fatalf("writer not found:\n%s", out)
	}
	if se := p.record(t, "c1", "news-item").SideEffects; se == nil || !reflect.DeepEqual(se.Items, []string{"db/table:news"}) {
		t.Fatalf("news-item side effects: %+v", se)
	}
	if se := p.record(t, "c1", "news").SideEffects; se == nil || len(se.Items) != 0 {
		t.Fatalf("news side effects: %+v", se)
	}
	se := p.record(t, "c1", "faq-b").SideEffects
	// The inserted row's values are named under the project key: every
	// column but the ignored one, none as it is.
	if se != nil && len(se.Rows) == 1 {
		if v := se.Rows[0].Values; len(v) != 1 || len(v["id"]) != 32 {
			t.Fatalf("faq-b row values: %v", v)
		}
		se.Rows[0].Values = nil
	}
	want := &snapshot.SideEffects{Items: []string{"db/table:faq"}, Rows: []snapshot.RowChange{{Item: "db/table:faq", Key: "id=2", Change: "inserted"}}}
	if !reflect.DeepEqual(se, want) {
		t.Fatalf("faq-b side effects: %+v", se)
	}

	// The same code on the same state gives the same snapshot: views and
	// counters came back with every restore.
	p.capture(t, "c2")
	if code, r := compareJSON(t, p.cfg, "c2", "c1"); code != ExitClean {
		t.Fatalf("two captures differ: %+v", r.Results)
	}

	// v2 also writes a log row when a FAQ entry is created.
	p.api.SetVersion("v2")
	p.capture(t, "c3")
	_, r := compareJSON(t, p.cfg, "c3", "c1")
	found := false
	for _, res := range r.Results {
		for _, d := range res.Drifts {
			found = found || (d.Request == "faq-a" && d.Kind == "side-effect" && d.Where == "side-effects:db/table:faq_log")
		}
	}
	if !found {
		t.Fatalf("side effect drift not found: %+v", r.Results)
	}
	p.api.SetVersion("v1")

	// --restore each: every unit starts from the pristine state.
	p.capture(t, "c4", "--restore", "each")
	if se := p.record(t, "c4", "faq-b").SideEffects; se.Rows[0].Key != "id=1" {
		t.Fatalf("with --restore each faq-b inserted %s", se.Rows[0].Key)
	}

	// c1 becomes the baseline; a capture from different data is refused.
	if err := os.CopyFS(filepath.Join(p.root, "driftgate", "baselines", "local"), os.DirFS(filepath.Join(os.Getenv("DRIFTGATE_STORE"), "captures", "c1"))); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(filepath.Join(p.root, "driftgate", "baselines", "local")); err != nil {
		t.Fatal(err)
	}
	p.capture(t, "c5")
	if _, err := p.db.Exec("UPDATE news SET views = 100 WHERE id = 11"); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "seed", "--config", p.cfg); code != ExitClean {
		t.Fatalf("seed: %s", errOut)
	}
	code, _, errOut := runCLI(t, "capture", "c6", "--config", p.cfg)
	if code != ExitEnv || !strings.Contains(errOut, "db/table:news") {
		t.Fatalf("capture from other data: exit %d %s", code, errOut)
	}
	p.capture(t, "c7", "--skip-fingerprint-check")
	if code, _, errOut := runCLI(t, "compare", "c7", "--config", p.cfg); code != ExitConfig || !strings.Contains(errOut, "--skip-fingerprint-check") {
		t.Fatalf("compare of an unreliable capture: exit %d %s", code, errOut)
	}
	if code, _, _ := runCLI(t, "compare", "c7", "--config", p.cfg, "--skip-fingerprint-check"); code != ExitDrift {
		t.Fatalf("compare with the flag: exit %d (views differ, so drift)", code)
	}
}

func TestRepeatProposesAllowlist(t *testing.T) {
	p := newDemoProject(t, "")
	write(t, filepath.Join(filepath.Dir(p.cfg), "driftgate", "allowlist.yaml"), "version: 1\n")
	code, out, errOut := runCLI(t, "capture", "r", "--config", p.cfg, "--repeat", "2")
	if code != ExitDrift {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// The token is new on every login; times may fall in the same second,
	// so they are not checked here.
	for _, want := range []string{
		"  - request: 'login'\n    path: '$.token'\n    format: '[0-9a-f]{16}\\.[0-9a-f]{32}\\.[0-9a-f]{32}'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing proposal:\n%s\n--- output:\n%s", want, out)
		}
	}
	store := os.Getenv("DRIFTGATE_STORE")
	if _, err := os.Stat(filepath.Join(store, "captures", "r.repeat-2")); !os.IsNotExist(err) {
		t.Fatal("repeated run was kept")
	}
}
