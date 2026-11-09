package capture

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/orangehill/driftgate/internal/state"
)

// runKind is what a tracker restores for; a stop says what to do in its
// terms.
type runKind int

const (
	runCapture      runKind = iota // a capture or a throughput probe, which run prepare:
	runReadOnly                    // a --read-only capture, which takes no prepare:
	runMigrateState                // verify --migrate-state, which runs no prepare:
)

// schemaDiff is a component's working schema before the first restore:
// the items that differed from the pristine copy's (d) with their working
// hashes (w); no w when the working database could not be read.
type schemaDiff struct {
	d []string
	w state.Fingerprint
}

const (
	schemaChanged   = "the schema of the working database changed since the pristine copy was made: %s; the restore would undo it, so nothing was restored. %s"
	notMadeAgain    = "the schema of the working database changed since the pristine copy was made: %s; the restore undid it, and prepare did not make it again: it comes from another branch, or was made by hand. %s"
	fixPrepare      = "Fix: add the command that runs the migrations to prepare: in %s, so the capture makes the change again after the restore; or run driftgate seed to make the working database the new pristine copy"
	fixSeed         = "Fix: run driftgate seed to make the working database the new pristine copy"
	fixReadOnly     = "Fix (a --read-only capture takes no prepare:): run driftgate seed to make the working database the new pristine copy"
	fixCaptureFirst = "Fix: run a capture first; its prepare: makes the change again"
	fixMissing      = "Fix: a restore that stopped left them missing; make them again in the working database as the pristine copy has them (SHOW CREATE there), then run again"
	unreadPrepared  = "state %s: the working database could not be read before the restore (a restore stopped halfway), so what prepare made again cannot be checked; it now has the schema prepare makes, and the next run goes on"
)

// checkSchema compares the schema of every working database with its
// pristine copy's (state.Schemer) before any is restored: the restore
// would undo a migration run since the seed. A difference stops the run,
// or with prepare: (a capture or probe) is kept for checkPrepared. A
// working database that cannot be read goes on: only a restore that
// stopped leaves one.
func (t *tracker) checkSchema(ctx context.Context) error {
	t.schema = map[string]schemaDiff{}
	var items []string
	t.missingOnly = true // only triggers and sequences the working database lacks
	for _, c := range t.comps {
		s, ok := c.(state.Schemer)
		if !ok {
			continue
		}
		p, err := s.SchemaFingerprint(ctx, true)
		if err != nil {
			return envErr("state %s: schema of the pristine copy: %v", c.Name(), err)
		}
		w, err := s.SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		if w == nil {
			t.schema[c.Name()] = schemaDiff{}
			continue
		}
		d := state.Diff(w, p)
		if len(d) > 0 {
			t.schema[c.Name()] = schemaDiff{d: d, w: w.Subset(d)}
		}
		for _, it := range d {
			_, inW := w[it]
			_, inP := p[it]
			how := map[bool]string{true: "changed", false: "missing"}[inW]
			if !inP {
				how = "new"
			}
			t.missingOnly = t.missingOnly && !inW && (strings.HasPrefix(it, "trigger:") || strings.HasPrefix(it, "sequence:"))
			items = append(items, fmt.Sprintf("%s/%s (%s)", c.Name(), it, how))
		}
	}
	if t.prepare && t.run == runCapture {
		t.prepareCheck = len(t.schema) > 0
		return nil
	}
	if len(items) == 0 {
		return nil
	}
	sort.Strings(items)
	fix := fmt.Sprintf(fixPrepare, t.config)
	switch {
	case t.missingOnly:
		fix = fixMissing
	case t.run == runReadOnly:
		fix = fixReadOnly
	case t.run == runMigrateState && t.prepare:
		fix = fixCaptureFirst
	}
	return envErr(schemaChanged, strings.Join(limit(items, 30), ", "), fix)
}

// checkPrepared runs once, after the first prepare: where the working
// schema differed from the pristine copy's before the restore, it must be
// as it was then.
func (t *tracker) checkPrepared(ctx context.Context) error {
	if !t.prepareCheck {
		return nil
	}
	t.prepareCheck = false
	var items []string
	for _, c := range t.comps {
		sd, ok := t.schema[c.Name()]
		if !ok {
			continue
		}
		if sd.w == nil {
			return envErr(unreadPrepared, c.Name())
		}
		s, err := c.(state.Schemer).SchemaFingerprint(ctx, false)
		if err != nil {
			return envErr("state %s: schema: %v", c.Name(), err)
		}
		for _, it := range state.Diff(s.Subset(sd.d), sd.w) {
			items = append(items, c.Name()+"/"+it)
		}
	}
	if len(items) == 0 {
		return nil
	}
	fix := fixSeed
	if t.missingOnly {
		fix = fixMissing
	}
	return envErr(notMadeAgain, strings.Join(limit(items, 30), ", "), fix)
}
