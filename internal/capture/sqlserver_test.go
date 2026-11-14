package capture

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// The SQL Server tests take the server from DRIFTGATE_TEST_SQLSERVER, a
// sqlserver:// URL of a login that may create databases (sa in CI), and
// reach it through the "sqlserver" driver the state package registers.
// Their databases are named dgt_cap_*.

// sqlServerCapture is a database of a test's own on the test server, with
// the DSN DriftGate reads from the environment (DG_TEST_SQLSERVER); it is
// dropped at the end with what a seed and a restore of it leave.
type sqlServerCapture struct {
	dsn, work string
	master    *sql.DB
}

func newSQLServerCapture(t *testing.T) *sqlServerCapture {
	t.Helper()
	dsn := os.Getenv("DRIFTGATE_TEST_SQLSERVER")
	if dsn == "" {
		t.Skip("DRIFTGATE_TEST_SQLSERVER not set")
	}
	b := make([]byte, 4)
	rand.Read(b)
	s := &sqlServerCapture{dsn: dsn, work: "dgt_cap_" + hex.EncodeToString(b)}
	s.master = s.open(t, "master")
	if _, err := s.master.Exec("CREATE DATABASE [" + s.work + "]"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The backup files a seed wrote, as the server's history knows
		// them, then the databases.
		rows, err := s.master.Query(`SELECT DISTINCT f.physical_device_name FROM msdb.dbo.backupset b
			JOIN msdb.dbo.backupmediafamily f ON f.media_set_id = b.media_set_id WHERE b.database_name LIKE @p1`, s.work+"%")
		if err == nil {
			var files []string
			for rows.Next() {
				var f string
				if rows.Scan(&f) == nil {
					files = append(files, f)
				}
			}
			rows.Close()
			for _, f := range files {
				s.master.Exec("EXEC master.sys.xp_delete_file 0, N'" + strings.ReplaceAll(f, "'", "''") + "'")
			}
		}
		for _, db := range []string{s.work, s.work + "_pristine", s.work + "_pristine_dgnew", s.work + "_pristine_dgx"} {
			s.master.Exec("IF DB_ID(N'" + db + "') IS NOT NULL BEGIN ALTER DATABASE [" + db + "] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [" + db + "] END")
		}
	})
	t.Setenv("DG_TEST_SQLSERVER", dsn)
	return s
}

