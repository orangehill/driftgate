package state

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// checkRestoreOnly is what every driver's restore_only test checks once
// the application wrote to a tracked table and to the restore_only ones:
// the fingerprint names only the tracked table (a write to a restore_only
// table never hides it, and is no change of its own); the restore_only
// tables differ from the pristine copy's, which a seed records apart; a
// restore of both brings the working state back to the pristine
// fingerprint and the restore_only tables to the pristine rows.
func checkRestoreOnly(t *testing.T, c Component, tracked string, ro []string, write func()) {
	t.Helper()
	ctx := context.Background()
	r := c.(RestoreOnly)
	fp0, err := c.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pro, err := r.RestoreOnlyFingerprint(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	wro, err := r.RestoreOnlyFingerprint(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for it := range pro {
		names = append(names, it)
		if _, ok := fp0[it]; ok {
			t.Fatalf("restore_only %s fingerprinted: %v", it, fp0)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, ro) {
		t.Fatalf("restore_only items %v, want %v", names, ro)
	}
	if d := Diff(wro, pro); len(d) > 0 {
		t.Fatalf("the working restore_only tables differ from the pristine ones after the seed: %v", d)
	}
	if seeded, ok := c.(interface {
		SeededFingerprint(ctx context.Context) (Fingerprint, error)
	}); ok {
		f, err := seeded.SeededFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d := Diff(fp0, f); len(d) > 0 {
			t.Fatalf("the seeded fingerprint differs: %v", d)
		}
	}

	write()
	fp1, err := c.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, fp1); !slices.Equal(d, []string{tracked}) {
		t.Fatalf("changed %v, want only %s", d, tracked)
	}
	// Asked by name, a fingerprint still leaves them out.
	if f, err := c.Fingerprint(ctx, ro); err != nil || len(f) > 0 {
		t.Fatalf("fingerprint of the restore_only items by name: %v %v", f, err)
	}
	wro, err = r.RestoreOnlyFingerprint(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	changed := Diff(wro, pro)
	if len(changed) == 0 {
		t.Fatal("the writes to the restore_only tables are not seen")
	}
	if err := c.Restore(ctx, append([]string{tracked}, changed...)); err != nil {
		t.Fatal(err)
	}
	fp2, err := c.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp0, fp2); len(d) > 0 {
		t.Fatalf("after the restore %v differ", d)
	}
	if wro, err = r.RestoreOnlyFingerprint(ctx, false); err != nil {
		t.Fatal(err)
	}
	if d := Diff(wro, pro); len(d) > 0 {
		t.Fatalf("restore_only tables not restored: %v", d)
	}

	// Written alone, they change no item of the fingerprint.
	write()
	fp3, err := c.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(fp2, fp3); !slices.Equal(d, []string{tracked}) {
		t.Fatalf("changed %v after the second writes, want only %s", d, tracked)
	}
}

func roCount(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestMySQLRestoreOnly(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_ro_work", "dgt_ro_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			db := mysqlAdmin(t, dsn, work)
			mustExec(t, db,
				"CREATE TABLE news (id INT AUTO_INCREMENT PRIMARY KEY, title VARCHAR(50))",
				"CREATE TABLE sessions (id VARCHAR(40) PRIMARY KEY, payload TEXT)",
				"CREATE TABLE cache_entries (id INT AUTO_INCREMENT PRIMARY KEY, v VARCHAR(20))",
				"CREATE TABLE logs (id INT AUTO_INCREMENT PRIMARY KEY)",
				"INSERT INTO news (title) VALUES ('First')",
				"INSERT INTO sessions VALUES ('s1', 'x')",
				"INSERT INTO cache_entries (v) VALUES ('a'), ('b')",
				"INSERT INTO logs VALUES ()",
			)
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"logs"}, RestoreOnly: []string{"sessions", "cache_*"}})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			pdb := mysqlAdmin(t, dsn, pristine)
			if n := roCount(t, pdb, "SELECT COUNT(*) FROM sessions"); n != 1 {
				t.Fatalf("%d sessions in the pristine copy, want its 1", n)
			}
			checkRestoreOnly(t, m, "table:news", []string{"table:cache_entries", "table:sessions"}, func() {
				mustExec(t, db, "INSERT INTO news (title) VALUES ('Next')", "INSERT INTO sessions VALUES (UUID(), 'y')",
					"INSERT INTO cache_entries (v) VALUES ('c')", "DELETE FROM cache_entries WHERE v = 'a'", "INSERT INTO logs VALUES ()")
			})
			if n := roCount(t, db, "SELECT COUNT(*) FROM sessions"); n != 2 {
				t.Fatalf("%d sessions, want the pristine one and the last write", n)
			}
			// The excluded table keeps the application's rows.
			if n := roCount(t, db, "SELECT COUNT(*) FROM logs"); n != 3 {
				t.Fatalf("%d rows of the excluded table, want 3", n)
			}

			both, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"sess*"}, RestoreOnly: []string{"sessions"}})
			if err != nil {
				t.Fatal(err)
			}
			defer both.Close()
			if _, err := both.Fingerprint(ctx, nil); err == nil || !strings.Contains(err.Error(), "table sessions is both excluded and restore_only") {
				t.Fatalf("a table both excluded and restore_only: %v", err)
			}
		})
	}
}

