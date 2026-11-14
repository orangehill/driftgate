package cli

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// TestRebaseAfterNewSeed: after a new seed changed the data of an SQLite
// or SQL Server database, a capture against the baseline stops on the
// state and says how to rebase; one with --skip-fingerprint-check,
// rebased into the baseline, gives it the fingerprint of the new data, and
// the next capture against the baseline runs and compares clean.
func TestRebaseAfterNewSeed(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		root := t.TempDir()
		db, err := sql.Open("sqlite", filepath.Join(root, "app.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		for _, s := range []string{"CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)", "INSERT INTO items (name) VALUES ('a')"} {
			if _, err := db.Exec(s); err != nil {
				t.Fatal(err)
			}
		}
		rebaseAfterNewSeed(t, root, db, "    driver: sqlite\n    working: app.sqlite\n", "table:items")
	})
	t.Run("sqlserver", func(t *testing.T) {
		u := sqlServerTestDSN(t)
		master := sqlServerOpen(t, u, "", "", "")
		work := sqlServerName()
		sqlServerLogin(t, master, work, work, "Dg-"+work)
		t.Setenv("DG_TEST_SQLSERVER", u.String())
		db := sqlServerOpen(t, u, work, "", "")
		rebaseAfterNewSeed(t, t.TempDir(), db, "    driver: sqlserver\n    dsn: { env: DG_TEST_SQLSERVER }\n    working: "+work+"\n    pristine: "+work+"_pristine\n", "table:dbo.items")
	})
}

// rebaseAfterNewSeed runs the steps of TestRebaseAfterNewSeed on the
// database db, whose state component is component, with a table items
// (item) the application counts.
func rebaseAfterNewSeed(t *testing.T, root string, db *sql.DB, component, item string) {
	srv := httptest.NewServer(noDate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM items").Scan(&n); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":%d}`, n)
	})))
	t.Cleanup(srv.Close)
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: p\nmode: evolve\nlevel: values\ntarget:\n  base_url: %s\nstate:\n  db:\n%s", srv.URL, component))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[{"id":"items","method":"GET","path":"/items","storage":"public"}]}`)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	k := make([]byte, secret.KeySize)
	rand.Read(k)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	cli := func(want int, args ...string) string {
		t.Helper()
		code, out, errOut := runCLI(t, append(args, "--config", cfg)...)
		if code != want {
			t.Fatalf("%v: %d, want %d\n%s\n%s", args, code, want, out, errOut)
		}
		return out + errOut
	}
	cli(ExitClean, "seed")
	cli(ExitClean, "run")
	base := filepath.Join(root, "driftgate", "baselines", "local")
	before := stateOf(t, base)

	// A new seed: one item more.
	if err := state.RetryInTransition(func() error { _, err := db.Exec("INSERT INTO items (name) VALUES ('b')"); return err }); err != nil {
		t.Fatal(err)
	}
	cli(ExitClean, "seed")
	if out := cli(ExitEnv, "capture", "new"); !strings.Contains(out, "the state differs from the one the baseline was captured from (1 items): [db/"+item+"]") ||
		!strings.Contains(out, "driftgate baselines rebase local --from <tag>") {
		t.Fatalf("capture on new data:\n%s", out)
	}
	cli(ExitClean, "capture", "new", "--skip-fingerprint-check", "--repeat", "2")
	if out := cli(ExitClean, "baselines", "rebase", "local", "--from", "new", "--reason", "new seed"); !strings.Contains(out, "the state differs from the old baseline's in 1 items") {
		t.Fatalf("rebase:\n%s", out)
	}
	after := stateOf(t, base)
	if after["db"][item] == "" || after["db"][item] == before["db"][item] {
		t.Fatalf("the baseline's state fingerprint: %v, before %v", after, before)
	}
	cli(ExitClean, "capture", "next")
	cli(ExitClean, "compare", "next", "--against", "local")
	if log, err := os.ReadFile(filepath.Join(base, baseline.LogFile)); err != nil || !strings.Contains(string(log), `"kind":"rebase"`) {
		t.Fatalf("acceptance log: %v\n%s", err, log)
	}
}

// stateOf is the state fingerprint a baseline records.
func stateOf(t *testing.T, dir string) map[string]map[string]string {
	t.Helper()
	meta, err := (&snapshot.Set{Dir: dir}).Meta()
	if err != nil || meta == nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return meta.State
}
