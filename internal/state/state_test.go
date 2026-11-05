package state

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestDiff(t *testing.T) {
	a := Fingerprint{"table:a": "1", "table:b": "2", "table:c": "3"}
	b := Fingerprint{"table:a": "1", "table:b": "x", "table:d": "4"}
	if got := Diff(a, b); !reflect.DeepEqual(got, []string{"table:b", "table:c", "table:d"}) {
		t.Fatalf("diff %v", got)
	}
}

func TestRowDiff(t *testing.T) {
	before := Rows{"faq": {"id=1": {"id": "a", "q": "b"}, "id=2": {"id": "c", "q": "d"}}}
	after := Rows{"faq": {"id=1": {"id": "a", "q": "x"}, "id=3": {"id": "e", "q": "f"}}}
	want := []RowChange{
		{Table: "faq", Key: "id=1", Change: "updated", Columns: []string{"q"}},
		{Table: "faq", Key: "id=2", Change: "deleted"},
		{Table: "faq", Key: "id=3", Change: "inserted"},
	}
	if got := RowDiff(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("row diff %+v", got)
	}
}

// mysqlServers lists test servers from DRIFTGATE_TEST_MYSQL, comma-separated
// mysql:// URLs (MySQL and MariaDB both).
// The state tests check that nothing was written to the server between
// two fingerprints, and the server's write counters are global: they get
// servers of their own when DRIFTGATE_TEST_MYSQL_STATE names them (CI),
// so the other packages' tests writing at the same time do not disturb
// them.
func mysqlServers(t *testing.T) []string {
	v := os.Getenv("DRIFTGATE_TEST_MYSQL_STATE")
	if v == "" {
		v = os.Getenv("DRIFTGATE_TEST_MYSQL")
	}
	if v == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	return strings.Split(v, ",")
}

func mustExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if err := RetryInTransition(func() error { _, err := db.Exec(s); return err }); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func mysqlAdmin(t *testing.T, dsn, database string) *sql.DB {
	t.Helper()
	cfg, err := mysqlDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = database
	cfg.Params = map[string]string{"sql_mode": "''"}
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMySQL(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQL(t, dsn) })
	}
}

func testMySQL(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_work", "dgt_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db,
		"CREATE TABLE news (id INT AUTO_INCREMENT PRIMARY KEY, title VARCHAR(50), views INT NOT NULL DEFAULT 0, published DATETIME NOT NULL DEFAULT '0000-00-00 00:00:00', slug VARCHAR(60) AS (LOWER(title)) VIRTUAL)",
		"CREATE TABLE tags (id INT AUTO_INCREMENT PRIMARY KEY, news_id INT NOT NULL, name VARCHAR(20), FOREIGN KEY (news_id) REFERENCES news(id))",
		"CREATE TABLE audit (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(50))",
		"CREATE TABLE sessions (id VARCHAR(40) PRIMARY KEY)",
		"CREATE TRIGGER news_views AFTER UPDATE ON news FOR EACH ROW INSERT INTO audit (note) VALUES (CONCAT('views ', NEW.id))",
		"CREATE VIEW news_titles AS SELECT id, title FROM news",
		"INSERT INTO news (title, views) VALUES ('First', 1), ('Second', 2), ('Third', 3)",
		"INSERT INTO tags (news_id, name) VALUES (1, 'a'), (2, 'b')",
		"DELETE FROM news WHERE id = 3", // the counter stays at 4
	)
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"sess*"}, RowTracking: []string{"news"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "run driftgate seed") {
		t.Fatalf("restore without a pristine copy: %v", err)
	}
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fp0["table:sessions"]; ok {
		t.Fatal("excluded table fingerprinted")
	}
	if len(fp0) != 5 { // 3 tables, 1 view, 1 trigger
		t.Fatalf("fingerprint items: %v", fp0)
	}
	// What a seed records is the pristine copy's own fingerprint: the
	// working database's while nothing wrote in between. Reading it leaves
	// the working database's counters as they were.
	last, tables := maps.Clone(m.last), m.tables
	seeded, err := m.SeededFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, seeded); len(d) > 0 {
		t.Fatalf("the seeded fingerprint differs from the working database's: %v", d)
	}
	if !maps.Equal(m.last, last) || m.tables != tables {
		t.Fatal("the seeded fingerprint changed the working database's counters")
	}
	rows0, err := m.Rows(ctx, []string{"table:news", "table:tags"})
	if err != nil {
		t.Fatal(err)
	}

	mustExec(t, db,
		"UPDATE news SET views = views + 1 WHERE id = 1", // fires the trigger: audit changes too
		"INSERT INTO tags (news_id, name) VALUES (2, 'c')",
		"ALTER TABLE tags ADD COLUMN color VARCHAR(10)",
		"CREATE TABLE extra (id INT PRIMARY KEY)",
		"DROP VIEW news_titles",
		"INSERT INTO sessions VALUES ('s1')",
	)
	rows1, err := m.Rows(ctx, []string{"table:news"})
	if err != nil {
		t.Fatal(err)
	}
	if got := RowDiff(Rows{"news": rows0["news"]}, rows1); len(got) != 1 || got[0].Key != "id=1" || !reflect.DeepEqual(got[0].Columns, []string{"views"}) {
		t.Fatalf("row changes %+v", got)
	}
	fp1, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := Diff(fp0, fp1)
	want := []string{"table:audit", "table:extra", "table:tags", "table:news", "view:news_titles"}
	if !sameItems(changed, want) {
		t.Fatalf("changed %v, want %v", changed, want)
	}
	if err := m.Restore(ctx, changed); err != nil {
		t.Fatal(err)
	}
	fp2, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, fp2); len(d) > 0 {
		t.Fatalf("after restore still different: %v", d)
	}
	// The counter came back too: the next news gets id 4, as before.
	mustExec(t, db, "INSERT INTO news (title) VALUES ('Nova')")
	var id int
	if err := db.QueryRow("SELECT MAX(id) FROM news").Scan(&id); err != nil || id != 4 {
		t.Fatalf("next id %d (%v), want 4", id, err)
	}
	// The trigger works on the restored table.
	mustExec(t, db, "UPDATE news SET views = 9 WHERE id = 2")
	var notes int
	db.QueryRow("SELECT COUNT(*) FROM audit").Scan(&notes)
	if notes != 1 {
		t.Fatalf("trigger fired %d times", notes)
	}
	// The excluded table is left alone.
	var sessions int
	db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&sessions)
	if sessions != 1 {
		t.Fatal("excluded table was restored")
	}
	// A full restore (no list) finds the differences itself.
	if err := m.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	fp3, _ := m.Fingerprint(ctx, nil)
	if d := Diff(fp0, fp3); len(d) > 0 {
		t.Fatalf("after full restore still different: %v", d)
	}
}

