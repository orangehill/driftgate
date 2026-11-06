package capture

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// Retries. A read-only request of a safe method (GET, HEAD, OPTIONS) whose
// send failed as a server under load does now and then (loadFailed) is
// sent again, up to the service's retries. A request that may change data
// (whatever its mutates says), a sequence, a command step and a repeated
// request are never sent twice. The record keeps the first send's outcome
// and whether a later send recovered, so no report, and no baseline made
// from it, takes it for a clean answer.
//
// The resends wait until the read-only phase is over and go one at a time
// (retryDeferred): the load that made the first send fail is gone then, so
// a resend that passes is evidence, not luck. (A request whose parameter
// comes from another response is in a sequence, which is never retried.)
// Retries are not spent on what cannot recover: a request whose baseline
// response failed the same way (known to be broken), and the load failures
// of a service once enough of its resends failed again (budget).

// Budgets: how many resends of a service may fail again, by the kind of
// failure, before the rest of that kind are no longer sent again in this
// capture. A resend goes alone, without load: one that fails again shows
// the failure is not load. A timeout costs a whole time limit, so one is
// enough (16 endpoints that always hang past a 1 s timeout: 1 s more than
// retry: 0, not 16 s); a status (an application in maintenance mode
// answering 503 to everything) costs little each, but every request of
// the catalog at once.
const (
	timeoutBudget = 1
	statusBudget  = 2
)

// retrying is the retry state of one capture.
type retrying struct {
	order    map[string]int // place in the catalog
	mu       sync.Mutex
	deferred []deferredSend
	again    map[[2]string]int // resends that failed again, by service and kind (failureKind)
}

// deferredSend is a read-only request whose first send failed under load,
// with that response, kept until it is sent again.
type deferredSend struct {
	order int // in the catalog
	req   catalog.Request
	first httpx.Result
	retry *snapshot.Retry
}

func newRetrying(readonly []catalog.Request) *retrying {
	rt := &retrying{order: map[string]int{}, again: map[[2]string]int{}}
	for i, req := range readonly {
		rt.order[req.ID] = i
	}
	return rt
}

// sendRead sends a read-only request of the recorded phase. A load failure
// that may be retried is put aside for retryDeferred: ok is then false,
// and the result belongs to the deferred send.
func (r *runner) sendRead(ctx context.Context, req catalog.Request) (httpx.Result, bool) {
	// A request the restore's ended sessions made fail is sent again at
	// once; a load failure of that second send is retried as any other
	// (its record then keeps both).
	res := r.resendAfterRestore(ctx, req, r.send(ctx, req))
	first := r.retryable(req, res)
	if first == nil {
		return res, true
	}
	r.retry.mu.Lock()
	r.retry.deferred = append(r.retry.deferred, deferredSend{order: r.retry.order[req.ID], req: req, first: res, retry: first})
	r.retry.mu.Unlock()
	return httpx.Result{}, false
}

// retryDeferred sends the requests put aside again, one at a time in
// catalog order, and writes their responses in the background (a write
// queue as the read-only phase has): when a budget is spent, most of them
// are only written.
func (r *runner) retryDeferred(ctx context.Context) error {
	list := r.retry.deferred
	r.retry.deferred = nil
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].order < list[j].order })
	writers := 1
	for _, s := range r.services {
		writers = max(writers, cap(s.slots))
	}
	q := newWriteQueue(r, writers, newBudget(queueBudget))
	skipped := map[[2]string]int{}
	var err error
	for i, d := range list {
		if err = ctx.Err(); err != nil {
			release(list[i:])
			break
		}
		res := d.first
		if kind := r.failureKind(d.req.Target, d.retry); r.retry.spent(d.req.Target, kind) {
			skipped[[2]string{d.req.Target, kind}]++
		} else {
			res = r.resend(ctx, d.req, res, d.retry)
		}
		if r.sum.Tracked {
			res.Record.SideEffects = &snapshot.SideEffects{Items: []string{}}
		}
		if err = q.add(d.req, res); err != nil {
			res.Release()
			release(list[i+1:])
			break
		}
	}
	if cerr := q.close(); err == nil {
		err = cerr
	}
	for k, n := range skipped {
		of := ""
		if k[0] != "" {
			of = " of " + k[0]
		}
		if k[1] == "timeout" {
			fmt.Fprintf(r.out, "%d %s%s not sent again: %d resent alone timed out again\n", n, plural(n, "timeout", "timeouts"), of, timeoutBudget)
		} else {
			fmt.Fprintf(r.out, "%d 502, 503 or 504 %s%s not sent again: %d resent alone got one again\n", n, plural(n, "answer", "answers"), of, statusBudget)
		}
	}
	return err
}

