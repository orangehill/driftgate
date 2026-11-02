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
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/httpx"
	"github.com/orangehill/driftgate/internal/pii"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/scan"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/state"
)

// verifyCmd checks baselines against their manifests: every file (changed,
// missing, extra) and the chain of the acceptance log. Private bodies are
// not in the repository, so it only counts how many of them this machine
// holds; a missing one is not a problem of the baseline. Without --baseline
// it checks every baseline of the project. --adopt writes the manifest of a
// baseline that has none (made before manifests); --migrate-private brings
// an older baseline to keyed refs and keeps what a capture keeps out of
// git out of it; --migrate-state records the state fingerprint of an older
// baseline in the current format.
func verifyCmd(args []string, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	name := fs.String("baseline", "", "")
	adopt := fs.Bool("adopt", false, "")
	migrate := fs.Bool("migrate-private", false, "")
	newFormat := fs.Bool("migrate-state", false, "")
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("verify")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if *adopt && (*migrate || *newFormat) || *migrate && *newFormat {
		return 0, configErr("--adopt, --migrate-private and --migrate-state are separate steps; run them one at a time, --adopt first")
	}
	if *adopt {
		if *name == "" {
			return 0, configErr("--adopt needs --baseline <name>")
		}
		return 0, adoptBaseline(p, *name, out)
	}
	if *migrate || *newFormat {
		flag := "--migrate-private"
		if *newFormat {
			flag = "--migrate-state"
		}
		if *name == "" {
			return 0, configErr("%s needs --baseline <name>", flag)
		}
		// The baseline is rewritten through the store (.tmp files, private
		// bodies): no other command may sweep or write it meanwhile.
		ctx, stop := signalContext(os.Stderr)
		defer stop()
		unlock, err := lockStore(ctx, p, os.Stderr)
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
		if *newFormat {
			return 0, migrateState(ctx, p, *name, out)
		}
		return 0, migratePrivate(p, *name, out)
	}
	if err := ensureLineEndings(p, out); err != nil {
		return 0, err
	}
	names := []string{*name}
	if *name == "" {
		if names, err = baselineNames(p); err != nil {
			return 0, err
		}
		if len(names) == 0 {
			return 0, configErr("there are no baselines in %s", p.Layout.Baselines)
		}
	}
	return verifyBaselines(p, names, out)
}

