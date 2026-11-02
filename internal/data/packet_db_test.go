package data

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestMySQLPacket: no INSERT is longer than the target's max_allowed_packet
// (here 64 KiB): the rows before one that would make it longer go alone,
// and a row too long for a statement of its own stops the run, naming its
// table and column.
func TestMySQLPacket(t *testing.T) {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	const packet = 64 << 10
	defer func(f func(context.Context, *sql.Conn) (int, error)) { maxPacket = f }(maxPacket)
	maxPacket = func(context.Context, *sql.Conn) (int, error) { return packet, nil }
	rules, err := ParseRules([]byte("version: 1\ntables:\n  files:\n    columns: { id: keep, data: keep }\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dsn := range strings.Split(v, ",") {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			s := newServer(t, MySQL, dsn)
			const src, dst = "dgdata_packet", "dgdata_packet_dst"
			t.Cleanup(func() { s.drop(src, dst) })
			s.create(t, src)
			s.drop(dst)
			// 20 KiB a row, 40 KiB as a hex literal: two rows do not fit.
			s.exec(t, src, "CREATE TABLE files (id INT PRIMARY KEY, data BLOB NOT NULL)",
				"INSERT INTO files VALUES (1, REPEAT('a', 20000)), (2, REPEAT('b', 20000)), (3, REPEAT('c', 20000)), (4, 'd')")
			target := s.db(dst)
			var dump bytes.Buffer
			if _, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Dump: &dump, Rules: rules, Key: testKey, BatchRows: 100}); err != nil {
				t.Fatal(err)
			}
			inserts := 0
			for _, st := range strings.Split(dump.String(), ";\n") {
				if strings.HasPrefix(st, "INSERT INTO") {
					inserts++
					if len(st) > packet-packetMargin {
						t.Errorf("an INSERT of %d bytes", len(st))
					}
				}
			}
			if inserts != 3 {
				t.Errorf("%d INSERT statements, want 3", inserts)
			}
			var n int
			tdb, _ := open(target, dst)
			defer tdb.Close()
			if err := tdb.QueryRow("SELECT COUNT(*) FROM files").Scan(&n); err != nil || n != 4 {
				t.Fatalf("%d rows in the target, %v", n, err)
			}

			s.exec(t, src, "INSERT INTO files VALUES (5, REPEAT('e', 40000))")
			_, err := Anonymize(ctx, Options{Source: s.db(src), Target: &target, Replace: true, Rules: rules, Key: testKey})
			if err == nil || !strings.Contains(err.Error(), "files: a row takes an INSERT of") || !strings.Contains(err.Error(), "column data") {
				t.Fatalf("a row longer than max_allowed_packet: %v", err)
			}
		})
	}
}
