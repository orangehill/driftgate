package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/glob"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// writeManifest writes the manifest of a baseline that driftgate made or changed.
func writeManifest(dir string) error { return baseline.Write(dir) }

// verified are the baselines that verified in this command, by folder,
// with their stamp at the time: a command checks a baseline at several
// steps (run: before the capture, against its state, before the compare),
// and one that has not been written since is not read again. The cache
// lives for one command only (Main empties it), as tests run many commands
// in one process and edit baselines between them.
var verified = struct {
	sync.Mutex
	stamps map[string]string
}{stamps: map[string]string{}}

func forgetVerified() {
	verified.Lock()
	clear(verified.stamps)
	verified.Unlock()
}

// checkBaseline refuses a baseline that changed outside driftgate: a result
// compared with it, or a capture checked against its state, cannot be trusted.
func checkBaseline(p *config.Project, set *snapshot.Set, name string) error {
	// The stamp is taken before the check, so a change during the check
	// makes the next one read the files again.
	stamp, err := baseline.Stamp(set.Dir)
	if err == nil {
		verified.Lock()
		ok := verified.stamps[set.Dir] == stamp
		verified.Unlock()
		if ok {
			return nil
		}
	}
	if err := verifyBaseline(p, set, name); err != nil {
		return err
	}
	if stamp != "" {
		verified.Lock()
		verified.stamps[set.Dir] = stamp
		verified.Unlock()
	}
	return nil
}

func verifyBaseline(p *config.Project, set *snapshot.Set, name string) error {
	res, err := baseline.Verify(set.Dir)
	if errors.Is(err, baseline.ErrNoManifest) {
		return envErr(fmt.Errorf("baseline %s has no %s (made before manifests, or the file was removed); check it, then driftgate verify --baseline %s --adopt writes one", name, baseline.ManifestFile, name))
	}
	if err != nil {
		return envErr(err)
	}
	if !res.OK() {
		more := ""
		if n := len(res.Problems) - 1; n > 0 {
			more = fmt.Sprintf(" and %d more", n)
		}
		hint := "run driftgate verify"
		if res.LineEndings {
			hint = crlfHint(p, filepath.Dir(set.Dir))
		}
		return envErr(fmt.Errorf("baseline %s changed outside driftgate accept (%s%s); %s", name, res.Problems[0], more, hint))
	}
	// Its private bodies would be ignored: they belong in the local store.
	if privateInRepo(set) {
		return configErr("baseline %s carries private bodies in the repository (%s/, encrypted), and personal data stays out of git: driftgate verify --baseline %s --migrate-private moves them into the local store",
			name, repoPrivate, name)
	}
	return nil
}

// digests identify the compared sets for accept: the manifest of a baseline
// and the records of a capture.
func digests(base *snapshot.Set, baseName string, cur *snapshot.Set, curName string) (string, string, error) {
	var b, c string
	var err error
	if strings.HasPrefix(baseName, "baseline:") {
		if b, err = baseline.Digest(base.Dir); err != nil {
			return "", "", envErr(err)
		}
	}
	if strings.HasPrefix(curName, "capture:") {
		if c, err = baseline.SetDigest(cur.Dir); err != nil {
			return "", "", envErr(err)
		}
	}
	return b, c, nil
}

// readLastCompare reads the report compare kept in the local store.
func readLastCompare(p *config.Project) (*report.Report, string, error) {
	path, err := lastReport(p)
	if err != nil {
		return nil, "", failed(err)
	}
	r, err := report.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", configErr("there is no comparison yet: run driftgate compare or driftgate run first")
	}
	if err != nil {
		return nil, "", configErr("%v", err)
	}
	if r.Version != report.Version {
		return nil, "", configErr("%s: unsupported version %d", path, r.Version)
	}
	return r, path, nil
}

