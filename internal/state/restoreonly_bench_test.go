package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The restore_only tables of the benchmarks: a session table of 200 rows,
// a cache of 1,000 and a job table of 50, next to twenty tracked tables of
// 100 rows.
var benchRestoreOnly = []string{"sessions", "cache", "jobs"}

// benchRestoreOnlyStmts makes the tables of the benchmarks, in SQL that
// MySQL, PostgreSQL and SQLite all take.
func benchRestoreOnlyStmts(serial, text string) []string {
	var stmts []string
	for i := range 20 {
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE small%d (id %s, name %s, v INTEGER)", i, serial, text))
	}
	stmts = append(stmts,
		"CREATE TABLE sessions (id "+serial+", payload "+text+", last_activity INTEGER)",
		"CREATE TABLE cache (id "+serial+", k "+text+", v "+text+", expiration INTEGER)",
		"CREATE TABLE jobs (id "+serial+", queue "+text+", payload "+text+")")
	return stmts
}

// benchRestoreOnlyFill inserts the rows, one statement per row: the same
// SQL in every database.
func benchRestoreOnlyFill(b *testing.B, db *sql.DB) {
	b.Helper()
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := range 20 {
		for r := range 100 {
			if _, err := tx.Exec(fmt.Sprintf("INSERT INTO small%d (name, v) VALUES ('n%d', %d)", i, r, r)); err != nil {
				b.Fatal(err)
			}
		}
	}
	for r := range 1000 {
		q := fmt.Sprintf("INSERT INTO cache (k, v, expiration) VALUES ('key%d', 'a cached value of some length %d', %d)", r, r, r)
		if r < 200 {
			q += fmt.Sprintf("; INSERT INTO sessions (payload, last_activity) VALUES ('a session payload of some length %d', %d)", r, r)
		}
		if r < 50 {
			q += fmt.Sprintf("; INSERT INTO jobs (queue, payload) VALUES ('default', 'job %d')", r)
		}
		for _, s := range splitStatements(q) {
			if _, err := tx.Exec(s); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
}

func splitStatements(q string) []string {
	var out []string
	for len(q) > 0 {
		i := 0
		for i < len(q) && q[i] != ';' {
			i++
		}
		out = append(out, q[:i])
		if i < len(q) {
			i += 2
		}
		q = q[min(i, len(q)):]
	}
	return out
}

// benchRestoreOnlyRun measures the two ways a capture can keep the
// restore_only tables at the pristine rows at each restore: check them
// (RestoreOnlyFingerprint) and restore those that differ, or restore them
// all every time. "unchanged" is a restore where the application wrote
// none of them; "written" one where it wrote a session row. Each includes
// the fingerprint that follows the restore (a restore of DriftGate's own
// makes it read the tables it wrote); "parts" are the check, the restore
// of all of them and a fingerprint alone. Measured on an Apple M4 with
// MySQL 8 and PostgreSQL 16 in Docker (check, restore): SQLite 0.58 ms,
// 0.88 ms; MySQL 5.1 ms, 73 ms; PostgreSQL 17 ms, 53 ms (a restore copies
// the whole database). The capture checks.
func benchRestoreOnlyRun(b *testing.B, c Component, write func()) {
	ctx := context.Background()
	r := c.(RestoreOnly)
	pristine, err := r.RestoreOnlyFingerprint(ctx, true)
	if err != nil {
		b.Fatal(err)
	}
	all := make([]string, 0, len(pristine))
	for it := range pristine {
		all = append(all, it)
	}
	check := func(b *testing.B) {
		cur, err := r.RestoreOnlyFingerprint(ctx, false)
		if err != nil {
			b.Fatal(err)
		}
		if d := Diff(cur, pristine); len(d) > 0 {
			if err := c.Restore(ctx, d); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := c.Fingerprint(ctx, nil); err != nil {
			b.Fatal(err)
		}
	}
	always := func(b *testing.B) {
		if err := c.Restore(ctx, all); err != nil {
			b.Fatal(err)
		}
		if _, err := c.Fingerprint(ctx, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("parts/check", func(b *testing.B) {
		for b.Loop() {
			if _, err := r.RestoreOnlyFingerprint(ctx, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parts/restore", func(b *testing.B) {
		for b.Loop() {
			if err := c.Restore(ctx, all); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parts/fingerprint", func(b *testing.B) {
		for b.Loop() {
			if _, err := c.Fingerprint(ctx, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, way := range []struct {
		name string
		do   func(*testing.B)
	}{{"check", check}, {"always", always}} {
		b.Run(way.name+"/unchanged", func(b *testing.B) {
			way.do(b)
			for b.Loop() {
				way.do(b)
			}
		})
		b.Run(way.name+"/written", func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				write()
				if _, err := c.Fingerprint(ctx, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				way.do(b)
			}
		})
	}
}

func BenchmarkRestoreOnlySQLite(b *testing.B) {
	dir := b.TempDir()
	work := filepath.Join(dir, "app.sqlite")
	app := sqliteApp(b, work)
	for _, q := range append([]string{"PRAGMA journal_mode = WAL"}, benchRestoreOnlyStmts("INTEGER PRIMARY KEY", "TEXT")...) {
		if _, err := app.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	benchRestoreOnlyFill(b, app)
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "pristine.sqlite"), RestoreOnly: benchRestoreOnly})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	benchRestoreOnlyRun(b, s, func() {
		if _, err := app.Exec("INSERT INTO sessions (payload, last_activity) VALUES ('new', 1)"); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkRestoreOnlyMySQL(b *testing.B) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		b.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(v, ",")[0]
	work, pristine := "dgt_rob_work", "dgt_rob_pristine"
	root := mysqlAdmin(b, dsn, "")
	for _, q := range []string{"DROP DATABASE IF EXISTS " + work, "DROP DATABASE IF EXISTS " + pristine, "CREATE DATABASE " + work} {
		if _, err := root.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(b, dsn, work)
	for _, q := range benchRestoreOnlyStmts("INT AUTO_INCREMENT PRIMARY KEY", "VARCHAR(100)") {
		if _, err := db.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	benchRestoreOnlyFill(b, db)
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreOnly: benchRestoreOnly})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Close() })
	if err := m.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	benchRestoreOnlyRun(b, m, func() {
		if _, err := db.Exec("INSERT INTO sessions (payload, last_activity) VALUES ('new', 1)"); err != nil {
			b.Fatal(err)
		}
	})
}

func BenchmarkRestoreOnlyPostgres(b *testing.B) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		b.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	work, pristine := "dgt_rob_work", "dgt_rob_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreOnly: benchRestoreOnly})
	if err != nil {
		b.Fatal(err)
	}
	drop := func() {
		for _, db := range []string{work, pristine, work + "__driftgate_new", work + "__driftgate_old", pristine + "__driftgate_new", pristine + "__driftgate_old"} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
	}
	drop()
	b.Cleanup(func() {
		p.Close()
		drop()
	})
	if _, err := p.admin.Exec("CREATE DATABASE " + work); err != nil {
		b.Fatal(err)
	}
	db, err := p.open(work)
	if err != nil {
		b.Fatal(err)
	}
	for _, q := range benchRestoreOnlyStmts("serial PRIMARY KEY", "text") {
		if _, err := db.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	benchRestoreOnlyFill(b, db)
	db.Close()
	if err := p.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	benchRestoreOnlyRun(b, p, func() {
		db, err := p.open(work)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("INSERT INTO sessions (payload, last_activity) VALUES ('new', 1)"); err != nil {
			b.Fatal(err)
		}
	})
}

// BenchmarkRestoreEmptyPostgres: a restore of restore_empty tables alone
// empties them in place ("empty") instead of copying the whole database
// ("copy"), on the database of benchPostgres with a cache table.
func BenchmarkRestoreEmptyPostgres(b *testing.B) {
	base := benchPostgres(b)
	ctx := context.Background()
	o := base.o
	o.RestoreEmpty = []string{"cache"}
	p, err := NewPostgres(o)
	if err != nil {
		b.Fatal(err)
	}
	defer p.Close()
	db, err := p.open(o.Working)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE cache (id serial PRIMARY KEY, k text, v text)"); err != nil {
		b.Fatal(err)
	}
	db.Close()
	if err := p.Seed(ctx); err != nil {
		b.Fatal(err)
	}
	write := func(b *testing.B) {
		db, err := p.open(o.Working)
		if err != nil {
			b.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("INSERT INTO cache (k, v) SELECT 'k' || g, 'value' FROM generate_series(1, 200) g"); err != nil {
			b.Fatal(err)
		}
	}
	items := []string{"table:public.cache", "sequence:public.cache_id_seq"}
	b.Run("empty", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			write(b)
			b.StartTimer()
			if err := p.Restore(ctx, items); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("copy", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			write(b)
			b.StartTimer()
			if err := p.copyDatabase(ctx, p.o.Pristine, p.o.Working, nil, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
