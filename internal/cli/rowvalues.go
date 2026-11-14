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
// compared requests that wrote into tracked rows whose values were not
// compared, and why: a side whose capture recorded none (an earlier
// release, no project key), or rows of a table whose values the record
// compared against did not see in two captures of one state
// (SideEffects.Learned: a request accepted from one capture, a set
// captured once). nil when there are none.
func rowValuesGap(base, cur *snapshot.Set, against string, ids []string) *report.ReasonGap {
	recorded := func(s *snapshot.Set) bool {
		meta, err := s.Meta()
		tracksNothing := err == nil && meta != nil && meta.RowTracking != nil && len(meta.RowTracking) == 0
		return err != nil || meta == nil || meta.RowValues != "" || tracksNothing
	}
	var reason string
	switch {
	case !recorded(cur):
		reason = "the capture recorded no row values: no project key when it was made"
	case !recorded(base):
		reason = against + " recorded no row values: make it again, with the project key at hand, to compare them"
	default:
		reason = against + " did not see them in two captures of one state: driftgate capture <tag> --repeat 2, then driftgate baselines rebase <name> --from <tag>"
	}
	all := !recorded(cur) || !recorded(base)
	var names []string
	for _, id := range ids {
		var learned []string
		if rec, err := base.Record(id); err == nil && rec != nil && rec.SideEffects != nil {
			learned = rec.SideEffects.Learned
		}
		for _, s := range []*snapshot.Set{base, cur} {
			if rec, err := s.Record(id); err == nil && rec != nil && rec.SideEffects != nil &&
				slices.ContainsFunc(rec.SideEffects.Rows, func(r snapshot.RowChange) bool {
					return r.Change != "deleted" && (all || !slices.Contains(learned, r.Item))
				}) {
				names = append(names, id)
				break
			}
		}
	}
	g := report.NewGap(names)
	if g == nil {
		return nil
	}
	return &report.ReasonGap{Reason: reason, Gap: *g}
}

// rowLearning is what captures of one state learned of the values a
// request wrote into tracked rows: the tables whose values every capture
// recorded (Learned), and the columns whose values differed (Volatile).
type rowLearning struct {
	Learned, Volatile []string
	seen              bool // Learned holds the first comparison's tables
}

// learnRows narrows what the captures learned of request id by one more
// capture (cur) compared with the first: the tables whose values both
// recorded.
func learnRows(rows map[string]*rowLearning, id string, first, cur *snapshot.Set) error {
	l := rows[id]
	if l == nil {
		l = &rowLearning{}
		rows[id] = l
	}
	var items [2][]string
	for i, s := range []*snapshot.Set{first, cur} {
		rec, err := s.Record(id)
		if err != nil {
			return envErr(err)
		}
		if rec != nil {
			items[i] = valueItems(rec.SideEffects)
		}
	}
	both := slices.DeleteFunc(items[0], func(it string) bool { return !slices.Contains(items[1], it) })
	if l.seen {
		both = slices.DeleteFunc(both, func(it string) bool { return !slices.Contains(l.Learned, it) })
	}
	l.Learned, l.seen = both, true
	return nil
}

// valueItems are the tables of the rows a request inserted or updated
// with their values recorded, sorted.
func valueItems(se *snapshot.SideEffects) []string {
	if se == nil {
		return nil
	}
	var out []string
	for _, r := range se.Rows {
		if r.Change != "deleted" && r.Values != nil {
			out = append(out, r.Item)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// writeLearned records in the records of the capture tag what its runs
// learned of the values of tracked rows (rowLearning): a comparison
// against it compares the values of the learned tables, apart from the
// volatile columns.
func writeLearned(p *config.Project, tag string, rows map[string]*rowLearning) error {
	if len(rows) == 0 {
		return nil
	}
	set, err := p.Capture(tag)
	if err != nil {
		return failed(err)
	}
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		l := rows[id]
		if len(l.Learned) == 0 && len(l.Volatile) == 0 {
			continue
		}
		rec, err := set.Record(id)
		if err != nil {
			return envErr(err)
		}
		if rec == nil || rec.SideEffects == nil {
			continue
		}
		rec.SideEffects.Learned = l.Learned
		rec.SideEffects.Volatile = mergeVolatile(rec.SideEffects.Volatile, l.Volatile)
		if err := set.Write(rec, nil); err != nil {
			return envErr(err)
		}
	}
	return nil
}

// acceptLearning is what an accepted record keeps of the baseline
// record's learning. A record of a capture made once learned nothing: it
// keeps the learned tables it still wrote values into, and the volatile
// columns; a table it newly writes is not learned. A record whose
// captures learned (capture --repeat) keeps its own.
func acceptLearning(old, rec *snapshot.SideEffects) {
	if rec == nil || len(rec.Learned) > 0 || len(rec.Volatile) > 0 || old == nil {
		return
	}
	now := valueItems(rec)
	for _, it := range old.Learned {
		if slices.Contains(now, it) {
			rec.Learned = append(rec.Learned, it)
		}
	}
	rec.Volatile = slices.Clone(old.Volatile)
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