// baselineNames lists the baselines of the project.
func baselineNames(p *config.Project) ([]string, error) {
	entries, err := os.ReadDir(p.Layout.Baselines)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, envErr(err)
	}
	var names []string
	for _, e := range entries {
		if (e.IsDir() || e.Type()&os.ModeSymlink != 0) && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// verifyBaselines checks baselines as verify does: their files against
// the manifest, and what they put into git. It returns the exit code.
func verifyBaselines(p *config.Project, names []string, out io.Writer) (int, error) {
	detector, err := pii.New(p.PII.Allow)
	if err != nil {
		return 0, configErr("pii.allow: %v", err)
	}
	code := ExitClean
	changed := false // a baseline changed outside driftgate
	if left, _ := filepath.Glob(filepath.Join(p.Layout.Baselines, ".tmp-*")); len(left) > 0 {
		fmt.Fprintf(out, "FAIL   %s left by an interrupted accept or first run; they must not be committed\n", strings.Join(left, ", "))
		code = ExitDrift
		changed = true
	}
	for _, n := range names {
		set, err := p.Baseline(n)
		if err != nil {
			return 0, failed(err)
		}
		if !set.Exists() {
			return 0, configErr("no baseline named %q", n)
		}
		res, err := baseline.Verify(set.Dir)
		if err != nil && !errors.Is(err, baseline.ErrNoManifest) && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "FAIL   baseline %s: %v\n", n, err)
			code = ExitDrift
			changed = true
			continue
		}
		if errors.Is(err, baseline.ErrNoManifest) {
			fmt.Fprintf(out, "FAIL   baseline %s: no %s; check the baseline, then driftgate verify --baseline %s --adopt\n", n, baseline.ManifestFile, n)
			code = ExitDrift
			changed = true
			continue
		}
		if err != nil {
			return 0, envErr(err)
		}
		if !res.OK() {
			fmt.Fprintf(out, "FAIL   baseline %s changed outside driftgate accept:\n", n)
			for _, pr := range res.Problems {
				fmt.Fprintf(out, "         %s\n", pr)
			}
			if res.LineEndings {
				fmt.Fprintf(out, "         %s\n", crlfHint(p, p.Layout.Baselines))
			}
			code = ExitDrift
			changed = true
			continue
		}
		// What the repository must not hold: private bodies, even encrypted,
		// and plain hashes of them.
		if privateInRepo(set) {
			fmt.Fprintf(out, "FAIL   baseline %s carries private bodies in the repository (%s/, encrypted); personal data stays out of git\n         on the machine with the project key: driftgate verify --baseline %s --migrate-private moves them into the local store\n",
				n, repoPrivate, n)
			code = ExitDrift
			continue
		}
		leaks, err := repoLeaks(set, detector)
		if err != nil {
			return 0, envErr(err)
		}
		if len(leaks) > 0 {
			fmt.Fprintf(out, "FAIL   baseline %s: %d records would publish in git what must stay out of it:\n", n, len(leaks))
			for _, l := range leaks {
				fmt.Fprintf(out, "         %s: %s\n", l.id, strings.Join(l.found, ", "))
			}
			fmt.Fprintf(out, "         on the machine with the project key: driftgate verify --baseline %s --migrate-private stores those bodies private (encrypted, in the local store), masks those header values as a capture does and marks the requests storage: private in the catalog; the values were committed, so treat credentials as leaked\n", n)
			code = ExitDrift
			continue
		}
		referenced, here, plain, err := privateBodies(set)
		if err != nil {
			return 0, envErr(err)
		}
		if len(plain) > 0 {
			fmt.Fprintf(out, "FAIL   baseline %s: %d private bodies are named by their plain SHA-256 (made before keyed refs): %s\n         on the machine whose local store holds them, with the project key: driftgate verify --baseline %s --migrate-private\n",
				n, len(plain), listIDs(plain), n)
			code = ExitDrift
			continue
		}
		fmt.Fprintf(out, "OK     baseline %s: %d files, %d acceptances\n", n, res.Files, res.Acceptances)
		if referenced > 0 {
			fmt.Fprintf(out, "         %d private bodies are outside the repository (encrypted in the local store, checked against their ref when read); %d of them are on this machine\n", referenced, here)
		}
	}
	if changed {
		restore := "restore it from git (git checkout -- <baseline folder>)"
		if !p.Layout.Committed() {
			restore = "restore it from a backup, or make it again"
		}
		fmt.Fprintf(out, "\na baseline changes only through driftgate accept; %s or accept the change again\n", restore)
	}
	return code, nil
}

// adoptBaseline writes the manifest of a baseline made before manifests
// existed. A baseline that has one changes only through accept.
func adoptBaseline(p *config.Project, name string, out io.Writer) error {
	set, err := p.Baseline(name)
	if err != nil {
		return failed(err)
	}
	if !set.Exists() {
		return configErr("no baseline named %q", name)
	}
	if _, err := baseline.Read(set.Dir); err == nil {
		return configErr("baseline %s already has a %s; it changes only through driftgate accept", name, baseline.ManifestFile)
	} else if !errors.Is(err, baseline.ErrNoManifest) {
		return configErr("%v", err)
	}
	if err := writeManifest(set.Dir); err != nil {
		return configErr("%v", err)
	}
	// The files agree with a fresh manifest; the log chain may still not.
	if res, err := baseline.Verify(set.Dir); err != nil || !res.OK() {
		os.Remove(filepath.Join(set.Dir, baseline.ManifestFile))
		if err == nil {
			err = errors.New(strings.Join(res.Problems, "; "))
		}
		return configErr("baseline %s: %v", name, err)
	}
	fmt.Fprintf(out, "wrote %s for baseline %s%s\n", baseline.ManifestFile, name, commitNote(p, "; commit it with the baseline"))
	return nil
}

