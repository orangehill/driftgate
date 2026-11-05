package state

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/dbx"
)

func TestChunkRange(t *testing.T) {
	// Each key is in the range of the chunk DIV puts it in, and in no other.
	keys := []int64{0, 1, -1, 1023, -1023, 1024, -1024, 1025, -1025, 2047, -2047, 2048, -2048, 5000, -5000, 1<<62 + 7, -(1 << 62)}
	for _, id := range keys {
		k := id / rowDiffChunk // Go truncates toward zero, as DIV does
		for _, c := range []int64{k - 1, k, k + 1} {
			if got := inRange(t, chunkRange("id", c), big.NewInt(id)); got != (c == k) {
				t.Errorf("key %d in chunk %d (%s): %v", id, c, chunkRange("id", c), got)
			}
		}
	}
	// BIGINT UNSIGNED: the last chunk's bound does not fit int64.
	last := new(big.Int).SetUint64(1<<64 - 1)
	k := new(big.Int).Div(last, big.NewInt(rowDiffChunk)).Int64()
	if !inRange(t, chunkRange("id", k), last) {
		t.Errorf("largest BIGINT UNSIGNED not in %s", chunkRange("id", k))
	}
}

var bound = regexp.MustCompile(`^id (>=|>|<|<=) (-?\d+)$`)

// inRange evaluates a chunkRange condition for one key.
func inRange(t *testing.T, cond string, id *big.Int) bool {
	t.Helper()
	ok := true
	for _, part := range strings.Split(cond, " AND ") {
		m := bound.FindStringSubmatch(part)
		if m == nil {
			t.Fatalf("condition %q", cond)
		}
		v, _ := new(big.Int).SetString(m[2], 10)
		c := id.Cmp(v)
		switch m[1] {
		case ">=":
			ok = ok && c >= 0
		case ">":
			ok = ok && c > 0
		case "<":
			ok = ok && c < 0
		case "<=":
			ok = ok && c <= 0
		}
	}
	return ok
}

// rowDiffSchema has tables of every kind the chunked restore meets:
// negative, BIGINT and BIGINT UNSIGNED keys, NULL and the empty string,
// FLOAT, a unique key, a default expression, stored and VIRTUAL generated
// columns (one indexed), a composite key (copied whole), and a child whose
// foreign key cascades (a chunk deleted from the parent must not reach it).
// Each has rows in several chunks.
var rowDiffSchema = []string{
	"CREATE TABLE digits (d INT)",
	"INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)",
	`CREATE TABLE n (id INT AUTO_INCREMENT PRIMARY KEY, s VARCHAR(20) NULL, f FLOAT, d DOUBLE, u VARCHAR(20) UNIQUE, ts DATETIME DEFAULT CURRENT_TIMESTAMP,
		h INT AS (CHAR_LENGTH(s)) STORED, b VARBINARY(8), su VARCHAR(45) AS (CONCAT(s, '/', u)) VIRTUAL, ul INT AS (CHAR_LENGTH(u)) VIRTUAL, KEY (ul))`,
	`INSERT INTO n (id, s, f, d, u, b) SELECT (a.d*1000+b.d*100+c.d*10+e.d)*2 - 9999, IF(a.d = 3, NULL, CONCAT('s', b.d, c.d)), (c.d + 0.1) / 3, e.d / 7,
		CONCAT('u', a.d*1000+b.d*100+c.d*10+e.d), UNHEX('00ff') FROM digits a, digits b, digits c, digits e`,
	"CREATE TABLE ub (id BIGINT UNSIGNED PRIMARY KEY, v VARCHAR(10))",
	"INSERT INTO ub VALUES (0, 'a'), (1023, 'b'), (1024, 'c'), (9223372036854775807, 'd'), (9223372036854775808, 'e'), (18446744073709551615, 'f'), (18446744073709550000, 'g')",
	"CREATE TABLE sb (id BIGINT PRIMARY KEY, v VARCHAR(10), vv VARCHAR(10) AS (UPPER(v)) VIRTUAL)",
	"INSERT INTO sb (id, v) VALUES (-9223372036854775808, 'a'), (-1025, 'b'), (-1, 'c'), (0, 'd'), (9223372036854775807, 'e')",
	"CREATE TABLE cp (a INT, b BIGINT, v VARCHAR(10), PRIMARY KEY (a, b))",
	"INSERT INTO cp SELECT a.d, b.d*1000000000000 + c.d, CONCAT('v', c.d) FROM digits a, digits b, digits c",
	"CREATE TABLE ch (id INT PRIMARY KEY, nid INT, FOREIGN KEY (nid) REFERENCES n (id) ON DELETE CASCADE ON UPDATE CASCADE)",
	"INSERT INTO ch SELECT id, id FROM n WHERE ABS(id) % 100 = 1", // 200 rows
}

