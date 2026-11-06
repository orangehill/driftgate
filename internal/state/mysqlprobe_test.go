package state

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

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
	// A Galera node checks triggers as with the binary log on (1419).
	var wsrep bool
	if f.root.QueryRow("SELECT @@wsrep_on").Scan(&wsrep) == nil && wsrep {
		f.logBin = true
	}
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
			other, err := f.root.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			release, err := f.open(t, f.dsn, Options{}).probeLock(context.Background(), other, f.work)
			if err != nil {
				t.Fatal(err)
			}
			// The wait stays below the DSN's readTimeout (2 s): 1 s.
			short := f.dsn + "?readTimeout=2s"
			if strings.Contains(f.dsn, "?") {
				short = f.dsn + "&readTimeout=2s"
			}
			err = f.open(t, short, Options{}).Restore(context.Background(), []string{"view:v"})
			if err == nil || !strings.Contains(err.Error(), "was not free within 1 seconds: another DriftGate process") {
				t.Fatalf("restore while locked: %v", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if err := f.open(t, f.dsn, Options{}).Restore(context.Background(), []string{"view:v"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestAnchored: the firing order goes after FOR EACH ROW, in place of one
// the statement had.
func TestAnchored(t *testing.T) {
	for _, c := range [][3]string{
		{"CREATE DEFINER=`a`@`%` TRIGGER `t` BEFORE INSERT ON `x` FOR EACH ROW SET @a = 1", "FOLLOWS `s`", "CREATE DEFINER=`a`@`%` TRIGGER `t` BEFORE INSERT ON `x` FOR EACH ROW FOLLOWS `s` SET @a = 1"},
		{"CREATE DEFINER=`a`@`%` trigger t before insert on x for each row precedes `u` set @a = 1", "FOLLOWS `s`", "CREATE DEFINER=`a`@`%` trigger t before insert on x for each row FOLLOWS `s` set @a = 1"},
		{"CREATE DEFINER=`a`@`%` trigger t before insert on x for each row follows u set @a = 1", "", "CREATE DEFINER=`a`@`%` trigger t before insert on x for each row set @a = 1"},
		{"CREATE TRIGGER t BEFORE INSERT ON x FOR EACH ROW BEGIN SET @a = 'FOR EACH ROW'; END", "PRECEDES `s`", "CREATE TRIGGER t BEFORE INSERT ON x FOR EACH ROW PRECEDES `s` BEGIN SET @a = 'FOR EACH ROW'; END"},
		{"CREATE TRIGGER `for each row t` BEFORE INSERT ON `x FOR EACH ROW y` FOR EACH ROW SET @a = 1", "FOLLOWS `s`", "CREATE TRIGGER `for each row t` BEFORE INSERT ON `x FOR EACH ROW y` FOR EACH ROW FOLLOWS `s` SET @a = 1"},
		{"CREATE TRIGGER `a``for each row ` BEFORE INSERT ON x for each row follows `b``c` SET @a = 1", "PRECEDES `s`", "CREATE TRIGGER `a``for each row ` BEFORE INSERT ON x for each row PRECEDES `s` SET @a = 1"},
	} {
		if got := anchored(c[0], c[1]); got != c[2] {
			t.Errorf("%s %s:\n got %s\nwant %s", c[0], c[1], got, c[2])
		}
	}
}

// TestMySQLUndoTriggerOrder: a restore drops t1 (another DEFINER) first,
// then its drop of t2 is refused: t1 is made again before t2, as in the
// pristine copy, not after it.
func TestMySQLUndoTriggerOrder(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "undoorder", []string{"GRANT SELECT, INSERT ON {w}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT, what VARCHAR(10))"})
			mustExec(t, f.rootWork,
				"CREATE DEFINER = 'dgt_undoorder'@'%' TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id, 't1')",
				"CREATE TRIGGER t2 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id, 't2')",
				"CREATE TRIGGER t3 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id, 't3')")
			m := f.open(t, f.dsn, Options{})
			if err := m.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			order := func() string {
				rows, err := f.root.Query("SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY ACTION_ORDER", f.work)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var out []string
				for rows.Next() {
					var n string
					rows.Scan(&n)
					out = append(out, n)
				}
				return strings.Join(out, " ")
			}
			m.hooks = &mysqlHooks{dropRefused: func(kind, name string) bool { return kind == "trigger" && name == "t2" }}
			err := m.Restore(context.Background(), []string{"table:items"})
			if err == nil || !strings.Contains(err.Error(), "made again from the pristine copy: trigger t1") {
				t.Fatalf("restore: %v", err)
			}
			if got := order(); got != "t1 t2 t3" {
				t.Fatalf("firing order %s", got)
			}
			// t3 refused after t1 and t2: t1 then follows nothing left,
			// t2 follows t1, and both precede t3.
			m.hooks = &mysqlHooks{dropRefused: func(kind, name string) bool { return kind == "trigger" && name == "t3" }}
			if err := m.Restore(context.Background(), []string{"table:items"}); err == nil {
				t.Fatal("restore: no error")
			}
			if got := order(); got != "t1 t2 t3" {
				t.Fatalf("firing order %s", got)
			}
		})
	}
}

