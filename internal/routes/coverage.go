package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/glob"
)

// ErrorTemplates are the templates of generated requests that check an
// error case: they show a route answers, not that it works.
var ErrorTemplates = []string{"not-found", "invalid-param", "anonymous", "empty-body"}

// JSONErrors is the template that sends each request of ErrorTemplates once
// more with Accept: application/json; its requests check an error case too.
const JSONErrors = "json-errors"

// expectsError reports a generated request that checks an error case.
func expectsError(r catalog.Request) bool {
	return r.Source != nil && (slices.Contains(ErrorTemplates, r.Source.Template) || r.Source.Template == JSONErrors)
}

// meantToSucceed reports a request the catalog says should succeed: one
// generate made with real parameter values (ok, json) and, for a route that
// takes a body, a sample body. A request written by hand or imported may
// expect a client error (a wrong password, a rate limit); a generated one
// without a sample body is a validation error (routes without a sample are
// reported apart).
func meantToSucceed(r catalog.Request) bool {
	if r.Source == nil || (r.Source.Template != "ok" && r.Source.Template != "json") {
		return false
	}
	return !takesBody(Route{Methods: []string{r.Method}}) || r.Multipart != nil || !emptyBody(r.Body)
}

// EmptyBody is the template whose request sends a route that validates its
// body an empty one: a validation error.
const EmptyBody = "empty-body"

// sendsInvalidBody reports a request whose answer is the validation error
// of its route, so that a changed message is compared. Other error cases
// (not-found, anonymous) are answered before the body is validated.
//
//   - With a baseline answer: an empty-body request or its JSON twin, or
//     one written by hand or imported, that answered 400 or 422. A
//     framework that answers a browser with a redirect back (Laravel's
//     302) has not shown its message.
//   - Without one: an empty-body request, and on a route of a framework
//     that negotiates errors by Accept (Route.ErrorsByAccept) only one
//     that asks for JSON, such as the json-errors twin. A request written
//     by hand is not known to be refused.
func sendsInvalidBody(r catalog.Request, route *Route, a Answer, answered bool) bool {
	generated := r.Source != nil && r.Source.Template != ""
	invalid := !generated || r.Source.Template == EmptyBody || r.Source.Template == JSONErrors && emptyBody(r.Body)
	switch {
	case !invalid:
		return false
	case answered:
		return a.Status == 400 || a.Status == 422
	case !generated:
		return false
	}
	return !route.ErrorsByAccept || acceptsJSON(r)
}

// acceptsJSON reports a request that asks for JSON (Accept).
func acceptsJSON(r catalog.Request) bool {
	for _, h := range r.Headers {
		if strings.EqualFold(h[0], "accept") && strings.Contains(strings.ToLower(h[1]), "json") {
			return true
		}
	}
	return false
}

func emptyBody(b json.RawMessage) bool {
	var c bytes.Buffer
	return len(b) == 0 || (json.Compact(&c, b) == nil && (c.String() == "{}" || c.String() == "null"))
}

// Answer is what a baseline recorded for a request.
type Answer struct {
	Status   int    // 0 when there was no answer
	Error    string // the request failed: a timeout, a connection error, repeated sends cut short
	Repeat   string // statuses of a repeated request: "200×60, 429×1"
	Location string // the location header
}

// loginPaths are the last segments of the paths of sign-in pages: a request
// meant to succeed that is redirected there did not get in.
var loginPaths = []string{"login", "signin", "sign-in", "sign_in", "log-in", "logon"}

func loginRedirect(location string) (string, bool) {
	u, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	p := strings.TrimSuffix(u.Path, "/")
	last := strings.ToLower(p[strings.LastIndexByte(p, '/')+1:])
	return u.Path, slices.Contains(loginPaths, last)
}

// serverErrorAmong reports a 5xx among the statuses of a repeated request.
func serverErrorAmong(repeat string) bool {
	for _, part := range strings.Split(repeat, ", ") {
		code, _, _ := strings.Cut(part, "×")
		if n, err := strconv.Atoi(code); err == nil && n >= 500 {
			return true
		}
	}
	return false
}

