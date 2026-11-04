package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
)

// ssLog is a log a test reads (the component writes to it from one
// goroutine at a time, the test reads it after).
type ssLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *ssLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *ssLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fellBack reports whether the log says a restore restored the whole
// database, and empties it.
func (l *ssLog) fellBack() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok := strings.Contains(l.b.String(), "restoring the whole database")
	l.b.Reset()
	return ok
}

// newSQLServerSeeded is newSQLServerTest with a log, the schema made by
// setup, and a seed; what a seed and a restore make next to the pristine
// copy is dropped at the end too.
func newSQLServerSeeded(t testing.TB, o Options, setup ...string) (*SQLServer, *sql.DB, *ssLog) {
	t.Helper()
	log := &ssLog{}
	o.Log = log
	s, db := newSQLServerTest(t, o)
	cleanSQLServerSeed(t, s)
	sqlServerExec(t, db, setup...)
	if err := s.Seed(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s, db, log
}

// cleanSQLServerSeed drops, at the end of a test, the databases a seed or
// restore of s may leave and, as far as the login may, the backup files.
func cleanSQLServerSeed(t testing.TB, s *SQLServer) {
	master := sqlServerTestDB(t, sqlServerTestDSN(t), "") // the test server's login: it may delete files
	t.Cleanup(func() {
		dropSQLServerDatabase(master, s.o.Pristine+"_dgnew")
		dropSQLServerDatabase(master, s.o.Pristine+"_dgx")
		var data, backup sql.NullString
		master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512)), CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(512))").Scan(&data, &backup)
		p := &ssPaths{data: data.String, log: data.String, backup: backup.String}
		if p.backup == "" {
			p.backup = p.data
		}
		for _, f := range s.backupSlots(p) {
			master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(f))
		}
	})
}

// pristineSQLServer fingerprints the pristine copy.
func pristineSQLServer(t testing.TB, s *SQLServer) Fingerprint {
	t.Helper()
	ctx := context.Background()
	c, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fp, err := s.fingerprint(ctx, c, s.o.Pristine, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fp.f
}

// restoreSQLServer restores what changed as the tracker does (the items
// that differ from the pristine fingerprint p) and checks that the working
// database then fingerprints as p, by the component and the long way.
// It reports whether the whole database was restored. db, the
// application's pool, gets new connections after such a restore.
func restoreSQLServer(t testing.TB, s *SQLServer, db *sql.DB, log *ssLog, p Fingerprint) bool {
	t.Helper()
	ctx := context.Background()
	items := Diff(mustSQLServerFingerprint(t, s), p)
	if len(items) == 0 {
		t.Fatal("nothing changed")
	}
	if err := s.Restore(ctx, items); err != nil {
		t.Fatalf("restore of %v: %v", items, err)
	}
	whole := log.fellBack()
	if whole && db != nil {
		// The server ended the application's sessions: each idle
		// connection of its pool would fail once.
		db.SetMaxIdleConns(0)
		db.SetMaxIdleConns(2)
	}
	got, err := s.Fingerprint(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(got, p.Subset(items)); len(d) > 0 {
		t.Fatalf("after the restore of %v, %v differ (whole database: %t)", items, d, whole)
	}
	if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
		t.Fatalf("after the restore of %v, %v differ read the long way (whole database: %t)", items, d, whole)
	}
	return whole
}

func sqlServerInt(t testing.TB, db *sql.DB, q string) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n.Int64
}

// nextIdentity inserts a row into table (DEFAULT VALUES) in a transaction
// it rolls back and returns the identity value it got.
func nextIdentity(t testing.TB, db *sql.DB, table string) int64 {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow("INSERT INTO " + table + " DEFAULT VALUES; SELECT CAST(SCOPE_IDENTITY() AS bigint)").Scan(&id); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return id
}

// The schema of the restore tests: tables with identity columns in every
// state, a sequence of every kind, foreign keys with cascades, triggers
// that would write if they fired, a database DDL trigger that refuses
// ALTER TABLE, constraints trusted, not trusted and disabled.
var sqlServerRestoreSchema = []string{
	"CREATE SCHEMA app",
	"CREATE TABLE audit (id int IDENTITY PRIMARY KEY, what nvarchar(100) NULL)",
	"CREATE TABLE app.users (id int IDENTITY(10, 5) PRIMARY KEY, name nvarchar(50) COLLATE Latin1_General_CI_AS NULL, score float NULL, rv rowversion, total AS (id * 2))",
	"CREATE TABLE orders (id int IDENTITY PRIMARY KEY, user_id int NULL CONSTRAINT fk_orders_users REFERENCES app.users(id) ON DELETE CASCADE, note nvarchar(max) NULL)",
	"CREATE TABLE lines (id int PRIMARY KEY, order_id int NULL CONSTRAINT fk_lines_orders REFERENCES orders(id) ON DELETE CASCADE)",
	"CREATE TABLE heap (a int NULL, b varchar(10) NULL)",
	"CREATE TABLE fresh (id int IDENTITY(10, 5) PRIMARY KEY, v int NULL)",
	"CREATE TABLE reseeded (id int IDENTITY(1, 1) PRIMARY KEY, v int NULL)",
	"DBCC CHECKIDENT ('reseeded', RESEED, 100) WITH NO_INFOMSGS",
	"CREATE TABLE emptied (id int IDENTITY(1, 1) PRIMARY KEY, v int NULL)",
	"INSERT INTO emptied (v) VALUES (1), (2), (3)",
	"DELETE FROM emptied",
	"CREATE TABLE down (id int IDENTITY(-1, -1) PRIMARY KEY, v int NULL)",
	"INSERT INTO down (v) VALUES (1), (2)",
	"CREATE TABLE checked (id int PRIMARY KEY, v int NULL CONSTRAINT ck_v CHECK (v > 0), w int NULL CONSTRAINT ck_w CHECK (w > 0), x int NULL CONSTRAINT ck_x CHECK (x > 0))",
	"INSERT INTO checked VALUES (1, 1, 1, 1)",
	"ALTER TABLE checked NOCHECK CONSTRAINT ck_w",
	"INSERT INTO checked VALUES (2, 1, -1, 1)",
	"ALTER TABLE checked CHECK CONSTRAINT ck_w", // on, not trusted, with a row that breaks it
	"ALTER TABLE checked NOCHECK CONSTRAINT ck_x",
	"CREATE SEQUENCE app.seq AS bigint START WITH 100 INCREMENT BY 10 CACHE 50",
	"CREATE SEQUENCE unused AS int START WITH 7",
	"CREATE SEQUENCE cycle AS tinyint START WITH 1 MINVALUE 1 MAXVALUE 3 CYCLE NO CACHE",
	"CREATE SEQUENCE ending AS tinyint START WITH 253 MAXVALUE 255",
	"DECLARE @x bigint = NEXT VALUE FOR app.seq; SET @x = NEXT VALUE FOR app.seq",
	"DECLARE @x tinyint = NEXT VALUE FOR cycle; SET @x = NEXT VALUE FOR cycle",
	"DECLARE @x tinyint = NEXT VALUE FOR ending; SET @x = NEXT VALUE FOR ending",
	"INSERT INTO app.users (name, score) VALUES (N'ann', 1.5), (N'bob', -0e0), (N'cy', NULL)",
	"INSERT INTO orders (user_id, note) VALUES (10, N'a'), (15, REPLICATE(CAST(N'x' AS nvarchar(max)), 9000)), (NULL, NULL)",
	"INSERT INTO lines VALUES (1, 1), (2, 2)",
	"INSERT INTO heap VALUES (1, 'a'), (1, 'a'), (NULL, NULL)",
	"CREATE TRIGGER app.users_audit ON app.users AFTER INSERT, UPDATE, DELETE AS INSERT INTO audit (what) VALUES (N'users')",
	"CREATE TRIGGER orders_audit ON orders AFTER INSERT, DELETE AS INSERT INTO audit (what) VALUES (N'orders')",
	"CREATE TRIGGER heap_off ON heap AFTER INSERT AS INSERT INTO audit (what) VALUES (N'heap')",
	"DISABLE TRIGGER heap_off ON heap",
	"CREATE TRIGGER no_alter ON DATABASE FOR ALTER_TABLE, ALTER_SEQUENCE AS BEGIN RAISERROR (N'no ALTER here', 16, 1); ROLLBACK; END",
}

