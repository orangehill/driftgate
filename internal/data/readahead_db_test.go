package data

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seedBig creates big(id, email, body) with n rows whose body is size
// bytes of text that ends with the row's email.
func seedBig(t *testing.T, s *server, db string, n, size int) {
	t.Helper()
	s.create(t, db)
	body := fmt.Sprintf("CONCAT(REPEAT('Lorem ipsum dolor sit amet. ', %d), ' pozdrav k', i, '@firma.example')", size/28)
	q := fmt.Sprintf("INSERT INTO big WITH RECURSIVE s(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM s WHERE i < %d) SELECT i, CONCAT('k', i, '@firma.example'), %s FROM s", n, body)
	typ := "LONGTEXT"
	if s.driver == Postgres {
		typ = "text"
		body = fmt.Sprintf("repeat('Lorem ipsum dolor sit amet. ', %d) || ' pozdrav k' || i || '@firma.example'", size/28)
		q = fmt.Sprintf("INSERT INTO big SELECT i, 'k' || i || '@firma.example', %s FROM generate_series(1, %d) i", body, n)
	}
	s.exec(t, db, "CREATE TABLE big (id INT PRIMARY KEY, email VARCHAR(100) NOT NULL, body "+typ+" NOT NULL)", q)
}

// TestReadAheadBytes: rows of 512 KiB are read ahead by bytes, not only by
// rows: the batches between the reader and each never hold more than
// aheadBytes, however slow each is, and the rows come in order.
func TestReadAheadBytes(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src, rows = "dgahead_src", 400
		t.Cleanup(func() { s.drop(src) })
		seedBig(t, s, src, rows, 512<<10)
		d, err := open(s.db(src), src)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		c, err := d.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		dl, _ := dialectFor(s.driver)
		if err := dl.session(ctx, c); err != nil {
			t.Fatal(err)
		}
		sc, err := dl.schema(ctx, c, src)
		if err != nil {
			t.Fatal(err)
		}
		tb := sc.Tables[0]
		size := func(b *rowBatch) int64 {
			var n int64
			for _, row := range b.rows {
				for _, v := range row {
					n += int64(len(v))
				}
			}
			return n
		}
		// A batch counts from its work (after the reader let it go) to the
		// end of its each (before the reader may take its bytes again).
		var inFlight, peak atomic.Int64
		next := 1
		err = readAhead(ctx, c, dl.selectRows(tb, tb.Columns, ""), len(tb.Columns), func(b *rowBatch) error {
			n := inFlight.Add(size(b))
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			return nil
		}, func(b *rowBatch) error {
			time.Sleep(10 * time.Millisecond) // a slow writer
			for _, row := range b.rows {
				if want := fmt.Sprint(next); string(row[0]) != want {
					return fmt.Errorf("row %s, want %s", row[0], want)
				}
				next++
			}
			inFlight.Add(-size(b))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if next != rows+1 {
			t.Fatalf("%d rows, want %d", next-1, rows)
		}
		t.Logf("peak %d MiB in flight", peak.Load()>>20)
		if p := peak.Load(); p > aheadBytes || p < 256<<10 {
			t.Fatalf("%d bytes in flight, want at most %d", p, aheadBytes)
		}
	})
}

// TestLargeFreeText: free text of large rows, cut into small batches, is
// replaced as in small rows: the output is the same with any batch of
// statements and verify finds no original.
func TestLargeFreeText(t *testing.T) {
	eachServer(t, func(t *testing.T, s *server) {
		ctx := context.Background()
		const src = "dglarge_src"
		t.Cleanup(func() { s.drop(src) })
		seedBig(t, s, src, 40, 256<<10)
		rules, err := ParseRules([]byte("version: 1\ntables:\n  big:\n    columns: {id: keep, email: email, body: free-text}\n"))
		if err != nil {
			t.Fatal(err)
		}
		var dumps [][]byte
		for _, batch := range []int{1, 500} {
			var d bytes.Buffer
			if _, err := Anonymize(ctx, Options{Source: s.db(src), Dump: &d, Rules: rules, Key: testKey, BatchRows: batch}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(d.String(), "@firma.example") {
				t.Fatal("an original email is left in the dump")
			}
			dumps = append(dumps, d.Bytes())
		}
		// The statements differ by batch; the rows they hold do not.
		rowsOf := func(b []byte) string {
			var out []string
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "(") || strings.HasPrefix(l, ",") {
					out = append(out, strings.TrimRight(strings.TrimPrefix(l, ","), ",;"))
				}
			}
			return strings.Join(out, "\n")
		}
		if a, b := rowsOf(dumps[0]), rowsOf(dumps[1]); a != b || strings.Count(a, "\n") != 39 {
			t.Fatalf("rows differ by batch (%d and %d lines)", strings.Count(a, "\n")+1, strings.Count(b, "\n")+1)
		}
		rep, err := Verify(ctx, VerifyOptions{Source: s.db(src), Dump: bytes.NewReader(dumps[1]), Rules: rules, Key: testKey})
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Clean() {
			t.Fatalf("verify: %+v", rep.Findings)
		}
	})
}
