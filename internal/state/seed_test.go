package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/orangehill/driftgate/internal/dbx"
)

// TestMySQLSeedMark: a seed marks the pristine database it makes and
// replaces only such a database, an empty one or an adopted one; the mark
// is no item of the state.
func TestMySQLSeedMark(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			work, pristine := "dgt_mark", "dgt_mark_pristine"
			root := mysqlAdmin(t, dsn, "")
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work, "CREATE DATABASE "+pristine)
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			mustExec(t, root, "CREATE TABLE "+work+".news (id INT PRIMARY KEY)", "INSERT INTO "+work+".news VALUES (1)")
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()

			// An empty database is taken; the seed marks it.
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			var n int
			root.QueryRow("SELECT COUNT(*) FROM " + pristine + "." + pristineTable).Scan(&n)
			if n != 1 {
				t.Fatalf("no mark in the pristine database")
			}
			if err := m.Restore(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Exec("SELECT 1 FROM " + work + "." + pristineTable); err == nil {
				t.Fatal("the mark was restored into the working database")
			}
			c, _ := m.session(ctx)
			p, err := m.fingerprint(ctx, c, pristine, nil)
			c.Close()
			if err != nil {
				t.Fatal(err)
			}
			if d := Diff(mustFingerprint(t, m), p); len(d) > 0 {
				t.Fatalf("pristine and working differ in %v", d)
			}
			// A marked database is replaced.
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}

			// Someone's database, not made by a seed, is never dropped.
			mustExec(t, root, "DROP TABLE "+pristine+"."+pristineTable, "CREATE TABLE "+pristine+".orders (id INT)")
			err = m.Seed(ctx)
			if err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
				t.Fatalf("seed over an unmarked database: %v", err)
			}
			if _, err := root.Exec("SELECT 1 FROM " + pristine + ".orders"); err != nil {
				t.Fatalf("the unmarked database was changed: %v", err)
			}

			// One a seed made before the mark existed is adopted.
			adopt, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, AdoptPristine: true})
			if err != nil {
				t.Fatal(err)
			}
			defer adopt.Close()
			if err := adopt.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			if err := m.Seed(ctx); err != nil {
				t.Fatalf("seed after the adoption: %v", err)
			}
		})
	}
}

// TestMySQLRestoreNoBinlog: a restore's copies stay out of the binary log.
func TestMySQLRestoreNoBinlog(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			root := mysqlAdmin(t, dsn, "")
			var on int
			root.QueryRow("SELECT @@log_bin").Scan(&on)
			if on == 0 {
				t.Skip("the binary log is off on this server")
			}
			work, pristine := "dgt_binlog", "dgt_binlog_pristine"
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "CREATE DATABASE "+work,
				"CREATE TABLE "+work+".news (id INT PRIMARY KEY, title VARCHAR(20))", "INSERT INTO "+work+".news VALUES (1, 'a'), (2, 'b')")
			t.Cleanup(func() { root.Exec("DROP DATABASE IF EXISTS " + work); root.Exec("DROP DATABASE IF EXISTS " + pristine) })
			m, err := NewMySQL(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(t, root, "UPDATE "+work+".news SET title = 'x'")
			before := binlogPosition(t, root)
			if err := m.Restore(ctx, []string{"table:news"}); err != nil {
				t.Fatal(err)
			}
			if events := binlogEvents(t, root, before, work, pristine); len(events) > 0 {
				t.Fatalf("the restore was logged: %q", events)
			}
		})
	}
}

