package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/lazy"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/storelock"
)

// journalName is the file that marks a switch of baselines: in progress, in
// the project's .driftgate/ folder: "<mode> copy" while the files are
// copied, "<mode> clean" once the new configuration is in place, and on
// the next line the configuration it came from. Commands refuse to run
// while it is there; driftgate baselines <mode> finishes the switch.
const journalName = "switching"

type journal struct {
	mode, phase, from string
}

func readJournal(root string) (journal, bool) {
	raw, err := os.ReadFile(filepath.Join(root, LocalDir, journalName))
	if err != nil {
		return journal{}, false
	}
	head, from, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	mode, phase, _ := strings.Cut(head, " ")
	return journal{mode, phase, strings.TrimSpace(from)}, true
}

func writeJournal(root string, j journal) error {
	return snapshot.WriteFile(filepath.Join(root, LocalDir, journalName), []byte(j.mode+" "+j.phase+"\n"+j.from+"\n"), 0o600)
}

// Switch moves a project to the other baselines: mode (driftgate
// baselines). It writes the new place fully, then puts the new
// configuration in place, then removes the old place; one stopped at any
// point is finished by running it again.
type Switch struct {
	From *Project // the project as it is laid out now
	To   Layout   // where its files go

	cleaning bool   // the new configuration is in place; only the old place is left to remove
	text     []byte // the text of the new configuration
	locks    []func()
}

// ErrSwitched means the project already has the mode.
var ErrSwitched = errors.New("already in that mode")

// loadGitKey reads the key as baselines: git finds it; a test replaces it,
// so that no test reads the Keychain.
var loadGitKey = func(project string, src secret.Source) (secret.Key, error) {
	return secret.Load(project, src, "")
}

