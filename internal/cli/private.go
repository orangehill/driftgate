package cli

import (
	"errors"
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
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/generate"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// repoPrivate is the folder in which a baseline carried its private bodies,
// encrypted, before personal data was kept out of git altogether.
const repoPrivate = "private"

// Acceptance-log reasons of verify --migrate-private.
const (
	movedOutReason = "private bodies moved out of the repository"
	keyedRefReason = "private bodies named by a keyed hash"
	leaksReason    = "public bodies with personal data, credentials or no text stored private, header values masked, as a capture does"
)

// privateKey makes sure the private bodies of the records can be read
// before anything is compared: a private body is compared by its content,
// never by its name alone. what names the set in messages.
func privateKey(p *config.Project, what string, recs []*snapshot.Record) error {
	var plain []string
	need := false
	for _, r := range recs {
		switch {
		case r.Body.PlainHash():
			plain = append(plain, r.ID)
		case r.Body.Storage == snapshot.Private && r.Body.Ref != "":
			need = true
		}
	}
	if len(plain) > 0 {
		sort.Strings(plain)
		if name, ok := strings.CutPrefix(what, "baseline:"); ok {
			return configErr("baseline %s names %d private bodies by their plain SHA-256 (made before keyed refs): %s; on the machine whose local store holds them, with the project key, driftgate verify --baseline %s --migrate-private names them by a keyed hash",
				name, len(plain), listIDs(plain), name)
		}
		return configErr("%s names %d private bodies by their plain SHA-256 (captured before keyed refs): %s; capture it again",
			what, len(plain), listIDs(plain))
	}
	if !need {
		return nil
	}
	if _, err := p.LoadKey(); err != nil {
		return envErr(fmt.Errorf("%s has private bodies, encrypted with the project key, and comparing them needs it: set %s or the key source under key: in %s (%v)",
			what, secret.EnvKey, config.FileName, err))
	}
	return nil
}

// checkKey reads the smallest private body of the records that is on this
// machine. A body with the same name as the other side's is not read when
// comparing, so without it a wrong key could go unnoticed.
func checkKey(set *snapshot.Set, recs []*snapshot.Record) error {
	var small *snapshot.Record
	for _, r := range recs {
		if isPrivate(r) && r.Body.Ref != "" && (small == nil || r.Body.Size < small.Body.Size) && set.Private.Has(r.Body.Ref) {
			small = r
		}
	}
	if small == nil {
		return nil
	}
	if _, err := set.Body(small); err != nil {
		return envErr(fmt.Errorf("%s: %w", small.ID, err))
	}
	return nil
}

// privateInRepo reports whether a baseline still carries private bodies in
// the repository (a private/ folder, from before they were kept out of git).
func privateInRepo(set *snapshot.Set) bool {
	_, err := os.Stat(filepath.Join(set.Dir, repoPrivate))
	return err == nil
}

// missingPrivate reports whether r is private and its body is not on this
// machine: it never leaves the local store of the machine that captured it.
func missingPrivate(set *snapshot.Set, r *snapshot.Record) bool {
	return isPrivate(r) && r.Body.Ref != "" && !set.Private.Has(r.Body.Ref)
}

// listIDs lists IDs, at most ten of them.
func listIDs(ids []string) string {
	if len(ids) <= 10 {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:10], ", "), len(ids)-10)
}

