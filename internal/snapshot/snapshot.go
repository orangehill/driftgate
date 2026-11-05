// Package snapshot stores captured responses: one record per request plus the
// body, kept by content. Public bodies sit next to the records; private bodies
// (of captures and baselines alike) are encrypted in the project's local store
// and never enter git, not even encrypted: a record names its private body by
// a keyed hash only.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/secret"
)

const Version = 1

// Storage of a body.
const (
	Public  = "public"
	Private = "private"
)

// Body describes a response body without holding it. A public body is
// named by the SHA-256 of its content; a private one by its Ref, a keyed
// hash, so that nothing committed holds a plain hash of personal data.
type Body struct {
	SHA256  string `json:"sha256"` // public bodies; empty for a private body with a ref
	Ref     string `json:"ref,omitempty"`
	Storage string `json:"storage"`
	Size    int64  `json:"size"`
}

// PlainHash reports whether a private body is named by the plain SHA-256 of
// its content: a record made before refs, whose body only the local store of
// the machine that captured it may hold.
func (b Body) PlainHash() bool { return b.Storage == Private && b.Ref == "" && b.SHA256 != "" }

// Record is one captured response.
type Record struct {
	Version int         `json:"version"`
	ID      string      `json:"id"`
	Status  int         `json:"status"`
	Headers [][2]string `json:"headers"` // names lower-case, sorted by name; values in received order
	Body    Body        `json:"body"`
	Error   string      `json:"error,omitempty"` // the request failed; no status or body
	// SideEffects is set when the capture tracked the state: what the request
	// changed in the data (an empty list when nothing).
	SideEffects *SideEffects `json:"side_effects,omitempty"`
	// Repeat lists the statuses of a request sent several times, in order
	// ("200×60, 429×1"); the record is the last response.
	Repeat string `json:"repeat,omitempty"`
	// Exit is the exit code of a command step; its body is the output, when
	// the step compares it.
	Exit *int `json:"exit,omitempty"`
	// Resent says the request was sent again and the record is the second
	// answer (capture's resend after a restore that ended the
	// application's database sessions).
	Resent *Resent `json:"resent,omitempty"`
}

// Resent is why a request was sent again, and the status of its first
// answer.
type Resent struct {
	After string `json:"after"` // "the restore of db ended 3 database sessions of the application"
	First int    `json:"first"` // the first answer's status
}

// SideEffects are the changes one request made to the state.
type SideEffects struct {
	Items []string    `json:"items"`          // <component>/<item>, e.g. db/table:faq
	Rows  []RowChange `json:"rows,omitempty"` // rows of tables under row_tracking
}

// RowChange is one changed row of a tracked table.
type RowChange struct {
	Item    string   `json:"item"` // <component>/table:<table>
	Key     string   `json:"key"`  // primary key as column=value pairs
	Change  string   `json:"change"`
	Columns []string `json:"columns,omitempty"` // for an update: the columns that changed
}

// Meta describes a snapshot set.
type Meta struct {
	Version int    `json:"version"`
	Tag     string `json:"tag"`
	Source  string `json:"source"`
	Created string `json:"created"`
	// State is the fingerprint of the state the capture started from, per
	// component; a capture of another state cannot be compared with it.
	State map[string]map[string]string `json:"state,omitempty"`
	// StateFormat is the state.FingerprintFormat of State; 0 before v1.3.
	StateFormat int `json:"state_format,omitempty"`
	// Unreliable marks a capture made with --skip-fingerprint-check.
	Unreliable bool `json:"unreliable,omitempty"`
	// Incomplete marks a capture that has not finished: it is written
	// first and replaced at the end, so an interrupted capture is not
	// taken for one whose requests are missing.
	Incomplete bool `json:"incomplete,omitempty"`
	// Concurrency is what concurrency: auto decided for each service, in
	// name order; later captures use it without measuring again.
	Concurrency []Concurrency `json:"concurrency,omitempty"`
	// Origins are the origins (scheme://host[:port]) the requests were sent
	// to, per service (the empty name: the only one). Responses that write
	// absolute URLs are compared with another origin's through them.
	Origins map[string]string `json:"origins,omitempty"`
	// Roots are the folder of the project the capture was made in and its
	// real path: responses that write it (a debug stack trace) are
	// compared with another checkout's through them.
	Roots []string `json:"roots,omitempty"`
	// RowTracking are the items whose rows the capture tracked
	// (row_tracking: <component>/table:<name>), [] for none; nil in a
	// capture of an earlier release, which does not say.
	RowTracking []string `json:"row_tracking"`
}

