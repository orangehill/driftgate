package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// runKind is what a tracker restores for; a stop says what to do in its
// terms.
type runKind int

const (
	runCapture      runKind = iota // a capture or a throughput probe, which run prepare:
	runMigrateState                // verify --migrate-state, which runs no prepare:
)

// schemaRecord is a component's working schema before a restore on the
// prepare path: the items that differed from the pristine copy's and the
// working hashes of them, which prepare must make again; and, once a check
// after prepare failed, what prepare made of them instead. It is kept
// beside the record of the pristine copy (schemaPath) until checkPrepared
// passes or a seed runs, so a run that stops before the check (a prepare
// that fails, a cancel, a kill) leaves it to the next run.
type schemaRecord struct {
	Items    []string          `json:"items"`
	Working  state.Fingerprint `json:"working"`
	Prepared state.Fingerprint `json:"prepared,omitempty"`
}

// schemaPath is where the schemaRecord of a component is, beside the
// record of its pristine copy (pristinePath).
func schemaPath(record string) string { return strings.TrimSuffix(record, ".json") + ".schema.json" }

// readSchemaRecord reads a schemaRecord; found is false when there is none.
func readSchemaRecord(path string) (r schemaRecord, found bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &r)
	}
	return r, err == nil, err
}

// expect merges the record of an earlier run (r) with the working schema
// (w; nil when it cannot be read) and the pristine one (p): rec is what
// the working schema must have where it differs from the pristine copy's.
// A recorded item is still expected as recorded where the working database
// has it so, as the pristine copy has it, as the earlier run's prepare
// made it, or not at all; any other definition has changed again since
// (again). An item that differs now and is not recorded is expected as it
// is.
func expect(r schemaRecord, w, p state.Fingerprint) (rec schemaRecord, again []string) {
	rec = schemaRecord{Items: slices.Clone(r.Items), Working: state.Fingerprint{}}
	for _, it := range r.Items {
		if v, ok := w[it]; ok && v != r.Working[it] && v != p[it] && v != r.Prepared[it] {
			again = append(again, it)
		}
		if v, ok := r.Working[it]; ok {
			rec.Working[it] = v
		}
	}
	if w == nil {
		return rec, again
	}
	for _, it := range state.Diff(w, p) {
		if !slices.Contains(r.Items, it) {
			rec.Items = append(rec.Items, it)
			if v, ok := w[it]; ok {
				rec.Working[it] = v
			}
		}
	}
	return rec, again
}

const (
	schemaChanged   = "the schema of the working database changed since the pristine copy was made: %s; %snothing was restored. %s%s"
	wouldUndo       = "the restore would undo it, so "
	changedAgain    = "the schema of the working database changed again since an earlier run whose restore it outlived: %s; nothing was restored. %s%s"
	notMadeAgain    = "the schema of the working database changed since the pristine copy was made: %s; the restore undid it, and prepare did not make it again, or made it otherwise (another migration may change it too): it comes from another branch, or was made by hand. %s%s"
	fixPrepare      = "Fix: add the command that runs the migrations to prepare: in %s, so the capture makes the change again after the restore; or run driftgate seed to make the working database the new pristine copy"
	fixSeed         = "Fix: run driftgate seed to make the working database the new pristine copy"
	fixUndone       = "Fix: the working database no longer has it: to keep it, make it again (run the migration), then run driftgate seed"
	fixCaptureFirst = "Fix: run a capture first; its prepare: makes the change again"
	fixMissing      = "Fix: a restore that stopped may have left them missing; make them again in the working database as the pristine copy has them, then run again"
	acceptUndo      = "; seeding now, or deleting %s, accepts that it was undone"
	undoneEarlier   = "undone by an earlier run's restore"
)

