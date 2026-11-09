package state

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// definerCase is one engine's statements for testDefiner: the two tables,
// a migration and its rollback in migrations, rows the application writes,
// and a column a migration adds.
type definerCase struct {
	create, rollback, rows, column []string
	items, migrations              string // the items of the two tables
}

// testDefiner: Definitions tells a change of a table's definition from
// rows written to it (an AUTO_INCREMENT or identity that moved included);
// Contents tells rows that changed from a migration and its rollback,
// which leave the same rows and a counter moved. Both read the working
// database or the pristine copy. open connects to the working database
// (a seed of PostgreSQL ends the sessions with it).
func testDefiner(t *testing.T, c Component, open func() *sql.DB, k definerCase) {
	t.Helper()
	ctx := context.Background()
	d, ok := c.(Definer)
	if !ok {
		t.Fatalf("%T is no Definer", c)
	}
	mustExec(t, open(), k.create...)
	if err := c.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	db := open()
	both := []string{k.items, k.migrations}
	read := func(f func(context.Context, []string, bool) (Fingerprint, error), pristine bool) Fingerprint {
		t.Helper()
		got, err := f(ctx, append(both, "table:none", "view:x"), pristine)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("items read: %v", got)
		}
		return got
	}
	defs, rows := read(d.Definitions, true), read(d.Contents, true)
	if work := read(d.Definitions, false); len(Diff(work, defs)) != 0 {
		t.Fatalf("definitions right after the seed differ: %v", Diff(work, defs))
	}
	if work := read(d.Contents, false); len(Diff(work, rows)) != 0 {
		t.Fatalf("rows right after the seed differ: %v", Diff(work, rows))
	}
	mustExec(t, db, k.rollback...)
	if work := read(d.Contents, false); len(Diff(work, rows)) != 0 {
		t.Errorf("a migration and its rollback: rows differ: %v", Diff(work, rows))
	}
	mustExec(t, db, k.rows...)
	if got := Diff(read(d.Definitions, false), defs); len(got) != 0 {
		t.Errorf("rows written: definitions differ: %v", got)
	}
	if got := Diff(read(d.Contents, false), rows); len(got) != 1 || got[0] != k.items {
		t.Errorf("rows written: rows differ in %v, want %s", got, k.items)
	}
	mustExec(t, db, k.column...)
	if got := Diff(read(d.Definitions, false), defs); len(got) != 1 || got[0] != k.items {
		t.Errorf("a column: definitions differ in %v, want %s", got, k.items)
	}
	if got := Diff(read(d.Definitions, true), defs); len(got) != 0 {
		t.Errorf("the pristine copy's definitions moved: %v", got)
	}
}

func TestDefinerSQLite(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "app.sqlite")
	db := sqliteApp(t, work)
	mustExec(t, db, "PRAGMA journal_mode = wal")
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "pristine.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testDefiner(t, s, func() *sql.DB { return db }, definerCase{
		create: []string{
			"CREATE TABLE items (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)",
			"CREATE TABLE migrations (id INTEGER PRIMARY KEY AUTOINCREMENT, migration TEXT)",
			"INSERT INTO items (name) VALUES ('a')", "INSERT INTO migrations (migration) VALUES ('one')",
		},
		rollback: []string{"INSERT INTO migrations (migration) VALUES ('two')", "DELETE FROM migrations WHERE migration = 'two'"},
		rows:     []string{"INSERT INTO items (name) VALUES ('b')", "DELETE FROM items WHERE name = 'b'", "UPDATE items SET name = 'z'"},
		column:   []string{"ALTER TABLE items ADD COLUMN slug TEXT"},
		items:    "table:items", migrations: "table:migrations",
	})
}

func TestDefinerMySQL(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			work, pristine := "dgt_def", "dgt_def_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			db := mysqlAdmin(t, dsn, work)
			testDefiner(t, m, func() *sql.DB { return db }, definerCase{
				create: []string{
					"CREATE TABLE items (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(20))",
					"CREATE TABLE migrations (id INT AUTO_INCREMENT PRIMARY KEY, migration VARCHAR(50))",
					"INSERT INTO items (name) VALUES ('a')", "INSERT INTO migrations (migration) VALUES ('one')",
				},
				rollback: []string{"INSERT INTO migrations (migration) VALUES ('two')", "DELETE FROM migrations WHERE migration = 'two'"},
				rows:     []string{"INSERT INTO items (name) VALUES ('b')", "DELETE FROM items WHERE name = 'b'", "UPDATE items SET name = 'z'"},
				column:   []string{"ALTER TABLE items ADD COLUMN slug VARCHAR(20)"},
				items:    "table:items", migrations: "table:migrations",
			})
		})
	}
}