// Concurrency is the decision of concurrency: auto for one service, with
// its evidence.
type Concurrency struct {
	Service string `json:"service,omitempty"` // empty: the only service
	Value   int    `json:"value"`
	Reason  string `json:"reason"`
	// Probe is how long the first read-only requests took at each setting
	// tried.
	Probe []Probe `json:"probe,omitempty"`
	// Tried is the setting compared with a sequential capture of the same
	// state; SequentialMS and ParallelMS are the two captures' times.
	Tried        int   `json:"tried,omitempty"`
	SequentialMS int64 `json:"sequential_ms,omitempty"`
	ParallelMS   int64 `json:"parallel_ms,omitempty"`
	// Differed lists, sorted, the requests or state items that differed
	// from the sequential capture, the reason for one at a time.
	Differed []string `json:"differed,omitempty"`
}

// Probe is one setting of the throughput probe.
type Probe struct {
	Concurrency int   `json:"concurrency"`
	Requests    int   `json:"requests"`
	MS          int64 `json:"ms"`
}

// PrivateStore keeps encrypted private bodies, named by their Ref (the keyed
// hash of the plaintext): the local store as <ref>.enc. Every file is sealed
// with its name, so a file renamed or moved to another body does not
// decrypt. The key is loaded only when a body is actually read or written.
type PrivateStore struct {
	Dir string
	Ext string // ".enc" in the local store; none in the private/ folder of an older baseline
	Key func() (secret.Key, error)
	// KeyHint, when set, says where the key came from, for a body it
	// cannot open.
	KeyHint func() string
}

// ErrPrivateMissing means a private body is not in its store.
var ErrPrivateMissing = errors.New("private body not found")

// ErrPlainHash means a private record names its body by the plain SHA-256 of
// the content, as records did before keyed refs; such a body is only in the
// local store of the machine that captured it.
var ErrPlainHash = errors.New("private body named by its plain SHA-256 (made before keyed refs)")

func (p *PrivateStore) hint() string {
	if p.KeyHint == nil {
		return ""
	}
	if h := p.KeyHint(); h != "" {
		return " (" + h + ")"
	}
	return ""
}

func (p *PrivateStore) path(name string) string { return filepath.Join(p.Dir, name+p.Ext) }

// hashName is the name of a body file: a SHA-256 or a ref, in hex. Records
// come from the repository, so a name is checked before it becomes a path.
var hashName = lazy.Regexp(`^[0-9a-f]{64}$`)

func checkName(name string) error {
	if !hashName().MatchString(name) {
		return fmt.Errorf("invalid body name %q (64 hex digits expected)", name)
	}
	return nil
}

// simpleName is the form of a name that names a file or folder: a request
// ID (the file of its record), a tag, a baseline, a state component.
var simpleName = lazy.Regexp(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// windowsDevices are the names Windows reserves for devices, in any case
// and with any extension: con.json opens the console, not a file.
var windowsDevices = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true}

// SimpleName reports a name that can name a file or folder on every system
// DriftGate runs on: letters, digits and . _ - (not first), not ending in a
// dot (Windows drops it, so "a." would be "a"), and not a device name
// Windows reserves (con, nul, com1…, also as con.x). Names that differ only
// in case are the same file on macOS and Windows; callers that keep several
// names side by side check that with FoldName.
func SimpleName(name string) bool {
	if !simpleName().MatchString(name) || strings.HasSuffix(name, ".") {
		return false
	}
	base, _, _ := strings.Cut(name, ".")
	return !windowsDevices[strings.ToLower(base)]
}

// FoldName is the form under which two names are the same file on a file
// system that ignores case.
func FoldName(name string) string { return strings.ToLower(name) }

// Has reports whether the encrypted body exists.
func (p *PrivateStore) Has(ref string) bool {
	if checkName(ref) != nil {
		return false
	}
	_, err := os.Stat(p.path(ref))
	return err == nil
}

// Put encrypts and stores a body and returns its ref.
func (p *PrivateStore) Put(body []byte) (string, error) {
	return p.put(body, false)
}

// PutInPlace is Put that encrypts body in its own storage, so a large body
// is not held twice: body is overwritten and must not be used afterwards.
func (p *PrivateStore) PutInPlace(body []byte) (string, error) {
	return p.put(body, true)
}

func (p *PrivateStore) put(body []byte, inPlace bool) (string, error) {
	key, err := p.Key()
	if err != nil {
		return "", err
	}
	ref := key.Ref(body)
	if p.Has(ref) {
		// Sealing again would only change the nonce, and the file with it.
		return ref, nil
	}
	if inPlace {
		header, sealed, err := key.SealInPlace(body, []byte(ref))
		if err != nil {
			return "", err
		}
		return ref, writeFile(p.path(ref), 0o600, false, header, sealed)
	}
	sealed, err := key.Seal(body, []byte(ref))
	if err != nil {
		return "", err
	}
	return ref, WriteFile(p.path(ref), sealed, 0o600)
}