// TestMySQLRestoreOnlyCounter: the AUTO_INCREMENT of a restore_only table
// comes back with its rows.
func TestMySQLRestoreOnlyCounter(t *testing.T) {
	dsn := mysqlServers(t)[0]
	ctx := context.Background()
	work, pristine := "dgt_roc_work", "dgt_roc_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, "CREATE TABLE jobs (id INT AUTO_INCREMENT PRIMARY KEY, payload TEXT)", "INSERT INTO jobs (payload) VALUES ('a')")
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreOnly: []string{"jobs"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO jobs (payload) VALUES ('b'), ('c')")
	if err := m.Restore(ctx, []string{"table:jobs"}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "INSERT INTO jobs (payload) VALUES ('d')")
	if n := roCount(t, db, "SELECT MAX(id) FROM jobs"); n != 2 {
		t.Fatalf("next id %d, want 2", n)
	}
}

func TestPostgresRestoreOnly(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	work, pristine := "dgt_ro_work", "dgt_ro_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"logs"}, RestoreOnly: []string{"public.sessions", "jobs"}})
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
	defer db.Close()
	mustExec(t, db,
		"CREATE TABLE news (id serial PRIMARY KEY, title text)",
		"CREATE TABLE sessions (id text PRIMARY KEY, payload text)",
		"CREATE TABLE jobs (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, payload text)",
		"CREATE TABLE logs (id serial PRIMARY KEY)",
		"INSERT INTO news (title) VALUES ('First')",
		"INSERT INTO sessions VALUES ('s1', 'x')",
		"INSERT INTO jobs (payload) VALUES ('a')",
	)
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	// The sequences of the restore_only tables are restore_only too; the
	// excluded table's stays an item, as before.
	db.Close()
	// A restore makes the working database anew: the application connects again.
	checkRestoreOnly(t, p, "table:public.news", []string{"sequence:public.jobs_id_seq", "table:public.jobs", "table:public.sessions"}, func() {
		db, _ := p.open(work)
		defer db.Close()
		mustExec(t, db, "UPDATE news SET title = title || '!'", "INSERT INTO sessions VALUES (md5(random()::text), 'y')",
			"INSERT INTO jobs (payload) VALUES ('b')")
	})
	db, _ = p.open(work)
	if n := roCount(t, db, "SELECT count(*) FROM sessions"); n != 2 {
		t.Fatalf("%d sessions, want the pristine one and the last write", n)
	}
	mustExec(t, db, "DELETE FROM jobs WHERE payload = 'b'", "INSERT INTO jobs (payload) VALUES ('c')")
	if n := roCount(t, db, "SELECT max(id) FROM jobs"); n != 3 {
		t.Fatalf("next job id %d, want 3 (the sequence moved by the last write only)", n)
	}
	db.Close()

	both, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, Exclude: []string{"public.sessions"}, RestoreOnly: []string{"sessions"}})
	if err != nil {
		t.Fatal(err)
	}
	defer both.Close()
	if _, err := both.Fingerprint(ctx, nil); err == nil || !strings.Contains(err.Error(), "table public.sessions is both excluded and restore_only") {
		t.Fatalf("a table both excluded and restore_only: %v", err)
	}
}