// selectRecords reads the records of the given IDs, in parallel; missing
// ones are left out.
func selectRecords(set *snapshot.Set, ids []string) ([]*snapshot.Record, error) {
	recs := make([]*snapshot.Record, len(ids))
	if err := snapshot.Each(len(ids), func(i int) error {
		var err error
		recs[i], err = set.Record(ids[i])
		return err
	}); err != nil {
		return nil, envErr(err)
	}
	out := recs[:0]
	for _, r := range recs {
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// privateBodies describes the private bodies of a baseline: how many it
// refers to by ref, how many of those the local store of this machine
// holds, and the requests that still name their body by its plain SHA-256.
func privateBodies(set *snapshot.Set) (referenced, here int, plain []string, err error) {
	recs, err := set.Records()
	if err != nil {
		return 0, 0, nil, err
	}
	seen := map[string]bool{}
	for _, r := range recs {
		switch {
		case r.Body.PlainHash():
			plain = append(plain, r.ID)
		case r.Body.Storage != snapshot.Private || r.Body.Ref == "" || seen[r.Body.Ref]:
		default:
			seen[r.Body.Ref] = true
			referenced++
			if set.Private.Has(r.Body.Ref) {
				here++
			}
		}
	}
	return referenced, here, plain, nil
}

// migratePrivate brings an older baseline to the rule that personal data
// never enters git: records that name their private body by its plain
// SHA-256 name it by its ref instead (the body, read from the local store,
// is encrypted there again under the ref), and the private bodies the
// baseline carries in its private/ folder move into the local store, the
// folder out of the baseline. What a capture now keeps out of git
// (repoLeaks) goes too: such a public body is stored private, header
// values are masked, and the catalog marks the requests storage: private.
// The change is logged like an acceptance. Every body must be at hand, or
// nothing changes. The caller holds the store's lock.
func migratePrivate(p *config.Project, name string, out io.Writer) error {
	set, err := p.Baseline(name)
	if err != nil {
		return failed(err)
	}
	if !set.Exists() {
		return configErr("no baseline named %q", name)
	}
	if res, err := baseline.Verify(set.Dir); errors.Is(err, baseline.ErrNoManifest) {
		return configErr("baseline %s has no %s; check it, then driftgate verify --baseline %s --adopt, then --migrate-private", name, baseline.ManifestFile, name)
	} else if err != nil {
		return envErr(err)
	} else if !res.OK() {
		return configErr("baseline %s does not verify (%s); restore it from git first", name, strings.Join(res.Problems, "; "))
	}
	recs, err := set.Records()
	if err != nil {
		return envErr(err)
	}
	inRepo := privateInRepo(set)
	var plain []*snapshot.Record
	for _, r := range recs {
		if r.Body.PlainHash() {
			plain = append(plain, r)
		}
	}
	detector, err := pii.New(p.PII.Allow)
	if err != nil {
		return configErr("pii.allow: %v", err)
	}
	leaks, err := repoLeaks(set, detector)
	if err != nil {
		return envErr(err)
	}
	if len(plain) == 0 && !inRepo && len(leaks) == 0 {
		return configErr("baseline %s names its private bodies by a keyed hash, keeps none in the repository and nothing a capture keeps out of git; there is nothing to migrate", name)
	}
	// The catalog marks the requests whose bodies become private, so the
	// next capture stores them so without a finding (exit 2 for personal
	// data or a credential). A generated entry takes its storage from
	// storage: in the configuration, which only the user changes.
	cat, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return configErr("%v", err)
	}
	var marked, generated []string
	for _, l := range leaks {
		i := slices.IndexFunc(cat.Requests, func(r catalog.Request) bool { return r.ID == l.id })
		switch {
		case !l.private || i < 0 || cat.Requests[i].Storage == snapshot.Private:
		case generate.IsGenerated(cat.Requests[i]):
			generated = append(generated, l.id)
		default:
			cat.Requests[i].Storage = snapshot.Private
			marked = append(marked, l.id)
		}
	}
	if _, err := p.LoadKey(); err != nil {
		return envErr(err)
	}

	bodies := map[string][]byte{}
	var missing []string
	for _, r := range plain {
		body, err := set.Private.GetPlain(r.Body.SHA256)
		if err != nil {
			return envErr(fmt.Errorf("%s: %w", r.ID, err))
		}
		if body == nil {
			missing = append(missing, r.ID)
			continue
		}
		bodies[r.ID] = body
	}
	if len(missing) > 0 {
		return configErr("%d private bodies of baseline %s are not in the local store (%s), so nothing was changed:\n  %s\nthey are on the machine that captured the baseline",
			len(missing), name, set.Private.Dir, strings.Join(missing, "\n  "))
	}

	// The bodies in private/ were sealed with the project key and named by
	// the same ref; each is decrypted and checked before it is stored locally.
	var ids, reasons []string
	moved := 0
	if inRepo {
		repo := &snapshot.PrivateStore{Dir: filepath.Join(set.Dir, repoPrivate), Key: p.LoadKey}
		seen := map[string]bool{}
		for _, r := range recs {
			if r.Body.Storage != snapshot.Private || r.Body.Ref == "" || !repo.Has(r.Body.Ref) {
				continue
			}
			ids = append(ids, r.ID)
			if seen[r.Body.Ref] {
				continue
			}
			seen[r.Body.Ref] = true
			body, err := repo.Get(r.Body.Ref)
			if err != nil {
				return envErr(fmt.Errorf("%s: %w", r.ID, err))
			}
			ref, err := set.Private.Put(body)
			if err != nil {
				return envErr(err)
			}
			if ref != r.Body.Ref {
				return envErr(fmt.Errorf("%s: private body named %s under the project key, not %s (encrypted with another key?)", r.ID, ref, r.Body.Ref))
			}
			moved++
		}
		reasons = append(reasons, movedOutReason)
	}
	for _, r := range plain {
		ids = append(ids, r.ID)
	}
	if len(plain) > 0 {
		reasons = append(reasons, keyedRefReason)
	}
	for _, l := range leaks {
		ids = append(ids, l.id)
	}
	if len(leaks) > 0 {
		reasons = append(reasons, leaksReason)
	}
	sort.Strings(ids)
	ids = slices.Compact(ids)
	a := baseline.Acceptance{
		At:       time.Now().UTC().Format(time.RFC3339),
		By:       acceptedBy(p.Root),
		Reason:   strings.Join(reasons, "; "),
		Requests: ids,
		Commit:   gitOutput(p.Root, "rev-parse", "--short", "HEAD"),
	}
	err = rewriteBaseline(set, a, func(work *snapshot.Set) error {
		for _, r := range plain {
			if err := work.Write(r, bodies[r.ID]); err != nil {
				return err
			}
		}
		for _, l := range leaks {
			r, err := work.Record(l.id)
			if err != nil {
				return err
			}
			if l.headers != nil {
				r.Headers = l.headers
			}
			var body []byte // nil: the record keeps its body
			if l.private {
				if body, err = work.Body(r); err != nil {
					return fmt.Errorf("%s: %w", l.id, err)
				}
				r.Body.Storage = snapshot.Private
			}
			if err := work.Write(r, body); err != nil {
				return err
			}
		}
		return os.RemoveAll(filepath.Join(work.Dir, repoPrivate))
	})
	if err != nil {
		return err
	}
	if len(marked) > 0 {
		if err := cat.Save(p.Layout.Catalog); err != nil {
			return envErr(err)
		}
	}
	if inRepo {
		fmt.Fprintf(out, "moved %d private bodies of baseline %s out of the repository into the local store (%s)\n", moved, name, set.Private.Dir)
	}
	if len(plain) > 0 {
		fmt.Fprintf(out, "named %d private bodies of baseline %s by a keyed hash instead of their plain SHA-256\n", len(plain), name)
	}
	if len(leaks) > 0 {
		var bodies, headers []string
		for _, l := range leaks {
			if l.private {
				bodies = append(bodies, l.id)
			}
			if l.headers != nil {
				headers = append(headers, l.id)
			}
		}
		if len(bodies) > 0 {
			fmt.Fprintf(out, "stored %d public bodies of baseline %s as private, as a capture now stores them: %s\n", len(bodies), name, listIDs(bodies))
		}
		if len(headers) > 0 {
			fmt.Fprintf(out, "masked header values of %d records of baseline %s as a capture does: %s\n", len(headers), name, listIDs(headers))
		}
		if len(marked) > 0 {
			fmt.Fprintf(out, "marked %d requests storage: private in %s: %s\n", len(marked), p.Layout.Catalog, listIDs(marked))
		}
		if len(generated) > 0 {
			fmt.Fprintf(out, "%d generated requests take their storage from %s (storage.public names them): %s; take them out of storage.public and run driftgate generate --write\n",
				len(generated), config.FileName, listIDs(generated))
		}
		fmt.Fprintln(out, "the values are in the history of git: treat committed credentials as leaked")
	}
	fmt.Fprintf(out, "logged in %s%s. The private bodies stay on this machine: elsewhere (CI) the private requests cannot be compared, see --skip-private\n", baseline.LogFile, commitNote(p, "; commit the baseline"))
	return nil
}

// rewriteBaseline changes a copy of the baseline, drops public bodies no
// record uses, appends the acceptance and writes the manifest; then the copy
// replaces the baseline, so an interruption leaves the old baseline intact.
// Private bodies the change writes go into the local store.
func rewriteBaseline(base *snapshot.Set, a baseline.Acceptance, change func(work *snapshot.Set) error) error {
	return swapBaseline(base, func(work *snapshot.Set) error {
		if err := change(work); err != nil {
			return err
		}
		if err := dropUnusedBodies(work.Dir); err != nil {
			return err
		}
		return baseline.Append(work.Dir, a)
	})
}

// swapBaseline changes a copy of the baseline and writes its manifest; then
// the copy replaces the baseline, so an interruption leaves the old
// baseline intact (or, if the swap itself fails half way, in
// .tmp-old-*/baseline, where interrupted() finds it).
func swapBaseline(base *snapshot.Set, change func(work *snapshot.Set) error) error {
	parent := filepath.Dir(base.Dir)
	if err := keepLineEndings(filepath.Join(parent, ".gitattributes"), snapshot.WriteFile); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-accept-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil { // it becomes the baseline folder
		return err
	}
	if _, err := copyFiles(base.Dir, tmp); err != nil {
		return err
	}
	if err := change(&snapshot.Set{Dir: tmp, Private: base.Private}); err != nil {
		return err
	}
	if err := baseline.Write(tmp); err != nil {
		return err
	}
	return installBaseline(base, tmp)
}

// installBaseline puts the baseline written in tmp (a folder next to it)
// in the place of base, keeping the old one aside until the new one is
// there.
func installBaseline(base *snapshot.Set, tmp string) error {
	parent := filepath.Dir(base.Dir)
	oldDir, err := os.MkdirTemp(parent, ".tmp-old-")
	if err != nil {
		return err
	}
	old := filepath.Join(oldDir, "baseline")
	if err := rename(base.Dir, old); err != nil {
		os.Remove(oldDir)
		return err
	}
	chaos.Point("baseline.swap")
	if err := rename(tmp, base.Dir); err != nil {
		if rerr := rename(old, base.Dir); rerr != nil {
			// The old baseline stays where interrupted() says it is.
			return fmt.Errorf("%w; the old baseline is in %s", err, old)
		}
		os.Remove(oldDir)
		return err
	}
	os.RemoveAll(oldDir)
	return nil
}

// lineEndings is the .gitattributes DriftGate keeps in the baselines folder:
// a baseline is checked byte for byte against its manifest, so git must
// not change its line endings on checkout (core.autocrlf, which Git for
// Windows turns on).
const lineEndings = "# DriftGate checks baselines byte for byte (manifest.json): git keeps them as written.\n* -text\n"

// keepLineEndings writes lineEndings to path (a .gitattributes in the
// baselines folder) unless the project has one there.
func keepLineEndings(path string, write func(path string, data []byte, perm os.FileMode) error) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	return write(path, []byte(lineEndings), 0o644)
}