// PrepareSwitch reads the project a switch to mode starts from: the one of
// path, or of the working folder (as Load finds it). Nothing is written.
func PrepareSwitch(path, mode string) (*Switch, error) {
	if mode != BaselinesLocal && mode != BaselinesGit {
		return nil, fmt.Errorf("baselines must be %s or %s, got %q", BaselinesLocal, BaselinesGit, mode)
	}
	from := BaselinesLocal
	if mode == BaselinesLocal {
		from = BaselinesGit
	}
	var root string
	if path != "" {
		file, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		root, _ = locate(file)
	} else {
		var err error
		if root, err = projectRoot("."); err != nil {
			return nil, err
		}
		path = configPath(root, from)
	}
	j, switching := readJournal(root)
	if switching && j.mode != mode {
		return nil, fmt.Errorf("a switch to baselines: %s stopped before it was done; finish it first (driftgate baselines %s), then switch back", j.mode, j.mode)
	}
	if switching && j.from != "" {
		path = j.from
	}
	s := &Switch{cleaning: j.phase == "clean"}
	if s.cleaning {
		// The source configuration may be gone: the new one says the rest.
		p, err := load(configPath(root, mode), false)
		if err != nil {
			return nil, err
		}
		s.To = p.Layout
		if p.Layout, err = NewLayout(p.Config, root, from); err != nil {
			return nil, err
		}
		p.Layout.Config = path
		s.From = p
		return s, nil
	}
	if !switching {
		// The new place's configuration must not be there: its files
		// would be overwritten with older ones.
		if _, err := configIn(root); err != nil {
			return nil, err
		}
	}
	p, err := load(path, false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNoConfig) {
		if !switching {
			if _, err := load(configPath(root, mode), false); err == nil {
				return nil, fmt.Errorf("%s: %w", root, ErrSwitched)
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if p.Layout.Mode == mode {
		return nil, fmt.Errorf("%s: %w", root, ErrSwitched)
	}
	if own := ownStore(p.Layout, p.Project); own != p.Layout.Store && exists(own) {
		return nil, invalid{fmt.Errorf("the store is %s (DRIFTGATE_STORE or store:), which a switch leaves where it is, but the project's own store %s holds data too, which it would leave behind. Move what %s holds into %s, or unset DRIFTGATE_STORE and remove store: so that the switch moves it, then run driftgate baselines %s again",
			p.Layout.Store, own, own, p.Layout.Store, mode)}
	}
	raw, err := os.ReadFile(p.Layout.Config)
	if err != nil {
		return nil, err
	}
	// Before anything is written: a configuration it cannot rewrite must
	// not leave a switch half done.
	if s.text, err = withBaselines(raw, mode); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Layout.Config, err)
	}
	// The configuration as the other mode reads it.
	c := p.Config
	c.Baselines = mode
	if s.To, err = NewLayout(c, root, mode); err != nil {
		return nil, err
	}
	s.From = p
	return s, nil
}

// ownStore is the store of a layout when neither DRIFTGATE_STORE nor
// store: names another.
func ownStore(l Layout, project string) string {
	if l.Mode == BaselinesLocal {
		return filepath.Join(l.Dir, "store")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".driftgate", project)
}

// configPath is where a project at root in mode keeps driftgate.yaml.
func configPath(root, mode string) string {
	if mode == BaselinesLocal {
		return filepath.Join(root, LocalDir, FileName)
	}
	return filepath.Join(root, FileName)
}

// projectRoot walks up from dir to the project folder as Find does, also
// past a switch in progress or a configuration in both places.
func projectRoot(dir string) (string, error) {
	start, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := start; ; {
		if exists(filepath.Join(d, FileName)) || exists(filepath.Join(d, LocalDir, FileName)) || exists(filepath.Join(d, LocalDir, journalName)) {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d || exists(filepath.Join(d, ".git")) {
			return "", fmt.Errorf("%w in %s or a folder above it", ErrNoConfig, start)
		}
		d = parent
	}
}

// Run makes the switch. enterGit checks what a switch to git puts into
// the repository (the baselines) and refuses with an error; it runs before
// anything is written. errOut gets the messages of a wait for a store
// lock.
func (s *Switch) Run(ctx context.Context, enterGit func(*Project) error, out, errOut io.Writer) error {
	f, t := s.From.Layout, s.To
	root := f.Root
	// The stores stay locked until the old place is gone: a command that
	// loaded the project before and waits for the lock then finds it moved
	// (Project.Current) instead of writing into the old place.
	if err := s.lock(ctx, f.Store, errOut); err != nil {
		return err
	}
	defer s.unlock()
	if s.cleaning && t.Store != f.Store {
		if err := s.lock(ctx, t.Store, errOut); err != nil {
			return err
		}
	}
	if !s.cleaning {
		if err := s.copy(ctx, enterGit, errOut); err != nil {
			return err
		}
	}
	// The old place: its files, its store when it is the project's own,
	// its key file, then the configuration and the journal.
	moved, err := s.entries()
	if err != nil {
		return err
	}
	for _, e := range moved {
		if err := os.RemoveAll(filepath.Join(f.Dir, e)); err != nil {
			return err
		}
		chaos.Point("baselines.remove")
	}
	if f.Key != "" && f.Key != t.Key {
		if err := os.Remove(f.Key); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(f.Config); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(root, LocalDir, journalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The project's own store in its folder: all but the lock while it is
	// held (Windows removes no open file), then the rest.
	ownGone := f.Store != t.Store && f.Ignored != "" && within(f.Ignored, f.Store)
	if ownGone {
		list, err := os.ReadDir(f.Store)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, e := range list {
			if e.Name() != storelock.FileName {
				if err := os.RemoveAll(filepath.Join(f.Store, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	s.unlock()
	if ownGone {
		if err := os.RemoveAll(f.Store); err != nil {
			return err
		}
	}
	os.Remove(f.Dir) // when nothing else is in it

	fmt.Fprintf(out, "baselines: %s: the configuration is %s, the project's files are in %s/, the store is %s\n", t.Mode, rel(root, t.Config), rel(root, t.Dir), t.Store)
	if t.Committed() {
		fmt.Fprintf(out, "commit %s and %s/; the key stays where it was (DRIFTGATE_KEY, key: or the Keychain), not in the repository\n", rel(root, t.Config), rel(root, t.Dir))
	} else {
		fmt.Fprintf(out, "git sees %s and %s/ removed: commit that; from now on they are only in %s/, which git ignores\n", rel(root, f.Config), rel(root, f.Dir), rel(root, t.Dir))
	}
	if others := s.others(); len(others) > 0 {
		fmt.Fprintf(out, "%s stay where they are: they are not DriftGate's files\n", strings.Join(others, ", "))
	}
	if f.Store != t.Store && exists(f.Store) && (f.Ignored == "" || !within(f.Ignored, f.Store)) {
		fmt.Fprintf(out, "the store %s stays: other checkouts of %s may use it; once none does, remove it (rm -rf %s)\n", f.Store, s.From.Project, f.Store)
	}
	return nil
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}

// copy writes the new place fully and then puts the new configuration in
// place: the steps up to the old place's removal.
func (s *Switch) copy(ctx context.Context, enterGit func(*Project) error, errOut io.Writer) error {
	f, t := s.From.Layout, s.To
	root := f.Root
	if t.Committed() {
		if err := enterGit(s.From); err != nil {
			return err
		}
		if err := s.gitKey(); err != nil {
			return err
		}
	}
	if err := t.WriteIgnores(); err != nil {
		return err
	}
	if err := t.checkIgnored(true); err != nil {
		return err
	}
	if t.Store != f.Store {
		if err := s.lock(ctx, t.Store, errOut); err != nil {
			return err
		}
	}
	// Nothing in the other place is replaced: a conflict stops the switch
	// before it starts.
	check := &merger{dry: true, checkout: s.From.CheckoutID()}
	if err := s.merge(check); err != nil {
		return err
	}
	if len(check.conflicts) > 0 {
		return invalid{fmt.Errorf("nothing was moved: %s exist already, with other content than in %s (a baseline or capture of the same name from another checkout?); keep the right one and remove the other, then run driftgate baselines %s again",
			strings.Join(check.conflicts, ", "), rel(root, f.Dir), t.Mode)}
	}
	if err := writeJournal(root, journal{t.Mode, "copy", f.Config}); err != nil {
		return err
	}
	if err := s.localKey(); err != nil {
		return err
	}
	if err := s.merge(&merger{checkout: s.From.CheckoutID()}); err != nil {
		return err
	}
	if err := snapshot.WriteFile(t.Config, s.text, 0o644); err != nil {
		return err
	}
	chaos.Point("baselines.flip")
	return writeJournal(root, journal{t.Mode, "clean", f.Config})
}

// merge copies the project's files and its store into the new place.
func (s *Switch) merge(m *merger) error {
	f, t := s.From.Layout, s.To
	moved, err := s.entries()
	if err != nil {
		return err
	}
	for _, e := range moved {
		from, to := filepath.Join(f.Dir, e), filepath.Join(t.Dir, e)
		merge := m.file
		if fi, err := os.Stat(from); err == nil && fi.IsDir() {
			merge = m.sets
		}
		if err := merge(from, to); err != nil {
			return err
		}
		if !m.dry {
			chaos.Point("baselines.copy")
		}
	}
	if t.Store != f.Store && exists(f.Store) {
		return m.store(f.Store, t.Store)
	}
	return nil
}

// projectFiles are the project's files in its folder, which a switch
// moves: what driftgate/ holds with baselines: git.
var projectFiles = []string{"catalog.json", "allowlist.yaml", "routes.json", "exclusions.yaml", "classify.yaml", "baselines"}

// entries are the project's files the old place has.
func (s *Switch) entries() ([]string, error) {
	var out []string
	for _, name := range projectFiles {
		if exists(filepath.Join(s.From.Layout.Dir, name)) {
			out = append(out, name)
		}
	}
	return out, nil
}

// others are the files in the old place's folder that are not DriftGate's
// (a tokens file kept there): they stay where they are.
func (s *Switch) others() []string {
	f := s.From.Layout
	list, _ := os.ReadDir(f.Dir)
	var out []string
	for _, e := range list {
		path := filepath.Join(f.Dir, e.Name())
		switch path {
		case f.Config, f.Key, f.Store, ownStore(f, s.From.Project), filepath.Join(f.Root, LocalDir, journalName):
			continue
		}
		if !slices.Contains(projectFiles, e.Name()) {
			out = append(out, rel(f.Root, path))
		}
	}
	return out
}

// gitKey makes sure baselines: git finds the key of the key file, which
// is not copied into the repository and goes with .driftgate/.
func (s *Switch) gitKey() error {
	file := s.From.Layout.Key
	if file == "" || !exists(file) {
		return nil // key: or DRIFTGATE_KEY, which git mode reads as well
	}
	now, err := secret.ReadFile(file)
	if err != nil {
		return err
	}
	then, err := loadGitKey(s.From.Project, s.From.Key)
	switch {
	case errors.Is(err, secret.ErrNoKey):
		return noKey{fmt.Errorf("no key for baselines: git, which reads the key from key: in driftgate.yaml, DRIFTGATE_KEY or the macOS Keychain, never from %s. Put the key there, then run driftgate baselines git again:\n"+
			"  macOS: security add-generic-password -s driftgate -a %s -w   (paste the content of %s when asked)\n"+
			"  elsewhere: export DRIFTGATE_KEY=\"$(cat %s)\", or key: { op: \"op://…\" } in driftgate.yaml", file, s.From.Project, file, file)}
	case err != nil:
		return err
	case !bytes.Equal(now, then):
		return invalid{fmt.Errorf("baselines: git would read another key (from key:, DRIFTGATE_KEY or the Keychain) than the one in %s, which the switch removes: its private bodies would become unreadable. Make the source git mode reads hold the key of %s (unset another DRIFTGATE_KEY), then run driftgate baselines git again", file, file)}
	}
	return nil
}

// localKey writes the key file of a switch to local from where baselines:
// git found the key, unless key: in driftgate.yaml names its source.
func (s *Switch) localKey() error {
	t := s.To
	if t.Key == "" || s.From.Key != (secret.Source{}) || exists(t.Key) {
		return nil
	}
	k, err := s.From.LoadKey()
	if errors.Is(err, secret.ErrNoKey) {
		return nil // driftgate key create makes one
	}
	if err != nil {
		return err
	}
	return secret.WriteFile(t.Key, k)
}

// lock takes the lock of a store for the switch, until unlock.
func (s *Switch) lock(ctx context.Context, store string, errOut io.Writer) error {
	if err := os.MkdirAll(store, 0o700); err != nil {
		return err
	}
	release, err := storelock.Acquire(ctx, store, func(int) {
		fmt.Fprintf(errOut, "another driftgate is using the store %s; waiting until it is done (Ctrl-C to stop)\n", store)
	})
	if err == nil {
		s.locks = append(s.locks, release)
	}
	return err
}

func (s *Switch) unlock() {
	for i := len(s.locks) - 1; i >= 0; i-- {
		s.locks[i]()
	}
	s.locks = nil
}

// merger copies one place's files into the other's without removing or
// replacing anything there: the store of baselines: git is shared by the
// checkouts of the project. Folders of sets (baselines/, captures/,
// private/, pristine/) are merged by entry: a set (a baseline, a capture)
// is copied whole when the other place lacks it, a file when it lacks it;
// one there already must be the same, else it is a conflict. dry only
// collects the conflicts.
type merger struct {
	dry       bool
	conflicts []string
	checkout  string // CheckoutID: this checkout's run capture and last comparison are its own
}

// store merges a store: its files and its folders of sets.
func (m *merger) store(src, dst string) error {
	return m.folder(src, dst, func(from, to string) error { return m.sets(from, to) })
}

// sets merges a folder of sets.
func (m *merger) sets(src, dst string) error {
	return m.folder(src, dst, m.set)
}

func (m *merger) folder(src, dst string, dir func(from, to string) error) error {
	list, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range list {
		if e.Name() == storelock.FileName || strings.HasPrefix(e.Name(), switchTemp) {
			continue
		}
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			err = dir(from, to)
		} else {
			err = m.file(from, to)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// switchTemp starts the name of a set being copied.
const switchTemp = ".switch-"

// set copies a set whole, through a temporary name, unless the same is
// there.
//
// This checkout's own run capture replaces the one it left there before
// (a switch back and forth).
func (m *merger) set(src, dst string) error {
	own := filepath.Base(dst) == "run-"+m.checkout && filepath.Base(filepath.Dir(dst)) == "captures"
	if exists(dst) && !own {
		if same, err := sameTree(src, dst); err != nil || same {
			return err
		}
		m.conflicts = append(m.conflicts, dst)
		return nil
	}
	if m.dry {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(dst), switchTemp+filepath.Base(dst))
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := copyDir(src, tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// file copies a file unless the same is there. The last comparison of
// this checkout is the newer one where it moves from; those of other
// checkouts stay with them. Of two records of a pristine copy the newer
// is kept: a seed replaced the copy since.
func (m *merger) file(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	name, last := filepath.Base(src), "last-compare-"+m.checkout+".json"
	if strings.HasPrefix(name, "last-compare-") && name != last {
		return nil
	}
	if dfi, err := os.Lstat(dst); err == nil && filepath.Base(filepath.Dir(dst)) == "pristine" {
		if m.dry || !fi.ModTime().After(dfi.ModTime()) {
			return nil
		}
		return copyFile(src, dst, fi)
	}
	if exists(dst) && name != last {
		a, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(dst)
		if err != nil || bytes.Equal(a, b) {
			return err
		}
		m.conflicts = append(m.conflicts, dst)
		return nil
	}
	if m.dry {
		return nil
	}
	return copyFile(src, dst, fi)
}

// sameTree reports whether two folders hold the same files with the same
// content.
func sameTree(a, b string) (bool, error) {
	count := 0
	same := true
	err := filepath.WalkDir(a, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !same {
			return err
		}
		count++
		r, _ := filepath.Rel(a, path)
		x, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		y, err := os.ReadFile(filepath.Join(b, r))
		same = err == nil && bytes.Equal(x, y)
		return nil
	})
	if err != nil || !same {
		return false, err
	}
	err = filepath.WalkDir(b, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count--
		}
		return err
	})
	return count == 0, err
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, r)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(to, fi.Mode().Perm()|0o700)
		}
		return copyFile(path, to, fi)
	})
}

// copyFile copies a file (or a symbolic link, as a link) with its
// permissions.
func copyFile(src, dst string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		return os.Symlink(target, dst)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return snapshot.WriteFile(dst, raw, fi.Mode().Perm())
}

var (
	baselinesLine = lazy.Regexp(`(?m)^baselines:[ \t]*(?:"[A-Za-z]*"|'[A-Za-z]*'|[A-Za-z]*)`)
	baselinesFull = lazy.Regexp(`(?m)^baselines:.*(\r?\n|$)`)
	projectLine   = lazy.Regexp(`(?m)^project:.*$`)
)

// withBaselines is the text of a configuration for mode, its comments and
// layout kept. For local the baselines: line is replaced, or added after
// project:; for git it is taken out: a driftgate.yaml at the root without
// it is git, and an earlier release reads it.
func withBaselines(raw []byte, mode string) ([]byte, error) {
	var text []byte
	if mode == BaselinesGit {
		mode = ""
		text = baselinesFull().ReplaceAll(raw, nil)
	} else if baselinesLine().Match(raw) {
		text = baselinesLine().ReplaceAll(raw, []byte("baselines: "+mode))
	} else if loc := projectLine().FindIndex(raw); loc != nil {
		eol := "\n"
		if bytes.Contains(raw, []byte("\r\n")) {
			eol = "\r\n"
		}
		end := loc[1]
		if end < len(raw) && raw[end] == '\r' {
			end++
		}
		if end < len(raw) {
			end++ // the \n
		}
		line := "baselines: " + mode + eol
		if end == len(raw) && !bytes.HasSuffix(raw, []byte("\n")) {
			line = eol + "baselines: " + mode + eol
		}
		text = append(append(append([]byte{}, raw[:end]...), line...), raw[end:]...)
	} else {
		return nil, errors.New("no project: line to put baselines: after")
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil || c.Baselines != mode {
		return nil, fmt.Errorf("cannot set baselines: in it for the switch (%v); set it by hand", err)
	}
	return text, nil
}
