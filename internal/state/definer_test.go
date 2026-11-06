package state

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestRefusedText: a trigger or view refused for a privilege names the
// object, the database, the DEFINER and the login when they differ, and
// what to do; the server's error stays in the chain.
func TestRefusedText(t *testing.T) {
	const trig = "CREATE DEFINER=`root`@`%` TRIGGER `t` AFTER INSERT ON `items` FOR EACH ROW SET @x = 1"
	const view = "CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` AS select 1 AS `1`"
	denied := &mysql.MySQLError{Number: 1227, Message: "Access denied; you need (at least one of) the SUPER or SET_ANY_DEFINER privilege(s) for this operation"}
	binlog := &mysql.MySQLError{Number: 1419, Message: "You do not have the SUPER privilege and binary logging is enabled"}
	for _, c := range []struct {
		kind, name, stmt, login string
		me                      *mysql.MySQLError
		want, not               []string
	}{
		{"view", "v", view, "app@%", denied,
			[]string{"view v cannot be created in shop: its DEFINER is `root`@`localhost`, this login is app@%", "SET_ANY_DEFINER", "SET USER (MariaDB)", "with this login as their DEFINER", "does not change it", "SQL SECURITY DEFINER", "Error 1227"}, nil},
		{"trigger", "t", trig, "app@%", binlog,
			[]string{"trigger t cannot be created in shop: the server writes a binary log", "SET PERSIST log_bin_trust_function_creators = 1", "Its DEFINER `root`@`%` is not this login (app@%) either", "SET_ANY_DEFINER", "Error 1419"}, nil},
		{"trigger", "t", strings.Replace(trig, "`root`@`%`", "`app`@`%`", 1), "app@%", binlog,
			[]string{"log_bin_trust_function_creators"}, []string{"either"}},
		// The host of an account is compared without letter case.
		{"view", "v", strings.Replace(view, "`root`@`localhost`", "`app`@`LocalHost`", 1), "app@localhost", denied,
			[]string{"view v: Error 1227"}, []string{"its DEFINER"}},
		// A login that is not known names no DEFINER.
		{"view", "v", view, "", denied, []string{"view v: Error 1227"}, []string{"cannot be created"}},
		{"view", "v", strings.Replace(view, "`root`", "`ro``ot`", 1), "ro`ot@localhost", denied, []string{"view v: Error 1227"}, nil},
	} {
		err := refusedText(c.kind, c.name, "shop", c.stmt, c.login, c.me)
		var me *mysql.MySQLError
		if !errors.As(err, &me) || me != c.me {
			t.Errorf("%s: the server's error is not in the chain: %v", c.stmt, err)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s %s:\n%v\nlacks %q", c.kind, c.login, err, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(err.Error(), n) {
				t.Errorf("%s %s:\n%v\nhas %q", c.kind, c.login, err, n)
			}
		}
	}
}