// Get decrypts a body and checks that it is the body its ref names.
func (p *PrivateStore) Get(ref string) ([]byte, error) {
	if err := checkName(ref); err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(p.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", p.path(ref), ErrPrivateMissing)
	}
	if err != nil {
		return nil, err
	}
	key, err := p.Key()
	if err != nil {
		return nil, err
	}
	body, err := key.OpenInPlace(sealed, []byte(ref)) // the body is held once
	if err != nil {
		return nil, fmt.Errorf("private body %s: %w%s", ref, err, p.hint())
	}
	if key.Ref(body) != ref {
		return nil, fmt.Errorf("private body %s: content does not match its name", ref)
	}
	if body == nil {
		body = []byte{} // an empty body is still a body
	}
	return body, nil
}

// GetPlain decrypts a body stored under the plain SHA-256 of its content,
// as the local store kept them before refs; a body that is not there
// returns nil without error.
func (p *PrivateStore) GetPlain(sum string) ([]byte, error) {
	if err := checkName(sum); err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(p.path(sum))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := p.Key()
	if err != nil {
		return nil, err
	}
	body, err := key.OpenInPlace(sealed, []byte(sum)) // the body is held once
	if err != nil {
		return nil, fmt.Errorf("private body %s: %w%s", sum, err, p.hint())
	}
	if Sum(body) != sum {
		return nil, fmt.Errorf("private body %s: content does not match its name", sum)
	}
	if body == nil {
		body = []byte{}
	}
	return body, nil
}

// Set is a directory of records (responses/<id>.json) and public bodies
// (bodies/<sha256>). Its private bodies are in the local store.
type Set struct {
	Dir     string
	Private *PrivateStore

	cache *recordCache

	mu      sync.Mutex
	written map[string]bool // public bodies this Set wrote
}

// wrote reports whether the Set wrote the public body sum before; with
// note it notes that it just did.
func (s *Set) wrote(sum string, note bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if note {
		if s.written == nil {
			s.written = map[string]bool{}
		}
		s.written[sum] = true
	}
	return s.written[sum]
}

type recordCache struct {
	mu      sync.Mutex
	records map[string]*Record
}

// CacheRecords makes the set keep every record it reads, for a command
// that reads a record more than once and writes none. A cached record is
// shared: callers must not change it.
func (s *Set) CacheRecords() { s.cache = &recordCache{records: map[string]*Record{}} }

// Exists reports whether the set has been written.
func (s *Set) Exists() bool {
	_, err := os.Stat(filepath.Join(s.Dir, "responses"))
	return err == nil
}

func (s *Set) recordPath(id string) string {
	return filepath.Join(s.Dir, "responses", id+".json")
}

// Record reads one record; a missing record returns nil without error.
func (s *Set) Record(id string) (*Record, error) {
	if s.cache == nil {
		return s.readRecord(id)
	}
	s.cache.mu.Lock()
	r, ok := s.cache.records[id]
	s.cache.mu.Unlock()
	if ok {
		return r, nil
	}
	r, err := s.readRecord(id)
	if err != nil {
		return nil, err
	}
	s.cache.mu.Lock()
	s.cache.records[id] = r
	s.cache.mu.Unlock()
	return r, nil
}

func (s *Set) readRecord(id string) (*Record, error) {
	if !SimpleName(id) {
		return nil, fmt.Errorf("invalid request id %q", id)
	}
	raw, err := os.ReadFile(s.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", s.recordPath(id), err)
	}
	if r.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d", s.recordPath(id), r.Version)
	}
	return &r, nil
}

