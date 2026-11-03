package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sqliteBenchSizes are the database sizes of the benchmarks, in MB; 500 MB
// only with DRIFTGATE_BENCH_LARGE=1.
func sqliteBenchSizes() []int {
	if os.Getenv("DRIFTGATE_BENCH_LARGE") == "1" {
		return []int{1, 50, 500}
	}
	return []int{1, 50}
}

// sqliteBenchFile makes a working file of about mb megabytes in WAL mode:
// one large table with an index (rows of an integer key, an email, a
// number, a real, a DATETIME and a 60-byte blob), twenty small tables of
// 100 rows and an excluded session table; then seeds its pristine file.
func sqliteBenchFile(b *testing.B, mb int) (*SQLite, *sql.DB) {
	b.Helper()
	dir := b.TempDir()
	work := filepath.Join(dir, "app.sqlite")
	app := sqliteApp(b, work)
	rows := mb * 1_000_000 / 190
	stmts := []string{
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE big (id INTEGER PRIMARY KEY, email TEXT, n INTEGER, r REAL, created_at DATETIME, body BLOB)`,
		`CREATE INDEX big_email ON big (email)`,
		fmt.Sprintf(`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < %d)
			INSERT INTO big SELECT i, 'user' || i || '@example.com', i * 7, i * 0.5, datetime(1700000000 + i, 'unixepoch'), randomblob(60) FROM r`, rows),
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY, payload TEXT)`,
	}
	for i := range 20 {
		stmts = append(stmts,
			fmt.Sprintf("CREATE TABLE small%d (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, v INTEGER)", i),
			fmt.Sprintf("WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 100) INSERT INTO small%d (name, v) SELECT 'n' || i, i FROM r", i))
	}
	for _, q := range stmts {
		if _, err := app.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "pristine.sqlite"), Exclude: []string{"sessions"}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	return s, app
}

// BenchmarkSQLiteFingerprint: a full fingerprint when nothing was written
// (reused), when only an excluded table was (every page digest matches),
// when one row of the large table was (it is read again), and with no row
// hash to keep (every table read).
func BenchmarkSQLiteFingerprint(b *testing.B) {
	ctx := context.Background()
	for _, mb := range sqliteBenchSizes() {
		s, app := sqliteBenchFile(b, mb)
		fp := func(b *testing.B) {
			if _, err := s.Fingerprint(ctx, nil); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(fmt.Sprintf("%dMB/reused", mb), func(b *testing.B) {
			fp(b)
			for b.Loop() {
				fp(b)
			}
			if !s.Reused() {
				b.Fatal("not reused")
			}
		})
		b.Run(fmt.Sprintf("%dMB/digest", mb), func(b *testing.B) {
			fp(b)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("INSERT OR REPLACE INTO sessions VALUES (1, '%d')", i))
				b.StartTimer()
				fp(b)
			}
			if s.Reused() || len(s.scanned) > 0 {
				b.Fatalf("reused %v, read %v", s.Reused(), s.scanned)
			}
		})
		b.Run(fmt.Sprintf("%dMB/changed", mb), func(b *testing.B) {
			fp(b)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE big SET n = %d WHERE id = 1", i))
				b.StartTimer()
				fp(b)
			}
			if len(s.scanned) != 1 {
				b.Fatalf("read %v", s.scanned)
			}
		})
		b.Run(fmt.Sprintf("%dMB/rows", mb), func(b *testing.B) {
			for b.Loop() {
				s.forget()
				s.sums = nil
				fp(b)
			}
		})
	}
}

// BenchmarkSQLiteRestore: one changed row of a small table, and (500 MB
// only) one changed row of the large table, which is copied whole.
func BenchmarkSQLiteRestore(b *testing.B) {
	ctx := context.Background()
	for _, mb := range sqliteBenchSizes() {
		s, app := sqliteBenchFile(b, mb)
		b.Run(fmt.Sprintf("%dMB/small", mb), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE small3 SET v = %d WHERE id = 5", -i-1))
				b.StartTimer()
				if err := s.Restore(ctx, []string{"table:small3"}); err != nil {
					b.Fatal(err)
				}
			}
		})
		if mb < 500 {
			continue
		}
		b.Run(fmt.Sprintf("%dMB/large-table", mb), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE big SET n = %d WHERE id = 5", -i-1))
				b.StartTimer()
				if err := s.Restore(ctx, []string{"table:big"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSQLiteSeed: a new pristine file.
func BenchmarkSQLiteSeed(b *testing.B) {
	ctx := context.Background()
	for _, mb := range sqliteBenchSizes() {
		s, _ := sqliteBenchFile(b, mb)
		b.Run(fmt.Sprintf("%dMB", mb), func(b *testing.B) {
			for b.Loop() {
				if err := s.Seed(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func mustExecB(b *testing.B, db *sql.DB, q string) {
	b.Helper()
	if _, err := db.Exec(q); err != nil {
		b.Fatalf("%s: %v", q, err)
	}
}
