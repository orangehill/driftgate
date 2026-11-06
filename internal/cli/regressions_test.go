package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/report"
)

// Regressions a framework upgrade brought into a real API (Laravel 5.5 to
// 13, PHP 7 to 8), each rebuilt as a pair of synthetic servers, the API
// before and after. Each test checks that DriftGate reports the drift, as
// the kind and place a reviewer reads, not only that something differs.

// regression captures the catalog from before, then from after, and
// compares them (after a seed, when extra configures a state:); it returns
// the exit code of the comparison and the drifts
// by request. A capture of after that fails (a request failed: exit 3)
// sets failed; the comparison then may stop too (exit 3, no drifts).
func regression(t *testing.T, catalog, allow, extra string, before, after http.HandlerFunc) (int, map[string][]compare.Drift) {
	code, drifts, _ := regressionFailed(t, catalog, allow, extra, before, after)
	return code, drifts
}

func regressionFailed(t *testing.T, catalog, allow, extra string, before, after http.HandlerFunc) (int, map[string][]compare.Drift, bool) {
	t.Helper()
	var current atomic.Pointer[http.HandlerFunc]
	current.Store(&before)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*current.Load())(w, r) }))
	defer srv.Close()
	root := t.TempDir()
	cfg := filepath.Join(root, "driftgate.yaml")
	write(t, cfg, fmt.Sprintf("version: 1\nproject: regression\nmode: freeze\nlevel: values\ntarget:\n  base_url: %s\n%s", srv.URL, extra))
	write(t, filepath.Join(root, "driftgate", "catalog.json"), catalog)
	write(t, filepath.Join(root, "driftgate", "allowlist.yaml"), "version: 1\nheaders:\n  - name: date\n    format: '.+'\n    reason: time\n"+allow)
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	if strings.HasPrefix(extra, "state:") {
		if code, out, errOut := runCLI(t, "seed", "--config", cfg); code != ExitClean {
			t.Fatalf("seed: %d\n%s\n%s", code, out, errOut)
		}
	}
	failed := false
	for _, tag := range []string{"before", "after"} {
		code, out, errOut := runCLI(t, "capture", tag, "--config", cfg)
		if failed = code == ExitEnv && tag == "after" && strings.Contains(errOut, "requests failed (connection, timeout"); code != ExitClean && !failed {
			t.Fatalf("capture %s: %d\n%s\n%s", tag, code, out, errOut)
		}
		current.Store(&after)
	}
	code, out, errOut := runCLI(t, "compare", "after", "--against", "before", "--format", "json", "--config", cfg)
	var r report.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil && failed && code == ExitEnv {
		return code, nil, failed // the failed request stops the comparison
	} else if err != nil {
		t.Fatalf("compare: %d %v\n%s\n%s", code, err, out, errOut)
	}
	drifts := map[string][]compare.Drift{}
	for _, res := range r.Results {
		drifts[res.ID] = res.Drifts
	}
	return code, drifts, failed
}

// has fails the test unless the drifts hold one of kind at where.
func has(t *testing.T, drifts []compare.Drift, kind, where string) {
	t.Helper()
	if !slices.ContainsFunc(drifts, func(d compare.Drift) bool { return d.Kind == kind && d.Where == where }) {
		t.Errorf("no %s drift at %s in %+v", kind, where, drifts)
	}
}

func jsonBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, body)
}

const oneGet = `{"version":1,"requests":[{"id":"home","method":"GET","path":"/","storage":"public"}]}`

// TestRegressionCookieSameSite: the framework's merged session
// configuration adds SameSite=Lax to the session cookie of every response.
// The cookie's value changes every time and is masked (digits to 0); its
// attributes are not, so the new one shows on the header.
func TestRegressionCookieSameSite(t *testing.T) {
	var n atomic.Int64
	cookie := func(extra string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Set-Cookie", fmt.Sprintf("session=%040d; expires=Fri, 30-Oct-2026 10:00:00 GMT; Max-Age=7200; path=/; httponly%s", n.Add(1), extra))
			jsonBody(w, 200, `{"ok":true}`)
		}
	}
	code, drifts := regression(t, oneGet, "", "", cookie(""), cookie("; samesite=lax"))
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["home"], compare.KindHeaderValue, "header:set-cookie")
}