// checkSchema compares the schema of every working database with its
// pristine copy's (state.Schemer) before any is restored: the restore
// would undo a migration run since the seed. For each component it reads
// the record of an earlier run and both schemas, and merges them
// (expect). A definition changed again since stops the run; so does any
// difference, unless prepare: (a capture or probe) is to make it again,
// which is then recorded for checkPrepared. A working database that
// cannot be read has only what was recorded: only a restore that stopped
// leaves one.
func (t *tracker) checkSchema(ctx context.Context) error {
	schema := map[string]schemaRecord{}
	var items, again, found []string
	fresh := false       // some item is not from an earlier run's record
	t.missingOnly = true // only triggers and sequences the working database lacks
	for _, c := range t.comps {
		s, ok := c.(state.Schemer)
		if !ok {
			continue
		}
		r, ok, err := readSchemaRecord(t.records[c.Name()])
		if err != nil {
			return envErr("state %s: %v", c.Name(), err)
		}
		if ok {
			found = append(found, t.records[c.Name()])
		}
		p, err := s.SchemaFingerprint(ctx, true)
		if err != nil {
			return envErr("state %s: schema of the pristine copy: %v", c.Name(), err)
		}
		w, err := s.SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		rec, changed := expect(r, w, p)
		for _, it := range changed {
			again = append(again, c.Name()+"/"+it)
		}
		if len(rec.Items) == 0 {
			continue
		}
		schema[c.Name()] = rec
		for _, it := range rec.Items {
			_, inW := rec.Working[it]
			_, inP := p[it]
			how := "changed"
			switch {
			case slices.Contains(r.Items, it) && w[it] != rec.Working[it]:
				how = undoneEarlier
			case !inP:
				how = "new"
			case !inW:
				how = "missing"
			}
			fresh = fresh || how != undoneEarlier
			t.missingOnly = t.missingOnly && !inW && state.MissingAfterStop(it)
			items = append(items, fmt.Sprintf("%s/%s (%s)", c.Name(), it, how))
		}
	}
	undo := ""
	if len(found) > 0 {
		undo = fmt.Sprintf(acceptUndo, strings.Join(found, ", "))
	}
	if len(again) > 0 {
		return envErr(changedAgain, strings.Join(limit(again, 30), ", "), fixSeed, undo)
	}
	if t.prepare && t.run == runCapture {
		for name, r := range schema {
			if err := snapshot.WriteJSON(t.records[name], r); err != nil {
				return err
			}
		}
		t.schema = schema
		return nil
	}
	if len(items) == 0 {
		return nil
	}
	sort.Strings(items)
	stale := ""
	for _, c := range t.comps {
		if r, ok := schema[c.Name()]; ok && stale == "" {
			stale = staleHint(ctx, c, t.pristine[c.Name()], r.Items)
		}
	}
	fix := fmt.Sprintf(fixPrepare, t.config)
	switch {
	case t.missingOnly:
		fix = fixMissing
	case t.run == runMigrateState && t.prepare:
		fix = fixCaptureFirst
	}
	head := ""
	if fresh {
		head = wouldUndo
	}
	return envErr(schemaChanged, strings.Join(limit(items, 30), ", "), head, fix, undo+stale)
}

// checkPrepared, after the first prepare, checks that the working schema
// is again what checkSchema recorded, and then removes the records; it
// checks once. A check that fails records what prepare made, which the
// next run takes for no change since (expect).
func (t *tracker) checkPrepared(ctx context.Context) error {
	schema := t.schema
	t.schema = nil
	var items, found []string
	for _, c := range t.comps {
		r, ok := schema[c.Name()]
		if !ok {
			continue
		}
		found = append(found, t.records[c.Name()])
		s, err := c.(state.Schemer).SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		d := state.Diff(s.Subset(r.Items), r.Working)
		if len(d) == 0 {
			continue
		}
		r.Prepared = s.Subset(r.Items)
		if err := snapshot.WriteJSON(t.records[c.Name()], r); err != nil {
			return err
		}
		for _, it := range d {
			items = append(items, c.Name()+"/"+it)
		}
	}
	if len(items) > 0 {
		fix := fixUndone
		if t.missingOnly {
			fix = fixMissing
		}
		return envErr(notMadeAgain, strings.Join(limit(items, 30), ", "), fix, fmt.Sprintf(acceptUndo, strings.Join(found, ", ")))
	}
	for _, path := range found {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