// ensureLineEndings writes the .gitattributes of the baselines folder of a
// project whose baselines were made before DriftGate wrote one, so
// git on Windows does not change their line endings on the next checkout.
func ensureLineEndings(p *config.Project, out io.Writer) error {
	dir := p.Layout.Baselines
	path := filepath.Join(dir, ".gitattributes")
	if _, err := os.Stat(dir); err != nil {
		return nil // no baselines yet: the first run or accept writes it
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	if err := keepLineEndings(path, snapshot.WriteFile); err != nil {
		return envErr(err)
	}
	fmt.Fprintf(out, "wrote %s (* -text: git keeps baselines as they are written)%s\n", path, commitNote(p, "; commit it"))
	return nil
}

// commitNote is note when the project's files go into git (baselines:
// git), else nothing.
func commitNote(p *config.Project, note string) string {
	if p.Layout.Committed() {
		return note
	}
	return ""
}

// crlfHint says what to do with a baseline that git checked out with CRLF
// line endings (baseline.Result.LineEndings); dir is the baselines folder.
// Baselines git does not hold were changed by something else.
func crlfHint(p *config.Project, dir string) string {
	if !p.Layout.Committed() {
		return "its files have CRLF line endings, which something other than driftgate wrote (an editor, a copy); restore the baseline from a backup or make it again"
	}
	return fmt.Sprintf("git checked the baseline out with CRLF line endings (core.autocrlf, on by default on Windows): commit %s (* -text), then check the baselines out again: git rm -r -q --cached %s && git checkout HEAD -- %s",
		filepath.Join(dir, ".gitattributes"), dir, dir)
}

// rename is os.Rename; a test makes it fail.
var rename = os.Rename

// dropUnusedBodies removes public bodies that no record refers to any more.
func dropUnusedBodies(dir string) error {
	used := map[string]bool{}
	recs, err := (&snapshot.Set{Dir: dir}).Records()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.Body.Storage == snapshot.Public {
			used[r.Body.SHA256] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "bodies"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && !used[e.Name()] {
			if err := os.Remove(filepath.Join(dir, "bodies", e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
