package state

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// tableHash fingerprints one table of the working database, nothing
// reused.
func tableHash(t *testing.T, s *SQLServer, table string) string {
	t.Helper()
	f := sqlServerFull(t, s)
	h, ok := f["table:"+table]
	if !ok {
		t.Fatalf("no item table:%s in %v", table, f)
	}
	return h
}

// TestSQLServerEncoding: rows that differ in any way hash differently,
// and the same rows in any order hash alike.
func TestSQLServerEncoding(t *testing.T) {
	s, db := newSQLServerTest(t, Options{}, "COLLATE SQL_Latin1_General_CP1_CI_AS")
	long := func(n int, last string) string {
		return "REPLICATE(CAST('a' AS varchar(max)), " + fmt.Sprint(n-1) + ") + '" + last + "'"
	}
	for _, tc := range []struct {
		name, columns string
		a, b          []string // rows, as VALUES lists
	}{
		{"split", "x nvarchar(10), y nvarchar(10)", []string{"N'ab', N'c'"}, []string{"N'a', N'bc'"}},
		{"null and empty", "x nvarchar(10)", []string{"NULL"}, []string{"N''"}},
		{"null and empty varbinary", "x varbinary(10)", []string{"NULL"}, []string{"0x"}},
		{"trailing space under CI", "x varchar(10)", []string{"'a'"}, []string{"'a '"}},
		{"case under CI", "x varchar(10)", []string{"'A'"}, []string{"'a'"}},
		{"float -0", "x float", []string{"0e0"}, []string{"-1e0 * 0e0"}},
		{"float last bit", "x float", []string{"0.1e0"}, []string{"0.1e0 + 1.3877787807814457e-17"}},
		{"real", "x real", []string{"CAST(1.5 AS real)"}, []string{"CAST(1.25 AS real)"}},
		{"datetime2", "x datetime2(7)", []string{"'2020-01-01T00:00:00.1234567'"}, []string{"'2020-01-01T00:00:00.1234568'"}},
		{"datetimeoffset", "x datetimeoffset", []string{"'2020-01-01T01:00:00+01:00'"}, []string{"'2020-01-01T00:00:00+00:00'"}},
		{"time", "x time(7)", []string{"'01:02:03.1234567'"}, []string{"'01:02:03.1234568'"}},
		{"decimal", "x decimal(10, 2)", []string{"1.10"}, []string{"1.11"}},
		{"money", "x money", []string{"1.5"}, []string{"1.5001"}},
		{"uniqueidentifier", "x uniqueidentifier", []string{"'6F9619FF-8B86-D011-B42D-00C04FC964FF'"}, []string{"'6F9619FF-8B86-D011-B42D-00C04FC964FE'"}},
		{"sql_variant type", "x sql_variant", []string{"CAST(1 AS int)"}, []string{"CAST(1 AS bigint)"}},
		{"sql_variant text", "x sql_variant", []string{"CAST(N'1' AS nvarchar(10))"}, []string{"CAST('1' AS varchar(10))"}},
		{"sql_variant collation", "x sql_variant", []string{"CAST('a' AS varchar(10)) COLLATE Latin1_General_CS_AS"}, []string{"CAST('a' AS varchar(10)) COLLATE Latin1_General_CI_AS"}},
		{"sql_variant null", "x sql_variant", []string{"NULL"}, []string{"CAST(N'' AS nvarchar(10))"}},
		{"xml", "x xml", []string{`N'<a b="1"/>'`}, []string{`N'<a b="2"/>'`}},
		{"text", "x text, y ntext, z image", []string{"'abc', N'abc', 0x01"}, []string{"'abc', N'abd', 0x01"}},
		{"hierarchyid", "x hierarchyid", []string{"hierarchyid::Parse('/1/2/')"}, []string{"hierarchyid::Parse('/1/3/')"}},
		{"geography", "x geography", []string{"geography::Parse('POINT(1 2)')"}, []string{"geography::Parse('POINT(1 3)')"}},
		{"bit", "x bit", []string{"0"}, []string{"1"}},
		{"heap duplicates", "x int", []string{"1", "1", "2", "2"}, []string{"3", "3", "4", "4"}},
		{"heap one duplicate more", "x int", []string{"1", "1", "2"}, []string{"1", "2", "2"}},
		{"varbinary(8000) last byte", "x varbinary(8000)", []string{"CAST(" + long(8000, "b") + " AS varbinary(8000))"},
			[]string{"CAST(" + long(8000, "c") + " AS varbinary(8000))"}},
		{"two long columns", "x varchar(8000), y varchar(8000)", []string{long(8000, "a") + ", " + long(8000, "b")}, []string{long(8000, "a") + ", " + long(8000, "c")}},
		{"sql_variant 8000 bytes", "x sql_variant", []string{"CAST(CAST(" + long(7995, "a") + " AS varbinary(max)) + 0x0102030405 AS varbinary(8000))"},
			[]string{"CAST(CAST(" + long(7995, "a") + " AS varbinary(max)) + 0xFFFFFFFFFF AS varbinary(8000))"}},
		{"sql_variant 4000 characters", "x sql_variant", []string{"CAST(REPLICATE(N'a', 3998) + N'bc' AS nvarchar(4000))"}, []string{"CAST(REPLICATE(N'a', 3998) + N'bd' AS nvarchar(4000))"}},
		{"sql_variant length", "x sql_variant", []string{"CAST('a' AS varchar(10))"}, []string{"CAST('a' AS varchar(20))"}},
		{"max column", "x nvarchar(max)", []string{"CAST(" + long(100000, "b") + " AS nvarchar(max))"}, []string{"CAST(" + long(100000, "c") + " AS nvarchar(max))"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := "e_" + strings.ReplaceAll(tc.name, " ", "_")
			mustExec(t, db, "CREATE TABLE "+qms(table)+" ("+tc.columns+")")
			fill := func(rows []string) string {
				mustExec(t, db, "DELETE FROM "+qms(table))
				for _, r := range rows {
					mustExec(t, db, "INSERT INTO "+qms(table)+" VALUES ("+r+")")
				}
				return tableHash(t, s, "dbo."+table)
			}
			a, b := fill(tc.a), fill(tc.b)
			if a == b {
				t.Errorf("%v and %v hash alike", tc.a, tc.b)
			}
			// The same rows in another order hash alike.
			rev := append([]string{}, tc.a...)
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			if c := fill(rev); c != a {
				t.Errorf("%v in another order hashes differently", tc.a)
			}
		})
	}
}