// TestMySQLDefinerRefused: a login without the right to name another
// account as DEFINER (an application's own login, owning its databases)
// cannot re-create a trigger or a view root defined. The restore and the
// seed stop before they drop anything (definerProbe) and say which
// object, why and what to do; the DEFINER is never dropped to make the
// object fit, and no probe is left behind or fingerprinted.
func TestMySQLDefinerRefused(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			root := mysqlAdmin(t, dsn, "")
			work, pristine := "dgt_definer", "dgt_definer_pristine"
			var logBin int
			root.QueryRow("SELECT @@log_bin").Scan(&logBin)
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "DROP USER IF EXISTS 'dgt_definer'@'%'", "DROP USER IF EXISTS 'dgt_definer_other'@'%'",
				"CREATE DATABASE "+work, "CREATE USER 'dgt_definer_other'@'%' IDENTIFIED BY 'x'", "GRANT SELECT ON "+work+".* TO 'dgt_definer_other'@'%'", "CREATE USER 'dgt_definer'@'%' IDENTIFIED BY 'x'",
				"GRANT ALL ON "+work+".* TO 'dgt_definer'@'%'", "GRANT ALL ON "+pristine+".* TO 'dgt_definer'@'%'")
			t.Cleanup(func() {
				root.Exec("DROP DATABASE IF EXISTS " + work)
				root.Exec("DROP DATABASE IF EXISTS " + pristine)
				root.Exec("DROP USER IF EXISTS 'dgt_definer'@'%'")
				root.Exec("DROP USER IF EXISTS 'dgt_definer_other'@'%'")
			})
			// Made in the database, so their definitions name none.
			mustExec(t, mysqlAdmin(t, dsn, work),
				"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT AUTO_INCREMENT PRIMARY KEY, item INT)",
				"CREATE TRIGGER items_ai AFTER INSERT ON items FOR EACH ROW INSERT INTO log (item) VALUES (NEW.id)",
				"CREATE SQL SECURITY DEFINER VIEW v_items AS SELECT id FROM items",
				"CREATE DEFINER = 'dgt_definer_other'@'%' SQL SECURITY DEFINER VIEW v_other AS SELECT id FROM items")
			var rootAccount string
			root.QueryRow("SELECT CURRENT_USER()").Scan(&rootAccount)
			at := strings.LastIndexByte(rootAccount, '@')
			definer := "`" + rootAccount[:at] + "`@`" + rootAccount[at+1:] + "`"

			app := *u
			app.User = url.UserPassword("dgt_definer", "x")
			open := func(dsn string) *MySQL {
				m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { m.Close() })
				return m
			}
			trigger := func(err error, db string) {
				t.Helper()
				if err == nil {
					t.Fatal("the trigger was created")
				}
				want := []string{"trigger items_ai cannot be created in " + db, "does not change it"}
				if logBin != 0 { // no trigger without SUPER
					want = append(want, "log_bin_trust_function_creators")
				}
				if !strings.Contains(err.Error(), "log_bin_trust_function_creators") {
					want = append(want, "its DEFINER is "+definer+", this login is dgt_definer@%")
				}
				for _, w := range want {
					if !strings.Contains(err.Error(), w) {
						t.Fatalf("%v\nlacks %q", err, w)
					}
				}
				t.Log(err)
			}

			// objects lists the triggers and views of the database with
			// their DEFINERs, and any probe.
			objects := func(db string) string {
				t.Helper()
				var out []string
				for _, q := range []string{
					"SELECT CONCAT('trigger ', TRIGGER_NAME, ' ', DEFINER) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?",
					"SELECT CONCAT('view ', TABLE_NAME, ' ', DEFINER) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?",
					"SELECT CONCAT('probe ', TABLE_NAME) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME LIKE 'dgt\\_probe\\_%'",
				} {
					rows, err := root.Query(q, db)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var s string
						rows.Scan(&s)
						out = append(out, s)
					}
					rows.Close()
				}
				sort.Strings(out)
				return strings.Join(out, ", ")
			}
			fingerprint := func(pristine bool) string {
				t.Helper()
				m := open(dsn)
				get := m.Fingerprint
				if pristine {
					get = m.PristineFingerprint
				}
				f, err := get(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				return fmt.Sprint(f)
			}
			workObjects := objects(work)
			if strings.Contains(workObjects, "probe") || strings.Count(workObjects, ", ")+1 != 3 {
				t.Fatalf("working database: %s", workObjects)
			}

			// The application's login seeds: the pristine copy's trigger
			// would be root's. The seed stops before the copy is touched.
			trigger(open(app.String()).Seed(ctx), pristine)
			if got := objects(work); got != workObjects {
				t.Fatalf("after the refused seed: %s", got)
			}

			// root seeds; the application's login restores, and seeds again.
			if err := open(dsn).Seed(ctx); err != nil {
				t.Fatal(err)
			}
			workPrint, pristinePrint, pristineObjects := fingerprint(false), fingerprint(true), objects(pristine)
			m := open(app.String())
			trigger(m.Restore(ctx, []string{"trigger:items_ai"}), work)
			trigger(m.Restore(ctx, []string{"table:items"}), work) // its trigger is made again
			err := m.Restore(ctx, []string{"view:v_items"})
			if err == nil || !strings.Contains(err.Error(), "view v_items cannot be created in "+work+": its DEFINER is "+definer+", this login is dgt_definer@%") {
				t.Fatalf("view: %v", err)
			}
			t.Log(err)
			err = m.Restore(ctx, []string{"view:v_other"})
			if err == nil || !strings.Contains(err.Error(), "view v_other cannot be created in "+work+": its DEFINER is `dgt_definer_other`@`%`, this login is dgt_definer@%") {
				t.Fatalf("view: %v", err)
			}
			t.Log(err)
			trigger(open(app.String()).Seed(ctx), pristine)
			// Nothing was dropped, nothing made with another DEFINER, no
			// probe is left, and the fingerprints did not change.
			if got := objects(work); got != workObjects {
				t.Fatalf("working database after the refused restores: %s, was %s", got, workObjects)
			}
			if got := objects(pristine); got != pristineObjects {
				t.Fatalf("pristine database after the refused seed: %s, was %s", got, pristineObjects)
			}
			if fingerprint(false) != workPrint || fingerprint(true) != pristinePrint {
				t.Fatal("a fingerprint changed")
			}

			// root may: its restore probes, makes the objects again and
			// leaves no probe. A probe a killed process left behind is in
			// no fingerprint, and the next restore drops it.
			mustExec(t, mysqlAdmin(t, dsn, work), "CREATE TABLE dgt_probe_0123456789abcdef (id INT PRIMARY KEY)",
				"CREATE TRIGGER dgt_probe_0123456789abcdef BEFORE INSERT ON dgt_probe_0123456789abcdef FOR EACH ROW SET @x = 1",
				"CREATE VIEW dgt_probe_fedcba9876543210 AS SELECT 1 AS a")
			if fingerprint(false) != workPrint {
				t.Fatal("a probe left behind is in the fingerprint")
			}
			if err := open(dsn).Restore(ctx, []string{"trigger:items_ai", "view:v_items", "view:v_other"}); err != nil {
				t.Fatal(err)
			}
			if got := objects(work); got != workObjects {
				t.Fatalf("working database after root's restore: %s, was %s", got, workObjects)
			}
			if fingerprint(false) != workPrint {
				t.Fatal("the fingerprint changed")
			}
		})
	}
}
