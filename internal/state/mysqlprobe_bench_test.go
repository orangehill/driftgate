package state

import (
	"context"
	"os"
	"strings"
	"testing"
)

// BenchmarkMySQLProbe measures what the DEFINER probe adds to a restore of
// one trigger and one view, on the first MySQL server of
// DRIFTGATE_TEST_MYSQL: first, a restore by a new component, which probes
// both DEFINERs; probed, the same restore once they are probed; lock, the
// named lock taken and released (probeLock). Put a latency proxy in front
// of the server to see the cost of the round trips: tools/latencyproxy,
// whose doc comment shows how.
func BenchmarkMySQLProbe(b *testing.B) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		b.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	ctx := context.Background()
	work, pristine := "dgt_bench_probe", "dgt_bench_probe_pristine"
	root := mysqlAdmin(b, dsn, "")
	for _, q := range []string{"DROP DATABASE IF EXISTS " + work, "DROP DATABASE IF EXISTS " + pristine, "CREATE DATABASE " + work} {
		if _, err := root.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		root.Exec("DROP DATABASE IF EXISTS " + work)
		root.Exec("DROP DATABASE IF EXISTS " + pristine)
	})
	db := mysqlAdmin(b, dsn, work)
	for _, q := range []string{"CREATE TABLE items (id INT PRIMARY KEY)", "CREATE TABLE log (id INT)",
		"CREATE TRIGGER items_ai AFTER INSERT ON items FOR EACH ROW INSERT INTO log VALUES (NEW.id)",
		"CREATE VIEW v AS SELECT id FROM items"} {
		if _, err := db.Exec(q); err != nil {
			b.Fatal(err)
		}
	}
	open := func() *MySQL {
		m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
		if err != nil {
			b.Fatal(err)
		}
		return m
	}
	seeder := open()
	defer seeder.Close()
	if err := seeder.Seed(ctx); err != nil {
		b.Fatal(err)
	}
	items := []string{"trigger:items_ai", "view:v"}
	restore := func(m *MySQL) {
		if err := m.Restore(ctx, items); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("first", func(b *testing.B) {
		for range b.N {
			b.StopTimer()
			m := open()
			if err := m.Restore(ctx, []string{"table:log"}); err != nil { // opens the pool, probes nothing
				b.Fatal(err)
			}
			b.StartTimer()
			restore(m)
			b.StopTimer()
			m.Close()
			b.StartTimer()
		}
	})
	b.Run("probed", func(b *testing.B) {
		m := open()
		defer m.Close()
		restore(m)
		b.ResetTimer()
		for range b.N {
			restore(m)
		}
	})
	b.Run("lock", func(b *testing.B) {
		m := open()
		defer m.Close()
		c, err := m.db.Conn(ctx)
		if err != nil {
			b.Fatal(err)
		}
		defer c.Close()
		b.ResetTimer()
		for range b.N {
			release, err := m.probeLock(ctx, c, work)
			if err != nil {
				b.Fatal(err)
			}
			if err := release(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