// TestSQLServerItems: the items, what their hash covers, exclusion and
// schemas other than dbo.
func TestSQLServerItems(t *testing.T) {
	s, db := newSQLServerTest(t, Options{Exclude: []string{"Logs", "audit.*"}})
	mustExec(t, db,
		"CREATE SCHEMA app", "CREATE SCHEMA audit",
		"CREATE TABLE app.orders (id int IDENTITY(10, 5) PRIMARY KEY, total decimal(10, 2) NOT NULL CONSTRAINT ck_total CHECK (total >= 0), note nvarchar(50) NULL)",
		"CREATE TABLE app.lines (id int PRIMARY KEY, order_id int NOT NULL CONSTRAINT fk_order REFERENCES app.orders (id) ON DELETE CASCADE)",
		"CREATE TABLE dbo.logs (id int)", "CREATE TABLE audit.trail (id int)",
		"CREATE VIEW app.big AS SELECT id FROM app.orders WHERE total > 100",
		"CREATE TRIGGER app.tr_orders ON app.orders AFTER INSERT AS SET NOCOUNT ON",
		"CREATE TRIGGER ddl_log ON DATABASE FOR CREATE_TABLE AS SET NOCOUNT ON",
		"CREATE SEQUENCE app.numbers AS bigint START WITH 100 CACHE 10",
		"CREATE PROCEDURE app.p AS SELECT 1", "CREATE FUNCTION app.f() RETURNS int AS BEGIN RETURN 1 END",
		"CREATE TYPE app.money2 FROM decimal(12, 2) NOT NULL", "CREATE SYNONYM app.o FOR app.orders",
	)
	f := sqlServerFull(t, s)
	var items []string
	for it := range f {
		items = append(items, it)
	}
	want := []string{"database:", "function:app.f", "procedure:app.p", "schema:app", "schema:audit", "sequence:app.numbers", "synonym:app.o",
		"table:app.lines", "table:app.orders", "trigger:app.tr_orders", "trigger:database.ddl_log", "type:app.money2", "view:app.big"}
	if !sameItems(items, want) {
		t.Fatalf("items %v, want %v", items, want)
	}
	// Each change shows in its item only.
	for _, tc := range []struct{ stmt, item string }{
		{"INSERT INTO dbo.logs VALUES (1)", ""},
		{"INSERT INTO audit.trail VALUES (1)", ""},
		{"ALTER TABLE app.orders NOCHECK CONSTRAINT ck_total", "table:app.orders"},
		{"ALTER TABLE app.lines NOCHECK CONSTRAINT fk_order", "table:app.lines"},
		{"DISABLE TRIGGER app.tr_orders ON app.orders", "trigger:app.tr_orders"},
		{"DISABLE TRIGGER ddl_log ON DATABASE", "trigger:database.ddl_log"},
		{"ALTER VIEW app.big AS SELECT id FROM app.orders WHERE total > 200", "view:app.big"},
		{"ALTER PROCEDURE app.p AS SELECT 2", "procedure:app.p"},
		{"GRANT SELECT ON app.orders TO public", "table:app.orders"},
		{"EXEC sp_addextendedproperty N'MS_Description', N'x', N'SCHEMA', N'app', N'TABLE', N'lines'", "table:app.lines"},
		{"CREATE INDEX ix_note ON app.orders (note) WHERE note IS NOT NULL", "table:app.orders"},
		{"DECLARE @x bigint = NEXT VALUE FOR app.numbers", "sequence:app.numbers"},
		{"INSERT INTO app.orders (total) VALUES (1)", "table:app.orders"},
		{"BEGIN TRAN; INSERT INTO app.orders (total) VALUES (2); ROLLBACK", "table:app.orders"}, // the identity moved
		{"DROP SYNONYM app.o", "synonym:app.o"},
		{"DECLARE @v sql_variant = CAST(1.0000001 AS float); EXEC sp_addextendedproperty N'ver', @v, N'SCHEMA', N'app', N'TABLE', N'orders'", "table:app.orders"},
		{"DECLARE @v sql_variant = CAST(1.0000002 AS float); EXEC sp_updateextendedproperty N'ver', @v, N'SCHEMA', N'app', N'TABLE', N'orders'", "table:app.orders"},
		{"DECLARE @v sql_variant = CAST('2020-01-01T10:00:01' AS datetime); EXEC sp_updateextendedproperty N'ver', @v, N'SCHEMA', N'app', N'TABLE', N'orders'", "table:app.orders"},
		{"DECLARE @v sql_variant = CAST('2020-01-01T10:00:59' AS datetime); EXEC sp_updateextendedproperty N'ver', @v, N'SCHEMA', N'app', N'TABLE', N'orders'", "table:app.orders"},
	} {
		mustExec(t, db, tc.stmt)
		g := sqlServerFull(t, s)
		d := Diff(f, g)
		if tc.item == "" && len(d) > 0 || tc.item != "" && !reflect.DeepEqual(d, []string{tc.item}) {
			t.Errorf("%s: changed %v, want %q", tc.stmt, d, tc.item)
		}
		f = g
	}
}

// TestSQLServerIdentity: what an identity column and a sequence hand out
// next is part of the fingerprint, also where no row and no log record
// shows it.
func TestSQLServerIdentity(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE fresh (id int IDENTITY(10, 5), v int)", "CREATE TABLE used (id int IDENTITY, v int)",
		"INSERT INTO used (v) VALUES (1)", "CREATE SEQUENCE q START WITH 1 CACHE 50")
	for _, tc := range []struct{ stmt, item string }{
		{"DBCC CHECKIDENT('fresh', RESEED, 50) WITH NO_INFOMSGS", "table:dbo.fresh"}, // last_value stays NULL
		{"BEGIN TRAN; INSERT INTO used (v) VALUES (2); ROLLBACK", "table:dbo.used"},
		{"DELETE FROM used", "table:dbo.used"},
		{"TRUNCATE TABLE used", "table:dbo.used"}, // no rows either way; last_value NULL again
		{"DECLARE @x int = NEXT VALUE FOR q", "sequence:dbo.q"},
		{"DECLARE @x int = NEXT VALUE FOR q", "sequence:dbo.q"}, // from the cache: no log record
		{"ALTER SEQUENCE q RESTART WITH 1", "sequence:dbo.q"},
	} {
		before := mustSQLServerFingerprint(t, s)
		mustExec(t, db, tc.stmt)
		after := mustSQLServerFingerprint(t, s)
		if s.Reused() {
			t.Errorf("%s: fingerprint reused", tc.stmt)
		}
		if d := Diff(before, after); !reflect.DeepEqual(d, []string{tc.item}) {
			t.Errorf("%s: changed %v, want %s", tc.stmt, d, tc.item)
		}
	}
}