// TestMySQLRowDiff: a restore by chunks leaves every table exactly as a
// copy of every row does (fingerprint, next AUTO_INCREMENT value), and a
// chunk whose hash misses a change makes it copy the table whole.
func TestMySQLRowDiff(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLRowDiff(t, dsn) })
	}
}

func testMySQLRowDiff(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_rowdiff", "dgt_rowdiff_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, rowDiffSchema...)
	var log bytes.Buffer
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"digits"}, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0 := mustFingerprint(t, m)
	// MariaDB 10.3 and 10.4 copy n, with its VIRTUAL columns, whole: the
	// chunks are not counted there, the results are still checked.
	chunked := !m.purgeCrash()

	defer func(min int64) { rowDiffMin, rowDiffMiss = min, nil }(rowDiffMin)
	var mu sync.Mutex
	var seen map[string]int // chunks the hashes found different, by table
	watch := func(miss int64) func(int64) bool {
		return func(k int64) bool {
			mu.Lock()
			defer mu.Unlock()
			seen[fmt.Sprint(k)]++
			return k == miss
		}
	}
	cases := []struct {
		name   string
		writes []string
		chunks int // at least this many chunks found different with chunks on
	}{
		{"insert", []string{"INSERT INTO n (s, u) VALUES ('new', 'unew')", "INSERT INTO ub VALUES (5000, 'x')", "INSERT INTO sb (id, v) VALUES (-9223372036854775807, 'x')"}, 3},
		{"delete", []string{"DELETE FROM n WHERE id IN (-4999, -1, 1, 3999)", "DELETE FROM ub WHERE id = 18446744073709551615", "DELETE FROM sb WHERE id IN (-1025, 9223372036854775807)"}, 5},
		{"update", []string{"UPDATE n SET s = CONCAT(s, 'x') WHERE id IN (-2999, 2501)", "UPDATE n SET f = f + 0.0001, d = d + 1e-12 WHERE id = 7", "UPDATE ub SET v = 'z' WHERE id = 9223372036854775808"}, 3},
		{"NULL and ''", []string{"UPDATE n SET s = '' WHERE id = -1999", "UPDATE n SET s = NULL WHERE id = 101"}, 2},
		{"unique value moved", []string{"UPDATE n SET u = 'tmp' WHERE id = 11", "UPDATE n SET u = 'u5005' WHERE id = 3011", "UPDATE n SET u = 'u6505' WHERE id = 11"}, 2},
		{"AUTO_INCREMENT", []string{"INSERT INTO n (id, s, u) VALUES (90000, 'hi', 'uhi')", "DELETE FROM n WHERE id = 90000"}, 0},
		{"composite key", []string{"UPDATE cp SET v = 'w' WHERE a = 1 AND b = 2000000000003", "DELETE FROM cp WHERE a = 9"}, 0},
		{"key moved", []string{"UPDATE n SET id = 10001 WHERE id = -9999"}, 2},
	}
	for _, tc := range cases {
		var got []Fingerprint
		for _, mode := range []string{"chunks", "whole"} {
			seen = map[string]int{}
			rowDiffMin, rowDiffMiss = 0, watch(-1<<40)
			if mode == "whole" {
				rowDiffMin = 1 << 60
			}
			mustExec(t, db, tc.writes...)
			changed := Diff(fp0, mustFingerprint(t, m))
			if len(changed) == 0 {
				t.Fatalf("%s: no change", tc.name)
			}
			if err := m.Restore(ctx, changed); err != nil {
				t.Fatalf("%s, %s: %v", tc.name, mode, err)
			}
			f := mustFingerprint(t, m)
			if d := Diff(fp0, f); len(d) > 0 {
				t.Errorf("%s, %s: after the restore still different: %v", tc.name, mode, d)
			}
			if mode == "chunks" && chunked && len(seen) < tc.chunks {
				t.Errorf("%s: %d chunks restored, want at least %d", tc.name, len(seen), tc.chunks)
			}
			if strings.Contains(log.String(), "copying it whole") {
				t.Errorf("%s, %s: %s", tc.name, mode, log.String())
			}
			log.Reset()
			got = append(got, f)
		}
		if !reflect.DeepEqual(got[0], got[1]) {
			t.Errorf("%s: by chunks %v, whole %v", tc.name, got[0], got[1])
		}
	}
	var children int
	db.QueryRow("SELECT COUNT(*) FROM ch").Scan(&children)
	if children != 200 {
		t.Errorf("the child table has %d rows, want 200: a cascade reached it", children)
	}
	// The counter came back too (the fingerprint has it as well).
	mustExec(t, db, "INSERT INTO n (s, u) VALUES ('next', 'unext')")
	var id int
	if err := db.QueryRow("SELECT MAX(id) FROM n").Scan(&id); err != nil || id != 10000 {
		t.Errorf("next id %d (%v), want 10000", id, err)
	}
	mustExec(t, db, "DELETE FROM n WHERE id = 10000")
	if !chunked {
		return
	}

	// A change in a chunk whose hash agrees: the check after the copy finds
	// it and the table is copied whole.
	log.Reset()
	seen = map[string]int{}
	rowDiffMin, rowDiffMiss = 0, watch(2)
	mustExec(t, db, "UPDATE n SET s = 'miss' WHERE id = 2101", "UPDATE n SET s = 'hit' WHERE id = 4101")
	if err := m.Restore(ctx, []string{"table:n"}); err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
		t.Errorf("after a missed chunk still different: %v", d)
	}
	if !reflect.DeepEqual(seen, map[string]int{"2": 1, "4": 1}) {
		t.Errorf("chunks found different %v", seen)
	}
	if !strings.Contains(log.String(), "missed a change; copying it whole") {
		t.Errorf("no fallback to a whole copy: %q", log.String())
	}
}