func sameItems(a, b []string) bool {
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

// TestPostgres runs with both ways of copying a database: WAL_LOG, the
// default, and FILE_COPY, which large databases get.
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	defer func(n int64) { fileCopyMin = n }(fileCopyMin)
	for _, n := range []int64{fileCopyMin, 0} {
		fileCopyMin = n
		t.Run(fmt.Sprintf("FILE_COPY from %d bytes", n), func(t *testing.T) { testPostgres(t, dsn) })
	}
}

func testPostgres(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_work", "dgt_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RowTracking: []string{"news"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mustExec(t, p.admin, "DROP DATABASE IF EXISTS "+work+" WITH (FORCE)", "DROP DATABASE IF EXISTS "+pristine+" WITH (FORCE)", "CREATE DATABASE "+work)
	t.Cleanup(func() {
		p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
		p.admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
	})
	db, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db,
		"CREATE TABLE news (id serial PRIMARY KEY, title text, views int NOT NULL DEFAULT 0)",
		"CREATE TABLE tags (id serial PRIMARY KEY, news_id int NOT NULL REFERENCES news(id), name text)",
		"CREATE VIEW news_titles AS SELECT id, title FROM news",
		"INSERT INTO news (title, views) VALUES ('First', 1), ('Second', 2), ('Third', 3)",
		"INSERT INTO tags (news_id, name) VALUES (1, 'a')",
	)
	db.Close()
	var version int
	if err := p.admin.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	// STRATEGY came with PostgreSQL 15; before, every copy is a file copy.
	if got := p.fileCopy(ctx, work); got != (fileCopyMin == 0 && version >= 150000) {
		t.Fatalf("FILE_COPY %v with %d bytes as the limit (server %d)", got, fileCopyMin, version)
	}
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp0) != 5 { // 2 tables, 1 view, 2 sequences
		t.Fatalf("fingerprint items: %v", fp0)
	}
	seeded, err := p.SeededFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, seeded); len(d) > 0 {
		t.Fatalf("the seeded fingerprint differs from the working database's: %v", d)
	}
	// The pristine database accepts connections only while it is read.
	var allow bool
	if err := p.admin.QueryRow("SELECT datallowconn FROM pg_database WHERE datname = $1", pristine).Scan(&allow); err != nil || allow {
		t.Fatalf("the pristine database accepts connections after its fingerprint: %v %v", allow, err)
	}
	rows0, err := p.Rows(ctx, []string{"table:public.news"})
	if err != nil {
		t.Fatal(err)
	}
	db, _ = p.open(work)
	mustExec(t, db, "UPDATE news SET views = views + 1 WHERE id = 2", "INSERT INTO tags (news_id, name) VALUES (2, 'b')", "ALTER TABLE news ADD COLUMN color text")
	db.Close()
	rows1, err := p.Rows(ctx, []string{"table:public.news"})
	if err != nil {
		t.Fatal(err)
	}
	if got := RowDiff(rows0, rows1); len(got) != 3 || got[1].Key != "id=2" || !reflect.DeepEqual(got[1].Columns, []string{"color", "views"}) {
		t.Fatalf("row changes %+v", got)
	}
	fp1, _ := p.Fingerprint(ctx, nil)
	changed := Diff(fp0, fp1)
	if !sameItems(changed, []string{"table:public.news", "table:public.tags", "sequence:public.tags_id_seq"}) {
		t.Fatalf("changed %v", changed)
	}
	if err := p.Restore(ctx, changed); err != nil {
		t.Fatal(err)
	}
	fp2, _ := p.Fingerprint(ctx, nil)
	if d := Diff(fp0, fp2); len(d) > 0 {
		t.Fatalf("after restore still different: %v", d)
	}
	if err := p.Restore(ctx, []string{}); err != nil {
		t.Fatal(fmt.Errorf("restore with nothing to do: %w", err))
	}
}

// TestRestoreSpeed checks that restoring one changed table is much faster
// than copying every table again, which is itself faster than importing a
// dump (no SQL text to parse).
func TestRestoreSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	dsn := mysqlServers(t)[0]
	ctx := context.Background()
	work, pristine := "dgt_speed", "dgt_speed_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, "CREATE TABLE digits (d INT)", "INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)")
	const tables = 20
	for i := 0; i < tables; i++ {
		mustExec(t, db,
			fmt.Sprintf("CREATE TABLE t%02d (id INT AUTO_INCREMENT PRIMARY KEY, body VARCHAR(200))", i),
			fmt.Sprintf(`INSERT INTO t%02d (body) SELECT REPEAT(MD5(a.d*1000+b.d*100+c.d*10+e.d), 6)
				FROM digits a, digits b, digits c, digits e, digits f`, i)) // 100,000 rows
	}
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := make([]string, 0, len(fp))
	for it := range fp {
		all = append(all, it)
	}
	// The best of a few runs each, against a ratio well below the expected
	// one (about tables+1): a machine busy with other tests slows down single
	// runs, while the best run still shows the work done.
	best := func(items []string, runs int) time.Duration {
		var low time.Duration
		for i := 0; i < runs; i++ {
			mustExec(t, db, "UPDATE t07 SET body = 'x' WHERE id = 1")
			start := time.Now()
			if err := m.Restore(ctx, items); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); i == 0 || d < low {
				low = d
			}
		}
		return low
	}
	one := best([]string{"table:t07"}, 3)
	full := best(all, 2)
	start := time.Now()
	if _, err := m.Fingerprint(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("restore of 1 of %d tables: %s; of all tables: %s (%.1f× faster); fingerprint of all: %s",
		tables+1, one, full, float64(full)/float64(one), time.Since(start))
	if one*3 > full {
		t.Fatalf("one table %s is not clearly faster than all tables %s", one, full)
	}
}

// TestMySQLEdgeCases covers what a review found: columns with default
// expressions, triggers during a restore, their SQL mode and firing order,
// views on excluded tables, an object that changes kind, a dot in a table
// name, and MariaDB sequences and system-versioned tables.
func TestMySQLEdgeCases(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLEdgeCases(t, dsn) })
	}
}

