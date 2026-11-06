package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/dbx"
)

// showSchema has what SHOW CREATE is read for: tables (one with a VIRTUAL
// column, one with a STORED one, foreign keys, AUTO_INCREMENT that moved,
// a name that needs quoting), views (one on another) and triggers (two on
// one table, in order).
var showSchema = []string{
	"CREATE TABLE news (id INT AUTO_INCREMENT PRIMARY KEY, title VARCHAR(50), views INT NOT NULL DEFAULT 0, slug VARCHAR(60) AS (LOWER(title)) VIRTUAL)",
	"CREATE TABLE tags (id INT AUTO_INCREMENT PRIMARY KEY, news_id INT NOT NULL, name VARCHAR(20), FOREIGN KEY (news_id) REFERENCES news(id) ON DELETE CASCADE)",
	"CREATE TABLE totals (id INT PRIMARY KEY, a INT, b INT, s INT AS (a + b) STORED)",
	"CREATE TABLE audit (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(50))",
	"CREATE TABLE `odd; name``s` (id INT PRIMARY KEY)",
	"CREATE TABLE plain (code CHAR(3), n INT)",
	"CREATE VIEW news_titles AS SELECT id, title FROM news",
	"CREATE VIEW news_short AS SELECT id FROM news_titles WHERE id < 10",
	"CREATE TRIGGER news_views AFTER UPDATE ON news FOR EACH ROW INSERT INTO audit (note) VALUES (CONCAT('views ', NEW.id))",
	"CREATE TRIGGER news_views2 AFTER UPDATE ON news FOR EACH ROW FOLLOWS news_views INSERT INTO audit (note) VALUES ('again')",
	"CREATE TRIGGER tags_name BEFORE INSERT ON tags FOR EACH ROW SET NEW.name = LOWER(NEW.name)",
	"INSERT INTO news (title, views) VALUES ('First', 1), ('Second', 2), ('Third', 3)",
	"INSERT INTO tags (news_id, name) VALUES (1, 'a'), (2, 'b')",
	"INSERT INTO totals (id, a, b) VALUES (1, 1, 2), (2, 3, 4)",
	"INSERT INTO plain VALUES ('abc', 1), ('def', NULL)",
	"DELETE FROM news WHERE id = 3",
}