// TestSQLServerWritePaths: after every way of writing, the fingerprint is
// the one a full read makes: never reused from before the write, and
// every table the counters missed would show here.
func TestSQLServerWritePaths(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the test login cannot read the server's performance state")
	}
	mustExec(t, db,
		"CREATE TABLE p (id int PRIMARY KEY, v int, txt nvarchar(max))",
		"CREATE TABLE c (id int PRIMARY KEY, p_id int REFERENCES p (id) ON DELETE CASCADE ON UPDATE CASCADE, v int)",
		"CREATE TABLE h (v int, txt varchar(100))", // a heap
		"CREATE TABLE logt (id int IDENTITY PRIMARY KEY, what nvarchar(20))",
		"CREATE TABLE src (id int PRIMARY KEY, v int)",
		"CREATE TABLE sw (id int PRIMARY KEY, v int)", "CREATE TABLE sw2 (id int PRIMARY KEY, v int)",
		"CREATE TABLE tx (id int PRIMARY KEY, txt text, ntx ntext, img image)", "INSERT INTO tx VALUES (1, REPLICATE('a', 100), N'abc', 0x0102)",
		"CREATE TABLE x (id int PRIMARY KEY, doc xml)", "INSERT INTO x VALUES (1, N'<a><b>1</b></a>')",
		"CREATE TABLE sp (id int PRIMARY KEY, a int SPARSE NULL, b nvarchar(20) SPARSE NULL, cs xml COLUMN_SET FOR ALL_SPARSE_COLUMNS)", "INSERT INTO sp (id, a) VALUES (1, 1)",
		"CREATE VIEW vsrc AS SELECT id, v FROM src",
		"CREATE TRIGGER tr_p ON p AFTER UPDATE AS INSERT INTO logt (what) SELECT N'p' FROM inserted",
		"INSERT INTO p VALUES (1, 1, REPLICATE(CAST(N'L' AS nvarchar(max)), 20000)), (2, 2, N'short')",
		"INSERT INTO c VALUES (1, 1, 1), (2, 2, 2)", "INSERT INTO h VALUES (1, 'a'), (1, 'a')",
		"INSERT INTO src VALUES (1, 10), (3, 30)", "INSERT INTO sw VALUES (1, 1)",
	)
	bulk := func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, err := tx.Prepare(mssql.CopyIn("h", mssql.BulkOptions{}, "v", "txt"))
		if err != nil {
			tx.Rollback()
			return err
		}
		for i := range 3 {
			if _, err := st.Exec(i, "bulk"); err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := st.Exec(); err != nil {
			tx.Rollback()
			return err
		}
		st.Close()
		return tx.Commit()
	}
	for _, tc := range []struct {
		name, stmt string
		run        func() error
		changed    bool // the data changed
	}{
		{name: "insert", stmt: "INSERT INTO src VALUES (5, 50)", changed: true},
		{name: "update", stmt: "UPDATE src SET v = 11 WHERE id = 1", changed: true},
		{name: "update to the same value", stmt: "UPDATE src SET v = v"},
		{name: "update of no row", stmt: "UPDATE src SET v = 0 WHERE 1 = 0"},
		{name: "delete", stmt: "DELETE FROM src WHERE id = 5", changed: true},
		{name: "heap update", stmt: "UPDATE h SET txt = 'b' WHERE txt = 'a'", changed: true},
		{name: "heap delete", stmt: "DELETE TOP (1) FROM h", changed: true},
		{name: "merge", stmt: "MERGE src AS t USING (VALUES (1, 12), (7, 70)) AS n (id, v) ON t.id = n.id WHEN MATCHED THEN UPDATE SET v = n.v WHEN NOT MATCHED THEN INSERT VALUES (n.id, n.v);", changed: true},
		{name: "output into", stmt: "UPDATE src SET v = v + 1 OUTPUT N'out' INTO logt (what) WHERE id = 1", changed: true},
		{name: "bulk copy", run: bulk, changed: true},
		{name: "sp_executesql", stmt: "EXEC sp_executesql N'INSERT INTO src VALUES (@id, 1)', N'@id int', @id = 9", changed: true},
		{name: "truncate", stmt: "TRUNCATE TABLE h", changed: true},
		{name: "switch", stmt: "ALTER TABLE sw SWITCH TO sw2", changed: true},
		{name: "cascade delete", stmt: "DELETE FROM p WHERE id = 2", changed: true},
		{name: "cascade update and trigger", stmt: "UPDATE p SET id = 3 WHERE id = 1", changed: true},
		{name: "lob write", stmt: "UPDATE p SET txt.WRITE(N'Q', 10, 1)", changed: true},
		{name: "lob same length", stmt: "UPDATE p SET txt = REPLICATE(CAST(N'M' AS nvarchar(max)), 20000)", changed: true},
		{name: "rolled back", stmt: "BEGIN TRAN; UPDATE src SET v = 99; ROLLBACK"},
		{name: "rebuild, then one update", stmt: "ALTER INDEX ALL ON src REBUILD; UPDATE src SET v = v + 100 WHERE id = 1", changed: true},
		{name: "select into", stmt: "SELECT * INTO copy FROM src", changed: true},
		{name: "writetext", stmt: "DECLARE @p varbinary(16); SELECT @p = TEXTPTR(txt) FROM tx WHERE id = 1; WRITETEXT tx.txt @p 'zzz'", changed: true},
		{name: "updatetext", stmt: "DECLARE @p varbinary(16); SELECT @p = TEXTPTR(txt) FROM tx WHERE id = 1; UPDATETEXT tx.txt @p 0 1 'Q'", changed: true},
		{name: "updatetext ntext", stmt: "DECLARE @p varbinary(16); SELECT @p = TEXTPTR(ntx) FROM tx WHERE id = 1; UPDATETEXT tx.ntx @p 0 1 N'Q'", changed: true},
		{name: "writetext image", stmt: "DECLARE @p varbinary(16); SELECT @p = TEXTPTR(img) FROM tx WHERE id = 1; WRITETEXT tx.img @p 0xFFFF", changed: true},
		{name: "xml modify", stmt: `UPDATE x SET doc.modify('replace value of (/a/b/text())[1] with "2"')`, changed: true},
		{name: "column set", stmt: "UPDATE sp SET cs = N'<b>hi</b>'", changed: true},
		{name: "view update", stmt: "UPDATE vsrc SET v = v + 1", changed: true},
		{name: "delayed durability", stmt: "ALTER DATABASE CURRENT SET DELAYED_DURABILITY = FORCED; INSERT INTO src VALUES (11, 1); ALTER DATABASE CURRENT SET DELAYED_DURABILITY = DISABLED", changed: true},
	} {
		before := mustSQLServerFingerprint(t, s)
		if again := mustSQLServerFingerprint(t, s); !reflect.DeepEqual(before, again) {
			t.Fatalf("%s: two fingerprints without a write differ: %v", tc.name, Diff(before, again))
		}
		var err error
		if tc.run != nil {
			err = tc.run()
		} else {
			_, err = db.Exec(tc.stmt)
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		after := mustSQLServerFingerprint(t, s)
		if full := sqlServerFull(t, s); !reflect.DeepEqual(after, full) {
			t.Errorf("%s: the fingerprint differs from a full one in %v (reused %v)", tc.name, Diff(after, full), s.Reused())
		}
		if changed := len(Diff(before, after)) > 0; changed != tc.changed {
			t.Errorf("%s: changed %v (%v), want %v", tc.name, changed, Diff(before, after), tc.changed)
		}
		if tc.changed && s.Reused() {
			t.Errorf("%s: fingerprint reused", tc.name)
		}
	}
}

func mustSQLServerInfo(t testing.TB, s *SQLServer) *sqlServerInfo {
	t.Helper()
	i, err := s.server(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

// TestSQLServerReuse: an unchanged database is not read again; a write
// made while the tables are read shows next time; the tables the counters
// say are unchanged are not read again.
func TestSQLServerReuse(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the test login cannot read the server's performance state")
	}
	mustExec(t, db, "CREATE TABLE a (id int PRIMARY KEY, v int)", "CREATE TABLE b (id int PRIMARY KEY, v int)",
		"INSERT INTO a VALUES (1, 1)", "INSERT INTO b VALUES (1, 1)")
	f := mustSQLServerFingerprint(t, s)
	for range 3 { // the first reads of a new database may write to its log
		f = mustSQLServerFingerprint(t, s)
	}
	g := mustSQLServerFingerprint(t, s)
	if !s.Reused() || !reflect.DeepEqual(f, g) {
		t.Fatalf("an unchanged database was read again (reused %v, %v)", s.Reused(), Diff(f, g))
	}
	if s.Mode() == "" {
		t.Fatal("no mode")
	}

	// A write after the signals, before the tables are read: the tables
	// are read again, and the fingerprint has it.
	writes := 0
	sqlServerBeforeScan = func() {
		if writes == 0 {
			db.Exec("UPDATE a SET v = 2")
		}
		writes++
	}
	mustExec(t, db, "UPDATE b SET v = 2")
	h := mustSQLServerFingerprint(t, s)
	sqlServerBeforeScan = nil
	if writes != 2 {
		t.Fatalf("the tables were read %d times, want 2", writes)
	}
	if full := sqlServerFull(t, s); !reflect.DeepEqual(h, full) {
		t.Fatalf("differs from a full fingerprint: %v", Diff(h, full))
	}
	// Written every time: no fingerprint.
	mustExec(t, db, "UPDATE b SET v = 3")
	sqlServerBeforeScan = func() { db.Exec("UPDATE a SET v = v + 1") }
	_, err := s.Fingerprint(context.Background(), nil)
	sqlServerBeforeScan = nil
	if err == nil || !strings.Contains(err.Error(), "no consistent fingerprint") {
		t.Fatalf("a database written during every read: %v", err)
	}

	// Only the written table is read again: what is kept of the other one
	// stays (here a made-up content, to tell).
	mustSQLServerFingerprint(t, s)
	for id, k := range s.last.content {
		if k.item == "table:dbo.b" {
			k.content = "remembered"
			s.last.content[id] = k
		}
	}
	mustExec(t, db, "UPDATE a SET v = 3")
	mustSQLServerFingerprint(t, s)
	for _, k := range s.last.content {
		if (k.content == "remembered") != (k.item == "table:dbo.b") {
			t.Fatalf("%s: content %q (b is not to be read again, a is)", k.item, k.content)
		}
	}
}

// TestSQLServerOpenWrites: a fingerprint waits for a transaction that
// wrote; one still open after the wait is not kept for reuse.
func TestSQLServerOpenWrites(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the test login cannot read the server's performance state")
	}
	mustExec(t, db, "CREATE TABLE a (id int PRIMARY KEY, v int)", "INSERT INTO a VALUES (1, 1)")
	mustSQLServerFingerprint(t, s)
	defer func(d time.Duration) { sqlServerOpenWait = d }(sqlServerOpenWait)

	open := func(commitAfter time.Duration) chan error {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO a VALUES (2, 2)"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			time.Sleep(commitAfter)
			done <- tx.Commit()
		}()
		return done
	}
	// Committed within the wait: the fingerprint sees the row and is kept.
	sqlServerOpenWait = 5 * time.Second
	done := open(300 * time.Millisecond)
	f := mustSQLServerFingerprint(t, s)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.last == nil {
		t.Fatal("not kept although the transaction ended within the wait")
	}
	if full := sqlServerFull(t, s); !reflect.DeepEqual(f, full) {
		t.Fatalf("differs from a full fingerprint: %v", Diff(f, full))
	}
	// Still open after the wait: no fingerprint (its rows would be read
	// before the commit, its other writes maybe after), nothing kept.
	sqlServerOpenWait = 100 * time.Millisecond
	mustExec(t, db, "ALTER DATABASE CURRENT SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE") // the scan does not wait for the row
	db.SetMaxIdleConns(0)
	mustSQLServerFingerprint(t, s)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE a SET v = 5 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fingerprint(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "stayed open") {
		t.Fatalf("a fingerprint while a write stays open: %v", err)
	}
	if s.last != nil {
		t.Fatal("kept a fingerprint taken while a write was open")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	g := mustSQLServerFingerprint(t, s)
	if s.Reused() {
		t.Fatal("reused after an open write")
	}
	if full := sqlServerFull(t, s); !reflect.DeepEqual(g, full) {
		t.Fatalf("differs from a full fingerprint: %v", Diff(g, full))
	}
}