// binlogPosition is the server's binary log file and position.
func binlogPosition(t *testing.T, root *sql.DB) string {
	t.Helper()
	for _, q := range []string{"SHOW BINARY LOG STATUS", "SHOW MASTER STATUS"} {
		rows, err := root.Query(q)
		if err != nil {
			continue
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		var file, pos string
		vals[0], vals[1] = &file, &pos
		for i := 2; i < len(cols); i++ {
			vals[i] = new(any)
		}
		if !rows.Next() {
			t.Fatal("no binary log position")
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatal(err)
		}
		return file + ":" + pos
	}
	t.Fatal("no binary log status")
	return ""
}

// binlogEvents lists the binary log events after the position from
// binlogPosition that name one of the databases. Other tests
// write on the same server at the same time: the position alone moves with
// their writes.
func binlogEvents(t *testing.T, root *sql.DB, from string, databases ...string) []string {
	t.Helper()
	file, pos, _ := strings.Cut(from, ":")
	var files []string
	rows, err := root.Query("SHOW BINARY LOGS")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		var name string
		vals[0] = &name
		for i := 1; i < len(cols); i++ {
			vals[i] = new(any)
		}
		if err := rows.Scan(vals...); err != nil {
			t.Fatal(err)
		}
		if name >= file {
			files = append(files, name)
		}
	}
	rows.Close()
	var out []string
	for _, f := range files {
		q := "SHOW BINLOG EVENTS IN '" + f + "'"
		if f == file {
			q += " FROM " + pos
		}
		rows, err := root.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var log, typ, info string
			var at, server, end any
			if err := rows.Scan(&log, &at, &typ, &server, &end, &info); err != nil {
				t.Fatal(err)
			}
			for _, db := range databases {
				// Row events name db.table, statements `db`.
				if strings.Contains(info, db+".") || strings.Contains(info, "`"+db+"`") {
					out = append(out, typ+": "+info)
					break
				}
			}
		}
		rows.Close()
	}
	return out
}

// TestMySQLRestoreLoggedUnknown: a user that may not list the server's
// replicas cannot tell whether one reads the binary log, so its restores
// are logged, with a note.
func TestMySQLRestoreLoggedUnknown(t *testing.T) {
	for _, dsn := range mysqlServers(t) {
		u, _ := url.Parse(dsn)
		t.Run(u.Host, func(t *testing.T) {
			ctx := context.Background()
			root := mysqlAdmin(t, dsn, "")
			var on int
			root.QueryRow("SELECT @@log_bin").Scan(&on)
			if on == 0 {
				t.Skip("the binary log is off on this server")
			}
			work, pristine := "dgt_binlog2", "dgt_binlog2_pristine"
			mustExec(t, root, "DROP DATABASE IF EXISTS "+work, "DROP DATABASE IF EXISTS "+pristine, "DROP USER IF EXISTS 'dgt_norepl'@'%'",
				"CREATE DATABASE "+work, "CREATE TABLE "+work+".news (id INT PRIMARY KEY, title VARCHAR(20))", "INSERT INTO "+work+".news VALUES (1, 'a')",
				"CREATE USER 'dgt_norepl'@'%' IDENTIFIED BY 'x'", "GRANT ALL ON *.* TO 'dgt_norepl'@'%'")
			t.Cleanup(func() {
				root.Exec("DROP DATABASE IF EXISTS " + work)
				root.Exec("DROP DATABASE IF EXISTS " + pristine)
				root.Exec("DROP USER IF EXISTS 'dgt_norepl'@'%'")
			})
			// What lists replicas: MySQL REPLICATION SLAVE, MariaDB 10.5+
			// REPLICATION MASTER ADMIN (before: REPLICATION SLAVE).
			for _, priv := range []string{"REPLICATION SLAVE", "REPLICATION MASTER ADMIN"} {
				root.Exec("REVOKE " + priv + " ON *.* FROM 'dgt_norepl'@'%'")
			}
			u.User = url.UserPassword("dgt_norepl", "x")
			var log strings.Builder
			m, err := NewMySQL(Options{Name: "db", DSN: u.String(), Working: work, Pristine: pristine, Log: &log})
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.Seed(ctx); err != nil {
				t.Fatal(err)
			}
			mustExec(t, root, "UPDATE "+work+".news SET title = 'x'")
			before := binlogPosition(t, root)
			if err := m.Restore(ctx, []string{"table:news"}); err != nil {
				t.Fatal(err)
			}
			if len(binlogEvents(t, root, before, work, pristine)) == 0 {
				t.Fatal("the restore was not logged")
			}
			if !strings.Contains(log.String(), "whether a replica reads it is unknown") || strings.Count(log.String(), "binary log") != 1 {
				t.Fatalf("log: %s", log.String())
			}
		})
	}
}

