package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/catalog"
)

// Secrets and personal data the recorded traffic carries; none may reach a
// written file.
var recordSecrets = []string{"tok-admin-123", "tok-other-456", "sess-789", "hunter2", "SECRET-ROWS", "Petrovic", "pera.peric", "QUERYSECRET"}

// recordApp is the application behind the proxy: a JSON API with path
// parameters, a login that returns a token, an authenticated request, an
// upload, static files and an event stream.
func recordApp(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	jsonOut := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /api/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"id": r.PathValue("id"), "email": "pera.peric@example.com"})
	})
	mux.HandleFunc("GET /api/projects/{pid}/tasks/{tid}", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"project": r.PathValue("pid"), "task": r.PathValue("tid")})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, map[string]any{"token": "tok-admin-123"})
	})
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(401)
			return
		}
		jsonOut(w, map[string]any{"name": "admin"})
	})
	mux.HandleFunc("POST /api/uploads", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		jsonOut(w, map[string]any{"kind": r.FormValue("kind")})
	})
	mux.HandleFunc("GET /api/notifications", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, []any{}) })
	mux.HandleFunc("GET /api/news", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{"items": []any{}}) })
	mux.HandleFunc("GET /api/vault", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, map[string]any{}) })
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hello\n\n")
	})
	for _, p := range []string{"/app.js", "/_next/static/chunks/main.js", "/logo.png", "/favicon.ico", "/_next/image"} {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "static") })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const recordRoutes = `{"version":1,"routes":[
 {"methods":["GET"],"path":"/api/users/{id}","source":"test"},
 {"methods":["GET"],"path":"/api/projects/{pid}/tasks/{tid}","source":"test"},
 {"methods":["POST"],"path":"/api/login","source":"test"},
 {"methods":["GET"],"path":"/api/me","source":"test"},
 {"methods":["POST"],"path":"/api/uploads","source":"test"},
 {"methods":["GET"],"path":"/api/notifications","source":"test"},
 {"methods":["GET"],"path":"/api/news","source":"test"},
 {"methods":["GET"],"path":"/api/orphan","source":"test"}]}`