func testMySQLEdgeCases(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_edge", "dgt_edge_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	var version string
	db.QueryRow("SELECT VERSION()").Scan(&version)
	mariadb := strings.Contains(version, "MariaDB")
	mustExec(t, db,
		"CREATE TABLE ts (id INT PRIMARY KEY, created TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP, changed TIMESTAMP NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP)",
		"INSERT INTO ts VALUES (1, '2020-01-01 00:00:00', '2020-01-02 00:00:00')",
		"CREATE TABLE t (id INT AUTO_INCREMENT PRIMARY KEY, n INT)",
		"CREATE TABLE audit (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(20))",
		"CREATE TABLE ord (id INT AUTO_INCREMENT PRIMARY KEY, s VARCHAR(20))",
		"CREATE TABLE excl_sessions (id INT PRIMARY KEY)",
		"CREATE TABLE `a.b` (id INT PRIMARY KEY)",
		"INSERT INTO `a.b` VALUES (1)",
		"CREATE VIEW v_excl AS SELECT id FROM excl_sessions",
		"CREATE VIEW v_t AS SELECT id FROM t",
		"SET SESSION sql_mode = 'STRICT_TRANS_TABLES'",
		"CREATE TRIGGER t_ai AFTER INSERT ON t FOR EACH ROW INSERT INTO audit (note) VALUES ('t')",
		"CREATE TRIGGER zz BEFORE INSERT ON ord FOR EACH ROW SET NEW.s = CONCAT(NEW.s, 'z')",
		"CREATE TRIGGER aa BEFORE INSERT ON ord FOR EACH ROW SET NEW.s = CONCAT(NEW.s, 'a')",
		"SET SESSION sql_mode = ''",
		"INSERT INTO t (n) VALUES (1), (2)",
	)
	if mariadb {
		mustExec(t, db, "CREATE SEQUENCE seq1")
	}
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"excl*"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := m.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fp0, err := m.fingerprint(ctx, c, work, nil)
	if err != nil {
		t.Fatal(err)
	}
	fpPristine, err := m.fingerprint(ctx, c, pristine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, fpPristine); len(d) > 0 {
		t.Fatalf("pristine copy differs from the working database: %v", d)
	}

	mustExec(t, db,
		"INSERT INTO t (n) VALUES (3)", // fires t_ai
		"INSERT INTO ord (s) VALUES ('x')",
		"UPDATE ts SET created = '2021-01-01 00:00:00' WHERE id = 1",
		"INSERT INTO `a.b` VALUES (2)",
		"DROP VIEW v_t",
		"CREATE TABLE v_t (id INT)",
	)
	if mariadb {
		mustExec(t, db, "SELECT NEXTVAL(seq1)", "SELECT NEXTVAL(seq1)")
	}
	fp1, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := Diff(fp0, fp1)
	for _, want := range []string{"table:a.b", "table:t", "table:audit", "table:ord", "table:ts", "view:v_t", "table:v_t"} {
		if !contains(changed, want) {
			t.Errorf("%s not seen as changed: %v", want, changed)
		}
	}
	if mariadb && !contains(changed, "sequence:seq1") {
		t.Errorf("sequence not seen as changed: %v", changed)
	}
	if err := m.Restore(ctx, changed); err != nil {
		t.Fatal(err)
	}
	fp2, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, fp2); len(d) > 0 {
		t.Fatalf("after restore still different: %v", d)
	}
	// The triggers fire in their old order, with their old SQL mode.
	mustExec(t, db, "INSERT INTO ord (s) VALUES ('')")
	var s string
	db.QueryRow("SELECT s FROM ord ORDER BY id DESC LIMIT 1").Scan(&s)
	if s != "za" {
		t.Errorf("triggers fired as %q, want za", s)
	}
	var mode string
	db.QueryRow("SELECT SQL_MODE FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? AND TRIGGER_NAME = 't_ai'", work).Scan(&mode)
	if !strings.Contains(mode, "STRICT_TRANS_TABLES") {
		t.Errorf("trigger sql_mode %q", mode)
	}

	if mariadb {
		mustExec(t, db, "CREATE TABLE hist (id INT PRIMARY KEY) WITH SYSTEM VERSIONING")
		if _, err := m.Fingerprint(ctx, nil); err == nil || !strings.Contains(err.Error(), "exclude") {
			t.Errorf("system-versioned table: %v", err)
		}
	}
}

// samePristine checks that the working database and its pristine copy have
// the same fingerprint, read the way a restore compares them.
func samePristine(t *testing.T, m *MySQL) Fingerprint {
	t.Helper()
	ctx := context.Background()
	c, err := m.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w, err := m.fingerprint(ctx, c, m.o.Working, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.fingerprint(ctx, c, m.o.Pristine, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(w, p); len(d) > 0 {
		t.Fatalf("the pristine copy differs from the working database: %v", d)
	}
	return w
}

// TestMySQLVirtualColumns: two copies of the same rows of a table with
// generated columns have the same fingerprint (MariaDB 11.4's CHECKSUM
// TABLE … EXTENDED of a table with VIRTUAL ones can differ, MariaDB 10.6's
// of one with STORED ones), and every change of its stored data still
// shows and is restored.
func TestMySQLVirtualColumns(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLVirtualColumns(t, dsn) })
	}
}

func testMySQLVirtualColumns(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_virtual", "dgt_virtual_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db,
		"CREATE TABLE digits (d INT)",
		"INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)",
		`CREATE TABLE v (id INT AUTO_INCREMENT PRIMARY KEY, s VARCHAR(20) NULL, f FLOAT, d DOUBLE, u VARCHAR(20) UNIQUE, ts TIMESTAMP(3) NULL,
			label VARCHAR(50) AS (CONCAT(s, '-', u)) VIRTUAL, len INT AS (CHAR_LENGTH(u)) VIRTUAL, h INT AS (CHAR_LENGTH(s)) STORED, KEY (len))`,
		`INSERT INTO v (id, s, f, d, u, ts) SELECT a.d*1000+b.d*100+c.d*10+e.d+1, IF(a.d = 3, NULL, CONCAT('s', b.d, c.d)), (c.d + 0.1) / 3, e.d / 7,
			CONCAT('u', a.d*1000+b.d*100+c.d*10+e.d), '2026-03-29 01:30:00.125' FROM digits a, digits b, digits c, digits e`,
		"UPDATE v SET f = 16777216 WHERE id = 600",
		// (MariaDB's CHECKSUM TABLE of a table with a BLOB column was stable.)
		"CREATE TABLE bl (id INT PRIMARY KEY, b BLOB, t TEXT, n INT AS (LENGTH(b)) VIRTUAL)",
		"INSERT INTO bl (id, b, t) SELECT d, REPEAT(UNHEX('00ff'), d), REPEAT('t', d) FROM digits",
		"CREATE TABLE nopk (s VARCHAR(20), x VARCHAR(25) AS (CONCAT('x', s)) VIRTUAL)",
		"INSERT INTO nopk (s) VALUES ('a'), ('b'), (NULL)",
		"CREATE TABLE st (id INT PRIMARY KEY, s VARCHAR(20), h INT AS (CHAR_LENGTH(s)) STORED, j VARCHAR(40) AS (CONCAT(s, s)) STORED)",
		"INSERT INTO st (id, s) SELECT a.d*1000+b.d*100+c.d*10+e.d, IF(a.d = 3, NULL, CONCAT('s', c.d)) FROM digits a, digits b, digits c, digits e",
	)
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"digits"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0 := samePristine(t, m)
	if got := mustFingerprint(t, m); !reflect.DeepEqual(got, fp0) {
		t.Fatalf("fingerprint %v, want %v", got, fp0)
	}

	cases := []struct {
		name   string
		writes []string
		table  string
	}{
		{"insert", []string{"INSERT INTO v (s, u) VALUES ('new', 'unew')"}, "v"},
		{"delete", []string{"DELETE FROM v WHERE id = 77"}, "v"},
		{"update", []string{"UPDATE v SET s = 'changed' WHERE id = 5000"}, "v"},
		{"NULL to ''", []string{"UPDATE v SET s = '' WHERE id = 3001"}, "v"},
		{"'' to NULL", []string{"UPDATE v SET s = NULL WHERE id = 1"}, "v"},
		{"row moved", []string{"UPDATE v SET id = 20000 WHERE id = 10"}, "v"},
		{"values swapped", []string{"UPDATE v SET s = IF(id = 1, 's10', 's00') WHERE id IN (1, 101)"}, "v"},
		{"FLOAT printed alike", []string{"UPDATE v SET f = 16777218 WHERE id = 600"}, "v"},
		{"DOUBLE", []string{"UPDATE v SET d = d + 1e-15 WHERE id = 700"}, "v"},
		{"TIMESTAMP", []string{"UPDATE v SET ts = ts + INTERVAL 1 MICROSECOND WHERE id = 800", "UPDATE v SET ts = '2026-03-29 01:30:00.126' WHERE id = 801"}, "v"},
		{"BLOB", []string{"UPDATE bl SET b = CONCAT(b, 'x') WHERE id = 9"}, "bl"},
		{"TEXT", []string{"UPDATE bl SET t = NULL WHERE id = 0"}, "bl"},
		{"duplicate row without a key", []string{"INSERT INTO nopk (s) VALUES ('a')"}, "nopk"},
		{"stored generated column", []string{"UPDATE st SET s = 'x' WHERE id = 5"}, "st"},
	}
	for _, tc := range cases {
		mustExec(t, db, tc.writes...)
		changed := Diff(fp0, mustFingerprint(t, m))
		if !reflect.DeepEqual(changed, []string{"table:" + tc.table}) {
			t.Errorf("%s: changed %v, want table:%s", tc.name, changed, tc.table)
			continue
		}
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
			t.Errorf("%s: after the restore still different: %v", tc.name, d)
		}
	}
	samePristine(t, m)

	// Tables without generated columns are still checksummed by the server,
	// on MySQL also those with STORED ones only.
	c, err := m.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defs := map[string]string{}
	for _, tb := range []string{"v", "st", "digits"} {
		if defs[tb], err = showCreate(ctx, c, "TABLE", work, tb); err != nil {
			t.Fatal(err)
		}
	}
	mariadb, err := m.mariadb(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	sums, err := checksumGroup(ctx, c, work, []string{"v", "st", "digits"}, defs, mariadb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sums["v"], "stored ") || strings.HasPrefix(sums["st"], "stored ") != mariadb || strings.HasPrefix(sums["digits"], "stored ") {
		t.Errorf("checksums %v", sums)
	}
}