// TestSQLiteRestoreOnly: a restore_only table named in another letter
// case, and a virtual table with its shadow tables.
func TestSQLiteRestoreOnly(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			s, app := sqliteFixture(t, mode, Options{Exclude: []string{"audit"}, RestoreOnly: []string{"SESSIONS", "docs"}})
			p := sqliteApp(t, s.o.Pristine)
			if n := roCount(t, p, "SELECT count(*) FROM sessions"); n != 1 {
				t.Fatalf("%d sessions in the pristine copy, want its 1", n)
			}
			ro := []string{"table:docs", "table:docs_config", "table:docs_content", "table:docs_data", "table:docs_docsize", "table:docs_idx", "table:sessions"}
			checkRestoreOnly(t, s, "table:users", ro, func() {
				mustExec(t, app, "INSERT INTO users (email) VALUES ('c' || random() || '@x')", "INSERT INTO sessions VALUES (random(), 'y')",
					"INSERT INTO docs VALUES ('lazy dog')")
			})
			if n := roCount(t, app, "SELECT count(*) FROM docs WHERE docs MATCH 'quick'"); n != 1 {
				t.Fatalf("full-text matches after the restore: %d", n)
			}
			if n := roCount(t, app, "SELECT count(*) FROM docs WHERE docs MATCH 'lazy'"); n != 1 {
				t.Fatalf("full-text matches of the last write: %d", n)
			}
			// The trigger of a restore_only table is an item, as an
			// excluded table's: its body may write any table.
			if _, ok := sqliteFingerprint(t, s)["trigger:sessions_audit"]; !ok {
				t.Fatal("the trigger of the restore_only table is no item")
			}
		})
	}
}

// TestSQLiteRestoreOnlyShadow: a glob of restore_only that matches the
// shadow tables of a virtual table that is fingerprinted leaves them in
// the fingerprint: their rows are the virtual table's.
func TestSQLiteRestoreOnlyShadow(t *testing.T) {
	s, app := sqliteFixture(t, "wal", Options{RestoreOnly: []string{"docs_*"}})
	fp0 := sqliteFingerprint(t, s)
	if _, ok := fp0["table:docs_data"]; !ok {
		t.Fatalf("a shadow table of a fingerprinted virtual table left out: %v", fp0)
	}
	mustExec(t, app, "INSERT INTO docs VALUES ('lazy dog')")
	if d := Diff(fp0, sqliteFingerprint(t, s)); !slices.Contains(d, "table:docs_data") {
		t.Fatalf("a write to the virtual table changed %v", d)
	}
	both := Options{Name: "db", Working: s.o.Working, Pristine: s.o.Pristine, Exclude: []string{"Sessions"}, RestoreOnly: []string{"sessions"}}
	b, err := NewSQLite(both)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Fingerprint(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "table sessions is both excluded and restore_only") {
		t.Fatalf("a table both excluded and restore_only: %v", err)
	}
}