// sqlServerFlags reads the flags a restore turns off and on again.
func sqlServerFlags(t testing.TB, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT name + ' ' + CAST(is_disabled AS varchar(1)) + CAST(is_not_trusted AS varchar(1)) FROM sys.foreign_keys
		UNION ALL SELECT name + ' ' + CAST(is_disabled AS varchar(1)) + CAST(is_not_trusted AS varchar(1)) FROM sys.check_constraints
		UNION ALL SELECT name + ' ' + CAST(is_disabled AS varchar(1)) FROM sys.triggers ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// TestSQLServerRestoreItems: every kind of change to tables and sequences
// is restored item by item, without firing a trigger or running a
// cascade, with every constraint's and trigger's flags as they were, and
// identity columns and sequences hand out what the pristine copy's would.
func TestSQLServerRestoreItems(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{}, sqlServerRestoreSchema...)
	p := pristineSQLServer(t, s)
	if d := Diff(mustSQLServerFingerprint(t, s), p); len(d) > 0 {
		t.Fatalf("after the seed the working database and the pristine copy differ in %v", d)
	}
	flags := sqlServerFlags(t, db)
	audit := sqlServerInt(t, db, "SELECT COUNT(*) FROM audit")
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"insert", []string{"INSERT INTO app.users (name) VALUES (N'dee')", "INSERT INTO orders (user_id) VALUES (20)"}},
		{"update", []string{"UPDATE app.users SET name = N'ANN' WHERE id = 10", "UPDATE orders SET note = note + N'y' WHERE id = 2"}},
		{"same value", []string{"UPDATE app.users SET name = name WHERE id = 15"}},
		{"delete with cascade", []string{"DELETE FROM app.users WHERE id = 10"}},
		{"truncate and refill", []string{"DELETE FROM lines", "INSERT INTO lines VALUES (3, NULL)"}},
		{"heap duplicates", []string{"DELETE TOP (1) FROM heap WHERE a = 1", "INSERT INTO heap VALUES (2, 'b')"}},
		{"rolled back insert", []string{"BEGIN TRAN; INSERT INTO app.users (name) VALUES (N'x'); ROLLBACK"}},
		{"fresh identity used", []string{"INSERT INTO fresh (v) VALUES (1), (2)"}},
		{"reseeded identity used", []string{"INSERT INTO reseeded (v) VALUES (1)"}},
		{"emptied identity used", []string{"INSERT INTO emptied (v) VALUES (1)"}},
		{"descending identity", []string{"INSERT INTO down (v) VALUES (3)", "DELETE FROM down WHERE id = -1"}},
		{"reseed", []string{"DBCC CHECKIDENT ('down', RESEED, -50) WITH NO_INFOMSGS"}},
		{"untrusted check", []string{"DELETE FROM checked WHERE id = 1"}},
		{"sequences", []string{"DECLARE @a bigint = NEXT VALUE FOR app.seq, @b int = NEXT VALUE FOR unused, @c tinyint = NEXT VALUE FOR cycle, @d tinyint = NEXT VALUE FOR ending"}},
		{"cycled", []string{"DECLARE @c tinyint = NEXT VALUE FOR cycle; SET @c = NEXT VALUE FOR cycle"}}, // past the maximum
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlServerExec(t, db, tc.stmts...)
			if restoreSQLServer(t, s, db, log, p) {
				t.Fatal("restored the whole database")
			}
			if got := sqlServerFlags(t, db); got != flags {
				t.Fatalf("flags\n%s\nwant\n%s", got, flags)
			}
			if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM audit"); n != audit {
				t.Fatalf("triggers fired: %d audit rows, want %d", n, audit)
			}
		})
	}

	// What identity columns and sequences hand out next is the pristine
	// copy's (each probe changes the database; a restore follows).
	for _, tc := range []struct {
		table string
		want  int64
	}{{"app.users", 25}, {"fresh", 10}, {"reseeded", 100}, {"emptied", 4}, {"down", -3}, {"orders", 4}} {
		if got := nextIdentity(t, db, tc.table); got != tc.want {
			t.Errorf("%s: next identity %d, want %d", tc.table, got, tc.want)
		}
		restoreSQLServer(t, s, db, log, p)
	}
	for _, tc := range []struct {
		seq  string
		want int64
	}{{"app.seq", 120}, {"unused", 7}, {"cycle", 3}, {"ending", 255}} {
		if got := sqlServerInt(t, db, "SELECT CAST(NEXT VALUE FOR "+tc.seq+" AS bigint)"); got != tc.want {
			t.Errorf("%s: next value %d, want %d", tc.seq, got, tc.want)
		}
		if restoreSQLServer(t, s, db, log, p) {
			t.Fatal("restored the whole database")
		}
	}
	if _, err := db.Exec("DECLARE @d tinyint = NEXT VALUE FOR ending; SET @d = NEXT VALUE FOR ending"); err == nil {
		t.Error("the restored sequence ending is not exhausted after 255")
	}
	restoreSQLServer(t, s, db, log, p)
}

// TestSQLServerRestoreIdentityFallback: an identity column that only a
// TRUNCATE could set back, on a table a foreign key refers to, makes the
// restore restore the whole database.
func TestSQLServerRestoreIdentityFallback(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{},
		"CREATE TABLE parent (id int IDENTITY PRIMARY KEY, v int NULL)",
		"CREATE TABLE child (id int PRIMARY KEY, parent_id int NULL REFERENCES parent(id))")
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO parent (v) VALUES (1)")
	if !restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored item by item")
	}
	if got := nextIdentity(t, db, "parent"); got != 1 {
		t.Fatalf("next identity %d, want 1", got)
	}
}

// TestSQLServerRestoreAtomic: a restore stopped right before its COMMIT
// leaves the working database as it was, and the next one restores it.
func TestSQLServerRestoreAtomic(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{}, sqlServerRestoreSchema...)
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO app.users (name) VALUES (N'dee')", "DELETE FROM lines", "DECLARE @a bigint = NEXT VALUE FOR app.seq")
	before := sqlServerFull(t, s)
	items := Diff(before, p)
	type stop struct{}
	chaos.At = func(point string) {
		if point == "state.sqlserver.restore" {
			panic(stop{})
		}
	}
	func() {
		defer func() {
			chaos.At = nil
			if r := recover(); r == nil {
				t.Fatal("the restore passed the chaos point")
			} else if _, ok := r.(stop); !ok {
				panic(r)
			}
		}()
		s.Restore(context.Background(), items)
	}()
	if d := Diff(sqlServerFull(t, s), before); len(d) > 0 {
		t.Fatalf("a restore stopped before COMMIT changed %v", d)
	}
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
}

// TestSQLServerRestoreDelta: a large table restored by its changed rows
// ends as it does copied whole, and its unchanged rows are not written.
func TestSQLServerRestoreDelta(t *testing.T) {
	defer func(n int64) { sqlServerDeltaRows = n }(sqlServerDeltaRows)
	sqlServerDeltaRows = 1000
	s, db, log := newSQLServerSeeded(t, Options{},
		"CREATE TABLE big (k nvarchar(20) COLLATE Latin1_General_CI_AS NOT NULL PRIMARY KEY, id int IDENTITY, v float NULL, s varchar(20) NULL, rv rowversion)",
		`WITH n AS (SELECT TOP (3000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects a CROSS JOIN sys.all_objects b)
		 INSERT INTO big (k, v, s) SELECT CONCAT(N'key', i), i * 1.5, CASE WHEN i % 7 = 0 THEN NULL ELSE CONCAT('s', i) END FROM n`,
		"CREATE TABLE refs (id int PRIMARY KEY, k nvarchar(20) COLLATE Latin1_General_CI_AS NULL REFERENCES big(k))",
		"INSERT INTO refs VALUES (1, N'key5')")
	p := pristineSQLServer(t, s)
	changes := []string{
		"UPDATE big SET v = -1e0 * 0e0 WHERE k = N'key1'", // -0 from 1.5: only the bytes differ
		"UPDATE big SET s = 'S2' WHERE k = N'key2'",
		"UPDATE big SET s = 's3 ' WHERE k = N'key3'",
		"UPDATE big SET k = N'KEY4' WHERE k = N'key4'", // the same key under the collation
		"DELETE FROM big WHERE k = N'key6'",
		"INSERT INTO big (k, v) VALUES (N'new', 1)",
		"UPDATE big SET s = NULL WHERE k = N'key8'",
	}
	// Rows written after now have a larger rowversion than any now.
	var mark []byte
	written := func() int64 {
		return sqlServerInt(t, db, fmt.Sprintf("SELECT COUNT(*) FROM big WHERE rv > 0x%x", mark))
	}
	db.QueryRow("SELECT MAX(rv) FROM big").Scan(&mark)
	sqlServerExec(t, db, changes...)
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
	if n := written(); n > 14 {
		t.Fatalf("the restore of 7 changed rows wrote %d", n)
	}
	delta := sqlServerFull(t, s)

	sqlServerDeltaRows = 1 << 40 // copied whole
	db.QueryRow("SELECT MAX(rv) FROM big").Scan(&mark)
	sqlServerExec(t, db, changes...)
	restoreSQLServer(t, s, db, log, p)
	if d := Diff(sqlServerFull(t, s), delta); len(d) > 0 {
		t.Fatalf("restored by rows and whole, %v differ", d)
	}
	if n := written(); n < 2900 {
		t.Fatalf("copied whole, only %d rows written", n)
	}
}

