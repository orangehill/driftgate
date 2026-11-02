package state

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// benchPostgres makes a database of 60 tables with a function each, of
// DRIFTGATE_BENCH_PG_ROWS rows per table (default 2000: about 22 MB), and
// seeds it.
func benchPostgres(b *testing.B) *Postgres {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		b.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	rows := 2000
	if n, err := strconv.Atoi(os.Getenv("DRIFTGATE_BENCH_PG_ROWS")); err == nil {
		rows = n
	}
	work, pristine := "dgt_bench", "dgt_bench_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
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
	defer db.Close()
	for i := 0; i < 60; i++ {
		for _, q := range []string{
			fmt.Sprintf("CREATE TABLE t%d (id serial PRIMARY KEY, s text, n int)", i),
			fmt.Sprintf("INSERT INTO t%d (s, n) SELECT md5(g::text), g FROM generate_series(1, %d) g", i, rows),
			fmt.Sprintf("CREATE FUNCTION f%d(a int) RETURNS int AS 'SELECT a + 1' LANGUAGE sql", i),
		} {
			if _, err := db.Exec(q); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := p.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	return p
}

// BenchmarkPostgresRestore restores the whole database after the
// application wrote a row and the database was fingerprinted, as between
// two requests of a capture; /check also fingerprints the restored
// database, as a capture does to check the restore.
func BenchmarkPostgresRestore(b *testing.B) {
	p := benchPostgres(b)
	ctx := context.Background()
	for _, check := range []bool{false, true} {
		b.Run(map[bool]string{false: "restore", true: "check"}[check], func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				db, err := p.open(p.o.Working)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := db.Exec("UPDATE t1 SET n = n + 1 WHERE id = 1"); err != nil {
					b.Fatal(err)
				}
				db.Close()
				if _, err := p.Fingerprint(ctx, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := p.Restore(ctx, nil); err != nil {
					b.Fatal(err)
				}
				if check {
					if _, err := p.Fingerprint(ctx, nil); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkPostgresFingerprint(b *testing.B) {
	p := benchPostgres(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.Fingerprint(ctx, nil); err != nil {
			b.Fatal(err)
		}
	}
}
