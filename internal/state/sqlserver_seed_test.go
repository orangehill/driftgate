package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
)

// TestSQLServerSeedMark: a seed marks the pristine copy it makes,
// replaces only such a database, an empty one or an adopted one, never
// the pristine copy of another working database, and drops what a seed
// that stopped left.
func TestSQLServerSeedMark(t *testing.T) {
	ctx := context.Background()
	s, db := newSQLServerTest(t, Options{})
	cleanSQLServerSeed(t, s)
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, db, "CREATE TABLE news (id int PRIMARY KEY)", "INSERT INTO news VALUES (1)")

	// An empty database is taken; the seed marks it.
	sqlServerExec(t, master, "CREATE DATABASE "+qms(s.o.Pristine))
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := s.readMark(ctx, s.o.Pristine)
	if err != nil || m == nil || m.Working != s.o.Working || m.Fallback == "" {
		t.Fatalf("mark %+v, %v", m, err)
	}
	d, _ := s.database(ctx, s.o.Pristine)
	if !d.ReadOnly || d.UserAccess != "MULTI_USER" {
		t.Fatalf("pristine copy read-only %t, %s", d.ReadOnly, d.UserAccess)
	}
	if d := Diff(mustSQLServerFingerprint(t, s), pristineSQLServer(t, s)); len(d) > 0 {
		t.Fatalf("pristine and working differ in %v", d)
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sys.extended_properties WHERE class = 0 AND name = N'driftgate_pristine'"); n != 0 {
		t.Fatal("the working database is marked")
	}
	c, _ := s.conn(ctx, "")
	first, err := s.fallbackFile(ctx, c, m)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}

	// A marked database is replaced; the backup goes to the other file.
	sqlServerExec(t, db, "INSERT INTO news VALUES (2)")
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	m2, _ := s.readMark(ctx, s.o.Pristine)
	c, _ = s.conn(ctx, "")
	second, err := s.fallbackFile(ctx, c, m2)
	c.Close()
	if err != nil || second == first {
		t.Fatalf("the second seed's backup is in %s (the first's in %s): %v", second, first, err)
	}
	if n := sqlServerInt(t, sqlServerTestDB(t, s.o.DSN, s.o.Pristine), "SELECT COUNT(*) FROM news"); n != 2 {
		t.Fatalf("pristine copy has %d rows, want 2", n)
	}

	// A seed that stopped after its RESTORE left an unmarked copy of the
	// working database: the next seed drops it. Someone's database of
	// that name is never dropped.
	dgnew := s.o.Pristine + "_dgnew"
	sqlServerExec(t, master, "BACKUP DATABASE "+qms(s.o.Working)+" TO DISK = "+sqlServerString(first)+" WITH COPY_ONLY, INIT, FORMAT")
	files, err := func() ([]ssBackupFile, error) {
		c, _ := s.conn(ctx, "")
		defer c.Close()
		return backupFiles(ctx, c, first)
	}()
	if err != nil {
		t.Fatal(err)
	}
	restore := func(name func(f ssBackupFile) string) {
		q := "RESTORE DATABASE " + qms(dgnew) + " FROM DISK = " + sqlServerString(first) + " WITH RECOVERY"
		for _, f := range files {
			q += ", MOVE " + sqlServerString(f.Logical) + " TO " + sqlServerString(f.Physical[:strings.LastIndexAny(f.Physical, `/\`)+1]+name(f))
		}
		sqlServerExec(t, master, q)
	}
	// A copy of the working database with files of other names is
	// someone's.
	restore(func(f ssBackupFile) string { return fmt.Sprintf("mine_%d.%s", f.ID, strings.ToLower(f.Type)) })
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over someone's copy of the working database: %v", err)
	}
	dropSQLServerDatabase(master, dgnew)
	restore(func(f ssBackupFile) string {
		return fmt.Sprintf("dg_0badc0de_%d.%s", f.ID, map[string]string{"D": "mdf", "L": "ldf"}[f.Type])
	})
	if err := s.Seed(ctx); err != nil {
		t.Fatalf("seed over a copy a stopped seed left: %v", err)
	}
	sqlServerExec(t, master, "CREATE DATABASE "+qms(dgnew), "CREATE TABLE "+qms(dgnew)+".dbo.mine (id int)")
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over someone's %s: %v", dgnew, err)
	}
	dropSQLServerDatabase(master, dgnew)

	// Someone's database, not made by a seed, is never dropped.
	dropSQLServerDatabase(master, s.o.Pristine)
	sqlServerExec(t, master, "CREATE DATABASE "+qms(s.o.Pristine), "CREATE TABLE "+qms(s.o.Pristine)+".dbo.orders (id int)")
	if err := s.Seed(ctx); err == nil || !strings.Contains(err.Error(), "was not made by driftgate seed") {
		t.Fatalf("seed over an unmarked database: %v", err)
	}
	if err := s.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "no DriftGate mark") {
		t.Fatalf("restore from an unmarked database: %v", err)
	}
	if _, err := master.Exec("SELECT 1 FROM " + qms(s.o.Pristine) + ".dbo.orders"); err != nil {
		t.Fatalf("the unmarked database was changed: %v", err)
	}
	// Adopted, it is replaced.
	o := s.o
	o.AdoptPristine = true
	adopt, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer adopt.Close()
	if err := adopt.Seed(ctx); err != nil {
		t.Fatalf("seed over an adopted database: %v", err)
	}

	// The pristine copy of another working database is not replaced or
	// restored from.
	other := sqlServerTestName()
	sqlServerExec(t, master, "CREATE DATABASE "+qms(other))
	t.Cleanup(func() { dropSQLServerDatabase(master, other) })
	o = s.o
	o.Working, o.AdoptPristine = other, true
	theirs, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer theirs.Close()
	if err := theirs.Seed(ctx); err == nil || !strings.Contains(err.Error(), "is the pristine copy of "+s.o.Working) {
		t.Fatalf("seed over another working database's copy: %v", err)
	}
	if err := theirs.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "is the pristine copy of "+s.o.Working) {
		t.Fatalf("restore from another working database's copy: %v", err)
	}

	// No working database, no seed.
	o.Working = other + "_none"
	none, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	defer none.Close()
	if err := none.Seed(ctx); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("seed without a working database: %v", err)
	}
}

// TestSQLServerSeedExcluded: the rows of excluded tables are neither in
// the pristine copy nor in the backup a restore of the whole database
// restores from, and a seed fires no trigger.
func TestSQLServerSeedExcluded(t *testing.T) {
	ctx := context.Background()
	s, db, _ := newSQLServerSeeded(t, Options{Exclude: []string{"sess*"}},
		"CREATE TABLE users (id int PRIMARY KEY)", "INSERT INTO users VALUES (1)",
		"CREATE TABLE sessions (id int PRIMARY KEY, user_id int NULL REFERENCES users(id) ON DELETE CASCADE, secret nvarchar(50) NULL)",
		"CREATE TABLE session_log (id int PRIMARY KEY, session_id int NULL REFERENCES sessions(id))",
		"INSERT INTO sessions VALUES (1, 1, N'top secret')", "INSERT INTO session_log VALUES (1, 1)",
		"CREATE TABLE audit (n int)",
		"CREATE TRIGGER sessions_audit ON sessions AFTER DELETE AS INSERT INTO audit VALUES (1)",
		"CREATE TRIGGER no_alter ON DATABASE FOR ALTER_TABLE AS BEGIN RAISERROR (N'no', 16, 1); ROLLBACK; END")
	pdb := sqlServerTestDB(t, s.o.DSN, s.o.Pristine)
	for _, q := range []string{"SELECT COUNT(*) FROM sessions", "SELECT COUNT(*) FROM session_log", "SELECT COUNT(*) FROM audit"} {
		if n := sqlServerInt(t, pdb, q); n != 0 {
			t.Fatalf("pristine copy: %s = %d", q, n)
		}
	}
	if n := sqlServerInt(t, pdb, "SELECT COUNT(*) FROM users"); n != 1 {
		t.Fatal("the pristine copy lost a row of a table that is not excluded")
	}
	if got, want := sqlServerFlags(t, pdb), sqlServerFlags(t, db); got != want {
		t.Fatalf("flags of the pristine copy\n%s\nwant\n%s", got, want)
	}
	// The backup, restored as a database of its own.
	m, _ := s.readMark(ctx, s.o.Pristine)
	c, _ := s.conn(ctx, "")
	defer c.Close()
	bak, err := s.fallbackFile(ctx, c, m)
	if err != nil {
		t.Fatal(err)
	}
	files, err := backupFiles(ctx, c, bak)
	if err != nil {
		t.Fatal(err)
	}
	check := s.o.Working + "_bak"
	master := sqlServerTestDB(t, s.o.DSN, "")
	t.Cleanup(func() { dropSQLServerDatabase(master, check) })
	restore := "RESTORE DATABASE " + qms(check) + " FROM DISK = " + sqlServerString(bak) + " WITH RECOVERY"
	for _, f := range files {
		restore += ", MOVE " + sqlServerString(f.Logical) + " TO " + sqlServerString(f.Physical+".check")
	}
	sqlServerExec(t, master, restore)
	if n := sqlServerInt(t, master, "SELECT COUNT(*) FROM "+qms(check)+".dbo.sessions"); n != 0 {
		t.Fatalf("%d rows of an excluded table in the backup", n)
	}
	if n := sqlServerInt(t, master, "SELECT COUNT(*) FROM "+qms(check)+".sys.extended_properties WHERE class = 0"); n != 0 {
		t.Fatal("the backup holds the mark")
	}
}

// TestSQLServerSeedReferencesExcluded: a seed refuses to leave out rows
// of an excluded table that a table that is not excluded refers to with a
// trusted foreign key.
func TestSQLServerSeedReferencesExcluded(t *testing.T) {
	s, db := newSQLServerTest(t, Options{Exclude: []string{"sessions"}})
	cleanSQLServerSeed(t, s)
	sqlServerExec(t, db, "CREATE TABLE sessions (id int PRIMARY KEY)", "INSERT INTO sessions VALUES (1)",
		"CREATE TABLE logins (id int PRIMARY KEY, session_id int NULL REFERENCES sessions(id))", "INSERT INTO logins VALUES (1, 1)")
	if err := s.Seed(context.Background()); err == nil || !strings.Contains(err.Error(), "exclude that table too") {
		t.Fatalf("seed: %v", err)
	}
	if d, _ := s.database(context.Background(), s.o.Pristine+"_dgnew"); d != nil {
		t.Fatal("the failed seed left its copy")
	}
}

// TestSQLServerChaosHelper is the process TestSQLServerChaos kills at a
// chaos point; it runs only from there.
func TestSQLServerChaosHelper(t *testing.T) {
	point := os.Getenv("DRIFTGATE_TEST_SS_CHAOS")
	if point == "" {
		t.Skip("run by TestSQLServerChaos")
	}
	o := Options{Name: "db", DSN: os.Getenv("DRIFTGATE_TEST_SQLSERVER"), Working: os.Getenv("DRIFTGATE_TEST_SS_WORKING"),
		Pristine: os.Getenv("DRIFTGATE_TEST_SS_WORKING") + "_p", Exclude: []string{"sessions"}}
	s, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	chaos.At = func(p string) {
		if p == point {
			os.Exit(7) // as a kill: no deferred function runs, the server ends the sessions
		}
	}
	if strings.HasPrefix(point, "state.sqlserver.seed") {
		err = s.Seed(context.Background())
	} else {
		err = s.Restore(context.Background(), nil)
	}
	t.Fatalf("not stopped at %s: %v", point, err)
}

// TestSQLServerChaos: a seed or a restore killed at any of its points
// leaves databases the next seed and restore recover from, with the rows
// of excluded tables kept.
func TestSQLServerChaos(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	ctx := context.Background()
	s, db, log := newSQLServerSeeded(t, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE users (id int IDENTITY PRIMARY KEY, name nvarchar(20) NULL)", "INSERT INTO users (name) VALUES (N'a')",
		"CREATE TABLE sessions (id int IDENTITY PRIMARY KEY, n int NULL)",
		"CREATE VIEW v AS SELECT id FROM users")
	kill := func(point string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSQLServerChaosHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "DRIFTGATE_TEST_SS_CHAOS="+point, "DRIFTGATE_TEST_SS_WORKING="+s.o.Working)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("%s: %v\n%s", point, err, out)
		}
	}
	sessions := func() string {
		rows, err := db.Query("SELECT CONCAT(id, ' ', n) FROM sessions UNION ALL SELECT CONCAT('ident ', IDENT_CURRENT('sessions')) ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var l string
			rows.Scan(&l)
			out = append(out, l)
		}
		return strings.Join(out, ",")
	}
	n := 0
	for _, point := range []string{"state.sqlserver.restore", "state.sqlserver.restore.saved", "state.sqlserver.restore.offline", "state.sqlserver.restore.restored"} {
		t.Run(point, func(t *testing.T) {
			p := pristineSQLServer(t, s)
			n++
			sqlServerExec(t, db, "INSERT INTO users (name) VALUES (N'b')", "INSERT INTO sessions (n) VALUES ("+string(rune('0'+n))+")")
			if point != "state.sqlserver.restore" {
				sqlServerExec(t, db, "ALTER VIEW v AS SELECT id, name FROM users")
			}
			want := sessions()
			kill(point)
			db.SetMaxIdleConns(0)
			db.SetMaxIdleConns(2)
			if err := s.Restore(ctx, nil); err != nil {
				t.Fatalf("restore after the kill: %v", err)
			}
			log.fellBack()
			db.SetMaxIdleConns(0)
			db.SetMaxIdleConns(2)
			if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
				t.Fatalf("after the restore %v differ", d)
			}
			if got := sessions(); got != want {
				t.Fatalf("excluded rows %s, want %s", got, want)
			}
		})
	}
	for _, point := range []string{"state.sqlserver.seed.backup", "state.sqlserver.seed.restored", "state.sqlserver.seed.marked", "state.sqlserver.seed.swap"} {
		t.Run(point, func(t *testing.T) {
			sqlServerExec(t, db, "INSERT INTO users (name) VALUES (N'c')")
			kill(point)
			// The old pristine copy is still there and restores, or
			// there is none.
			if err := s.Restore(ctx, nil); err != nil && !strings.Contains(err.Error(), "run driftgate seed") {
				t.Fatalf("restore after the kill: %v", err)
			}
			if err := s.Seed(ctx); err != nil {
				t.Fatalf("seed after the kill: %v", err)
			}
			p := pristineSQLServer(t, s)
			if d := Diff(sqlServerFull(t, s), p); len(d) > 0 {
				t.Fatalf("after the seed %v differ", d)
			}
			sqlServerExec(t, db, "INSERT INTO users (name) VALUES (N'd')")
			restoreSQLServer(t, s, db, log, p)
			if d, _ := s.database(ctx, s.o.Pristine+"_dgnew"); d != nil {
				t.Fatal("a seed's copy is left")
			}
		})
	}
}

// TestSQLServerNotSysadmin: a login with dbcreator and db_owner of the
// working database seeds, restores item by item and the whole database,
// and seeds again.
func TestSQLServerNotSysadmin(t *testing.T) {
	testSQLServerLogin(t, false)
}

// TestSQLServerNarrowGrant: so does a login with CREATE ANY DATABASE that
// owns the working database (the narrower grant the docs offer instead of
// dbcreator), and it cannot take another database offline or restore over
// it.
func TestSQLServerNarrowGrant(t *testing.T) {
	testSQLServerLogin(t, true)
}

func testSQLServerLogin(t *testing.T, narrow bool) {
	ctx := context.Background()
	s, db := newSQLServerTest(t, Options{})
	b := make([]byte, 4)
	rand.Read(b)
	login, password := "dgt_login_"+hex.EncodeToString(b), "Dg-"+hex.EncodeToString(b)+"-Pw;@{x}"
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, master, "CREATE LOGIN "+qms(login)+" WITH PASSWORD = "+sqlServerString(password)+", CHECK_POLICY = OFF")
	if narrow {
		sqlServerExec(t, master, "GRANT CREATE ANY DATABASE TO "+qms(login), "ALTER AUTHORIZATION ON DATABASE::"+qms(s.o.Working)+" TO "+qms(login))
	} else {
		sqlServerExec(t, master, "ALTER SERVER ROLE dbcreator ADD MEMBER "+qms(login))
		sqlServerExec(t, db, "CREATE USER "+qms(login)+" FOR LOGIN "+qms(login), "ALTER ROLE db_owner ADD MEMBER "+qms(login))
	}
	sqlServerExec(t, db, "CREATE TABLE users (id int IDENTITY PRIMARY KEY, name nvarchar(20) NULL)", "INSERT INTO users (name) VALUES (N'a')",
		"CREATE TABLE sessions (id int PRIMARY KEY)", "INSERT INTO sessions VALUES (1)",
		"CREATE SEQUENCE seq START WITH 1", "CREATE VIEW v AS SELECT id FROM users")
	u, _ := url.Parse(s.o.DSN)
	u.User = url.UserPassword(login, password)
	o := s.o
	o.DSN, o.Exclude = u.String(), []string{"sessions"}
	log := &ssLog{}
	o.Log = log
	user, err := NewSQLServer(o)
	if err != nil {
		t.Fatal(err)
	}
	cleanSQLServerSeed(t, user)
	t.Cleanup(func() {
		user.Close()
		dropSQLServerDatabase(master, o.Pristine)
		dropSQLServerDatabase(master, o.Working) // a login that owns a database cannot be dropped
		master.Exec("DROP LOGIN " + qms(login))
	})
	if info := mustSQLServerInfo(t, user); info.Sysadmin {
		t.Fatal("the login is sysadmin")
	}
	if err := user.Seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := pristineSQLServer(t, user)
	sqlServerExec(t, db, "INSERT INTO users (name) VALUES (N'b')", "DECLARE @x int = NEXT VALUE FOR seq", "INSERT INTO sessions VALUES (2)")
	if restoreSQLServer(t, user, db, log, p) {
		t.Fatal("restored the whole database")
	}
	sqlServerExec(t, db, "ALTER VIEW v AS SELECT id, name FROM users")
	if !restoreSQLServer(t, user, db, log, p) {
		t.Fatal("restored item by item")
	}
	if n := sqlServerInt(t, db, "SELECT COUNT(*) FROM sessions"); n != 2 {
		t.Fatalf("%d rows of the excluded table, want 2", n)
	}
	if err := user.Seed(ctx); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if err := user.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !narrow {
		return
	}
	other := sqlServerTestName()
	sqlServerExec(t, master, "CREATE DATABASE "+qms(other))
	t.Cleanup(func() { dropSQLServerDatabase(master, other) })
	u.RawQuery = "encrypt=" + u.Query().Get("encrypt") + "&TrustServerCertificate=" + u.Query().Get("TrustServerCertificate")
	as := sqlServerTestDB(t, u.String(), "")
	for _, q := range []string{"ALTER DATABASE " + qms(other) + " SET OFFLINE WITH ROLLBACK IMMEDIATE",
		"RESTORE DATABASE " + qms(other) + " FROM DISK = N'dg_none.bak' WITH REPLACE", "DROP DATABASE " + qms(other)} {
		if _, err := as.Exec(q); err == nil || !strings.Contains(err.Error(), "permission") {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// TestSQLServerSeededFingerprint: the record of a seed is the pristine
// copy's fingerprint, whatever the application wrote while the seed ran;
// a restore of the whole database then brings every table back to it, not
// only the items it was asked for. A pristine copy another seed replaced
// meanwhile is not restored from.
func TestSQLServerSeededFingerprint(t *testing.T) {
	ctx := context.Background()
	log := &ssLog{}
	s, db := newSQLServerTest(t, Options{Log: log})
	cleanSQLServerSeed(t, s)
	sqlServerExec(t, db, "CREATE TABLE jobs (id int IDENTITY PRIMARY KEY, at datetime2 DEFAULT SYSUTCDATETIME())", "CREATE TABLE b (id int PRIMARY KEY)")
	stop, done := make(chan struct{}), make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				done <- n
				return
			default:
			}
			if _, err := db.Exec("INSERT INTO jobs DEFAULT VALUES"); err == nil {
				n++
			}
		}
	}()
	err := s.Seed(ctx)
	close(stop)
	n := <-done
	if err != nil {
		t.Fatal(err)
	}
	record, err := s.SeededFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d := Diff(record, pristineSQLServer(t, s)); len(d) > 0 {
		t.Fatalf("the record differs from the pristine copy in %v", d)
	}
	if d := Diff(mustSQLServerFingerprint(t, s), record); n > 0 && !contains(d, "table:dbo.jobs") {
		t.Logf("%d inserts during the seed all made it into its backup", n)
	}
	sqlServerExec(t, db, "CREATE VIEW v AS SELECT id FROM b")
	if !restoreSQLServer(t, s, db, log, record) {
		t.Fatal("restored item by item")
	}
	id, err := s.PristineID(ctx)
	m, _ := s.readMark(ctx, s.o.Pristine)
	if err != nil || id == "" || id != m.Fallback {
		t.Fatalf("PristineID %q, %v; mark %+v", id, err, m)
	}
	// Another component seeds anew.
	other, err := NewSQLServer(s.o)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Restore(ctx, nil); err == nil || !strings.Contains(err.Error(), "replaced by another seed") {
		t.Fatalf("restore from a replaced pristine copy: %v", err)
	}
}

// TestSQLServerSeedLog: the pristine copy keeps none of the working
// database's log; a restore of the whole database gives the working
// database its log size back. The sizes are reported.
func TestSQLServerSeedLog(t *testing.T) {
	ctx := context.Background()
	s, db, size := newSQLServerLogTest(t, &ssLog{})
	start := time.Now()
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	m, _ := s.readMark(ctx, s.o.Pristine)
	c, _ := s.conn(ctx, "")
	bak, _ := s.fallbackFile(ctx, c, m)
	rows, err := c.QueryContext(ctx, "RESTORE HEADERONLY FROM DISK = "+sqlServerString(bak))
	if err != nil {
		t.Fatal(err)
	}
	header, err := ssColumns(rows)
	c.Close()
	if err != nil || len(header) != 1 {
		t.Fatal(err)
	}
	var full, compressed int64
	fmt.Sscan(ssText(header[0]["backupsize"]), &full)
	fmt.Sscan(ssText(header[0]["compressedbackupsize"]), &compressed)
	t.Logf("seed of %d MB data (40 MB of it excluded), log %d MB: %s; pristine data %d MB, log %d MB; backup %d MB (%d MB compressed)",
		size(s.o.Working, 0), size(s.o.Working, 1), took.Round(time.Millisecond), size(s.o.Pristine, 0), size(s.o.Pristine, 1), full>>20, compressed>>20)
	if l := size(s.o.Pristine, 1); l > 16 {
		t.Errorf("the pristine copy's log is %d MB", l)
	}
	sqlServerExec(t, db, "ALTER VIEW v AS SELECT id, pad FROM big")
	if err := s.Restore(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("after a restore of the whole database: working data %d MB, log %d MB", size(s.o.Working, 0), size(s.o.Working, 1))
	if l := size(s.o.Working, 1); l < 300 {
		t.Errorf("the working database's log is %d MB after the restore, not 300", l)
	}
}

// newSQLServerLogTest makes a working database of 136 MB of data, 40 MB
// of it in the excluded table audit, with a log of 300 MB, whose active
// part is not at the start of the file. size gives the MB of the data
// (typ 0) or log (typ 1) files of a database.
func newSQLServerLogTest(t *testing.T, log *ssLog) (s *SQLServer, db *sql.DB, size func(db string, typ int) int64) {
	t.Helper()
	s, db = newSQLServerTest(t, Options{Exclude: []string{"audit"}, Log: log})
	cleanSQLServerSeed(t, s)
	master := sqlServerTestDB(t, s.o.DSN, "")
	sqlServerExec(t, db, "CREATE TABLE big (id int IDENTITY PRIMARY KEY, pad char(2000) NOT NULL DEFAULT 'x')",
		"INSERT INTO big (pad) SELECT TOP (20000) 'y' FROM sys.all_objects a CROSS JOIN sys.all_objects b",
		"CREATE TABLE audit (id int IDENTITY PRIMARY KEY, pad char(2000) NOT NULL DEFAULT 'x')",
		"INSERT INTO audit (pad) SELECT TOP (20000) 'y' FROM sys.all_objects a CROSS JOIN sys.all_objects b",
		"CREATE VIEW v AS SELECT id FROM big")
	var logName string
	db.QueryRow("SELECT name FROM sys.database_files WHERE type = 1").Scan(&logName)
	sqlServerExec(t, master, "ALTER DATABASE "+qms(s.o.Working)+" MODIFY FILE (NAME = "+sqlServerString(logName)+", SIZE = 300MB)")
	return s, db, func(db string, typ int) int64 {
		return sqlServerInt(t, master, fmt.Sprintf("SELECT SUM(CAST(size AS bigint)) * 8 / 1024 FROM sys.master_files WHERE type = %d AND database_id = DB_ID(%s)", typ, sqlServerString(db)))
	}
}

// TestSQLServerSeedLogHeld: a transaction holds the active part of the new
// copy's log while the seed shrinks it, as a task of the server can, so
// the shrink stops at the end of the held part. Released once the seed is
// seen waiting to shrink again (WAITFOR), the shrink that runs again frees
// the rest. Held to the end of the seed (the rename ends it), the log
// stays and the seed says so.
func TestSQLServerSeedLogHeld(t *testing.T) {
	for _, release := range []string{"while the seed waits", "never"} {
		t.Run(release, func(t *testing.T) {
			ctx := context.Background()
			log := &ssLog{}
			s, _, size := newSQLServerLogTest(t, log)
			newDB := s.o.Pristine + "_dgnew"
			copyDB := sqlServerTestDB(t, s.o.DSN, newDB)
			done, stop := make(chan error, 1), make(chan struct{})
			defer func() { chaos.At = nil }()
			chaos.At = func(p string) {
				if p != "state.sqlserver.seed.restored" {
					return
				}
				hold, err := copyDB.Conn(ctx)
				if err == nil {
					_, err = hold.ExecContext(ctx, "BEGIN TRANSACTION; UPDATE big SET pad = 'z' WHERE id = 1")
				}
				if err != nil {
					t.Errorf("holding the log of %s: %v", newDB, err)
					return
				}
				go func() {
					defer hold.Close()
					if release == "never" {
						<-stop
						done <- nil
						return
					}
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					timeout := time.After(time.Minute)
					for {
						select {
						case <-tick.C:
							var n int
							if copyDB.QueryRow("SELECT COUNT(*) FROM sys.dm_exec_requests WHERE database_id = DB_ID() AND command = 'WAITFOR' AND session_id <> @@SPID").Scan(&n) == nil && n > 0 {
								_, err := hold.ExecContext(ctx, "ROLLBACK")
								done <- err
								return
							}
						case <-timeout:
							done <- errors.New("the seed did not wait to shrink the log of the new copy again within a minute")
							return
						case <-stop:
							done <- errors.New("the seed ended without waiting to shrink the log of the new copy again")
							return
						}
					}
				}()
			}
			err := s.Seed(ctx)
			close(stop)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			l := size(s.o.Pristine, 1)
			noted := strings.Contains(log.String(), "the log of the pristine copy stays")
			t.Logf("released %s: the pristine copy's log is %d MB; log: %q", release, l, log.String())
			if held := release == "never"; held != (l > 16) || held != noted {
				t.Errorf("the pristine copy's log is %d MB (noted: %v)", l, noted)
			}
		})
	}
}

// BenchmarkSQLServerSeed: a seed of a small database (reported only).
func BenchmarkSQLServerSeed(b *testing.B) {
	s, _, _ := newSQLServerSeeded(b, Options{Exclude: []string{"sessions"}},
		"CREATE TABLE t (id int IDENTITY PRIMARY KEY, v int NULL)", "INSERT INTO t (v) SELECT TOP (1000) object_id FROM sys.all_objects",
		"CREATE TABLE sessions (id int PRIMARY KEY)", "INSERT INTO sessions VALUES (1)")
	start, n := time.Now(), 0
	for b.Loop() { // the setup runs once, also with -benchtime Nx
		n++
		if err := s.Seed(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(time.Since(start).Microseconds())/1000/float64(n), "ms/seed")
}