// Failure is a request meant to succeed whose answer in the baseline is an
// error.
type Failure struct {
	ID     string `json:"id"`
	Status int    `json:"status"`           // 0: no answer
	Detail string `json:"detail,omitempty"` // what failed besides the status
	Reason string `json:"reason,omitempty"` // a known failure: why it is accepted
	Ref    string `json:"ref,omitempty"`
	// KnownAs is the status of a known failure that matches the request but
	// no longer its failure: the failure changed.
	KnownAs string `json:"known_status,omitempty"`
}

func (f Failure) String() string {
	s := f.ID + ": "
	switch {
	case f.Status == 0:
		s += f.Detail
	case f.Detail != "":
		s += fmt.Sprintf("%d, %s", f.Status, f.Detail)
	default:
		s += strconv.Itoa(f.Status)
	}
	if f.KnownAs != "" {
		s += " (its known failure is status " + f.KnownAs + ": the failure changed)"
	}
	if f.Reason != "" {
		s += " (known: " + f.Reason
		if f.Ref != "" {
			s += ", " + f.Ref
		}
		s += ")"
	}
	return s
}

// failure says whether the answer of a request is a failure: no answer, a
// server error (of any of a repeated request's sends), or, for a request
// meant to succeed, a client error or a redirect to a sign-in page. An
// error case (not-found…) and a command step never fail here.
func failure(req catalog.Request, a Answer) (Failure, bool) {
	if req.IsCommand() || expectsError(req) {
		return Failure{}, false
	}
	f := Failure{ID: req.ID, Status: a.Status}
	switch {
	case a.Error != "":
		f.Status, f.Detail = 0, "no answer ("+a.Error+")"
	case a.Status == 0:
		f.Detail = "no answer"
	case a.Status >= 500:
	case a.Repeat != "" && serverErrorAmong(a.Repeat):
		f.Detail = "a server error among the repeated sends (" + a.Repeat + ")"
	case !meantToSucceed(req):
		return Failure{}, false
	case a.Status >= 400:
	case a.Status >= 300:
		path, ok := loginRedirect(a.Location)
		if !ok {
			return Failure{}, false
		}
		f.Detail = "redirect to the sign-in page " + path
	default:
		return Failure{}, false
	}
	return f, true
}

// Health is how the requests of a catalog fared in a baseline.
type Health struct {
	Failed []Failure      // requests meant to succeed that failed, in catalog order
	Known  []Failure      // failed requests a known failure acknowledges
	Stale  []string       // known failures that match no request, or whose requests succeed now
	Broad  []string       // known failures that acknowledge failures on many routes (see CheckWith)
	ackBy  map[string]int // request ID → the known failure that acknowledges it
}

