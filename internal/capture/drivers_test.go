package capture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/config"
)

// sqliteProject is a project with one SQLite component on working, whose
// file exists.
func sqliteProject(t *testing.T, comp config.Component) *config.Project {
	t.Helper()
	t.Setenv(config.EnvStore, t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "database"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "app.sqlite"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	comp.Driver = config.SQLite
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": comp}}, root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSQLiteServer: an SQLite database is named by the real path of its
// working file, which must exist inside the project; two checkouts with
// their own files have their own pristine records.
func TestSQLiteServer(t *testing.T) {
	p := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	server, err := serverOf(p, p.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Join(p.Root, "database", "app.sqlite"))
	if server != "file:"+real {
		t.Errorf("server %q, want file:%s", server, real)
	}
	if ok, err := Seeded(context.Background(), p); err != nil || ok {
		t.Errorf("seeded: %v %v", ok, err)
	}

	q := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	other, err := serverOf(q, q.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	pa, _ := pristinePath(q, "db", server)
	pb, _ := pristinePath(q, "db", other)
	if pa == pb {
		t.Errorf("one pristine record for two files: %s", pa)
	}

	missing := sqliteProject(t, config.Component{Working: "database/missing.sqlite"})
	if _, err := serverOf(missing, missing.State["db"], ""); err == nil || !strings.Contains(err.Error(), "working: database/missing.sqlite: not found") {
		t.Errorf("missing working file: %v", err)
	}
	if _, err := Seeded(context.Background(), missing); err == nil {
		t.Error("Seeded with a missing working file: no error")
	}
}

// TestSQLiteFiles: the paths state.Options gets: the pristine copy in the
// store by default, else the file set (it need not exist), never a link to
// the working file or a file outside the project.
func TestSQLiteFiles(t *testing.T) {
	p := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	server, err := serverOf(p, p.State["db"], "")
	if err != nil {
		t.Fatal(err)
	}
	record, err := pristinePath(p, "db", server)
	if err != nil {
		t.Fatal(err)
	}
	working, pristine, err := sqliteFiles(p, p.State["db"], server, record)
	if err != nil {
		t.Fatal(err)
	}
	if working != strings.TrimPrefix(server, "file:") || pristine != strings.TrimSuffix(record, ".json")+".sqlite" {
		t.Errorf("default: %s %s (record %s)", working, pristine, record)
	}
	if !filepath.IsAbs(working) || !filepath.IsAbs(pristine) {
		t.Errorf("not absolute: %s %s", working, pristine)
	}

	sc := p.State["db"]
	sc.Pristine = `database\pristine.sqlite`
	if _, pristine, err = sqliteFiles(p, sc, server, record); err != nil || filepath.Base(pristine) != "pristine.sqlite" || !filepath.IsAbs(pristine) {
		t.Errorf("set: %s %v", pristine, err)
	}
	sc.Pristine = "missing/pristine.sqlite"
	if _, _, err = sqliteFiles(p, sc, server, record); err == nil {
		t.Error("pristine in a missing folder: no error")
	}
	if os.Symlink(filepath.Join(p.Root, "database", "app.sqlite"), filepath.Join(p.Root, "database", "link.sqlite")) == nil {
		sc.Pristine = "database/link.sqlite"
		if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "is the working file") {
			t.Errorf("pristine linked to working: %v", err)
		}
	}
	if os.Symlink(t.TempDir(), filepath.Join(p.Root, "outside")) == nil {
		sc.Pristine = "outside/pristine.sqlite"
		if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "outside the project") {
			t.Errorf("pristine through a link outside the project: %v", err)
		}
	}
}

// TestOpenSQLiteAndSQLServer: the capture opens SQLite and SQL Server
// components through their drivers, an SQLite database without a DSN or
// an allowed host. (Until the drivers land, both stop there; the drivers'
// own tests replace this one.)
func TestOpenSQLiteAndSQLServer(t *testing.T) {
	p := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	if _, err := openComponents(context.Background(), p, nil, nil); err == nil || !strings.Contains(err.Error(), "state db: SQLite is not implemented yet") {
		t.Errorf("sqlite: %v", err)
	}

	t.Setenv("DG_TEST_MSSQL_DSN", "sqlserver://sa:pw@127.0.0.1:1")
	s, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": {Driver: "mssql", DSN: config.Secret{Env: "DG_TEST_MSSQL_DSN"}, Working: "Shop"}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openComponents(context.Background(), s, nil, nil); err == nil || !strings.Contains(err.Error(), "state db: ") || !strings.Contains(err.Error(), "SQL Server is not implemented yet") {
		t.Errorf("sqlserver: %v", err)
	}
}
