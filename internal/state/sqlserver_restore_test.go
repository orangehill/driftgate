package state

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
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
	master := sqlServerTestDB(t, s.o.DSN, "")
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
// fingerprints them as the tracker does, b.N times after one to warm up,
// and reports the time of a restore alone.
func benchRestore(b *testing.B, s *SQLServer, change func(n int), items func(n int) []string) {
	ctx := context.Background()
	mustSQLServerFingerprint(b, s)
	var took time.Duration
	for n := range b.N + 1 { // the first one warms up (connections, catalogs)
		change(n)
		start := time.Now()
		if err := s.Restore(ctx, items(n)); err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.ResetTimer()
		} else {
			took += time.Since(start)
		}
		if _, err := s.Fingerprint(ctx, items(n)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(took.Microseconds())/1000/float64(b.N), "ms/restore")
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