// TestMySQLRowDiffPercent: a table and a column with a % in the name are
// restored by chunks too, not copied whole.
func TestMySQLRowDiffPercent(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLRowDiffPercent(t, dsn) })
	}
}

func testMySQLRowDiffPercent(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_rowdiff_pct", "dgt_rowdiff_pct_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, "CREATE TABLE digits (d INT)", "INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)",
		"CREATE TABLE `rate%s` (id INT PRIMARY KEY, `share%d` VARCHAR(20))",
		"INSERT INTO `rate%s` SELECT a.d*1000+b.d*100+c.d*10+e.d, CONCAT('v', c.d) FROM digits a, digits b, digits c, digits e")
	var log bytes.Buffer
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"digits"}, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0 := mustFingerprint(t, m)

	defer func(min int64) { rowDiffMin, rowDiffMiss = min, nil }(rowDiffMin)
	var mu sync.Mutex
	chunks := 0
	rowDiffMin, rowDiffMiss = 0, func(int64) bool {
		mu.Lock()
		defer mu.Unlock()
		chunks++
		return false
	}
	mustExec(t, db, "UPDATE `rate%s` SET `share%d` = 'x' WHERE id IN (5, 4100)")
	changed := Diff(fp0, mustFingerprint(t, m))
	if len(changed) == 0 {
		t.Fatal("no change")
	}
	if err := m.Restore(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
		t.Errorf("after the restore still different: %v", d)
	}
	if chunks != 2 {
		t.Errorf("%d chunks found different, want 2", chunks)
	}
	if strings.Contains(log.String(), "copying it whole") {
		t.Errorf("not restored by chunks: %s", log.String())
	}
}

// TestFKGroups: tables a foreign key joins, in either database, are copied
// on one connection; the others are spread.
func TestFKGroups(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testFKGroups(t, dsn) })
	}
}