// makeSQLServerPristine copies the working database to the pristine one by
// BACKUP and RESTORE (as a seed does), marked and read-only.
func makeSQLServerPristine(t *testing.T, s *SQLServer) {
	t.Helper()
	mark, err := s.markValue(context.Background(), "", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	master := sqlServerTestDB(t, s.o.DSN, "")
	var data string
	if err := master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400))").Scan(&data); err != nil {
		t.Fatal(err)
	}
	sep := "/"
	if strings.Contains(data, `\`) {
		sep = `\`
	}
	data = strings.TrimRight(data, sep) + sep
	bak := data + s.o.Pristine + ".bak"
	stmts := []string{
		"BACKUP DATABASE " + qms(s.o.Working) + " TO DISK = " + sqlServerString(bak) + " WITH COPY_ONLY, INIT, FORMAT",
		"RESTORE DATABASE " + qms(s.o.Pristine) + " FROM DISK = " + sqlServerString(bak) + " WITH MOVE " + sqlServerString(s.o.Working) + " TO " +
			sqlServerString(data+s.o.Pristine+".mdf") + ", MOVE " + sqlServerString(s.o.Working+"_log") + " TO " + sqlServerString(data+s.o.Pristine+".ldf"),
		"EXEC " + qms(s.o.Pristine) + ".sys.sp_addextendedproperty @name = N'driftgate_pristine', @value = " + sqlServerString(mark),
		"ALTER DATABASE " + qms(s.o.Pristine) + " SET READ_ONLY WITH ROLLBACK IMMEDIATE",
	}
	for _, q := range stmts {
		if _, err := master.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(bak))
}

// TestSQLServerRowversion: an UPDATE that changes nothing but the
// rowversion changes the table's item; the new values a restore gives
// the rows do not.
func TestSQLServerRowversion(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE r (id int PRIMARY KEY, v int, rv rowversion)", "INSERT INTO r (id, v) VALUES (1, 1), (2, 2)")
	makeSQLServerPristine(t, s)
	f := mustSQLServerFingerprint(t, s)
	c, err := s.conn(context.Background(), s.o.Pristine)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.fingerprint(context.Background(), c, s.o.Pristine, nil, nil, nil, nil)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(f, p.f); len(d) > 0 {
		t.Fatalf("the working database and its copy differ in %v", d)
	}
	mustExec(t, db, "UPDATE r SET v = v WHERE id = 1")
	g := mustSQLServerFingerprint(t, s)
	if d := Diff(f, g); !reflect.DeepEqual(d, []string{"table:dbo.r"}) {
		t.Fatalf("an UPDATE of the rowversion only changed %v", d)
	}
	// As after a restore: the values are new, and remembered.
	wc, err := s.conn(context.Background(), s.o.Working)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := s.inventory(context.Background(), wc)
	if err == nil {
		err = s.rememberRowversions(context.Background(), wc, inv.Tables)
	}
	wc.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.restored([]string{"table:dbo.r"})
	h := mustSQLServerFingerprint(t, s)
	if d := Diff(f, h); len(d) > 0 {
		t.Fatalf("the values a restore left show as a change: %v", d)
	}
	mustExec(t, db, "UPDATE r SET v = v WHERE id = 2")
	if d := Diff(f, mustSQLServerFingerprint(t, s)); !reflect.DeepEqual(d, []string{"table:dbo.r"}) {
		t.Fatalf("an UPDATE of the rowversion only after a restore changed %v", d)
	}
	// A new pristine copy (a seed): its values are read again.
	s.forgetPristine()
	mustExec(t, sqlServerTestDB(t, s.o.DSN, ""), "ALTER DATABASE "+qms(s.o.Pristine)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE", "DROP DATABASE "+qms(s.o.Pristine))
	makeSQLServerPristine(t, s)
	if d := Diff(f, mustSQLServerFingerprint(t, s)); len(d) > 0 || s.last == nil {
		t.Fatalf("after a new pristine copy: changed %v", d)
	}
}

// TestSQLServerQuery: a query runs on the pristine copy; anything but a
// read is refused before it is sent.
func TestSQLServerQuery(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	ctx := context.Background()
	if _, err := s.Query(ctx, "SELECT 1"); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Fatalf("no pristine database: %v", err)
	}
	mustExec(t, db, "CREATE TABLE u (id int PRIMARY KEY, email nvarchar(50))", "INSERT INTO u VALUES (1, N'a@b.c'), (2, NULL)",
		"CREATE SEQUENCE q START WITH 1")
	makeSQLServerPristine(t, s)
	got, err := s.Query(ctx, "SELECT email FROM dbo.u ORDER BY id;")
	if err != nil || !reflect.DeepEqual(got, []string{"a@b.c", ""}) {
		t.Fatalf("query: %v %v", got, err)
	}
	for _, q := range []string{"SELECT 1 DROP TABLE u", "SELECT NEXT VALUE FOR dbo.q", "DELETE FROM u", "SELECT * INTO x FROM u"} {
		if _, err := s.Query(ctx, q); err == nil || !strings.HasPrefix(err.Error(), "sql: ") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// TestSQLServerRows: rows by primary key, with every column's hash.
func TestSQLServerRows(t *testing.T) {
	s, db := newSQLServerTest(t, Options{RowTracking: []string{"orders", "app.lines", "nokey"}})
	mustExec(t, db, "CREATE SCHEMA app",
		"CREATE TABLE orders (id int PRIMARY KEY, total decimal(10, 2), rv rowversion)",
		"CREATE TABLE app.lines (order_id int, at datetime2, code varbinary(4), qty int, PRIMARY KEY (order_id, at, code))",
		"CREATE TABLE nokey (v int)",
		"INSERT INTO orders (id, total) VALUES (1, 10), (2, 20)",
		"INSERT INTO app.lines VALUES (1, '2024-01-02T03:04:05', 0x0A0B, 1)")
	ctx := context.Background()
	if _, err := s.Rows(ctx, nil); err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Fatalf("a table without a primary key: %v", err)
	}
	items := []string{"table:dbo.orders", "table:app.lines"}
	before, err := s.Rows(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before["app.lines"]["order_id=1,at=2024-01-02T03:04:05,code=0x0A0B"]; !ok {
		t.Fatalf("keys %v", before["app.lines"])
	}
	mustExec(t, db, "UPDATE orders SET total = 11 WHERE id = 1", "UPDATE orders SET total = total WHERE id = 2",
		"DELETE FROM app.lines", "INSERT INTO orders (id, total) VALUES (3, 30)")
	after, err := s.Rows(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	want := []RowChange{
		{Table: "app.lines", Key: "order_id=1,at=2024-01-02T03:04:05,code=0x0A0B", Change: "deleted"},
		{Table: "dbo.orders", Key: "id=1", Change: "updated", Columns: []string{"rv", "total"}},
		{Table: "dbo.orders", Key: "id=2", Change: "updated", Columns: []string{"rv"}},
		{Table: "dbo.orders", Key: "id=3", Change: "inserted"},
	}
	if got := RowDiff(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("row diff %+v", got)
	}
}

// TestSQLServerCheckPristine: a database is replaced only when DriftGate
// made it for this working database, it is empty, or it is adopted.
func TestSQLServerCheckPristine(t *testing.T) {
	s, _ := newSQLServerTest(t, Options{})
	ctx := context.Background()
	master := sqlServerTestDB(t, s.o.DSN, "")
	p := s.o.Pristine
	check := func(want string) {
		t.Helper()
		err := s.checkPristine(ctx, p)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Fatalf("checkPristine: %v, want %q", err, want)
		}
	}
	check("") // none
	if err := s.checkPristine(ctx, strings.ToUpper(s.o.Working)); err == nil {
		t.Fatal("the working database may be replaced")
	}
	if err := s.checkPristine(ctx, "msdb"); err == nil {
		t.Fatal("a system database may be replaced")
	}
	mustExec(t, master, "CREATE DATABASE "+qms(p))
	check("") // empty
	pdb := sqlServerTestDB(t, s.o.DSN, p)
	mustExec(t, pdb, "CREATE TABLE data (id int)")
	check("not made by driftgate seed")
	s.o.AdoptPristine = true
	check("")
	s.o.AdoptPristine = false
	mark, err := s.markValue(ctx, "", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pdb, "EXEC sys.sp_addextendedproperty @name = N'driftgate_pristine', @value = "+sqlServerString(mark))
	check("")
	if m, err := s.readMark(ctx, p); err != nil || m == nil || m.Working != s.o.Working {
		t.Fatalf("mark %+v %v", m, err)
	}
	other := strings.Replace(mark, s.o.Working, "other", 1)
	mustExec(t, pdb, "EXEC sys.sp_updateextendedproperty @name = N'driftgate_pristine', @value = "+sqlServerString(other))
	check("pristine copy of other")
	mustExec(t, pdb, "EXEC sys.sp_updateextendedproperty @name = N'driftgate_pristine', @value = N'{not json'")
	check("cannot be read")
	pdb.Close()
}

// TestSQLServerCollations: a database of another collation (UTF-8, case
// sensitive) and names with ] and spaces.
func TestSQLServerCollations(t *testing.T) {
	s, db := newSQLServerTest(t, Options{}, "COLLATE Latin1_General_100_CS_AS_SC_UTF8")
	mustExec(t, db, "CREATE SCHEMA [we]]ird s]", "CREATE TABLE [we]]ird s].[t a]]b] ([c]]1] varchar(10) COLLATE Japanese_XJIS_140_CI_AS, [C]]1] nvarchar(10))",
		"INSERT INTO [we]]ird s].[t a]]b] VALUES ('ć', N'ć')",
		"EXEC sp_addextendedproperty N'p', 1, N'SCHEMA', N'we]ird s', N'TABLE', N't a]b'")
	f := sqlServerFull(t, s)
	if _, ok := f["table:we]ird s.t a]b"]; !ok {
		t.Fatalf("items %v", f)
	}
	mustExec(t, db, "UPDATE [we]]ird s].[t a]]b] SET [C]]1] = N'Ć'")
	if d := Diff(f, sqlServerFull(t, s)); len(d) != 1 {
		t.Fatalf("changed %v", d)
	}
}

// TestSQLServerClosePool: DriftGate's sessions in a database end with its
// pool (before ALTER DATABASE … SET OFFLINE), and the next fingerprint
// connects again.
func TestSQLServerClosePool(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE a (id int PRIMARY KEY)")
	f := mustSQLServerFingerprint(t, s)
	s.closePool(s.o.Working)
	if s.work != nil {
		t.Fatal("the pinned connection is still open")
	}
	if g := mustSQLServerFingerprint(t, s); !reflect.DeepEqual(f, g) {
		t.Fatalf("changed %v", Diff(f, g))
	}
	// The pinned connection ended by someone else: one more try.
	mustExec(t, sqlServerTestDB(t, s.o.DSN, ""), "ALTER DATABASE "+qms(s.o.Working)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE", "ALTER DATABASE "+qms(s.o.Working)+" SET MULTI_USER")
	if g := mustSQLServerFingerprint(t, s); !reflect.DeepEqual(f, g) {
		t.Fatalf("changed %v", Diff(f, g))
	}
}

// TestSQLServerMissingWorking: a working database that does not exist has
// no items.
func TestSQLServerMissingWorking(t *testing.T) {
	s, _ := newSQLServerTest(t, Options{})
	s.o.Working = sqlServerTestName()
	f, err := s.Fingerprint(context.Background(), nil)
	if err != nil || len(f) != 0 {
		t.Fatalf("%v %v", f, err)
	}
}

// TestSQLServerNoPasswordInErrors: errors of a failed sign-in do not show
// the password.
func TestSQLServerNoPasswordInErrors(t *testing.T) {
	dsn := sqlServerTestDSN(t)
	const secret = "Wr0ng;P@ss{w}rd"
	u := strings.Replace(dsn, "Dg-Passw0rd-2026", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(secret, ";", "%3B"), "@", "%40"), "{", "%7B"), "}", "%7D"), 1)
	if u == dsn {
		t.Skip("the test DSN has another password")
	}
	for _, d := range []string{u, strings.Replace(u, "14331", "14339", 1)} {
		s, err := NewSQLServer(Options{Name: "db", DSN: d, Working: "w", Pristine: "p"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = s.Fingerprint(ctx, nil)
		cancel()
		s.Close()
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "P%40ss") {
			t.Fatalf("error %v", err)
		}
	}
}

// TestSQLServerCatalogCache: the catalog is read again only when it
// changed, also by a change that moves no modify_date (permissions,
// extended properties, a schema's owner).
func TestSQLServerCatalogCache(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE a (id int PRIMARY KEY, v int)", "CREATE SCHEMA app", "CREATE USER someone WITHOUT LOGIN",
		"CREATE PARTITION FUNCTION pf (int) AS RANGE LEFT FOR VALUES (1)", "CREATE PARTITION SCHEME ps AS PARTITION pf ALL TO ([PRIMARY])")
	settle := func() { time.Sleep(2100 * time.Millisecond) } // older than the guard's window
	settle()
	mustSQLServerFingerprint(t, s)
	inv := s.inv
	if inv == nil {
		t.Fatal("the inventory is not kept")
	}
	mustExec(t, db, "INSERT INTO a VALUES (1, 1)")
	mustSQLServerFingerprint(t, s)
	if s.inv != inv {
		t.Fatal("the catalog was read again although it did not change")
	}
	for _, tc := range []struct{ stmt, item string }{
		{"GRANT SELECT ON a TO someone", "table:dbo.a"},
		{"EXEC sp_addextendedproperty N'p', N'x', N'SCHEMA', N'dbo', N'TABLE', N'a', N'COLUMN', N'v'", "table:dbo.a"},
		{"EXEC sp_updateextendedproperty N'p', N'y', N'SCHEMA', N'dbo', N'TABLE', N'a', N'COLUMN', N'v'", "table:dbo.a"},
		{"ALTER AUTHORIZATION ON SCHEMA::app TO someone", "schema:app"},
		{"GRANT CREATE TABLE TO someone", "database:"},
		{"DENY SELECT ON SCHEMA::dbo TO someone", "database:"},
		{"ALTER ROLE db_datareader ADD MEMBER someone", "database:"},
		{"EXEC sp_addextendedproperty @name = N'appver', @value = N'2'", "database:"},
		{"ALTER PARTITION SCHEME ps NEXT USED [PRIMARY]; ALTER PARTITION FUNCTION pf() SPLIT RANGE (5)", ""},
		{"ALTER TABLE a ADD w int NULL", "table:dbo.a"}, // within the window
	} {
		f := mustSQLServerFingerprint(t, s)
		if !strings.HasPrefix(tc.stmt, "ALTER TABLE") {
			settle()
		}
		mustExec(t, db, tc.stmt)
		g := mustSQLServerFingerprint(t, s)
		if d := Diff(f, g); tc.item != "" && !reflect.DeepEqual(d, []string{tc.item}) || len(d) == 0 {
			t.Errorf("%s: changed %v, want %s", tc.stmt, d, tc.item)
		}
		if full := sqlServerFull(t, s); !reflect.DeepEqual(g, full) {
			t.Errorf("%s: differs from a full fingerprint in %v", tc.stmt, Diff(g, full))
		}
	}
}

// TestSQLServerWithoutPerformanceState: a login without VIEW SERVER
// PERFORMANCE STATE (db_owner of the working database only) reads every
// table every time, and sees every change.
func TestSQLServerWithoutPerformanceState(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	master := sqlServerTestDB(t, s.o.DSN, "")
	login := "dgt_login_" + strings.TrimPrefix(s.o.Working, "dgt_ss_")
	const pw = "Dg-Login-Passw0rd-2026"
	mustExec(t, master, "CREATE LOGIN "+qms(login)+" WITH PASSWORD = "+sqlServerString(pw)+", CHECK_POLICY = OFF")
	t.Cleanup(func() {
		master.Exec("DECLARE @k nvarchar(max) = N''; SELECT @k += N'KILL ' + CAST(session_id AS nvarchar(10)) + N'; ' FROM sys.dm_exec_sessions WHERE login_name = " + sqlServerString(login) + "; EXEC (@k)")
		master.Exec("DROP LOGIN " + qms(login))
	})
	mustExec(t, db, "CREATE USER "+qms(login)+" FOR LOGIN "+qms(login), "ALTER ROLE db_owner ADD MEMBER "+qms(login),
		"CREATE TABLE a (id int IDENTITY PRIMARY KEY, v int)", "INSERT INTO a (v) VALUES (1)")
	u, err := url.Parse(s.o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, pw)
	o := s.o
	o.DSN = u.String()
	limited, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if mustSQLServerInfo(t, limited).Perf {
		t.Fatal("the login can read the performance state")
	}
	f := mustSQLServerFingerprint(t, limited)
	if !strings.Contains(limited.Mode(), "every table every time") {
		t.Fatalf("mode %q", limited.Mode())
	}
	for _, stmt := range []string{"UPDATE a SET v = 2", "BEGIN TRAN; INSERT INTO a (v) VALUES (3); ROLLBACK", "GRANT SELECT ON a TO public"} {
		mustExec(t, db, stmt)
		g := mustSQLServerFingerprint(t, limited)
		if limited.Reused() || !reflect.DeepEqual(Diff(f, g), []string{"table:dbo.a"}) {
			t.Errorf("%s: changed %v", stmt, Diff(f, g))
		}
		if full := sqlServerFull(t, s); !reflect.DeepEqual(g, full) {
			t.Errorf("%s: differs from a full fingerprint in %v", stmt, Diff(g, full))
		}
		f = g
	}
}

// TestSQLServerFeatures: tables with hidden, generated and sparse columns,
// graph, temporal, columnstore and memory-optimized tables fingerprint as a
// full read does, and their writes show.
func TestSQLServerFeatures(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	var data string
	if err := db.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400))").Scan(&data); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		"CREATE TABLE node_t (id int PRIMARY KEY, name nvarchar(20)) AS NODE",
		"CREATE TABLE edge_t (weight int) AS EDGE",
		"INSERT INTO node_t VALUES (1, N'a'), (2, N'b')",
		"INSERT INTO edge_t ($from_id, $to_id, weight) SELECT (SELECT $node_id FROM node_t WHERE id = 1), (SELECT $node_id FROM node_t WHERE id = 2), 5",
		`CREATE TABLE temporal_t (id int PRIMARY KEY, v int, vf datetime2 GENERATED ALWAYS AS ROW START HIDDEN, vt datetime2 GENERATED ALWAYS AS ROW END HIDDEN,
			PERIOD FOR SYSTEM_TIME (vf, vt)) WITH (SYSTEM_VERSIONING = ON (HISTORY_TABLE = dbo.temporal_h))`,
		"INSERT INTO temporal_t (id, v) VALUES (1, 1)",
		"CREATE TABLE sparse_t (id int PRIMARY KEY, a int SPARSE NULL, b nvarchar(20) SPARSE NULL, cs xml COLUMN_SET FOR ALL_SPARSE_COLUMNS, total AS id * 2 PERSISTED, rg uniqueidentifier ROWGUIDCOL DEFAULT NEWID())",
		"INSERT INTO sparse_t (id, a) VALUES (1, 1)",
		"CREATE TABLE cs_t (id int, v int, INDEX cci CLUSTERED COLUMNSTORE)",
		"INSERT INTO cs_t VALUES (1, 1), (2, 2)",
	)
	memory := true
	if _, err := db.Exec("ALTER DATABASE CURRENT ADD FILEGROUP mo CONTAINS MEMORY_OPTIMIZED_DATA; ALTER DATABASE CURRENT ADD FILE (NAME = '" +
		strings.TrimPrefix(s.o.Working, "dgt_") + "_mo', FILENAME = " + sqlServerString(strings.TrimRight(data, "/")+"/"+s.o.Working+"_mo") + ") TO FILEGROUP mo"); err != nil {
		t.Logf("no memory-optimized tables: %v", err)
		memory = false
	}
	if memory {
		mustExec(t, db,
			"CREATE TABLE mo_t (id int PRIMARY KEY NONCLUSTERED, v int) WITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_AND_DATA)",
			"CREATE TABLE mo_s (id int PRIMARY KEY NONCLUSTERED, v int) WITH (MEMORY_OPTIMIZED = ON, DURABILITY = SCHEMA_ONLY)",
			"INSERT INTO mo_t VALUES (1, 1)", "INSERT INTO mo_s VALUES (1, 1)")
	}
	writes := []struct{ stmt, item string }{
		{"UPDATE node_t SET name = N'c' WHERE id = 2", "table:dbo.node_t"},
		{"UPDATE edge_t SET weight = 6", "table:dbo.edge_t"},
		{"UPDATE temporal_t SET v = 2", ""}, // and its history; the period moves
		{"UPDATE sparse_t SET b = N'x'", "table:dbo.sparse_t"},
		{"UPDATE cs_t SET v = 3 WHERE id = 1", "table:dbo.cs_t"},
	}
	if memory {
		writes = append(writes, struct{ stmt, item string }{"UPDATE mo_t SET v = 2", "table:dbo.mo_t"},
			struct{ stmt, item string }{"UPDATE mo_s SET v = 2", "table:dbo.mo_s"})
	}
	f := mustSQLServerFingerprint(t, s)
	mustSQLServerFingerprint(t, s)
	for _, w := range writes {
		mustExec(t, db, w.stmt)
		g := mustSQLServerFingerprint(t, s)
		if full := sqlServerFull(t, s); !reflect.DeepEqual(g, full) {
			t.Fatalf("%s: differs from a full fingerprint in %v", w.stmt, Diff(g, full))
		}
		d := Diff(f, g)
		if w.item != "" && !reflect.DeepEqual(d, []string{w.item}) || len(d) == 0 {
			t.Errorf("%s: changed %v, want %s", w.stmt, d, w.item)
		}
		f = g
	}
}

// TestSQLServerQueryStaysRead: what slipped past an earlier check (a word
// glued to a number, a no-break space) is refused, nothing reaches the
// working database, and other databases cannot be named.
func TestSQLServerQueryStaysRead(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE TABLE a (i int)")
	makeSQLServerPristine(t, s)
	ctx := context.Background()
	w := strings.ReplaceAll(qms(s.o.Working), "'", "''")
	for _, q := range []string{
		"SELECT 1e1EXEC('COMMIT; CREATE TABLE " + w + ".dbo.pwned (i int); BEGIN TRAN')",
		"SELECT 1EXEC('COMMIT; CREATE TABLE " + w + ".dbo.pwned (i int); BEGIN TRAN')",
		"SELECT 1\u00a0EXEC('COMMIT; CREATE TABLE " + w + ".dbo.pwned (i int); BEGIN TRAN')",
		"SELECT * FROM " + qms(s.o.Working) + ".dbo.a",
		"SELECT CONVERT(varchar(200), LOGINPROPERTY('sa', 'PasswordHash'), 1)",
	} {
		if _, err := s.Query(ctx, q); err == nil || !strings.HasPrefix(err.Error(), "sql: ") {
			t.Errorf("%s: %v", q, err)
		}
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sys.tables WHERE name = 'pwned'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a query wrote to the working database (%d, %v)", n, err)
	}
}

// TestSQLServerRowversionAfterFallback: a restore of the whole database
// (forget) drops the remembered rowversion maxima, so later values cannot
// hide behind them.
func TestSQLServerRowversionAfterFallback(t *testing.T) {
	ctx := context.Background()
	s, db := newSQLServerTest(t, Options{})
	master := sqlServerTestDB(t, s.o.DSN, "")
	mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, v int, rv rowversion)", "INSERT INTO t (id, v) VALUES (1, 1)")
	makeSQLServerPristine(t, s)
	var data string
	if err := master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400))").Scan(&data); err != nil {
		t.Fatal(err)
	}
	bak := strings.TrimRight(data, "/") + "/" + s.o.Working + "_fb.bak"
	mustExec(t, master, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(bak)+" WITH COPY_ONLY, INIT, FORMAT")
	defer master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(bak))
	base := mustSQLServerFingerprint(t, s)["table:dbo.t"]
	// As an item-level restore of t: rows written anew, maxima remembered.
	mustExec(t, db, "UPDATE t SET v = v", "UPDATE t SET v = v", "DELETE FROM t", "INSERT INTO t (id, v) VALUES (1, 1)")
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := s.inventory(ctx, c)
	if err == nil {
		err = s.rememberRowversions(ctx, c, []*ssTable{inv.ByItem["table:dbo.t"]})
	}
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	s.restored([]string{"table:dbo.t"})
	m := s.rvMax["table:dbo.t"]
	// As a fallback restore later.
	s.closePool(s.o.Working)
	db.Close()
	mustExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" SET SINGLE_USER WITH ROLLBACK IMMEDIATE",
		"RESTORE DATABASE "+qms(s.o.Working)+" FROM DISK = "+sqlServerString(bak)+" WITH REPLACE", "ALTER DATABASE "+qms(s.o.Working)+" SET MULTI_USER")
	s.forget()
	db = sqlServerTestDB(t, s.o.DSN, s.o.Working)
	if got := mustSQLServerFingerprint(t, s)["table:dbo.t"]; got != base {
		t.Fatal("not the pristine state after the restore")
	}
	var rv []byte
	for range 1000 { // no-op updates until the value reaches the old maximum
		mustExec(t, db, "UPDATE t SET v = v")
		if err := db.QueryRow("SELECT rv FROM t").Scan(&rv); err != nil {
			t.Fatal(err)
		}
		if string(rv) >= string(m) {
			break
		}
	}
	if got := mustSQLServerFingerprint(t, s)["table:dbo.t"]; got == base {
		t.Fatal("the rowversion moved but the item equals the pristine one")
	}
}

// TestSQLServerItemNames: two objects whose item names come out alike are
// refused, not mixed up.
func TestSQLServerItemNames(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	mustExec(t, db, "CREATE SCHEMA [a.b]", "CREATE SCHEMA a", "CREATE TABLE [a.b].c (id int PRIMARY KEY)", "CREATE TABLE a.[b.c] (id int PRIMARY KEY)")
	if _, err := s.Fingerprint(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "both have the item name table:a.b.c") {
		t.Fatalf("%v", err)
	}
	mustExec(t, db, "DROP TABLE a.[b.c]", "CREATE SCHEMA [database]", "CREATE TABLE [database].t (id int)",
		"CREATE TRIGGER [database].x ON [database].t AFTER INSERT AS SET NOCOUNT ON",
		"CREATE TRIGGER x ON DATABASE FOR CREATE_TABLE AS SET NOCOUNT ON")
	if _, err := s.Fingerprint(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "trigger:database.x") {
		t.Fatalf("%v", err)
	}
}

// TestSQLServerRowLevelSecurity: a table behind an enabled security
// policy is refused (its filter hides rows from every read); a disabled
// policy is part of the table's definition.
func TestSQLServerRowLevelSecurity(t *testing.T) {
	s, db := newSQLServerTest(t, Options{RowTracking: []string{"orders"}})
	mustExec(t, db, "CREATE TABLE orders (id int PRIMARY KEY, tenant int, total int)", "INSERT INTO orders VALUES (1, 1, 10), (2, 2, 20)",
		"CREATE FUNCTION dbo.tenant_pred (@tenant int) RETURNS TABLE WITH SCHEMABINDING AS RETURN SELECT 1 AS ok WHERE @tenant = CAST(SESSION_CONTEXT(N'tenant') AS int)")
	f := mustSQLServerFingerprint(t, s)
	mustExec(t, db, "CREATE SECURITY POLICY dbo.tenants ADD FILTER PREDICATE dbo.tenant_pred(tenant) ON dbo.orders WITH (STATE = ON)")
	ctx := context.Background()
	if _, err := s.Fingerprint(ctx, nil); err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("fingerprint: %v", err)
	}
	if _, err := s.Rows(ctx, nil); err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("rows: %v", err)
	}
	mustExec(t, db, "ALTER SECURITY POLICY dbo.tenants WITH (STATE = OFF)")
	g := mustSQLServerFingerprint(t, s)
	if d := Diff(f, g); !reflect.DeepEqual(d, []string{"table:dbo.orders"}) {
		t.Fatalf("a disabled policy changed %v", d)
	}
}

// TestSQLServerTornRead: a transaction that commits while the tables are
// read cannot leave a fingerprint of a state the database never had.
func TestSQLServerTornRead(t *testing.T) {
	s, db := newSQLServerTest(t, Options{})
	if !mustSQLServerInfo(t, s).Perf {
		t.Skip("the test login cannot read the server's performance state")
	}
	mustExec(t, db, "CREATE TABLE a (id int PRIMARY KEY, v int)", "CREATE TABLE b (id int PRIMARY KEY, v int)",
		"INSERT INTO a VALUES (1, 1)", "INSERT INTO b VALUES (1, 1)")
	p := sqlServerFull(t, s) // a = 1, b = 1
	mustExec(t, db, "UPDATE a SET v = 2")
	defer func(d time.Duration) { sqlServerOpenWait = d }(sqlServerOpenWait)
	sqlServerOpenWait = 200 * time.Millisecond
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE a SET v = 1"); err != nil { // a back to 1 …
		t.Fatal(err)
	}
	type result struct {
		f   Fingerprint
		err error
	}
	got := make(chan result, 1)
	go func() {
		f, err := s.Fingerprint(context.Background(), nil)
		got <- result{f, err}
	}()
	time.Sleep(1500 * time.Millisecond)
	if _, err := tx.Exec("UPDATE b SET v = 2"); err != nil { // … and b to 2, in one transaction
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-got
	now := sqlServerFull(t, s)
	if r.err == nil && len(Diff(p, r.f)) == 0 && len(Diff(p, now)) > 0 {
		t.Fatalf("a torn read equals the pristine state while b differs")
	}
	if r.err == nil && !reflect.DeepEqual(r.f, now) {
		t.Fatalf("the fingerprint is of no state the database had: %v", Diff(r.f, now))
	}
}

// TestSQLServerDatabaseItem: changes outside tables and routines (users,
// roles, permissions on the database and on dbo, the database's options,
// rules and bound defaults, partitions, XML schema collections) show in
// the fingerprint, and one of the database itself is restored with the
// whole database.
func TestSQLServerDatabaseItem(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{},
		"CREATE TABLE t (id int PRIMARY KEY, v int NULL)", "INSERT INTO t VALUES (1, 1)", "CREATE USER u0 WITHOUT LOGIN",
		"CREATE RULE r0 AS @v > 0", "CREATE DEFAULT d0 AS 42")
	p := pristineSQLServer(t, s)
	if d := Diff(mustSQLServerFingerprint(t, s), p); len(d) > 0 {
		t.Fatalf("the working database and its pristine copy differ in %v", d)
	}
	for _, tc := range []struct{ stmt, item string }{
		{"CREATE USER u1 WITHOUT LOGIN", "database:"},
		{"CREATE ROLE r1", "database:"},
		{"ALTER ROLE db_datareader ADD MEMBER u0", "database:"},
		{"GRANT CREATE TABLE TO u0", "database:"},
		{"DENY SELECT ON SCHEMA::dbo TO u0", "database:"},
		{"GRANT SELECT ON dbo.t TO u0", "table:dbo.t"},
		{"EXEC sp_addextendedproperty @name = N'appver', @value = N'2'", "database:"},
		{"ALTER DATABASE CURRENT SET READ_COMMITTED_SNAPSHOT ON WITH ROLLBACK IMMEDIATE", "database:"},
		{"ALTER DATABASE CURRENT SET COMPATIBILITY_LEVEL = 140", "database:"},
		{"ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON", "database:"},
		{"ALTER DATABASE CURRENT SET ARITHABORT ON", "database:"},
		{"ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 1", "database:"},
		{"CREATE PARTITION FUNCTION pf (int) AS RANGE LEFT FOR VALUES (1)", "partition_function:pf"},
		{"CREATE XML SCHEMA COLLECTION xs AS N'<xsd:schema xmlns:xsd=\"http://www.w3.org/2001/XMLSchema\"><xsd:element name=\"a\"/></xsd:schema>'", "xml_schema:dbo.xs"},
		{"CREATE RULE rpos AS @v > 0", "rule:dbo.rpos"},
		{"EXEC sp_bindrule 'r0', 't.v'", "table:dbo.t"},
		{"CREATE DEFAULT dfl AS 42", "default:dbo.dfl"},
		{"EXEC sp_bindefault 'd0', 't.v'", "table:dbo.t"},
		{"CREATE STATISTICS st ON t (v)", ""}, // statistics are left out on purpose
	} {
		mustExec(t, db, tc.stmt)
		d := Diff(sqlServerFull(t, s), p)
		if tc.item == "" {
			if len(d) > 0 {
				t.Errorf("%s: changed %v", tc.stmt, d)
			}
			continue
		}
		if !reflect.DeepEqual(d, []string{tc.item}) {
			t.Errorf("%s: changed %v, want %s", tc.stmt, d, tc.item)
		}
		whole := restoreSQLServer(t, s, db, log, p)
		if !strings.HasPrefix(tc.item, "table:") && !whole {
			t.Errorf("%s: restored item by item", tc.stmt)
		}
	}
}

// TestSQLServerSnapshotSetting: a read-only pristine copy reports snapshot
// isolation on whatever it is set to; the mark keeps the working
// database's setting, so the two compare as they are.
func TestSQLServerSnapshotSetting(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{}, "ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON", "CREATE TABLE t (id int PRIMARY KEY)")
	p := pristineSQLServer(t, s)
	if d := Diff(mustSQLServerFingerprint(t, s), p); len(d) > 0 {
		t.Fatalf("the working database and its pristine copy differ in %v", d)
	}
	mustExec(t, db, "ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION OFF")
	if d := Diff(sqlServerFull(t, s), p); !reflect.DeepEqual(d, []string{"database:"}) {
		t.Fatalf("changed %v", d)
	}
	if !restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored item by item")
	}
}

// TestSQLServerSeededFingerprintQuiet: the record of a fresh seed (the
// pristine copy's fingerprint) equals the working database's when nothing
// wrote: what only the pristine copy has (READ_ONLY, the snapshot
// isolation a read-only database reports, the mark) is not part of it.
func TestSQLServerSeededFingerprintQuiet(t *testing.T) {
	for _, snapshot := range []string{"OFF", "ON"} {
		t.Run("snapshot "+snapshot, func(t *testing.T) {
			s, _, _ := newSQLServerSeeded(t, Options{},
				"ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION "+snapshot,
				"CREATE TABLE t (id int IDENTITY PRIMARY KEY, v int NULL)", "INSERT INTO t (v) VALUES (1)",
				"CREATE USER u0 WITHOUT LOGIN", "ALTER ROLE db_datareader ADD MEMBER u0", "GRANT CREATE TABLE TO u0",
				"DENY SELECT ON SCHEMA::dbo TO u0", "EXEC sp_addextendedproperty @name = N'appver', @value = N'2'",
				"CREATE PARTITION FUNCTION pf (int) AS RANGE LEFT FOR VALUES (1)", "CREATE PARTITION SCHEME ps AS PARTITION pf ALL TO ([PRIMARY])",
				"CREATE RULE r0 AS @v > 0", "EXEC sp_bindrule 'r0', 't.v'", "ALTER DATABASE SCOPED CONFIGURATION SET MAXDOP = 2")
			record, err := s.SeededFingerprint(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if d := Diff(mustSQLServerFingerprint(t, s), record); len(d) > 0 {
				t.Fatalf("the working database and the record of its fresh seed differ in %v", d)
			}
		})
	}
}

// TestSQLServerWorkingNotOnline: a working database left OFFLINE or
// RESTORING (a restore that was stopped) fingerprints as different from
// the pristine copy, so the restore that follows brings it back.
func TestSQLServerWorkingNotOnline(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{}, "CREATE TABLE t (id int PRIMARY KEY)", "INSERT INTO t VALUES (1)")
	p := pristineSQLServer(t, s)
	master := sqlServerTestDB(t, s.o.DSN, "")
	var data string
	if err := master.QueryRow("SELECT CAST(SERVERPROPERTY('InstanceDefaultDataPath') AS nvarchar(400))").Scan(&data); err != nil {
		t.Fatal(err)
	}
	bak := strings.TrimRight(data, "/") + "/" + s.o.Working + "_nr.bak"
	mustExec(t, master, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(bak)+" WITH COPY_ONLY, INIT, FORMAT")
	defer master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlServerString(bak))
	mustSQLServerFingerprint(t, s) // a pinned connection, ended by what follows
	for _, tc := range []struct{ state, stmt string }{
		{"OFFLINE", "ALTER DATABASE " + qms(s.o.Working) + " SET OFFLINE WITH ROLLBACK IMMEDIATE"},
		{"RESTORING", "ALTER DATABASE " + qms(s.o.Working) + " SET SINGLE_USER WITH ROLLBACK IMMEDIATE; RESTORE DATABASE " + qms(s.o.Working) +
			" FROM DISK = " + sqlServerString(bak) + " WITH REPLACE, NORECOVERY"},
	} {
		mustExec(t, master, tc.stmt)
		f := mustSQLServerFingerprint(t, s)
		if s.Reused() || s.last != nil {
			t.Fatalf("%s: a fingerprint was reused or kept", tc.state)
		}
		if d := Diff(f, p); !contains(d, "database:") {
			t.Fatalf("%s: the fingerprint matches the pristine copy's (%v)", tc.state, d)
		}
		restoreSQLServer(t, s, db, log, p)
		db.SetMaxIdleConns(0)
		db.SetMaxIdleConns(2)
	}
}
