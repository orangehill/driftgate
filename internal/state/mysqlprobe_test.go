package state

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/dbx"
)

// TestMySQLProbeName: only dgt_probe_ and 16 lower-case hex digits.
func TestMySQLProbeName(t *testing.T) {
	for name, want := range map[string]bool{"dgt_probe_0123456789abcdef": true, "dgt_probe_0123456789ABCDEF": false,
		"dgt_probe_0123456789abcde": false, "dgt_probe_0123456789abcdef0": false, "dgt_probe_users": false, "users": false} {
		if dbx.MySQLProbe(name) != want {
			t.Errorf("%s: %v", name, !want)
		}
	}
}

// TestViewSecurity: SQL SECURITY is read before " VIEW " only.
func TestViewSecurity(t *testing.T) {
	for stmt, want := range map[string]string{
		"CREATE ALGORITHM=UNDEFINED DEFINER=`a`@`%` SQL SECURITY INVOKER VIEW `v` AS select 1":                             " SQL SECURITY INVOKER",
		"CREATE ALGORITHM=UNDEFINED DEFINER=`a`@`%` SQL SECURITY DEFINER VIEW `v` AS select 'SQL SECURITY INVOKER' AS `x`": " SQL SECURITY DEFINER",
	} {
		if got := viewSecurity(stmt); got != want {
			t.Errorf("%s: %q", stmt, got)
		}
	}
}

// probeFixture is a working and a pristine database root made, and a login
// with the given grants (in which {w}, {p} and {u} stand for the working
// and pristine databases and the login).
type probeFixture struct {
	root, rootWork  *sql.DB
	dsn, app        string // root's and the login's DSN
	work, pristine  string
	logBin, mariadb bool
}

func newProbeFixture(t *testing.T, dsn, name string, grants, objects []string) probeFixture {
	t.Helper()
	u, _ := url.Parse(dsn)
	f := probeFixture{root: mysqlAdmin(t, dsn, ""), dsn: dsn, work: "dgt_" + name, pristine: "dgt_" + name + "_pristine"}
	var version string
	f.root.QueryRow("SELECT @@log_bin, VERSION()").Scan(&f.logBin, &version)
	f.mariadb = strings.Contains(version, "MariaDB")
	user := "'dgt_" + name + "'@'%'"
	mustExec(t, f.root, "DROP DATABASE IF EXISTS "+f.work, "DROP DATABASE IF EXISTS "+f.pristine, "DROP USER IF EXISTS "+user,
		"CREATE DATABASE "+f.work, "CREATE USER "+user+" IDENTIFIED BY 'x'")
	t.Cleanup(func() {
		f.root.Exec("DROP DATABASE IF EXISTS " + f.work)
		f.root.Exec("DROP DATABASE IF EXISTS " + f.pristine)
		f.root.Exec("DROP USER IF EXISTS " + user)
	})
	f.rootWork = mysqlAdmin(t, dsn, f.work)
	r := strings.NewReplacer("{w}", f.work, "{p}", f.pristine, "{u}", user)
	for _, o := range objects {
		mustExec(t, f.rootWork, r.Replace(o))
	}
	for _, g := range grants {
		mustExec(t, f.root, r.Replace(g))
	}
	app := *u
	app.User = url.UserPassword("dgt_"+name, "x")
	f.app = app.String()
	return f
}

func (f probeFixture) open(t *testing.T, dsn string, o Options) *MySQL {
	t.Helper()
	o.Name, o.DSN, o.Working, o.Pristine = "db", dsn, f.work, f.pristine
	m, err := NewMySQL(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// TestMySQLProbeSeedReadOnlyWorking: a login that may only read the working
// database (SELECT, SHOW VIEW) seeds a view of its own: the view's probe,
// which the login may not create there, is left out, as the trigger's is.
func TestMySQLProbeSeedReadOnlyWorking(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probero", []string{"GRANT SELECT, SHOW VIEW ON {w}.* TO {u}", "GRANT ALL ON {p}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE DEFINER = 'dgt_probero'@'%' VIEW v AS SELECT id FROM items"})
			if err := f.open(t, f.app, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := definerObjects(t, f.root, f.pristine); got != "view v dgt_probero@%" {
				t.Fatalf("pristine: %s", got)
			}
		})
	}
}