// TestSQLServerRestoreDeltaWrite: a write between the comparison that
// finds a large table's changed rows and the restore of those rows waits
// for the restore (the table is locked from before the comparison), so it
// is never left behind as a change the restore missed.
func TestSQLServerRestoreDeltaWrite(t *testing.T) {
	defer func(n int64) { sqlServerDeltaRows = n }(sqlServerDeltaRows)
	sqlServerDeltaRows = 100
	s, db, log := newSQLServerSeeded(t, Options{},
		"CREATE TABLE big (id int PRIMARY KEY, v int NULL)",
		"INSERT INTO big SELECT TOP (1000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)), 0 FROM sys.all_objects")
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "UPDATE big SET v = 1 WHERE id = 1")
	var during error
	sqlServerAfterDiff = func() {
		_, during = db.Exec("SET LOCK_TIMEOUT 0; UPDATE big SET v = 2 WHERE id = 2; SET LOCK_TIMEOUT -1")
	}
	defer func() { sqlServerAfterDiff = nil }()
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
	if sqlServerNumber(during) != 1222 {
		t.Fatalf("a write during the restore: %v, want a lock timeout", during)
	}
}

// TestSQLServerDeltaCompare: the comparison that finds a delta's changed
// rows tells apart every two rows the encoding does (key columns too,
// which the join matches under their collation), and no two equal rows.
func TestSQLServerDeltaCompare(t *testing.T) {
	s, db := newSQLServerTest(t, Options{}, "COLLATE SQL_Latin1_General_CP1_CI_AS")
	type pair struct {
		name, columns string
		a, b          string
	}
	var pairs []pair
	for _, tc := range sqlServerEncodingPairs {
		if len(tc.a) == 1 && len(tc.b) == 1 {
			pairs = append(pairs, pair{tc.name, "dg_id int NOT NULL PRIMARY KEY, " + tc.columns, "1, " + tc.a[0], "1, " + tc.b[0]})
		}
	}
	pairs = append(pairs,
		pair{"key case", "k varchar(10) NOT NULL PRIMARY KEY, v int NULL", "'a', 1", "'A', 1"},
		pair{"key trailing space", "k nvarchar(10) NOT NULL PRIMARY KEY", "N'a'", "N'a '"},
		pair{"key accent", "k nchar(4) COLLATE Latin1_General_CI_AI NOT NULL PRIMARY KEY", "N'a'", "N'á'"},
		pair{"key trailing zero byte", "k varbinary(10) NOT NULL PRIMARY KEY", "0x01", "0x0100"},
		pair{"key datetimeoffset", "k datetimeoffset NOT NULL PRIMARY KEY", "'2020-01-01T10:00:00+02:00'", "'2020-01-01T08:00:00+00:00'"},
		pair{"key float -0", "k float NOT NULL PRIMARY KEY", "0e0", "-1e0 * 0e0"},
		pair{"two keys", "a int NOT NULL, b varchar(5) NOT NULL, PRIMARY KEY (a, b)", "1, 'x'", "1, 'X'"},
		pair{"key on one side", "k int NOT NULL PRIMARY KEY", "1", "2"})
	ctx := context.Background()
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i, tc := range pairs {
		t.Run(tc.name, func(t *testing.T) {
			w, p := fmt.Sprintf("w%d", i), fmt.Sprintf("p%d", i)
			sqlServerExec(t, db, "CREATE TABLE "+w+" ("+tc.columns+")", "CREATE TABLE "+p+" ("+tc.columns+")")
			inv, err := s.inventory(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			tb := inv.ByItem["table:dbo."+w]
			changed := func(a, b string) int64 {
				t.Helper()
				sqlServerExec(t, db, "DELETE FROM "+w, "DELETE FROM "+p, "INSERT INTO "+w+" VALUES ("+a+")", "INSERT INTO "+p+" VALUES ("+b+")")
				q, ok := ssChangedSQL(tb, "dbo."+w, "dbo."+p, "#k")
				if !ok {
					t.Fatal("no comparison")
				}
				var n int64
				if err := c.QueryRowContext(ctx, q+";\nSELECT COUNT_BIG(*) FROM #k; DROP TABLE #k").Scan(&n); err != nil {
					t.Fatalf("%v\n%s", err, q)
				}
				return n
			}
			if n := changed(tc.a, tc.b); n == 0 {
				t.Errorf("(%s) and (%s) compare equal", tc.a, tc.b)
			}
			if n := changed(tc.b, tc.a); n == 0 {
				t.Errorf("(%s) and (%s) compare equal", tc.b, tc.a)
			}
			for _, r := range []string{tc.a, tc.b} {
				if n := changed(r, r); n != 0 {
					t.Errorf("(%s) differs from itself", r)
				}
			}
		})
	}
}

// TestSQLServerRestoredKept: the tables the tracker's fingerprint of the
// restored items read are reused by the next full fingerprint, and a write
// to them in between is seen.
func TestSQLServerRestoredKept(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{}, "CREATE TABLE t (id int PRIMARY KEY, v int NULL)", "INSERT INTO t VALUES (1, 1), (2, 2)",
		"CREATE TABLE u (id int PRIMARY KEY)", "INSERT INTO u VALUES (1)")
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the login cannot read the server's performance state")
	}
	p := pristineSQLServer(t, s)
	ctx := context.Background()
	id := sqlServerInt(t, db, "SELECT OBJECT_ID('dbo.t')")
	items := []string{"table:dbo.t"}
	for _, write := range []string{"", "UPDATE t SET v = 5 WHERE id = 2", "INSERT INTO t VALUES (3, 3)", "DELETE FROM t WHERE id = 1"} {
		sqlServerExec(t, db, "UPDATE t SET v = 9 WHERE id = 1")
		mustSQLServerFingerprint(t, s)
		if err := s.Restore(ctx, items); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Fingerprint(ctx, items); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.last.content[id]; !ok {
			t.Fatal("the restored table read after the restore is not kept")
		}
		if write != "" {
			sqlServerExec(t, db, write)
		}
		f := mustSQLServerFingerprint(t, s)
		want := []string(nil)
		if write != "" {
			want = items
		}
		if d := Diff(f, p); !reflect.DeepEqual(d, want) {
			t.Fatalf("after %q: %v differ, want %v", write, d, want)
		}
		if d := Diff(f, sqlServerFull(t, s)); len(d) > 0 {
			t.Fatalf("after %q: %v differ from a full read", write, d)
		}
		if write != "" {
			if err := s.Restore(ctx, items); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestSQLServerOwnDDL: after a restore that turned triggers and foreign
// keys off and on, the catalog is reused within two seconds (the restore's
// own changes are not taken for an application's), and an application's
// ALTER right after the restore is seen, of the very objects too.
func TestSQLServerOwnDDL(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{},
		"CREATE TABLE par (id int PRIMARY KEY, v int NULL)", "INSERT INTO par VALUES (1, 1), (2, 2)",
		"CREATE TABLE kid (id int PRIMARY KEY, p int NULL CONSTRAINT kid_fk REFERENCES par(id))", "INSERT INTO kid VALUES (1, 1)",
		"CREATE TABLE audit (n int NULL)",
		"CREATE TRIGGER par_t ON par AFTER UPDATE AS INSERT INTO audit VALUES (1)",
		"CREATE TRIGGER ddl_t ON DATABASE FOR CREATE_PROCEDURE AS SET NOCOUNT ON")
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the login cannot read the server's performance state")
	}
	p := pristineSQLServer(t, s)
	ctx := context.Background()
	items := []string{"table:dbo.par"}
	restore := func() {
		t.Helper()
		sqlServerExec(t, db, "DELETE FROM par WHERE id = 2")
		mustSQLServerFingerprint(t, s)
		if err := s.Restore(ctx, items); err != nil {
			t.Fatal(err)
		}
		if s.own.values == "" {
			t.Fatal("the restore noted no catalog change of its own")
		}
		// Any change from now on has a later modify_date than the
		// restore's own, however rounded to datetime's steps.
		if n := sqlServerInt(t, db, "SELECT CASE WHEN GETDATE() > DATEADD(millisecond, 7, CONVERT(datetime, '"+s.own.latest+"', 121)) THEN 1 ELSE 0 END"); n != 1 {
			t.Fatalf("the restore committed before the clock passed %s", s.own.latest)
		}
	}
	// Two seconds of the setup's own changes first.
	time.Sleep(2100 * time.Millisecond)
	restore()
	mustSQLServerFingerprint(t, s)
	inv := s.inv
	sqlServerExec(t, db, "INSERT INTO audit VALUES (2)")
	mustSQLServerFingerprint(t, s)
	if inv == nil || s.inv != inv {
		t.Error("the catalog is read again after the restore's own changes")
	}
	sqlServerExec(t, db, "DELETE FROM audit")
	for i, alter := range []string{
		"ALTER TRIGGER par_t ON par AFTER UPDATE AS INSERT INTO audit VALUES (3)",
		"ALTER TABLE kid NOCHECK CONSTRAINT kid_fk",
		"ALTER TABLE par ADD w int NULL",
		"DISABLE TRIGGER ddl_t ON DATABASE",
		"CREATE INDEX par_v ON par (v)",
	} {
		restore()
		sqlServerExec(t, db, alter) // at once
		f := mustSQLServerFingerprint(t, s)
		if d := Diff(f, sqlServerFull(t, s)); len(d) > 0 {
			t.Fatalf("%d: after %q, %v differ from a full read", i, alter, d)
		}
		if d := Diff(f, p); len(d) == 0 {
			t.Fatalf("%d: %q is not seen", i, alter)
		}
		// Back to the pristine copy (the whole database for the
		// definitions).
		if err := s.Restore(ctx, nil); err != nil {
			t.Fatal(err)
		}
		db.SetMaxIdleConns(0)
		db.SetMaxIdleConns(2)
		if d := Diff(mustSQLServerFingerprint(t, s), p); len(d) > 0 {
			t.Fatalf("%d: %v differ after the restore", i, d)
		}
	}
}