// TestMySQLUndoMessages: a refused drop of a table the pristine copy no
// longer has makes the trigger dropped before it again, and names the view
// dropped as the pristine copy has none, which no restore makes again. A
// view that cannot be made after the drops is named as missing.
func TestMySQLUndoMessages(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "undomsg", []string{"GRANT SELECT, INSERT ON {w}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)", "CREATE VIEW v2 AS SELECT id FROM items"})
			mustExec(t, f.rootWork, "CREATE DEFINER = 'dgt_undomsg'@'%' TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)")
			m := f.open(t, f.dsn, Options{})
			if err := m.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.rootWork, "CREATE TABLE extra (id INT PRIMARY KEY)", "CREATE VIEW wonly AS SELECT 1 AS a")
			m.hooks = &mysqlHooks{dropRefused: func(kind, name string) bool { return kind == "table" && name == "extra" }}
			err := m.Restore(context.Background(), []string{"table:items", "table:extra", "view:wonly"})
			if err == nil || !strings.Contains(err.Error(), "made again from the pristine copy: trigger t1") ||
				!strings.Contains(err.Error(), "Dropped from "+f.work+", as the pristine copy has nothing of that name, which no restore makes again: view wonly") {
				t.Fatalf("restore: %v", err)
			}
			if got := definerObjects(t, f.root, f.work); got != "trigger t1 dgt_undomsg@%, view v2 root@%" {
				t.Fatalf("after the refused restore: %s", got)
			}
			m.hooks = nil
			mustExec(t, f.rootWork, "RENAME TABLE items TO items_old")
			err = m.Restore(context.Background(), []string{"view:v2"})
			if err == nil || !strings.Contains(err.Error(), "Not made, so missing from "+f.work+" until the next successful restore: view v2") {
				t.Fatalf("restore: %v", err)
			}
		})
	}
}