// TestRegressionCharsetCase: Content-Type says charset=utf-8 where it said
// UTF-8. Reported as a changed header; a header rule with ignore_case (the
// names are case-insensitive) takes it, and only it.
func TestRegressionCharsetCase(t *testing.T) {
	page := func(charset, title string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset="+charset)
			fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>%s</title></head><body><h1>News</h1></body></html>", title)
		}
	}
	code, drifts := regression(t, oneGet, "", "", page("UTF-8", "Home"), page("utf-8", "Home"))
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["home"], compare.KindHeaderValue, "header:content-type")
	ignore := "  - name: content-type\n    ignore_case: true\n    reason: charset names are case-insensitive\n"
	if code, drifts := regression(t, oneGet, ignore, "", page("UTF-8", "Home"), page("utf-8", "Home")); code != ExitClean {
		t.Errorf("ignore_case: exit %d %+v", code, drifts)
	}
	// It does not hide another change of the type.
	if code, _ := regression(t, oneGet, ignore, "", page("UTF-8", "Home"), page("ISO-8859-1", "Home")); code != ExitDrift {
		t.Errorf("ignore_case let another charset pass: exit %d", code)
	}
}

// TestRegressionNestedSetError: a nested-set (tree) model fails to boot on
// the new framework, and every endpoint that reads it, public ones too,
// answers 500 instead of its tree. One status change per request.
func TestRegressionNestedSetError(t *testing.T) {
	catalog := `{"version":1,"requests":[
 {"id":"menu","method":"GET","path":"/api/menu","storage":"public"},
 {"id":"public-menu","method":"GET","path":"/public-api/menu","storage":"public"},
 {"id":"health","method":"GET","path":"/health","storage":"public"}]}`
	tree := `{"items":[{"id":1,"lft":1,"rgt":6,"children":[{"id":2,"lft":2,"rgt":3,"children":[]},{"id":3,"lft":4,"rgt":5,"children":[]}]}]}`
	before := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			jsonBody(w, 200, `{"ok":true}`)
			return
		}
		jsonBody(w, 200, tree)
	}
	after := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			jsonBody(w, 200, `{"ok":true}`)
			return
		}
		jsonBody(w, 500, `{"message":"Server Error"}`)
	}
	code, drifts := regression(t, catalog, "", "", before, after)
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	for _, id := range []string{"menu", "public-menu"} {
		has(t, drifts[id], compare.KindStatus, "")
	}
	if len(drifts["health"]) > 0 {
		t.Errorf("drift on an endpoint that did not change: %+v", drifts["health"])
	}
}

// TestRegressionTokenTypes: a token's lifetime in the login response comes
// back as an integer where it was a string (a configuration value cast to
// fix another break); clients that compare it as text break. And when the
// token cannot be made at all, login answers 500 and the requests made with
// a token 401.
func TestRegressionTokenTypes(t *testing.T) {
	catalog := `{"version":1,"requests":[
 {"id":"login","method":"POST","path":"/api/auth/login","body":{"user":"demo"},"mutates":false,"storage":"public"},
 {"id":"me","method":"GET","path":"/api/me","storage":"public"}]}`
	api := func(ttl string, broken bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case broken && r.URL.Path == "/api/auth/login":
				jsonBody(w, 500, `{"message":"Carbon::rawAddUnit(): Argument #3 ($value) must be of type int|float, string given"}`)
			case broken:
				jsonBody(w, 401, `{"message":"Unauthenticated."}`)
			case r.URL.Path == "/api/auth/login":
				jsonBody(w, 200, `{"token":{"type":"bearer","ttl":`+ttl+`}}`)
			default:
				jsonBody(w, 200, `{"user":{"id":1}}`)
			}
		}
	}
	code, drifts := regression(t, catalog, "", "", api(`"60"`, false), api(`60`, false))
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["login"], compare.KindType, "$.token.ttl")
	if len(drifts["me"]) > 0 {
		t.Errorf("me: %+v", drifts["me"])
	}

	_, drifts = regression(t, catalog, "", "", api(`"60"`, false), api(`"60"`, true))
	has(t, drifts["login"], compare.KindStatus, "")
	has(t, drifts["me"], compare.KindStatus, "")
}

// TestRegressionRedirect: a page now redirects to the start page. A client
// that follows redirects (the old gate script did) sees the start page's
// 200; DriftGate records the redirect itself: the status and where to.
func TestRegressionRedirect(t *testing.T) {
	before := func(w http.ResponseWriter, r *http.Request) { jsonBody(w, 200, `{"page":"`+r.URL.Path+`"}`) }
	after := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			before(w, r)
			return
		}
		http.Redirect(w, r, "/", http.StatusFound)
	}
	catalog := `{"version":1,"requests":[{"id":"settings","method":"GET","path":"/settings","storage":"public"}]}`
	code, drifts := regression(t, catalog, "", "", before, after)
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["settings"], compare.KindStatus, "")
	has(t, drifts["settings"], compare.KindHeaderExtra, "header:location")
}