// TestSQLServerRestoreOnly: restore_only tables keep their rows in the
// pristine copy and get them back through a restore item by item and a
// restore of the whole database, which never saves them.
func TestSQLServerRestoreOnly(t *testing.T) {
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"logs"}, RestoreOnly: []string{"Sessions", "HangFire.*"}},
		"CREATE SCHEMA HangFire",
		"CREATE TABLE users (id int IDENTITY PRIMARY KEY, name nvarchar(20) NULL)",
		"INSERT INTO users (name) VALUES (N'a'), (N'b')",
		"CREATE TABLE sessions (id nvarchar(40) PRIMARY KEY, payload nvarchar(max) NULL)",
		"CREATE TABLE HangFire.Job (Id bigint IDENTITY PRIMARY KEY, Payload nvarchar(max) NULL)",
		"CREATE TABLE logs (id int IDENTITY PRIMARY KEY, note nvarchar(20) NULL)",
		"INSERT INTO sessions VALUES (N's1', N'x')",
		"INSERT INTO HangFire.Job (Payload) VALUES (N'a')",
	)
	pdb := sqlServerTestDB(t, s.o.DSN, s.o.Pristine)
	if n := sqlServerInt(t, pdb, "SELECT COUNT(*) FROM sessions"); n != 1 {
		t.Fatalf("%d sessions in the pristine copy, want its 1", n)
	}
	write := func() {
		sqlServerExec(t, db, "UPDATE users SET name = name + N'!'", "INSERT INTO sessions VALUES (CONVERT(nvarchar(40), NEWID()), N'y')",
			"INSERT INTO HangFire.Job (Payload) VALUES (N'b')", "INSERT INTO logs (note) VALUES (N'n')")
	}
	checkRestoreOnly(t, s, "table:dbo.users", []string{"table:HangFire.Job", "table:dbo.sessions"}, write)
	if log.fellBack() {
		t.Fatal("restored the whole database")
	}
	// Restored to the pristine row 1, then the last write added 2.
	if got := nextIdentity(t, db, "HangFire.Job"); got != 3 {
		t.Fatalf("next identity of the restore_only table %d, want 3", got)
	}
	// A restore of the whole database takes them from the pristine copy
	// too, and keeps the excluded table's rows.
	ctx := context.Background()
	pro, err := s.RestoreOnlyFingerprint(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	p := pristineSQLServer(t, s)
	sqlServerExec(t, db, "CREATE VIEW v AS SELECT id FROM users")
	if !restoreSQLServer(t, s, db, log, p) {
		t.Fatal("restored item by item")
	}
	wro, err := s.RestoreOnlyFingerprint(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(wro, pro); len(d) > 0 {
		t.Fatalf("restore_only tables after a restore of the whole database: %v", d)
	}
	if got := nextIdentity(t, db, "HangFire.Job"); got != 2 {
		t.Fatalf("next identity of the restore_only table after a restore of the whole database %d, want 2", got)
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM logs"); n != 2 {
		t.Fatalf("%d rows of the excluded table, want 2", n)
	}
}

// TestSQLiteFromPristine: the working file of a new checkout is a copy of
// the pristine file without its mark; a file there already, or one of
// the files SQLite keeps next to it, is never written over.
func TestSQLiteFromPristine(t *testing.T) {
	s, _ := sqliteFixture(t, "wal", Options{})
	ctx := context.Background()
	dir := t.TempDir()
	work := filepath.Join(dir, "db", "app.sqlite")
	if err := SQLiteFromPristine(ctx, s.o.Pristine, work); err != nil {
		t.Fatal(err)
	}
	db := sqliteApp(t, work)
	if n := roCount(t, db, "SELECT count(*) FROM sqlite_schema WHERE name = 'driftgate_pristine'"); n != 0 {
		t.Fatal("the copy has the pristine mark")
	}
	if n := roCount(t, db, "SELECT count(*) FROM users"); n != 2 {
		t.Fatalf("%d users in the copy", n)
	}
	if err := SQLiteFromPristine(ctx, s.o.Pristine, work); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("over an existing file: %v", err)
	}
	other := filepath.Join(dir, "other.sqlite")
	if err := os.WriteFile(other+"-wal", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SQLiteFromPristine(ctx, s.o.Pristine, other); err == nil || !strings.Contains(err.Error(), "-wal exists") {
		t.Fatalf("next to a -wal file: %v", err)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("a file was made next to a -wal file")
	}
	if err := SQLiteFromPristine(ctx, s.o.Working, filepath.Join(dir, "x.sqlite")); err == nil || !strings.Contains(err.Error(), "no pristine copy") {
		t.Fatalf("from a file that is no pristine copy: %v", err)
	}
	// A file system without hard links.
	defer func(f func(string, string) error) { linkFile = f }(linkFile)
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: errors.ErrUnsupported} }
	nolink := filepath.Join(dir, "nolink.sqlite")
	if err := SQLiteFromPristine(ctx, s.o.Pristine, nolink); err != nil {
		t.Fatalf("without hard links: %v", err)
	}
	if n := roCount(t, sqliteApp(t, nolink), "SELECT count(*) FROM users"); n != 2 {
		t.Fatalf("%d users in the copy made without a hard link", n)
	}
	if err := SQLiteFromPristine(ctx, s.o.Pristine, nolink); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("without hard links, over an existing file: %v", err)
	}
}