// staleComparison says why the last comparison no longer describes its
// sets; empty when it still does. Only a comparison of a capture against a
// baseline can be accepted from.
func staleComparison(p *config.Project, r *report.Report) (string, error) {
	tag, ok := strings.CutPrefix(r.Tag, "capture:")
	if r.Baseline == "" || !ok {
		return fmt.Sprintf("the last comparison is %s against %s; accept takes drifts of a capture compared against a baseline", r.Tag, r.Against), nil
	}
	base, err := p.Baseline(r.Baseline)
	if err != nil {
		return "", failed(err)
	}
	if !base.Exists() {
		return fmt.Sprintf("baseline %s is gone", r.Baseline), nil
	}
	if d, err := baseline.Digest(base.Dir); err != nil || d != r.BaselineDigest {
		return fmt.Sprintf("baseline %s changed since the comparison", r.Baseline), nil
	}
	cur, err := p.Capture(tag)
	if err != nil {
		return "", failed(err)
	}
	if !cur.Exists() {
		return fmt.Sprintf("capture %s is gone", tag), nil
	}
	if d, err := baseline.SetDigest(cur.Dir); err != nil || d != r.CaptureDigest {
		return fmt.Sprintf("capture %s changed since the comparison (captured again?)", tag), nil
	}
	if meta, err := cur.Meta(); err != nil || meta == nil || meta.Unreliable {
		return fmt.Sprintf("capture %s was made with --skip-fingerprint-check, on data that may differ from the baseline's; it is for diagnosis only and cannot be accepted", tag), nil
	}
	return "", nil
}

// comparisonID names one comparison and its group numbering: review prints
// it with the accept command, so group:N takes the group that was reviewed
// even if compare ran again in between.
func comparisonID(r *report.Report) string {
	h := sha256.New()
	fmt.Fprintln(h, r.Created, r.Tag, r.Against, r.CaptureDigest)
	for _, g := range r.Groups {
		fmt.Fprintln(h, g.N, g.Signature)
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:6])
}

// acceptable reports whether a result has something to take into the
// baseline: drifts, or a record the baseline does not have yet.
func acceptable(res report.Result) bool {
	return res.Outcome == report.Drift || (res.Outcome == report.Skipped && res.Note == "no baseline")
}

// acceptPick is what a selection takes from the last comparison.
type acceptPick struct {
	ids  []string // whole requests, sorted
	sigs []string // signatures of their drifts, sorted
}