// legacySession is a connection prepared as sessions were before they were
// prepared once: mysqlSession's statements one by one, on a pool of its own.
func legacySession(t *testing.T, dsn string) *sql.Conn {
	t.Helper()
	db, err := dbx.OpenMySQL(dsn, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := mysqlSession.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// oldFingerprint is the fingerprint as it was read before SHOW CREATE was
// batched: by the code of then (legacyFingerprintAs), one statement at a
// time, on a session prepared the old way.
func oldFingerprint(t *testing.T, m *MySQL, legacy *sql.Conn, db string, format int) Fingerprint {
	t.Helper()
	f, err := m.legacyFingerprintAs(context.Background(), legacy, db, nil, format)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func newFingerprint(t *testing.T, m *MySQL, db string, format int) Fingerprint {
	t.Helper()
	ctx := context.Background()
	c, err := m.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := m.fingerprintAs(ctx, c, db, nil, format)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestMySQLShowBatchSameFingerprint: the fingerprint read with SHOW CREATE
// batched, and spread over connections when batches are refused, is the
// one read a statement at a time, item by item and byte for byte: tables,
// views, triggers, a table with a VIRTUAL column, an empty database, both
// fingerprint formats, and after a restore and writes.
func TestMySQLShowBatchSameFingerprint(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLShowBatchSameFingerprint(t, dsn) })
	}
}

func testMySQLShowBatchSameFingerprint(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine, empty := "dgt_show", "dgt_show_pristine", "dgt_show_empty"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "DROP DATABASE IF EXISTS "+empty,
		"CREATE DATABASE "+work, "CREATE DATABASE "+empty)
	t.Cleanup(func() {
		for _, d := range []string{work, pristine, empty} {
			root.Exec("DROP DATABASE IF EXISTS " + d)
		}
	})
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, showSchema...)
	legacy := legacySession(t, dsn)
	var log strings.Builder
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	same := func(when string) {
		t.Helper()
		for _, format := range []int{1, FingerprintFormat} {
			for _, d := range []string{work, empty} {
				old := oldFingerprint(t, m, legacy, d, format)
				if d == work {
					kinds := map[string]int{}
					for it := range old {
						k, _, _ := strings.Cut(it, ":")
						kinds[k]++
					}
					if kinds["table"] != 6 || kinds["view"] != 2 || kinds["trigger"] != 3 {
						t.Fatalf("%s: items %v", when, slices.Sorted(maps.Keys(old)))
					}
				} else if len(old) != 0 {
					t.Fatalf("%s: empty database: %v", when, old)
				}
				m.multiRefused.Store(false)
				batched := newFingerprint(t, m, d, format)
				m.multiRefused.Store(true) // spread over connections (showSpread)
				spread := newFingerprint(t, m, d, format)
				m.multiRefused.Store(false)
				if !reflect.DeepEqual(old, batched) {
					t.Errorf("%s, %s, format %d: batched differs in %v", when, d, format, Diff(old, batched))
				}
				if !reflect.DeepEqual(old, spread) {
					t.Errorf("%s, %s, format %d: spread differs in %v", when, d, format, Diff(old, spread))
				}
			}
		}
	}
	same("before a seed")
	if !m.multiOK.Load() {
		t.Fatal("no query of several statements was used")
	}
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	// The pristine copy, read the same three ways, is the working database.
	if old, now := oldFingerprint(t, m, legacy, pristine, FingerprintFormat), newFingerprint(t, m, pristine, FingerprintFormat); !reflect.DeepEqual(old, now) {
		t.Fatalf("pristine: %v", Diff(old, now))
	}
	// Writes that move AUTO_INCREMENT, a changed view and trigger, and a
	// restore, whose connections go back to the pool with another default
	// database.
	mustExec(t, db, "INSERT INTO news (title) VALUES ('Fourth'), ('Fifth')", "UPDATE news SET views = 9 WHERE id = 1",
		"CREATE OR REPLACE VIEW news_titles AS SELECT id, title, views FROM news",
		"DROP TRIGGER tags_name", "CREATE TRIGGER tags_name BEFORE INSERT ON tags FOR EACH ROW SET NEW.name = UPPER(NEW.name)")
	same("after writes")
	if err := m.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	same("after a restore")
	if d := Diff(newFingerprint(t, m, work, FingerprintFormat), newFingerprint(t, m, pristine, FingerprintFormat)); len(d) > 0 {
		t.Fatalf("restore left %v", d)
	}
	if log.Len() > 0 && strings.Contains(log.String(), "SHOW CREATE") {
		t.Fatalf("log: %s", log.String())
	}
}

// TestMySQLShowBatchFallbacks: a statement in the middle of a batch that
// fails (an object dropped in the meantime) has the rest read one by one,
// a missing object read as nothing; one that fails for another reason
// fails the read as it fails alone; a server that refuses queries of
// several statements has the statements spread over connections, from then
// on, with one note.
func TestMySQLShowBatchFallbacks(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLShowBatchFallbacks(t, dsn) })
	}
}

