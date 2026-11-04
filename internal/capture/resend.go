package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// resendAfterRestore sends a request of the read-only phase once more when
// its answer was a 5xx right after a restore that ended the application's
// own sessions with its database (SQL Server's restore of the whole
// database): each of the application's pooled connections then fails
// once, on its next use. It resends only:
//
//   - a GET or HEAD, sent once (not a repeat:, a step of a sequence or a
//     unit, which are not in this phase), to a target on an allowed host;
//   - not one the baseline knows writes (its record has side effects):
//     a GET can write, and a GET that writes is never sent twice;
//   - within the budget of the restore: as many requests as it ended
//     sessions (tracker.resends), across all requests.
//
// The answer kept is the second, whatever it is (a 5xx that stays is the
// application's); the record says it was sent again and what the first
// answer was (Record.Resent), and every report shows that.
func (r *runner) resendAfterRestore(ctx context.Context, req catalog.Request, res httpx.Result) httpx.Result {
	if r.tr == nil || res.Record == nil || res.Record.Error != "" || res.Record.Status < 500 || ctx.Err() != nil ||
		req.Method != http.MethodGet && req.Method != http.MethodHead || req.Repeat > 1 || req.IsCommand() {
		return res
	}
	if s := r.services[req.Target]; s == nil || s.remote {
		return res
	}
	if known, _ := r.knownWriters([]catalog.Request{req}); len(known) > 0 {
		return res
	}
	if !takeResend(r.tr) {
		return res
	}
	first, after := res.Record.Status, r.tr.resentAfter
	fmt.Fprintf(r.log, "%s: %d after %s; sending it again once\n", req.ID, first, after)
	res.Release()
	res = r.send(ctx, req)
	if res.Record != nil {
		res.Record.Resent = &snapshot.Resent{After: after, First: first}
	}
	return res
}

// takeResend takes one resend of the budget, false when none is left.
func takeResend(t *tracker) bool {
	for {
		n := t.resends.Load()
		if n <= 0 {
			return false
		}
		if t.resends.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

// endedFile keeps, in the local store, how many of the application's
// database sessions the last restore of a capture ended: its pooled
// connections fail at the next capture, whose own first restore may end
// none (saveEnded, takeEnded).
func endedFile(p *config.Project) string {
	if p.Layout.Pristine == "" {
		return ""
	}
	return filepath.Join(p.Layout.Pristine, "ended-sessions.json")
}

type endedRecord struct {
	Sessions int64  `json:"sessions"`
	After    string `json:"after"`
}

// saveEnded keeps the budget of resends the last restore of this capture
// left (none: the file goes).
func saveEnded(p *config.Project, t *tracker) {
	f := endedFile(p)
	if f == "" {
		return
	}
	if n := t.resends.Load(); n > 0 {
		snapshot.WriteJSON(f, endedRecord{Sessions: n, After: t.resentAfter + " (at the end of the previous capture)"})
		return
	}
	os.Remove(f)
}

// takeEnded adds to the budget of this capture's read-only phase what the
// previous capture's last restore left, once.
func takeEnded(p *config.Project, t *tracker) {
	f := endedFile(p)
	if f == "" {
		return
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		return
	}
	os.Remove(f)
	var e endedRecord
	if json.Unmarshal(raw, &e) != nil || e.Sessions <= 0 {
		return
	}
	t.resends.Add(e.Sessions)
	if t.resentAfter == "" {
		t.resentAfter = e.After
	} else {
		t.resentAfter += "; " + e.After
	}
}