// TestMySQLZeroAutoIncrement: a row with 0 in its AUTO_INCREMENT column
// keeps it through a seed and a restore, whole or by chunks (the sessions
// copy with NO_AUTO_VALUE_ON_ZERO, as mysqldump does).
func TestMySQLZeroAutoIncrement(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLZeroAutoIncrement(t, dsn) })
	}
}

func testMySQLZeroAutoIncrement(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_zero", "dgt_zero_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db,
		"CREATE TABLE digits (d INT)",
		"INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)",
		"CREATE TABLE z (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(20))",
		"INSERT INTO z SELECT a.d*1000+b.d*100+c.d*10+e.d+1, 'v' FROM digits a, digits b, digits c, digits e",
		"INSERT INTO z VALUES (20000, 'zero')",
		"UPDATE z SET id = 0 WHERE id = 20000", // an UPDATE keeps the 0
	)
	zero := func(db string) string {
		t.Helper()
		var v string
		if err := root.QueryRow("SELECT v FROM " + db + ".z WHERE id = 0").Scan(&v); err != nil {
			t.Errorf("%s: no row with id 0: %v", db, err)
		}
		return v
	}
	var log bytes.Buffer
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"digits"}, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	zero(pristine)
	fp0 := samePristine(t, m)

	defer func(min int64) { rowDiffMin = min }(rowDiffMin)
	for _, mode := range []string{"whole", "chunks"} {
		rowDiffMin = 1 << 60
		if mode == "chunks" {
			rowDiffMin = 0
		}
		mustExec(t, db, "UPDATE z SET v = 'changed' WHERE id = 0", "DELETE FROM z WHERE id = 5")
		changed := Diff(fp0, mustFingerprint(t, m))
		if !reflect.DeepEqual(changed, []string{"table:z"}) {
			t.Fatalf("%s: changed %v", mode, changed)
		}
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if v := zero(work); v != "zero" {
			t.Errorf("%s: row 0 holds %q", mode, v)
		}
		if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
			t.Errorf("%s: after the restore still different: %v", mode, d)
		}
		if strings.Contains(log.String(), "copying it whole") {
			t.Errorf("%s: %s", mode, log.String())
		}
		log.Reset()
	}
	samePristine(t, m)
}

func TestPostgresColumnLength(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: "dgt_len", Pristine: "dgt_len_pristine"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mustExec(t, p.admin, "DROP DATABASE IF EXISTS dgt_len WITH (FORCE)", "CREATE DATABASE dgt_len")
	t.Cleanup(func() { p.admin.Exec("DROP DATABASE IF EXISTS dgt_len WITH (FORCE)") })
	db, _ := p.open("dgt_len")
	mustExec(t, db, "CREATE TABLE t (s varchar(10))")
	fp0, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "ALTER TABLE t ALTER COLUMN s TYPE varchar(50)")
	db.Close()
	fp1, _ := p.Fingerprint(ctx, nil)
	if d := Diff(fp0, fp1); len(d) != 1 {
		t.Fatalf("length change not seen: %v", d)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/dgt_len"
	if _, err := NewPostgres(Options{Name: "db", DSN: u.String(), Working: "dgt_len", Pristine: "x"}); err == nil {
		t.Fatal("dsn naming the working database accepted")
	}
}