func testFKGroups(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_fkg", "dgt_fkg_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, "CREATE TABLE digits (d INT)", "INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)")
	mustExec(t, db,
		"CREATE TABLE p (id INT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE c (id INT PRIMARY KEY, pid INT, v VARCHAR(20), FOREIGN KEY (pid) REFERENCES p (id) ON DELETE CASCADE)",
		"CREATE TABLE gc (id INT PRIMARY KEY, cid INT, FOREIGN KEY (cid) REFERENCES c (id))",
		"CREATE TABLE x (id INT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE y (id INT PRIMARY KEY, xid INT)", // the foreign key comes later, in the working copy only
		"CREATE TABLE z1 (id INT PRIMARY KEY, v VARCHAR(20))",
		"CREATE TABLE z2 (id INT PRIMARY KEY, v VARCHAR(20))",
	)
	for _, tb := range []string{"p", "x", "z1", "z2"} {
		mustExec(t, db, "INSERT INTO "+tb+" SELECT a.d*1000+b.d*100+c.d*10+e.d, 'v' FROM digits a, digits b, digits c, digits e")
	}
	mustExec(t, db, "INSERT INTO c SELECT id, id, v FROM p", "INSERT INTO gc SELECT id, id FROM c WHERE id < 100", "INSERT INTO y SELECT id, id FROM x WHERE id < 100")
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"digits"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0 := mustFingerprint(t, m)
	mustExec(t, db, "ALTER TABLE y ADD FOREIGN KEY (xid) REFERENCES x (id)")

	c, err := m.session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var jobs []tableJob
	for _, tb := range []string{"z1", "gc", "x", "p", "z2", "y", "c"} {
		jobs = append(jobs, tableJob{t: tb})
	}
	var groups []string
	for _, g := range fkGroups(ctx, c, []string{pristine, work}, jobs) {
		var names []string
		for _, j := range g {
			names = append(names, j.t)
		}
		sort.Strings(names)
		groups = append(groups, strings.Join(names, " "))
	}
	sort.Strings(groups)
	if want := []string{"c gc p", "x y", "z1", "z2"}; !reflect.DeepEqual(groups, want) {
		t.Fatalf("groups %q, want %q", groups, want)
	}

	// Parents and children all changed, restored over several connections,
	// a parent by chunks while its child is copied whole.
	defer func(min int64) { rowDiffMin = min }(rowDiffMin)
	rowDiffMin = 0
	for round := 0; round < 3; round++ {
		mustExec(t, db, "UPDATE p SET v = 'w' WHERE id = 5", "DELETE FROM c WHERE id = 7000", "UPDATE x SET v = 'w'", "INSERT INTO y VALUES (5000, 1)",
			"UPDATE z1 SET v = 'w' WHERE id = 1", "DELETE FROM z2 WHERE id > 9000", "DELETE FROM gc WHERE id = 3")
		changed := Diff(fp0, mustFingerprint(t, m))
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatal(err)
		}
		if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
			t.Fatalf("after the restore still different: %v", d)
		}
	}
}

// TestMySQLRestoreOrder: a query without ORDER BY returns its rows in the
// same order after a restore as on the tables loaded from a dump. The shape
// is that of roles and permissions in Laravel (Spatie laravel-permission): the pivot is
// read joined to the permissions with no ORDER BY. A copy into a table that
// TRUNCATE or CREATE TABLE emptied left its statistics at zero rows until
// the server recomputed them in the background; a query right after the
// restore then drove the join from the pivot, and the rows came in another
// order (MariaDB 10.3 and MySQL 8.4).
func TestMySQLRestoreOrder(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLRestoreOrder(t, dsn) })
	}
}

