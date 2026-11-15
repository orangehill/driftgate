package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// driftText is a drift as its digest covers it (compare.Drift.DigestText).
func driftText(d compare.Drift) []byte { return []byte(d.DigestText()) }

// driftDigest names a drift of a request with its values
// (compare.Drift.Digest); empty when it cannot.
type driftDigest func(req catalog.Request, private bool, d compare.Drift) string

// digester names the drifts of a request, by which expectations match
// them. The digest covers the values and the witness, so an expectation
// matches only exactly the drift it recorded. Values of a private
// response (or of a request whose path holds such values) are named by a
// keyed hash, as private bodies are: the digest goes into the
// expectations file, which may be in git. Without the key such a drift
// gets no digest and cannot be expected; nor can a side effect, which
// names what changed and not the values.
func digester(p *config.Project) driftDigest {
	return func(req catalog.Request, private bool, d compare.Drift) string {
		if d.Kind == compare.KindSideEffect {
			return ""
		}
		if !private && len(req.ParamsFrom) == 0 {
			return "sha256:" + snapshot.Sum(driftText(d))
		}
		key, err := p.LoadKey()
		if err != nil {
			return ""
		}
		return "ref:" + key.Ref(driftText(d))
	}
}

// expectations reads the expectations in force of the baseline a
// comparison is against (name as resolve gives it); none for a capture.
func expectations(p *config.Project, name string) ([]report.Expectation, error) {
	base, ok := strings.CutPrefix(name, "baseline:")
	if !ok {
		return nil, nil
	}
	set, err := p.Baseline(base)
	if err != nil {
		return nil, failed(err)
	}
	list, _, err := baseline.Expectations(set.Dir)
	if err != nil {
		return nil, expectErr(p, base, err)
	}
	out := make([]report.Expectation, len(list))
	for i, e := range list {
		out[i] = report.Expectation{Ref: e.Ref, Reason: e.Reason, Request: e.ID, Drifts: e.Drifts}
	}
	return out, nil
}

// expectErr is the error of an expectations file that cannot be used: one
// that changed outside driftgate is refused as a changed baseline is
// (exit 3), since expected drifts would pass on its word.
func expectErr(p *config.Project, name string, err error) error {
	var bad *baseline.ExpectProblems
	if !errors.As(err, &bad) {
		return envErr(err)
	}
	path := filepath.Join(rel(p.Root, p.Layout.Baselines), filepath.FromSlash(bad.File))
	restore := "restore it from git (git checkout -- " + path + ")"
	if !p.Layout.Committed() {
		restore = "restore it from a backup"
	}
	return envErr(fmt.Errorf("baseline %s: %v; %s, or remove the file, which removes the expectations of its ref", name, err, restore))
}

// catalogIDs reports whether a request is in the catalog.
func catalogIDs(cat *catalog.Catalog) func(string) bool {
	ids := map[string]bool{}
	for _, req := range cat.Requests {
		ids[req.ID] = true
	}
	return func(id string) bool { return ids[id] }
}

func expectCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("expect", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	ref := fs.String("ref", "", "")
	reason := fs.String("reason", "", "")
	reviewed := fs.String("comparison", "", "")
	drop := fs.String("drop", "", "")
	imported := fs.String("import", "", "")
	name := fs.String("baseline", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	switch {
	case *drop != "" && *imported != "":
		return 0, configErr("--drop and --import are separate steps")
	case *drop != "" || *imported != "":
		if len(pos) > 0 || *ref != "" || *reviewed != "" {
			return 0, configErr("--drop <ref> and --import <file> take no requests, --ref or --comparison")
		}
	case *name != "":
		return 0, configErr("--baseline goes with --drop or --import; expect takes the drifts, and the baseline, of the last comparison")
	case len(pos) == 0:
		return 0, usageErr("expect")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if *imported == "" {
		if strings.TrimSpace(*ref) == "" && *drop == "" {
			return 0, configErr("expect needs --ref <ticket>: the change that intends the drifts")
		}
		if strings.TrimSpace(*reason) == "" {
			return 0, configErr(`expect needs --reason "…": why the change makes these drifts, or why they are dropped (it is written to the expectations file)`)
		}
	}
	unlock, err := lockStore(context.Background(), p, os.Stderr)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := interrupted(p, os.Stderr); err != nil {
		return 0, err
	}
	if err := ensureLineEndings(p, out); err != nil {
		return 0, err
	}
	if *drop != "" || *imported != "" {
		if *name == "" {
			// The baseline of the last comparison, else local.
			*name = "local"
			if r, _, err := readLastCompare(p); err == nil && r.Baseline != "" {
				*name = r.Baseline
			}
		}
		if *imported != "" {
			return ExitClean, importExpectations(p, *name, *imported, out)
		}
		return ExitClean, dropExpectations(p, *name, strings.TrimSpace(*drop), strings.TrimSpace(*reason), out)
	}

	r, path, err := readLastCompare(p)
	if err != nil {
		return 0, err
	}
	if r.Baseline == "" {
		return 0, configErr("the last comparison is %s against %s; expect takes drifts of a comparison against a baseline", r.Tag, r.Against)
	}
	base, err := p.Baseline(r.Baseline)
	if err != nil {
		return 0, failed(err)
	}
	if base.Exists() {
		if err := checkBaseline(p, base, r.Baseline); err != nil {
			return 0, err
		}
	}
	why, err := staleComparison(p, r)
	if err != nil {
		return 0, err
	}
	if why == "" && *reviewed != "" && *reviewed != comparisonID(r) {
		why = "compare ran again after the review, so its groups may be numbered differently"
	}
	if why != "" {
		return 0, configErr("the last comparison is out of date: %s; run driftgate compare (or driftgate run) again", why)
	}
	theRef := strings.TrimSpace(*ref)
	sel, leftOut, err := chooseExpected(r, pos, theRef)
	if err != nil {
		return 0, err
	}
	before, _, err := baseline.Expectations(base.Dir)
	if err != nil {
		return 0, expectErr(p, r.Baseline, err)
	}
	notes := mergeEarlier(r, sel, before, theRef)
	line := baseline.ExpectLine{
		Op:       baseline.OpExpect,
		At:       time.Now().UTC().Format(time.RFC3339),
		By:       acceptedBy(p.Root),
		Ref:      theRef,
		Reason:   strings.TrimSpace(*reason),
		Capture:  strings.TrimPrefix(r.Tag, "capture:"),
		Commit:   gitOutput(p.Root, "rev-parse", "--short", "HEAD"),
		Requests: sel,
	}
	if err := baseline.AppendExpectation(base.Dir, line); err != nil {
		return 0, expectErr(p, r.Baseline, err)
	}
	if err := remarkLast(p, r, path); err != nil {
		return 0, err
	}
	ids := make([]string, len(sel))
	sigs := map[string]bool{}
	n := 0
	for i, e := range sel {
		ids[i] = e.ID
		n += len(e.Drifts)
		for _, s := range e.Signatures {
			sigs[s] = true
		}
	}
	fmt.Fprintf(out, "expected %s in %s of baseline %s for %s: %s\n", count(n, "drift", "drifts"), count(len(ids), "request", "requests"), r.Baseline, theRef, strings.Join(ids, ", "))
	for _, s := range sortedKeys(sigs) {
		fmt.Fprintf(out, "  %s\n", report.Printable(s))
	}
	for _, note := range notes {
		fmt.Fprintln(out, note)
	}
	for _, ref := range sortedKeys(boolKeys(leftOut)) {
		fmt.Fprintf(out, "left out %s that %s expects already\n", count(leftOut[ref], "drift", "drifts"), ref)
	}
	fmt.Fprintf(out, "recorded in %s (exactly these drifts, with their values; the baseline is unchanged)%s\n",
		rel(p.Root, refPath(base, theRef)), commitNote(p, "; commit it"))
	return ExitClean, nil
}

// count writes a number with its word.
func count(n int, one, many string) string { return fmt.Sprintf("%d %s", n, plural(n, one, many)) }

// refPath is the file of a ref's expectations of a baseline.
func refPath(base *snapshot.Set, ref string) string {
	dir := baseline.ExpectDir(base.Dir)
	plain := filepath.Join(dir, baseline.RefFile(ref, false))
	if _, err := os.Lstat(plain); err != nil {
		if hashed := filepath.Join(dir, baseline.RefFile(ref, true)); fileExists(hashed) {
			return hashed
		}
	}
	return plain
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// remarkLast marks the last comparison again with the expectations in
// force, so report and review show them.
func remarkLast(p *config.Project, r *report.Report, path string) error {
	exps, err := expectations(p, "baseline:"+r.Baseline)
	if err != nil {
		return err
	}
	cat, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return configErr("%v", err)
	}
	r.Expect(exps, catalogIDs(cat))
	return r.Save(path)
}

// chooseExpected resolves selections (request IDs, globs over IDs,
// group:N) against the last comparison. A request named by its ID or a
// glob is expected with all of its drifts; a group takes the drifts of its
// signature, so tickets that change one request in different ways each
// expect their own drifts. A request every drift of which is expected
// already is taken again only by its ref (which replaces its expectation).
func chooseExpected(r *report.Report, args []string, ref string) ([]baseline.Expected, map[string]int, error) {
	done := map[string]bool{}
	for _, id := range r.Accepted {
		done[id] = true
	}
	open := func(res report.Result) bool {
		return res.Outcome == report.Drift && !done[res.ID] && (len(res.Expected) == 0 || slices.Contains(res.Expected, ref))
	}
	sel, err := pick(r, args, pickRules{
		verb: "expect",
		open: open,
		refuse: func(res report.Result) string {
			switch {
			case done[res.ID]:
				return "was accepted from the last comparison"
			case res.Outcome != report.Drift:
				return fmt.Sprintf("has no drift to expect (%s)", res.Outcome)
			case !open(res):
				return fmt.Sprintf("is expected already (%s); --drop that ref first, or expect it with that ref again", strings.Join(res.Expected, ", "))
			}
			return ""
		},
		none: "with unexpected drift",
	})
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]report.Result{}
	for _, res := range r.Results {
		byID[res.ID] = res
	}
	var blind []string
	// A drift another ref expects already stays that ref's: one ticket
	// does not take another's drifts.
	leftOut := map[string]int{}
	out := make([]baseline.Expected, 0, len(sel.drifts))
	for _, id := range sel.ids() {
		res := byID[id]
		e := baseline.Expected{ID: id}
		sigs := map[string]bool{}
		ok := res.Note != report.NotePrivateMissing
		for _, i := range sel.drifts[id] {
			d := res.Drifts[i]
			if d.Expected != "" && d.Expected != ref {
				leftOut[d.Expected]++
				continue
			}
			if d.Digest == "" {
				ok = false
			}
			e.Drifts = append(e.Drifts, d.Digest)
			sigs[d.Signature()] = true
		}
		if !ok {
			blind = append(blind, id)
			continue
		}
		if len(e.Drifts) == 0 {
			continue
		}
		sort.Strings(e.Drifts)
		e.Drifts = compactStrings(e.Drifts)
		e.Signatures = sortedKeys(sigs)
		out = append(out, e)
	}
	if len(blind) > 0 {
		return nil, nil, configErr("%s cannot be expected: a side effect (it names what changed, not the values), a private body not on this machine, the project key not at hand, or a comparison by an earlier release; compare them where the bodies and the key are", strings.Join(blind, ", "))
	}
	if len(out) == 0 {
		by := ""
		if len(leftOut) > 0 {
			by = " (by " + strings.Join(sortedKeys(boolKeys(leftOut)), ", ") + ")"
		}
		return nil, nil, configErr("nothing to expect: the selected drifts are expected%s or accepted already", by)
	}
	return out, leftOut, nil
}

func boolKeys(m map[string]int) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// mergeEarlier adds to a selection the drifts the ref's earlier
// expectation of the same request names that the request still has: an
// expectation of a request with the same ref again adds to it (a second
// group of the ticket's drifts), and drops only the drifts that no longer
// occur. It returns what it says about it.
func mergeEarlier(r *report.Report, sel []baseline.Expected, before []baseline.Expectation, ref string) []string {
	earlier := map[string][]string{}
	for _, e := range before {
		if e.Ref == ref {
			earlier[e.ID] = e.Drifts
		}
	}
	byID := map[string]report.Result{}
	for _, res := range r.Results {
		byID[res.ID] = res
	}
	var notes []string
	for i, e := range sel {
		old, ok := earlier[e.ID]
		if !ok {
			continue
		}
		occurs := map[string]compare.Drift{}
		for _, d := range byID[e.ID].Drifts {
			occurs[d.Digest] = d
		}
		have := map[string]bool{}
		for _, d := range e.Drifts {
			have[d] = true
		}
		sigs := map[string]bool{}
		for _, s := range e.Signatures {
			sigs[s] = true
		}
		dropped := 0
		for _, d := range old {
			drift, ok := occurs[d]
			switch {
			case !ok:
				dropped++
			case !have[d]:
				e.Drifts = append(e.Drifts, d)
				sigs[drift.Signature()] = true
				have[d] = true
			}
		}
		sort.Strings(e.Drifts)
		e.Signatures = sortedKeys(sigs)
		sel[i] = e
		still := len(old) - dropped
		added := len(e.Drifts) - still
		var note string
		switch {
		case still == 0:
			note = fmt.Sprintf("this replaces the earlier expectation of %s for %s, none of whose drifts occur now", ref, e.ID)
		case added > 0:
			note = fmt.Sprintf("this adds %s to the earlier expectation of %s for %s", count(added, "drift", "drifts"), ref, e.ID)
		default:
			note = fmt.Sprintf("the earlier expectation of %s for %s names these drifts already", ref, e.ID)
		}
		if still > 0 && dropped > 0 {
			note += fmt.Sprintf("; %s of it no longer occur and are dropped", count(dropped, "drift", "drifts"))
		}
		notes = append(notes, note)
	}
	return notes
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dropExpectations removes the expectations of a ref from a baseline,
// with a line in the ref's file: its history stays, and no other ref's
// file changes.
func dropExpectations(p *config.Project, name, ref, reason string, out io.Writer) error {
	set, err := p.Baseline(name)
	if err != nil {
		return failed(err)
	}
	exps, _, err := baseline.Expectations(set.Dir)
	if err != nil {
		return expectErr(p, name, err)
	}
	var ids []string
	inForce := map[string]bool{}
	for _, e := range exps {
		inForce[e.Ref] = true
		if e.Ref == ref {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		in := "it has none in force"
		if len(inForce) > 0 {
			in = "it has " + strings.Join(sortedKeys(inForce), ", ")
		}
		return configErr("baseline %s has no expectation with ref %s in force (%s; --baseline names another baseline)", name, ref, in)
	}
	line := baseline.ExpectLine{Op: baseline.OpDrop, At: time.Now().UTC().Format(time.RFC3339), By: acceptedBy(p.Root), Ref: ref, Reason: reason,
		Commit: gitOutput(p.Root, "rev-parse", "--short", "HEAD")}
	if err := baseline.AppendExpectation(set.Dir, line); err != nil {
		return expectErr(p, name, err)
	}
	if r, path, err := readLastCompare(p); err == nil && r.Baseline == name {
		if err := remarkLast(p, r, path); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "dropped the expectations of %s from baseline %s: %s\nrecorded in %s%s\n", ref, name, strings.Join(ids, ", "),
		rel(p.Root, refPath(set, ref)), commitNote(p, "; commit it"))
	return nil
}

// importExpectations takes a ref's file from another checkout (a
// worktree's .driftgate/baselines/<name>.expectations/<ref>.jsonl) into a
// baseline of this one, after checking its chain, seal and baseline.
func importExpectations(p *config.Project, name, file string, out io.Writer) error {
	set, err := p.Baseline(name)
	if err != nil {
		return failed(err)
	}
	if !set.Exists() {
		return configErr("no baseline named %q", name)
	}
	ref, changed, err := baseline.ImportExpectations(set.Dir, file)
	var bad *baseline.ExpectProblems
	switch {
	case errors.As(err, &bad):
		return configErr("%s cannot be imported: %v", file, err)
	case errors.Is(err, os.ErrNotExist):
		return configErr("%v", err)
	case err != nil:
		return configErr("%v", err)
	case !changed:
		fmt.Fprintf(out, "baseline %s has the expectations of %s already\n", name, ref)
		return nil
	}
	if r, path, err := readLastCompare(p); err == nil && r.Baseline == name {
		if err := remarkLast(p, r, path); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "imported the expectations of %s into baseline %s: %s%s\n", ref, name, rel(p.Root, refPath(set, ref)), commitNote(p, "; commit it"))
	return nil
}

// closeSatisfied closes the expectations a baseline now holds: those of a
// request whose drifts from the old baseline to its new content (drifts,
// the digests by request) include every drift they expect, and those of
// requests that are gone from the catalog (gone). It reports what it
// closed; what it kept stays in force.
func closeSatisfied(p *config.Project, base *snapshot.Set, name, why, capture string, exps []baseline.Expectation, drifts map[string]map[string]bool, gone func(string) bool, out io.Writer) error {
	closed := map[string][]baseline.Expected{}
	var refs []string
	kept := 0
	for _, e := range exps {
		have := drifts[e.ID]
		all := have != nil
		for _, d := range e.Drifts {
			all = all && have[d]
		}
		if !all && !gone(e.ID) {
			if have != nil {
				kept++ // its request changed, otherwise than it expects
			}
			continue
		}
		if closed[e.Ref] == nil {
			refs = append(refs, e.Ref)
		}
		closed[e.Ref] = append(closed[e.Ref], baseline.Expected{ID: e.ID, Drifts: e.Drifts})
	}
	for _, ref := range refs {
		line := baseline.ExpectLine{Op: baseline.OpClose, At: time.Now().UTC().Format(time.RFC3339), By: acceptedBy(p.Root), Ref: ref,
			Reason: why, Capture: capture, Requests: closed[ref]}
		if err := baseline.AppendExpectation(base.Dir, line); err != nil {
			return expectErr(p, name, err)
		}
		var ids []string
		for _, e := range closed[ref] {
			ids = append(ids, e.ID)
		}
		fmt.Fprintf(out, "closed the expectations of %s the baseline now holds: %s (%s)\n", ref, strings.Join(ids, ", "), rel(p.Root, refPath(base, ref)))
	}
	if kept > 0 {
		fmt.Fprintf(out, "%s kept: their drifts are not all in the baseline (driftgate expect --drop <ref> --reason \"…\" removes one)\n", count(kept, "expectation", "expectations"))
	}
	return nil
}