func testMySQLShowBatchFallbacks(t *testing.T, dsn string) {
	ctx := context.Background()
	work := "dgt_show_fallback"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
	mustExec(t, mysqlAdmin(t, dsn, work), showSchema...)
	reqs := []showReq{
		{"TABLE", work, "news"},
		{"TABLE", work, "gone"}, // 1146, mid-batch
		{"VIEW", work, "news_titles"},
		{"TRIGGER", work, "gone_trigger"}, // 1360
		{"TRIGGER", work, "news_views"},
		{"TABLE", work, "odd; name`s"},
		{"TABLE", work, "plain"},
	}
	open := func(t *testing.T) (*MySQL, *strings.Builder, *sql.Conn) {
		var log strings.Builder
		m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Log: &log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.Close() })
		c, err := m.session(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return m, &log, c
	}
	m, _, c := open(t)
	want := make([]map[string]string, len(reqs))
	if err := showEach(ctx, c, reqs, want); err != nil {
		t.Fatal(err)
	}
	if want[0] == nil || want[1] != nil || want[3] != nil || want[5] == nil || want[6] == nil {
		t.Fatalf("one by one: %v", want)
	}
	check := func(t *testing.T, how string, got []map[string]string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", how, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v\nwant %v", how, got, want)
		}
	}

	t.Run("missing mid-batch", func(t *testing.T) {
		// The batch itself stops at the missing table.
		part := make([]map[string]string, len(reqs))
		n, err := m.showMany(ctx, reqs, part)
		if n != 1 || !dbx.ShowCreateMissing(err) {
			t.Fatalf("batch read %d, then %v", n, err)
		}
		got, err := m.showAll(ctx, c, reqs)
		check(t, "showAll", got, err)
		if m.multiRefused.Load() {
			t.Fatal("a missing table taken for a refused batch")
		}
		// The connection of the failed batch is not used again: the next
		// batch reads every statement.
		ok := []showReq{reqs[0], reqs[2], reqs[4], reqs[5], reqs[6]}
		part = make([]map[string]string, len(ok))
		if n, err := m.showMany(ctx, ok, part); n != len(ok) || err != nil {
			t.Fatalf("batch after a failed one: %d, %v", n, err)
		}
		// A missing object first: read alone, the rest batched, not
		// refused.
		m2, _, c2 := open(t)
		first := append([]showReq{reqs[1]}, reqs...)
		got, err = m2.showAll(ctx, c2, first)
		check(t, "missing first", got[1:], err)
		if got[0] != nil || m2.multiRefused.Load() {
			t.Fatalf("missing first: %v, refused %v", got[0], m2.multiRefused.Load())
		}
	})

	t.Run("other error", func(t *testing.T) {
		// SHOW CREATE VIEW of a table fails (1347) alone, batched, after a
		// refused batch, and spread.
		bad := []showReq{reqs[0], {"VIEW", work, "plain"}, reqs[6]}
		_, alone := show(ctx, c, "VIEW", work, "plain")
		if alone == nil {
			t.Fatal("SHOW CREATE VIEW of a table did not fail")
		}
		for _, how := range []string{"batch", "spread"} {
			m, _, c := open(t)
			if how == "spread" {
				m.multiRefused.Store(true)
			}
			if _, err := m.showAll(ctx, c, bad); err == nil || err.Error() != alone.Error() {
				t.Fatalf("%s: %v, want %v", how, err, alone)
			}
		}
	})

	t.Run("chunks", func(t *testing.T) {
		// Batches of two: the offsets of the chunks, and a missing object
		// in the second one, after which the batches go on.
		defer func(n int) { showBatchMax = n }(showBatchMax)
		showBatchMax = 2
		var calls []string
		defer func() { showManyFunc = (*MySQL).showMany }()
		showManyFunc = func(m *MySQL, ctx context.Context, reqs []showReq, out []map[string]string) (int, error) {
			n, err := m.showMany(ctx, reqs, out)
			calls = append(calls, fmt.Sprintf("%s:%d/%d", reqs[0].name, n, len(reqs)))
			return n, err
		}
		m, _, c := open(t)
		got, err := m.showAll(ctx, c, reqs)
		check(t, "chunks", got, err)
		// [news gone] stops at gone (read alone); [news_titles
		// gone_trigger] stops at the trigger; then [news_views odd],
		// [plain].
		if want := []string{"news:1/2", "news_titles:1/2", "news_views:2/2", "plain:1/1"}; !slices.Equal(calls, want) {
			t.Fatalf("batches %v, want %v", calls, want)
		}
	})

	t.Run("refused after good batches", func(t *testing.T) {
		// A proxy that stops passing the multi-statement flag on after
		// batches worked: the first 1064 switches to spread for the rest,
		// with one note, and no batch is sent again.
		defer func(n int) { showBatchMax = n }(showBatchMax)
		showBatchMax = 2
		calls := 0
		defer func() { showManyFunc = (*MySQL).showMany }()
		showManyFunc = func(m *MySQL, ctx context.Context, reqs []showReq, out []map[string]string) (int, error) {
			calls++
			if calls > 1 {
				return 0, &mysql.MySQLError{Number: 1064, Message: "syntax"}
			}
			return m.showMany(ctx, reqs, out)
		}
		m, log, c := open(t)
		ok := []showReq{reqs[0], reqs[2]}
		if _, err := m.showAll(ctx, c, ok); err != nil || !m.multiOK.Load() {
			t.Fatalf("first batch: %v", err)
		}
		got, err := m.showAll(ctx, c, reqs)
		check(t, "refused later", got, err)
		if _, err := m.showAll(ctx, c, reqs); err != nil {
			t.Fatal(err)
		}
		if calls != 2 || !m.multiRefused.Load() || strings.Count(log.String(), "SHOW CREATE is read one statement at a time") != 1 {
			t.Fatalf("%d batches, refused %v, log %q", calls, m.multiRefused.Load(), log.String())
		}
	})

	t.Run("misaligned", func(t *testing.T) {
		// A batch that read every statement and then failed (more results
		// than statements): its results are read again one by one.
		defer func() { showManyFunc = (*MySQL).showMany }()
		showManyFunc = func(m *MySQL, ctx context.Context, reqs []showReq, out []map[string]string) (int, error) {
			for i := range out {
				out[i] = map[string]string{"Create Table": "shifted"}
			}
			return len(reqs), fmt.Errorf("SHOW CREATE: more results than %d statements", len(reqs))
		}
		m, _, c := open(t)
		got, err := m.showAll(ctx, c, reqs)
		check(t, "misaligned", got, err)
	})

	t.Run("first statement fails", func(t *testing.T) {
		// Not a refusal: the error is the statement's, as alone.
		bad := []showReq{{"VIEW", work, "plain"}, reqs[0]}
		_, alone := show(ctx, c, "VIEW", work, "plain")
		m, log, c := open(t)
		if _, err := m.showAll(ctx, c, bad); err == nil || err.Error() != alone.Error() {
			t.Fatalf("%v, want %v", err, alone)
		}
		if m.multiRefused.Load() || log.Len() > 0 {
			t.Fatalf("refused %v, log %q", m.multiRefused.Load(), log.String())
		}
	})

	for _, how := range []string{"closed", "refused login"} {
		t.Run("no connection: "+how, func(t *testing.T) {
			// No batch can be sent: one by one, with one note, without a
			// connection tried again for each object; not a refusal.
			m, log, c := open(t)
			m.multi.Close()
			if how == "refused login" {
				// A server error as the connection is made, as 1040 (too
				// many connections) or 1226 (max_user_connections) are.
				u, _ := url.Parse(dsn)
				u.User = url.UserPassword(u.User.Username(), "not the password")
				multi, err := dbx.OpenMySQLWith(u.String(), "", nil, dbx.MySQLOpen{MultiStatements: true, Init: m.initMulti})
				if err != nil {
					t.Fatal(err)
				}
				m.multi = multi
			}
			calls := 0
			defer func() { showManyFunc = (*MySQL).showMany }()
			showManyFunc = func(m *MySQL, ctx context.Context, reqs []showReq, out []map[string]string) (int, error) {
				calls++
				return m.showMany(ctx, reqs, out)
			}
			got, err := m.showAll(ctx, c, reqs)
			check(t, "no connection", got, err)
			if _, err := m.showAll(ctx, c, reqs); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || m.multiRefused.Load() || strings.Count(log.String(), "SHOW CREATE is read one statement at a time") != 1 {
				t.Fatalf("%d batches, refused %v, log %q", calls, m.multiRefused.Load(), log.String())
			}
		})
	}

	for _, refusal := range []string{"no multi-statements", "charset"} {
		t.Run("refused: "+refusal, func(t *testing.T) {
			m, log, c := open(t)
			switch refusal {
			case "no multi-statements":
				// A connection that did not ask for several statements per
				// query, as a proxy that does not pass the flag on: the
				// server rejects the query (1064).
				m.multi.Close()
				multi, err := dbx.OpenMySQLWith(dsn, "", nil, dbx.MySQLOpen{Init: m.initMulti})
				if err != nil {
					t.Fatal(err)
				}
				m.multi = multi
			case "charset":
				// A client character set in which 0x60 can end a character
				// (the driver's own DSN form takes charset=).
				u, _ := url.Parse(dsn)
				pass, _ := u.User.Password()
				sjis := fmt.Sprintf("%s:%s@tcp(%s)/?charset=sjis", u.User.Username(), pass, u.Host)
				m.multi.Close()
				multi, err := dbx.OpenMySQLWith(sjis, "", nil, dbx.MySQLOpen{MultiStatements: true, Init: m.initMulti})
				if err != nil {
					t.Fatal(err)
				}
				m.multi = multi
				conn, err := multi.Conn(ctx)
				if conn != nil {
					conn.Close()
				}
				if !errors.Is(err, errMultiCharset) {
					t.Fatalf("connection with sjis: %v", err)
				}
			}
			got, err := m.showAll(ctx, c, reqs)
			check(t, "refused", got, err)
			if !m.multiRefused.Load() || strings.Count(log.String(), "SHOW CREATE is read one statement at a time") != 1 {
				t.Fatalf("refused %v, log %q", m.multiRefused.Load(), log.String())
			}
			got, err = m.showAll(ctx, c, reqs)
			check(t, "refused again", got, err)
			if strings.Count(log.String(), "SHOW CREATE is read") != 1 {
				t.Fatalf("log %q", log.String())
			}
			// A whole fingerprint the same way.
			old := oldFingerprint(t, m, c, work, FingerprintFormat)
			if now := newFingerprint(t, m, work, FingerprintFormat); !reflect.DeepEqual(old, now) {
				t.Fatalf("fingerprint: %v", Diff(old, now))
			}
		})
	}
}

