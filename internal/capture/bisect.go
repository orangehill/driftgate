package capture

import (
	"context"
	"fmt"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// findWriters finds the read-only requests that changed the state. The
// requests whose baseline record has side effects run first, one at a time
// on a restored state; the others then run together, and only when they
// change the state are they split in halves: each half runs again on a
// restored state, and a half that changes nothing is dropped (and the other
// half, known to change the state, is split without a run). Without a
// baseline the whole list is split. The responses of these runs are not
// kept. Each request found gets its side effects in its record.
//
// A group runs at the services' concurrency, since the question is only
// whether it changes the state; a request is a writer only when it changes
// the state alone, so the writers found do not depend on the groups tried
// (a request that changes the state alone changes it in any group). Every run
// compares the whole state, so a change that depends on the order of the
// requests (and so did not show in the read-only phase) is still seen and
// restored afterwards.
//
// A run starts from a restore, prepare and the tokens command. When that
// does not lead to the state the read-only phase started from (a tokens
// command that writes a new token on every run, a guard that logged
// itself), each run is compared with the state it started from instead:
// what the group changed is still all that differs. The runs then start
// from another state than the read-only phase did, so a change of that
// phase that no request is found to make stops the capture (exit 3),
// naming both, rather than pass as an order effect.
func (r *runner) findWriters(ctx context.Context, reqs []catalog.Request, before states, changed map[string][]string) error {
	fmt.Fprintf(r.out, "read-only requests changed %v; finding which …\n", limit(flatten(changed), 10))
	covered := map[string]bool{}
	var unrepeatable []string // what a restore, prepare and tokens left other than before the read-only phase

	run := func(group []catalog.Request) (map[string][]string, []snapshot.RowChange, error) {
		now, err := r.reset(ctx)
		if err != nil {
			return nil, nil, err
		}
		if d := flatten(diffStates(before, now)); len(d) > 0 && unrepeatable == nil {
			unrepeatable = d
			fmt.Fprintf(r.out, "note: after a restore, prepare and the tokens command the state differs from the one before the read-only requests (%v): they are not repeatable, so each run of the search is compared with the state it started from\n", limit(d, 10))
		}
		var rowsBefore map[string]state.Rows
		if len(group) == 1 {
			if rowsBefore, err = r.tr.readRows(ctx); err != nil {
				return nil, nil, err
			}
		}
		if len(group) == 1 {
			r.send(ctx, group[0]).Release()
		} else if err := r.byService(ctx, group, false); err != nil {
			return nil, nil, err
		}
		after, err := r.tr.fingerprint(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		r.tr.markDirty(diffStates(before, after))
		d := diffStates(now, after) // what the group changed
		r.tr.markDirty(d)
		var rows []snapshot.RowChange
		if len(d) > 0 && rowsBefore != nil {
			rowsAfter, err := r.tr.readRows(ctx)
			if err != nil {
				return nil, nil, err
			}
			rows = r.tr.rowChanges(rowsBefore, rowsAfter)
		}
		return d, rows, nil
	}

	// search finds the writers in a group and reports whether the group
	// changed the state. A group of several requests that is known to change
	// it is split without a run of its own.
	var search func(group []catalog.Request, changes bool) (bool, error)
	split := func(group []catalog.Request) error {
		mid := len(group) / 2
		first, err := search(group[:mid], false)
		if err != nil {
			return err
		}
		// When the first half changes nothing, the change comes from the
		// second half, alone or together with the first (an order effect).
		_, err = search(group[mid:], !first)
		return err
	}
	search = func(group []catalog.Request, changes bool) (bool, error) {
		if changes && len(group) > 1 {
			return true, split(group)
		}
		d, rows, err := run(group)
		if err != nil || len(d) == 0 {
			return false, err
		}
		if len(group) > 1 {
			return true, split(group)
		}
		items := flatten(d)
		for _, it := range items {
			covered[it] = true
		}
		r.sum.Writers[group[0].ID] = items
		fmt.Fprintf(r.out, "changes data: %s → %v\n", group[0].ID, items)
		return true, r.addSideEffects(group[0].ID, &snapshot.SideEffects{Items: items, Rows: rows})
	}
	var err error
	if known, rest := r.knownWriters(reqs); len(known) > 0 {
		for _, req := range known {
			if _, err = search([]catalog.Request{req}, false); err != nil {
				return err
			}
		}
		if len(rest) > 0 {
			_, err = search(rest, false)
		}
	} else {
		// The whole list is known to change something.
		_, err = search(reqs, true)
	}
	if err != nil {
		return err
	}
	var open []string
	for _, it := range flatten(changed) {
		if !covered[it] {
			open = append(open, it)
		}
	}
	if len(open) > 0 && unrepeatable != nil {
		return envErr("read-only requests changed %v, and no single one of them does it when sent again: the search could not start from the state the read-only requests had, as a restore, prepare and the tokens command leave %v different each time (a tokens command that writes a new token on every run?); make them repeatable, or mark the request that writes mutates: true", limit(open, 10), limit(unrepeatable, 10))
	}
	r.sum.Unattributed = append(r.sum.Unattributed, open...)
	return nil
}

// knownWriters splits the requests into those whose baseline record has side
// effects and the others, both in catalog order. A baseline record that
// cannot be read is only a missed hint.
func (r *runner) knownWriters(reqs []catalog.Request) (known, rest []catalog.Request) {
	if r.base == nil {
		return nil, reqs
	}
	for _, req := range reqs {
		if rec, err := r.base.Record(req.ID); err == nil && rec != nil && rec.SideEffects != nil && len(rec.SideEffects.Items) > 0 {
			known = append(known, req)
		} else {
			rest = append(rest, req)
		}
	}
	return known, rest
}

func (r *runner) addSideEffects(id string, se *snapshot.SideEffects) error {
	rec, err := r.set.Record(id)
	if err != nil || rec == nil {
		return err
	}
	rec.SideEffects = se
	return r.set.Write(rec, nil)
}
