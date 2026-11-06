// Package catalog is the list of requests DriftGate sends: one entry is one
// request, and variants of a route are separate entries with their own ID.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/snapshot"
)

const Version = 1

// Request is one catalog entry.
type Request struct {
	ID     string            `json:"id"`
	Target string            `json:"target,omitempty"` // the service, with several targets
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Params map[string]string `json:"params,omitempty"`
	// ParamsFrom takes parameter values from the response of an earlier
	// request of the same capture.
	ParamsFrom map[string]From `json:"params_from,omitempty"`
	Headers    [][2]string     `json:"headers,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	Identity   string          `json:"identity,omitempty"`
	// Mutates says whether the request changes data; unset, a method other
	// than GET, HEAD and OPTIONS does (see Mutating). A search sent as POST
	// is written mutates: false.
	Mutates *bool   `json:"mutates,omitempty"`
	Storage string  `json:"storage"`
	Level   string  `json:"level,omitempty"`
	Observe bool    `json:"observe,omitempty"`
	Source  *Source `json:"source,omitempty"`
	// A multipart body with files, sending N times, a command step.
	Multipart    *Multipart `json:"multipart,omitempty"`
	Repeat       int        `json:"repeat,omitempty"`
	Command      []string   `json:"command,omitempty"`
	Dir          string     `json:"dir,omitempty"`   // where a command step runs; default the project root
	Stdin        string     `json:"stdin,omitempty"` // a file given to a command step as standard input
	Output       bool       `json:"output,omitempty"`
	OutputIgnore []string   `json:"output_ignore,omitempty"`
}

// From names a value in the body of another request's response.
type From struct {
	Request string `json:"request"`
	Path    string `json:"path"` // JSON path, e.g. $.entry.id or $.entries[0].id
}

// Source records where an entry came from.
type Source struct {
	Route    string `json:"route,omitempty"`
	Template string `json:"template,omitempty"`
	Import   string `json:"import,omitempty"`
}

// Sequence is a group of requests that run in order.
type Sequence struct {
	ID       string   `json:"id"`
	Restore  string   `json:"restore,omitempty"` // "before" or empty
	Requests []string `json:"requests"`
}

// Catalog is driftgate/catalog.json.
type Catalog struct {
	Version   int        `json:"version"`
	Requests  []Request  `json:"requests"`
	Sequences []Sequence `json:"sequences,omitempty"`
}

// SafeMethod reports a method that only reads: GET, HEAD, OPTIONS, or none
// (which is sent as GET).
func SafeMethod(method string) bool {
	switch method {
	case "", "GET", "HEAD", "OPTIONS":
		return true
	}
	return false
}

// MutatesFor is the mutates field a new entry gets from its method: true for
// one that is not safe, unset otherwise.
func MutatesFor(method string) *bool {
	if SafeMethod(method) {
		return nil
	}
	t := true
	return &t
}

// Mutating reports a request that changes data: mutates as written, else
// any method that is not safe. A request that changes data runs after a
// restore, one at a time, and needs a state with a restore command.
func (r Request) Mutating() bool {
	if r.Mutates != nil {
		return *r.Mutates
	}
	return !SafeMethod(r.Method)
}

// LiteralID is the first segment of a request path that is a number
// written in the path itself (17 of /api/users/17), not a {parameter}:
// on another copy of the data that id may be a real record. Empty when
// there is none; the query is not read.
//
// Two kinds of number are not ids and are passed over: an API version, a
// number of one or two digits right after a segment api or v (/api/2/users,
// /v/1/items; v2 is not a number anyway), and a date, a year from 1900 to
// 2099 followed by a month (1 to 12) and perhaps a day (1 to 31), as in
// /reports/2024/05 or /posts/2024/5/17. A year alone (/stats/2024) may as
// well be an id, and is one.
func LiteralID(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(path, "/")
	for i := 0; i < len(segs); i++ {
		seg := segs[i]
		if !number(seg) {
			continue
		}
		if i > 0 && len(seg) <= 2 && (strings.EqualFold(segs[i-1], "api") || strings.EqualFold(segs[i-1], "v")) {
			continue // a version
		}
		if inRange(seg, 4, 1900, 2099) && i+1 < len(segs) && inRange(segs[i+1], 2, 1, 12) {
			i++ // the month
			if i+1 < len(segs) && inRange(segs[i+1], 2, 1, 31) {
				i++ // the day
			}
			continue
		}
		return seg
	}
	return ""
}

// number reports a path segment of digits only.
func number(seg string) bool { return seg != "" && strings.Trim(seg, "0123456789") == "" }

// inRange reports a segment of up to digits digits whose value is from lo
// to hi.
func inRange(seg string, digits, lo, hi int) bool {
	if !number(seg) || len(seg) > digits {
		return false
	}
	n, _ := strconv.Atoi(seg)
	return lo <= n && n <= hi
}

// MutatingByMethod reports a request that changes data only because of its
// method: the catalog does not say mutates. A search sent as POST is one
// until it is written mutates: false.
func (r Request) MutatingByMethod() bool { return r.Mutates == nil && !SafeMethod(r.Method) }

// validMethod reports an HTTP method token (RFC 9110: letters, digits and
// !#$%&'*+-.^_`|~).
func validMethod(m string) bool {
	for i := 0; i < len(m); i++ {
		c := m[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// CheckPath refuses a request or route path that does not start with "/".
// The path is appended to the base URL, so "@host/x" or ".example.com/x"
// would send the request, with the identity's token, to another host.
func CheckPath(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("path %q must start with /", path)
	}
	return nil
}

// Load reads and validates a catalog.
func Load(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("catalog not found: %s", path)
	}
	if err != nil {
		return nil, err
	}
	// Unknown fields are errors, as in the YAML files: a misspelled
	// "mutate" must not silently send a request that changes data as if
	// it only read.
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.Validate()
}

// Validate checks IDs, methods, storage and levels. A method is written in
// upper case, as servers and the safe methods are: "post" becomes POST.
func (c *Catalog) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("catalog: unsupported version %d", c.Version)
	}
	seen := map[string]bool{}
	folded := map[string]string{} // an ID names the file of its record
	for i := range c.Requests {
		r := &c.Requests[i]
		if !snapshot.SimpleName(r.ID) {
			return fmt.Errorf("catalog: invalid request id %q (letters, digits, . _ -; not a name Windows reserves such as con or nul)", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("catalog: duplicate request id %q", r.ID)
		}
		if other, ok := folded[snapshot.FoldName(r.ID)]; ok {
			return fmt.Errorf("catalog: request ids %q and %q differ only in case; on macOS and Windows they would share one record file", other, r.ID)
		}
		seen[r.ID], folded[snapshot.FoldName(r.ID)] = true, r.ID
		if !r.IsCommand() { // a command step has no method or path (validateStep checks it has none)
			if err := CheckPath(r.Path); err != nil {
				return fmt.Errorf("catalog: %s: %w", r.ID, err)
			}
			if !validMethod(r.Method) {
				return fmt.Errorf("catalog: %s: invalid method %q", r.ID, r.Method)
			}
			r.Method = strings.ToUpper(r.Method)
		}
		if r.Storage != snapshot.Public && r.Storage != snapshot.Private {
			return fmt.Errorf("catalog: %s: storage must be public or private", r.ID)
		}
		if r.Level != "" {
			if _, err := compare.ParseLevel(r.Level); err != nil {
				return fmt.Errorf("catalog: %s: %w", r.ID, err)
			}
		}
		if err := validateStep(*r); err != nil {
			return fmt.Errorf("catalog: %s: %w", r.ID, err)
		}
	}
	seqOf, pos := map[string]string{}, map[string]int{}
	seqIDs := map[string]string{}
	for _, s := range c.Sequences {
		if !snapshot.SimpleName(s.ID) {
			return fmt.Errorf("catalog: invalid sequence id %q (letters, digits, . _ -; not a name Windows reserves such as con or nul)", s.ID)
		}
		if other, ok := seqIDs[snapshot.FoldName(s.ID)]; ok {
			return fmt.Errorf("catalog: sequence ids %q and %q are the same or differ only in case", other, s.ID)
		}
		seqIDs[snapshot.FoldName(s.ID)] = s.ID
		for i, id := range s.Requests {
			if !seen[id] {
				return fmt.Errorf("catalog: sequence %s: unknown request %q", s.ID, id)
			}
			if other, ok := seqOf[id]; ok {
				return fmt.Errorf("catalog: %s is in sequences %s and %s; a request runs once", id, other, s.ID)
			}
			seqOf[id], pos[id] = s.ID, i
		}
	}
	// A value taken from another response must exist when the request is
	// sent: the giver runs earlier in the same sequence.
	for _, r := range c.Requests {
		for name, f := range r.ParamsFrom {
			switch {
			case !seen[f.Request]:
				return fmt.Errorf("catalog: %s: parameter %s from unknown request %q", r.ID, name, f.Request)
			case seqOf[r.ID] == "" || seqOf[r.ID] != seqOf[f.Request] || pos[f.Request] >= pos[r.ID]:
				return fmt.Errorf("catalog: %s: parameter %s comes from %s, which must run earlier in the same sequence", r.ID, name, f.Request)
			}
		}
	}
	return nil
}

// Save writes the catalog with stable formatting.
func (c *Catalog) Save(path string) error {
	c.Version = Version
	return snapshot.WriteJSON(path, c)
}