// recordProject writes a project for the app; storage is public by
// default, so private entries come from an identity or personal data.
func recordProject(t *testing.T, baseURL string) string {
	root := t.TempDir()
	write(t, filepath.Join(root, "driftgate.yaml"), fmt.Sprintf(`version: 1
project: rec
target:
  base_url: %s
identities:
  admin: { env: DG_REC_ADMIN }
storage:
  default: public
`, baseURL))
	write(t, filepath.Join(root, "driftgate", "routes.json"), recordRoutes)
	write(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[
 {"id":"hand-news","method":"GET","path":"/api/news?page=9","storage":"public"}]}`)
	t.Setenv("DG_REC_ADMIN", "tok-admin-123")
	t.Setenv("DRIFTGATE_STORE", filepath.Join(t.TempDir(), "store"))
	return root
}

// step is one request of the recorded session.
type step struct {
	method, path, contentType, body string
	headers                         [][2]string
}

func recordSession(t *testing.T) []step {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	mw.WriteField("kind", "orders")
	fw, _ := mw.CreateFormFile("file", "Petar Petrovic 2024.xlsx")
	fw.Write([]byte("PK\x03\x04 SECRET-ROWS"))
	mw.Close()
	signedIn := [][2]string{{"Authorization", "Bearer tok-admin-123"}, {"Cookie", "session=sess-789"}, {"Accept", "application/json"}}
	return []step{
		{method: "GET", path: "/app.js"},
		{method: "GET", path: "/_next/static/chunks/main.js"},
		{method: "GET", path: "/logo.png"},
		{method: "GET", path: "/favicon.ico"},
		{method: "GET", path: "/_next/image?url=x"},
		{method: "POST", path: "/api/login", contentType: "application/json", body: `{"email":"admin@example.com","password":"hunter2"}`},
		{method: "GET", path: "/api/users/1", headers: [][2]string{{"Accept", "application/json"}}},
		{method: "GET", path: "/api/users/2", headers: [][2]string{{"Accept", "application/json"}}},
		{method: "GET", path: "/api/projects/7/tasks/9"},
		{method: "GET", path: "/api/me", headers: signedIn},
		{method: "GET", path: "/api/me", headers: [][2]string{{"Authorization", "Bearer tok-other-456"}}},
		{method: "POST", path: "/api/uploads", contentType: mw.FormDataContentType(), body: form.String(), headers: signedIn},
		{method: "GET", path: "/api/notifications?since=1"},
		{method: "GET", path: "/api/notifications?since=2"},
		{method: "GET", path: "/api/notifications?since=3"},
		{method: "GET", path: "/api/news?page=2"},
		{method: "GET", path: "/api/vault?access_token=QUERYSECRET"},
	}
}

func send(t *testing.T, base string, s step) *http.Response {
	t.Helper()
	var body io.Reader
	if s.body != "" {
		body = strings.NewReader(s.body)
	}
	req, err := http.NewRequest(s.method, base+s.path, body)
	if err != nil {
		t.Fatal(err)
	}
	if s.contentType != "" {
		req.Header.Set("Content-Type", s.contentType)
	}
	for _, h := range s.headers {
		req.Header.Add(h[0], h[1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// runRecord starts record, runs drive against the proxy address and stops
// it as Ctrl-C would.
func runRecord(t *testing.T, args []string, drive func(addr string)) (int, string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	addrs := make(chan string, 1)
	go func() {
		code, err := record(ctx, args, &out, func(addr string) { addrs <- addr })
		done <- result{code, err}
	}()
	select {
	case addr := <-addrs:
		drive(addr)
		cancel()
		r := <-done
		return r.code, out.String(), r.err
	case r := <-done:
		return r.code, out.String(), r.err
	}
}

func requestsOf(t *testing.T, root string) []catalog.Request {
	t.Helper()
	cat, err := catalog.Load(filepath.Join(root, "driftgate", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cat.Requests
}

func TestRecord(t *testing.T) {
	app := recordApp(t)
	root := recordProject(t, app.URL)
	cfg := filepath.Join(root, "driftgate.yaml")
	code, out, err := runRecord(t, []string{"--config", cfg, "--listen", "127.0.0.1:0", "--write"}, func(addr string) {
		for _, s := range recordSession(t) {
			resp := send(t, "http://"+addr, s)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		// The event stream passes through: the client reads the event.
		resp := send(t, "http://"+addr, step{method: "GET", path: "/events"})
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		resp.Body.Close()
		if line != "data: hello\n" {
			t.Errorf("event stream: %q", line)
		}
	})
	if err != nil || code != ExitClean {
		t.Fatalf("record: %d %v\n%s", code, err, out)
	}
	for _, want := range []string{"passed through, not recorded: GET /events (event stream)",
		"skipped POST /api/login: the body carries password", "skipped GET /api/vault: the query carries access_token",
		"route GET /api/users/{id}, uncovered before", "added to "} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	reqs := requestsOf(t, root)
	got := map[string]catalog.Request{}
	for _, r := range reqs {
		got[r.ID] = r
	}
	if len(reqs) != 7 {
		t.Fatalf("catalog: %d requests\n%s", len(reqs), out)
	}
	if r := got["hand-news"]; r.Path != "/api/news?page=9" || r.Source != nil {
		t.Errorf("existing entry changed: %+v", r)
	}
	users := got["rec-get-api-users-id"]
	if users.Path != "/api/users/{id}" || !reflect.DeepEqual(users.Params, map[string]string{"id": "1"}) ||
		users.Storage != "private" || users.Source == nil || users.Source.Import != "record" || users.Mutating() {
		t.Errorf("users (personal data in the response → private): %+v", users)
	}
	if r := got["rec-get-api-projects-pid-tasks-tid"]; !reflect.DeepEqual(r.Params, map[string]string{"pid": "7", "tid": "9"}) || r.Storage != "public" {
		t.Errorf("tasks: %+v", r)
	}
	if r := got["rec-get-api-me"]; r.Identity != "admin" || r.Storage != "private" || len(r.Headers) != 1 {
		t.Errorf("me (token of admin): %+v", r)
	}
	if r := got["rec-get-api-me-2"]; r.Identity != "" {
		t.Errorf("me with an unknown token: %+v", r)
	}
	if !strings.Contains(out, "signed in; set identity") {
		t.Errorf("an unknown token is not reported:\n%s", out)
	}
	up := got["rec-post-api-uploads"]
	if up.Multipart == nil || !reflect.DeepEqual(up.Multipart.Fields, [][2]string{{"kind", "orders"}}) ||
		len(up.Multipart.Files) != 1 || up.Multipart.Files[0].Synthetic != "xlsx" || up.Identity != "admin" || !up.Mutating() {
		t.Errorf("upload: %+v %+v", up, up.Multipart)
	}
	if r := got["rec-get-api-notifications-since-1"]; r.Path != "/api/notifications?since=1" {
		t.Errorf("polling is one entry: %+v", got)
	}
	for id := range got {
		if strings.Contains(id, "js") || strings.Contains(id, "png") || strings.Contains(id, "events") || strings.Contains(id, "favicon") || strings.Contains(id, "image") {
			t.Errorf("recorded %s", id)
		}
	}
	assertNoSecrets(t, root)
	if strings.Contains(out, "QUERYSECRET") || strings.Contains(out, "tok-") || strings.Contains(out, "hunter2") {
		t.Errorf("the output shows a secret:\n%s", out)
	}

	// Recording the same again proposes nothing.
	_, out, _ = runRecord(t, []string{"--config", cfg, "--listen", "127.0.0.1:0", "--write"}, func(addr string) {
		for _, s := range recordSession(t) {
			resp := send(t, "http://"+addr, s)
			resp.Body.Close()
		}
	})
	if !strings.Contains(out, "0 new catalog entries") || len(requestsOf(t, root)) != 7 {
		t.Errorf("a second recording added entries:\n%s", out)
	}
}

func assertNoSecrets(t *testing.T, root string) {
	t.Helper()
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		for _, s := range recordSecrets {
			if bytes.Contains(raw, []byte(s)) {
				t.Errorf("%s holds %s", path, s)
			}
		}
		return nil
	})
}

// TestRecordAndHARImportAgree: the same traffic as a HAR file gives the
// same catalog entries through har import (one pipeline).
func TestRecordAndHARImportAgree(t *testing.T) {
	app := recordApp(t)
	recRoot := recordProject(t, app.URL)
	harRoot := recordProject(t, app.URL)
	session := recordSession(t)
	if _, out, err := runRecord(t, []string{"--config", filepath.Join(recRoot, "driftgate.yaml"), "--listen", "127.0.0.1:0", "--write"}, func(addr string) {
		for _, s := range session {
			resp := send(t, "http://"+addr, s)
			resp.Body.Close()
		}
	}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// The HAR a browser would save: requests with their headers and bodies,
	// responses with their content.
	var entries []any
	for _, s := range session {
		resp := send(t, app.URL, s)
		content, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var headers []map[string]string
		if s.contentType != "" {
			headers = append(headers, map[string]string{"name": "Content-Type", "value": s.contentType})
		}
		for _, h := range s.headers {
			headers = append(headers, map[string]string{"name": h[0], "value": h[1]})
		}
		req := map[string]any{"method": s.method, "url": app.URL + s.path, "headers": headers}
		if s.body != "" {
			req["postData"] = map[string]any{"mimeType": s.contentType, "text": s.body}
		}
		entries = append(entries, map[string]any{"request": req, "response": map[string]any{"content": map[string]any{"text": string(content)}}})
	}
	raw, _ := json.Marshal(map[string]any{"log": map[string]any{"entries": entries}})
	harFile := filepath.Join(t.TempDir(), "session.har")
	write(t, harFile, string(raw))
	if code, out, errOut := runCLI(t, "har", "import", harFile, "--config", filepath.Join(harRoot, "driftgate.yaml"), "--write"); code != ExitClean {
		t.Fatalf("har import: %d\n%s%s", code, out, errOut)
	}

	normal := func(rs []catalog.Request) []catalog.Request {
		var out []catalog.Request
		for _, r := range rs {
			r.ID = strings.TrimPrefix(strings.TrimPrefix(r.ID, "rec-"), "har-")
			r.Source = nil
			out = append(out, r)
		}
		return out
	}
	rec, imp := normal(requestsOf(t, recRoot)), normal(requestsOf(t, harRoot))
	if !reflect.DeepEqual(rec, imp) {
		a, _ := json.MarshalIndent(rec, "", " ")
		b, _ := json.MarshalIndent(imp, "", " ")
		t.Fatalf("record and har import differ:\nrecord: %s\nhar: %s", a, b)
	}
	if len(rec) != 7 {
		t.Fatalf("entries: %d", len(rec))
	}
	assertNoSecrets(t, harRoot)
}

func TestRecordRefuses(t *testing.T) {
	app := recordApp(t)
	root := recordProject(t, app.URL)
	cfg := filepath.Join(root, "driftgate.yaml")
	for _, listen := range []string{"0.0.0.0:8941", ":8941", "192.168.1.10:8941"} {
		code, _, errOut := runCLI(t, "record", "--config", cfg, "--listen", listen)
		if code != ExitConfig || !strings.Contains(errOut, "not a loopback address") || !strings.Contains(errOut, "--allow-non-loopback") {
			t.Errorf("--listen %s: %d %s", listen, code, errOut)
		}
	}
	// A base URL outside allow_hosts: refused before listening.
	write(t, cfg, "version: 1\nproject: rec\ntarget:\n  base_url: http://10.255.255.1:1\n  allow_hosts: [\"127.0.0.0/8\"]\n")
	code, _, errOut := runCLI(t, "record", "--config", cfg, "--listen", "127.0.0.1:0")
	if code != ExitConfig || !strings.Contains(errOut, "host not allowed") || !strings.Contains(errOut, "allow_hosts") {
		t.Errorf("remote target: %d %s", code, errOut)
	}
}

// TestRecordStopsWithRequestsInFlight: Ctrl-C while a request waits for a
// slow service ends that request, waits for its handler and closes the
// connections to the service: nothing of the proxy is left running.
func TestRecordStopsWithRequestsInFlight(t *testing.T) {
	started := make(chan struct{}, 1)
	aborted := make(chan struct{})
	var mu sync.Mutex
	open := 0 // connections from the proxy to the service
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slow" {
			return
		}
		started <- struct{}{}
		select {
		case <-r.Context().Done(): // the proxy gave up the request
			close(aborted)
		case <-time.After(time.Minute):
		}
	}))
	app.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew:
			open++
		case http.StateClosed, http.StateHijacked:
			open--
		}
	}
	app.Start()
	defer app.Close()
	root := recordProject(t, app.URL)
	failed := make(chan error, 1)
	begin := time.Now()
	code, out, err := runRecord(t, []string{"--config", filepath.Join(root, "driftgate.yaml"), "--listen", "127.0.0.1:0"}, func(addr string) {
		// One request in flight, one done (its connection stays idle).
		go func() {
			resp, err := http.Get("http://" + addr + "/api/slow")
			if err == nil {
				resp.Body.Close()
			}
			failed <- err
		}()
		<-started
		resp := send(t, "http://"+addr, step{method: "GET", path: "/api/news"})
		resp.Body.Close()
	})
	if err != nil || code != ExitClean {
		t.Fatalf("record: %d %v\n%s", code, err, out)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("stopping took %s", d)
	}
	buf := make([]byte, 1<<20)
	if stacks := string(buf[:runtime.Stack(buf, true)]); strings.Contains(stacks, "internal/proxy.(*Proxy).ServeHTTP") {
		t.Errorf("a handler of the proxy runs after stop:\n%s", stacks)
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("the request to the service is still open")
	}
	<-failed
	if !strings.Contains(out, "1 requests recorded") {
		t.Errorf("output:\n%s", out)
	}
	// The service sees the connections close a moment later.
	for range 100 {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d connections to the service left open after stop", open)
}

// TestRecordLabelMasks: the path a passed-through request is printed with
// has no query, and personal data in it is masked.
func TestRecordLabelMasks(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:9/api")
	for raw, want := range map[string]string{
		"http://127.0.0.1:9/api/events?token=QUERYSECRET":          "/events",
		"http://127.0.0.1:9/api/users/pera.peric@example.com/feed": "/users/{personal}/feed",
		"http://127.0.0.1:9/api":                                   "/",
	} {
		if got := relPath(base, raw); got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
}

// TestRecordLaterResponsePersonal: personal data in the response of a
// request already seen (another poll of the same route) still makes the
// entry private.
func TestRecordLaterResponsePersonal(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"from":"pera.peric@example.com"}]}`)
	}))
	defer app.Close()
	root := recordProject(t, app.URL)
	code, out, err := runRecord(t, []string{"--config", filepath.Join(root, "driftgate.yaml"), "--listen", "127.0.0.1:0", "--write"}, func(addr string) {
		for _, since := range []string{"1", "2"} {
			resp := send(t, "http://"+addr, step{method: "GET", path: "/api/notifications?since=" + since})
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
	if err != nil || code != ExitClean {
		t.Fatalf("record: %d %v\n%s", code, err, out)
	}
	var got []catalog.Request
	for _, r := range requestsOf(t, root) {
		if r.Source != nil {
			got = append(got, r)
		}
	}
	if len(got) != 1 || got[0].Storage != "private" {
		t.Fatalf("recorded %+v\n%s", got, out)
	}
	if !strings.Contains(out, "private") {
		t.Errorf("the summary does not say private:\n%s", out)
	}
}

// TestRecordCompressedPersonal: personal data in a gzip response is found
// (the entry is private), and a response in an encoding record cannot read
// makes its entry private too.
func TestRecordCompressedPersonal(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/news" {
			w.Header().Set("Content-Encoding", "br")
			fmt.Fprint(w, "\x1b\x21\x00\xf8\x25\x00")
			return
		}
		// Long enough to be compressed, not stored: the address is not in
		// the bytes as they are.
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		fmt.Fprint(gz, `{"id":1,"email":"pera.peric@example.com","notes":"`+strings.Repeat("see the notes of pera ", 40)+`"}`)
		gz.Close()
	}))
	defer app.Close()
	root := recordProject(t, app.URL)
	code, out, err := runRecord(t, []string{"--config", filepath.Join(root, "driftgate.yaml"), "--listen", "127.0.0.1:0", "--write"}, func(addr string) {
		for _, path := range []string{"/api/users/1", "/api/news"} {
			resp := send(t, "http://"+addr, step{method: "GET", path: path, headers: [][2]string{{"Accept-Encoding", "gzip, br"}}})
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
	if err != nil || code != ExitClean {
		t.Fatalf("record: %d %v\n%s", code, err, out)
	}
	got := map[string]string{}
	for _, r := range requestsOf(t, root) {
		if r.Source != nil {
			got[r.Path] = r.Storage
		}
	}
	if want := map[string]string{"/api/users/{id}": "private", "/api/news": "private"}; !reflect.DeepEqual(got, want) {
		t.Errorf("storage %v, want %v\n%s", got, want, out)
	}
}
