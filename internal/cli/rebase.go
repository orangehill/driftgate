package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/allowlist"
	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// rebaseCmd makes a capture the content of a baseline: after a merge the
// baseline holds responses the code no longer gives, for reasons that
// were reviewed elsewhere. The capture passes the checks of a baseline
// made by run (complete, a state fingerprint, private bodies on this
// machine, nothing for git that must stay out of it), and the change is
// one line of the acceptance log. Expectations the new baseline holds are
// closed; the others stay.
func rebaseCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("baselines rebase", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	from := fs.String("from", "", "")
	reason := fs.String("reason", "", "")
	ref := fs.String("ref", "", "")
	inFreeze := fs.Bool("allow-freeze", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 || *from == "" {
		return 0, usageErr("baselines rebase")
	}
	name, tag := pos[0], *from
	p, cat, allow, err := loadProject(*cfg)
	if err != nil {
		return 0, err
	}
	if p.Mode == config.Freeze && !*inFreeze {
		return 0, configErr("mode is freeze: the baseline changes only with intent; a rebase takes changes reviewed elsewhere (a merge) into it, so say so with --allow-freeze, or set mode: evolve in %s", config.FileName)
	}
	if strings.TrimSpace(*reason) == "" {
		return 0, configErr(`baselines rebase needs --reason "…": why the baseline takes this capture (it is written to the acceptance log)`)
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
	base, err := p.Baseline(name)
	if err != nil {
		return 0, failed(err)
	}
	if !base.Exists() {
		return 0, configErr("no baseline named %q (the first run makes one)", name)
	}
	if err := checkBaseline(p, base, name); err != nil {
		return 0, err
	}
	exps, _, err := baseline.Expectations(base.Dir)
	if err != nil {
		return 0, expectErr(p, name, err)
	}
	cur, err := p.Capture(tag)
	if err != nil {
		return 0, failed(err)
	}
	if !cur.Exists() {
		return 0, configErr("no capture named %q", tag)
	}
	// Records are read by several checks; bodies only where they differ.
	base.CacheRecords()
	cur.CacheRecords()
	oldMeta, err := base.Meta()
	if err != nil {
		return 0, envErr(err)
	}
	meta, err := rebaseChecks(p, oldMeta, cur, tag)
	if err != nil {
		return 0, err
	}

	// What changes: the drifts of the capture against the baseline, as
	// compare finds them, and the requests that come or go.
	baseRecs, err := base.Records()
	if err != nil {
		return 0, envErr(err)
	}
	inCatalog := catalogIDs(cat)
	var missing, gone []string
	for _, r := range baseRecs {
		rec, err := cur.Record(r.ID)
		if err != nil {
			return 0, envErr(err)
		}
		switch {
		case rec != nil:
		case inCatalog(r.ID):
			missing = append(missing, r.ID)
		default:
			gone = append(gone, r.ID)
		}
	}
	if len(missing) > 0 {
		return 0, configErr("capture %s has no response to %d requests of the catalog that baseline %s has: %s; a rebase takes the whole capture, so capture all of the catalog (without --only or --target)",
			tag, len(missing), name, listIDs(missing))
	}
	results, err := rebaseDrifts(p, cat, allow, base, cur)
	if err != nil {
		return 0, err
	}
	changed := append([]string{}, gone...)
	sigs := map[string]bool{}
	drifted := map[string]map[string]bool{} // request → digests of its drifts
	added := 0
	for _, res := range results {
		switch {
		case res.Outcome == report.Drift:
			changed = append(changed, res.ID)
			drifted[res.ID] = map[string]bool{}
			for _, d := range res.Drifts {
				sigs[d.Signature()] = true
				drifted[res.ID][d.Digest] = true
			}
		case res.Outcome == report.Skipped && res.Note == report.NoteNoBaseline:
			if rec, err := cur.Record(res.ID); err != nil {
				return 0, envErr(err)
			} else if rec != nil {
				changed = append(changed, res.ID)
				added++
			}
		}
	}
	sort.Strings(changed)

	// The baseline's own settings stay: concurrency: auto was decided with
	// evidence, and a capture at another setting that disagrees falls back.
	if oldMeta != nil {
		meta.Concurrency = oldMeta.Concurrency
	}
	meta.Unreliable, meta.Incomplete = false, false
	a := baseline.Acceptance{
		At:         time.Now().UTC().Format(time.RFC3339),
		By:         acceptedBy(p.Root),
		Reason:     strings.TrimSpace(*reason),
		Ref:        *ref,
		Requests:   changed,
		Signatures: sortedKeys(sigs),
		Capture:    tag,
		Commit:     gitOutput(p.Root, "rev-parse", "--short", "HEAD"),
		Kind:       "rebase",
	}
	if err := rebaseBaseline(base, cur, meta, a); err != nil {
		return 0, envErr(err)
	}

	what := count(len(changed), "request", "requests") + " changed"
	if added > 0 || len(gone) > 0 {
		what += fmt.Sprintf(" (%d new, %d no longer in the catalog)", added, len(gone))
	}
	if oldMeta != nil {
		if n := len(flatState(oldMeta.State, meta.State)); n > 0 {
			what += fmt.Sprintf("; the state differs from the old baseline's in %d items (a new seed or migration)", n)
		}
	}
	fmt.Fprintf(out, "rebased baseline %s on capture %s: %s\n", name, tag, what)
	for _, s := range a.Signatures {
		fmt.Fprintf(out, "  %s\n", report.Printable(s))
	}
	fmt.Fprintf(out, "logged in %s%s\n", rel(p.Root, filepath.Join(base.Dir, baseline.LogFile)), commitNote(p, "; commit the baseline"))
	// The checks of a baseline made by run, said as accept says them: the
	// rebase is written and was asked for, so they never change its exit.
	inv, excl, err := loadInventory(p)
	if err != nil {
		inv, excl = nil, nil // no inventory: the requests alone
	}
	baselineHealth(p, inv, excl, cat, name, nil, out)
	if retried, err := retriedRecords(cur, catalogRequestIDs(cat)); err != nil {
		fmt.Fprintf(out, "could not check the rebased responses for retries: %v\n", err)
	} else {
		retriedBaselineNote(out, name, retried)
	}
	why := "baseline " + name + " rebased on capture " + tag + ", which has these drifts"
	return ExitClean, closeSatisfied(p, base, name, why, tag, exps, drifted, func(id string) bool { return !inCatalog(id) }, out)
}

// catalogRequestIDs are the IDs of the requests of a catalog.
func catalogRequestIDs(cat *catalog.Catalog) []string {
	ids := make([]string, len(cat.Requests))
	for i, req := range cat.Requests {
		ids[i] = req.ID
	}
	return ids
}

// rebaseBaseline writes the capture as the baseline's content next to
// it, with the baseline's acceptance log and the new line, and puts it in
// the baseline's place. The old baseline is not copied first, as accept
// does: none of it but the log stays. The manifest is written from the
// SHA-256 of the bytes as they were copied, as run writes a first one.
func rebaseBaseline(base, cur *snapshot.Set, meta *snapshot.Meta, a baseline.Acceptance) error {
	parent := filepath.Dir(base.Dir)
	if err := keepLineEndings(filepath.Join(parent, ".gitattributes"), snapshot.WriteFile); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-accept-") // interrupted() removes it
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil { // it becomes the baseline folder
		return err
	}
	if _, err := copyFile(filepath.Join(base.Dir, baseline.LogFile), filepath.Join(tmp, baseline.LogFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sums, err := copyFiles(cur.Dir, tmp)
	if err != nil {
		return err
	}
	work := &snapshot.Set{Dir: tmp, Private: base.Private}
	if err := work.WriteMeta(*meta); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "meta.json"))
	if err != nil {
		return err
	}
	sums["meta.json"] = snapshot.Sum(raw)
	if err := baseline.Append(tmp, a); err != nil {
		return err
	}
	if err := baseline.WriteFrom(tmp, sums); err != nil {
		return err
	}
	return installBaseline(base, tmp)
}

// rebaseChecks checks that a capture can be a baseline, as one made by
// run is, and returns its meta.
func rebaseChecks(p *config.Project, oldMeta *snapshot.Meta, cur *snapshot.Set, tag string) (*snapshot.Meta, error) {
	meta, err := cur.Meta()
	if err != nil {
		return nil, envErr(err)
	}
	switch {
	case meta == nil || meta.Incomplete:
		return nil, envErr(fmt.Errorf("capture %s is incomplete (interrupted?); capture it again with --force", tag))
	case meta.ReadOnly:
		return nil, configErr("capture %s was made with --read-only: as a baseline it would leave the requests that change data unchecked; capture it again without --read-only", tag)
	case meta.State == nil && oldMeta != nil && oldMeta.State != nil:
		return nil, configErr("capture %s records no state fingerprint, and the baseline does, so later captures are checked against its data; capture it with the state restored", tag)
	case meta.State != nil && meta.StateFormat != state.FingerprintFormat:
		return nil, configErr("capture %s has a state fingerprint of an earlier format; capture it again", tag)
	case meta.Agreed < 2:
		// run makes a baseline only from two captures of the same state
		// that agree: one capture may hold a value that changes by itself
		// (a time a new request writes), which would drift on the next run.
		skip := ""
		if meta.Unreliable {
			skip = " --skip-fingerprint-check"
		}
		return nil, configErr("capture %[1]s was made once, and a baseline takes only responses that two captures of the same state agree on (as run makes one); capture it twice, add the allowlist entries it proposes, then rebase:\n  driftgate capture %[1]s --repeat 2 --force%[2]s", tag, skip)
	}
	recs, err := cur.Records()
	if err != nil {
		return nil, envErr(err)
	}
	if len(recs) == 0 {
		return nil, configErr("capture %s has no responses", tag)
	}
	// Private bodies stay in the local store; the baseline names them.
	referenced, here, plain, err := privateBodies(cur)
	if err != nil {
		return nil, envErr(err)
	}
	if len(plain) > 0 {
		return nil, configErr("capture %s names %d private bodies by their plain SHA-256 (captured before keyed refs): %s; capture it again", tag, len(plain), listIDs(plain))
	}
	if here < referenced {
		return nil, envErr(fmt.Errorf("capture %s refers to %d private bodies, and %d of them are not on this machine; rebase where it was captured", tag, referenced, referenced-here))
	}
	if err := privateKey(p, "capture:"+tag, recs); err != nil {
		return nil, err
	}
	if err := checkKey(cur, recs); err != nil {
		return nil, err
	}
	// What goes into the baseline is scanned as verify scans a baseline
	// (and capture scanned it before storing it).
	detector, err := pii.New(p.PII.Allow)
	if err != nil {
		return nil, configErr("pii.allow: %v", err)
	}
	leaks, err := repoLeaks(cur, detector)
	if err != nil {
		return nil, envErr(err)
	}
	if len(leaks) > 0 {
		var list []string
		for _, l := range leaks {
			list = append(list, fmt.Sprintf("  %s: %s", l.id, strings.Join(l.found, ", ")))
		}
		return nil, configErr("capture %s has %d responses that a baseline would keep where personal data and credentials must not be:\n%s\nstore those requests private (storage: private in the catalog) and capture again",
			tag, len(leaks), strings.Join(list, "\n"))
	}
	return meta, nil
}

// rebaseDrifts compares the capture with the baseline as compare does,
// with the drifts' digests, without keeping the comparison.
func rebaseDrifts(p *config.Project, cat *catalog.Catalog, allow *allowlist.List, base, cur *snapshot.Set) ([]report.Result, error) {
	origins, _, err := setOrigins(p, base, cur)
	if err != nil {
		return nil, envErr(err)
	}
	var ids []string
	for _, req := range cat.Requests {
		if !req.Observe {
			ids = append(ids, req.ID)
		}
	}
	baseRecs, err := selectRecords(base, ids)
	if err != nil {
		return nil, err
	}
	if err := privateKey(p, "baseline", baseRecs); err != nil {
		return nil, err
	}
	reqs := cat.Requests
	levels := make([]compare.Level, len(reqs))
	for i, req := range reqs {
		levels[i] = p.Level
		if req.Level != "" {
			levels[i], _ = compare.ParseLevel(req.Level)
		}
	}
	results, _, err := compareAll(reqs, levels, base, cur, origins, allow, digester(p), false)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// flatState lists the state items (component/item) whose fingerprint
// differs between two records of it.
func flatState(a, b map[string]map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range []map[string]map[string]string{a, b} {
		for comp, items := range m {
			for item := range items {
				k := comp + "/" + item
				if !seen[k] && a[comp][item] != b[comp][item] {
					out = append(out, k)
				}
				seen[k] = true
			}
		}
	}
	sort.Strings(out)
	return out
}
