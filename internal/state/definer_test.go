package state

import (
	"context"
	"errors"
	"net/url"
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
// seed stop and say which object, why and what to do; the DEFINER is
// never dropped to make the object fit.
func TestMySQLDefinerRefused(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			root := mysqlAdmin(t, dsn, "")
			work, pristine := "dgt_definer", "dgt_definer_pristine"
			var logBin int
			var version string
			root.QueryRow("SELECT @@log_bin, VERSION()").Scan(&logBin, &version)
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
				if logBin != 0 && !strings.Contains(version, "MariaDB") { // MySQL: no trigger without SUPER
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

			// The application's login seeds: the pristine copy's trigger
			// would be root's.
			trigger(open(app.String()).Seed(ctx), pristine)

			// root seeds; the application's login restores.
			if err := open(dsn).Seed(ctx); err != nil {
				t.Fatal(err)
			}
			m := open(app.String())
			trigger(m.Restore(ctx, []string{"trigger:items_ai"}), work)
			// MySQL refuses already to drop a view root defined (root has
			// SYSTEM_USER); MariaDB to create it again.
			err := m.Restore(ctx, []string{"view:v_items"})
			if err == nil || !strings.Contains(err.Error(), "view v_items cannot be created in "+work+": its DEFINER is "+definer+", this login is dgt_definer@%") &&
				!strings.Contains(err.Error(), "view v_items cannot be dropped from "+work+" to be made again: an object whose DEFINER has SYSTEM_USER") {
				t.Fatalf("view: %v", err)
			}
			t.Log(err)
			// A view of another account without SYSTEM_USER: dropped, and
			// not made again.
			err = m.Restore(ctx, []string{"view:v_other"})
			if err == nil || !strings.Contains(err.Error(), "view v_other cannot be created in "+work+": its DEFINER is `dgt_definer_other`@`%`, this login is dgt_definer@%") {
				t.Fatalf("view: %v", err)
			}
			t.Log(err)
			// Nothing was made with another DEFINER.
			var n int
			root.QueryRow("SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND DEFINER <> ?", work, rootAccount).Scan(&n)
			if k := 0; n != k {
				t.Fatalf("%d triggers with another DEFINER", n)
			}
			root.QueryRow("SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ? AND DEFINER <> ?", work, rootAccount).Scan(&n)
			if n != 0 {
				t.Fatalf("%d views with another DEFINER", n)
			}
		})
	}
}