// release gives back the responses put aside, of a capture that stops.
func (rt *retrying) release() {
	release(rt.deferred)
	rt.deferred = nil
}

func release(list []deferredSend) {
	for _, d := range list {
		d.first.Release()
	}
}

// failureKind is the kind of a load failure, for its budget: "timeout"
// for a timeout and for a 504 that came no sooner than the time limit (a
// proxy that waited as long), "status" for any other status.
func (r *runner) failureKind(service string, f *snapshot.Retry) string {
	if f.Error == "timeout" || f.Status == http.StatusGatewayTimeout && f.MS >= r.services[service].timeout.Milliseconds() {
		return "timeout"
	}
	if f.Error != "" {
		return f.Error
	}
	return "status"
}

// spent reports that the load failures of a kind of a service are no
// longer sent again: its budget of resends that failed again is used up.
func (rt *retrying) spent(service, kind string) bool {
	budget := statusBudget
	switch kind {
	case "timeout":
		budget = timeoutBudget
	case "status":
	default:
		return false // a reset connection costs little and has no budget
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.again[[2]string{service, kind}] >= budget
}

// resend sends a request again after its first send (res) failed under
// load, up to the service's retries, and returns the last response, its
// record marked with the first outcome. It recovered only when the last
// send got a response that is not a load failure: one that failed in
// another way (a refused connection, an interrupt, a body too large) did
// not.
func (r *runner) resend(ctx context.Context, req catalog.Request, res httpx.Result, first *snapshot.Retry) httpx.Result {
	resent := res.Record.Resent // sent again after a restore before this load failure
	for sends := 1; sends <= r.services[req.Target].retries && ctx.Err() == nil; sends++ {
		res.Release()
		res = r.send(ctx, req)
		first.Sends = sends + 1
		res.Record.Retry, res.Record.Resent = first, resent
		last := loadFailed(res)
		switch {
		case last == nil && res.Record.Error == "":
			first.Recovered = true
			return res
		case last == nil:
			return res // failed otherwise: no use sending again
		}
		if sends == r.services[req.Target].retries {
			r.retry.mu.Lock()
			r.retry.again[[2]string{req.Target, r.failureKind(req.Target, last)}]++
			r.retry.mu.Unlock()
		}
	}
	return res
}

// retryable describes the first send of a request that may be sent again:
// a safe method, a service with retries, a load failure (loadFailed), a
// baseline response that did not fail the same way, and no side effects
// in that response (knownWriters). Nil otherwise.
func (r *runner) retryable(req catalog.Request, res httpx.Result) *snapshot.Retry {
	if r.retry == nil || !catalog.SafeMethod(req.Method) || r.services[req.Target].retries == 0 {
		return nil
	}
	first := loadFailed(res)
	if first == nil || r.knownBroken(req.ID, res) {
		return nil
	}
	// A GET can write: one the baseline knows writes is never sent twice.
	if known, _ := r.knownWriters([]catalog.Request{req}); len(known) > 0 {
		return nil
	}
	return first
}

// knownBroken reports a request whose baseline response failed as this
// send did: the same status, a failed request both, or a retry that did
// not recover. Sending it again would only cost time.
func (r *runner) knownBroken(id string, res httpx.Result) bool {
	if r.base == nil {
		return false
	}
	rec, err := r.base.Record(id)
	if err != nil || rec == nil {
		return false
	}
	switch {
	case rec.Retry != nil && !rec.Retry.Recovered:
		return true
	case rec.Error != "":
		return res.Record.Error != ""
	}
	return res.Record.Error == "" && rec.Status == res.Record.Status
}

// loadFailed describes a send that failed as a server under load does now
// and then, for a retry: a timeout, a connection reset or closed early, or
// a 502, 503 or 504 status (a proxy or a server out of workers). A 500 is
// an exception of the application, often the first call after a restore
// (a cold cache), and stays what it is. Nil for any other outcome.
func loadFailed(res httpx.Result) *snapshot.Retry {
	ms := res.Duration.Milliseconds()
	switch {
	case res.LoadFailure != "":
		return &snapshot.Retry{Error: res.LoadFailure, MS: ms}
	case res.Record.Error != "":
		return nil
	}
	switch res.Record.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &snapshot.Retry{Status: res.Record.Status, MS: ms}
	}
	return nil
}
