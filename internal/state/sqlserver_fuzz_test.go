package state

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// newSQLServerFuzz is newSQLServerSeeded for a fuzz test. The fuzzing
// engine ends its workers without their cleanup, so their databases have
// names of their own kind, and what runs that ended over 15 minutes ago
// left is dropped first.
func newSQLServerFuzz(f *testing.F, o Options, setup ...string) (*SQLServer, *sql.DB, *ssLog) {
	f.Helper()
	dsn := sqlServerTestDSN(f)
	master := sqlServerTestDB(f, dsn, "")
	rows, err := master.Query("SELECT name FROM sys.databases WHERE name LIKE 'dgt[_]ssfz[_]%' AND name NOT LIKE '%[_]p%' AND create_date < DATEADD(minute, -15, GETDATE())")
	if err != nil {
		f.Fatal(err)
	}
	var stale []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		stale = append(stale, n)
	}
	rows.Close()
	paths := &ssPaths{}
	master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512)), ISNULL(CAST(SERVERPROPERTY('InstanceDefaultBackupPath') AS nvarchar(512)), CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(512)))").Scan(&paths.data, &paths.backup)
	for _, n := range stale {
		for _, db := range []string{n, n + "_p", n + "_p_dgnew", n + "_p_dgx"} {
			dropSQLServerDatabase(master, db)
		}
		old := &SQLServer{o: Options{Working: n, Pristine: n + "_p"}}
		for _, file := range old.backupSlots(paths) {
			master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(file))
		}
	}
	work := strings.Replace(sqlServerTestName(), "dgt_ss_", "dgt_ssfz_", 1)
	sqlServerExec(f, master, "CREATE DATABASE "+qms(work))
	f.Cleanup(func() {
		dropSQLServerDatabase(master, work)
		dropSQLServerDatabase(master, work+"_p")
	})
	log := &ssLog{}
	o.Name, o.DSN, o.Working, o.Pristine, o.Log = "db", dsn, work, work+"_p", log
	s, err := NewSQLServer(o)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.Close() })
	cleanSQLServerSeed(f, s)
	db := sqlServerTestDB(f, dsn, work)
	sqlServerExec(f, db, setup...)
	if err := s.Seed(context.Background()); err != nil {
		f.Fatalf("seed: %v", err)
	}
	return s, db, log
}

