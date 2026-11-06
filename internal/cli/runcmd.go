package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/capture"
	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/report"
	"github.com/orangehill/driftgate/internal/routes"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// runTag is the capture run writes on every call: one per checkout, as
// the last comparison (lastReport), so two worktrees that share the store
// never capture into the same folder.
func runTag(p *config.Project) string { return "run-" + checkoutID(p) }

// runCmd does everything in one command. The first time (no baseline yet) it
// writes the route inventory and the generated catalog, seeds the state,
// captures twice on the same state and keeps the result as the baseline
// only when both captures agree. Later it checks that routes and catalog
// are unchanged, captures, and compares with the baseline.
func runCmd(args []string, out, errOut io.Writer) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfg := fs.String("config", "", "")
	baseline := fs.String("baseline", "local", "")
	noRestore := fs.Bool("allow-mutating-without-restore", false, "")
	skipPrivate := fs.Bool("skip-private", false, "")
	readOnly := fs.Bool("read-only", false, "")
	targets := targetFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 0 {
		return 0, usageErr("run")
	}
	p, err := config.Load(*cfg)
	if err != nil {
		return 0, configErr("%v", err)
	}
	base, err := p.Baseline(*baseline)
	if err != nil {
		return 0, failed(err)
	}
	ctx, stop := signalContext(errOut)
	defer stop()
	unlock, err := lockStore(ctx, p, errOut)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if err := interrupted(p, errOut); err != nil {
		return 0, err
	}
	if err := ensureLineEndings(p, out); err != nil {
		return 0, err
	}
	first := !base.Exists()
	if first {
		// The first run makes the baseline folder at its end: a folder there
		// already (without responses/) would stop it after the whole capture.
		// An empty one, as git leaves behind, is taken away.
		if entries, err := os.ReadDir(base.Dir); err == nil {
			if len(entries) > 0 {
				return 0, configErr("%s exists but holds no baseline (no responses/); move it away, and the first run makes the baseline there", base.Dir)
			}
			if err := os.Remove(base.Dir); err != nil {
				return 0, envErr(err)
			}
		}
	} else {
		// Before minutes of capturing: a baseline changed by hand stops here.
		if err := checkBaseline(p, base, *baseline); err != nil {
			return 0, err
		}
		// Nor does a baseline whose private bodies cannot be read.
		recs, err := base.Records()
		if err != nil {
			return 0, envErr(err)
		}
		if err := privateKey(p, "baseline:"+*baseline, recs); err != nil {
			return 0, err
		}
	}
	sel, err := selectTargets(p, targets)
	if err != nil {
		return 0, err
	}
	// Before anything runs (the seed, routes, a capture).
	if *readOnly {
		if err := capture.CheckReadOnly(p); err != nil {
			return 0, failed(err)
		}
	}
	if first && *readOnly {
		return 0, configErr("there is no baseline yet: the first run makes it from every request, so it takes no --read-only (a read-only baseline would leave the requests that change data unchecked)")
	}
	if first && sel != nil {
		return 0, configErr("there is no baseline yet: the first run makes it for every service, so it takes no --target")
	}
	step := func(name string) { fmt.Fprintf(out, "\n== %s ==\n", name) }
	code := ExitClean
	var reasons []string // why the run fails besides drift in responses

	// Seed first: parameters with sql read the pristine database.
	if seeded, err := capture.Seeded(ctx, p); err != nil {
		return 0, failed(err)
	} else if !seeded {
		step("seed")
		if err := capture.Seed(ctx, p, capture.SeedOptions{Out: out, Log: errOut}); err != nil {
			return 0, failed(err)
		}
	}

	// The first run adopts the routes and the generated catalog. Later runs
	// report a changed inventory or catalog as drift, but still capture with
	// the catalog as it is and compare: one run gives the whole picture, and
	// the requests stay the ones the baseline was made with.
	var checked routeCheck
	if len(p.Discover) > 0 {
		if code, reasons, checked, err = runRoutes(ctx, p, sel, first, *baseline, step, out, errOut); err != nil {
			return 0, err
		}
	}
	listed, err := catalog.Load(p.Layout.Catalog)
	if err != nil {
		return 0, configErr("%v", err)
	}
	if listed = sel.requests(listed); len(listed.Requests) == 0 {
		return 0, configErr("the catalog has no requests: configure discover: in %s (see driftgate init) or write catalog entries", *cfg)
	}
	cat, allow, err := loadFiles(p)
	if err != nil {
		return 0, err
	}
	cat = sel.requests(cat)

	step("capture")
	var retried []string // of the capture a first run makes the baseline from
	opts := captureOptions{force: true, quiet: true, baseline: *baseline, restore: "once", repeat: 1, noRestore: *noRestore, readOnly: *readOnly,
		first: func(sum *capture.Summary) { retried = retriedIDs(sum) }}
	if first {
		// Two sequential captures that agree, then concurrency: auto is
		// decided against the first one.
		opts.repeat, opts.calibrate = 2, len(autoServices(p, cat)) > 0
	}
	c, err := captureSet(ctx, p, cat, allow, runTag(p), opts, out, errOut)
	if err != nil {
		return 0, err
	}
	if c != ExitClean {
		if first && c == ExitDrift {
			fmt.Fprintln(out, "\nno baseline was made: add the proposed allowlist entries (with reasons) and run again")
		}
		return max(code, c), nil
	}

	if first {
		step("baseline")
		set, err := p.Capture(runTag(p))
		if err != nil {
			return 0, failed(err)
		}
		if err := copyTree(set.Dir, base.Dir); err != nil {
			return 0, err
		}
		if err := keepLineEndings(filepath.Join(filepath.Dir(base.Dir), ".gitattributes"), snapshot.WriteFile); err != nil {
			return 0, envErr(err)
		}
		fmt.Fprintf(out, "baseline %s made from two equal captures: %s\n%s", *baseline, base.Dir,
			commitNote(p, fmt.Sprintf("commit %s/ (catalog, routes, allowlist and the baseline)\n", rel(p.Root, p.Layout.Dir))))
		// A bug the baseline now holds is said; the baseline stays.
		c, why := baselineHealth(p, checked.inv, checked.excl, cat, *baseline, nil, out)
		code, reasons = max(code, c), append(reasons, why...)
		retriedBaselineNote(out, *baseline, retried)
		leftOver(p, base, *baseline, out)
		failReasons(out, reasons)
		return code, nil
	}

	step("compare")
	c, err = compareSets(p, cat, allow, runTag(p), "baseline:"+*baseline, compareOptions{format: "text", maxDrifts: report.DefaultDrifts, skipPrivate: *skipPrivate,
		sel: sel, coverage: checked.coverage}, out)
	if err != nil {
		return 0, err
	}
	failReasons(out, reasons)
	sel.partial(p, out)
	return max(code, c), nil
}

