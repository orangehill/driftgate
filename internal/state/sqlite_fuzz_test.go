package state

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// sqliteFuzzSchema: tables of every kind the fuzzed application writes:
// AUTOINCREMENT, WITHOUT ROWID, hidden rowids, a STORED generated column,
// a table a trigger writes, an excluded table with a trigger of its own,
// a view and a full-text index.
var sqliteFuzzSchema = []string{
	`CREATE TABLE t1 (id INTEGER PRIMARY KEY AUTOINCREMENT, v)`,
	`CREATE TABLE nr (k TEXT PRIMARY KEY, n INT) WITHOUT ROWID`,
	`CREATE TABLE hr (v TEXT)`,
	`CREATE TABLE t3 (x)`,
	`CREATE TABLE g (a INT, b INT GENERATED ALWAYS AS (a * 2) STORED)`,
	`CREATE TABLE sessions (id INTEGER PRIMARY KEY, p TEXT)`,
	`CREATE VIEW v1 AS SELECT id, v FROM t1 WHERE id % 2 = 0`,
	`CREATE TRIGGER tr1 AFTER INSERT ON t1 BEGIN INSERT INTO t3 VALUES (NEW.id); END`,
	`CREATE TRIGGER tr_s AFTER INSERT ON sessions BEGIN INSERT INTO t3 VALUES ('session'); END`,
	`CREATE VIRTUAL TABLE docs USING fts5 (body)`,
	`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 60)
		INSERT INTO t1 (v) SELECT CASE i % 5 WHEN 0 THEN i WHEN 1 THEN i * 1.5 WHEN 2 THEN 'text ' || i WHEN 3 THEN randomblob(8) ELSE NULL END FROM r`,
	`WITH RECURSIVE r(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM r WHERE i < 20) INSERT INTO nr SELECT 'k' || i, i FROM r`,
	`INSERT INTO hr (rowid, v) VALUES (2, 'a'), (5, 'b'), (9, 'c'), (40, 'd')`,
	`INSERT INTO g (a) VALUES (1), (2), (3)`,
	`INSERT INTO sessions VALUES (1, 'x')`,
	`INSERT INTO docs VALUES ('alpha beta'), ('gamma delta'), ('epsilon')`,
	`DELETE FROM t3`,
}

