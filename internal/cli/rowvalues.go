package cli

import (
	"fmt"
	"slices"
	"sort"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// rowValuesKey makes sure the values of tracked rows two sets recorded
// were named under one project key (meta.json's row_values): under two
// keys every value would differ, and nothing of them can be compared.
func rowValuesKey(base, cur namedSet) error {
	var ids [2]string
	for i, s := range []namedSet{base, cur} {
		meta, err := s.set.Meta()
		if err != nil {
			return envErr(err)
		}
		if meta != nil {
			ids[i] = meta.RowValues
		}
	}
	if ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
		return nil
	}
	return envErr(fmt.Errorf("%s and %s named the values of tracked rows with different project keys, so they cannot be compared: set the project key both were made with (%s or key: in %s) and capture again",
		base.name, cur.name, secret.EnvKey, config.FileName))
}

// rowValuesGap names, for the result's "not covered" section, the
// compared requests whose rows changed but whose row values were not
// compared: a side whose capture recorded none (made by an earlier
// release, or without the project key). nil when there are none.
func rowValuesGap(base, cur *snapshot.Set, ids []string) *report.SkippedGap {
	var reason string
	for i, s := range []*snapshot.Set{base, cur} {
		meta, err := s.Meta()
		if err != nil || meta == nil || meta.RowValues != "" || len(meta.RowTracking) == 0 && meta.RowTracking != nil {
			continue
		}
		if i == 0 {
			reason = "the baseline recorded no row values: make the baseline again, with the project key at hand, to compare them"
		} else {
			reason = "the capture recorded no row values: no project key when it was made"
		}
		break
	}
	if meta, err := base.Meta(); reason == "" && err == nil && meta != nil && onceWithValues(meta) {
		reason = "the set compared against was captured once, so the columns that change by themselves are not known: capture it with --repeat 2, as run makes a baseline"
	}
	if reason == "" {
		return nil
	}
	var names []string
	for _, id := range ids {
		for _, s := range []*snapshot.Set{base, cur} {
			if rec, err := s.Record(id); err == nil && rec != nil && rec.SideEffects != nil &&
				slices.ContainsFunc(rec.SideEffects.Rows, func(r snapshot.RowChange) bool { return r.Change != "deleted" }) {
				names = append(names, id)
				break
			}
		}
	}
	g := report.NewGap(names)
	if g == nil {
		return nil
	}
	return &report.SkippedGap{What: report.SkippedMutating, Reason: reason, Gap: *g}
}

// learnVolatile records, in the records of the capture tag, the columns
// whose values differed between captures of the same state (drifts of
// values only, compare.Drift.RowValues), per request: a later comparison
// leaves their values out.
func learnVolatile(p *config.Project, tag string, learned map[string][]string) error {
	if len(learned) == 0 {
		return nil
	}
	set, err := p.Capture(tag)
	if err != nil {
		return failed(err)
	}
	ids := make([]string, 0, len(learned))
	for id := range learned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rec, err := set.Record(id)
		if err != nil {
			return envErr(err)
		}
		if rec == nil || rec.SideEffects == nil {
			continue
		}
		rec.SideEffects.Volatile = mergeVolatile(rec.SideEffects.Volatile, learned[id])
		if err := set.Write(rec, nil); err != nil {
			return envErr(err)
		}
	}
	return nil
}

// mergeVolatile is the sorted union of two lists of volatile columns.
func mergeVolatile(a, b []string) []string {
	out := slices.Concat(a, b)
	sort.Strings(out)
	return slices.Compact(out)
}

// valuesOnly reports whether a drift of two captures of one state is a
// tracked row that changed alike with other values, which the captures
// learn as volatile.
func valuesOnly(d compare.Drift) bool {
	return d.Kind == compare.KindSideEffect && len(d.RowValues) > 0
}

// rebaseRowValues refuses to make a baseline of a capture whose values of
// tracked rows were named under another key than the project key (or,
// without the key at hand, than the baseline's): later captures, named
// under the project key, would differ in every value.
func rebaseRowValues(p *config.Project, oldMeta, meta *snapshot.Meta, tag string) error {
	if meta.RowValues == "" {
		return nil
	}
	want, what := "", "the project key"
	if key, err := p.LoadKey(); err == nil {
		want = key.RowValues().ID()
	} else if oldMeta != nil {
		want, what = oldMeta.RowValues, "the baseline's key"
	}
	if want == "" || want == meta.RowValues {
		return nil
	}
	return envErr(fmt.Errorf("capture %s named the values of tracked rows with another key than %s: capture it again with the project key at hand (%s or key: in %s)",
		tag, what, secret.EnvKey, config.FileName))
}
