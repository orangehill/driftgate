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
// working hashes of them, which prepare must make again. It is kept beside
// the record of the pristine copy (schemaPath) until checkPrepared passes
// or a seed runs, so a run that stops before the check (a prepare that
// fails, a cancel, a kill) leaves it to the next run.
type schemaRecord struct {
	Items   []string          `json:"items"`
	Working state.Fingerprint `json:"working"`
}

// schemaPath is where the schemaRecord of a component is, beside the
// record of its pristine copy (pristinePath).
func schemaPath(record string) string { return strings.TrimSuffix(record, ".json") + ".schema.json" }

const (
	schemaChanged   = "the schema of the working database changed since the pristine copy was made: %s; the restore would undo it, so nothing was restored. %s%s"
	changedAgain    = "the schema of the working database changed again since an earlier run whose restore it outlived: %s; nothing was restored. %s%s"
	notMadeAgain    = "the schema of the working database changed since the pristine copy was made: %s; the restore undid it, and prepare did not make it again, or made it otherwise (another migration may change it too): it comes from another branch, or was made by hand. %s%s"
	fixPrepare      = "Fix: add the command that runs the migrations to prepare: in %s, so the capture makes the change again after the restore; or run driftgate seed to make the working database the new pristine copy"
	fixSeed         = "Fix: run driftgate seed to make the working database the new pristine copy"
	fixCaptureFirst = "Fix: run a capture first; its prepare: makes the change again"
	fixMissing      = "Fix: a restore that stopped left them missing; make them again in the working database as the pristine copy has them (SHOW CREATE there), then run again"
	acceptUndo      = "; or delete %s to accept that a restore undid it"
)

// checkSchema compares the schema of every working database with its
// pristine copy's (state.Schemer) before any is restored: the restore
// would undo a migration run since the seed. What an earlier run recorded
// (schemaRecord) is still expected where the working database has it, the
// pristine copy's definition, or nothing; a third definition has changed
// again since, and stops the run. A difference stops the run, or with
// prepare: (a capture or probe) is recorded for checkPrepared. A working
// database that cannot be read has only what was recorded: only a restore
// that stopped leaves one.
func (t *tracker) checkSchema(ctx context.Context) error {
	schema := map[string]schemaRecord{}
	var items, again, records []string
	t.missingOnly = true // only triggers and sequences the working database lacks
	for _, c := range t.comps {
		s, ok := c.(state.Schemer)
		if !ok {
			continue
		}
		var r schemaRecord
		raw, err := os.ReadFile(t.records[c.Name()])
		if err == nil {
			err = json.Unmarshal(raw, &r)
			records = append(records, t.records[c.Name()])
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return envErr("state %s: %v", c.Name(), err)
		}
		p, err := s.SchemaFingerprint(ctx, true)
		if err != nil {
			return envErr("state %s: schema of the pristine copy: %v", c.Name(), err)
		}
		w, err := s.SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		d, want := r.Items, state.Fingerprint{}
		for _, it := range r.Items {
			if v, ok := w[it]; ok && v != r.Working[it] && v != p[it] {
				again = append(again, c.Name()+"/"+it)
			}
			if v, ok := r.Working[it]; ok {
				want[it] = v
			}
		}
		if w != nil {
			for _, it := range state.Diff(w, p) {
				if !slices.Contains(r.Items, it) {
					d = append(d, it)
					if v, ok := w[it]; ok {
						want[it] = v
					}
				}
			}
		}
		if len(d) == 0 {
			continue
		}
		schema[c.Name()] = schemaRecord{Items: d, Working: want}
		for _, it := range d {
			_, inW := want[it]
			_, inP := p[it]
			how := map[bool]string{true: "changed", false: "missing"}[inW]
			if !inP {
				how = "new"
			}
			t.missingOnly = t.missingOnly && !inW && state.MissingAfterStop(it)
			items = append(items, fmt.Sprintf("%s/%s (%s)", c.Name(), it, how))
		}
	}
	undo := ""
	if len(records) > 0 {
		undo = fmt.Sprintf(acceptUndo, strings.Join(records, ", "))
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
	return envErr(schemaChanged, strings.Join(limit(items, 30), ", "), fix, undo+stale)
}

// checkPrepared, after the first prepare, checks that the working schema
// is again what checkSchema recorded, and then removes the records; it
// checks once.
func (t *tracker) checkPrepared(ctx context.Context) error {
	schema := t.schema
	t.schema = nil
	var items, records []string
	for _, c := range t.comps {
		r, ok := schema[c.Name()]
		if !ok {
			continue
		}
		records = append(records, t.records[c.Name()])
		s, err := c.(state.Schemer).SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		for _, it := range state.Diff(s.Subset(r.Items), r.Working) {
			items = append(items, c.Name()+"/"+it)
		}
	}
	if len(items) > 0 {
		fix := fixSeed
		if t.missingOnly {
			fix = fixMissing
		}
		return envErr(notMadeAgain, strings.Join(limit(items, 30), ", "), fix, fmt.Sprintf(acceptUndo, strings.Join(records, ", ")))
	}
	for _, path := range records {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