// sqliteFuzzOp is one change of the application's; arg varies it. Some
// fail (a column that exists already): an application's failed statement
// changes nothing either.
func sqliteFuzzOp(db *sql.DB, op, arg byte) {
	a := int(arg)
	val := []string{fmt.Sprint(a), fmt.Sprintf("%d.25", a), fmt.Sprintf("'t%d'", a), fmt.Sprintf("x'%02x'", a), "NULL", "''"}[a%6]
	var stmts []string
	switch op % 28 {
	case 0:
		stmts = []string{"INSERT INTO t1 (v) VALUES (" + val + ")"}
	case 1:
		stmts = []string{fmt.Sprintf("UPDATE t1 SET v = %s WHERE id %% 7 = %d", val, a%7)}
	case 2:
		stmts = []string{fmt.Sprintf("DELETE FROM t1 WHERE id %% 5 = %d", a%5)}
	case 3:
		stmts = []string{fmt.Sprintf("INSERT INTO nr VALUES ('k%d', %d) ON CONFLICT (k) DO UPDATE SET n = n + 1", a%30, a)}
	case 4:
		stmts = []string{fmt.Sprintf("DELETE FROM nr WHERE n %% 3 = %d", a%3)}
	case 5:
		stmts = []string{fmt.Sprintf("INSERT OR REPLACE INTO hr (rowid, v) VALUES (%d, %s)", a*3+1, val)}
	case 6:
		stmts = []string{fmt.Sprintf("DELETE FROM hr WHERE rowid %% 4 = %d", a%4)}
	case 7:
		if a%2 == 0 {
			stmts = []string{"CREATE INDEX IF NOT EXISTS t1_v ON t1 (v)"}
		} else {
			stmts = []string{"DROP INDEX IF EXISTS t1_v"}
		}
	case 8:
		stmts = []string{fmt.Sprintf("ALTER TABLE t1 ADD COLUMN c%d DEFAULT %s", a%3, val)}
	case 9:
		switch a % 3 {
		case 0:
			stmts = []string{"DROP TABLE IF EXISTS t2"}
		case 1:
			stmts = []string{"CREATE TABLE IF NOT EXISTS t2 (id INTEGER PRIMARY KEY, w)", fmt.Sprintf("INSERT INTO t2 (w) VALUES (%s)", val)}
		default:
			stmts = []string{"DROP TABLE IF EXISTS g", "CREATE TABLE g (a INT, b INT GENERATED ALWAYS AS (a * 3) STORED)", "INSERT INTO g (a) VALUES (1)"}
		}
	case 10:
		if a%2 == 0 {
			stmts = []string{"DROP VIEW IF EXISTS v1"}
		} else {
			stmts = []string{"DROP VIEW IF EXISTS v1", fmt.Sprintf("CREATE VIEW v1 AS SELECT id FROM t1 WHERE id > %d", a)}
		}
	case 11:
		if a%2 == 0 {
			stmts = []string{"DROP TRIGGER IF EXISTS tr1"}
		} else {
			stmts = []string{"DROP TRIGGER IF EXISTS tr2", "CREATE TRIGGER tr2 AFTER DELETE ON nr BEGIN INSERT INTO t3 VALUES (OLD.k); END"}
		}
	case 12:
		stmts = []string{fmt.Sprintf("PRAGMA user_version = %d", a)}
	case 13:
		stmts = []string{fmt.Sprintf("INSERT INTO t1 (v) VALUES (randomblob(%d))", a*100)}
	case 14:
		if a%4 == 0 {
			stmts = []string{"VACUUM"}
		}
	case 15:
		stmts = []string{fmt.Sprintf("INSERT INTO docs VALUES ('word%d lorem')", a)}
	case 16:
		stmts = []string{"UPDATE t1 SET v = v"}
	case 17:
		stmts = []string{fmt.Sprintf("UPDATE sqlite_sequence SET seq = seq + %d WHERE name = 't1'", a)}
	case 18:
		stmts = []string{"ANALYZE"}
	case 19:
		stmts = []string{fmt.Sprintf("INSERT OR REPLACE INTO sessions VALUES (%d, 'p')", a%5)}
	case 20:
		stmts = []string{fmt.Sprintf("DELETE FROM docs WHERE rowid %% 2 = %d", a%2)}
	case 21:
		stmts = []string{"BEGIN", "INSERT INTO t1 (v) VALUES (1)", "ROLLBACK"}
	case 22:
		if a%2 == 0 {
			stmts = []string{"ALTER TABLE hr RENAME TO hr2"}
		} else {
			stmts = []string{"ALTER TABLE hr2 RENAME TO hr"}
		}
	case 23:
		stmts = []string{fmt.Sprintf("PRAGMA application_id = %d", a)}
	case 24:
		// auto_vacuum changes with the next VACUUM: root pages move when
		// a table is dropped, free pages leave the file.
		stmts = []string{"PRAGMA auto_vacuum = " + []string{"NONE", "FULL", "INCREMENTAL"}[a%3], "VACUUM"}
	case 25:
		stmts = []string{fmt.Sprintf("PRAGMA incremental_vacuum(%d)", a%8)}
	case 26:
		// A new page size (not in WAL mode, where the VACUUM keeps it).
		stmts = []string{fmt.Sprintf("PRAGMA page_size = %d", 512<<(a%8)), "VACUUM"}
	case 27:
		stmts = []string{fmt.Sprintf("DELETE FROM t1 WHERE id > %d", a), fmt.Sprintf("INSERT INTO nr SELECT 'big' || id, id FROM t1 WHERE id %% 3 = %d", a%3)}
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil && q != "ROLLBACK" {
			db.Exec("ROLLBACK")
			return
		}
	}
}

