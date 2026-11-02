// Package demo is a small API for testing DriftGate end to end. Version "v1"
// is the baseline; "v2" plants drift: a string ID becomes a number, the date
// format changes, a key appears, a header is added and creating a FAQ entry
// also writes a log row. Some requests change data, reading a news item among
// them; one returns personal data, and tokens and cookies are new each time.
//
// The data lives in memory, or in a MySQL/MariaDB database (NewSQL), so the
// state components of DriftGate can be tested against it.
package demo

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AdminToken is the token the demo accepts for its one identity.
const AdminToken = "demo-admin-token"

// Server is the demo API.
type Server struct {
	version atomic.Value // "v1" or "v2"
	mu      sync.Mutex
	db      *sql.DB // nil: data in memory
	views   map[int]int
	faq     []map[string]any
	files   files // uploads, a queued job, exports and a rate limit (files.go)
}

// New returns a demo server of the given version that keeps its data in memory.
func New(version string) *Server {
	s := &Server{}
	s.SetVersion(version)
	s.Reset()
	return s
}

// Schema creates the demo tables with their initial data in a MySQL or
// MariaDB database.
var Schema = []string{
	"CREATE TABLE news (id INT PRIMARY KEY, views INT NOT NULL)",
	"INSERT INTO news VALUES (11, 41), (12, 9)",
	"CREATE TABLE faq (id INT AUTO_INCREMENT PRIMARY KEY, created_at DATETIME NOT NULL)",
	"CREATE TABLE faq_log (id INT AUTO_INCREMENT PRIMARY KEY, faq_id INT NOT NULL)",
}

// NewSQL returns a demo server whose data is in db (see Schema).
func NewSQL(version string, db *sql.DB) *Server {
	s := &Server{db: db}
	s.SetVersion(version)
	return s
}

// SetVersion switches between "v1" and "v2" while running.
func (s *Server) SetVersion(v string) { s.version.Store(v) }

func (s *Server) v2() bool { return s.version.Load() == "v2" }

// Reset restores the initial data.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.views = map[int]int{11: 41, 12: 9}
	s.faq = nil
	s.files.reset()
}

var news = []struct {
	id    int
	title string
	date  string
}{
	{12, "Hello world", "2024-03-01 10:00:00"},
	{11, "Release notes", "2024-02-27 08:30:00"},
}

func (s *Server) viewCount(id int) int {
	if s.db == nil {
		return s.views[id]
	}
	var v int
	s.db.QueryRow("SELECT views FROM news WHERE id = ?", id).Scan(&v)
	return v
}

func (s *Server) addView(id int) {
	if s.db == nil {
		s.views[id]++
		return
	}
	s.db.Exec("UPDATE news SET views = views + 1 WHERE id = ?", id)
}

func (s *Server) addFAQ(created string) (int, error) {
	if s.db == nil {
		s.faq = append(s.faq, map[string]any{})
		return len(s.faq), nil
	}
	res, err := s.db.Exec("INSERT INTO faq (created_at) VALUES (?)", created)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if s.v2() {
		if _, err := s.db.Exec("INSERT INTO faq_log (faq_id) VALUES (?)", id); err != nil {
			return 0, err
		}
	}
	return int(id), nil
}

func (s *Server) item(i int) map[string]any {
	n := news[i]
	m := map[string]any{"title": n.title, "published_at": n.date, "views": strconv.Itoa(s.viewCount(n.id))}
	if s.v2() {
		m["id"] = n.id
		m["published_at"] = strings.Replace(n.date, " ", "T", 1) + ".000000Z"
		m["meta"] = map[string]any{}
	} else {
		m["id"] = strconv.Itoa(n.id)
	}
	return m
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if s.v2() {
		w.Header().Set("X-Powered-By", "demo")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ServeHTTP implements the API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveFiles(w, r) {
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/news":
		items := []any{}
		for i := range news {
			items = append(items, s.item(i))
		}
		s.write(w, 200, map[string]any{"entries": items, "count": len(items)})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/news/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/news/"))
		for i, n := range news {
			if n.id == id {
				s.addView(id) // reading a news item changes data, like real sites do
				s.write(w, 200, map[string]any{"entry": s.item(i)})
				return
			}
		}
		s.write(w, 404, map[string]any{"message": "Not found."})
	case r.Method == "GET" && r.URL.Path == "/api/me":
		if r.Header.Get("Authorization") != "Bearer "+AdminToken {
			s.write(w, 401, map[string]any{"message": "Unauthenticated."})
			return
		}
		s.write(w, 200, map[string]any{"user": map[string]any{"id": 1, "email": "pera.peric@example.com"}})
	case r.Method == "GET" && r.URL.Path == "/api/contact":
		s.write(w, 200, map[string]any{"office": "Beograd", "email": "info@demo.test", "manager": "mila.milic@example.com"})
	case r.Method == "POST" && r.URL.Path == "/api/auth/login":
		http.SetCookie(w, &http.Cookie{Name: "session", Value: randomHex(20), Path: "/", HttpOnly: true})
		s.write(w, 200, map[string]any{"token": randomHex(8) + "." + randomHex(16) + "." + randomHex(16)})
	case r.Method == "POST" && r.URL.Path == "/api/faq":
		created := time.Now().UTC().Format("2006-01-02 15:04:05")
		id, err := s.addFAQ(created)
		if err != nil {
			s.write(w, 500, map[string]any{"message": err.Error()})
			return
		}
		s.write(w, 201, map[string]any{"entry": map[string]any{"id": id, "created_at": created}})
	case r.Method == "GET" && r.URL.Path == "/api/slow":
		time.Sleep(20 * time.Millisecond)
		s.write(w, 200, map[string]any{"rows": 100000})
	case r.Method == "POST" && r.URL.Path == "/admin/reset":
		s.mu.Unlock()
		s.Reset()
		s.mu.Lock()
		w.WriteHeader(204)
	default:
		s.write(w, 404, map[string]any{"message": fmt.Sprintf("no route %s %s", r.Method, r.URL.Path)})
	}
}