// TestQuery runs parameter queries on the pristine copy, read-only.
func TestQuery(t *testing.T) {
	ctx := context.Background()
	t.Run("mysql", func(t *testing.T) {
		dsn := mysqlServers(t)[0]
		root := mysqlAdmin(t, dsn, "")
		mustExec(t, root, "DROP DATABASE IF EXISTS dgt_q", "DROP DATABASE IF EXISTS dgt_q_pristine", "CREATE DATABASE dgt_q",
			"CREATE TABLE dgt_q.news (id INT PRIMARY KEY, slug VARCHAR(20))", "INSERT INTO dgt_q.news VALUES (3, 'c'), (1, NULL), (2, 'b')")
		t.Cleanup(func() {
			root.Exec("DROP DATABASE IF EXISTS dgt_q")
			root.Exec("DROP DATABASE IF EXISTS dgt_q_pristine")
		})
		m, _ := NewMySQL(Options{Name: "db", DSN: dsn, Working: "dgt_q", Pristine: "dgt_q_pristine"})
		defer m.Close()
		if _, err := m.Query(ctx, "SELECT id FROM news ORDER BY id"); err == nil {
			t.Fatal("query before seed should fail")
		}
		if err := m.Seed(ctx); err != nil {
			t.Fatal(err)
		}
		mustExec(t, root, "INSERT INTO dgt_q.news VALUES (4, 'd')") // only in the working database
		got, err := m.Query(ctx, "SELECT id, slug FROM news ORDER BY id")
		if err != nil || strings.Join(got, ",") != "1,2,3" {
			t.Fatalf("query: %v %v", got, err)
		}
		if _, err := m.Query(ctx, "DELETE FROM news ORDER BY id"); err == nil {
			t.Fatal("a write was allowed")
		}
		// DDL commits the read-only transaction implicitly: it is refused
		// before it reaches the server.
		for _, q := range []string{"DROP TABLE dgt_q.news", "CREATE TABLE dgt_q.x AS SELECT 1 AS a ORDER BY a", "SELECT id FROM news ORDER BY id INTO OUTFILE '/tmp/dgt_q'"} {
			if _, err := m.Query(ctx, q); err == nil {
				t.Errorf("%s: allowed", q)
			}
		}
		if err := root.QueryRow("SELECT COUNT(*) FROM dgt_q.news").Scan(new(int)); err != nil {
			t.Fatalf("the working table is gone: %v", err)
		}
		// The pooled connections stay writable for restores.
		mustExec(t, root, "DELETE FROM dgt_q.news")
		if err := m.Restore(ctx, nil); err != nil {
			t.Fatalf("restore after a query: %v", err)
		}
		var n int
		root.QueryRow("SELECT COUNT(*) FROM dgt_q_pristine.news").Scan(&n)
		if n != 3 {
			t.Fatalf("pristine has %d rows", n)
		}
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
		if dsn == "" {
			t.Skip("DRIFTGATE_TEST_POSTGRES not set")
		}
		p, _ := NewPostgres(Options{Name: "db", DSN: dsn, Working: "dgt_q", Pristine: "dgt_q_pristine"})
		defer p.Close()
		mustExec(t, p.admin, "DROP DATABASE IF EXISTS dgt_q WITH (FORCE)", "DROP DATABASE IF EXISTS dgt_q_pristine WITH (FORCE)", "CREATE DATABASE dgt_q")
		t.Cleanup(func() {
			p.admin.Exec("DROP DATABASE IF EXISTS dgt_q WITH (FORCE)")
			p.admin.Exec("DROP DATABASE IF EXISTS dgt_q_pristine WITH (FORCE)")
		})
		db, _ := p.open("dgt_q")
		mustExec(t, db, "CREATE TABLE news (id int PRIMARY KEY, slug text)", "INSERT INTO news VALUES (2, 'b'), (1, 'a')")
		db.Close()
		if err := p.Seed(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := p.Query(ctx, "SELECT slug FROM news ORDER BY id")
		if err != nil || strings.Join(got, ",") != "a,b" {
			t.Fatalf("query: %v %v", got, err)
		}
		if _, err := p.Query(ctx, "DELETE FROM news"); err == nil {
			t.Fatal("a write was allowed")
		}
		// COPY TO PROGRAM and functions that run SQL text run inside a
		// read-only transaction; they are refused before they reach the
		// server. A query without columns is an error, not a panic.
		mark := filepath.Join(t.TempDir(), "ran")
		for _, q := range []string{
			"COPY (SELECT 1) TO PROGRAM 'touch " + mark + "'",
			"SELECT query_to_xml('COPY (SELECT 1) TO PROGRAM ''touch " + mark + "''', true, true, '') ORDER BY 1",
			"SELECT pg_read_file('/etc/hosts') ORDER BY 1",
			"SELECT FROM news ORDER BY id",
		} {
			if _, err := p.Query(ctx, q); err == nil {
				t.Errorf("%s: allowed", q)
			}
		}
		if _, err := os.Stat(mark); err == nil {
			t.Error("a program ran")
		}
		// The pristine database closed again: a restore can use it as template.
		if err := p.Restore(ctx, nil); err != nil {
			t.Fatalf("restore after a query: %v", err)
		}
	})
}

func TestMySQLWriteCheck(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		// Separate databases; the per-table runs wait for whole seconds.
		t.Run(u.Host, func(t *testing.T) { t.Parallel(); testMySQLWriteCheck(t, dsn, false) })
		t.Run(u.Host+"/without per-table counters", func(t *testing.T) { t.Parallel(); testMySQLWriteCheck(t, dsn, true) })
	}
}

// writeCheckSchema has foreign keys that change rows of other tables, a
// trigger that writes another table, an updatable view, a MyISAM table and a
// table nothing writes.
var writeCheckSchema = []string{
	"CREATE TABLE a (id INT AUTO_INCREMENT PRIMARY KEY, code VARCHAR(10) UNIQUE, n INT)",
	"INSERT INTO a (code, n) VALUES ('x', 1), ('y', 2)",
	"CREATE TABLE b (id INT PRIMARY KEY, n INT)",
	"INSERT INTO b VALUES (1, 1), (2, 2)",
	"CREATE TABLE log (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(20))",
	"CREATE TABLE audit (id INT AUTO_INCREMENT PRIMARY KEY, note VARCHAR(20))",
	"CREATE TABLE m (id INT PRIMARY KEY) ENGINE=MyISAM",
	"CREATE TABLE untouched (id INT PRIMARY KEY)",
	"INSERT INTO untouched VALUES (1)",
	"CREATE TABLE p (id INT PRIMARY KEY, k INT UNIQUE, n INT)",
	"CREATE TABLE c (id INT AUTO_INCREMENT PRIMARY KEY, pk INT, FOREIGN KEY (pk) REFERENCES p (k) ON DELETE CASCADE ON UPDATE CASCADE)",
	"CREATE TABLE gc (id INT AUTO_INCREMENT PRIMARY KEY, cid INT, FOREIGN KEY (cid) REFERENCES c (id) ON DELETE CASCADE)",
	"CREATE TABLE sn (id INT AUTO_INCREMENT PRIMARY KEY, pid INT NULL, FOREIGN KEY (pid) REFERENCES p (id) ON DELETE SET NULL ON UPDATE SET NULL)",
	"INSERT INTO p VALUES (1, 10, 1), (2, 20, 2), (3, 30, 3)",
	"INSERT INTO c (pk) VALUES (10), (20), (30)",
	"INSERT INTO gc (cid) VALUES (1), (2), (3)",
	"INSERT INTO sn (pid) VALUES (1), (2), (3)",
	"CREATE TRIGGER p_audit AFTER UPDATE ON p FOR EACH ROW INSERT INTO audit (note) VALUES (CONCAT('p ', NEW.id))",
	"CREATE VIEW vb AS SELECT id, n FROM b",
	"CREATE TABLE tally (id INT PRIMARY KEY, n INT)",
	"INSERT INTO tally VALUES (1, 0)",
	// A write MySQL's performance_schema does not count on tally.
	"CREATE TRIGGER audit_tally AFTER INSERT ON audit FOR EACH ROW UPDATE b STRAIGHT_JOIN tally SET tally.n = tally.n + 1 WHERE b.id = 1",
}