// FuzzSQLServerRestore: whatever a run of writes does to the working
// database, the restore of the items that changed (item by item or the
// whole database) leaves it as the pristine copy: the same fingerprint,
// no row on one side only (EXCEPT both ways over the encoded rows),
// identity columns and sequences where the pristine copy's stand, and no
// row that breaks a constraint.
//
//	go test ./internal/state/ -run '^$' -fuzz FuzzSQLServerRestore -fuzztime 60s -parallel 2
func FuzzSQLServerRestore(f *testing.F) {
	defer func(n int64) { sqlServerDeltaRows = n }(sqlServerDeltaRows)
	sqlServerDeltaRows = 100
	s, db, log := newSQLServerFuzz(f, Options{Exclude: []string{"scratch"}},
		"CREATE TABLE parent (id int IDENTITY PRIMARY KEY, v int NULL CHECK (v IS NULL OR v >= 0), s nvarchar(20) COLLATE Latin1_General_CI_AS NULL)",
		"INSERT INTO parent (v, s) VALUES (1, N'a'), (2, N'b'), (3, NULL)",
		"CREATE TABLE child (id int PRIMARY KEY, parent_id int NULL REFERENCES parent(id) ON DELETE CASCADE, f float NULL)",
		"INSERT INTO child VALUES (1, 1, 0.5), (2, 2, NULL)",
		`CREATE TABLE big (k int PRIMARY KEY, s varchar(20) COLLATE Latin1_General_CI_AS NULL, f float NULL)`,
		`WITH n AS (SELECT TOP (300) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects) INSERT INTO big SELECT i, CONCAT('s', i), i / 3.0 FROM n`,
		"CREATE TABLE heap (a int NULL, b nvarchar(10) NULL)", "INSERT INTO heap VALUES (1, N'x'), (1, N'x')",
		"CREATE TABLE fresh (id int IDENTITY(5, 2) PRIMARY KEY, v int NULL)",
		"CREATE TABLE scratch (id int PRIMARY KEY)",
		"CREATE SEQUENCE seq AS int START WITH 1 MINVALUE 1 MAXVALUE 20 CYCLE CACHE 5",
		"CREATE TRIGGER parent_t ON parent AFTER INSERT, DELETE AS UPDATE heap SET a = a + 1",
		"CREATE VIEW pv AS SELECT id FROM parent")
	p := pristineSQLServer(f, s)
	ctx := context.Background()
	pc, err := s.conn(ctx, s.o.Pristine)
	if err != nil {
		f.Fatal(err)
	}
	pinv, err := s.catalog(ctx, pc)
	if err != nil {
		f.Fatal(err)
	}
	pvals, err := values(ctx, pc)
	pc.Close()
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{{0, 1, 2}, {1, 3, 4, 2, 0, 0}, {5, 7, 1, 6, 9, 9, 7, 3, 3}, {4, 0, 0, 9, 2, 0}, {10, 4, 4, 11, 1, 1, 12, 0, 0, 13, 0, 0}, {14, 0, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 60 {
			ops = ops[:60]
		}
		for i := 0; i+2 < len(ops); i += 3 {
			a, b := int(ops[i+1]), int(ops[i+2])
			var q string
			switch ops[i] % 15 {
			case 0:
				q = fmt.Sprintf("INSERT INTO parent (v, s) VALUES (%d, N'%c')", a, 'a'+rune(b%26))
			case 1:
				q = fmt.Sprintf("UPDATE parent SET v = %d WHERE id %% 5 = %d", b, a%5)
			case 2:
				q = fmt.Sprintf("DELETE FROM parent WHERE id %% 7 = %d", a%7)
			case 3:
				q = "DECLARE @x int = NEXT VALUE FOR seq"
			case 4:
				q = "BEGIN TRAN; INSERT INTO fresh (v) VALUES (1); ROLLBACK"
			case 5:
				q = fmt.Sprintf("INSERT INTO child VALUES (%d, %d, %d)", 100+a, b%10, a)
			case 6:
				q = fmt.Sprintf("UPDATE big SET s = UPPER(s) WHERE k = %d", a+1)
			case 7:
				q = fmt.Sprintf("UPDATE big SET f = -1e0 * 0e0, s = s + ' ' WHERE k = %d", b+1)
			case 8:
				q = fmt.Sprintf("DELETE FROM big WHERE k %% 50 = %d", a%50)
			case 9:
				q = fmt.Sprintf("INSERT INTO big VALUES (%d, NULL, %d)", 1000+a*256+b, a)
			case 10:
				q = "INSERT INTO fresh (v) VALUES (1)"
			case 11:
				q = fmt.Sprintf("DBCC CHECKIDENT ('parent', RESEED, %d) WITH NO_INFOMSGS", 100+a)
			case 12:
				q = fmt.Sprintf("INSERT INTO heap VALUES (%d, NULL)", a%3)
			case 13:
				q = fmt.Sprintf("INSERT INTO scratch VALUES (%d)", a*256+b)
			case 14:
				q = "UPDATE child SET f = f"
			}
			db.Exec(q) // a write the constraints refuse is a write too
		}
		w := mustSQLServerFingerprint(t, s)
		items := Diff(w, p)
		if len(items) == 0 {
			return
		}
		if err := s.Restore(ctx, items); err != nil {
			t.Fatalf("restore of %v: %v", items, err)
		}
		if log.fellBack() {
			db.SetMaxIdleConns(0)
			db.SetMaxIdleConns(2)
		}
		if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
			t.Fatalf("after the restore of %v, %v differ", items, d)
		}
		for _, tb := range pinv.Tables {
			if tb.Excluded {
				continue
			}
			row, err := sqlServerRow(tb.Columns)
			if err != nil {
				t.Fatal(err)
			}
			wq := "SELECT " + row + " AS r FROM " + tb.qname()
			pq := "SELECT " + row + " AS r FROM " + qms(s.o.Pristine) + "." + tb.qname()
			for _, q := range []string{wq + " EXCEPT " + pq, pq + " EXCEPT " + wq} {
				if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM ("+q+") x"); n != 0 {
					t.Fatalf("%s: %d rows on one side only", tb.Item, n)
				}
			}
		}
		c, err := s.conn(ctx, s.o.Working)
		if err != nil {
			t.Fatal(err)
		}
		vals, err := values(ctx, c)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(vals.ident, pvals.ident) || !reflect.DeepEqual(vals.seq, pvals.seq) {
			t.Fatalf("identity and sequences %v %v, want %v %v", vals.ident, vals.seq, pvals.ident, pvals.seq)
		}
		rows, err := db.Query("DBCC CHECKCONSTRAINTS WITH ALL_CONSTRAINTS, NO_INFOMSGS")
		if err != nil {
			t.Fatal(err)
		}
		broken := rows.Next()
		rows.Close()
		if broken {
			t.Fatal("rows break a constraint after the restore")
		}
	})
}