// leak is a record of a baseline that puts into git what a capture now
// keeps out of it.
type leak struct {
	id      string
	found   []string    // what was found and where, never the value
	headers [][2]string // the record's headers as a capture masks them; nil: unchanged
	private bool        // its public body is stored private by a capture
}

// repoLeaks scans what a baseline puts into git as capture scans it before
// it does: every public body (personal data, credentials, a body that
// cannot be scanned) and every record's headers (values capture masks). A
// baseline made by an older DriftGate or edited by hand can
// hold what a capture now keeps out.
func repoLeaks(set *snapshot.Set, d *pii.Detector) ([]leak, error) {
	recs, err := set.Records()
	if err != nil {
		return nil, err
	}
	var out []leak
	for _, r := range recs {
		var l leak
		for i, kv := range r.Headers {
			v := httpx.MaskHeader(kv[0], kv[1])
			if v != kv[1] {
				l.found = append(l.found, "credential at header:"+kv[0])
			} else if f, masked := d.Headers([][2]string{kv}); len(f) > 0 && masked[0][1] != kv[1] {
				// A value masked already is found again: only an unmasked one counts.
				l.found = append(l.found, f[0].Kind+" at "+f[0].Where)
				v = masked[0][1]
			}
			if v != kv[1] {
				if l.headers == nil {
					l.headers = slices.Clone(r.Headers)
				}
				l.headers[i][1] = v
			}
		}
		if r.Body.Storage == snapshot.Public && r.Body.SHA256 != "" {
			body, err := set.Body(r)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.ID, err)
			}
			findings, unscanned := scan.Public(d, r.Headers, body)
			if unscanned != "" {
				l.found = append(l.found, "a public body that cannot be scanned for personal data: "+report.Printable(unscanned))
			}
			for _, f := range findings {
				l.found = append(l.found, f.Kind+" at "+f.Where)
			}
			l.private = unscanned != "" || len(findings) > 0
		}
		if len(l.found) > 0 {
			l.id, l.found = r.ID, slices.Compact(l.found)
			out = append(out, l)
		}
	}
	return out, nil
}

// migrateState records the state fingerprint of a baseline captured before
// fingerprint format 2 in the current format, once capture.MigrateState
// has shown that the data is the one the baseline was captured from. The
// change is logged like an acceptance; the records stay as they are.
func migrateState(ctx context.Context, p *config.Project, name string, out io.Writer) error {
	set, err := p.Baseline(name)
	if err != nil {
		return failed(err)
	}
	if !set.Exists() {
		return configErr("no baseline named %q", name)
	}
	if err := checkBaseline(p, set, name); err != nil {
		return err
	}
	meta, err := set.Meta()
	if err != nil {
		return envErr(err)
	}
	if meta == nil || meta.State == nil {
		return configErr("baseline %s records no state fingerprint; there is nothing to migrate", name)
	}
	if meta.StateFormat == state.FingerprintFormat {
		return configErr("baseline %s has the current state fingerprint format already; there is nothing to migrate", name)
	}
	from := max(meta.StateFormat, 1)
	if meta.State, err = capture.MigrateState(ctx, p, meta, out, os.Stderr); err != nil {
		return failed(err)
	}
	meta.StateFormat = state.FingerprintFormat
	a := baseline.Acceptance{
		At:       time.Now().UTC().Format(time.RFC3339),
		By:       acceptedBy(p.Root),
		Reason:   fmt.Sprintf("%s %d (the data of format %d)", stateFormatReason, state.FingerprintFormat, from),
		Requests: []string{},
		Commit:   gitOutput(p.Root, "rev-parse", "--short", "HEAD"),
	}
	if err := rewriteBaseline(set, a, func(work *snapshot.Set) error { return work.WriteMeta(*meta) }); err != nil {
		return err
	}
	fmt.Fprintf(out, "the data is the one baseline %s was captured from; its state fingerprint is now in format %d\nlogged in %s%s\n",
		name, state.FingerprintFormat, baseline.LogFile, commitNote(p, "; commit the baseline"))
	return nil
}

// stateFormatReason starts the acceptance-log reason of verify --migrate-state.
const stateFormatReason = "state fingerprint recorded in format"
