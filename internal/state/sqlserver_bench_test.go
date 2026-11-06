package state

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The budgets of the SQL Server fingerprint (measured on SQL Server 2022
// in Docker; under Rosetta on Apple silicon the server's side is several
// times slower than on x86): a reused fingerprint < 3 ms, one table of
// 100 changed < 50 ms, a full read of 50 MB < 1.5 s.

// BenchmarkSQLServerReuse: nothing changed since the last fingerprint.
func BenchmarkSQLServerReuse(b *testing.B) {
	s, db := newSQLServerTest(b, Options{})
	if !mustSQLServerInfo(b, s).Perf {
		b.Skip("the login cannot read the server's performance state")
	}
	for i := range 10 {
		sqlServerExec(b, db, fmt.Sprintf("CREATE TABLE t%d (id int IDENTITY PRIMARY KEY, v nvarchar(20))", i), fmt.Sprintf("INSERT INTO t%d (v) VALUES (N'a'), (N'b')", i))
	}
	for range 3 {
		mustSQLServerFingerprint(b, s)
	}
	// The server writes to a new database's log now and then on its own
	// (checkpoints); a fingerprint then reads the changed tables.
	n, reused := 0, 0
	for b.Loop() {
		mustSQLServerFingerprint(b, s)
		n++
		if s.Reused() {
			reused++
		}
	}
	b.ReportMetric(100*float64(reused)/float64(n), "%reused")
}

// BenchmarkSQLServerOneTableOf100: one table of 100 written to (the write
// is part of the time).
func BenchmarkSQLServerOneTableOf100(b *testing.B) {
	s, db := newSQLServerTest(b, Options{})
	if !mustSQLServerInfo(b, s).Perf {
		b.Skip("the login cannot read the server's performance state")
	}
	for i := range 100 {
		sqlServerExec(b, db, fmt.Sprintf("CREATE TABLE t%d (id int IDENTITY PRIMARY KEY, v int, s nvarchar(50))", i),
			fmt.Sprintf("INSERT INTO t%d (v, s) SELECT TOP (100) object_id, name FROM sys.all_objects", i))
	}
	// The catalog read within two seconds of a change is not reused
	// (ssGuardSQL): the setup's tables are older than that when timed.
	time.Sleep(2 * time.Second)
	for range 2 {
		mustSQLServerFingerprint(b, s)
	}
	n := 0
	for b.Loop() {
		n++
		sqlServerExec(b, db, fmt.Sprintf("UPDATE t%d SET v = %d WHERE id = 1", n%100, n))
		mustSQLServerFingerprint(b, s)
		if s.Reused() {
			b.Fatal("reused")
		}
	}
}

// BenchmarkSQLServerFull: every table read, 50 MB of rows.
func BenchmarkSQLServerFull(b *testing.B) {
	s, db := newSQLServerTest(b, Options{})
	sqlServerExec(b, db, `CREATE TABLE big (id int PRIMARY KEY, b bigint, n nvarchar(100), s varchar(200), d datetime2, m decimal(18, 4),
		f float, g uniqueidentifier, bt bit, vb varbinary(50), nm nvarchar(max), rv rowversion)`,
		`WITH n AS (SELECT TOP (180000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i FROM sys.all_objects a CROSS JOIN sys.all_objects b)
		 INSERT INTO big (id, b, n, s, d, m, f, g, bt, vb, nm)
		 SELECT i, i * 7919, CONCAT(N'name ', i, N' ', REPLICATE(N'x', i % 40)), CONCAT('text ', i, ' ', REPLICATE('y', 60 + i % 100)),
		        DATEADD(second, i, '2020-01-01'), i / 7.0, i / 3.0e0, NEWID(), i % 2, CAST(CONCAT('v', i) AS varbinary(50)),
		        CASE WHEN i % 10 = 0 THEN NULL ELSE CONCAT(N'long ', REPLICATE(N'z', i % 50)) END FROM n`)
	var size int64
	if err := db.QueryRow("SELECT SUM(used_page_count) * 8192 FROM sys.dm_db_partition_stats WHERE object_id = OBJECT_ID('big')").Scan(&size); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	c, err := s.conn(ctx, s.o.Working)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for b.Loop() {
		if _, err := s.fingerprint(ctx, c, s.o.Working, nil, nil, nil, nil, false); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(size)/(1<<20), "MB")
}
