//go:build unix

package e2e

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// rowValueApp is an application whose two requests that change data write
// into the tracked table follows: /upd updates row 1, /ins inserts row 2.
// target is the value under test, touched a new random value on every
// request (as a token or a timestamp: two captures of one state show it),
// seen a value that changes only between runs (as a date: row_tracking
// ignores it).
type rowValueApp struct {
	db           *sql.DB
	ph           func(int) string // the driver's placeholder n
	mu           sync.Mutex
	target, seen string
}

func (a *rowValueApp) set(target, seen string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.target, a.seen = target, seen
}

func (a *rowValueApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	target, seen := a.target, a.seen
	a.mu.Unlock()
	b := make([]byte, 8)
	rand.Read(b)
	touched := hex.EncodeToString(b)
	var err error
	switch r.URL.Path {
	case "/upd":
		_, err = a.db.Exec("UPDATE follows SET target = "+a.ph(1)+", touched = "+a.ph(2)+", seen = "+a.ph(3)+" WHERE id = 1", target, touched, seen)
	case "/ins":
		_, err = a.db.Exec("INSERT INTO follows (id, target, touched, seen) VALUES (2, "+a.ph(1)+", "+a.ph(2)+", "+a.ph(3)+")", target, touched, seen)
	case "/ins3":
		_, err = a.db.Exec("INSERT INTO follows (id, target, touched, seen) VALUES (3, "+a.ph(1)+", "+a.ph(2)+", "+a.ph(3)+")", target, touched, seen)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const rowValueCatalog = `{"version":1,"requests":[
 {"id":"upd","method":"POST","path":"/upd","mutates":true,"storage":"public"},
 {"id":"ins","method":"POST","path":"/ins","mutates":true,"storage":"public"}]}`

const rowValueSchema = "CREATE TABLE follows (id INT PRIMARY KEY, target VARCHAR(20) NOT NULL, touched VARCHAR(40) NOT NULL, seen VARCHAR(20) NOT NULL)"

func rowValueKey() string {
	k := make([]byte, 32)
	rand.Read(k)
	return base64.StdEncoding.EncodeToString(k)
}

// rowValueProject sets up the application on one driver's database with
// row_tracking on follows (its column seen ignored) and the project key
// in DG_ROWKEY (empty: no key).
func rowValueProject(t *testing.T, driver, key string) (*project, *rowValueApp) {
	t.Helper()
	app := &rowValueApp{ph: func(int) string { return "?" }}
	var stateYAML string
	var env []string
	var root string
	switch driver {
	case "mysql":
		dsn, db := mysqlDatabase(t, "rowval")
		app.db, stateYAML, env = db, mysqlState("rowval"), []string{"DG_E2E_MYSQL=" + dsn}
	case "postgres":
		dsn, db := postgresDatabase(t, "rowval")
		app.db, stateYAML, env = db, postgresState("rowval"), []string{"DG_E2E_POSTGRES=" + dsn}
		app.ph = func(n int) string { return fmt.Sprintf("$%d", n) }
	case "sqlite":
		p := sqliteProject(t, "wal", rowValueCatalog)
		app.db, root = p.db, p.root
		stateYAML = "  db:\n    driver: sqlite\n    working: database/app.sqlite\n"
	case "sqlserver":
		p := sqlServerProject(t, "rowval")
		app.db, root, env = p.db, p.root, []string{"DG_E2E_SQLSERVER=" + sqlServerE2E(t)}
		stateYAML = fmt.Sprintf("  db:\n    driver: sqlserver\n    dsn: { env: DG_E2E_SQLSERVER }\n    working: %s\n    pristine: %s_pristine\n    exclude: [sessions]\n", p.work, p.work)
	}
	sqlExec(t, app.db, rowValueSchema, "INSERT INTO follows VALUES (1, 'seeded', 'seeded', 'seeded')")
	app.set("X", "s1")
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p := newProject(t, srv.URL, "", rowValueCatalog, "")
	if root != "" {
		p.root = root
	}
	write(t, filepath.Join(p.root, "driftgate.yaml"), fmt.Sprintf("version: 1\nproject: e2e\nmode: evolve\nkey: { env: DG_ROWKEY }\ntarget:\n  base_url: %s\nstate:\n%s    row_tracking:\n      - follows: { ignore: [Seen] }\n", srv.URL, stateYAML))
	write(t, filepath.Join(p.root, "driftgate", "catalog.json"), rowValueCatalog)
	write(t, filepath.Join(p.root, "driftgate", "allowlist.yaml"), allowDate)
	p.env = append(p.env, env...)
	p.env = append(p.env, "DG_ROWKEY="+key)
	return p, app
}

func withKey(p *project, key string) {
	for i, e := range p.env {
		if strings.HasPrefix(e, "DG_ROWKEY=") {
			p.env[i] = "DG_ROWKEY=" + key
		}
	}
}

// TestRowValue: a request that writes another value into a tracked row
// than at baseline (an update of the same row, an inserted row) drifts,
// on every driver; a column that changes by itself is learned from the
// baseline's two captures, an ignored column is never compared, accept
// keeps what was learned, and a capture under another key stops.
func TestRowValue(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "sqlite", "sqlserver"} {
		t.Run(driver, func(t *testing.T) {
			key := rowValueKey()
			p, app := rowValueProject(t, driver, key)
			p.must(t, 0, "seed")
			r := p.must(t, 0, "run")
			if !strings.Contains(r.out, "baseline local made") || !strings.Contains(r.out, "follows.touched") {
				t.Fatalf("first run does not learn touched: %v", r)
			}
			app.set("X", "s2") // an ignored column only
			p.must(t, 0, "run")

			app.set("Y", "s2")
			r = p.must(t, 1, "run")
			for _, want := range []string{"side-effect ", "follows[id=1]", "follows[id=2]", "other value: target"} {
				if !strings.Contains(r.out, want) {
					t.Errorf("another value: no %q in\n%s", want, r.out)
				}
			}
			if strings.Contains(r.out, "other values") || strings.Contains(r.out, "value: touched") || strings.Contains(r.out, "value: seen") {
				t.Errorf("a learned or ignored column drifts:\n%s", r.out)
			}
			p.must(t, 0, "accept", "*", "--reason", "Y is intended")
			p.must(t, 0, "run") // touched stays learned after accept
			app.set("Z", "s2")
			if r := p.must(t, 1, "run"); !strings.Contains(r.out, "other value: target") {
				t.Errorf("after accept, another value: %v", r)
			}
			// A rebase takes a capture two runs agreed on, with what they
			// learned: touched stays out of the comparison.
			p.must(t, 0, "capture", "r", "--repeat", "2")
			p.must(t, 0, "baselines", "rebase", "local", "--from", "r", "--reason", "Z is intended")
			p.must(t, 0, "run")

			// A new request accepted from run's single capture: its values
			// were not seen twice, so they are not compared (touched would
			// stop every run), and the result says how to learn them.
			write(t, filepath.Join(p.root, "driftgate", "catalog.json"), strings.Replace(rowValueCatalog, "]}", `,
 {"id":"ins3","method":"POST","path":"/ins3","mutates":true,"storage":"public"}]}`, 1))
			p.must(t, 0, "run")
			p.must(t, 0, "accept", "ins3", "--reason", "new request")
			r = p.must(t, 0, "run")
			if !strings.Contains(r.out, "1 request that changes data whose row values were not compared (baseline:local did not see them in two captures") {
				t.Errorf("an accepted new request: %v", r)
			}
			// A rebase from two captures learns them: another value drifts.
			p.must(t, 0, "capture", "r2", "--repeat", "2")
			p.must(t, 0, "baselines", "rebase", "local", "--from", "r2", "--reason", "learn ins3")
			app.set("W", "s2")
			if r := p.must(t, 1, "run"); !strings.Contains(r.out, "follows[id=3]") || !strings.Contains(r.out, "other value: target") {
				t.Errorf("after the rebase, another value: %v", r)
			}

			// Two captures made once: touched, which changes by itself, is
			// not known, so no values are compared, and the result says so.
			p.must(t, 0, "capture", "a")
			p.must(t, 0, "capture", "b")
			if r := p.must(t, 0, "compare", "b", "--against", "a"); !strings.Contains(r.out, "row values were not compared (capture:a did not see them in two captures") {
				t.Errorf("captures made once: %v", r)
			}

			withKey(p, rowValueKey())
			r = p.must(t, 3, "run")
			if !strings.Contains(r.out+r.err, "different project keys") {
				t.Errorf("another key: %v", r)
			}
		})
	}
}

// TestRowValueWithoutValues: a baseline made without the project key
// records no row values; a later run says so under "not covered", and
// compares what it can.
func TestRowValueWithoutValues(t *testing.T) {
	p, app := rowValueProject(t, "mysql", "")
	p.must(t, 0, "seed")
	r := p.must(t, 0, "run")
	if !strings.Contains(r.out, "values requests write were not recorded") {
		t.Errorf("no note of the values not recorded: %v", r)
	}
	withKey(p, rowValueKey())
	app.set("Y", "s1")
	r = p.must(t, 0, "run")
	if !strings.Contains(r.out, "Not covered by this result") || !strings.Contains(r.out, "2 requests that change data whose row values were not compared (the baseline recorded no row values") {
		t.Errorf("not covered does not name the row values: %v", r)
	}
}
