package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"

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
	// noKey: every table left out is one whose rows the baseline's record
	// holds without values (accepted from a capture without the key).
	noKey := true
	for _, id := range ids {
		var learned []string
		var baseRows []snapshot.RowChange
		if rec, err := base.Record(id); err == nil && rec != nil && rec.SideEffects != nil {
			learned, baseRows = rec.SideEffects.Learned, rec.SideEffects.Rows
		}
		named := false
		for _, s := range []*snapshot.Set{base, cur} {
			rec, err := s.Record(id)
			if err != nil || rec == nil || rec.SideEffects == nil {
				continue
			}
			for _, r := range rec.SideEffects.Rows {
				if r.Change == "deleted" || !all && slices.Contains(learned, r.Item) {
					continue
				}
				named = true
				noKey = noKey && slices.ContainsFunc(baseRows, func(b snapshot.RowChange) bool {
					return b.Item == r.Item && b.Change != "deleted" && b.Values == nil
				})
			}
		}
		if named {
			names = append(names, id)
		}
	}
	g := report.NewGap(names)
	if g == nil {
		return nil
	}
	if !all && noKey {
		reason = against + " holds them from a capture without the project key: driftgate capture <tag> --repeat 2 with the key at hand, then driftgate baselines rebase <name> --from <tag>"
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

// acceptLearning sets what an accepted record (rec) learned of the values
// of tracked rows. From a capture two runs agreed on (agreed), it keeps
// what its own runs learned. From a capture made once, it learned
// nothing: per table, it keeps the baseline record's learning (old) only
// when it changed exactly the same rows the same way (keys, changes and
// columns, as compare describes them) with their values recorded; any
// other table is not learned, and a comparison lists it as not covered.
func acceptLearning(old, rec *snapshot.SideEffects, agreed bool) {
	if rec == nil || agreed {
		return
	}
	rec.Learned, rec.Volatile = nil, nil
	if old == nil {
		return
	}
	for _, item := range old.Learned {
		if sameRows(old, rec, item) {
			rec.Learned = append(rec.Learned, item)
			for _, col := range old.Volatile {
				if strings.HasPrefix(col, item+".") {
					rec.Volatile = append(rec.Volatile, col)
				}
			}
		}
	}
}

// sameRows reports whether two records changed the same rows of a table
// the same way, the second with the values of every row it inserted or
// updated recorded.
func sameRows(old, rec *snapshot.SideEffects, item string) bool {
	shape := func(se *snapshot.SideEffects, needValues bool) ([]string, bool) {
		var out []string
		for _, r := range se.Rows {
			if r.Item != item {
				continue
			}
			if needValues && r.Change != "deleted" && r.Values == nil {
				return nil, false
			}
			out = append(out, r.Key+"\x00"+compare.DescribeRow(r))
		}
		sort.Strings(out)
		return out, true
	}
	a, _ := shape(old, false)
	b, ok := shape(rec, true)
	return ok && len(a) > 0 && slices.Equal(a, b)
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