// testMySQLWriteCheck: a fingerprint the write counters let through equals
// the one read in full, after every kind of change; with nothing written it
// is not read again. With per-table counters only the written tables (and
// those their foreign keys reach) are checksummed again.
func testMySQLWriteCheck(t *testing.T, dsn string, noTables bool) {
	ctx := context.Background()
	work := "dgt_writes"
	if noTables {
		work = "dgt_writes_off"
	}
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
	db := mysqlAdmin(t, dsn, work)
	var version string
	db.QueryRow("SELECT VERSION()").Scan(&version)
	mariadb := strings.Contains(version, "MariaDB")
	mustExec(t, db, writeCheckSchema...)
	var log strings.Builder
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.noTables = noTables
	full := func() Fingerprint {
		fresh, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work})
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		fresh.noTables = true
		f, err := fresh.Fingerprint(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if _, err := m.Fingerprint(ctx, nil); err != nil {
		t.Fatal(err)
	}
	perTable := strings.Contains(m.Mode(), "per table")
	switch {
	case noTables && perTable:
		t.Fatalf("mode %q with the counters turned off", m.Mode())
	case !noTables && mariadb:
		var userstat int
		db.QueryRow("SELECT @@userstat").Scan(&userstat)
		if perTable != (userstat == 1) {
			t.Fatalf("userstat %d, mode %q", userstat, m.Mode())
		}
	case !noTables && !perTable:
		t.Fatalf("MySQL with performance_schema on: mode %q", m.Mode())
	}
	if !strings.Contains(log.String(), m.Mode()) {
		t.Errorf("mode not reported: %q", log.String())
	}
	t.Logf("mode: %s", m.Mode())
	// Other tests may write on the same server, and a write in the second of
	// the last reading or an open transaction anywhere forbids reuse: each
	// try starts in a new second, and one quiet try is enough.
	quiet := false
	for i := 0; i < 8 && !quiet; i++ {
		nextSecond()
		m.Fingerprint(ctx, nil)
		m.Fingerprint(ctx, nil)
		quiet = m.Reused()
	}
	if !quiet {
		t.Error("with nothing written the fingerprint was read in full every time")
	}

	// The application's connection, with server-side prepared statements.
	cfg, _ := mysqlDSN(dsn)
	cfg.DBName, cfg.InterpolateParams, cfg.MultiStatements = work, false, true
	app, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	// Reads the application prepares on the server leave the fingerprint
	// as it was (Laravel prepares every query).
	m.Fingerprint(ctx, nil)
	reused := false
	// On a server shared with other tests a quiet moment may take a while.
	for i := 0; i < 60 && !reused; i++ {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		var n int
		if err := app.QueryRow("SELECT n FROM a WHERE id = ?", 1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		m.Fingerprint(ctx, nil)
		reused = m.Reused()
	}
	if !reused {
		t.Error("a prepared read made the fingerprint read in full")
	}

	// Each write, and the tables the per-table path must read again after it
	// (other tests' changes of structure on the same server make it read
	// everything at times; that is never wrong).
	// alone: the step starts a second after the earlier ones, so that only
	// its own signals can tell (see written).
	writes := []struct {
		sql   string
		read  []string
		alone bool
	}{
		{"UPDATE a SET n = n + 1 WHERE id = 1", []string{"a"}, false},
		{"INSERT INTO a (code, n) VALUES ('z', 3)", []string{"a"}, false},
		{"INSERT IGNORE INTO a (code, n) VALUES ('x', 9)", []string{"a"}, true}, // no row, but the counter moves
		{"REPLACE INTO a (code, n) VALUES ('x', 10)", []string{"a"}, false},
		{"INSERT INTO a (code, n) VALUES ('y', 0) ON DUPLICATE KEY UPDATE n = 11", []string{"a"}, false},
		{"DELETE FROM b WHERE id = 2", []string{"b"}, false},
		{"UPDATE p SET n = 5 WHERE id = 1", []string{"audit", "c", "gc", "p", "sn"}, true},            // the trigger writes audit
		{"DELETE FROM p WHERE id = 1", []string{"c", "gc", "p", "sn"}, true},                          // cascades to c, gc; sets sn.pid NULL
		{"UPDATE p SET k = 21 WHERE id = 2", []string{"audit", "c", "gc", "p", "sn", "tally"}, true},  // cascades to c.pk
		{"UPDATE p SET id = 22 WHERE id = 2", []string{"audit", "c", "gc", "p", "sn", "tally"}, true}, // sets sn.pid NULL
		{"UPDATE vb SET n = 8 WHERE id = 1", []string{"b"}, false},
		{"UPDATE a, b SET a.n = a.n + 1, b.n = b.n + 1 WHERE b.id = 1", []string{"a", "b"}, false},
		{"INSERT INTO b SELECT id + 10, n FROM a", []string{"b"}, false},
		// Writes run as joins.
		{"UPDATE a SET n = n + 1 WHERE id IN (SELECT id + 1 FROM b)", nil, true},
		{"UPDATE b STRAIGHT_JOIN a SET a.n = a.n + 1 WHERE b.id = 1", []string{"a"}, true},
		{"DELETE a FROM b STRAIGHT_JOIN a ON a.code = 'z' WHERE b.id = 1", []string{"a"}, true},
		{"INSERT INTO log (note) SELECT a.code FROM a JOIN b ON b.id = 1", []string{"log"}, true},
		{"START TRANSACTION; UPDATE b SET n = 7; COMMIT", []string{"b"}, false},
		{"START TRANSACTION; UPDATE b SET n = 99; ROLLBACK", nil, false},
		{"START TRANSACTION; DELETE FROM p WHERE id = 3; ROLLBACK", nil, false},
		{"INSERT INTO m VALUES (1)", nil, false},
		{"prepared:INSERT INTO b (id, n) VALUES (?, ?)", []string{"b"}, false},
		{"prepared:INSERT INTO m VALUES (?)", nil, false},
		// Statements an application prepares on the server: rows and
		// structure change without a Com_ counter of their own.
		{"prepared:UPDATE a SET n = n + 1 WHERE id = ?", nil, false},
		{"prepared:DELETE FROM b WHERE id = ?", nil, false},
		{"prepared:DELETE FROM p WHERE id = 3", []string{"c", "gc", "p", "sn"}, false},
		{"TRUNCATE log", nil, true},
		{"INSERT INTO log (note) VALUES ('after truncate')", []string{"log"}, false},
		{"ALTER TABLE b AUTO_INCREMENT = 50", nil, true},
		{"ALTER TABLE b ADD COLUMN extra INT", nil, true},
		{"UPDATE b SET extra = 1", []string{"b"}, false},
		{"CREATE VIEW v AS SELECT id FROM a", nil, true},
		{"CREATE TRIGGER a_log AFTER UPDATE ON a FOR EACH ROW INSERT INTO log (note) VALUES ('u')", nil, true},
		{"UPDATE a SET n = 0 WHERE id = 2", []string{"a", "log"}, false},
		{"RENAME TABLE log TO swap, audit TO log, swap TO audit", nil, true},
		{"INSERT INTO untouched VALUES (2)", []string{"untouched"}, false},
		{"prepared:ALTER TABLE b AUTO_INCREMENT = 77", nil, true},
		{"prepared:TRUNCATE log", nil, true},
		{"prepared:RENAME TABLE log TO swap, audit TO log, swap TO audit", nil, true},
		{"INSERT INTO p VALUES (9, 90, 9)", []string{"c", "gc", "p", "sn"}, false},
	}
	if mariadb {
		writes = append(writes, struct {
			sql   string
			read  []string
			alone bool
		}{"CREATE SEQUENCE s", nil, false}, struct {
			sql   string
			read  []string
			alone bool
		}{"SELECT NEXTVAL(s)", nil, true}, struct {
			sql   string
			read  []string
			alone bool
		}{"SELECT NEXTVAL(s)", nil, false})
	}
	partial := 0
	for i, w := range writes {
		if perTable && w.alone {
			// The fingerprint before the write is read a second after the
			// earlier writes: otherwise the tables written in its second
			// are read anyway, and the other signals go untested.
			nextSecond()
			mustFingerprint(t, m)
		}
		var err error
		if q, ok := strings.CutPrefix(w.sql, "prepared:"); ok {
			switch strings.Count(q, "?") {
			case 2:
				_, err = app.Exec(q, 100+i, i)
			case 1:
				_, err = app.Exec(q, 100+i)
			default:
				var st *sql.Stmt
				if st, err = app.Prepare(q); err == nil {
					_, err = st.Exec()
					st.Close()
				}
			}
		} else {
			_, err = app.Exec(w.sql)
		}
		if err != nil {
			t.Fatalf("%s: %v", w.sql, err)
		}
		got, err := m.Fingerprint(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d := Diff(got, full()); len(d) > 0 {
			t.Errorf("after %s the fingerprint missed %v (reused %v, read again %v)", w.sql, d, m.Reused(), m.checked)
		}
		if m.checked != nil {
			partial++
			if noTables {
				t.Errorf("after %s: per-table path with the counters off", w.sql)
			}
			// Tables written in the second of the last reading are read
			// again too (see written); minimal reads are checked below.
			for _, want := range w.read {
				if !contains(m.checked, want) {
					t.Errorf("after %s read again %v, want %v", w.sql, m.checked, w.read)
				}
			}
		}
	}
	if perTable && partial == 0 {
		t.Error("the per-table counters were never used")
	}
	t.Logf("%d of %d writes read only the written tables", partial, len(writes))
	if perTable {
		// Two writes MySQL's performance_schema does not count, in the
		// second of the reading between them: only UPDATE_TIME, in that
		// second, tells.
		nextSecond()
		for i := 0; i < 2; i++ {
			mustExec(t, db, "UPDATE b STRAIGHT_JOIN a SET a.n = a.n + 1 WHERE b.id = 1")
			got := mustFingerprint(t, m)
			if d := Diff(got, full()); len(d) > 0 {
				t.Errorf("after a second join write in one second the fingerprint missed %v", d)
			}
		}
		// A second after the last write, only the next one is read.
		minimal(t, m, db, "INSERT INTO untouched VALUES (3)", []string{"untouched"})
		minimal(t, m, db, "UPDATE p SET n = n + 1 WHERE id = 9", []string{"audit", "c", "gc", "p", "sn", "tally"})
	}

	// A transaction still open while the fingerprint is taken: its commit
	// moves no counter, so that fingerprint must not be reused.
	tx, err := app.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE untouched SET id = id + 100"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fingerprint(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(got, full()); len(d) > 0 {
		t.Errorf("after a transaction committed later the fingerprint missed %v (reused %v)", d, m.Reused())
	}
}

// TestMySQLRestoreKeepsCounters: after a restore the next fingerprint still
// reads only the tables written since, and equals one read in full; a change
// of structure by another connection before the restore is not taken for
// the restore's own.
func TestMySQLRestoreKeepsCounters(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLRestoreKeepsCounters(t, dsn) })
	}
}