// TestRegressionSlowEndpoint: a list endpoint that took a moment now takes
// longer than the request's time limit. The capture names the request that
// failed (exit 3), and the comparison does not pass it (here it refuses the
// failed capture, exit 3).
func TestRegressionSlowEndpoint(t *testing.T) {
	var slow atomic.Bool
	release := make(chan struct{})
	defer close(release)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		jsonBody(w, 200, `{"events":[]}`)
	}
	catalog := `{"version":1,"requests":[{"id":"events","method":"GET","path":"/api/events","storage":"public"}]}`
	after := func(w http.ResponseWriter, r *http.Request) { slow.Store(true); handler(w, r) }
	start := time.Now()
	code, drifts, failed := regressionFailed(t, catalog, "", "  timeout: 300ms\n", handler, after)
	if !failed || code == ExitClean {
		t.Fatalf("a request that timed out passed: capture failed %v, compare exit %d %+v", failed, code, drifts)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

// TestRegressionRowOrder: a list read without ORDER BY comes back in
// another order after the database's statistics changed (a fresh copy, a
// new server version). An array no rule pairs by key reports the same
// items in another order as one drift at the array.
func TestRegressionRowOrder(t *testing.T) {
	list := func(ids ...int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var items []string
			for _, id := range ids {
				items = append(items, fmt.Sprintf(`{"id":%d,"title":"t%d"}`, id, id))
			}
			jsonBody(w, 200, `{"items":[`+strings.Join(items, ",")+`]}`)
		}
	}
	code, drifts := regression(t, oneGet, "", "", list(1, 2, 3), list(2, 1, 3))
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["home"], compare.KindItemOrder, "$.items")
	if len(drifts["home"]) != 1 {
		t.Errorf("drifts %+v, want only the order", drifts["home"])
	}
}

// TestRegressionDeleteLeavesTables: deleting a module drops its tables.
// After the upgrade the delete fails (500) half way: the module's row is
// gone, its tables stay. Both show: the status, and what the request did
// to the database (side effects, from the state fingerprint).
func TestRegressionDeleteLeavesTables(t *testing.T) {
	servers := os.Getenv("DRIFTGATE_TEST_MYSQL")
	if servers == "" {
		t.Skip("DRIFTGATE_TEST_MYSQL not set")
	}
	dsn := strings.Split(servers, ",")[0]
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mysql.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net, cfg.Addr = "tcp", u.Host
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	run := func(stmts ...string) error {
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	}
	if err := run("DROP DATABASE IF EXISTS dgt_regress", "DROP DATABASE IF EXISTS dgt_regress_pristine", "CREATE DATABASE dgt_regress",
		"CREATE TABLE dgt_regress.modules (id INT PRIMARY KEY, name VARCHAR(20))", "INSERT INTO dgt_regress.modules VALUES (7, 'events')",
		"CREATE TABLE dgt_regress.module_7_items (id INT PRIMARY KEY)", "CREATE TABLE dgt_regress.module_7_tags (item_id INT, tag_id INT)"); err != nil {
		t.Fatal(err)
	}
	defer run("DROP DATABASE IF EXISTS dgt_regress", "DROP DATABASE IF EXISTS dgt_regress_pristine")
	del := func(complete bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := run("DELETE FROM dgt_regress.modules WHERE id = 7"); err != nil {
				jsonBody(w, 500, `{"message":"database"}`)
				return
			}
			if !complete {
				jsonBody(w, 500, `{"message":"Server Error"}`)
				return
			}
			if err := run("DROP TABLE dgt_regress.module_7_items, dgt_regress.module_7_tags"); err != nil {
				jsonBody(w, 500, `{"message":"database"}`)
				return
			}
			jsonBody(w, 200, `{"deleted":7}`)
		}
	}
	t.Setenv("DG_REGRESS_DSN", dsn)
	state := "state:\n  db:\n    driver: mysql\n    dsn: { env: DG_REGRESS_DSN }\n    working: dgt_regress\n    pristine: dgt_regress_pristine\n"
	catalog := `{"version":1,"requests":[{"id":"delete-module","method":"DELETE","path":"/api/modules/7","storage":"public"}]}`
	code, drifts := regression(t, catalog, "", state, del(true), del(false))
	if code != ExitDrift {
		t.Fatalf("exit %d", code)
	}
	has(t, drifts["delete-module"], compare.KindStatus, "")
	if !slices.ContainsFunc(drifts["delete-module"], func(d compare.Drift) bool {
		return d.Kind == compare.KindSideEffect && strings.Contains(d.Where+d.Baseline+d.Current, "module_7_items")
	}) {
		t.Errorf("the tables left behind do not show: %+v", drifts["delete-module"])
	}
}