// TestMySQLProbeRestoreOnlyTable: a login without CREATE probes a trigger
// on a restore_only table, so on a server whose binary log refuses it the
// trigger, the restore stops before the trigger is dropped.
func TestMySQLProbeRestoreOnlyTable(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probero2", []string{"GRANT SELECT, INSERT, DELETE, TRIGGER ON {w}.* TO {u}", "GRANT SELECT, TRIGGER ON {p}.* TO {u}"},
				[]string{"CREATE TABLE sessions (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE DEFINER = 'dgt_probero2'@'%' TRIGGER sessions_ai AFTER INSERT ON sessions FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			if !f.logBin {
				t.Skip("the binary log is off: the trigger is not refused")
			}
			o := Options{RestoreOnly: []string{"sessions", "log"}}
			if err := f.open(t, f.dsn, o).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := definerObjects(t, f.root, f.work)
			err := f.open(t, f.app, o).Restore(context.Background(), []string{"trigger:sessions_ai"})
			if err == nil || !strings.Contains(err.Error(), "log_bin_trust_function_creators") {
				t.Fatalf("restore: %v", err)
			}
			if after := definerObjects(t, f.root, f.work); after != before {
				t.Fatalf("after the refused restore: %s, was %s", after, before)
			}
		})
	}
}

// TestMySQLProbeTableTrigger: a login granted TRIGGER on its tables only,
// not on the probe's table, probes on the trigger's own table instead of
// being refused.
func TestMySQLProbeTableTrigger(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probetbl", []string{"GRANT SELECT, CREATE, DROP, INSERT ON {w}.* TO {u}", "GRANT TRIGGER ON {w}.items TO {u}", "GRANT ALL ON {p}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE DEFINER = 'dgt_probetbl'@'%' TRIGGER items_ai AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			if err := f.open(t, f.dsn, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := definerObjects(t, f.root, f.work)
			err := f.open(t, f.app, Options{}).Restore(context.Background(), []string{"trigger:items_ai"})
			switch {
			case f.logBin:
				if err == nil || !strings.Contains(err.Error(), "log_bin_trust_function_creators") {
					t.Fatalf("restore: %v", err)
				}
			case err != nil:
				t.Fatalf("restore: %v", err)
			}
			if after := definerObjects(t, f.root, f.work); after != before {
				t.Fatalf("after the restore: %s, was %s", after, before)
			}
		})
	}
}

// TestMySQLProbeLeftBehind: a login without DROP cannot drop its probe;
// the error names it. The next restore by a login that may drops it, and
// says once that it found it.
func TestMySQLProbeLeftBehind(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probenodrop", []string{"GRANT SELECT, INSERT, CREATE, TRIGGER, CREATE VIEW, SHOW VIEW ON {w}.* TO {u}", "GRANT ALL ON {p}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE DEFINER = 'dgt_probenodrop'@'%' TRIGGER items_ai AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			if err := f.open(t, f.dsn, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := f.open(t, f.app, Options{}).Restore(context.Background(), []string{"trigger:items_ai"})
			if err == nil || !strings.Contains(err.Error(), "is left in "+f.work+", as it could not be dropped") {
				t.Fatalf("restore: %v", err)
			}
			if !strings.Contains(definerObjects(t, f.root, f.work), "probe dgt_probe_") {
				t.Fatal("no probe was left")
			}
			var log strings.Builder
			m := f.open(t, f.dsn, Options{Log: &log})
			if _, err := m.Fingerprint(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			if err := m.Restore(context.Background(), []string{"trigger:items_ai"}); err != nil {
				t.Fatal(err)
			}
			if got := definerObjects(t, f.root, f.work); strings.Contains(got, "dgt_probe_") || strings.Count(log.String(), "left behind") != 1 {
				t.Fatalf("after root's restore: %s\nlog: %s", got, log.String())
			}
		})
	}
}

// TestMySQLProbeLock: the probes of a database run under its named lock,
// so a second process waits for the first, and gives up with a clear
// error.
func TestMySQLProbeLock(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probelock", nil,
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM items"})
			if err := f.open(t, f.dsn, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func(w int) { probeLockWait = w }(probeLockWait)
			probeLockWait = 1
			other, err := f.root.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			release, err := probeLock(context.Background(), other, f.work)
			if err != nil {
				t.Fatal(err)
			}
			err = f.open(t, f.dsn, Options{}).Restore(context.Background(), []string{"view:v"})
			if err == nil || !strings.Contains(err.Error(), "was not free within 1 seconds: another DriftGate process") {
				t.Fatalf("restore while locked: %v", err)
			}
			release()
			if err := f.open(t, f.dsn, Options{}).Restore(context.Background(), []string{"view:v"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
