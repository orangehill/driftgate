package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
			for range b.N {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("INSERT OR REPLACE INTO sessions VALUES (1, '%d')", sqliteBenchValue.Add(1)))
				b.StartTimer()
				fp(b)
			}
			if s.Reused() || len(s.scanned) > 0 {
				b.Fatalf("reused %v, read %v", s.Reused(), s.scanned)
			}
		})
		b.Run(fmt.Sprintf("%dMB/changed", mb), func(b *testing.B) {
			fp(b)
			for range b.N {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE big SET n = %d WHERE id = 1", sqliteBenchValue.Add(1)))
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

// sqliteLaravelFile makes a working file of about mb megabytes in a
// journal mode, shaped like a Laravel application's: users, a large posts
// table with an index, comments, and the session, cache and queue tables
// (excluded) in the same file; then seeds its pristine file.
func sqliteLaravelFile(b *testing.B, mb int, mode string) (*SQLite, *sql.DB) {
	b.Helper()
	dir := b.TempDir()
	work := filepath.Join(dir, "database.sqlite")
	app := sqliteApp(b, work)
	stmts := []string{
		"PRAGMA journal_mode = " + mode,
		`CREATE TABLE migrations (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, migration VARCHAR NOT NULL, batch INTEGER NOT NULL)`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, name VARCHAR NOT NULL, email VARCHAR NOT NULL, email_verified_at DATETIME, password VARCHAR NOT NULL, remember_token VARCHAR, created_at DATETIME, updated_at DATETIME)`,
		`CREATE UNIQUE INDEX users_email_unique ON users (email)`,
		`CREATE TABLE password_reset_tokens (email VARCHAR NOT NULL, token VARCHAR NOT NULL, created_at DATETIME, PRIMARY KEY (email))`,
		`CREATE TABLE sessions (id VARCHAR NOT NULL, user_id INTEGER, ip_address VARCHAR, user_agent TEXT, payload TEXT NOT NULL, last_activity INTEGER NOT NULL, PRIMARY KEY (id))`,
		`CREATE INDEX sessions_user_id_index ON sessions (user_id)`,
		`CREATE INDEX sessions_last_activity_index ON sessions (last_activity)`,
		`CREATE TABLE cache (key VARCHAR NOT NULL, value TEXT NOT NULL, expiration INTEGER NOT NULL, PRIMARY KEY (key))`,
		`CREATE TABLE cache_locks (key VARCHAR NOT NULL, owner VARCHAR NOT NULL, expiration INTEGER NOT NULL, PRIMARY KEY (key))`,
		`CREATE TABLE jobs (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, queue VARCHAR NOT NULL, payload TEXT NOT NULL, attempts INTEGER NOT NULL, reserved_at INTEGER, available_at INTEGER NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE failed_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, uuid VARCHAR NOT NULL, connection TEXT NOT NULL, queue TEXT NOT NULL, payload TEXT NOT NULL, exception TEXT NOT NULL, failed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE posts (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, user_id INTEGER NOT NULL, title VARCHAR NOT NULL, body TEXT NOT NULL, created_at DATETIME, updated_at DATETIME)`,
		`CREATE INDEX posts_user_id_index ON posts (user_id)`,
		`CREATE TABLE comments (id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL, post_id INTEGER NOT NULL, body TEXT NOT NULL, created_at DATETIME, updated_at DATETIME)`,
		`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 40) INSERT INTO migrations (migration, batch) SELECT 'm_' || i, 1 FROM r`,
		`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 2000) INSERT INTO users (name, email, password, created_at, updated_at) SELECT 'User ' || i, 'u' || i || '@example.com', '$2y$12$' || hex(randomblob(26)), datetime('now'), datetime('now') FROM r`,
		fmt.Sprintf(`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < %d) INSERT INTO posts (user_id, title, body, created_at, updated_at) SELECT i %% 2000 + 1, 'Title ' || i, hex(randomblob(300)), datetime('now'), datetime('now') FROM r`, mb*1_000_000/700),
		`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 5000) INSERT INTO comments (post_id, body, created_at, updated_at) SELECT i, hex(randomblob(100)), datetime('now'), datetime('now') FROM r`,
		`INSERT INTO sessions VALUES ('s1', 1, '127.0.0.1', 'Mozilla', 'x', 0)`,
	}
	for _, q := range stmts {
		if _, err := app.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: filepath.Join(dir, "pristine.sqlite"),
		Exclude: []string{"sessions", "cache", "cache_locks", "jobs", "failed_jobs"}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.Seed(context.Background()); err != nil {
		b.Fatal(err)
	}
	return s, app
}

// BenchmarkSQLiteLaravel: the fingerprint after one request of a Laravel
// application, which writes its session (and every third time its cache)
// in the same file, and only that, or also inserts a comment or updates a
// post.
func BenchmarkSQLiteLaravel(b *testing.B) {
	ctx := context.Background()
	for _, mode := range []string{"WAL", "DELETE"} {
		for _, mb := range sqliteBenchSizes() {
			s, app := sqliteLaravelFile(b, mb, mode)
			for _, c := range []struct{ name, write string }{
				{"session", ""},
				{"comment", "INSERT INTO comments (post_id, body) VALUES (%d, 'new')"},
				{"post", "UPDATE posts SET title = 't%d' WHERE id = 7"},
			} {
				b.Run(fmt.Sprintf("%s/%dMB/%s", mode, mb, c.name), func(b *testing.B) {
					if _, err := s.Fingerprint(ctx, nil); err != nil {
						b.Fatal(err)
					}
					for range b.N {
						b.StopTimer()
						v := sqliteBenchValue.Add(1)
						stmts := []string{fmt.Sprintf("UPDATE sessions SET payload = '%s', last_activity = %d WHERE id = 's1'", strings.Repeat("a", 1200), v)}
						if v%3 == 0 {
							stmts = append(stmts, fmt.Sprintf("INSERT INTO cache VALUES ('k', '%d', 1) ON CONFLICT (key) DO UPDATE SET value = excluded.value", v))
						}
						if c.write != "" {
							stmts = append(stmts, fmt.Sprintf(c.write, v))
						}
						mustExecB(b, app, "BEGIN")
						mustExecB(b, app, stmts...)
						mustExecB(b, app, "COMMIT")
						b.StartTimer()
						if _, err := s.Fingerprint(ctx, nil); err != nil {
							b.Fatal(err)
						}
					}
					if s.Reused() {
						b.Fatal("reused")
					}
				})
			}
		}
	}
}

// BenchmarkSQLiteRestoreCycle: what the capture does for a request that
// changed a small table: restore it, fingerprint it to check the restore,
// and take the full fingerprint before the next request.
func BenchmarkSQLiteRestoreCycle(b *testing.B) {
	ctx := context.Background()
	for _, mb := range sqliteBenchSizes() {
		s, app := sqliteBenchFile(b, mb)
		b.Run(fmt.Sprintf("%dMB", mb), func(b *testing.B) {
			for range b.N {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE small3 SET v = %d WHERE id = 5", -sqliteBenchValue.Add(1)))
				b.StartTimer()
				if err := s.Restore(ctx, []string{"table:small3"}); err != nil {
					b.Fatal(err)
				}
				if _, err := s.Fingerprint(ctx, []string{"table:small3"}); err != nil {
					b.Fatal(err)
				}
				if _, err := s.Fingerprint(ctx, nil); err != nil {
					b.Fatal(err)
				}
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
			for range b.N {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE small3 SET v = %d WHERE id = 5", -sqliteBenchValue.Add(1)))
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
			for range b.N {
				b.StopTimer()
				mustExecB(b, app, fmt.Sprintf("UPDATE big SET n = %d WHERE id = 5", -sqliteBenchValue.Add(1)))
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

// sqliteBenchValue is a value no write of a benchmark wrote before, in any
// run of -count: a write of the same value would change no page.
var sqliteBenchValue atomic.Int64

func mustExecB(b *testing.B, db *sql.DB, stmts ...string) {
	b.Helper()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
}