// runRoutes is the routes part of run: discover, generate and coverage. On
// the first run discover and generate write what they find; later runs
// only check. It returns the exit code and why the run fails.
func runRoutes(ctx context.Context, p *config.Project, sel selection, first bool, baseName string, step func(string), out, errOut io.Writer) (int, []string, routeCheck, error) {
	var none routeCheck
	code := ExitClean
	var reasons []string
	step("discover")
	c, found, err := discoverRoutes(ctx, p, sel, first, out, errOut)
	if err != nil {
		return 0, nil, none, err
	}
	if c != ExitClean {
		reasons = append(reasons, "the routes changed (driftgate discover --write adopts them)")
	}
	code = max(code, c)

	step("generate")
	whole, _, err := loadFiles(p)
	if err != nil {
		return 0, nil, none, err
	}
	if c, err = generateCatalog(ctx, p, sel, whole, generateOptions{write: first, check: !first}, out, errOut); err != nil {
		return 0, nil, none, err
	}
	if c != ExitClean {
		fmt.Fprintln(out, "the catalog differs from what generate proposes; it is used as it is — review with driftgate generate, adopt with generate --write")
		reasons = append(reasons, "the catalog differs from what generate proposes (driftgate generate --write adopts it)")
	}
	code = max(code, c)

	// Coverage checks the routes found now, also when they are not adopted.
	step("coverage")
	cat, _, err := loadFiles(p)
	if err != nil {
		return 0, nil, none, err
	}
	inv, excl, err := withExclusions(p, found, "the inventory discover found")
	if err != nil {
		return 0, nil, none, err
	}
	// A baseline there already says which requests succeed; one that
	// cannot be read for it leaves the catalog alone to decide (compare
	// then reports what is wrong with it).
	answers, err := readAnswers(p, baseName, nil)
	if err != nil {
		fmt.Fprintf(out, "warning: baseline %s was not checked for errors kept in it: %v\n", baseName, err)
		answers = nil
	}
	cov, c, err := checkCoverage(sel, cat, inv, excl, answers, "text", out)
	if err != nil {
		return 0, nil, none, err
	}
	if c != ExitClean {
		reasons = append(reasons, "routes are not covered (see coverage above)")
	}
	return max(code, c), reasons, routeCheck{inv, excl, cov}, nil
}