func validKnownStatus(s string) bool {
	switch s {
	case "", "error", "timeout", "4xx", "5xx":
		return true
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 100 && n <= 599
}

// acknowledges reports whether a known failure's status covers the failure
// of a request with this answer.
func (k KnownFailure) acknowledges(a Answer) bool {
	noAnswer := a.Error != "" || a.Status == 0
	statuses := []int{a.Status}
	for _, part := range strings.Split(a.Repeat, ", ") {
		code, _, _ := strings.Cut(part, "×")
		if n, err := strconv.Atoi(code); err == nil {
			statuses = append(statuses, n)
		}
	}
	has := func(ok func(int) bool) bool { return !noAnswer && slices.ContainsFunc(statuses, ok) }
	switch k.Status {
	case "":
		return true
	case "error":
		return noAnswer
	case "timeout":
		e := strings.ToLower(a.Error)
		return noAnswer && (strings.Contains(e, "timeout") || strings.Contains(e, "deadline exceeded"))
	case "4xx":
		return has(func(n int) bool { return n >= 400 && n < 500 })
	case "5xx":
		return has(func(n int) bool { return n >= 500 })
	}
	want, _ := strconv.Atoi(k.Status)
	return has(func(n int) bool { return n == want })
}

// CheckHealth judges the answers of a baseline (by request ID; a request
// without one is not judged). A request takes the first known failure
// whose glob matches it; when that one names a status the failure does not
// have, the request is failed, with a note that its failure changed.
func CheckHealth(cat *catalog.Catalog, answers map[string]Answer, known []KnownFailure) *Health {
	h := &Health{ackBy: map[string]int{}}
	matched := make([]bool, len(known))
	stillFails := make([]bool, len(known))
	open := make([]bool, len(known)) // a request without an answer: not stale
	for _, req := range cat.Requests {
		first := -1
		for i := range known {
			if glob.Match(known[i].Request, req.ID) {
				matched[i] = true
				if first < 0 {
					first = i
				}
				if _, ok := answers[req.ID]; !ok {
					open[i] = true
				}
			}
		}
		a, ok := answers[req.ID]
		if !ok {
			continue
		}
		f, failed := failure(req, a)
		if !failed {
			continue
		}
		if first < 0 {
			h.Failed = append(h.Failed, f)
			continue
		}
		for i := range known {
			stillFails[i] = stillFails[i] || glob.Match(known[i].Request, req.ID)
		}
		k := known[first]
		if !k.acknowledges(a) {
			f.KnownAs = k.Status
			h.Failed = append(h.Failed, f)
			continue
		}
		f.Reason, f.Ref = k.Reason, k.Ref
		h.Known = append(h.Known, f)
		h.ackBy[req.ID] = first
	}
	for i, k := range known {
		switch {
		case !matched[i]:
			h.Stale = append(h.Stale, k.Request+" (no request)")
		case answers != nil && !stillFails[i] && !open[i]:
			h.Stale = append(h.Stale, k.Request+" (its requests succeed)")
		}
	}
	return h
}

// BroadKnown is how many routes one known failure may acknowledge before
// coverage warns that its glob is broad: a glob such as post-* would hide
// every new failure it reaches.
const BroadKnown = 5

// FailedRoute is a route whose requests meant to succeed all failed.
type FailedRoute struct {
	Route    string    `json:"route"`
	Requests []Failure `json:"requests"`
}

// Coverage is the result of checking a catalog against the inventory.
type Coverage struct {
	Covered   []string            // route keys reached by a request
	Excluded  map[string]string   // route key → reason
	Uncovered []string            // neither reached by a request that should succeed nor excluded
	Negative  []string            // reached only by requests that expect an error (not-found, invalid-param…); also in Uncovered
	Requests  map[string][]string // route key → request IDs
	Unknown   []string            // requests that reach no route
	Stale     []string            // exclusions that match no route
	NoBody    []string            // routes that take a body and have no sample
	// NoInvalidBody are covered routes that take a body with a sample (the
	// rules it is validated against) and that no request sends an invalid
	// body (sendsInvalidBody): a changed validation error passes unseen.
	NoInvalidBody []string
	// With the answers of a baseline:
	Failed    []FailedRoute // routes whose requests meant to succeed all failed; also in Uncovered
	Known     []string      // routes whose requests failed, each acknowledged as a known failure: neither covered nor failed
	ErrorOnly []string      // covered only by requests that answered a client error that is not a failure (written by hand); also in Covered
	Health    *Health
}

// CheckWith computes coverage, with the answers a baseline recorded by
// request ID (nil: no baseline, the catalog alone). A request covers its
// route only when it did not fail in the baseline (see failure), or when
// the baseline has no answer for it yet: a comparison checks that answers
// stay equal, so an error already in the baseline would pass forever.
func CheckWith(inv *Inventory, cat *catalog.Catalog, excl *Exclusions, answers map[string]Answer) *Coverage {
	h := CheckHealth(cat, answers, excl.KnownFailures)
	failed := map[string]Failure{}
	for _, f := range h.Failed {
		failed[f.ID] = f
	}
	// Coverage is counted per method of a route (see Units): GET /items and
	// POST /items are checked separately.
	c := &Coverage{Excluded: map[string]string{}, Requests: map[string][]string{}, Health: h}
	unitKey := func(r *Route, unit string) string {
		k := unit + " " + r.Path
		if r.Domain != "" {
			k = r.Domain + " " + k
		}
		if r.Target != "" {
			k = r.Target + ": " + k
		}
		return k
	}
	const (
		succeeded = 1 << iota
		answeredError
		acknowledged
	)
	reach := map[string]int{}
	invalid := map[string]bool{} // route keys a request sends an invalid body
	failures := map[string][]Failure{}
	ackRoutes := map[int][]string{} // known failure → the routes of its requests
	m := NewMatcher(inv)
	for _, req := range cat.Requests {
		if req.IsCommand() {
			continue // a command step reaches no route
		}
		r := m.Match(req)
		if r == nil {
			c.Unknown = append(c.Unknown, req.ID)
			continue
		}
		k := unitKey(r, r.UnitOf(req.Method))
		c.Requests[k] = append(c.Requests[k], req.ID)
		a, ok := answers[req.ID]
		if sendsInvalidBody(req, r, a, ok) {
			invalid[k] = true
		}
		if expectsError(req) {
			continue
		}
		switch f, bad := failed[req.ID]; {
		case bad:
			failures[k] = append(failures[k], f)
		case h.acked(req.ID):
			reach[k] |= acknowledged
			ackRoutes[h.ackBy[req.ID]] = append(ackRoutes[h.ackBy[req.ID]], k)
		case ok && a.Status >= 400:
			reach[k] |= answeredError
		default:
			reach[k] |= succeeded
		}
	}
	used := map[int]bool{}
	for i := range inv.Routes {
		r := &inv.Routes[i]
		for _, unit := range r.Units() {
			k := unitKey(r, unit)
			noBody := takesBody(Route{Methods: []string{unit}}) && len(r.Body) == 0 && excl.ExcludedUnit(*r, unit) == nil
			validated := takesBody(Route{Methods: []string{unit}}) && len(r.Body) > 0 && !invalid[k]
			switch x := excl.ExcludedUnit(*r, unit); {
			case reach[k]&succeeded != 0:
				c.Covered = append(c.Covered, k)
				if validated {
					c.NoInvalidBody = append(c.NoInvalidBody, k)
				}
			case len(failures[k]) > 0:
				c.Uncovered = append(c.Uncovered, k)
				c.Failed = append(c.Failed, FailedRoute{Route: k, Requests: failures[k]})
			case reach[k]&answeredError != 0:
				c.Covered = append(c.Covered, k)
				if !noBody {
					c.ErrorOnly = append(c.ErrorOnly, k)
				}
				if validated {
					c.NoInvalidBody = append(c.NoInvalidBody, k)
				}
			case reach[k]&acknowledged != 0:
				c.Known = append(c.Known, k)
			case x != nil:
				c.Excluded[k] = x.Reason
			default:
				c.Uncovered = append(c.Uncovered, k)
				if len(c.Requests[k]) > 0 {
					c.Negative = append(c.Negative, k)
				}
			}
			if noBody {
				c.NoBody = append(c.NoBody, k)
			}
		}
		for i := range excl.Routes {
			if excl.Routes[i].excludes(*r) {
				used[i] = true
			}
		}
	}
	for i, x := range excl.Routes {
		if !used[i] {
			c.Stale = append(c.Stale, x.Route)
		}
	}
	for i, k := range excl.KnownFailures {
		if n := len(slices.Compact(slices.Sorted(slices.Values(ackRoutes[i])))); n > BroadKnown {
			h.Broad = append(h.Broad, fmt.Sprintf("%s (%d routes)", k.Request, n))
		}
	}
	return c
}

func (h *Health) acked(id string) bool {
	_, ok := h.ackBy[id]
	return ok
}