// checkRestoreEmpty: a restore_empty table has no rows in the pristine
// copy; written, it differs, and a restore of it alone empties it again
// and leaves it as the pristine copy has it. count reads its rows in the
// working database (connect: it may be made anew).
func checkRestoreEmpty(t *testing.T, c Component, item string, count func() int, write func()) {
	t.Helper()
	ctx := context.Background()
	r := c.(RestoreOnly)
	pro, err := r.RestoreOnlyFingerprint(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pro[item]; !ok {
		t.Fatalf("%s is no restore_only item: %v", item, pro)
	}
	if n := count(); n == 0 {
		t.Fatal("the working table has no rows to leave out")
	}
	fp0, err := c.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	wro, _ := r.RestoreOnlyFingerprint(ctx, false)
	d := Diff(wro, pro)
	if !slices.Contains(d, item) {
		t.Fatalf("the working table with rows equals the pristine copy's: %v", d)
	}
	if err := c.Restore(ctx, d); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d rows after the restore, want none", n)
	}
	wro, _ = r.RestoreOnlyFingerprint(ctx, false)
	if d := Diff(wro, pro); len(d) > 0 {
		t.Fatalf("after the restore %v differ", d)
	}
	write()
	if wro, _ = r.RestoreOnlyFingerprint(ctx, false); len(Diff(wro, pro)) == 0 {
		t.Fatal("a write to the emptied table is not seen")
	}
	if err := c.Restore(ctx, Diff(wro, pro)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d rows after the second restore", n)
	}
	if fp, err := c.Fingerprint(ctx, nil); err != nil || len(Diff(fp, fp0)) > 0 {
		t.Fatalf("the tracked items changed: %v %v", Diff(fp, fp0), err)
	}
}

func TestMySQLRestoreEmpty(t *testing.T) {
	dsn := mysqlServers(t)[0]
	ctx := context.Background()
	work, pristine := "dgt_re_work", "dgt_re_pristine"
	root := mysqlAdmin(t, dsn, "")
	mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work)
	t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
	db := mysqlAdmin(t, dsn, work)
	mustExec(t, db, "CREATE TABLE news (id INT PRIMARY KEY)", "INSERT INTO news VALUES (1)",
		"CREATE TABLE cache (k VARCHAR(20) PRIMARY KEY, v TEXT)", "INSERT INTO cache VALUES ('a', 'the developer''s own')")
	m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if n := roCount(t, mysqlAdmin(t, dsn, pristine), "SELECT COUNT(*) FROM cache"); n != 0 {
		t.Fatalf("%d cache rows in the pristine copy", n)
	}
	checkRestoreEmpty(t, m, "table:cache", func() int { return roCount(t, db, "SELECT COUNT(*) FROM cache") },
		func() { mustExec(t, db, "INSERT INTO cache VALUES (UUID(), 'x')") })
}