func testMySQLRestoreOrder(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_order", "dgt_order_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	// As a dump loads them: each table created with its keys, then its rows
	// in primary-key order.
	var perms, pivot []string
	for i := 1; i <= 150; i++ {
		perms = append(perms, fmt.Sprintf("(%d, 'p%d', NULL)", i, i))
	}
	for i := 1; i <= 300; i++ {
		pivot = append(pivot, fmt.Sprintf("(%d, %d, %d)", i, 2+(i/27)%11, 1+(i*37)%150))
	}
	mustExec(t, db,
		"CREATE TABLE permissions (id INT UNSIGNED NOT NULL AUTO_INCREMENT, name VARCHAR(255) NOT NULL, created_by INT UNSIGNED DEFAULT NULL, PRIMARY KEY (id), KEY (created_by))",
		`CREATE TABLE role_has_permissions (id INT UNSIGNED NOT NULL AUTO_INCREMENT, role_id INT UNSIGNED NOT NULL, permission_id INT UNSIGNED NOT NULL,
			PRIMARY KEY (id), KEY (role_id), KEY (permission_id), FOREIGN KEY (permission_id) REFERENCES permissions (id) ON DELETE CASCADE)`,
		"INSERT INTO permissions VALUES "+strings.Join(perms, ", "),
		"INSERT INTO role_has_permissions VALUES "+strings.Join(pivot, ", "),
		// The statistics a loaded database has by the time it is used (the
		// server computes them in the background, seconds after the load).
		"ANALYZE TABLE permissions, role_has_permissions",
	)
	const q = `SELECT permissions.id, role_has_permissions.role_id FROM permissions
		INNER JOIN role_has_permissions ON permissions.id = role_has_permissions.permission_id
		WHERE role_has_permissions.role_id IN (2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)`
	order := func(db *sql.DB) string {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var p, r int
			if err := rows.Scan(&p, &r); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%d:%d ", r, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	loaded := order(db)

	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := order(mysqlAdmin(t, dsn, pristine)); got != loaded {
		t.Errorf("pristine copy:\n%s\nloaded:\n%s", got, loaded)
	}
	fp0 := mustFingerprint(t, m)
	defer func(v int64) { rowDiffMin = v }(rowDiffMin)
	for _, mode := range []string{"whole", "chunks"} {
		if mode == "chunks" {
			rowDiffMin = 0
		}
		// A request that adds a pivot row and takes it away again.
		mustExec(t, db, "INSERT INTO role_has_permissions (role_id, permission_id) VALUES (2, 31)", "DELETE FROM role_has_permissions WHERE id = LAST_INSERT_ID()")
		changed := Diff(fp0, mustFingerprint(t, m))
		if len(changed) == 0 {
			t.Fatalf("%s: no change", mode)
		}
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got := order(db); got != loaded {
			t.Errorf("after a restore (%s):\n%s\nloaded:\n%s", mode, got, loaded)
		}
	}
}

// TestPurgeCrash: MariaDB before 10.5 copies tables with VIRTUAL columns
// whole (the chunked restore's ALTER TABLE crashed 10.3 and 10.4); newer
// ones and MySQL keep the chunked restore.
func TestPurgeCrash(t *testing.T) {
	for version, want := range map[string]bool{
		"10.3.39-MariaDB-1:10.3.39+maria~ubu2004": true,
		"10.2.44-MariaDB":                         true,
		"10.4.34-MariaDB":                         true,
		"10.5.27-MariaDB":                         false,
		"10.11.9-MariaDB":                         false,
		"11.4.5-MariaDB-ubu2404":                  false,
		"8.0.40":                                  false,
	} {
		m := &MySQL{server: dbx.FlavorMySQL, version: version}
		if dbx.IsMariaDB(version) {
			m.server = dbx.FlavorMariaDB
		}
		if got := m.purgeCrash(); got != want {
			t.Errorf("%s: %v, want %v", version, got, want)
		}
	}
}

// TestMySQLImplicitCollation: a column that takes its table's collation
// without naming it (a Laravel 5.5 table, utf8 and utf8_unicode_ci, and a
// column a migration added) keeps its definition through a seed and a
// restore. MySQL 8 prints it as COLLATE utf8mb3_unicode_ci, and that text
// run as it is made the copy's column print as CHARACTER SET utf8mb3
// COLLATE utf8mb3_unicode_ci: the restore left the table different from
// the pristine record. A real change of a column's collation still shows.
func TestMySQLImplicitCollation(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLImplicitCollation(t, dsn) })
	}
}