func testMySQLRestoreKeepsCounters(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_rcount", "dgt_rcount_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, writeCheckSchema...)
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the tables' creation is a second old
	full := func() Fingerprint {
		fresh, _ := NewMySQL(Options{Name: "db", DSN: dsn, Working: work})
		defer fresh.Close()
		fresh.noTables = true
		f, err := fresh.Fingerprint(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	check := func(what string) {
		t.Helper()
		got, err := m.Fingerprint(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d := Diff(got, full()); len(d) > 0 {
			t.Errorf("after %s the fingerprint missed %v (read again %v)", what, d, m.checked)
		}
	}
	fp0, err := m.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Mode(), "per table") {
		t.Skipf("no per-table counters: %s", m.Mode())
	}
	partial := 0
	for round := 0; round < 4; round++ {
		mustExec(t, db, "UPDATE p SET n = n + 1 WHERE id = 2", "INSERT INTO a (code, n) VALUES (CONCAT('r', "+fmt.Sprint(round)+"), 1)")
		check("writes")
		changed := Diff(fp0, mustFingerprint(t, m))
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatal(err)
		}
		// The check after a restore comes from the restore's own reading.
		got, err := m.Fingerprint(ctx, changed)
		if err != nil {
			t.Fatal(err)
		}
		if d := Diff(got, fp0.Subset(changed)); len(d) > 0 {
			t.Fatalf("after restore still different: %v", d)
		}
		mustExec(t, db, "INSERT INTO untouched VALUES (100)")
		check("a write after the restore")
		if m.checked != nil {
			partial++
			// The restored tables are read by the next fingerprint,
			// and so are the tables p's foreign keys reach.
			want := []string{"untouched"}
			for _, it := range changed {
				if tb, ok := strings.CutPrefix(it, "table:"); ok {
					want = append(want, tb)
				}
			}
			if contains(want, "p") {
				want = append(want, "c", "gc", "sn")
			}
			sort.Strings(want)
			if !reflect.DeepEqual(m.checked, want) {
				t.Errorf("after the restore read again %v, want %v", m.checked, want)
			}
		}
		mustExec(t, db, "DELETE FROM untouched WHERE id = 100")
		check("a delete")
		changed = Diff(fp0, mustFingerprint(t, m))
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatal(err)
		}
	}
	if partial == 0 {
		t.Error("every fingerprint after a restore read all tables")
	}
	// Another connection changes structure before the restore: the restore
	// must not count it as its own.
	mustExec(t, db, "UPDATE a SET n = 5 WHERE id = 1")
	check("a write")
	nextSecond()
	mustFingerprint(t, m)
	// A view leaves no trace in any table's counters.
	mustExec(t, db, "ALTER TABLE untouched ADD COLUMN extra INT", "CREATE VIEW other AS SELECT id FROM untouched")
	if err := m.Restore(ctx, []string{"table:a"}); err != nil {
		t.Fatal(err)
	}
	check("an ALTER and a CREATE VIEW before the restore")
	// A restore of a parent table: its children are read again.
	mustExec(t, db, "DELETE FROM p WHERE id = 3")
	check("a cascade")
	if err := m.Restore(ctx, Diff(fp0, mustFingerprint(t, m))); err != nil {
		t.Fatal(err)
	}
	check("a restore of a parent")
}