func TestPostgresRestoreEmpty(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	work, pristine := "dgt_re_work", "dgt_re_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	mustExec(t, p.admin, "DROP DATABASE IF EXISTS "+work+" WITH (FORCE)", "DROP DATABASE IF EXISTS "+pristine+" WITH (FORCE)", "CREATE DATABASE "+work)
	t.Cleanup(func() {
		p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
		p.admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
	})
	exec := func(stmts ...string) {
		db, err := p.open(work)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mustExec(t, db, stmts...)
	}
	count := func() int {
		db, err := p.open(work)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		return roCount(t, db, "SELECT count(*) FROM cache")
	}
	exec("CREATE TABLE news (id int PRIMARY KEY)", "INSERT INTO news VALUES (1)",
		"CREATE TABLE cache (id serial PRIMARY KEY, v text)", "INSERT INTO cache (v) VALUES ('the developer''s own'), ('b')")
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	var oid int64
	if err := p.admin.QueryRow("SELECT oid FROM pg_database WHERE datname = $1", work).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	checkRestoreEmpty(t, p, "table:public.cache", count, func() { exec("INSERT INTO cache (v) VALUES ('c')") })
	// Emptied in place: the working database was not copied anew.
	var now int64
	if err := p.admin.QueryRow("SELECT oid FROM pg_database WHERE datname = $1", work).Scan(&now); err != nil || now != oid {
		t.Fatalf("the working database was copied for a restore of restore_empty tables (%v)", err)
	}
	// Next to a restore_only table with a sequence, and after an UPDATE
	// and a DELETE of the cache, the restore empties in place too.
	exec("CREATE TABLE audit_log (id serial PRIMARY KEY, note text)", "INSERT INTO audit_log (note) VALUES ('a')")
	q, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreOnly: []string{"audit_log"}, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.admin.QueryRow("SELECT oid FROM pg_database WHERE datname = $1", work).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"INSERT INTO cache (v) VALUES ('e')", "UPDATE cache SET v = 'f'", "DELETE FROM cache"} {
		exec("INSERT INTO cache (v) VALUES ('x')", write)
		pro, _ := q.RestoreOnlyFingerprint(ctx, true)
		wro, _ := q.RestoreOnlyFingerprint(ctx, false)
		if err := q.Restore(ctx, Diff(wro, pro)); err != nil {
			t.Fatal(err)
		}
		if err := p.admin.QueryRow("SELECT oid FROM pg_database WHERE datname = $1", work).Scan(&now); err != nil || now != oid {
			t.Fatalf("after %s next to a restore_only table: the working database was copied (%v)", write, err)
		}
	}
	exec("INSERT INTO cache (v) VALUES ('d')")
	db, _ := p.open(work)
	defer db.Close()
	if n := roCount(t, db, "SELECT max(id) FROM cache"); n != 1 {
		t.Fatalf("the sequence of the emptied table gave %d, want 1", n)
	}
}

func TestSQLiteRestoreEmpty(t *testing.T) {
	s, app := sqliteFixture(t, "wal", Options{Exclude: []string{"audit"}, RestoreEmpty: []string{"sessions"}})
	p := sqliteApp(t, s.o.Pristine)
	if n := roCount(t, p, "SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("%d sessions in the pristine copy", n)
	}
	checkRestoreEmpty(t, s, "table:sessions", func() int { return roCount(t, app, "SELECT count(*) FROM sessions") },
		func() { mustExec(t, app, "INSERT INTO sessions VALUES (random(), 'y')") })
}

func TestSQLServerRestoreEmpty(t *testing.T) {
	s, db, _ := newSQLServerSeeded(t, Options{RestoreEmpty: []string{"cache"}},
		"CREATE TABLE news (id int PRIMARY KEY)", "INSERT INTO news VALUES (1)",
		"CREATE TABLE cache (id int IDENTITY PRIMARY KEY, v nvarchar(40) NULL)", "INSERT INTO cache (v) VALUES (N'the developer''s own')")
	if n := sqlServerInt(t, sqlServerTestDB(t, s.o.DSN, s.o.Pristine), "SELECT COUNT(*) FROM cache"); n != 0 {
		t.Fatalf("%d cache rows in the pristine copy", n)
	}
	checkRestoreEmpty(t, s, "table:dbo.cache", func() int { return int(sqlServerInt(t, db, "SELECT COUNT(*) FROM cache")) },
		func() { sqlServerExec(t, db, "INSERT INTO cache (v) VALUES (N'x')") })
}

