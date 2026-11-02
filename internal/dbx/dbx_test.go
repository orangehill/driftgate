package dbx

import (
	"context"
	"database/sql"
	"net"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestQuote: the quote character doubled, everything else as it is.
func TestQuote(t *testing.T) {
	for _, c := range []struct{ name, mysql, postgres string }{
		{"users", "`users`", `"users"`},
		{"", "``", `""`},
		{"a`b", "`a``b`", "\"a`b\""},
		{`a"b`, "`a\"b`", `"a""b"`},
		{"``", "``````", "\"``\""},
		{"a.b", "`a.b`", `"a.b"`},
		{"x'; DROP TABLE t; --", "`x'; DROP TABLE t; --`", `"x'; DROP TABLE t; --"`},
		{"Šćepan ђ", "`Šćepan ђ`", `"Šćepan ђ"`},
		{`back\slash`, "`back\\slash`", `"back\slash"`},
	} {
		if got := QuoteMySQL(c.name); got != c.mysql {
			t.Errorf("QuoteMySQL(%q) = %s, want %s", c.name, got, c.mysql)
		}
		if got := QuotePostgres(c.name); got != c.postgres {
			t.Errorf("QuotePostgres(%q) = %s, want %s", c.name, got, c.postgres)
		}
	}
}

func TestIsMariaDB(t *testing.T) {
	for v, want := range map[string]bool{
		"11.4.2-MariaDB-ubu2404":          true,
		"10.6.18-mariadb-log":             true,
		"8.4.0":                           false,
		"8.0.36-0ubuntu0.22.04.1":         false,
		"5.5.5-10.11.6-MariaDB-0+deb12u1": true,
	} {
		if got := IsMariaDB(v); got != want {
			t.Errorf("IsMariaDB(%q) = %v", v, got)
		}
	}
}

// TestMySQLSession: the statements of each caller, in the order a dump
// carries them.
func TestMySQLSession(t *testing.T) {
	data := MySQLSession{UTF8UTC: true, SQLMode: "NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES"}
	want := []string{
		"SET NAMES utf8mb4",
		"SET time_zone = '+00:00'",
		"SET foreign_key_checks = 0",
		"SET unique_checks = 0",
		"SET sql_mode = 'NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES'",
	}
	if got := data.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("data session %q", got)
	}
	state := MySQLSession{FreshStats: true}
	want = []string{"SET foreign_key_checks = 0", "SET unique_checks = 0", "SET sql_mode = ''"}
	if got := state.Statements(); !reflect.DeepEqual(got, want) {
		t.Errorf("state session %q", got)
	}
	if got := (MySQLSession{SQLMode: "a'b"}).Statements()[2]; got != "SET sql_mode = 'a''b'" {
		t.Errorf("quoted mode %s", got)
	}
}

// TestAllowHost: a local socket and the local system by default, not a
// private address; an allow-list replaces the default.
func TestAllowHost(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		lists [][]string
		host  string
		ok    bool
	}{
		{nil, "", true},
		{nil, "localhost", true},
		{nil, "127.0.0.1", true},
		{nil, "::1", true},
		{nil, "10.1.2.3", false},
		{nil, "192.168.1.5", false},
		{[][]string{{"10.0.0.0/8"}}, "10.1.2.3", true},
		{[][]string{{"10.0.0.0/8"}}, "127.0.0.1", false},
		{[][]string{{"10.0.0.0/8"}, {"192.168.0.0/16"}}, "192.168.1.5", true},
	} {
		err := AllowHost(ctx, c.lists, c.host, "x: true")
		if (err == nil) != c.ok {
			t.Errorf("lists %v, host %q: %v", c.lists, c.host, err)
		}
		if err != nil && !strings.HasSuffix(err.Error(), "or set x: true") {
			t.Errorf("hint missing: %v", err)
		}
	}
	if err := AllowHosts(ctx, "postgres", "postgres://u:p@127.0.0.1:5432,db.example.com:5432/db", nil, "x"); err == nil || !strings.Contains(err.Error(), "db.example.com") {
		t.Errorf("a fallback host is checked too: %v", err)
	}
	if err := AllowHosts(ctx, "mysql", "mysql://u:p@127.0.0.1:3306/", nil, "x"); err != nil {
		t.Error(err)
	}
}

func mysqlServers(t *testing.T) []string {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	return strings.Split(v, ",")
}

// TestShowCreate: an object that does not exist is nil with missingOK (the
// state components) and an error without it (the data module).
func TestShowCreate(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testShowCreate(t, dsn) })
	}
}