// routeCheck is the inventory and exclusions run checked coverage with,
// and the coverage it found.
type routeCheck struct {
	inv      *routes.Inventory
	excl     *routes.Exclusions
	coverage *routes.Coverage
}

// copyTree copies a capture into a new baseline folder, with its manifest:
// first beside it, then renamed, so a failed copy never leaves a half
// baseline behind. Private bodies stay where the capture put them, in the
// local store; the records name them by their ref.
func copyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return configErr("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".tmp-baseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil { // it becomes the baseline folder
		return err
	}
	sums, err := copyFiles(src, tmp)
	if err != nil {
		return err
	}
	// The manifest holds the hashes of the bytes written, taken as they were
	// copied: the copy is not read again.
	if err := baseline.WriteFrom(tmp, sums); err != nil {
		return err
	}
	chaos.Point("baseline.copy")
	return os.Rename(tmp, dst)
}

// copyFiles copies the files under src to dst, in parallel, and returns the
// SHA-256 of each by slash path.
func copyFiles(src, dst string) (map[string]string, error) {
	var rels []string
	var sizes []int64
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		var size int64
		if fi, err := d.Info(); err == nil {
			size = fi.Size()
		}
		rels, sizes = append(rels, rel), append(sizes, size)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rels))
	err = parallel(len(rels), func(i int) int64 { return sizes[i] }, func(i int) error {
		var err error
		out[i], err = copyFile(filepath.Join(src, rels[i]), filepath.Join(dst, rels[i]))
		return err
	})
	if err != nil {
		return nil, err
	}
	sums := make(map[string]string, len(rels))
	for i, rel := range rels {
		sums[filepath.ToSlash(rel)] = out[i]
	}
	return sums, nil
}

// copyFile copies one file and returns the SHA-256 of what it wrote.
func copyFile(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), in); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// failReasons says why the run fails when the responses alone do not.
func failReasons(out io.Writer, reasons []string) {
	if len(reasons) == 0 {
		return
	}
	fmt.Fprintln(out, "\nthe run fails because:")
	for _, r := range reasons {
		fmt.Fprintln(out, "  - "+r)
	}
}

// leftOver warns when a new baseline has the expectations of an earlier
// one of its name: they name drifts against that one.
func leftOver(p *config.Project, base *snapshot.Set, name string, out io.Writer) {
	dir := baseline.ExpectDir(base.Dir)
	left, _ := os.ReadDir(dir)
	// A file a killed write left is no expectation (baseline.readRefs).
	left = slices.DeleteFunc(left, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), snapshot.TempPrefix) })
	if len(left) > 0 {
		fmt.Fprintf(out, "warning: %s/ holds expectations of an earlier baseline %s; they name drifts against that one, so check them (driftgate expect --drop <ref> --reason …, or remove the folder)\n",
			rel(p.Root, dir), name)
	}
}