// TestPostgresRestoreEmptyFallback: the in-place emptying of restore_empty
// tables gives way to a copy of the whole database when a session of the
// application holds a lock on them (a transaction left open), and when a
// TRUNCATE trigger would run; a seed refuses a TRUNCATE trigger and a
// table that inherits from one and is not restore_empty.
func TestPostgresRestoreEmptyFallback(t *testing.T) {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	defer func(d time.Duration) { pgLockWait = d }(pgLockWait)
	pgLockWait = 200 * time.Millisecond
	ctx := context.Background()
	work, pristine := "dgt_ref_work", "dgt_ref_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, RestoreEmpty: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	reset := func() {
		mustExec(t, p.admin, "DROP DATABASE IF EXISTS "+work+" WITH (FORCE)", "DROP DATABASE IF EXISTS "+pristine+" WITH (FORCE)", "CREATE DATABASE "+work)
	}
	reset()
	t.Cleanup(func() {
		p.admin.Exec("DROP DATABASE IF EXISTS " + work + " WITH (FORCE)")
		p.admin.Exec("DROP DATABASE IF EXISTS " + pristine + " WITH (FORCE)")
	})
	exec := func(stmts ...string) {
		db, err := p.open(work)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		mustExec(t, db, stmts...)
	}
	oid := func() int64 {
		var n int64
		if err := p.admin.QueryRow("SELECT oid FROM pg_database WHERE datname = $1", work).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	items := []string{"table:public.cache"}
	exec("CREATE TABLE news (id int PRIMARY KEY, n int)", "INSERT INTO news VALUES (1, 0)", "CREATE TABLE cache (k text PRIMARY KEY)")
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}

	// A session idle in a transaction that read the cache.
	exec("INSERT INTO cache VALUES ('a')")
	app, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := app.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM cache").Scan(&n); err != nil {
		t.Fatal(err)
	}
	before := oid()
	done := make(chan error, 1)
	go func() { done <- p.Restore(ctx, items) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the restore waits for the session's lock")
	}
	tx.Rollback()
	app.Close()
	if oid() == before {
		t.Fatal("a cache locked by a session was not restored by a copy")
	}

	// A working database that is missing is copied from the pristine one.
	mustExec(t, p.admin, "DROP DATABASE "+work+" WITH (FORCE)")
	if err := p.Restore(ctx, items); err != nil {
		t.Fatalf("restore of a missing working database: %v", err)
	}
	db, _ := p.open(work)
	if n := roCount(t, db, "SELECT count(*) FROM news"); n != 1 {
		t.Fatalf("%d news after the restore of a missing working database", n)
	}
	db.Close()

	// A TRUNCATE trigger that writes a tracked table: a copy instead.
	exec(`CREATE FUNCTION bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE news SET n = n + 1; RETURN NULL; END $$`,
		"CREATE TRIGGER cache_bump AFTER TRUNCATE ON cache FOR EACH STATEMENT EXECUTE FUNCTION bump()", "INSERT INTO cache VALUES ('b')")
	before = oid()
	if err := p.Restore(ctx, items); err != nil {
		t.Fatal(err)
	}
	db, _ = p.open(work)
	if n := roCount(t, db, "SELECT n FROM news"); n != 0 || oid() == before {
		t.Fatalf("news.n %d after a restore with a TRUNCATE trigger (copied: %v)", n, oid() != before)
	}
	db.Close()

	exec(`CREATE FUNCTION bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE news SET n = n + 1; RETURN NULL; END $$`,
		"CREATE TRIGGER cache_bump AFTER TRUNCATE ON cache FOR EACH STATEMENT EXECUTE FUNCTION bump()")
	if err := p.Seed(ctx); err == nil || !strings.Contains(err.Error(), "table public.cache has a TRUNCATE trigger (cache_bump)") {
		t.Fatalf("seed with a TRUNCATE trigger: %v", err)
	}
	reset()
	exec("CREATE TABLE cache (k text PRIMARY KEY)", "CREATE TABLE cache_archive (note text) INHERITS (cache)", "INSERT INTO cache_archive VALUES ('x', 'kept')")
	if err := p.Seed(ctx); err == nil || !strings.Contains(err.Error(), "table public.cache_archive inherits from public.cache and is neither restore_empty nor excluded") {
		t.Fatalf("seed with an inheriting table: %v", err)
	}
}