func testMySQLImplicitCollation(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_icoll", "dgt_icoll_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work+" CHARACTER SET utf8 COLLATE utf8_unicode_ci")
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db,
		"CREATE TABLE user_has_roles (id INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, user_id INT UNSIGNED NOT NULL) DEFAULT CHARACTER SET utf8 COLLATE 'utf8_unicode_ci'",
		"ALTER TABLE user_has_roles ADD role VARCHAR(255) NULL",
		// Named, so explicit: it stays so.
		"ALTER TABLE user_has_roles ADD scope VARCHAR(255) CHARACTER SET utf8 COLLATE utf8_unicode_ci NULL",
		"ALTER TABLE user_has_roles ADD kind ENUM('a COLLATE utf8mb3_unicode_ci', 'b''c') NULL",
		"INSERT INTO user_has_roles (user_id, role, scope, kind) VALUES (1, 'admin', 'all', 'b''c'), (2, 'editor', 'news', NULL)",
	)
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defs := func(db string) string {
		t.Helper()
		c, err := root.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		def, err := showCreate(ctx, c, "TABLE", db, "user_has_roles")
		if err != nil {
			t.Fatal(err)
		}
		return def
	}
	before := defs(work)
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := defs(pristine); got != before {
		t.Fatalf("pristine copy:\n%s\nworking:\n%s", got, before)
	}
	fp0 := samePristine(t, m) // as the seed records it
	for _, change := range []string{
		"UPDATE user_has_roles SET role = 'owner' WHERE id = 1",
		"ALTER TABLE user_has_roles MODIFY role VARCHAR(255) COLLATE utf8_general_ci NULL",
		"ALTER TABLE user_has_roles MODIFY role VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci NULL",
		"ALTER TABLE user_has_roles MODIFY scope VARCHAR(255) COLLATE utf8_bin NULL",
	} {
		mustExec(t, db, change)
		changed := Diff(fp0, mustFingerprint(t, m))
		if !reflect.DeepEqual(changed, []string{"table:user_has_roles"}) {
			t.Fatalf("%s: changed %v", change, changed)
		}
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatalf("%s: %v", change, err)
		}
		if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
			t.Fatalf("%s: after the restore %v:\n%s\nwas:\n%s", change, d, defs(work), before)
		}
	}
}

// TestMySQLKeepsConnections: a fingerprint and a restore that copies
// several tables at once close none of the connections they used, and a
// chunked restore that declines (most chunks differ) starts no hash of the
// pristine table, whose cancelled query would close its connection.
func TestMySQLKeepsConnections(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) { testMySQLKeepsConnections(t, dsn) })
	}
}

func testMySQLKeepsConnections(t *testing.T, dsn string) {
	ctx := context.Background()
	work, pristine := "dgt_conns", "dgt_conns_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	var rows []string
	for i := range 5000 {
		rows = append(rows, fmt.Sprintf("(%d, 'v%d')", i, i))
	}
	for _, tb := range []string{"a", "b", "c", "d", "e"} {
		mustExec(t, db, "CREATE TABLE "+tb+" (id INT PRIMARY KEY, v VARCHAR(20))", "INSERT INTO "+tb+" VALUES "+strings.Join(rows, ", "))
	}
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0 := mustFingerprint(t, m)
	defer func(v int64) { rowDiffMin = v }(rowDiffMin)
	rowDiffMin = 0
	var mu sync.Mutex
	var hashed []string
	rowDiffHash = func(table string) { mu.Lock(); hashed = append(hashed, table); mu.Unlock() }
	defer func() { rowDiffHash = nil }()
	cycle := func(writes ...string) {
		t.Helper()
		mustExec(t, db, writes...)
		changed := Diff(fp0, mustFingerprint(t, m))
		if err := m.Restore(ctx, changed); err != nil {
			t.Fatal(err)
		}
		if d := Diff(fp0, mustFingerprint(t, m)); len(d) > 0 {
			t.Fatalf("after the restore %v", d)
		}
	}
	every := []string{"UPDATE a SET v = 'x'", "UPDATE b SET v = 'x'", "UPDATE c SET v = 'x'", "UPDATE d SET v = 'x'", "UPDATE e SET v = 'x' WHERE id = 7"}
	cycle(every...) // opens the connections
	closed := m.db.Stats().MaxIdleClosed
	hashed = nil
	cycle(every...)
	if n := m.db.Stats().MaxIdleClosed - closed; n > 0 {
		t.Errorf("%d connections closed", n)
	}
	// a to d differ in every chunk: declined; e in one: copied by chunks.
	if !reflect.DeepEqual(hashed, []string{"e"}) {
		t.Errorf("pristine tables hashed: %v, want [e]", hashed)
	}
}