// TestSQLServerOwnDDLSequence: after a restore that restarted a
// sequence, the catalog is reused within two seconds.
func TestSQLServerOwnDDLSequence(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{}, "CREATE SEQUENCE q AS int START WITH 1", "CREATE TABLE t (n int NULL)")
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the login cannot read the server's performance state")
	}
	time.Sleep(2100 * time.Millisecond) // the setup's own changes
	sqlServerExec(t, db, "SELECT NEXT VALUE FOR q")
	mustSQLServerFingerprint(t, s)
	if err := s.Restore(context.Background(), []string{"sequence:dbo.q"}); err != nil {
		t.Fatal(err)
	}
	if s.own.values == "" {
		t.Fatal("the restore noted no catalog change of its own")
	}
	mustSQLServerFingerprint(t, s)
	inv := s.inv
	sqlServerExec(t, db, "INSERT INTO t VALUES (1)")
	mustSQLServerFingerprint(t, s)
	if inv == nil || s.inv != inv {
		t.Error("the catalog is read again after the restore's own restart of the sequence")
	}
}

// TestSQLServerOwnDDLAhead: a restore whose own catalog changes read
// later than the server's clock (a modify_date ahead of it) commits after
// a bounded wait and exempts none of them, instead of holding its locks
// until the clock gets there.
func TestSQLServerOwnDDLAhead(t *testing.T) {
	s, _ := newSQLServerTest(t, Options{}, "")
	ctx := context.Background()
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	own, err := s.commitOwn(ctx, c, ssOwnDDL{values: "|1 2099-01-01 00:00:00.000|", latest: "2099-01-01 00:00:00.000"})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 10*sqlServerOwnWait {
		t.Errorf("the commit waited %v", d)
	}
	if own.values != "" {
		t.Errorf("own changes ahead of the clock exempted: %+v", own)
	}
	var n int
	if err := c.QueryRowContext(ctx, "SELECT @@TRANCOUNT").Scan(&n); err != nil || n != 0 {
		t.Fatalf("transactions open after the commit: %d, %v", n, err)
	}
}

// TestSQLServerRestoreFallbacks: changes item by item cannot restore
// restore the whole database, which then fingerprints as the pristine
// copy; an application's connection fails once and its pool connects
// again; item-level restores leave its session alone.
func TestSQLServerRestoreFallbacks(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{}, append(sqlServerRestoreSchema,
		"CREATE VIEW v AS SELECT id FROM orders",
		"CREATE PROCEDURE pr AS SELECT 1",
		"CREATE TABLE hist (id int PRIMARY KEY, v int NULL, f datetime2 GENERATED ALWAYS AS ROW START HIDDEN NOT NULL, t datetime2 GENERATED ALWAYS AS ROW END HIDDEN NOT NULL, PERIOD FOR SYSTEM_TIME (f, t)) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.hist_history))",
		"INSERT INTO hist (id, v) VALUES (1, 1)",
		"ALTER DATABASE CURRENT SET ENABLE_BROKER WITH ROLLBACK IMMEDIATE")...)
	p := pristineSQLServer(t, s)
	app, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var spid int
	app.QueryRowContext(context.Background(), "SELECT @@SPID").Scan(&spid)
	sqlServerExec(t, db, "INSERT INTO orders (note) VALUES (N'z')")
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
	var again int
	if err := app.QueryRowContext(context.Background(), "SELECT @@SPID").Scan(&again); err != nil || again != spid {
		t.Fatalf("the application's session after an item-level restore: %v, %d (was %d)", err, again, spid)
	}
	for _, tc := range []struct {
		name  string
		stmts []string
	}{
		{"view", []string{"ALTER VIEW v AS SELECT id, note FROM orders"}},
		{"procedure", []string{"ALTER PROCEDURE pr AS SELECT 2"}},
		{"new column", []string{"DISABLE TRIGGER no_alter ON DATABASE", "ALTER TABLE heap ADD c int NULL", "ENABLE TRIGGER no_alter ON DATABASE"}},
		{"new table", []string{"CREATE TABLE extra (id int)"}},
		{"dropped table", []string{"DROP TABLE lines"}},
		{"temporal", []string{"UPDATE hist SET v = 2"}},
		{"trigger off", []string{"DISABLE TRIGGER orders_audit ON orders"}},
		{"sequence", []string{"DISABLE TRIGGER no_alter ON DATABASE", "ALTER SEQUENCE unused INCREMENT BY 2", "ENABLE TRIGGER no_alter ON DATABASE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlServerExec(t, db, tc.stmts...)
			if !restoreSQLServer(t, s, db, log, p) {
				t.Fatal("restored item by item")
			}
		})
	}
	if err := app.PingContext(context.Background()); err == nil {
		t.Log("the application's pinned connection survived the restore of the whole database")
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("the application's pool after the restore of the whole database: %v", err)
	}
	var broker bool
	db.QueryRow("SELECT is_broker_enabled FROM sys.databases WHERE name = DB_NAME()").Scan(&broker)
	if !broker {
		t.Error("Service Broker is off after the restore of the whole database")
	}
	if got := nextIdentity(t, db, "app.users"); got != 25 {
		t.Errorf("next identity %d after the restore of the whole database, want 25", got)
	}
}