func postgresServer(t *testing.T) string {
	dsn := os.Getenv("DRIFTGATE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_POSTGRES not set")
	}
	return dsn
}

// TestPostgresSafeCopy: a restore or seed builds the new database under
// another name and replaces the old one only when it is complete; a missing
// working database is restored in full.
func TestPostgresSafeCopy(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work, pristine := "dgt_safe", "dgt_safe_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	drop := func() {
		for _, db := range []string{work, pristine, newName(work), newName(pristine)} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
	}
	drop()
	t.Cleanup(drop)
	mustExec(t, p.admin, "CREATE DATABASE "+work)
	db, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE TABLE news (id int PRIMARY KEY)", "INSERT INTO news VALUES (1), (2)")
	db.Close()
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	fp0, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A copy that fails before it takes the name leaves the database as it was.
	boom := errors.New("boom")
	if err := p.copyDatabase(ctx, pristine, work, func(string) error { return boom }, nil); !errors.Is(err, boom) {
		t.Fatalf("copy: %v", err)
	}
	for db, want := range map[string]bool{work: true, newName(work): false} {
		if ok, _ := p.exists(ctx, db); ok != want {
			t.Fatalf("%s exists: %v, want %v", db, ok, want)
		}
	}

	// A copy left behind by a restore that was killed is replaced.
	mustExec(t, p.admin, "CREATE DATABASE "+qp(newName(work)))
	if err := p.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// No working database: no items, and a restore brings it back.
	mustExec(t, p.admin, "DROP DATABASE "+work+" WITH (FORCE)")
	f, err := p.Fingerprint(ctx, nil)
	if err != nil || len(f) != 0 {
		t.Fatalf("fingerprint of a missing database: %v, %v", f, err)
	}
	if err := p.Restore(ctx, Diff(f, fp0)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Fingerprint(ctx, nil); len(Diff(got, fp0)) > 0 {
		t.Fatalf("after the restore: %v", Diff(got, fp0))
	}
	if ok, _ := p.exists(ctx, work); !ok {
		t.Fatal("no working database after the restore")
	}

	// The pristine database is marked; an unmarked one with objects is not
	// replaced unless adopted, an empty one is.
	var comment string
	p.admin.QueryRow("SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", pristine).Scan(&comment)
	if !strings.HasPrefix(comment, pristineComment) {
		t.Fatalf("pristine comment %q", comment)
	}
	mustExec(t, p.admin, "COMMENT ON DATABASE "+pristine+" IS NULL")
	if err := p.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over an unmarked database: %v", err)
	}
	adopt, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine, AdoptPristine: true})
	if err != nil {
		t.Fatal(err)
	}
	defer adopt.Close()
	if err := adopt.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, p.admin, "DROP DATABASE "+pristine, "CREATE DATABASE "+pristine)
	if err := p.Seed(ctx); err != nil {
		t.Fatalf("seed over an empty database: %v", err)
	}
	mustExec(t, p.admin, "DROP DATABASE "+pristine, "CREATE DATABASE "+pristine)
	db, _ = p.open(pristine)
	mustExec(t, db, "CREATE TABLE orders (id int)")
	db.Close()
	if err := p.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over someone's database: %v", err)
	}
}

