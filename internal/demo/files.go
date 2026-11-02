package demo

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/sheet"
)

// Planted changes of the upload, queue, export and rate-limit endpoints,
// switched on with Plant while the server runs.
const (
	ChangeUpload = "upload" // the import reads values as whole numbers
	ChangeJob    = "job"    // the queued import job fails
	ChangeExport = "export" // the export renames a column
	ChangeLimit  = "limit"  // the rate limit is 59 per minute instead of 60
)

// RateLimit is the number of likes a client may send per minute.
const RateLimit = 60

type orderRow struct {
	ID     int     `json:"id"`
	Region string  `json:"region"`
	Value  float64 `json:"value"`
}

type importJob struct {
	ID   int        `json:"id"`
	File string     `json:"file"`
	Rows []orderRow `json:"rows"`
}

// files is the state of the upload endpoints: imported rows, the queue of
// import jobs and the rate-limit counters (a cache in a real application).
// The data is always in memory, also with NewSQL.
type files struct {
	planted map[string]bool
	orders  []orderRow
	jobs    []importJob
	nextJob int
	window  map[string]time.Time // client → start of its rate-limit window
	hits    map[string]int
}

func (f *files) reset() {
	f.orders, f.jobs, f.nextJob = nil, nil, 0
	f.window, f.hits = map[string]time.Time{}, map[string]int{}
}

// Plant switches a planted change (Change*) on or off. Planted changes
// survive /admin/reset, which restores data only.
func (s *Server) Plant(change string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files.planted == nil {
		s.files.planted = map[string]bool{}
	}
	s.files.planted[change] = on
}

func formatValue(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// serveFiles handles the endpoints of file uploads and repeated requests:
//
//	POST /api/imports            multipart: kind=orders, file=<CSV or XLSX with region,value>; queues a job
//	POST /admin/queue/work       runs the queued jobs (the command step of a sequence calls it)
//	GET  /api/orders             the imported rows
//	GET  /api/exports/orders.xlsx and .csv
//	PUT  /api/posts/{id}/like    limited to RateLimit per minute per client: 429 with Retry-After
//	GET  /admin/state/orders and /admin/state/jobs   the data, for a fingerprint command
func (s *Server) serveFiles(w http.ResponseWriter, r *http.Request) bool {
	f := &s.files
	if f.window == nil {
		f.reset()
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/imports":
		s.upload(w, r)
	case r.Method == "POST" && r.URL.Path == "/admin/queue/work":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		n := 0
		for _, job := range f.jobs {
			if f.planted[ChangeJob] {
				f.jobs = nil
				w.WriteHeader(500)
				fmt.Fprintf(w, "job %d (%s) failed: no handler for the import\n", job.ID, job.File)
				return true
			}
			for _, row := range job.Rows {
				row.ID = len(f.orders) + 1
				f.orders = append(f.orders, row)
			}
			n++
		}
		f.jobs = nil
		fmt.Fprintf(w, "processed %d jobs\n", n)
	case r.Method == "GET" && r.URL.Path == "/api/orders":
		entries := []any{}
		for _, row := range f.orders {
			entries = append(entries, map[string]any{"id": row.ID, "region": row.Region, "value": formatValue(row.Value)})
		}
		s.write(w, 200, map[string]any{"entries": entries, "count": len(entries)})
	case r.Method == "GET" && (r.URL.Path == "/api/exports/orders.xlsx" || r.URL.Path == "/api/exports/orders.csv"):
		header := []string{"region", "value"}
		if f.planted[ChangeExport] {
			header[1] = "amount"
		}
		rows := [][]string{header}
		for _, row := range f.orders {
			rows = append(rows, []string{row.Region, formatValue(row.Value)})
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		if strings.HasSuffix(name, ".xlsx") {
			w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
			w.Write(sheet.WriteXLSX(rows))
			return true
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		for _, row := range rows {
			fmt.Fprintf(w, "%s\r\n", strings.Join(row, ","))
		}
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/posts/") && strings.HasSuffix(r.URL.Path, "/like"):
		s.like(w, r)
	case r.Method == "GET" && (r.URL.Path == "/admin/state/orders" || r.URL.Path == "/admin/state/jobs"):
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "orders") {
			json.NewEncoder(w).Encode(f.orders)
		} else {
			json.NewEncoder(w).Encode(f.jobs)
		}
	default:
		return false
	}
	return true
}

// upload validates an import file like a form request would, reads its
// rows and queues a job that imports them.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	f := &s.files
	if r.Header.Get("Authorization") != "Bearer "+AdminToken {
		s.write(w, 401, map[string]any{"message": "Unauthenticated."})
		return
	}
	invalid := func(field, msg string) {
		s.write(w, 422, map[string]any{"message": msg, "errors": map[string]any{field: []string{msg}}})
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		invalid("file", "The file field is required.")
		return
	}
	if r.FormValue("kind") != "orders" {
		invalid("kind", "The selected kind is invalid.")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		invalid("file", "The file field is required.")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		invalid("file", "The file failed to upload.")
		return
	}
	var sh sheet.Sheet
	if sheet.IsXLSX(data) {
		sheets, err := sheet.ReadXLSX(data)
		if err != nil {
			invalid("file", "The file must be a file of type: csv, xlsx.")
			return
		}
		sh = sheets[0]
	} else if sh, err = sheet.ReadCSV(data); err != nil {
		invalid("file", "The file must be a file of type: csv, xlsx.")
		return
	}
	rows := sh.Rows()
	if len(rows) == 0 || len(rows[0]) < 2 || rows[0][0] != "region" || rows[0][1] != "value" {
		invalid("file", "The file must have the columns region and value.")
		return
	}
	job := importJob{File: hdr.Filename}
	for i, row := range rows[1:] {
		for len(row) < 2 {
			row = append(row, "")
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64)
		if err != nil {
			invalid("file", fmt.Sprintf("Row %d: the value must be a number.", i+2))
			return
		}
		if f.planted[ChangeUpload] {
			v = math.Trunc(v)
		}
		job.Rows = append(job.Rows, orderRow{Region: row[0], Value: v})
	}
	f.nextJob++
	job.ID = f.nextJob
	f.jobs = append(f.jobs, job)
	s.write(w, 202, map[string]any{"job": job.ID, "file": job.File, "rows": len(job.Rows)})
}

// like is limited per client, the way Laravel's throttle middleware
// does it: a window starts with the first request and lasts a minute.
func (s *Server) like(w http.ResponseWriter, r *http.Request) {
	f := &s.files
	limit := RateLimit
	if f.planted[ChangeLimit] {
		limit--
	}
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	start, ok := f.window[client]
	if !ok || now.Sub(start) >= time.Minute {
		start, f.window[client], f.hits[client] = now, now, 0
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	if f.hits[client] >= limit {
		reset := start.Add(time.Minute)
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(time.Until(reset).Seconds()))))
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		s.write(w, 429, map[string]any{"message": "Too many attempts."})
		return
	}
	f.hits[client]++
	h.Set("X-RateLimit-Remaining", strconv.Itoa(limit-f.hits[client]))
	s.write(w, 200, map[string]any{"liked": true})
}