// TestLockUnsupported: the refusals of GET_LOCK by a cluster, and no other
// error, let the probes run without the lock.
func TestLockUnsupported(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&mysql.MySQLError{Number: 1235, Message: "This version of MariaDB doesn't yet support 'GET_LOCK in cluster (WSREP_ON=ON)'"}, true},
		{&mysql.MySQLError{Number: 1105, Message: "Percona-XtraDB-Cluster prohibits use of GET_LOCK with pxc_strict_mode = ENFORCING"}, true},
		{&mysql.MySQLError{Number: 1105, Message: "Unknown error"}, false},
		{&mysql.MySQLError{Number: 1227, Message: "Access denied"}, false},
		{errors.New("connection reset"), false},
	} {
		if got := lockUnsupported(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}

// TestMySQLProbeViewRefusedInRestore: a login that may drop views but not
// create them in the working database is stopped by the probe, before the
// view is dropped.
func TestMySQLProbeViewRefusedInRestore(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probenoview", []string{"GRANT SELECT, INSERT, DELETE, CREATE, DROP, TRIGGER, SHOW VIEW ON {w}.* TO {u}", "GRANT ALL ON {p}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE DEFINER = 'dgt_probenoview'@'%' VIEW v AS SELECT id FROM items"})
			if err := f.open(t, f.dsn, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := definerObjects(t, f.root, f.work)
			err := f.open(t, f.app, Options{}).Restore(context.Background(), []string{"view:v"})
			if err == nil || !strings.Contains(err.Error(), "view v: Error 1142") {
				t.Fatalf("restore: %v", err)
			}
			if after := definerObjects(t, f.root, f.work); after != before {
				t.Fatalf("after the refused restore: %s, was %s", after, before)
			}
		})
	}
}

// TestMySQLUndoSequences (MariaDB): a sequence dropped before a refused
// drop is made again from the pristine copy; one the pristine copy lacks
// is named as gone; a copy that fails after the drops names the sequences
// not made.
func TestMySQLUndoSequences(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "undoseq", nil,
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			if !f.mariadb {
				t.Skip("sequences are MariaDB's")
			}
			var wsrep int
			if f.root.QueryRow("SELECT @@wsrep_on").Scan(&wsrep) == nil && wsrep != 0 {
				t.Skip("a Galera node copies no cached sequence (1235)")
			}
			mustExec(t, f.rootWork, "CREATE SEQUENCE s START WITH 5 NOCACHE")
			m := f.open(t, f.dsn, Options{})
			if err := m.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.rootWork, "CREATE SEQUENCE wseq NOCACHE")
			m.hooks = &mysqlHooks{dropRefused: func(kind, name string) bool { return kind == "trigger" && name == "t1" }}
			err := m.Restore(context.Background(), []string{"sequence:s", "sequence:wseq", "table:items"})
			if err == nil || !strings.Contains(err.Error(), "made again from the pristine copy: sequence s") ||
				!strings.Contains(err.Error(), "which no restore makes again: sequence wseq") {
				t.Fatalf("restore: %v", err)
			}
			var n int
			if err := f.rootWork.QueryRow("SELECT NEXTVAL(s)").Scan(&n); err != nil || n != 5 {
				t.Fatalf("sequence s: %d %v", n, err)
			}
			m.hooks = &mysqlHooks{copyFails: func() error { return errors.New("copy failed") }}
			err = m.Restore(context.Background(), []string{"sequence:s", "table:items"})
			if err == nil || !strings.Contains(err.Error(), "copy failed. Not made, so missing from "+f.work+" until the next successful restore: sequence s, trigger t1") {
				t.Fatalf("restore: %v", err)
			}
		})
	}
}

// TestMySQLSeedNotMade: a seed that fails after it began to make the
// pristine copy says that a new seed makes the missing objects, not a
// restore.
func TestMySQLSeedNotMade(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "seednotmade", nil,
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)", "CREATE VIEW v AS SELECT id FROM items"})
			m := f.open(t, f.dsn, Options{})
			m.hooks = &mysqlHooks{copyFails: func() error { return errors.New("copy failed") }}
			err := m.Seed(context.Background())
			if err == nil || !strings.Contains(err.Error(), "copy failed. Not made, so missing from "+f.pristine+" until driftgate seed runs again: trigger t1, view v") {
				t.Fatalf("seed: %v", err)
			}
		})
	}
}