func testShowCreate(t *testing.T, dsn string) {
	ctx := context.Background()
	db, err := OpenMySQL(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const name = "dgx_show `odd`"
	q := QuoteMySQL(name)
	t.Cleanup(func() { db.Exec("DROP DATABASE IF EXISTS " + q) })
	for _, s := range []string{
		"DROP DATABASE IF EXISTS " + q,
		"CREATE DATABASE " + q,
		"CREATE TABLE " + q + ".`t``1` (id INT PRIMARY KEY)",
		"CREATE VIEW " + q + ".v AS SELECT id FROM " + q + ".`t``1`",
		"CREATE TRIGGER " + q + ".tr BEFORE INSERT ON " + q + ".`t``1` FOR EACH ROW SET NEW.id = NEW.id",
	} {
		if _, err := c.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := (MySQLSession{SQLMode: ""}).Apply(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"TABLE", "VIEW"} {
		obj := map[string]string{"TABLE": "t`1", "VIEW": "v"}[kind]
		def, err := ShowCreateSQL(ctx, c, kind, name, obj, false)
		if err != nil || !strings.HasPrefix(def, "CREATE ") {
			t.Errorf("%s %s: %q %v", kind, obj, def, err)
		}
	}
	tr, err := ShowCreate(ctx, c, "TRIGGER", name, "tr", false)
	if err != nil || !strings.Contains(tr["SQL Original Statement"], "SET NEW.id") {
		t.Errorf("trigger: %v %v", tr, err)
	}
	for _, kind := range []string{"TABLE", "VIEW", "TRIGGER"} {
		got, err := ShowCreate(ctx, c, kind, name, "missing", true)
		if got != nil || err != nil {
			t.Errorf("%s missing, missingOK: %v %v", kind, got, err)
		}
		def, err := ShowCreateSQL(ctx, c, kind, name, "missing", true)
		if def != "" || err != nil {
			t.Errorf("%s missing, missingOK: %q %v", kind, def, err)
		}
		if _, err := ShowCreate(ctx, c, kind, name, "missing", false); err == nil || !strings.Contains(err.Error(), "SHOW CREATE "+kind+" missing") {
			t.Errorf("%s missing, not missingOK: %v", kind, err)
		}
	}

	ok, err := MySQLDatabaseExists(ctx, c, name)
	if err != nil || !ok {
		t.Errorf("exists: %v %v", ok, err)
	}
	if _, _, err := MySQLCharset(ctx, c, "dgx_no_such_db"); err != sql.ErrNoRows {
		t.Errorf("charset of a missing database: %v", err)
	}
	objs, err := MySQLObjects(ctx, c, name)
	if err != nil || !reflect.DeepEqual(objs, []MySQLObject{{"t`1", "BASE TABLE"}, {"v", "VIEW"}}) {
		t.Errorf("objects %v %v", objs, err)
	}
	trs, err := MySQLTriggers(ctx, c, name)
	if err != nil || len(trs) != 1 || trs[0].Name != "tr" || trs[0].Table != "t`1" || trs[0].Timing != "BEFORE" || trs[0].Event != "INSERT" {
		t.Errorf("triggers %+v %v", trs, err)
	}
	if _, err := Flavor(ctx, c); err != nil {
		t.Error(err)
	}
}

// TestPostgresSchemaFilter: both callers skip the system's schemas and
// keep every schema of the database, also one whose name looks like a
// system schema to an unescaped LIKE; only the data module's filter skips
// the objects of an extension.
func TestPostgresSchemaFilter(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	const name = "dgx_schema_filter"
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + QuotePostgres(name) + " WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + QuotePostgres(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + QuotePostgres(name) + " WITH (FORCE)") })
	cs, err := PostgresDSN(dsn, name)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", cs)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		"CREATE SCHEMA pgxtemp",
		"CREATE SCHEMA pgxtoast",
		"CREATE TABLE public.a (id int)",
		"CREATE TABLE pgxtemp.b (id int)",
		"CREATE TABLE pgxtoast.c (id int)",
		"CREATE TEMPORARY TABLE tmp (id int)",    // in pg_temp_N
		"CREATE EXTENSION IF NOT EXISTS pg_trgm", // functions in public
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	list := func(q string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	tables := list(`SELECT n.nspname || '.' || c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND ` + PostgresUserSchema("n.nspname") + ` ORDER BY 1`)
	if want := []string{"pgxtemp.b", "pgxtoast.c", "public.a"}; !reflect.DeepEqual(tables, want) {
		t.Errorf("tables %v, want %v", tables, want)
	}
	// With pg_trgm installed, its functions are user-schema objects, which
	// only the extension filter leaves out.
	const funcs = `SELECT count(*)::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE `
	all := list(funcs + PostgresUserSchema("n.nspname"))
	own := list(funcs + PostgresUserSchema("n.nspname") + " AND " + PostgresNotExtension("p.oid", "pg_proc"))
	if all[0] == "0" || own[0] != "0" {
		t.Errorf("functions: %s in user schemas, %s not of an extension", all[0], own[0])
	}
}

// TestMySQLURLTLS: tls= of a mysql:// URL reaches the connection: encrypted
// with skip-verify, and refused with true when the server's certificate is
// self-signed (as the test servers make it).
func TestMySQLURLTLS(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		if !strings.HasPrefix(dsn, "mysql://") {
			continue
		}
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			plain, err := OpenMySQL(dsn, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer plain.Close()
			var name, cipher string
			if err := plain.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil || cipher != "" {
				t.Fatalf("without tls: cipher %q, %v", cipher, err)
			}
			q := "?tls=skip-verify"
			if u.RawQuery != "" {
				q = "&tls=skip-verify"
			}
			enc, err := OpenMySQL(dsn+q, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			if err := enc.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil {
				t.Skipf("the server has no TLS: %v", err)
			}
			if cipher == "" {
				t.Fatal("tls=skip-verify connected without TLS")
			}
			strict, err := OpenMySQL(strings.Replace(dsn+q, "skip-verify", "true", 1), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer strict.Close()
			if err := strict.PingContext(ctx); err == nil {
				t.Fatal("tls=true accepted a self-signed certificate")
			}
		})
	}
}

// TestMySQLNotMySQL: a server that is not MySQL (here one that sends a
// short first packet, as MySQL's X protocol port does) is an error, not a
// panic of the driver.
func TestMySQLNotMySQL(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte{5, 0, 0, 0, 10, 0, 0, 0, 0}) // protocol 10, then nothing
			c.Close()
		}
	}()
	db, err := OpenMySQL("mysql://u:p@"+l.Addr().String()+"/", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "did not answer as MySQL") {
		t.Fatalf("ping: %v", err)
	}
}