// TestSQLServerRestoreExcluded: excluded tables and sequences keep their
// rows and values through item-level restores and restores of the whole
// database, which also leave no database behind.
func TestSQLServerRestoreExcluded(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions", "app.jobs*", "ticket"}},
		"CREATE SCHEMA app",
		"CREATE TABLE users (id int IDENTITY PRIMARY KEY, name nvarchar(20) NULL)",
		"INSERT INTO users (name) VALUES (N'a'), (N'b')",
		"CREATE TABLE sessions (id int IDENTITY(5, 1) PRIMARY KEY, user_id int NULL CONSTRAINT fk_sessions_users REFERENCES users(id) ON DELETE CASCADE, token xml NULL)",
		"CREATE TABLE app.jobs (id bigint PRIMARY KEY, payload varbinary(max) NULL)",
		"CREATE SEQUENCE ticket START WITH 1",
		"CREATE VIEW v AS SELECT id FROM users",
		"INSERT INTO sessions (user_id, token) VALUES (1, N'<t a=\"1\"/>')", // left out of the pristine copy
	)
	ctx := context.Background()
	pdb := sqlServerTestDB(t, s.o.DSN, s.o.Pristine)
	if n := sqlServerInt(t, pdb, "SELECT COUNT(*) FROM sessions"); n != 0 {
		t.Fatalf("%d rows of an excluded table in the pristine copy", n)
	}
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO users (name) VALUES (N'c')",
		"INSERT INTO sessions (user_id, token) VALUES (3, N'<t/>'), (2, NULL)",
		"INSERT INTO app.jobs VALUES (1, 0x01), (2, NULL)",
		"DECLARE @x bigint = NEXT VALUE FOR ticket; SET @x = NEXT VALUE FOR ticket")
	snapshot := func() string {
		var b strings.Builder
		rows, err := db.Query(`SELECT CONCAT(id, ' ', user_id, ' ', CAST(token AS nvarchar(max))) FROM sessions
			UNION ALL SELECT CONCAT(id, ' ', CONVERT(varchar(20), payload, 1)) FROM app.jobs
			UNION ALL SELECT CONCAT('ident ', IDENT_CURRENT('sessions'), ' seq ', CAST(current_value AS varchar(20))) FROM sys.sequences WHERE name = 'ticket'
			UNION ALL SELECT CONCAT(name, ' ', is_disabled, is_not_trusted) FROM sys.foreign_keys ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var l string
			rows.Scan(&l)
			b.WriteString(l + "\n")
		}
		return b.String()
	}
	want := snapshot()
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
	// The row of user 3 refers to a user the restore removed: the
	// cascade did not run.
	if got := snapshot(); got != strings.Replace(want, "fk_sessions_users 00", "fk_sessions_users 01", 1) {
		t.Fatalf("excluded after an item-level restore:\n%s\nwant\n%s", got, want)
	}
	// The rows go back after a restore of the whole database too, the
	// foreign key no longer trusted.
	want = snapshot()
	sqlServerExec(t, db, "ALTER VIEW v AS SELECT id, name FROM users")
	if !restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored item by item")
	}
	if got := snapshot(); got != want {
		t.Fatalf("excluded after a restore of the whole database:\n%s\nwant\n%s", got, want)
	}
	if got := nextIdentity(t, db, "sessions"); got != 8 {
		t.Fatalf("next identity of the excluded table %d, want 8", got)
	}
	if d, err := s.database(ctx, s.excludedDB()); err != nil || d != nil {
		t.Fatalf("%s left: %v", s.excludedDB(), err)
	}
}

// TestSQLServerRestoreWholeWritten: an application that writes to
// excluded tables all through a restore of the whole database loses none
// of its rows when its login is not privileged (it is kept out meanwhile),
// keeps the rows its privileged login writes after the RESTORE, and gets
// no duplicate key from an identity set back below rows copied back.
func TestSQLServerRestoreWholeWritten(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions", "tokens"}},
		"CREATE TABLE users (id int IDENTITY PRIMARY KEY, name nvarchar(20) NULL)",
		"INSERT INTO users (name) VALUES (N'a')",
		"CREATE TABLE sessions (id int IDENTITY PRIMARY KEY, v nvarchar(40) NOT NULL)",
		"CREATE TABLE tokens (id uniqueidentifier PRIMARY KEY, v int NOT NULL)",
		"CREATE VIEW v AS SELECT id FROM users",
		"INSERT INTO sessions (v) VALUES (N'before the seed')")
	b := make([]byte, 4)
	rand.Read(b)
	login, password := "dgt_app_"+hex.EncodeToString(b), "Dg-"+hex.EncodeToString(b)+"-app"
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, master, "CREATE LOGIN "+qms(login)+" WITH PASSWORD = "+sqlServerString(password)+", CHECK_POLICY = OFF")
	t.Cleanup(func() { master.Exec("DROP LOGIN " + qms(login)) })
	sqlServerExec(t, db, "CREATE USER "+qms(login)+" FOR LOGIN "+qms(login), "GRANT SELECT, INSERT ON sessions TO "+qms(login))
	if err := s.Seed(context.Background()); err != nil { // with the user
		t.Fatal(err)
	}
	p := pristineSQLServer(t, s)
	u, _ := url.Parse(s.o.DSN)
	u.User = url.UserPassword(login, password)
	q := u.Query()
	q.Set("database", s.o.Working)
	u.RawQuery = q.Encode()
	app, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	sqlServerExec(t, db, "INSERT INTO sessions (v) VALUES (N'after the seed')")

	// The application: its own login writes sessions, a privileged one
	// (the test's) tokens, each every 5 ms; what committed is noted.
	var mu sync.Mutex
	var sessions []int64
	var tokens []string
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			var id int64
			if app.QueryRowContext(ctx, "INSERT INTO sessions (v) OUTPUT inserted.id VALUES (N'app')").Scan(&id) == nil {
				mu.Lock()
				sessions = append(sessions, id)
				mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; ctx.Err() == nil; n++ {
			var id string
			if db.QueryRowContext(ctx, "INSERT INTO tokens OUTPUT CONVERT(nvarchar(36), inserted.id) VALUES (NEWID(), @p1)", n).Scan(&id) == nil {
				mu.Lock()
				tokens = append(tokens, id)
				mu.Unlock()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	sqlServerExec(t, db, "ALTER VIEW v AS SELECT id, name FROM users")
	mu.Lock()
	before := len(tokens)
	mu.Unlock()
	whole := restoreSQLServer(t, s, nil, log, p)
	mu.Lock()
	after := len(tokens)
	mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	stop()
	wg.Wait()
	if !whole {
		t.Fatal("restored item by item")
	}

	have := map[string]bool{}
	rows, err := db.Query("SELECT CONCAT(N's', id) FROM sessions UNION ALL SELECT CONCAT(N't', CONVERT(nvarchar(36), id)) FROM tokens")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		rows.Scan(&k)
		have[k] = true
	}
	rows.Close()
	for _, id := range sessions {
		if !have[fmt.Sprintf("s%d", id)] {
			t.Errorf("session %d the application's login wrote is gone (%d written)", id, len(sessions))
		}
	}
	lost := 0
	for i, id := range tokens {
		if !have["t"+strings.ToLower(id)] && !have["t"+strings.ToUpper(id)] {
			if i < before || i >= after {
				t.Errorf("token %d of %d, written before or after the restore, is gone", i, len(tokens))
			}
			lost++
		}
	}
	t.Logf("%d sessions, %d tokens written (%d during the restore, %d of them lost)", len(sessions), len(tokens), after-before, lost)
	if len(sessions) == 0 || after == before {
		t.Fatal("the application wrote nothing")
	}
	// The identity is past every row: no duplicate key.
	for range 20 {
		var id int64
		if err := app.QueryRow("INSERT INTO sessions (v) OUTPUT inserted.id VALUES (N'after')").Scan(&id); err != nil {
			t.Fatalf("insert after the restore: %v", err)
		}
	}
	if a := sqlServerInt(t, master, "SELECT CASE WHEN user_access_desc = N'MULTI_USER' THEN 1 ELSE 0 END FROM sys.databases WHERE name = "+sqlServerString(s.o.Working)); a != 1 {
		t.Fatal("the working database is not MULTI_USER after the restore")
	}
}

// TestSQLServerNoDBCreator: a login without dbcreator is told what it
// lacks when it seeds, and when it restores from a copy another login
// made; the server's error stays in the message (setup matches it).
func TestSQLServerNoDBCreator(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{}, "CREATE TABLE t (id int PRIMARY KEY)")
	b := make([]byte, 4)
	rand.Read(b)
	login, password := "dgt_login_"+hex.EncodeToString(b), "Dg-"+hex.EncodeToString(b)+"-owner"
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, master, "CREATE LOGIN "+qms(login)+" WITH PASSWORD = "+sqlServerString(password)+", CHECK_POLICY = OFF")
	t.Cleanup(func() { master.Exec("DROP LOGIN " + qms(login)) })
	sqlServerExec(t, db, "CREATE USER "+qms(login)+" FOR LOGIN "+qms(login), "ALTER ROLE db_owner ADD MEMBER "+qms(login))
	u, _ := url.Parse(s.o.DSN)
	u.User = url.UserPassword(login, password)
	o := s.o
	o.DSN = u.String()
	l, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	sqlServerExec(t, db, "INSERT INTO t VALUES (1)")
	if err := l.Restore(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "needs the server role dbcreator") || !strings.Contains(err.Error(), "(916)") {
		t.Fatalf("restore from a copy another login made: %v", err)
	}
	dropSQLServerDatabase(master, s.o.Pristine)
	if err := l.Seed(context.Background()); err == nil || !strings.Contains(err.Error(), "needs the server role dbcreator") || !strings.Contains(err.Error(), "(262)") {
		t.Fatalf("seed without dbcreator: %v", err)
	}
}

// TestSQLServerSeedStopped: a seed stopped during the RESTORE of the new
// copy drops what that RESTORE left; one left restoring by a RESTORE that
// was cut off (the process killed) is dropped by the next seed.
func TestSQLServerSeedStopped(t *testing.T) {
	s, _, _ := newSQLServerSeeded(t, Options{},
		"CREATE TABLE b (id int IDENTITY PRIMARY KEY, v char(4000) NOT NULL DEFAULT 'x')",
		"INSERT INTO b (v) SELECT TOP 60000 'y' FROM sys.all_columns a CROSS JOIN sys.all_columns c")
	master := sqlServerTestDB(t, s.o.DSN, "")
	newDB := s.o.Pristine + "_dgnew"
	// during calls stop once a RESTORE of newDB runs.
	during := func(after time.Duration, stop func(session int)) chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 2000 {
				var session int
				err := master.QueryRow(`SELECT TOP (1) r.session_id FROM sys.dm_exec_requests r CROSS APPLY sys.dm_exec_sql_text(r.sql_handle) x
					WHERE r.command LIKE N'RESTORE%' AND CHARINDEX(N'RESTORE DATABASE ' + QUOTENAME(@p1) + N' ', x.text) > 0`, newDB).Scan(&session)
				if err == nil {
					time.Sleep(after) // past the start: it leaves the database restoring
					stop(session)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		return done
	}
	state := func() string {
		var st sql.NullString
		master.QueryRow("SELECT state_desc FROM sys.databases WHERE name = @p1", newDB).Scan(&st)
		return st.String
	}

	// The seed's context ends.
	ctx, cancel := context.WithCancel(context.Background())
	done := during(150*time.Millisecond, func(int) { cancel() })
	err := s.Seed(ctx)
	<-done
	if err == nil {
		t.Skip("the RESTORE ended before it could be stopped")
	}
	if st := state(); st != "" {
		t.Fatalf("a seed stopped during its RESTORE (%v) left %s %s", err, newDB, st)
	}

	// The RESTORE is cut off by the server, as when the process is
	// killed: the next seed drops what it left.
	c, err := s.conn(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := s.paths(context.Background(), c)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	bak := ssJoin(paths.backup, "finfix_stopped.bak")
	sqlServerExec(t, master, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(bak)+" WITH COPY_ONLY, INIT, FORMAT")
	t.Cleanup(func() { master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(bak)) })
	// When the cut comes too early nothing is left, too late the
	// database is online: another try.
	for _, after := range []time.Duration{150, 300, 80, 450, 600} {
		done = during(after*time.Millisecond, func(session int) {
			// Only while it still runs the RESTORE: a session id is reused.
			master.Exec(fmt.Sprintf("IF EXISTS (SELECT 1 FROM sys.dm_exec_requests WHERE session_id = %d AND command LIKE N'RESTORE%%') KILL %d", session, session))
		})
		tag := strings.TrimPrefix(sqlServerTestName(), "dgt_ss_") // a file a RESTORE cut off early leaves keeps its name
		master.Exec("RESTORE DATABASE " + qms(newDB) + " FROM DISK = " + sqlServerString(bak) + " WITH RECOVERY, MOVE N'" + s.o.Working +
			"' TO " + sqlServerString(ssJoin(paths.data, "dg_"+tag+"_1.mdf")) + ", MOVE N'" + s.o.Working + "_log' TO " + sqlServerString(ssJoin(paths.log, "dg_"+tag+"_2.ldf")))
		<-done
		if state() == "RESTORING" {
			break
		}
		dropSQLServerDatabase(master, newDB)
	}
	if st := state(); st != "RESTORING" {
		t.Skipf("the RESTORE was not cut off halfway (%q)", st)
	}
	if err := s.Seed(context.Background()); err != nil {
		t.Fatalf("seed after a RESTORE that was cut off: %v", err)
	}
	if st := state(); st != "" {
		t.Fatalf("%s left %s", newDB, st)
	}
}

// TestSQLServerRestoreNothing: a restore of nothing, or of everything
// when nothing differs, changes nothing; one without a pristine copy, or
// with one of another working database, says so.
func TestSQLServerRestoreNothing(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{}, "CREATE TABLE t (id int PRIMARY KEY)")
	ctx := context.Background()
	for _, items := range [][]string{nil, {}, {"table:dbo.nope"}} {
		if err := s.Restore(ctx, items); err != nil {
			t.Fatalf("%v: %v", items, err)
		}
	}
	sqlServerExec(t, db, "INSERT INTO t VALUES (1)")
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM t"); n != 0 {
		t.Fatalf("%d rows after a restore of everything", n)
	}
	o := s.o
	o.Pristine = o.Working + "_none"
	none, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer none.Close()
	if err := none.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "does not exist; run driftgate seed") {
		t.Fatalf("restore without a pristine copy: %v", err)
	}
}

// benchRestore changes the working database (change), restores items and
// fingerprints them as the tracker does, once to warm up (connections,
// catalogs) and then in a b.Loop (the setup runs once, also with
// -benchtime Nx), and reports the time of a restore alone. It starts two
// seconds after the setup changed the catalog, which is not reused
// before (ssGuardSQL).
func benchRestore(b *testing.B, s *SQLServer, change func(n int), items func(n int) []string) {
	ctx := context.Background()
	time.Sleep(2 * time.Second)
	mustSQLServerFingerprint(b, s)
	var took time.Duration
	one := func(n int) {
		change(n)
		start := time.Now()
		if err := s.Restore(ctx, items(n)); err != nil {
			b.Fatal(err)
		}
		if n > 0 {
			took += time.Since(start)
		}
		if _, err := s.Fingerprint(ctx, items(n)); err != nil {
			b.Fatal(err)
		}
	}
	one(0)
	n := 0
	for b.Loop() {
		n++
		one(n)
	}
	b.ReportMetric(float64(took.Microseconds())/1000/float64(n), "ms/restore")
}

// BenchmarkSQLServerRestoreSmall: one row of one table of ten changed and
// restored (the budget is 30 ms).
func BenchmarkSQLServerRestoreSmall(b *testing.B) {
	var setup []string
	for i := range 10 {
		setup = append(setup, fmt.Sprintf("CREATE TABLE t%d (id int IDENTITY PRIMARY KEY, v int NULL, s nvarchar(50) NULL)", i),
			fmt.Sprintf("INSERT INTO t%d (v, s) SELECT TOP (100) object_id, name FROM sys.all_objects", i))
	}
	s, db, _ := newSQLServerSeeded(b, Options{}, setup...)
	benchRestore(b, s, func(n int) { sqlServerExec(b, db, fmt.Sprintf("UPDATE t%d SET v = %d WHERE id = 1", n%10, n+1)) },
		func(n int) []string { return []string{fmt.Sprintf("table:dbo.t%d", n%10)} })
}

// BenchmarkSQLServerRestoreDelta: one row of a table of a million changed
// and restored (the budget is 300 ms).
func BenchmarkSQLServerRestoreDelta(b *testing.B) {
	s, db, _ := newSQLServerSeeded(b, Options{},
		"CREATE TABLE big (id int PRIMARY KEY, v int NULL, s nvarchar(50) NULL, f float NULL, d datetime2 NULL)",
		`WITH n AS (SELECT TOP (1000000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects a CROSS JOIN sys.all_objects b CROSS JOIN sys.all_objects c)
		 INSERT INTO big WITH (TABLOCK) SELECT i, i % 1000, CONCAT(N'name ', i), i * 1.5, DATEADD(second, i, '2020-01-01') FROM n`)
	benchRestore(b, s, func(n int) {
		sqlServerExec(b, db, fmt.Sprintf("UPDATE big SET v = -%d WHERE id = %d", n+1, (n+1)*7919%1000000+1))
	},
		func(int) []string { return []string{"table:dbo.big"} })
}

// BenchmarkSQLServerFallback: a restore of the whole (small) database
// (reported only).
func BenchmarkSQLServerFallback(b *testing.B) {
	s, db, _ := newSQLServerSeeded(b, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE t (id int IDENTITY PRIMARY KEY, v int NULL)", "INSERT INTO t (v) SELECT TOP (1000) object_id FROM sys.all_objects",
		"CREATE TABLE sessions (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM t")
	benchRestore(b, s, func(n int) {
		db.SetMaxIdleConns(0) // the restore ended the sessions
		db.SetMaxIdleConns(2)
		sqlServerExec(b, db, fmt.Sprintf("ALTER VIEW v AS SELECT id, %d AS n FROM t", n), fmt.Sprintf("INSERT INTO sessions VALUES (%d)", n))
	}, func(int) []string { return []string{"view:dbo.v"} })
}

// TestSQLServerRestoreEdges: keys equal only under the collation or with
// trailing zero bytes, sql_variant types, triggers with an order, identity
// columns at their type's ends and constraints NOT FOR REPLICATION are
// restored item by item or as a whole, as the pristine copy has them.
func TestSQLServerRestoreEdges(t *testing.T) {
	defer func(n int64) { sqlServerDeltaRows = n }(sqlServerDeltaRows)
	sqlServerDeltaRows = 100
	s, db, log := newSQLServerSeeded(t, Options{},
		"CREATE TABLE audit (id int IDENTITY PRIMARY KEY, w nvarchar(20))",
		"CREATE TABLE big (k varbinary(10) NOT NULL PRIMARY KEY, c nchar(5) COLLATE Latin1_General_CI_AI NOT NULL, v sql_variant NULL, UNIQUE (c))",
		`WITH n AS (SELECT TOP (400) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects)
		 INSERT INTO big SELECT CAST(i AS varbinary(4)), RIGHT(CONCAT('0000', i), 5), CAST(i AS int) FROM n`,
		"CREATE TRIGGER t1 ON big AFTER INSERT AS INSERT INTO audit (w) VALUES (N't1')",
		"CREATE TRIGGER t2 ON big AFTER INSERT AS INSERT INTO audit (w) VALUES (N't2')",
		"EXEC sp_settriggerorder N'dbo.t2', N'First', N'INSERT'",
		"EXEC sp_settriggerorder N'dbo.t1', N'Last', N'INSERT'",
		"CREATE TABLE lo (id int IDENTITY(1,1), v int NULL)",
		"INSERT INTO lo (v) VALUES (1),(2),(3),(4),(5)",
		"DBCC CHECKIDENT ('lo', RESEED, 2) WITH NO_INFOMSGS",
		"CREATE TABLE neg (id int IDENTITY(-1,-1) PRIMARY KEY, v int NULL)",
		"INSERT INTO neg (v) VALUES (1),(2)",
		"DBCC CHECKIDENT ('neg', RESEED, 10) WITH NO_INFOMSGS",
		"CREATE TABLE bigid (id bigint IDENTITY(9223372036854775800, 1) PRIMARY KEY, v int NULL)",
		"INSERT INTO bigid (v) VALUES (1)",
		"CREATE TABLE dec (id decimal(38,0) IDENTITY(99999999999999999999999999999999999990, 1) PRIMARY KEY, v int NULL)",
		"INSERT INTO dec (v) VALUES (1)",
		"CREATE TABLE tiny (id tinyint IDENTITY(254,1) PRIMARY KEY, v int NULL)",
		"INSERT INTO tiny (v) VALUES (1),(2)",
		"CREATE TABLE onlydel (id int IDENTITY(5,5) PRIMARY KEY, v int NULL)",
		"INSERT INTO onlydel (v) VALUES (1)", "DELETE FROM onlydel",
		"CREATE TABLE par (id int PRIMARY KEY)",
		"CREATE TABLE kid (id int PRIMARY KEY, p int NULL CONSTRAINT fk REFERENCES par(id) ON DELETE SET NULL NOT FOR REPLICATION, CONSTRAINT ck CHECK NOT FOR REPLICATION (id > 0))",
		"INSERT INTO par VALUES (1),(2)", "INSERT INTO kid VALUES (1,1)")
	p := pristineSQLServer(t, s)
	flags := sqlServerFlags(t, db)
	sqlServerExec(t, db,
		"UPDATE big SET k = k + 0x00 WHERE k = CAST(1 AS varbinary(4))", // the same key under =, another value
		"UPDATE big SET v = CAST(2 AS bigint) WHERE k = CAST(2 AS varbinary(4))",
		"UPDATE big SET c = N'00003 ' WHERE k = CAST(3 AS varbinary(4))",
		"UPDATE big SET c = LOWER(N'0000á') WHERE k = CAST(4 AS varbinary(4))",
		"UPDATE big SET v = CAST(N'5' AS sql_variant) WHERE k = CAST(5 AS varbinary(4))",
		"INSERT INTO par VALUES (3)", "DELETE FROM par WHERE id = 2")
	audit := sqlServerInt(t, db, "SELECT COUNT(*) FROM audit")
	if restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored the whole database")
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM audit"); n != audit {
		t.Fatalf("triggers fired: %d audit rows, want %d", n, audit)
	}
	if got := sqlServerFlags(t, db); got != flags {
		t.Fatalf("flags %s, want %s", got, flags)
	}
	want := map[string]int64{}
	for _, tb := range []string{"lo", "neg", "bigid", "onlydel"} {
		want[tb] = nextIdentity(t, db, tb)
		restoreSQLServer(t, s, db, log, p)
	}
	sqlServerExec(t, db, "INSERT INTO lo (v) VALUES (9)", "INSERT INTO neg (v) VALUES (9)", "INSERT INTO bigid (v) VALUES (9)",
		"INSERT INTO dec (v) VALUES (9)", "DELETE FROM tiny WHERE id = 255", "INSERT INTO onlydel (v) VALUES (9)", "TRUNCATE TABLE onlydel")
	restoreSQLServer(t, s, db, log, p) // onlydel: last_value without rows needs the whole database
	for tb, w := range want {
		if got := nextIdentity(t, db, tb); got != w {
			t.Errorf("%s: next identity %d, want %d", tb, got, w)
		}
		restoreSQLServer(t, s, db, log, p)
	}
	if _, err := db.Exec("INSERT INTO tiny (v) VALUES (3)"); err == nil {
		t.Error("the restored tinyint identity hands out a value past 255")
	}
}

// TestSQLServerRestoreFailed: a restore that fails inside its transaction
// (a foreign key of a table it does not restore) leaves the rows, the
// constraints' and the triggers' flags as they were.
func TestSQLServerRestoreFailed(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{},
		"CREATE TABLE par (id int IDENTITY PRIMARY KEY, v int NULL)",
		"CREATE TABLE kid (id int PRIMARY KEY, p int NULL CONSTRAINT fk REFERENCES par(id))",
		"INSERT INTO par (v) VALUES (1)",
		"CREATE TRIGGER par_t ON par AFTER INSERT AS RETURN",
		"CREATE TRIGGER no_alter ON DATABASE FOR ALTER_TABLE AS BEGIN RAISERROR (N'no', 16, 1); ROLLBACK; END")
	flags := sqlServerFlags(t, db)
	sqlServerExec(t, db, "INSERT INTO par (v) VALUES (2)", "INSERT INTO kid VALUES (1, 2)")
	if err := s.Restore(context.Background(), []string{"table:dbo.par"}); err == nil {
		t.Fatal("restored par without the row of kid that refers to it")
	}
	if got := sqlServerFlags(t, db); got != flags {
		t.Fatalf("flags %s, want %s", got, flags)
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM par"); n != 2 {
		t.Fatalf("%d rows in par, want 2", n)
	}
}

// TestSQLServerRestoreNotOnline: a working database the user took offline
// is brought online and restored, the rows of its excluded tables kept (a
// seed refuses it and names the restore); one restoring that DriftGate
// did not leave so, with tables excluded, is refused.
func TestSQLServerRestoreNotOnline(t *testing.T) {
	ctx := context.Background()
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE users (id int PRIMARY KEY)", "INSERT INTO users VALUES (1)",
		"CREATE TABLE sessions (id int PRIMARY KEY)")
	master := sqlServerTestDB(t, s.o.DSN, "")
	t.Cleanup(func() {
		master.Exec("IF DATABASEPROPERTYEX(" + sqlServerString(s.o.Working) + ", 'Status') = 'RESTORING' RESTORE DATABASE " + qms(s.o.Working) + " WITH RECOVERY")
	})
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO sessions VALUES (1),(2),(3)", "INSERT INTO users VALUES (2)")
	db.SetMaxIdleConns(0)
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" SET OFFLINE WITH ROLLBACK IMMEDIATE")
	// A seed does not copy it: it says what brings it back.
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "is OFFLINE; run a capture or a restore first: it brings the database back from the pristine copy") {
		t.Fatalf("seed of an offline working database: %v", err)
	}
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	log.fellBack()
	db.SetMaxIdleConns(2)
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != 3 {
		t.Fatalf("%d rows of the excluded table, want 3", n)
	}
	if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
		t.Fatalf("%v differ", d)
	}
	// Restoring, by someone else's RESTORE … WITH NORECOVERY.
	var dir string
	master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512))").Scan(&dir)
	file := ssJoin(dir, s.o.Working+"_user.bak")
	t.Cleanup(func() { master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(file)) })
	db.SetMaxIdleConns(0)
	sqlServerExec(t, master, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(file)+" WITH COPY_ONLY, INIT, FORMAT",
		"ALTER DATABASE "+qms(s.o.Working)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"RESTORE DATABASE "+qms(s.o.Working)+" FROM DISK = "+sqlServerString(file)+" WITH REPLACE, NORECOVERY")
	if err := s.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "has not saved the rows of its excluded tables") {
		t.Fatalf("restore of a database restoring: %v", err)
	}
	if d, _ := s.database(ctx, s.o.Working); d == nil || d.State != "RESTORING" {
		t.Fatal("the refused restore changed the database")
	}
}

// TestSQLServerExcludedChanged: a restore of the whole database refuses,
// before it changes anything, when an excluded table's columns differ
// from the pristine copy's (a decimal's scale): its rows would not go back
// as they are.
func TestSQLServerExcludedChanged(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE users (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM users",
		"CREATE TABLE sessions (id int PRIMARY KEY, amount decimal(18,2) NULL, name varchar(20) COLLATE Latin1_General_CI_AS NULL)")
	for _, change := range []string{
		"ALTER TABLE sessions ALTER COLUMN amount decimal(18,4) NULL",
		"ALTER TABLE sessions ALTER COLUMN name varchar(20) COLLATE Latin1_General_CS_AS NULL",
	} {
		sqlServerExec(t, db, change, "DELETE FROM sessions", "INSERT INTO sessions VALUES (1, 1.23, 'x')", "ALTER VIEW v AS SELECT id, id AS b FROM users")
		err := s.Restore(context.Background(), nil)
		if err == nil || !strings.Contains(err.Error(), "is not as in the pristine copy") {
			t.Fatalf("%s: %v", change, err)
		}
		if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != 1 {
			t.Fatalf("%s: the refused restore changed the excluded table", change)
		}
	}
}

// TestSQLServerExcludedMemoryOptimized: an excluded memory-optimized table
// with rows that last is refused by the seed (no restore of the whole
// database could keep them); one with DURABILITY = SCHEMA_ONLY must be
// excluded, and such a restore empties it.
func TestSQLServerExcludedMemoryOptimized(t *testing.T) {
	ctx := context.Background()
	s, db := newSQLServerTest(t, Options{Exclude: []string{"sessions", "cache"}, Log: &ssLog{}})
	cleanSQLServerSeed(t, s)
	master := sqlServerTestDB(t, s.o.DSN, "")
	var dir string
	master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512))").Scan(&dir)
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" ADD FILEGROUP mo CONTAINS MEMORY_OPTIMIZED_DATA",
		"ALTER DATABASE "+qms(s.o.Working)+" ADD FILE (NAME = 'mo1', FILENAME = "+sqlServerString(ssJoin(dir, s.o.Working+"_mo1"))+") TO FILEGROUP mo")
	sqlServerExec(t, db, "CREATE TABLE users (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM users",
		"CREATE TABLE sessions (id int PRIMARY KEY NONCLUSTERED, n int NULL) WITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_AND_DATA)")
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "do not exclude it") {
		t.Fatalf("seed with an excluded memory-optimized table: %v", err)
	}
	sqlServerExec(t, db, "DROP TABLE sessions", "CREATE TABLE temp (id int PRIMARY KEY NONCLUSTERED) WITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_ONLY)")
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "exclude it") {
		t.Fatalf("seed with a SCHEMA_ONLY table not excluded: %v", err)
	}
	sqlServerExec(t, db, "EXEC sp_rename 'temp', 'cache'")
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO cache VALUES (1)", "ALTER VIEW v AS SELECT id, id AS b FROM users")
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
		t.Fatalf("%v differ", d)
	}
}

// TestSQLServerFallbackStopped: a restore of the whole database stopped
// during its RESTORE leaves the working database restoring; the next
// restore completes it, with Service Broker on as it was and the rows of
// excluded tables kept.
func TestSQLServerFallbackStopped(t *testing.T) {
	ctx := context.Background()
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"ALTER DATABASE CURRENT SET ENABLE_BROKER WITH ROLLBACK IMMEDIATE",
		"CREATE TABLE users (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM users",
		"CREATE TABLE big (id int IDENTITY PRIMARY KEY, pad char(4000) NOT NULL DEFAULT 'x')",
		"INSERT INTO big (pad) SELECT TOP (60000) 'y' FROM sys.all_objects a CROSS JOIN sys.all_objects b",
		"CREATE TABLE sessions (id int PRIMARY KEY)")
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "INSERT INTO sessions VALUES (1),(2)", "ALTER VIEW v AS SELECT id, id AS b FROM users")
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chaos.At = func(point string) {
		if point == "state.sqlserver.restore.offline" {
			go func() { time.Sleep(300 * time.Millisecond); cancel() }()
		}
	}
	err := s.Restore(cctx, nil)
	chaos.At = nil
	w, _ := s.database(ctx, s.o.Working)
	if err == nil || w == nil || w.State != "RESTORING" {
		t.Skipf("the RESTORE ended before it was stopped (%v)", err)
	}
	if !strings.Contains(err.Error(), "unusable until a restore completes") {
		t.Errorf("message: %v", err)
	}
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "stopped halfway") {
		t.Errorf("log: %s", log.String())
	}
	log.fellBack()
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	if w, _ = s.database(ctx, s.o.Working); w.State != "ONLINE" || !w.Broker {
		t.Fatalf("working database %s, Service Broker %t", w.State, w.Broker)
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != 2 {
		t.Fatalf("%d rows of the excluded table, want 2", n)
	}
	if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
		t.Fatalf("%v differ", d)
	}
	if w.UserAccess != "MULTI_USER" {
		t.Fatalf("the working database is %s after the restore that stopped was completed", w.UserAccess)
	}
}

// TestSQLServerRestoreWholeRestricted: a working database the user made
// RESTRICTED_USER stays so through a restore of the whole database that
// saves excluded rows (which keeps the logins out meanwhile).
func TestSQLServerRestoreWholeRestricted(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE users (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM users",
		"CREATE TABLE sessions (id int PRIMARY KEY)")
	p := pristineSQLServer(t, s)
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" SET RESTRICTED_USER WITH ROLLBACK IMMEDIATE")
	t.Cleanup(func() { master.Exec("ALTER DATABASE " + qms(s.o.Working) + " SET MULTI_USER") })
	db.SetMaxIdleConns(0)
	db.SetMaxIdleConns(2)
	sqlServerExec(t, db, "INSERT INTO sessions VALUES (1)", "ALTER VIEW v AS SELECT id, id AS b FROM users")
	if !restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored item by item")
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != 1 {
		t.Fatalf("%d rows of the excluded table, want 1", n)
	}
	if w, err := s.database(context.Background(), s.o.Working); err != nil || w.UserAccess != "RESTRICTED_USER" {
		t.Fatalf("the working database is %v after the restore (%v), not RESTRICTED_USER as the user set it", w, err)
	}
}

// TestSQLServerEndSessions: the sessions in the working database are
// ended, a session elsewhere is not.
func TestSQLServerEndSessions(t *testing.T) {
	s, db := newSQLServerTest(t, Options{}, "")
	if !mustSQLServerInfo(t, s).Sysadmin {
		t.Skip("the login is not sysadmin")
	}
	ctx := context.Background()
	in, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := sqlServerTestDB(t, s.o.DSN, "").Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, c := range []*sql.Conn{in, out} {
		if _, err := c.ExecContext(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	w, err := s.database(ctx, s.o.Working)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.conn(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	s.endSessions(ctx, m, w.ID)
	m.Close()
	if _, err := in.ExecContext(ctx, "SELECT 1"); err == nil {
		t.Error("a session in the working database was not ended")
	}
	if _, err := out.ExecContext(ctx, "SELECT 1"); err != nil {
		t.Errorf("a session in master was ended: %v", err)
	}
}

// TestSQLServerFallbackKilledSaved: a restore of the whole database
// stopped right after it saved the rows of excluded tables (no deferred
// function runs, as when DriftGate is killed) leaves the working database
// RESTRICTED_USER; the next restore drops that save and lets every login
// in again when it is done, the user access the stopped one found.
func TestSQLServerFallbackKilledSaved(t *testing.T) {
	ctx := context.Background()
	s, db, _ := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE users (id int PRIMARY KEY)", "CREATE VIEW v AS SELECT id FROM users",
		"CREATE TABLE sessions (id int PRIMARY KEY)")
	p := pristineSQLServer(t, s)
	// The second time the save notes no user access, as one of an
	// earlier version.
	for i, older := range []bool{false, true} {
		sqlServerExec(t, db, fmt.Sprintf("INSERT INTO sessions VALUES (%d)", i+1), fmt.Sprintf("ALTER VIEW v AS SELECT id, %d AS b FROM users", i))
		killSQLServerSaved(t, s, older)
		db.SetMaxIdleConns(0)
		db.SetMaxIdleConns(2)
		if w, err := s.database(ctx, s.o.Working); err != nil || w.UserAccess != "MULTI_USER" {
			t.Fatalf("%t: the working database is %+v (%v) after the next restore, want MULTI_USER", older, w, err)
		}
		if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != int64(i+1) {
			t.Fatalf("%t: %d rows of the excluded table, want %d", older, n, i+1)
		}
		if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
			t.Fatalf("%t: %v differ", older, d)
		}
	}
}

// killSQLServerSaved stops a restore of the whole database right after it
// saved the rows of excluded tables, without its deferred functions (as a
// kill), optionally drops the user access the save noted, and restores
// again.
func killSQLServerSaved(t *testing.T, s *SQLServer, older bool) {
	t.Helper()
	ctx := context.Background()
	type killed struct{}
	chaos.At = func(point string) {
		if point == "state.sqlserver.restore.saved" {
			panic(killed{})
		}
	}
	func() {
		defer func() {
			chaos.At = nil
			if r := recover(); r != (killed{}) {
				t.Fatalf("not stopped at the save: %v", r)
			}
		}()
		s.Restore(ctx, nil)
	}()
	if w, _ := s.database(ctx, s.o.Working); w == nil || w.UserAccess != "RESTRICTED_USER" {
		t.Fatalf("the working database after the stop: %+v", w)
	}
	if older {
		sqlServerExec(t, sqlServerTestDB(t, s.o.DSN, ""), "DELETE FROM "+qms(s.excludedDB())+".dbo.driftgate_saved WHERE kind = N'access'")
	}
	s.forget()
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
}