// TestMySQLUndoRenamedTable: a trigger dropped with a table the pristine
// copy has under another name is named as on another table there, not as
// one the pristine copy lacks.
func TestMySQLUndoRenamedTable(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "undorenamed", nil,
				[]string{"CREATE TABLE a (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE TRIGGER tr AFTER INSERT ON a FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			m := f.open(t, f.dsn, Options{})
			if err := m.Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.rootWork, "RENAME TABLE a TO b")
			m.hooks = &mysqlHooks{dropRefused: func(kind, name string) bool { return kind == "table" && name == "b" }}
			err := m.Restore(context.Background(), []string{"table:b"})
			if err == nil || !strings.Contains(err.Error(), "trigger tr (on table b here, on a in the pristine copy)") || strings.Contains(err.Error(), "nothing of that name") {
				t.Fatalf("restore: %v", err)
			}
		})
	}
}

// TestMySQLProbeNoTables: a restore into a working database without
// tables, by a login that may not create one, cannot try a trigger's
// DEFINER: it is refused before the view is dropped.
func TestMySQLProbeNoTables(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probenotables", []string{"GRANT SELECT, DROP, CREATE VIEW, SHOW VIEW, TRIGGER ON {w}.* TO {u}", "GRANT ALL ON {p}.* TO {u}"},
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE DEFINER = 'dgt_probenotables'@'%' TRIGGER t1 AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)",
					"CREATE DEFINER = 'dgt_probenotables'@'%' VIEW v AS SELECT 1 AS a"})
			if err := f.open(t, f.dsn, Options{}).Seed(context.Background()); err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.rootWork, "DROP TABLE items", "DROP TABLE log")
			before := definerObjects(t, f.root, f.work)
			err := f.open(t, f.app, Options{}).Restore(context.Background(), []string{"table:items", "table:log", "view:v"})
			if err == nil || !strings.Contains(err.Error(), "trigger t1 cannot be tried before the restore") {
				t.Fatalf("restore: %v", err)
			}
			if after := definerObjects(t, f.root, f.work); after != before {
				t.Fatalf("after the refused restore: %s, was %s", after, before)
			}
		})
	}
}

// TestMySQLProbeTableTriggerEmpty: a login with CREATE but TRIGGER on
// items only seeds into a new pristine copy and restores into a working
// database without tables: the probe's table is made, its trigger refused,
// and with no table to put the probe on the CREATE TRIGGER on items itself
// decides; nothing is refused for the probe.
func TestMySQLProbeTableTriggerEmpty(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			f := newProbeFixture(t, dsn, "probetblempty", nil,
				[]string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
					"CREATE DEFINER = 'dgt_probetblempty'@'%' TRIGGER items_ai AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)"})
			mustExec(t, f.root, "CREATE DATABASE "+f.pristine, "CREATE TABLE "+f.pristine+".items (id INT PRIMARY KEY)")
			for _, db := range []string{f.work, f.pristine} {
				mustExec(t, f.root, "GRANT SELECT, CREATE, DROP, INSERT ON "+db+".* TO 'dgt_probetblempty'@'%'",
					"GRANT TRIGGER ON "+db+".items TO 'dgt_probetblempty'@'%'")
			}
			mustExec(t, f.root, "DROP DATABASE "+f.pristine)
			check := func(what string, err error) {
				t.Helper()
				switch {
				case f.logBin:
					if err == nil || !strings.Contains(err.Error(), "log_bin_trust_function_creators") {
						t.Fatalf("%s: %v", what, err)
					}
				case err != nil:
					t.Fatalf("%s: %v", what, err)
				}
			}
			check("seed", f.open(t, f.app, Options{}).Seed(context.Background()))
			if f.logBin {
				return
			}
			if got := definerObjects(t, f.root, f.pristine); got != "trigger items_ai dgt_probetblempty@%" {
				t.Fatalf("pristine: %s", got)
			}
			mustExec(t, f.rootWork, "DROP TABLE items", "DROP TABLE log")
			check("restore", f.open(t, f.app, Options{}).Restore(context.Background(), []string{"table:items", "table:log"}))
			if got := definerObjects(t, f.root, f.work); got != "trigger items_ai dgt_probetblempty@%" {
				t.Fatalf("working: %s", got)
			}
		})
	}
}