// FuzzSQLiteRestore: whatever the application does to the working file, a
// fingerprint that kept row hashes by page digest equals one read from
// scratch, and a restore of the items that differ from the pristine file
// leaves a file equal to it: the same fingerprint, the same rows both ways
// (EXCEPT) and the same schema, an intact file, and the excluded table
// and the triggers' table not written by the restore.
func FuzzSQLiteRestore(f *testing.F) {
	tmpl := f.TempDir()
	work, pristine := filepath.Join(tmpl, "app.sqlite"), filepath.Join(tmpl, "pristine.sqlite")
	app := sqliteApp(f, work)
	mustExecF(f, app, sqliteFuzzSchema...)
	app.Close()
	exclude := []string{"sessions"}
	s, err := NewSQLite(Options{Name: "db", Working: work, Pristine: pristine, Exclude: exclude})
	if err != nil {
		f.Fatal(err)
	}
	if err := s.Seed(context.Background()); err != nil {
		f.Fatal(err)
	}
	s.Close()
	for _, seed := range [][]byte{
		{},
		{0, 1, 1, 2, 2, 3},
		{1, 0, 7, 0, 8, 1, 9, 2, 14, 0},
		{0, 5, 13, 40, 14, 0, 2, 1, 6, 1},
		{11, 0, 10, 1, 12, 9, 15, 3, 20, 1, 22, 0},
		{0, 9, 17, 4, 18, 0, 19, 1, 21, 0, 3, 7, 4, 1},
		{1, 9, 2, 8, 0, 2, 7, 0, 8, 2, 9, 0, 9, 1, 5, 12, 6, 2, 23, 5},
		{0, 24, 1, 13, 30, 9, 0, 27, 4, 25, 3, 13, 9, 26, 2, 24, 2, 9, 3, 25, 0},
		{1, 24, 2, 13, 50, 27, 10, 9, 0, 25, 7, 22, 0, 26, 1, 14, 0},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		ctx := context.Background()
		dir := t.TempDir()
		w, p := filepath.Join(dir, "app.sqlite"), filepath.Join(dir, "pristine.sqlite")
		copyFile(t, work, w)
		copyFile(t, pristine, p)
		app := sqliteApp(t, w)
		if len(ops) > 0 && ops[0]%2 == 1 {
			mustExec(t, app, "PRAGMA journal_mode = WAL", "PRAGMA wal_autocheckpoint = 0")
		}
		s, err := NewSQLite(Options{Name: "db", Working: w, Pristine: p, Exclude: exclude})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		want := sqlitePristineFP(t, s)
		sqliteFingerprint(t, s)
		for i := 1; i+1 < len(ops) && i < 80; i += 2 {
			sqliteFuzzOp(app, ops[i], ops[i+1])
			if ops[i+1]%3 == 0 {
				sqliteFingerprint(t, s) // keep row hashes from more moments
			}
		}
		got := sqliteFingerprint(t, s)
		fresh, err := NewSQLite(Options{Name: "db", Working: w, Pristine: p, Exclude: exclude})
		if err != nil {
			t.Fatal(err)
		}
		scratch := sqliteFingerprint(t, fresh)
		fresh.Close()
		if d := Diff(got, scratch); len(d) > 0 {
			t.Fatalf("a fingerprint by page digests differs from one read from scratch in %v", d)
		}
		t3 := count(t, app, "SELECT count(*) FROM t3")
		sessions := count(t, app, "SELECT count(*) FROM sessions")
		if err := s.Restore(ctx, Diff(got, want)); err != nil {
			t.Fatal(err)
		}
		if d := Diff(sqliteFingerprint(t, s), want); len(d) > 0 {
			t.Fatalf("the restore left %v different", d)
		}
		if n := count(t, app, "SELECT count(*) FROM sessions"); n != sessions {
			t.Fatalf("the restore changed the excluded table: %d rows, had %d", n, sessions)
		}
		if contains(Diff(got, want), "table:t3") {
			t3 = count(t, app, "SELECT count(*) FROM t3") // restored itself
		}
		if n := count(t, app, "SELECT count(*) FROM t3"); n != t3 {
			t.Fatalf("the restore fired triggers: t3 has %d rows, had %d", n, t3)
		}
		mustExec(t, app, "ATTACH DATABASE "+ql(p)+" AS p")
		schema := `SELECT type, name, tbl_name, sql FROM %s.sqlite_schema WHERE name NOT LIKE 'sqlite\_%%' ESCAPE '\' AND name <> 'driftgate_pristine' AND tbl_name <> 'sessions'`
		if n := count(t, app, "SELECT count(*) FROM ("+fmt.Sprintf(schema, "main")+" EXCEPT "+fmt.Sprintf(schema, "p")+")") +
			count(t, app, "SELECT count(*) FROM ("+fmt.Sprintf(schema, "p")+" EXCEPT "+fmt.Sprintf(schema, "main")+")"); n != 0 {
			t.Fatalf("the schemas differ in %d objects", n)
		}
		rows, err := app.Query(`SELECT name, (SELECT count(*) FROM pragma_table_list WHERE schema = 'p' AND name = s.name AND type = 'table' AND wr = 0) FROM p.sqlite_schema s WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT IN ('driftgate_pristine', 'sessions', 'docs')`)
		if err != nil {
			t.Fatal(err)
		}
		type table struct {
			name  string
			rowid bool
		}
		var tables []table
		for rows.Next() {
			var tb table
			rows.Scan(&tb.name, &tb.rowid)
			tables = append(tables, tb)
		}
		rows.Close()
		for _, tb := range tables {
			cols := "*"
			if tb.rowid {
				cols = "rowid, *"
			}
			for _, pair := range [][2]string{{"main", "p"}, {"p", "main"}} {
				q := fmt.Sprintf(`SELECT count(*) FROM (SELECT %s FROM %s."%s" EXCEPT SELECT %s FROM %s."%s")`, cols, pair[0], tb.name, cols, pair[1], tb.name)
				if n := count(t, app, q); n != 0 {
					t.Fatalf("%s: %d rows of %s are not in %s", tb.name, n, pair[0], pair[1])
				}
			}
		}
		mustExec(t, app, "DETACH DATABASE p")
		var ok string
		if err := app.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
			t.Fatalf("integrity_check: %s %v", ok, err)
		}
	})
}

func mustExecF(f *testing.F, db *sql.DB, stmts ...string) {
	f.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			f.Fatalf("%s: %v", s, err)
		}
	}
}

func copyFile(t testing.TB, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