// chooseAccepted resolves selections (request IDs, globs over IDs, group:N)
// against the last comparison. A request is taken with all of its drifts or
// not at all: a group that covers only some drifts of a request is refused,
// with what is missing.
func chooseAccepted(r *report.Report, args []string) (*acceptPick, error) {
	done := map[string]bool{}
	for _, id := range r.Accepted {
		done[id] = true
	}
	byID := map[string]report.Result{}
	for _, res := range r.Results {
		byID[res.ID] = res
	}
	groupOf := map[string]int{}
	for _, g := range r.Groups {
		groupOf[g.Signature] = g.N
	}
	whole := map[string]bool{}
	named := map[string]bool{}  // requests named by their ID
	groups := map[string]bool{} // selected signatures
	var groupArgs []string
	for _, a := range args {
		if n, ok := strings.CutPrefix(a, "group:"); ok {
			num, err := strconv.Atoi(n)
			if err != nil || num < 1 || num > len(r.Groups) {
				return nil, configErr("%s: the last comparison has groups 1 to %d", a, len(r.Groups))
			}
			groups[r.Groups[num-1].Signature] = true
			groupArgs = append(groupArgs, a)
			continue
		}
		if strings.ContainsAny(a, "*?") {
			matched := 0
			for _, res := range r.Results {
				if glob.Match(a, res.ID) && acceptable(res) && !done[res.ID] {
					whole[res.ID] = true
					matched++
				}
			}
			if matched == 0 {
				return nil, configErr("%s matches no request with drift (or without a baseline record) in the last comparison", a)
			}
			continue
		}
		res, ok := byID[a]
		switch {
		case !ok:
			return nil, configErr("%s is not in the last comparison", a)
		case done[a]:
			return nil, configErr("%s was already accepted from the last comparison", a)
		case !acceptable(res):
			return nil, configErr("%s has nothing to accept (%s)", a, res.Outcome)
		}
		whole[a], named[a] = true, true
	}

	var partial []string
	for _, res := range r.Results {
		if res.Outcome != report.Drift || done[res.ID] || whole[res.ID] {
			continue
		}
		in := false
		missing := map[int]int{} // group → drifts not selected
		for _, d := range res.Drifts {
			if groups[d.Signature()] {
				in = true
			} else {
				missing[groupOf[d.Signature()]]++
			}
		}
		if !in {
			continue
		}
		if len(missing) == 0 {
			whole[res.ID] = true
			continue
		}
		n, list := 0, make([]string, 0, len(missing))
		for g, c := range missing {
			n += c
			list = append(list, fmt.Sprintf("group:%d (%d)", g, c))
		}
		sort.Strings(list)
		partial = append(partial, fmt.Sprintf("  %s: %d of %d drifts not selected, in %s", res.ID, n, len(res.Drifts), strings.Join(list, ", ")))
	}
	if len(partial) > 0 {
		return nil, configErr("%s takes only part of the drifts of %d requests; a request is accepted with all of its drifts or not at all:\n%s\nselect those groups too, or name the requests (driftgate review shows the command that covers them)",
			strings.Join(groupArgs, " "), len(partial), strings.Join(partial, "\n"))
	}
	if len(whole) == 0 {
		return nil, configErr("nothing to accept: the selected drifts were already accepted")
	}
	// Accepting a request that was not captured removes it from the
	// baseline; a capture of part of the catalog must not do that through
	// a glob or a group.
	var gone []string
	for id := range whole {
		for _, d := range byID[id].Drifts {
			if d.Kind == compare.KindNotCaptured && !named[id] {
				gone = append(gone, id)
				break
			}
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		return nil, configErr("%s were not captured this time; accepting them removes them from the baseline, so name each by its ID to do that", strings.Join(gone, ", "))
	}
	p := &acceptPick{}
	sigs := map[string]bool{}
	for id := range whole {
		p.ids = append(p.ids, id)
		for _, d := range byID[id].Drifts {
			sigs[d.Signature()] = true
		}
	}
	for s := range sigs {
		p.sigs = append(p.sigs, s)
	}
	sort.Strings(p.ids)
	sort.Strings(p.sigs)
	return p, nil
}

func acceptCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	reason := fs.String("reason", "", "")
	ref := fs.String("ref", "", "")
	reviewed := fs.String("comparison", "", "") // the comparison review showed
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) == 0 {
		return 0, usageErr("accept")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if p.Mode == config.Freeze {
		return 0, configErr("mode is freeze: the API must stay as the baseline has it, so drifts are not accepted; fix the change, or set mode: evolve in %s", config.FileName)
	}
	if strings.TrimSpace(*reason) == "" {
		return 0, configErr(`accept needs --reason "…": why the change is intended (it is written to the acceptance log)`)
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
	r, path, err := readLastCompare(p)
	if err != nil {
		return 0, err
	}
	if r.Baseline != "" {
		base, err := p.Baseline(r.Baseline)
		if err != nil {
			return 0, failed(err)
		}
		if base.Exists() {
			if err := checkBaseline(p, base, r.Baseline); err != nil {
				return 0, err
			}
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
	sel, err := chooseAccepted(r, pos)
	if err != nil {
		return 0, err
	}
	base, _ := p.Baseline(r.Baseline)
	tag := strings.TrimPrefix(r.Tag, "capture:")
	cur, err := p.Capture(tag)
	if err != nil {
		return 0, failed(err)
	}
	a := baseline.Acceptance{
		At:         time.Now().UTC().Format(time.RFC3339),
		By:         acceptedBy(p.Root),
		Reason:     strings.TrimSpace(*reason),
		Ref:        *ref,
		Requests:   sel.ids,
		Signatures: sel.sigs,
		Capture:    tag,
		Commit:     gitOutput(p.Root, "rev-parse", "--short", "HEAD"),
	}
	if err := applyAccept(base, cur, sel.ids, a); err != nil {
		return 0, err
	}

	// The rest of the comparison still holds: the other records did not change.
	r.Accepted = append(r.Accepted, sel.ids...)
	sort.Strings(r.Accepted)
	if r.BaselineDigest, err = baseline.Digest(base.Dir); err != nil {
		return 0, envErr(err)
	}
	if err := r.Save(path); err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "accepted %d requests into baseline %s: %s\n", len(sel.ids), r.Baseline, strings.Join(sel.ids, ", "))
	for _, s := range sel.sigs {
		fmt.Fprintf(out, "  %s\n", s)
	}
	rel, err := filepath.Rel(p.Root, filepath.Join(base.Dir, baseline.LogFile))
	if err != nil {
		rel = filepath.Join(base.Dir, baseline.LogFile)
	}
	fmt.Fprintf(out, "logged in %s%s\n", rel, commitNote(p, "; commit the baseline"))
	return ExitClean, nil
}

// interrupted handles what an interrupted accept or first run left next to
// the baselines; the caller holds the store's lock, so no other command of
// this checkout is writing them. A copy being made (.tmp-accept-*,
// .tmp-baseline-*) is removed: the baselines are as they were before it.
// A baseline moved aside (.tmp-old-*) stops the command: the baseline may
// be missing or half written, and a run would make a new one from the code
// under test.
func interrupted(p *config.Project, out io.Writer) error {
	left, _ := filepath.Glob(filepath.Join(p.Layout.Baselines, ".tmp-*"))
	var old []string
	for _, dir := range left {
		if strings.HasPrefix(filepath.Base(dir), ".tmp-old-") {
			old = append(old, dir)
		}
	}
	if len(old) > 0 {
		return envErr(fmt.Errorf("%s left by an interrupted accept; if a baseline is missing, the old one is in .tmp-old-*/baseline. Restore it from git (git checkout -- %s), then remove these folders",
			strings.Join(left, ", "), p.Layout.Baselines))
	}
	for _, dir := range left {
		if err := os.RemoveAll(dir); err != nil {
			return envErr(err)
		}
		fmt.Fprintf(out, "removed %s, left by an interrupted accept or first run\n", dir)
	}
	return nil
}

// applyAccept copies the records of the accepted requests from the capture
// into the baseline, with their bodies: public ones into the baseline,
// private ones (already there) stay in the local store, named by their ref.
func applyAccept(base, cur *snapshot.Set, ids []string, a baseline.Acceptance) error {
	return rewriteBaseline(base, a, func(work *snapshot.Set) error {
		for _, id := range ids {
			rec, err := cur.Record(id)
			if err != nil {
				return envErr(err)
			}
			if rec == nil {
				// Not captured any more: the baseline drops the request too.
				if err := os.Remove(filepath.Join(work.Dir, "responses", id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
			var body []byte // nil for a failed request
			if rec.Body.SHA256 != "" || rec.Body.Ref != "" {
				if body, err = cur.Body(rec); err != nil {
					return envErr(fmt.Errorf("%s: %w", id, err))
				}
			}
			if err := work.Write(rec, body); err != nil {
				return err
			}
		}
		return nil
	})
}

// acceptedBy names the person accepting, from git config; the system user
// when git has no identity.
func acceptedBy(root string) string {
	name, email := gitOutput(root, "config", "user.name"), gitOutput(root, "config", "user.email")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if email != "" {
		return fmt.Sprintf("%s <%s>", name, email)
	}
	return name
}

// gitOutput runs git in dir and returns its trimmed output; empty when git
// is missing or fails (not a repository, no commit yet).
func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = secret.Environ()
	raw, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// driftGroups returns the groups (by number) of a request's drifts.
func driftGroups(r *report.Report, drifts []compare.Drift) []int {
	n := map[string]int{}
	for _, g := range r.Groups {
		n[g.Signature] = g.N
	}
	seen := map[int]bool{}
	var out []int
	for _, d := range drifts {
		if g := n[d.Signature()]; !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Ints(out)
	return out
}