// TestMySQLSessionPreparedOnce: a connection is prepared when it is made,
// not each time it is taken from the pool, and every one has the session
// settings, sql_log_bin = 0 included where the server writes a binary log
// nothing reads, also one made before that was decided.
func TestMySQLSessionPreparedOnce(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			m, err := NewMySQL(Options{Name: "db", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			early, err := m.db.Conn(ctx) // made before the binary log is decided
			if err != nil {
				t.Fatal(err)
			}
			c, err := m.session(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mariadb, err := m.mariadb(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			wantLogBin := 1
			if m.unlog {
				wantLogBin = 0
			}
			settings := func(c *sql.Conn) string {
				t.Helper()
				q := "SELECT @@foreign_key_checks, @@unique_checks, @@sql_mode, @@sql_log_bin"
				if !mariadb {
					q += ", @@information_schema_stats_expiry"
				}
				row := c.QueryRowContext(ctx, q)
				var fk, uniq, logBin, expiry int
				var mode string
				dest := []any{&fk, &uniq, &mode, &logBin}
				if !mariadb {
					dest = append(dest, &expiry)
				}
				if err := row.Scan(dest...); err != nil {
					t.Fatal(err)
				}
				return fmt.Sprintf("%d %d %s %d %d", fk, uniq, mode, logBin, expiry)
			}
			want := fmt.Sprintf("0 0 NO_AUTO_VALUE_ON_ZERO %d 0", wantLogBin)
			if got := settings(c); got != want {
				t.Fatalf("session: %s, want %s", got, want)
			}
			sets := func(c *sql.Conn) int {
				t.Helper()
				var name string
				var n int
				if err := c.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Com_set_option'").Scan(&name, &n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			// The early connection is taken as a session once, and gets
			// sql_log_bin then; from there on neither is prepared again.
			early.Close()
			e2, err := m.session(ctx) // the only free connection: early
			if err != nil {
				t.Fatal(err)
			}
			id := func(c *sql.Conn) int64 {
				t.Helper()
				var n int64
				if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			before := map[int64]int{id(c): sets(c), id(e2): sets(e2)}
			if got := settings(e2); got != want {
				t.Fatalf("early session: %s, want %s", got, want)
			}
			c.Close()
			e2.Close()
			// Both connections are in the pool now: two sessions take them.
			c1, err := m.session(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c1.Close()
			c2, err := m.session(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c2.Close()
			for _, c := range []*sql.Conn{c1, c2} {
				if got := settings(c); got != want {
					t.Fatalf("pooled session: %s, want %s", got, want)
				}
				n, ok := before[id(c)]
				if !ok {
					t.Fatalf("a new connection instead of a pooled one")
				}
				if got := sets(c); got != n {
					t.Fatalf("the pooled session was prepared again: %d SETs before, %d after", n, got)
				}
			}
			if len(m.early) != 0 {
				t.Fatalf("connections left to decide: %d", len(m.early))
			}
		})
	}
}

// TestMySQLTableInfos: what a copy reads of all its tables in one query is
// what it read of each table on its own: the columns, the stored ones, the
// primary key and the estimated rows.
func TestMySQLTableInfos(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work := "dgt_table_infos"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db, showSchema...)
			mustExec(t, db,
				"CREATE TABLE pair (a INT, b BIGINT UNSIGNED, v INT AS (a * 2) VIRTUAL, PRIMARY KEY (a, b))",
				"CREATE TABLE big (id BIGINT UNSIGNED PRIMARY KEY, n INT, s INT AS (n + 1) STORED)",
				"INSERT INTO big (id, n) VALUES (1, 1), (2, 2), (3, 3)")
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			c, err := m.session(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tables := []string{"news", "tags", "totals", "audit", "odd; name`s", "plain", "pair", "big", "missing"}
			infos, err := tableInfos(ctx, c, work, tables)
			if err != nil {
				t.Fatal(err)
			}
			if infos["missing"] != nil || len(infos) != len(tables)-1 {
				t.Fatalf("tables: %v", slices.Sorted(maps.Keys(infos)))
			}
			defer func(n int64) { rowDiffMin = n }(rowDiffMin)
			rowDiffMin = 0 // readTableInfo reads the primary key too
			for _, tb := range tables[:len(tables)-1] {
				in := infos[tb]
				all, err := columns(ctx, c, work, tb, true)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := columns(ctx, c, work, tb, false)
				if err != nil {
					t.Fatal(err)
				}
				one, err := readTableInfo(ctx, c, work, tb)
				if err != nil {
					t.Fatal(err)
				}
				slices.Sort(one.pk)
				pk := slices.Sorted(slices.Values(in.pk))
				if !slices.Equal(in.all, all) || !slices.Equal(in.stored, stored) || !slices.Equal(pk, one.pk) ||
					(len(pk) == 1 && in.pkType != one.pkType) || in.rows != one.rows {
					t.Errorf("%s: %+v, one by one %+v, columns %v, stored %v", tb, in, one, all, stored)
				}
			}
			if len(infos["plain"].pk) != 0 || infos["big"].pkType != "bigint" || len(infos["pair"].pk) != 2 {
				t.Fatalf("keys: %+v %+v %+v", infos["plain"], infos["big"], infos["pair"])
			}
		})
	}
}

// TestMySQLQueryNotPooled: the connection a user's query ran on is closed,
// not put back into the pool, where its session could carry what the query
// changed.
func TestMySQLQueryNotPooled(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			pristine := "dgt_query_pool"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+pristine)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: "dgt_query_pool_work", Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			got, err := m.Query(ctx, "SELECT CONNECTION_ID()")
			if err != nil || len(got) != 1 {
				t.Fatalf("%v, %v", got, err)
			}
			if idle := m.db.Stats().Idle; idle != 0 {
				t.Fatalf("%d connections back in the pool", idle)
			}
			c, err := m.session(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var id string
			if err := c.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil || id == got[0] {
				t.Fatalf("the query's connection %s was used again (%s, %v)", got[0], id, err)
			}
		})
	}
}

// TestMySQLRestoreViewAndTableItem: items that name one table both as a
// table and as a view: the table, dropped as the view's name, is created
// again, not taken for the one it was.
func TestMySQLRestoreViewAndTableItem(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_view_table", "dgt_view_table_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db, "CREATE TABLE x (id INT PRIMARY KEY, v INT)", "INSERT INTO x VALUES (1, 1), (2, 2)")
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, "UPDATE x SET v = 9", "INSERT INTO x VALUES (3, 3)")
			if err := m.Restore(ctx, []string{"table:x", "view:x"}); err != nil {
				t.Fatal(err)
			}
			if d := Diff(newFingerprint(t, m, work, FingerprintFormat), newFingerprint(t, m, pristine, FingerprintFormat)); len(d) > 0 {
				t.Fatalf("restore left %v", d)
			}
		})
	}
}
