package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// BenchmarkMySQLFingerprint: a full fingerprint of a schema with tables,
// views and triggers, read every time (not reused from the write counters).
func BenchmarkMySQLFingerprint(b *testing.B) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		b.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	ctx := context.Background()
	work := "dgt_bench_fp"
	open := func(database string) *sql.DB {
		cfg, err := mysqlDSN(dsn)
		if err != nil {
			b.Fatal(err)
		}
		cfg.DBName = database
		conn, err := mysql.NewConnector(cfg)
		if err != nil {
			b.Fatal(err)
		}
		db := sql.OpenDB(conn)
		db.SetMaxOpenConns(1)
		b.Cleanup(func() { db.Close() })
		return db
	}
	root := open("")
	root.Exec("DROP DATABASE IF EXISTS " + work)
	if _, err := root.Exec("CREATE DATABASE " + work); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
	db := open(work)
	for i := range 20 {
		for _, s := range []string{
			fmt.Sprintf("CREATE TABLE t%d (id INT PRIMARY KEY, v VARCHAR(20))", i),
			fmt.Sprintf("INSERT INTO t%d VALUES (1, 'x'), (2, 'y')", i),
			fmt.Sprintf("CREATE VIEW v%d AS SELECT id FROM t%d", i, i),
			fmt.Sprintf("CREATE TRIGGER tr%d BEFORE INSERT ON t%d FOR EACH ROW SET NEW.v = 'y'", i, i),
		} {
			if _, err := db.Exec(s); err != nil {
				b.Fatal(err)
			}
		}
	}
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: work + "_pristine"})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	c, err := m.session(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	for b.Loop() {
		if _, err := m.fingerprint(ctx, c, work, nil); err != nil {
			b.Fatal(err)
		}
	}
}