// TestPostgresOtherSessions: a copy ends the other sessions on the
// database it copies or renames away. A role that is not a superuser may
// not end a superuser's: when the session stays, it gets that error (42501)
// after one wait of the server (5 s), not after five; when the session ends
// within that wait, the copy succeeds. A cancel while a session is in the
// way returns at once.
func TestPostgresOtherSessions(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work, pristine := "dgt_term", "dgt_term_pristine"
	su, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		for _, db := range []string{work, pristine, newName(work), oldName(work)} {
			su.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
		su.admin.Exec("DROP ROLE IF EXISTS dgt_term_app")
	}
	drop()
	t.Cleanup(func() {
		drop()
		su.Close()
	})
	mustExec(t, su.admin, "CREATE ROLE dgt_term_app LOGIN PASSWORD 'app' CREATEDB",
		"CREATE DATABASE "+work+" OWNER dgt_term_app", "CREATE DATABASE "+pristine+" OWNER dgt_term_app")
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword("dgt_term_app", "app")
	p, err := NewPostgres(Options{Name: "db", DSN: u.String(), Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// hold opens a superuser session on db that runs q; done gets its error.
	hold := func(db, q string) (done chan error, release func()) {
		t.Helper()
		d, err := su.open(db)
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan error, 1)
		go func() {
			_, err := d.Exec(q)
			d.Close()
			done <- err
		}()
		for i := 0; ; i++ {
			var n int
			su.admin.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query = $2", db, q).Scan(&n)
			if n > 0 {
				break
			}
			if i == 200 {
				t.Fatalf("no session on %s", db)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return done, func() {
			su.admin.Exec("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", db)
			<-done
		}
	}
	// The copy (CREATE DATABASE … TEMPLATE pristine) and the rename of work.
	for _, db := range []string{pristine, work} {
		t.Run(db, func(t *testing.T) {
			// A session that stays: the error that says why it stays.
			_, release := hold(db, "SELECT pg_sleep(60)")
			// pgTerminate's filter (no autovacuum workers) keeps it, also
			// for a role that may not see the superuser's session.
			var n int
			if err := p.admin.QueryRow(strings.Replace(pgTerminate, "pg_catalog.pg_terminate_backend(pid)", "count(*)", 1), db).Scan(&n); err != nil || n != 1 {
				t.Fatalf("sessions pgTerminate ends: %d, %v", n, err)
			}
			start := time.Now()
			err := p.copyDatabase(ctx, pristine, work, nil, nil)
			took := time.Since(start)
			release()
			pe := (*pgconn.PgError)(nil)
			if !errors.As(err, &pe) || pe.Code != "55006" || !strings.Contains(err.Error(), "SQLSTATE 42501") {
				t.Fatalf("copy over a superuser's session: %v", err)
			}
			if took > 8*time.Second {
				t.Fatalf("the error took %v", took)
			}
			var allow bool
			if err := su.admin.QueryRow("SELECT datallowconn FROM pg_database WHERE datname = $1", work).Scan(&allow); err != nil || !allow {
				t.Fatalf("working database after the failed copy: %v, %v", allow, err)
			}

			// A session that ends within the server's wait: the copy succeeds.
			done, _ := hold(db, "SELECT pg_sleep(1.5)")
			if err := p.copyDatabase(ctx, pristine, work, nil, nil); err != nil {
				t.Fatalf("copy over a session that ends: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("the session was ended: %v", err)
			}
			p.wait()
		})
	}

	// A cancel while a session is in the way (one that is not ended here)
	// returns at once. alone is tested itself, as the copy and the rename
	// share it, and what copyDatabase does after it (the drop of its copy,
	// which may checkpoint) is not part of the wait.
	defer func(q string) { pgTerminate = q }(pgTerminate)
	pgTerminate = "SELECT $1::text"
	_, release := hold(pristine, "SELECT pg_sleep(60)")
	defer release()
	create := "CREATE DATABASE " + qp(newName(work)) + " TEMPLATE " + qp(pristine)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	at := make(chan time.Time, 1)
	go func() {
		// Cancel once the first attempt failed: in the wait before the
		// next one.
		for seen := false; cctx.Err() == nil; time.Sleep(5 * time.Millisecond) {
			var n int
			su.admin.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query = $1", create).Scan(&n)
			if n > 0 {
				seen = true
			} else if seen {
				at <- time.Now()
				cancel()
			}
		}
	}()
	err = p.alone(cctx, pristine, create)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy cancelled: %v", err)
	}
	if took := time.Since(<-at); took > 150*time.Millisecond {
		t.Fatalf("returned %v after the cancel", took)
	}
}

func TestNewName(t *testing.T) {
	long := strings.Repeat("ž", 40) // 80 bytes
	if got := newName(long); len(got) > 63 || !strings.HasSuffix(got, newSuffix) || !strings.HasPrefix(long, strings.TrimSuffix(got, newSuffix)) {
		t.Fatalf("newName: %q", got)
	}
}

// TestPostgresKeepsDatabase: a restore or seed keeps what the copy of a
// template does not: the database's owner, its settings, privileges,
// comment and connection limit. The application's role, which owns the
// working database, can still create tables in public (PostgreSQL 15+:
// the schema belongs to the database's owner).
func TestPostgresKeepsDatabase(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work, pristine := "dgt_keep", "dgt_keep_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		for _, db := range []string{work, pristine, newName(work), oldName(work), newName(pristine), oldName(pristine)} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
		p.admin.Exec("DROP ROLE IF EXISTS dgt_keep_app")
		p.admin.Exec("DROP ROLE IF EXISTS dgt_keep_ro")
	}
	drop()
	t.Cleanup(func() {
		p.Close()
		drop()
	})
	mustExec(t, p.admin,
		"CREATE ROLE dgt_keep_app LOGIN PASSWORD 'app'", "CREATE ROLE dgt_keep_ro",
		"CREATE DATABASE "+work+" OWNER dgt_keep_app CONNECTION LIMIT 50",
		"ALTER DATABASE "+work+" SET timezone TO 'Asia/Tokyo'",
		"ALTER DATABASE "+work+` SET search_path TO 'app', '$user', 'public', 'My "Schema"'`,
		"ALTER ROLE dgt_keep_ro IN DATABASE "+work+" SET work_mem TO '8MB'",
		"COMMENT ON DATABASE "+work+" IS 'the application''s'",
		"REVOKE ALL ON DATABASE "+work+" FROM PUBLIC",
		"GRANT CONNECT ON DATABASE "+work+" TO dgt_keep_ro WITH GRANT OPTION")
	props := func() string {
		t.Helper()
		var s string
		if err := p.admin.QueryRow(`SELECT pg_get_userbyid(d.datdba) || ' ' || d.datconnlimit || ' ' || coalesce(shobj_description(d.oid, 'pg_database'), '') || ' ' ||
			coalesce(d.datacl::text, '') || ' ' || coalesce((SELECT string_agg(coalesce(r.rolname, '') || ':' || array_to_string(s.setconfig, ';'), ' ' ORDER BY r.rolname NULLS FIRST)
				FROM pg_db_role_setting s LEFT JOIN pg_roles r ON r.oid = s.setrole WHERE s.setdatabase = d.oid), '')
			FROM pg_database d WHERE d.datname = $1`, work).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := props()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User, u.Path = url.UserPassword("dgt_keep_app", "app"), "/"+work
	app := func(stmt string) error {
		db, err := dbx.OpenPostgres(u.String(), "", nil)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec(stmt)
		return err
	}
	if err := app("CREATE TABLE news (id int PRIMARY KEY, at timestamptz)"); err != nil {
		t.Fatal(err)
	}
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if got := props(); got != want {
		t.Fatalf("after the seed:\n%s\nwant\n%s", got, want)
	}
	for i := 0; i < 2; i++ {
		if err := p.Restore(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if got := props(); got != want {
			t.Fatalf("after restore %d:\n%s\nwant\n%s", i+1, got, want)
		}
		if err := app(fmt.Sprintf("CREATE TABLE t%d (id int)", i)); err != nil {
			t.Fatalf("the application after restore %d: %v", i+1, err)
		}
	}
	db, err := dbx.OpenPostgres(u.String(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tz string
	if err := db.QueryRow("SHOW TimeZone").Scan(&tz); err != nil || tz != "Asia/Tokyo" {
		t.Fatalf("time zone of the application: %q, %v", tz, err)
	}
}

// TestPostgresSeedSettings: the pristine database gets the working
// database's settings, not those of the copy it replaces, so its
// fingerprint, which a seed records, prints times, dates, intervals and
// names as the working database's does; every restore then matches it.
func TestPostgresSeedSettings(t *testing.T) {
	dsn := postgresServer(t)
	ctx := context.Background()
	work, pristine := "dgt_settings", "dgt_settings_pristine"
	p, err := NewPostgres(Options{Name: "db", DSN: dsn, Working: work, Pristine: pristine})
	if err != nil {
		t.Fatal(err)
	}
	drop := func() {
		for _, db := range []string{work, pristine, newName(work), oldName(work), newName(pristine), oldName(pristine)} {
			p.admin.Exec("DROP DATABASE IF EXISTS " + qp(db) + " WITH (FORCE)")
		}
	}
	drop()
	t.Cleanup(func() {
		p.Close()
		drop()
	})
	settings := func(db string) string {
		t.Helper()
		var s sql.NullString
		if err := p.admin.QueryRow(`SELECT string_agg(CASE WHEN s.setrole = 0 THEN '' ELSE 'role ' END || array_to_string(s.setconfig, ';'), ' ' ORDER BY s.setrole)
			FROM pg_db_role_setting s JOIN pg_database d ON d.oid = s.setdatabase WHERE d.datname = $1`, db).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s.String
	}
	mustExec(t, p.admin, "CREATE DATABASE "+work)
	db, err := p.open(work)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "CREATE SCHEMA app", "CREATE SEQUENCE app.s",
		"CREATE TABLE app.t (id int DEFAULT nextval('app.s'), at timestamptz, day date, took interval)",
		"CREATE VIEW app.v AS SELECT id, at FROM app.t",
		"INSERT INTO app.t (at, day, took) VALUES ('2026-01-01 00:00:00+00', '2026-02-03', '1 day 2 hours')")
	db.Close()
	// A pristine copy of earlier settings, which the next one must not keep.
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, p.admin,
		"ALTER DATABASE "+pristine+" SET timezone TO 'America/New_York'",
		"ALTER ROLE CURRENT_USER IN DATABASE "+pristine+" SET extra_float_digits TO 1",
		"ALTER DATABASE "+work+" SET timezone TO 'Asia/Tokyo'",
		"ALTER DATABASE "+work+" SET search_path TO app, public",
		"ALTER DATABASE "+work+" SET datestyle TO 'SQL, DMY'",
		"ALTER ROLE CURRENT_USER IN DATABASE "+work+" SET intervalstyle TO 'iso_8601'")
	if err := p.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := settings(pristine), settings(work); got != want {
		t.Fatalf("settings of the pristine database %q, want the working database's %q", got, want)
	}
	working, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := p.SeededFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(working, seeded); len(d) > 0 {
		t.Fatalf("the seeded fingerprint differs from the working database's: %v", d)
	}
	if err := p.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	restored, err := p.Fingerprint(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(restored, seeded); len(d) > 0 {
		t.Fatalf("after the restore the working database differs from the seeded fingerprint: %v", d)
	}
}

func TestPgSettingValue(t *testing.T) {
	for _, c := range []struct{ name, value, want string }{
		{"timezone", "Europe/Belgrade", `'Europe/Belgrade'`},
		{"DateStyle", "ISO, DMY", `'ISO, DMY'`},
		{"search_path", `app, "$user", public`, `'app', '$user', 'public'`},
		{"search_path", `"My ""Schema""", x`, `'My "Schema"', 'x'`},
		{"search_path", `""`, `''`},
		{"app.note", "it's", `'it''s'`},
	} {
		if got := pgSettingValue(c.name, c.value); got != c.want {
			t.Errorf("%s=%s: %s, want %s", c.name, c.value, got, c.want)
		}
	}
}