// open opens database db of the test server.
func (s *sqlServerCapture) open(t *testing.T, db string) *sql.DB {
	t.Helper()
	u, err := url.Parse(s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", db)
	u.RawQuery = q.Encode()
	state.AllowNegativeSerial() // as NewSQLServer: SQL Server 2017's own certificate
	d, err := sql.Open("sqlserver", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// project is a project on the application at url with the test database
// as its state db, its sessions table excluded.
func (s *sqlServerCapture) project(t *testing.T, url string) *config.Project {
	t.Helper()
	t.Setenv(secret.EnvKey, "c2VjcmV0LWtleS1mb3ItYS10ZXN0LW9ubHktMDAwMDA=") // row_tracking names row values with the key; never the Keychain
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: url},
		State: map[string]config.Component{"db": {Driver: config.SQLServer, DSN: config.Secret{Env: "DG_TEST_SQLSERVER"},
			Working: s.work, Pristine: s.work + "_pristine", Exclude: []string{"sessions"}, RowTracking: config.Tables("users")}}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sqlServerApp is an application on a SQL Server database: a pool of
// connections it keeps open for its whole life, as ASP.NET's or a Go
// server's does, and a session row written on every request, in a table
// the configuration excludes.
type sqlServerApp struct {
	db *sql.DB
	mu sync.Mutex
	// served is the last response of every request, by method and path;
	// errs are the errors of the requests the database failed.
	served map[string]string
	errs   []string
}

func newSQLServerApp(t *testing.T, db *sql.DB) *sqlServerApp {
	t.Helper()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	for _, s := range []string{
		"CREATE TABLE users (id int IDENTITY PRIMARY KEY, email nvarchar(100) NOT NULL UNIQUE)",
		"CREATE TABLE sessions (id int IDENTITY PRIMARY KEY, path nvarchar(200))",
		"INSERT INTO users (email) VALUES (N'a@x'), (N'b@x')",
	} {
		if err := state.RetryInTransition(func() error { _, err := db.Exec(s); return err }); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return &sqlServerApp{db: db, served: map[string]string{}}
}

func (a *sqlServerApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fail := func(err error) {
		a.errs = append(a.errs, r.Method+" "+r.URL.Path+": "+err.Error())
		http.Error(w, err.Error(), 500)
	}
	// On SQL Server 2019 and earlier another test's RESTORE may stop a
	// statement (927, 952): the application tries it again, so a capture
	// does not see a 500 that no change of DriftGate's made.
	retry := state.RetryInTransition
	if err := retry(func() error { _, err := a.db.Exec("INSERT INTO sessions (path) VALUES (@p1)", r.URL.Path); return err }); err != nil {
		fail(err)
		return
	}
	status, body := 200, ""
	switch {
	case r.Method == "GET" && r.URL.Path == "/users":
		var users []string
		err := retry(func() error {
			users = nil
			rows, err := a.db.Query("SELECT id, email FROM users ORDER BY id")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int
				var email string
				rows.Scan(&id, &email)
				users = append(users, fmt.Sprintf("%d:%s", id, email))
			}
			return rows.Err()
		})
		if err != nil {
			fail(err)
			return
		}
		body = fmt.Sprintf(`{"users":%q}`, strings.Join(users, ","))
	case r.Method == "POST" && r.URL.Path == "/users":
		var in struct{ Email string }
		json.NewDecoder(r.Body).Decode(&in)
		var id, n int
		if err := retry(func() error {
			return a.db.QueryRow("INSERT INTO users (email) OUTPUT INSERTED.id VALUES (@p1)", in.Email).Scan(&id)
		}); err != nil {
			status, body = 409, `{"error":"taken"}`
			break
		}
		retry(func() error { return a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n) })
		status, body = 201, fmt.Sprintf(`{"id":%d,"count":%d}`, id, n)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/users/"):
		var res sql.Result
		err := retry(func() (err error) {
			res, err = a.db.Exec("DELETE FROM users WHERE id = @p1", strings.TrimPrefix(r.URL.Path, "/users/"))
			return err
		})
		if err != nil {
			fail(err)
			return
		}
		n, _ := res.RowsAffected()
		body = fmt.Sprintf(`{"deleted":%d}`, n)
	case r.Method == "POST" && r.URL.Path == "/report":
		// A migration of the application's: an object a restore of the
		// tables cannot bring back, so the whole database is restored.
		if err := retry(func() error {
			_, err := a.db.Exec("CREATE VIEW user_report AS SELECT COUNT(*) AS n FROM dbo.users")
			return err
		}); err != nil {
			fail(err)
			return
		}
		status, body = 201, `{}`
	default:
		status, body = 404, `{}`
	}
	a.served[r.Method+" "+r.URL.Path] = fmt.Sprintf("%d %s", status, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (a *sqlServerApp) count(t *testing.T, table string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int
	if err := state.RetryInTransition(func() error { return a.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

// sqlServerCatalog reads the users, adds one twice (only a restore in
// between lets the second succeed, with the same id), and deletes one;
// with report, the application then makes a view, adds the user twice
// (a restore before each) and reads the users.
func sqlServerCatalog(report bool) *catalog.Catalog {
	post := json.RawMessage(`{"email":"c@x"}`)
	json := [][2]string{{"Content-Type", "application/json"}}
	c := &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "list", Method: "GET", Path: "/users", Storage: snapshot.Public},
		{ID: "add", Method: "POST", Path: "/users", Body: post, Headers: json, Storage: snapshot.Public},
		{ID: "add-again", Method: "POST", Path: "/users", Body: post, Headers: json, Storage: snapshot.Public},
		{ID: "remove", Method: "DELETE", Path: "/users/1", Storage: snapshot.Public},
	}}
	if report {
		c.Requests = append(c.Requests,
			catalog.Request{ID: "report", Method: "POST", Path: "/report", Storage: snapshot.Public},
			catalog.Request{ID: "after-report", Method: "POST", Path: "/users", Body: post, Headers: json, Storage: snapshot.Public},
			catalog.Request{ID: "add-after-report", Method: "POST", Path: "/users", Body: post, Headers: json, Storage: snapshot.Public},
			catalog.Request{ID: "list-after-report", Method: "GET", Path: "/users", Storage: snapshot.Public})
	}
	return c
}

// captureSQLServer runs a capture and returns its records by request.
func captureSQLServer(t *testing.T, p *config.Project, cat *catalog.Catalog, tag string, out *bytes.Buffer) map[string]snapshot.Record {
	t.Helper()
	out.Reset()
	sum, err := Run(context.Background(), p, cat, Options{Tag: tag, Restore: "each", Out: out, Log: out})
	if err != nil {
		t.Fatalf("capture %s: %v\n%s", tag, err, out.String())
	}
	if !sum.Tracked || len(sum.Failed) > 0 {
		t.Fatalf("capture %s: %+v\n%s", tag, sum, out.String())
	}
	set, err := p.Capture(tag)
	if err != nil {
		t.Fatal(err)
	}
	recs := map[string]snapshot.Record{}
	for _, r := range cat.Requests {
		rec, err := set.Record(r.ID)
		if err != nil || rec == nil {
			t.Fatalf("%s %s: %v", tag, r.ID, err)
		}
		recs[r.ID] = *rec
	}
	return recs
}

// sameRecords fails the test when two captures differ in anything but
// their headers (the date moves on).
func sameRecords(t *testing.T, a, b map[string]snapshot.Record) {
	t.Helper()
	for id, ra := range a {
		rb := b[id]
		ra.Headers, rb.Headers = nil, nil
		ja, _ := json.Marshal(ra)
		jb, _ := json.Marshal(rb)
		if !bytes.Equal(ja, jb) {
			t.Errorf("%s differs between the captures:\n%s\n%s", id, ja, jb)
		}
	}
}

// TestCaptureSQLServer: a capture against an application that writes its
// SQL Server database through a pool of connections it keeps open.
// Restores between the requests reach those connections (the second POST
// of the same user gets the same id: the rows and the identity came
// back), every restore is checked against the pristine fingerprint, two
// captures are the same, and the excluded sessions table keeps every row
// the application wrote. A pristine copy changed behind DriftGate's back
// makes the next restore fail its check instead of passing.
func TestCaptureSQLServer(t *testing.T) {
	s := newSQLServerCapture(t)
	app := newSQLServerApp(t, s.open(t, s.work))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p := s.project(t, srv.URL)
	var out bytes.Buffer
	if err := Seed(context.Background(), p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}

	runs := map[string]map[string]snapshot.Record{}
	served := map[string]map[string]string{}
	for _, tag := range []string{"a", "b"} {
		runs[tag] = captureSQLServer(t, p, sqlServerCatalog(false), tag, &out)
		// The restores before add-again and remove (and the one at the
		// end) were checked: a failed check is an error.
		if n := strings.Count(out.String(), "restored db in"); n < 3 {
			t.Fatalf("capture %s: %d checked restores, want 3 or more:\n%s", tag, n, out.String())
		}
		if strings.Contains(out.String(), "restoring the whole database") {
			t.Errorf("capture %s restored the whole database for rows:\n%s", tag, out.String())
		}
		app.mu.Lock()
		served[tag] = map[string]string{}
		for k, v := range app.served {
			served[tag][k] = v
		}
		app.mu.Unlock()
	}
	sameRecords(t, runs["a"], runs["b"])
	if add := runs["a"]["add-again"]; add.Status != 201 || add.Body.SHA256 != runs["a"]["add"].Body.SHA256 {
		t.Errorf("add-again: %d, body like add: %v (no restore before it?)", add.Status, add.Body.SHA256 == runs["a"]["add"].Body.SHA256)
	}
	if got := served["a"]["POST /users"]; got != `201 {"id":3,"count":3}` {
		t.Errorf("POST /users answered %s, want id 3 of 3 users", got)
	}
	se := runs["a"]["add"].SideEffects
	if se == nil || strings.Join(se.Items, ",") != "db/table:dbo.users" || len(se.Rows) != 1 {
		t.Errorf("side effects of add: %+v", se)
	}
	if se := runs["a"]["list"].SideEffects; se != nil && len(se.Items) > 0 {
		t.Errorf("the session write of a GET is a side effect: %+v", se)
	}
	if n := app.count(t, "users"); n != 2 {
		t.Errorf("%d users after the captures, want the 2 seeded", n)
	}
	if n := app.count(t, "sessions"); n < 8 {
		t.Errorf("%d session rows, want one per request (8 or more)", n)
	}
	if len(app.errs) > 0 {
		t.Errorf("requests of the application failed on its database: %q", app.errs)
	}

	// A pristine copy changed behind DriftGate's back: the restore copies
	// it, its check against the recorded fingerprint fails.
	pristine := "[" + s.work + "_pristine]"
	for _, q := range []string{"ALTER DATABASE " + pristine + " SET READ_WRITE WITH ROLLBACK IMMEDIATE",
		"UPDATE " + pristine + ".dbo.users SET email = N'tampered' WHERE id = 2",
		"ALTER DATABASE " + pristine + " SET READ_ONLY WITH ROLLBACK IMMEDIATE"} {
		if _, err := s.master.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	out.Reset()
	_, err := Run(context.Background(), p, sqlServerCatalog(false), Options{Tag: "c", Restore: "each", Out: &out})
	if err == nil || !strings.Contains(err.Error(), "restore of db left 1 items different from the fingerprint driftgate seed recorded of the pristine copy: [table:dbo.users]") {
		t.Fatalf("capture on a changed pristine copy: %v\n%s", err, out.String())
	}
}

// TestCaptureSQLServerWholeDatabase: the application makes a view, which a
// restore of the tables cannot take back: the next restore restores the
// whole database from the pristine copy's backup, which ends the
// application's connections. The first request on each connection of its
// pool then fails, as with any client when its server ends a session
// (SqlClient, go-mssqldb); the pool connects again, and the requests after
// it succeed. The view is gone, the excluded table keeps its rows, and two
// captures are the same.
func TestCaptureSQLServerWholeDatabase(t *testing.T) {
	s := newSQLServerCapture(t)
	app := newSQLServerApp(t, s.open(t, s.work))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p := s.project(t, srv.URL)
	var out bytes.Buffer
	if err := Seed(context.Background(), p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	runs := map[string]map[string]snapshot.Record{}
	sessions := 0
	for _, tag := range []string{"a", "b"} {
		app.mu.Lock()
		app.errs = nil
		app.mu.Unlock()
		runs[tag] = captureSQLServer(t, p, sqlServerCatalog(true), tag, &out)
		if !strings.Contains(out.String(), "restoring the whole database") {
			t.Errorf("capture %s: no restore of the whole database after the view:\n%s", tag, out.String())
		}
		// Only the first request after the restore (after-report) may
		// fail, on the connection the restore ended.
		if len(app.errs) > 1 || len(app.errs) == 1 && !strings.HasPrefix(app.errs[0], "POST /users: ") {
			t.Errorf("capture %s: the application failed: %q", tag, app.errs)
		}
		if r := runs[tag]["report"]; r.Status != 201 {
			t.Errorf("capture %s: report answered %d", tag, r.Status)
		}
		for _, id := range []string{"add-after-report", "list-after-report"} {
			if r := runs[tag][id]; r.Status >= 500 {
				t.Errorf("capture %s: %s answered %d: the application's pool did not connect again", tag, id, r.Status)
			}
		}
		var view sql.NullInt64
		if err := s.open(t, s.work).QueryRow("SELECT OBJECT_ID(N'dbo.user_report')").Scan(&view); err != nil || view.Valid {
			t.Errorf("capture %s left the view of the application (%v)", tag, err)
		}
		n := app.count(t, "sessions")
		if n <= sessions {
			t.Errorf("capture %s: %d session rows, want more than the %d before (excluded: never restored)", tag, n, sessions)
		}
		sessions = n
	}
	// The request on an ended connection fails with that connection's
	// error, whose text may differ; the rest is the same.
	delete(runs["a"], "after-report")
	sameRecords(t, runs["a"], runs["b"])
	if add := runs["a"]["add-after-report"]; add.Status != 201 || add.Body.SHA256 != runs["a"]["add"].Body.SHA256 {
		t.Errorf("add after the view: %d, body like add: %v", add.Status, add.Body.SHA256 == runs["a"]["add"].Body.SHA256)
	}
}

// TestSeedSQLServerWhileWriting: the application writes right after the
// seed's backup; the record is of the pristine copy (SeededFingerprint),
// not of the working database the write moved on, so the capture's
// restores pass their check and bring back the copy's rows.
func TestSeedSQLServerWhileWriting(t *testing.T) {
	s := newSQLServerCapture(t)
	app := newSQLServerApp(t, s.open(t, s.work))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p := s.project(t, srv.URL)
	chaos.At = func(point string) {
		if point == "state.sqlserver.seed.backup" {
			app.mu.Lock()
			defer app.mu.Unlock()
			if _, err := app.db.Exec("INSERT INTO users (email) VALUES (N'written-during-seed@x')"); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { chaos.At = nil })
	ctx := context.Background()
	var out bytes.Buffer
	if err := Seed(ctx, p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	chaos.At = nil
	if n := app.count(t, "users"); n != 3 {
		t.Fatalf("%d users after the seed, want the 2 copied and the one written during it", n)
	}
	captureSQLServer(t, p, sqlServerCatalog(false), "a", &out)
	if n := app.count(t, "users"); n != 2 {
		t.Errorf("%d users after the capture, want the 2 of the pristine copy", n)
	}
}

// TestSQLServerPristineReplaced: the record of a seed names the pristine
// copy it is of. A seed that keeps its record in another store (another
// checkout on the same databases) replaces the copy; a capture with the
// first record then stops and asks for a seed instead of trusting a
// fingerprint of a copy that is gone. A record without the name (an
// earlier release) is used as it is.
func TestSQLServerPristineReplaced(t *testing.T) {
	s := newSQLServerCapture(t)
	app := newSQLServerApp(t, s.open(t, s.work))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	var out bytes.Buffer
	p := s.project(t, srv.URL)
	if err := Seed(ctx, p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	matches, _ := filepath.Glob(filepath.Join(p.Layout.Pristine, "db-*.json"))
	if len(matches) != 1 {
		t.Fatalf("records %v, want one", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var pf pristineFile
	if err := json.Unmarshal(raw, &pf); err != nil || pf.PristineID == "" {
		t.Fatalf("record without the pristine copy's name (%v): %s", err, raw)
	}
	var mark string
	if err := s.master.QueryRow("SELECT CAST(value AS nvarchar(4000)) FROM [" + s.work + "_pristine].sys.extended_properties WHERE class = 0 AND name = N'driftgate_pristine'").Scan(&mark); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(mark), strings.ToLower(pf.PristineID)) {
		t.Fatalf("record names %s, the mark is %s", pf.PristineID, mark)
	}

	other := s.project(t, srv.URL) // its own store
	if err := Seed(ctx, other, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed of the other checkout: %v\n%s", err, out.String())
	}
	out.Reset()
	_, err = Run(ctx, p, sqlServerCatalog(false), Options{Tag: "a", Restore: "each", Out: &out})
	if err == nil || !strings.Contains(err.Error(), "the pristine copy is not the one driftgate seed recorded here: another seed replaced it") ||
		!strings.Contains(err.Error(), "run driftgate seed") {
		t.Fatalf("capture after another seed replaced the pristine copy: %v\n%s", err, out.String())
	}

	// The record of an earlier release has no name: no check.
	pf.PristineID = ""
	if err := snapshot.WriteJSON(matches[0], pf); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(matches[0]); strings.Contains(string(raw), "pristine_id") {
		t.Fatalf("an empty name is written: %s", raw)
	}
	captureSQLServer(t, p, sqlServerCatalog(false), "b", &out)

	// A seed with the first store records the copy anew.
	if err := Seed(ctx, p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed again: %v\n%s", err, out.String())
	}
	captureSQLServer(t, p, sqlServerCatalog(false), "c", &out)
}

// TestPrepareThatWritesSQLServer: on SQL Server, a prepare that writes
// through the application (a row in users) is named with the table; one
// that writes only to the excluded sessions table is not, and neither
// takes a fingerprint more. With a tokens command, the one fingerprint
// more between them is reused when nothing moved.
func TestPrepareThatWritesSQLServer(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not found")
	}
	s := newSQLServerCapture(t)
	app := newSQLServerApp(t, s.open(t, s.work))
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p := s.project(t, srv.URL)
	var out bytes.Buffer
	if err := Seed(context.Background(), p, SeedOptions{Out: &out}); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	run := func(tokens bool, prepare ...string) *Summary {
		t.Helper()
		return capturePrepared(t, p, sqlServerCatalog(false), tokens, &out, prepare...)
	}
	clean := run(false, "true")
	for _, c := range []struct {
		name    string
		prepare []string
		want    string
	}{
		{"sessions only", []string{curl, "-sf", srv.URL + "/users"}, ""},
		{"a user", []string{curl, "-sf", "-X", "POST", "-H", "Content-Type: application/json", "-d", `{"email":"p@x"}`, srv.URL + "/users"}, "db/table:dbo.users"},
	} {
		sum := run(false, c.prepare...)
		if got := strings.Join(sum.Prepared, ","); got != c.want || sum.Time.Fingerprints != clean.Time.Fingerprints {
			t.Errorf("%s: prepared %q, want %q; %d fingerprints, %d with a prepare that writes nothing", c.name, got, c.want, sum.Time.Fingerprints, clean.Time.Fingerprints)
		}
	}
	if len(clean.Prepared) != 0 {
		t.Errorf("a prepare that writes nothing: %v", clean.Prepared)
	}
	if both := run(true, "true"); len(both.Prepared) != 0 || both.Time.Fingerprints != clean.Time.Fingerprints+1 {
		t.Errorf("with a tokens command: prepared %v, %d fingerprints, %d without", both.Prepared, both.Time.Fingerprints, clean.Time.Fingerprints)
	}
	// A restore of the whole database (after the application's view) ends
	// the application's sessions; prepare, run after it, is not blamed.
	if sum := capturePrepared(t, p, sqlServerCatalog(true), false, &out, "true"); len(sum.Prepared) != 0 || !strings.Contains(out.String(), "restoring the whole database") {
		t.Errorf("after a restore of the whole database: prepared %v\n%s", sum.Prepared, out.String())
	}
}

// capturePrepared captures cat with prepare as the only prepare command,
// and with a tokens command that writes nothing when tokens is set.
func capturePrepared(t *testing.T, p *config.Project, cat *catalog.Catalog, tokens bool, out *bytes.Buffer, prepare ...string) *Summary {
	t.Helper()
	p.Prepare = []config.Command{{Run: prepare}}
	p.Tokens.Command = nil
	if tokens {
		p.Tokens.Command = []string{"echo", "{}"}
	}
	out.Reset()
	sum, err := Run(context.Background(), p, cat, Options{Tag: "c", Force: true, Restore: "each", Out: out, Log: out})
	if err != nil {
		t.Fatalf("capture: %v\n%s", err, out.String())
	}
	return sum
}
