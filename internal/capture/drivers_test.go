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
	// A folder that does not exist yet, which seed makes.
	sc.Pristine = "missing/deeper/pristine.sqlite"
	realRoot, _ := filepath.EvalSymlinks(p.Root)
	if _, pristine, err = sqliteFiles(p, sc, server, record); err != nil || pristine != filepath.Join(realRoot, "missing", "deeper", "pristine.sqlite") {
		t.Errorf("pristine in a missing folder: %s %v", pristine, err)
	}
	sc.Pristine = "missing/../../pristine.sqlite"
	if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "outside the project") {
		t.Errorf("pristine above the project: %v", err)
	}
	if os.Symlink(filepath.Join(p.Root, "database", "app.sqlite"), filepath.Join(p.Root, "database", "link.sqlite")) == nil {
		sc.Pristine = "database/link.sqlite"
		if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "is the working file") {
			t.Errorf("pristine linked to working: %v", err)
		}
	}
	// A sidecar of working through a linked folder, which need not exist.
	if os.Symlink(filepath.Join(p.Root, "database"), filepath.Join(p.Root, "alias")) == nil {
		for _, f := range []string{"alias/app.sqlite-wal", "alias/APP.sqlite-shm", "alias/app.sqlite"} {
			sc.Pristine = f
			if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "is the working file") {
				t.Errorf("pristine %s: %v", f, err)
			}
		}
	}
	// Working through a link; pristine is the real file's journal.
	q := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	if os.Symlink(filepath.Join(q.Root, "database", "app.sqlite"), filepath.Join(q.Root, "app.sqlite")) == nil {
		qc := q.State["db"]
		qc.Working, qc.Pristine = "app.sqlite", "database/app.sqlite-journal"
		qserver, err := serverOf(q, qc, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = sqliteFiles(q, qc, qserver, record); err == nil || !strings.Contains(err.Error(), "is the working file") {
			t.Errorf("pristine the journal of working's real file: %v", err)
		}
	}
	// The reverse: working is a sidecar of the pristine copy.
	if !sqliteOverlap("/p/db.sqlite-wal", "/p/db.sqlite") || sqliteOverlap("/p/db.sqlite", "/p/db2.sqlite") {
		t.Error("sqliteOverlap by path")
	}
	// A hard link to working's -wal file.
	wal := filepath.Join(p.Root, "database", "app.sqlite-wal")
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Link(wal, filepath.Join(p.Root, "database", "hard.sqlite")) == nil {
		sc.Pristine = "database/hard.sqlite"
		if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "is the working file") {
			t.Errorf("pristine a hard link to working's -wal: %v", err)
		}
	}
	if os.Symlink(t.TempDir(), filepath.Join(p.Root, "outside")) == nil {
		for _, f := range []string{"outside/pristine.sqlite", "outside/missing/pristine.sqlite", "outside/../pristine.sqlite"} {
			sc.Pristine = f
			if _, _, err = sqliteFiles(p, sc, server, record); err == nil || !strings.Contains(err.Error(), "outside the project") {
				t.Errorf("pristine %s through a link outside the project: %v", f, err)
			}
		}
	}
	if os.Symlink(filepath.Join(p.Root, "gone"), filepath.Join(p.Root, "dangling")) == nil {
		sc.Pristine = "dangling/pristine.sqlite"
		if _, _, err = sqliteFiles(p, sc, server, record); err == nil {
			t.Error("pristine through a link that leads nowhere: no error")
		}
	}
}

// TestSQLiteWorkingPhysical: working is the file the system opens, past a
// link before a .. (sub/../app.sqlite with sub a link to database/sub is
// database/app.sqlite), never one that leaves the project.
func TestSQLiteWorkingPhysical(t *testing.T) {
	p := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	if err := os.MkdirAll(filepath.Join(p.Root, "database", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if os.Symlink(filepath.Join(p.Root, "database", "sub"), filepath.Join(p.Root, "sub")) != nil {
		t.Skip("no symbolic links")
	}
	sc := p.State["db"]
	sc.Working = "sub/../app.sqlite"
	server, err := serverOf(p, sc, "")
	real, _ := filepath.EvalSymlinks(filepath.Join(p.Root, "database", "app.sqlite"))
	if err != nil || server != "file:"+real {
		t.Errorf("working through a link: %s %v, want file:%s", server, err, real)
	}
	sc.Pristine = "sub/../app.sqlite-wal"
	if _, _, err = sqliteFiles(p, sc, server, ""); err == nil || !strings.Contains(err.Error(), "is the working file") {
		t.Errorf("pristine working's -wal through the link: %v", err)
	}
	if os.Symlink(t.TempDir(), filepath.Join(p.Root, "out")) == nil {
		if err := os.WriteFile(filepath.Join(p.Root, "app.sqlite"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		sc.Working = "out/../app.sqlite" // lexically app.sqlite of the project
		if _, err := serverOf(p, sc, ""); err == nil || !strings.Contains(err.Error(), "outside the project") {
			t.Errorf("working through a link outside the project: %v", err)
		}
	}
}

// TestOpenSQLiteAndSQLServer: the capture opens SQLite and SQL Server
// components through their drivers. An SQLite database is a file: no host
// check applies to it, even when only a remote host is allowed. Whatever a
// driver reports is named after the component.
func TestOpenSQLiteAndSQLServer(t *testing.T) {
	p := sqliteProject(t, config.Component{Working: "database/app.sqlite"})
	p.Target.AllowHosts = []string{"203.0.113.10"}
	comps, err := openComponents(context.Background(), p, nil, nil)
	if err == nil {
		for _, c := range comps {
			c.Close()
		}
	} else if !strings.Contains(err.Error(), "state db: ") || strings.Contains(err.Error(), "allow") || strings.Contains(err.Error(), "not a server") {
		t.Errorf("sqlite: %v", err)
	}

	t.Setenv("DG_TEST_MSSQL_DSN", "sqlserver://sa:pw@127.0.0.1:1")
	s, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		State: map[string]config.Component{"db": {Driver: "mssql", DSN: config.Secret{Env: "DG_TEST_MSSQL_DSN"}, Working: "Shop"}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	comps, err = openComponents(context.Background(), s, nil, nil)
	if err == nil {
		for _, c := range comps {
			c.Close()
		}
	} else if !strings.Contains(err.Error(), "state db: ") {
		t.Errorf("sqlserver: %v", err)
	}
}