func TestDefinerPostgres(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	work, pristine := "dgt_def", "dgt_def_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mustExec(t, p.admin, "DROP DATABASE IF EXISTS "+work+" WITH (FORCE)", "DROP DATABASE IF EXISTS "+pristine+" WITH (FORCE)", "CREATE DATABASE "+work)
	t.Cleanup(func() {
		p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
		p.admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
	})
	open := func() *sql.DB {
		db, err := p.open(work)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	testDefiner(t, p, open, definerCase{
		create: []string{
			"CREATE TABLE items (id serial PRIMARY KEY, name text)",
			"CREATE TABLE migrations (id serial PRIMARY KEY, migration text)",
			"INSERT INTO items (name) VALUES ('a')", "INSERT INTO migrations (migration) VALUES ('one')",
		},
		rollback: []string{"INSERT INTO migrations (migration) VALUES ('two')", "DELETE FROM migrations WHERE migration = 'two'"},
		rows:     []string{"INSERT INTO items (name) VALUES ('b')", "DELETE FROM items WHERE name = 'b'", "UPDATE items SET name = 'z'"},
		column:   []string{"ALTER TABLE items ADD COLUMN slug text"},
		items:    "table:public.items", migrations: "table:public.migrations",
	})
}

func TestDefinerSQLServer(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	testDefiner(t, s, func() *sql.DB { return db }, definerCase{
		create: []string{
			"CREATE TABLE items (id int IDENTITY PRIMARY KEY, name nvarchar(20))",
			"CREATE TABLE migrations (id int IDENTITY PRIMARY KEY, migration nvarchar(50))",
			"INSERT INTO items (name) VALUES ('a')", "INSERT INTO migrations (migration) VALUES ('one')",
		},
		rollback: []string{"INSERT INTO migrations (migration) VALUES ('two')", "DELETE FROM migrations WHERE migration = 'two'"},
		rows:     []string{"INSERT INTO items (name) VALUES ('b')", "DELETE FROM items WHERE name = 'b'", "UPDATE items SET name = 'z'"},
		column:   []string{"ALTER TABLE items ADD slug nvarchar(20)"},
		items:    "table:dbo.items", migrations: "table:dbo.migrations",
	})
}

// BenchmarkDefinitions is what a capture adds before its first restore
// when the application wrote rows to every table of a schema of 100
// tables since the last capture: the definitions of all of them in the
// working database (the pristine copy's are in the record of the seed).
// With no table changed it reads nothing.
func BenchmarkDefinitions(b *testing.B) {
	const tables = 100
	var create, write, items []string
	for i := range tables {
		create = append(create, fmt.Sprintf("CREATE TABLE t%d (id INTEGER PRIMARY KEY, name VARCHAR(20), n INT)", i),
			fmt.Sprintf("CREATE INDEX t%d_n ON t%d (n)", i, i), fmt.Sprintf("INSERT INTO t%d (id, name, n) VALUES (1, 'a', 1), (2, 'b', 2)", i))
		write = append(write, fmt.Sprintf("INSERT INTO t%d (id, name, n) VALUES (3, 'c', 3)", i))
	}
	run := func(b *testing.B, c Component, db *sql.DB, prefix string) {
		mustExec(b, db, create...)
		if err := c.Seed(context.Background()); err != nil {
			b.Fatal(err)
		}
		mustExec(b, db, write...)
		items = items[:0]
		for i := range tables {
			items = append(items, fmt.Sprintf("table:%st%d", prefix, i))
		}
		d := c.(Definer)
		b.ResetTimer()
		for range b.N {
			f, err := d.Definitions(context.Background(), items, false)
			if err != nil || len(f) != tables {
				b.Fatal(len(f), err)
			}
		}
	}
	b.Run("sqlite", func(b *testing.B) {
		dir := b.TempDir()
		db := sqliteApp(b, filepath.Join(dir, "app.sqlite"))
		mustExec(b, db, "PRAGMA journal_mode = wal")
		s, err := NewSQLite(Options{Name: "db", Working: filepath.Join(dir, "app.sqlite"), Pristine: filepath.Join(dir, "p.sqlite")})
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		run(b, s, db, "")
	})
	b.Run("mysql", func(b *testing.B) {
		dsn := os.Getenv("DRIFTGATE_TEST_MYSQL")
		if dsn == "" {
			b.Skip("DRIFTGATE_TEST_MYSQL not set")
		}
		dsn, _, _ = strings.Cut(dsn, ",")
		root := mysqlAdmin(b, dsn, "")
		mustExec(b, root, "DROP DATABASE IF EXISTS dgt_bdef", "DROP DATABASE IF EXISTS dgt_bdef_p", "CREATE DATABASE dgt_bdef")
		b.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS dgt_bdef"); root.Exec("DROP DATABASE IF EXISTS dgt_bdef_p") })
		m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: "dgt_bdef", Pristine: "dgt_bdef_p"})
		if err != nil {
			b.Fatal(err)
		}
		defer m.Close()
		run(b, m, mysqlAdmin(b, dsn, "dgt_bdef"), "")
	})
	b.Run("postgres", func(b *testing.B) {
		dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
		if dsn == "" {
			b.Skip("DRIFTGATE_TEST_POSTGRES not set")
		}
		p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: "dgt_bdef", Pristine: "dgt_bdef_p"})
		if err != nil {
			b.Fatal(err)
		}
		defer p.Close()
		mustExec(b, p.admin, "DROP DATABASE IF EXISTS dgt_bdef WITH (FORCE)", "DROP DATABASE IF EXISTS dgt_bdef_p WITH (FORCE)", "CREATE DATABASE dgt_bdef")
		b.Cleanup(func() {
			p.admin.Exec("DROP DATABASE IF EXISTS dgt_bdef WITH (FORCE)")
			p.admin.Exec("DROP DATABASE IF EXISTS dgt_bdef_p WITH (FORCE)")
		})
		db, err := p.open("dgt_bdef")
		if err != nil {
			b.Fatal(err)
		}
		mustExec(b, db, create...)
		db.Close()
		if err := p.Seed(context.Background()); err != nil {
			b.Fatal(err)
		}
		if db, err = p.open("dgt_bdef"); err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		mustExec(b, db, write...)
		items = items[:0]
		for i := range tables {
			items = append(items, fmt.Sprintf("table:public.t%d", i))
		}
		b.ResetTimer()
		for range b.N {
			f, err := p.Definitions(context.Background(), items, false)
			if err != nil || len(f) != tables {
				b.Fatal(len(f), err)
			}
		}
	})
}
