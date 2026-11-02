package data

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestHosts: every host the driver connects to, as the driver reads the
// DSN; a host= parameter is not a local socket.
func TestHosts(t *testing.T) {
	t.Setenv("PGHOST", "")
	t.Setenv("PGSERVICE", "")
	for _, c := range []struct {
		driver, dsn string
		want        []string
	}{
		{Postgres, "postgres://u:p@/db?host=203.0.113.9", []string{"203.0.113.9"}},
		{Postgres, "postgres://u:p@/db?host=203.0.113.9&port=5433&sslmode=disable", []string{"203.0.113.9"}},
		{Postgres, "postgres://u:p@127.0.0.1:5432,db.example.com:5432/db?sslmode=disable", []string{"127.0.0.1", "db.example.com"}},
		{Postgres, "postgres://u:p@/db?host=/var/run/postgresql&sslmode=disable", []string{""}},
		{Postgres, "postgres://u:p@/db?host=/tmp,203.0.113.9&sslmode=disable", []string{"", "203.0.113.9"}},
		{Postgres, "postgres://u:p@localhost:5432/db?sslmode=disable", []string{"localhost"}},
		{MySQL, "mysql://u:p@db.example.com:3306/", []string{"db.example.com"}},
		{MySQL, "mysql://u:p@/", []string{"localhost"}},
		{MySQL, "u:p@tcp(203.0.113.9:3306)/", []string{"203.0.113.9"}},
		{MySQL, "u:p@unix(/tmp/mysql.sock)/", []string{""}},
	} {
		got, err := Database{Driver: c.driver, DSN: c.dsn}.Hosts()
		if err != nil {
			t.Errorf("%s: %v", c.dsn, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.dsn, got, c.want)
		}
	}
	if _, err := (Database{Driver: Postgres, DSN: "host=db.example.com"}).Hosts(); err == nil {
		t.Error("a postgres DSN that is not a URL was accepted")
	}
}

func TestUnqualify(t *testing.T) {
	for _, c := range [][2]string{
		{"INSERT INTO `shop`.`audit` (id) VALUES (NEW.id)", "INSERT INTO `audit` (id) VALUES (NEW.id)"},
		{"INSERT INTO shop.audit SELECT * FROM SHOP.log", "INSERT INTO audit SELECT * FROM log"},
		{`INSERT INTO "shop"."audit" VALUES (1)`, `INSERT INTO "audit" VALUES (1)`},
		// Another database, a column of a table of the same name, strings and comments stay.
		{"INSERT INTO other.audit VALUES ('shop.audit', \"`shop`.x\") -- shop.audit\n", "INSERT INTO other.audit VALUES ('shop.audit', \"`shop`.x\") -- shop.audit\n"},
		{"SET NEW.a = shop.shop.a /* shop.b */", "SET NEW.a = shop.a /* shop.b */"},
		{"SELECT 'it''s shop.x', `sh``op`.y, shopx.z, x.shop.y", "SELECT 'it''s shop.x', `sh``op`.y, shopx.z, x.shop.y"},
		{"SELECT 'a\\'shop.x' FROM `shop` . t, shop\n.u", "SELECT 'a\\'shop.x' FROM  t, u"},
	} {
		if got := unqualify(c[0], "shop"); got != c[1] {
			t.Errorf("unqualify(%q)\n got %q\nwant %q", c[0], got, c[1])
		}
	}
	if got := unqualify("INSERT INTO `sh``op`.t VALUES (1)", "sh`op"); got != "INSERT INTO t VALUES (1)" {
		t.Errorf("quoted backtick: %q", got)
	}
}

func TestStagingName(t *testing.T) {
	a, err := stagingName("app_dev")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := stagingName("app_dev")
	if a == b || !strings.HasPrefix(a, "app_dev"+stagingInfix) {
		t.Errorf("%q, %q", a, b)
	}
	long, _ := stagingName(strings.Repeat("ž", 40))
	if len(long) > 63 || !utf8.ValidString(long) || !strings.Contains(long, stagingInfix) {
		t.Errorf("%q (%d bytes)", long, len(long))
	}
}

func TestTargetCheck(t *testing.T) {
	for _, c := range []struct {
		st      dbState
		replace bool
		want    error
	}{
		{dbState{}, false, nil},
		{dbState{exists: true}, false, nil},                                       // empty: nothing to lose
		{dbState{exists: true, objects: 1}, true, ErrNotTarget},                   // not DriftGate's: never dropped
		{dbState{exists: true, objects: 3, marked: true}, false, ErrTargetExists}, // DriftGate's, without replace
		{dbState{exists: true, objects: 3, marked: true}, true, nil},
	} {
		if err := c.st.check("app", c.replace); !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%+v replace=%v: %v, want %v", c.st, c.replace, err, c.want)
		}
	}
}

func TestKeepsOrder(t *testing.T) {
	innodb := &Table{PrimaryKey: []string{"id"}, create: []string{"DROP TABLE IF EXISTS `t`", "CREATE TABLE `t` (\n  `id` int NOT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"}}
	myisam := &Table{PrimaryKey: []string{"id"}, create: []string{"CREATE TABLE `t` (`id` int NOT NULL, PRIMARY KEY (`id`)) ENGINE=MyISAM"}}
	noKey := &Table{create: []string{"CREATE TABLE `t` (`v` int) ENGINE=InnoDB"}}
	for _, c := range []struct {
		driver string
		t      *Table
		want   bool
	}{
		{MySQL, innodb, false},
		{MySQL, myisam, true},
		{MySQL, noKey, true},
		{Postgres, innodb, true},
	} {
		if got := keepsOrder(c.driver, c.t); got != c.want {
			t.Errorf("%s %q: %v", c.driver, c.t.create[len(c.t.create)-1], got)
		}
	}
}
