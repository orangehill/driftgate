package state

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMySQLDropPristine: the copy seed made is dropped, the working
// database stays, and a database without the mark is refused.
func TestMySQLDropPristine(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_clean_work", "dgt_clean_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work,
				"CREATE TABLE "+work+".news (id INT PRIMARY KEY)")
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if was, err := m.DropPristine(ctx); was || err != nil {
				t.Fatalf("no copy: %v %v", was, err)
			}
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			// Another project's working database with the same pristine
			// name: the copy is not its own.
			other, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: "dgt_clean_other", Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if _, err := other.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "not marked as the copy of dgt_clean_other") {
				t.Fatalf("another project's copy: %v", err)
			}
			// A mark of an earlier release names no working database.
			mustExec(t, root, "UPDATE "+pristine+"."+pristineTable+" SET note = 'DriftGate pristine copy, made by driftgate seed and replaced by the next one'")
			if _, err := m.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "earlier release") {
				t.Fatalf("an earlier release's mark: %v", err)
			}
			mustExec(t, root, "UPDATE "+pristine+"."+pristineTable+" SET note = '"+mysqlMarkNote(work)+"'")
			if was, err := m.DropPristine(ctx); !was || err != nil {
				t.Fatalf("drop: %v %v", was, err)
			}
			var n int
			root.QueryRow("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME IN (?, ?)", work, pristine).Scan(&n)
			if n != 1 {
				t.Fatalf("%d of the two databases left; want the working one", n)
			}
			mustExec(t, root, "CREATE DATABASE "+pristine, "CREATE TABLE "+pristine+".orders (id INT PRIMARY KEY)")
			if _, err := m.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "not removed") {
				t.Fatalf("a database seed did not make: %v", err)
			}
		})
	}
}

// TestMySQLMarkCharset: the mark names a working database whose name is
// not Latin-1 also in a latin1 database: in a column of the database's
// charset strict mode refuses it, and otherwise it is stored as ? and
// clean no longer finds this working database in it.
func TestMySQLMarkCharset(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_работа", "dgt_работа_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS `"+work+"`", "DROP DATABASE IF EXISTS `"+pristine+"`",
				"CREATE DATABASE `"+work+"` CHARACTER SET latin1", "CREATE TABLE `"+work+"`.news (id INT PRIMARY KEY)")
			t.Cleanup(func() {
				root.Exec("DROP DATABASE IF EXISTS `" + work + "`")
				root.Exec("DROP DATABASE IF EXISTS `" + pristine + "`")
			})
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if was, err := m.DropPristine(ctx); !was || err != nil {
				t.Fatalf("drop: %v %v", was, err)
			}
		})
	}
}

// TestPostgresDropPristine: as TestMySQLDropPristine.
func TestPostgresDropPristine(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	work, pristine := "dgt_clean_work", "dgt_clean_pristine"
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
	if was, err := p.DropPristine(ctx); was || err != nil {
		t.Fatalf("no copy: %v %v", was, err)
	}
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	other, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: "dgt_clean_other", Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "not marked as the copy of dgt_clean_other") {
		t.Fatalf("another project's copy: %v", err)
	}
	if was, err := p.DropPristine(ctx); !was || err != nil {
		t.Fatalf("drop: %v %v", was, err)
	}
	if ok, _ := p.exists(ctx, work); !ok {
		t.Fatal("the working database is gone")
	}
	mustExec(t, p.admin, "CREATE DATABASE "+pristine)
	if _, err := p.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "not removed") {
		t.Fatalf("a database seed did not make: %v", err)
	}
}

// TestSQLServerDropPristine: the copy seed made of this working database
// is dropped and the working database stays; a copy of another working
// database, one that is not ONLINE and a database without the mark are
// refused and kept.
func TestSQLServerDropPristine(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newSQLServerSeeded(t, Options{}, "CREATE TABLE news (id INT PRIMARY KEY)")
	master := sqlServerTestDB(t, sqlServerTestDSN(t), "")
	exists := func(db string) bool {
		var n int
		if err := master.QueryRow("SELECT COUNT(*) FROM sys.databases WHERE name = @p1", db).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	// Another working database with the same pristine name.
	otherWork := sqlServerTestName()
	sqlServerExec(t, master, "CREATE DATABASE "+qms(otherWork))
	t.Cleanup(func() { dropSQLServerDatabase(master, otherWork) })
	other, err := NewSQLServer(Options{Name: "db", DSN: s.o.DSN, Working: otherWork, Pristine: s.o.Pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "not removed") || !exists(s.o.Pristine) {
		t.Fatalf("another working database's copy: %v", err)
	}

	// Not ONLINE: DriftGate cannot see whether it made it.
	s.closePool(s.o.Pristine)
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Pristine)+" SET OFFLINE WITH ROLLBACK IMMEDIATE")
	if _, err := s.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "OFFLINE") || !exists(s.o.Pristine) {
		t.Fatalf("an OFFLINE copy: %v", err)
	}
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Pristine)+" SET ONLINE")

	if was, err := s.DropPristine(ctx); !was || err != nil || exists(s.o.Pristine) || !exists(s.o.Working) {
		t.Fatalf("drop: %v %v", was, err)
	}
	if was, err := s.DropPristine(ctx); was || err != nil {
		t.Fatalf("no copy: %v %v", was, err)
	}

	// A database of the pristine name seed did not make.
	sqlServerExec(t, master, "CREATE DATABASE "+qms(s.o.Pristine))
	sqlServerExec(t, sqlServerTestDB(t, s.o.DSN, s.o.Pristine), "CREATE TABLE orders (id INT PRIMARY KEY)")
	if _, err := s.DropPristine(ctx); err == nil || !strings.Contains(err.Error(), "no mark") || !exists(s.o.Pristine) {
		t.Fatalf("a database seed did not make: %v", err)
	}
}