// Records reads every record of the set, sorted by ID. They are read in
// parallel; the error is the one of the first ID that failed.
func (s *Set) Records() ([]*Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "responses"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			ids = append(ids, id)
		}
	}
	recs := make([]*Record, len(ids))
	if err := Each(len(ids), func(i int) error {
		var err error
		recs[i], err = s.Record(ids[i])
		return err
	}); err != nil {
		return nil, err
	}
	out := recs[:0]
	for _, r := range recs {
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// eachWorkers caps the goroutines of Each: reading and hashing records is
// as fast on four as on one per CPU, and each one holds buffers of its own.
const eachWorkers = 4

// Each runs do for 0…n-1 on one goroutine per CPU, at most eachWorkers.
// After an error no new item starts; the error returned is the one of the
// lowest i that failed, as when the items run one after another.
func Each(n int, do func(i int) error) error {
	errs := make([]error, n)
	var failed atomic.Bool
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(n, runtime.GOMAXPROCS(0), eachWorkers) {
		wg.Go(func() {
			for i := range next {
				if errs[i] = do(i); errs[i] != nil {
					failed.Store(true)
				}
			}
		})
	}
	for i := 0; i < n && !failed.Load(); i++ {
		next <- i
	}
	close(next)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// Body returns the body of a record. A private body that is not available
// returns ErrPrivateMissing, or ErrPlainHash for a record made before refs.
func (s *Set) Body(r *Record) ([]byte, error) {
	if r.Body.Storage == Private {
		switch {
		case r.Body.PlainHash():
			return nil, ErrPlainHash
		case r.Body.Ref == "":
			return nil, fmt.Errorf("%s: %w", r.ID, ErrPrivateMissing)
		}
		return s.Private.Get(r.Body.Ref)
	}
	if err := checkName(r.Body.SHA256); err != nil {
		return nil, fmt.Errorf("%s: %w", r.ID, err)
	}
	body, err := os.ReadFile(filepath.Join(s.Dir, "bodies", r.Body.SHA256))
	if err != nil {
		return nil, err
	}
	if Sum(body) != r.Body.SHA256 {
		return nil, fmt.Errorf("body %s: content does not match its name", r.Body.SHA256)
	}
	return body, nil
}

// Write stores a record and its body. A nil body keeps the record as it is
// (a private body whose raw content is not available keeps only its name).
func (s *Set) Write(r *Record, body []byte) error { return s.write(r, body, false) }

// WriteOwned is Write for a body nothing reads afterwards (a capture's
// response): a private one is encrypted in its own storage, so a large body
// is not held twice, and body must not be used after the call.
func (s *Set) WriteOwned(r *Record, body []byte) error { return s.write(r, body, true) }

func (s *Set) write(r *Record, body []byte, owned bool) error {
	if !SimpleName(r.ID) {
		return fmt.Errorf("invalid request id %q", r.ID)
	}
	r.Version = Version
	if body != nil {
		r.Body.Size = int64(len(body))
		switch r.Body.Storage {
		case Private:
			ref, err := s.Private.put(body, owned)
			if err != nil {
				return err
			}
			r.Body.SHA256, r.Body.Ref = "", ref
		case Public:
			r.Body.SHA256, r.Body.Ref = Sum(body), ""
			// A body is named by its content: one this set wrote already
			// is this body (two requests with the same response).
			if s.wrote(r.Body.SHA256, false) {
				break
			}
			if err := WriteFile(filepath.Join(s.Dir, "bodies", r.Body.SHA256), body, 0o644); err != nil {
				return err
			}
			s.wrote(r.Body.SHA256, true)
		default:
			return fmt.Errorf("%s: unknown storage %q", r.ID, r.Body.Storage)
		}
	}
	return WriteJSON(s.recordPath(r.ID), r)
}

// WriteMeta stores the set's description. A description that marks the
// set complete is handed to the disk before WriteMeta returns (the file and
// its folder are synced, syncFile), so a crash of the system does not leave
// a set whose description is lost or empty; the records and bodies are not
// synced one by one, which would cost a capture more than its requests.
func (s *Set) WriteMeta(m Meta) error {
	m.Version = Version
	raw, err := marshalJSON(m)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(s.Dir, "meta.json"), 0o644, !m.Incomplete, raw)
}

// Meta reads meta.json; a set without one returns nil without error.
func (s *Set) Meta() (*Meta, error) {
	path := filepath.Join(s.Dir, "meta.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.Version != Version {
		return nil, fmt.Errorf("%s: unsupported version %d", path, m.Version)
	}
	return &m, nil
}

// Sum is the hex SHA-256 of b.
func Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// WriteJSON writes v with stable formatting (one-space indent, trailing newline).
func WriteJSON(path string, v any) error {
	raw, err := marshalJSON(v)
	if err != nil {
		return err
	}
	return WriteFile(path, raw, 0o644)
}

func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// TempPrefix starts the name of a file being written (WriteFile).
const TempPrefix = ".tmp-"

// WriteFile writes atomically: a temporary file in the same directory is
// renamed over the target, so an interrupted run never leaves half a file.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	return writeFile(path, perm, false, data)
}

// writeFile is WriteFile of a file written in parts; durable syncs the file
// before the rename and the folder after it.
func writeFile(path string, perm os.FileMode, durable bool, parts ...[]byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	for _, data := range parts {
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if durable {
		if err := syncFile(tmp); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	chaos.Point("snapshot.rename")
	if err := os.Rename(tmp.Name(), path); err != nil || !durable {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename in dir durable. Not every system can sync a
// folder (Windows cannot open one for it): there the rename is left to the
// file system.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	syncFile(d)
	return nil
}

// RemoveStale removes the files a write left behind when its process was
// killed (TempPrefix, up to MaxBody each), under dir, that are older than
// before; the folder dir itself is left out (another process may be writing
// a file of its own there). The caller holds the store's lock, so no other
// process is writing below it.
func RemoveStale(dir string, before time.Time) {
	filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Dir(path) == dir || !strings.HasPrefix(e.Name(), TempPrefix) {
			return nil
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			os.Remove(path)
		}
		return nil
	})
}