// TestMySQLOpenWritesOldErrors: a foreign key error the server still shows
// (it keeps the latest one until it restarts) is not an open transaction;
// before, it made every fingerprint read every table again. The status is
// the server's own, with the transactions other sessions (tests of other
// packages) may have open taken out of it: only the old error's changes
// are left to be mistaken for an open write.
func TestMySQLOpenWritesOldErrors(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS dgt_fkerr", "CREATE DATABASE dgt_fkerr",
				"CREATE TABLE dgt_fkerr.p (id INT PRIMARY KEY)", "CREATE TABLE dgt_fkerr.c (id INT PRIMARY KEY, pid INT, FOREIGN KEY (pid) REFERENCES dgt_fkerr.p (id))",
				"INSERT INTO dgt_fkerr.p VALUES (1)", "INSERT INTO dgt_fkerr.c VALUES (1, 1)")
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS dgt_fkerr") })
			if _, err := root.Exec("DELETE FROM dgt_fkerr.p WHERE id = 1"); err == nil {
				t.Fatal("the foreign key let the parent go")
			}
			var typ, name, status string
			if err := root.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&typ, &name, &status); err != nil {
				t.Fatal(err)
			}
			start, end := strings.Index(status, "\nTRANSACTIONS\n"), strings.Index(status, "\nFILE I/O\n")
			if start < 0 || end < start {
				t.Fatalf("no TRANSACTIONS section in\n%s", status)
			}
			fkErr := undoEntries().FindString(status[:start])
			if !strings.Contains(status[:start], "LATEST FOREIGN KEY ERROR") || fkErr == "" {
				t.Skip("the server shows the foreign key error without its changes")
			}
			var own []string
			for _, line := range strings.SplitAfter(status[start:end], "\n") {
				if !undoEntries().MatchString(line) {
					own = append(own, line)
				}
			}
			quiet := status[:start] + strings.Join(own, "") + status[end:]
			if open, err := openWritesIn(quiet); err != nil || open {
				t.Errorf("open writes %v (%v) with no transaction open", open, err)
			}
			// The same changes listed as a transaction are an open write.
			active := strings.Replace(quiet, "\nTRANSACTIONS\n", "\nTRANSACTIONS\n---TRANSACTION 1, ACTIVE 1 sec\n"+fkErr+"\n", 1)
			if open, err := openWritesIn(active); err != nil || !open {
				t.Errorf("open writes %v (%v) with a transaction that changed rows", open, err)
			}
		})
	}
}

// nextSecond waits until the clock (the server's too) is in the next second.
func nextSecond() {
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 150*time.Millisecond)))
}

// minimal checks that a write a second after the last fingerprint makes the
// next one read only the given tables. Other tests' changes of structure on
// the same server make it read everything at times: it tries a few times.
func minimal(t *testing.T, m *MySQL, db *sql.DB, write string, want []string) {
	t.Helper()
	for try := 0; try < 3; try++ {
		// A write a second before the fingerprint the next one starts from.
		mustExec(t, db, "UPDATE b SET n = n + 1")
		time.Sleep(1100 * time.Millisecond)
		mustFingerprint(t, m)
		mustExec(t, db, write)
		mustFingerprint(t, m)
		if m.checked != nil {
			if !reflect.DeepEqual(m.checked, want) {
				t.Errorf("after %s read again %v, want %v", write, m.checked, want)
			}
			return
		}
		write = strings.Replace(write, "VALUES (", "VALUES (1", 1)
	}
	t.Errorf("after %s the per-table counters were never used", write)
}

func mustFingerprint(t *testing.T, m *MySQL) Fingerprint {
	t.Helper()
	f, err := m.Fingerprint(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestMySQLCountersOff: when the server's settings do not count every write
// to the database, fingerprints read every table after any write. On MySQL
// performance_schema stops instrumenting the test database alone; MariaDB's
// userstat is global, so it is checked as the server has it.
func TestMySQLCountersOff(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			work := "dgt_counters_off"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work) })
			var version string
			root.QueryRow("SELECT VERSION()").Scan(&version)
			want := "userstat is OFF"
			if !strings.Contains(version, "MariaDB") {
				want = "setup_objects"
				mustExec(t, root, "INSERT INTO performance_schema.setup_objects (OBJECT_TYPE, OBJECT_SCHEMA, OBJECT_NAME, ENABLED, TIMED) VALUES ('TABLE', '"+work+"', '%', 'NO', 'NO')")
				t.Cleanup(func() {
					root.Exec("DELETE FROM performance_schema.setup_objects WHERE OBJECT_SCHEMA = '" + work + "'")
				})
			} else {
				var userstat int
				root.QueryRow("SELECT @@userstat").Scan(&userstat)
				if userstat == 1 {
					t.Skip("userstat is ON on this server")
				}
			}
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db, writeCheckSchema...)
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			mustFingerprint(t, m)
			if !strings.Contains(m.Mode(), want) {
				t.Fatalf("mode %q, want the reason %q", m.Mode(), want)
			}
			fresh, _ := NewMySQL(Options{Name: "db", DSN: dsn, Working: work})
			defer fresh.Close()
			for _, w := range []string{"UPDATE a SET n = 7 WHERE id = 1", "DELETE FROM p WHERE id = 1", "INSERT IGNORE INTO a (code, n) VALUES ('x', 1)"} {
				mustExec(t, db, w)
				got := mustFingerprint(t, m)
				if m.checked != nil {
					t.Errorf("after %s: per-table path with the counters off", w)
				}
				fresh.noTables = true
				if d := Diff(got, mustFingerprint(t, fresh)); len(d) > 0 {
					t.Errorf("after %s the fingerprint missed %v", w, d)
				}
			}
		})
	}
}

// TestRunErrorHidesSecrets: the error of a failed command names it with
// its secrets as *** (seed, prepare, a command component's restore).
func TestRunErrorHidesSecrets(t *testing.T) {
	err := Run(context.Background(), t.TempDir(), []string{"sh", "-c", "exit 3", "PGPASSWORD=S3cret", "-pS3cret"}, nil, nil)
	if err == nil || strings.Contains(err.Error(), "S3cret") || !strings.Contains(err.Error(), "PGPASSWORD=***") {
		t.Fatalf("%v", err)
	}
}
