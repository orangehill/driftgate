package capture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// sqliteTestURI is a file: URI of path for the "sqlite" driver the state
// package registers (the test does not import a driver of its own).
func sqliteTestURI(path string, params ...string) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" {
		p = "/" + p
	}
	p = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
	uri := "file://" + p
	if len(params) > 0 {
		uri += "?" + strings.Join(params, "&")
	}
	return uri
}

// sqliteApp is an application on an SQLite file: one connection it keeps
// open for its whole life, as PHP-FPM, Rails or a Node server does, and a
// session row written on every request (Laravel's database session
// driver), in a table the configuration excludes.
type sqliteApp struct {
	db *sql.DB
	mu sync.Mutex
	// served is the last response body of every request, by method and
	// path.
	served map[string]string
}

func newSQLiteApp(t *testing.T, file, mode string) *sqliteApp {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteTestURI(file, "_pragma=busy_timeout(10000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		"PRAGMA journal_mode = " + mode,
		"CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL UNIQUE)",
		"CREATE TABLE IF NOT EXISTS sessions (id INTEGER PRIMARY KEY, path TEXT)",
		"INSERT OR IGNORE INTO users (email) VALUES ('a@x'), ('b@x')",
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return &sqliteApp{db: db, served: map[string]string{}}
}

func (a *sqliteApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.db.Exec("INSERT INTO sessions (path) VALUES (?)", r.URL.Path); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	status, body := 200, ""
	switch {
	case r.Method == "GET" && r.URL.Path == "/users":
		rows, err := a.db.Query("SELECT id, email FROM users ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var users []string
		for rows.Next() {
			var id int
			var email string
			rows.Scan(&id, &email)
			users = append(users, fmt.Sprintf("%d:%s", id, email))
		}
		rows.Close()
		body = fmt.Sprintf(`{"users":%q}`, strings.Join(users, ","))
	case r.Method == "POST" && r.URL.Path == "/users":
		var in struct{ Email string }
		json.NewDecoder(r.Body).Decode(&in)
		res, err := a.db.Exec("INSERT INTO users (email) VALUES (?)", in.Email)
		if err != nil {
			status, body = 409, `{"error":"taken"}`
			break
		}
		id, _ := res.LastInsertId()
		var n int
		a.db.QueryRow("SELECT count(*) FROM users").Scan(&n)
		status, body = 201, fmt.Sprintf(`{"id":%d,"count":%d}`, id, n)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/users/"):
		res, err := a.db.Exec("DELETE FROM users WHERE id = ?", strings.TrimPrefix(r.URL.Path, "/users/"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		n, _ := res.RowsAffected()
		body = fmt.Sprintf(`{"deleted":%d}`, n)
	default:
		status, body = 404, `{}`
	}
	a.served[r.Method+" "+r.URL.Path] = fmt.Sprintf("%d %s", status, body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

func (a *sqliteApp) count(t *testing.T, table string) int {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int
	if err := a.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// sqliteCatalog reads the users, adds one twice (only a restore in between
// lets the second succeed, with the same id), and deletes one.
func sqliteCatalog() *catalog.Catalog {
	post := json.RawMessage(`{"email":"c@x"}`)
	return &catalog.Catalog{Version: 1, Requests: []catalog.Request{
		{ID: "list", Method: "GET", Path: "/users", Storage: snapshot.Public},
		{ID: "add", Method: "POST", Path: "/users", Body: post, Headers: [][2]string{{"Content-Type", "application/json"}}, Storage: snapshot.Public},
		{ID: "add-again", Method: "POST", Path: "/users", Body: post, Headers: [][2]string{{"Content-Type", "application/json"}}, Storage: snapshot.Public},
		{ID: "remove", Method: "DELETE", Path: "/users/1", Storage: snapshot.Public},
	}}
}

// TestCaptureSQLite: a capture against an application that writes its
// SQLite file through a connection it keeps open, in WAL and in rollback
// mode. Restores between the requests that change data reach that
// connection (the second POST of the same user gets the same id: the rows
// and the AUTOINCREMENT counter came back), every restore is checked
// against the pristine fingerprint, the responses and side effects of two
// captures are the same, and the excluded sessions table keeps every row
// the application wrote. A pristine copy changed behind DriftGate's back
// makes the next restore fail its check instead of passing.
func TestCaptureSQLite(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
			root := t.TempDir()
			file := filepath.Join(root, "database", "app.sqlite")
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			app := newSQLiteApp(t, file, mode)
			srv := httptest.NewServer(app)
			t.Cleanup(srv.Close)
			p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
				Target: config.Target{BaseURL: srv.URL},
				State: map[string]config.Component{"db": {Driver: config.SQLite, Working: "database/app.sqlite",
					Exclude: []string{"sessions", "job_batches"}, RowTracking: []string{"users"}}}}, root)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var out bytes.Buffer
			if err := Seed(ctx, p, SeedOptions{Out: &out}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "pristine copy of db: ") {
				t.Fatalf("seed output:\n%s", out.String())
			}

			runs := map[string]map[string]snapshot.Record{}
			served := map[string]map[string]string{}
			for _, tag := range []string{"a", "b"} {
				out.Reset()
				sum, err := Run(ctx, p, sqliteCatalog(), Options{Tag: tag, Restore: "each", Out: &out})
				if err != nil {
					t.Fatalf("capture %s: %v\n%s", tag, err, out.String())
				}
				if !sum.Tracked || len(sum.Failed) > 0 {
					t.Fatalf("capture %s: %+v", tag, sum)
				}
				// The restores before add-again and remove (and the one at
				// the end) were checked: a failed check is an error.
				if n := strings.Count(out.String(), "restored db in"); n < 3 {
					t.Fatalf("capture %s: %d checked restores, want 3 or more:\n%s", tag, n, out.String())
				}
				set, err := p.Capture(tag)
				if err != nil {
					t.Fatal(err)
				}
				runs[tag] = map[string]snapshot.Record{}
				for _, r := range sqliteCatalog().Requests {
					rec, err := set.Record(r.ID)
					if err != nil || rec == nil {
						t.Fatalf("%s %s: %v", tag, r.ID, err)
					}
					runs[tag][r.ID] = *rec
				}
				app.mu.Lock()
				served[tag] = map[string]string{}
				for k, v := range app.served {
					served[tag][k] = v
				}
				app.mu.Unlock()
			}
			for id, a := range runs["a"] {
				b := runs["b"][id]
				a.Headers, b.Headers = nil, nil // the date header moves on
				ja, _ := json.Marshal(a)
				jb, _ := json.Marshal(b)
				if !bytes.Equal(ja, jb) {
					t.Errorf("%s differs between the captures:\n%s\n%s", id, ja, jb)
				}
			}
			if add := runs["a"]["add-again"]; add.Status != 201 || add.Body.SHA256 != runs["a"]["add"].Body.SHA256 {
				t.Errorf("add-again: %d, body like add: %v (no restore before it?)", add.Status, add.Body.SHA256 == runs["a"]["add"].Body.SHA256)
			}
			if got := served["a"]["POST /users"]; got != `201 {"id":3,"count":3}` {
				t.Errorf("POST /users answered %s, want id 3 of 3 users", got)
			}
			se := runs["a"]["add"].SideEffects
			if se == nil || strings.Join(se.Items, ",") != "db/table:users" || len(se.Rows) != 1 {
				t.Errorf("side effects of add: %+v", se)
			}
			if se := runs["a"]["list"].SideEffects; se != nil && len(se.Items) > 0 {
				t.Errorf("the session write of a GET is a side effect: %+v", se)
			}
			// The captures leave the data as seeded; the sessions table, never
			// restored, has a row for every request the application got.
			if n := app.count(t, "users"); n != 2 {
				t.Errorf("%d users after the captures, want the 2 seeded", n)
			}
			if n := app.count(t, "sessions"); n < 8 {
				t.Errorf("%d session rows, want one per request (8 or more)", n)
			}

			// A pristine copy changed behind DriftGate's back: the restore
			// copies it, its check against the recorded fingerprint fails.
			server, err := serverOf(p, p.State["db"], "")
			if err != nil {
				t.Fatal(err)
			}
			record, err := pristinePath(p, "db", server)
			if err != nil {
				t.Fatal(err)
			}
			pristine, err := sql.Open("sqlite", sqliteTestURI(sqliteStorePristine(record), "mode=rw"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pristine.Exec("UPDATE users SET email = 'tampered' WHERE id = 2"); err != nil {
				t.Fatal(err)
			}
			pristine.Close()
			out.Reset()
			_, err = Run(ctx, p, sqliteCatalog(), Options{Tag: "c", Restore: "each", Out: &out})
			if err == nil || !strings.Contains(err.Error(), "restore of db left 1 items different from the fingerprint driftgate seed recorded of the pristine copy: [table:users]") {
				t.Fatalf("capture on a changed pristine copy: %v\n%s", err, out.String())
			}
		})
	}
}

// TestSeedSQLiteWhileWriting: the application writes after the seed made
// its copy and before it recorded it. The record is of the copy, so the
// first restore brings the working file back to it and the capture runs;
// a record of the working file then would hold the application's row and
// fail every restore until the next seed.
func TestSeedSQLiteWhileWriting(t *testing.T) {
	t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
	root := t.TempDir()
	file := filepath.Join(root, "database", "app.sqlite")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	app := newSQLiteApp(t, file, "wal")
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	p, err := config.New(config.Config{Version: 1, Project: "p", Mode: config.Freeze, Level: "values",
		Target: config.Target{BaseURL: srv.URL},
		State:  map[string]config.Component{"db": {Driver: config.SQLite, Working: "database/app.sqlite", Exclude: []string{"sessions"}}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	chaos.At = func(point string) {
		if point == "state.sqlite.seed" {
			app.mu.Lock()
			defer app.mu.Unlock()
			if _, err := app.db.Exec("INSERT INTO users (email) VALUES ('written-during-seed@x')"); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { chaos.At = nil })
	ctx := context.Background()
	if err := Seed(ctx, p, SeedOptions{}); err != nil {
		t.Fatal(err)
	}
	chaos.At = nil
	if n := app.count(t, "users"); n != 3 {
		t.Fatalf("%d users after the seed, want the 2 copied and the one written during it", n)
	}
	if _, err := Run(ctx, p, sqliteCatalog(), Options{Tag: "a", Restore: "each"}); err != nil {
		t.Fatalf("capture after a seed the application wrote during: %v", err)
	}
	if n := app.count(t, "users"); n != 2 {
		t.Errorf("%d users after the capture, want the 2 of the pristine copy", n)
	}
}
