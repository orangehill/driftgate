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
// compared requests (ids) that wrote into tracked rows whose values were
// not compared, and why (rowValuesReason). nil when there are none.
func rowValuesGap(base, cur *snapshot.Set, against string, ids []string) *report.ReasonGap {
	reason, all := rowValuesReason(base, cur, against)
	g := report.NewGap(rowValuesLeft(base, cur, ids, all))
	if g == nil {
		return nil
	}
	return &report.ReasonGap{Reason: reason, Gap: *g}
}

// rowValuesReason is why row values were not compared: a side whose
// capture recorded none (an earlier release, no project key: all, no
// row's values were), else tables whose values the record compared
// against did not learn from two captures of one state (a request
// accepted from one capture, a set captured once).
func rowValuesReason(base, cur *snapshot.Set, against string) (reason string, all bool) {
	switch {
	case !valuesRecorded(cur):
		return "the capture recorded no row values: no project key when it was made", true
	case !valuesRecorded(base):
		return against + " recorded no row values: make it again, with the project key at hand, to compare them", true
	}
	return against + " did not see them in two captures of one state: driftgate capture <tag> --repeat 2, then driftgate baselines rebase <name> --from <tag>", false
}

// valuesRecorded reports whether a set recorded the values of tracked
// rows, or tracked none.
func valuesRecorded(s *snapshot.Set) bool {
	meta, err := s.Meta()
	if err != nil || meta == nil {
		return true // nothing to say of it
	}
	tracksNothing := meta.RowTracking != nil && len(meta.RowTracking) == 0
	return meta.RowValues != "" || tracksNothing
}

// rowValuesLeft are the requests of ids that inserted or updated rows on
// either side whose values were not compared: of any table (all), or of a
// table the base record did not learn.
func rowValuesLeft(base, cur *snapshot.Set, ids []string, all bool) []string {
	var out []string
	for _, id := range ids {
		var learned []string
		if rec, err := base.Record(id); err == nil && rec != nil && rec.SideEffects != nil {
			learned = rec.SideEffects.Learned
		}
		left := func(r snapshot.RowChange) bool {
			return r.Change != "deleted" && (all || !slices.Contains(learned, r.Item))
		}
		for _, s := range []*snapshot.Set{base, cur} {
			if rec, err := s.Record(id); err == nil && rec != nil && rec.SideEffects != nil && slices.ContainsFunc(rec.SideEffects.Rows, left) {
				out = append(out, id)
				break
			}
		}
	}
	return out
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
			items[i] = valueItems(rec.SideEffects, keyed(s))
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
// with their values recorded, sorted. In a set captured with the key
// (keyed), a row without values had every written column ignored: it
// has nothing to compare, and counts as recorded.
func valueItems(se *snapshot.SideEffects, keyed bool) []string {
	if se == nil {
		return nil
	}
	var out []string
	for _, r := range se.Rows {
		if r.Change != "deleted" && (keyed || r.Values != nil) {
			out = append(out, r.Item)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// keyed reports whether a set named the values of tracked rows under a
// project key (meta.RowValues).
func keyed(s *snapshot.Set) bool {
	meta, err := s.Meta()
	return err == nil && meta != nil && meta.RowValues != ""
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
func acceptLearning(old, rec *snapshot.SideEffects, agreed, keyed bool) {
	if rec == nil || agreed {
		return
	}
	rec.Learned, rec.Volatile = nil, nil
	if old == nil {
		return
	}
	for _, item := range old.Learned {
		if sameRows(old, rec, item, keyed) {
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
// updated recorded (keyed: its set was captured with the key, so a row
// without values had nothing left to compare).
func sameRows(old, rec *snapshot.SideEffects, item string, keyed bool) bool {
	shape := func(se *snapshot.SideEffects, needValues bool) ([]string, bool) {
		var out []string
		for _, r := range se.Rows {
			if r.Item != item {
				continue
			}
			if needValues && !keyed && r.Change != "deleted" && r.Values == nil {
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

// acceptRowKey refuses to accept into a baseline from a capture that
// named row values under another key than the baseline did: no later
// capture could be compared with both.
func acceptRowKey(base *snapshot.Set, key string) error {
	meta, err := base.Meta()
	if err != nil {
		return envErr(err)
	}
	if key == "" || meta == nil || meta.RowValues == "" || meta.RowValues == key {
		return nil
	}
	return envErr(fmt.Errorf("the capture named the values of tracked rows with another key than the baseline: capture again with the project key the baseline was made with (%s or key: in %s), then accept",
		secret.EnvKey, config.FileName))
}

// setRowKey records in a baseline that recorded no row values the key an
// accepted capture named its values under, when the accepted records hold
// values (carries): a later comparison then compares them.
func setRowKey(work *snapshot.Set, key string, carries bool) error {
	if key == "" || !carries {
		return nil
	}
	meta, err := work.Meta()
	if err != nil || meta == nil || meta.RowValues != "" {
		return err
	}
	meta.RowValues = key
	return work.WriteMeta(*meta)
}
