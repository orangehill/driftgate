package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/storelock"
)

// Where a project keeps its files (baselines: in driftgate.yaml).
const (
	// BaselinesLocal keeps everything in the project's .driftgate/ folder,
	// which git ignores: the configuration, catalog, allowlist, baselines,
	// the store and the key. Nothing goes outside the project.
	BaselinesLocal = "local"
	// BaselinesGit is the layout earlier releases used: driftgate.yaml
	// and driftgate/ in the repository, the store in
	// ~/.driftgate/<project>.
	BaselinesGit = "git"
)

// LocalDir is the project's own folder of baselines: local.
const LocalDir = ".driftgate"

// Layout is where the files of a project are. Only this package knows the
// mode: the layout is computed once, when the configuration is loaded, and
// every other package takes its paths from it.
type Layout struct {
	Mode      string // BaselinesLocal or BaselinesGit
	Root      string // the project folder (.gitignore, .env and the application's files)
	Config    string // driftgate.yaml
	Dir       string // the folder of the catalog, allowlist, routes, exclusions and baselines
	Catalog   string // catalog.json
	Allowlist string // allowlist.yaml
	Baselines string // the folder of the baselines, one folder each

	// The local store: captures, encrypted private bodies, pristine
	// records, the last comparisons and the lock. Empty when store: is not
	// usable; StoreDir says why.
	Store    string
	Captures string // one folder per tag
	Private  string // encrypted private bodies
	Pristine string // records of the pristine copies
	Lock     string // the lock of the commands that write the store

	// Key is the key file; empty when the key comes from elsewhere only
	// (the environment, 1Password, the macOS Keychain).
	Key string
	// Ignored is the folder git must ignore before anything sensitive is
	// written into it (the key, captures, private bodies, baselines); empty
	// when none of them is inside the project.
	Ignored string
}

// NewLayout is the layout of a project at root in mode, with the store of
// configuration c (store:, DRIFTGATE_STORE). The error is the one of an
// unusable store, which matters only to a command that uses it; the layout
// is complete apart from the store's paths.
func NewLayout(c Config, root, mode string) (Layout, error) {
	l := Layout{Mode: mode, Root: root}
	switch mode {
	case BaselinesLocal:
		l.Dir = filepath.Join(root, LocalDir)
		l.Config = filepath.Join(l.Dir, FileName)
		l.Key = filepath.Join(l.Dir, "key")
		l.Ignored = l.Dir
	case BaselinesGit:
		l.Dir = filepath.Join(root, Dir)
		l.Config = filepath.Join(root, FileName)
	default:
		return l, fmt.Errorf("%s: baselines must be %s or %s, got %q", FileName, BaselinesLocal, BaselinesGit, mode)
	}
	l.Catalog = filepath.Join(l.Dir, "catalog.json")
	l.Allowlist = filepath.Join(l.Dir, "allowlist.yaml")
	l.Baselines = filepath.Join(l.Dir, "baselines")
	store, err := storePath(c, l)
	if err != nil {
		return l, err
	}
	l.Store = store
	l.Captures = filepath.Join(store, "captures")
	l.Private = filepath.Join(store, "private")
	l.Pristine = filepath.Join(store, "pristine")
	l.Lock = filepath.Join(store, storelock.FileName)
	return l, nil
}

// storePath is the local store: DRIFTGATE_STORE, else store: in the
// configuration, else the layout's own (~/.driftgate/<project>, or
// .driftgate/store in the project). The environment comes first, as the
// configuration may be part of the repository (a pull request may change
// it). store: is relative to the project folder (or ~/…) and may not name a
// folder inside the git repository of the project: personal data never
// enters git, not even encrypted.
func storePath(c Config, l Layout) (string, error) {
	switch {
	case os.Getenv(EnvStore) != "":
		return expand(os.Getenv(EnvStore))
	case c.Store != "":
		dir := c.Store
		if _, home := HomeRelative(dir); !filepath.IsAbs(dir) && !home {
			dir = filepath.Join(l.Root, dir)
		}
		dir, err := expand(dir)
		if err == nil && within(repository(l.Root), dir) {
			err = invalid{fmt.Errorf("%s: store %s is inside the repository; the local store holds private bodies and must stay outside it", FileName, dir)}
		}
		return dir, err
	case l.Key != "":
		return filepath.Join(l.Dir, "store"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".driftgate", c.Project), nil
}

// Path joins elements under the folder of the project's files.
func (l Layout) Path(elem ...string) string {
	return filepath.Join(append([]string{l.Dir}, elem...)...)
}

// locate is the project folder and the mode of a configuration file: in a
// .driftgate folder it is baselines: local, anywhere else baselines: git.
func locate(file string) (root, mode string) {
	dir := filepath.Dir(file)
	if filepath.Base(dir) == LocalDir {
		return filepath.Dir(dir), BaselinesLocal
	}
	return dir, BaselinesGit
}

// Find walks up from dir to the configuration of the project dir is in:
// the first folder with driftgate.yaml (baselines: git) or
// .driftgate/driftgate.yaml (baselines: local). It stops at the top of the
// git work tree, or of the file system outside one.
func Find(dir string) (string, error) {
	start, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := start; ; {
		if path, err := configIn(d); path != "" || err != nil {
			return path, err
		}
		parent := filepath.Dir(d)
		if parent == d || exists(filepath.Join(d, ".git")) {
			if here, main := MainWorktreeProject(start); main != "" {
				return "", fmt.Errorf("%w in %s or a folder above it. This worktree has no .driftgate/ (git does not carry it); the main worktree has the project in %s: driftgate setup in %s takes its configuration, baselines and key from there, so the baseline is not made from this worktree's changed code", ErrNoConfig, start, main, here)
			}
			return "", fmt.Errorf("%w in %s or a folder above it (driftgate init writes it)", ErrNoConfig, start)
		}
		d = parent
	}
}

// configIn is the configuration of a project at root, "" when it has none.
// One in each place is an error: which of them holds the project is not
// clear.
func configIn(root string) (string, error) {
	if j, ok := readJournal(root); ok {
		return "", fmt.Errorf("a switch of %s to baselines: %s stopped before it was done; driftgate baselines %s finishes it", root, j.mode, j.mode)
	}
	git, local := filepath.Join(root, FileName), filepath.Join(root, LocalDir, FileName)
	switch g, l := exists(git), exists(local); {
	case g && l:
		return "", fmt.Errorf("%s has both %s (baselines: git) and %s (baselines: local), as after checking out a branch from before a switch of baselines. Do not delete %s/: it holds the store and the key. To keep baselines: local, take the git files out: git rm -r %s %s/ (and commit). To keep baselines: git, move the local folder out of the work tree, where git cannot take it in: mv %s %s (its store and key stay in it)",
			root, FileName, filepath.Join(LocalDir, FileName), LocalDir, FileName, Dir, LocalDir, aside(root))
	case g:
		return git, nil
	case l:
		return local, nil
	}
	return "", nil
}

// aside is where the both-configurations error suggests moving .driftgate/:
// beside the git work tree, so that no git add takes the key in.
func aside(root string) string {
	return filepath.Join(filepath.Dir(repository(root)), filepath.Base(root)+".driftgate-aside")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// ignored and gitChecked hold the folders found ignored, by the ignore
// files or by git, and by git itself: each is checked once per process.
var ignored, gitChecked sync.Map

// checkIgnored makes sure git ignores the layout's own folder before
// anything sensitive is written into it. A project outside a git work tree
// has nothing to check. The ignore files are read first, as git check-ignore
// costs a process (milliseconds) on every command that writes the store;
// asking git also finds files of the folder that git tracks (git add -f),
// which the rare writes of the key and of a switch do (byGit).
func (l Layout) checkIgnored(byGit bool) error {
	if l.Ignored == "" {
		return nil
	}
	cache := &ignored
	if byGit {
		cache = &gitChecked
	}
	if _, ok := cache.Load(l.Ignored); ok {
		return nil
	}
	// Outside a git work tree there is nothing to check (and nothing to
	// remember: the project may become one).
	top := repository(l.Root)
	if !exists(filepath.Join(top, ".git")) {
		return nil
	}
	if byGit || !ignoredByFiles(l.Root, top, filepath.Base(l.Ignored)) {
		if err := gitIgnores(l.Root, l.sensitive()); err != nil {
			return err
		}
		gitChecked.Store(l.Ignored, true)
	}
	ignored.Store(l.Ignored, true)
	return nil
}

// ignoredByFiles reports, without running git, that the ignore files
// settle that git ignores the folder name in root: the .gitignore files
// from root up to top, or the repository's info/exclude, have an entry
// that matches it there, and no negation in any of them could take it back
// (precedence between them then does not matter). false means they do not
// settle it (git check-ignore decides): no such entry, or a negation that
// names the folder or has a wildcard.
func ignoredByFiles(root, top, name string) bool {
	found := false
	for dir := root; ; dir = filepath.Dir(dir) {
		file := filepath.Join(dir, ".gitignore")
		if dir == top {
			// info/exclude: patterns relative to the top, like its .gitignore.
			if negated, matched := readIgnores(excludeFile(top), top, root, name); negated {
				return false
			} else if matched {
				found = true
			}
		}
		if negated, matched := readIgnores(file, dir, root, name); negated {
			return false
		} else if matched {
			found = true
		}
		if dir == top || dir == filepath.Dir(dir) {
			return found
		}
	}
}

// readIgnores reads the ignore file of folder dir: whether a negation
// could take name (in root) back, and whether an entry matches it.
func readIgnores(file, dir, root, name string) (negated, matched bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return false, false
	}
	// From dir, the folder is name in root: an entry without a slash
	// matches it at any depth, one with a slash only by its path from dir.
	path := filepath.ToSlash(filepath.Join(mustRel(dir, root), name))
	for _, line := range strings.Split(string(raw), "\n") {
		line = gitPattern(line)
		if rest, ok := strings.CutPrefix(line, "!"); ok {
			if strings.Contains(rest, name) || strings.ContainsAny(rest, "*?[") {
				return true, matched
			}
			continue
		}
		switch line {
		case name, name + "/", path, path + "/", "/" + path, "/" + path + "/":
			matched = true
		}
	}
	return false, matched
}

func mustRel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return r
}

// gitPattern is a line of a git ignore file as git reads it: without the
// \r of a CRLF ending and without trailing spaces unless escaped (\ ).
// Leading whitespace and trailing tabs stay: " .driftgate/" is another
// name.
func gitPattern(line string) string {
	line = strings.TrimSuffix(line, "\r")
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
		line = line[:len(line)-1]
	}
	return line
}

// excludeFile is the info/exclude of the repository whose work tree is
// top: in its .git folder, or, for a worktree whose .git is a file
// (gitdir: …), in the common folder of the repository.
func excludeFile(top string) string {
	common, _ := commonDir(top)
	if common == "" {
		return ""
	}
	return filepath.Join(common, "info", "exclude")
}

// commonDir is the git folder the worktrees of the repository share, for
// the work tree whose top is top (git rev-parse --git-common-dir, without
// a process); linked reports a worktree added with git worktree add,
// whose .git is a file.
func commonDir(top string) (common string, linked bool) {
	dir := filepath.Join(top, ".git")
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		raw, err := os.ReadFile(dir)
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
		if err != nil || !ok {
			return "", false
		}
		if dir = strings.TrimSpace(rest); !filepath.IsAbs(dir) {
			dir = filepath.Join(top, dir)
		}
		linked = true
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil {
		common := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(common) {
			common = filepath.Join(dir, common)
		}
		dir = common
	}
	return filepath.Clean(dir), linked
}

// sensitive are the paths in the layout's own folder git must ignore: the
// folder, the key, the store (when it is there) and the baselines.
func (l Layout) sensitive() []string {
	paths := []string{l.Ignored + "/", l.Key, l.Baselines + "/"}
	if within(l.Ignored, l.Store) {
		paths = append(paths, l.Store+"/")
	}
	return paths
}

// gitIgnores asks git whether it ignores each of paths (in root; a folder
// ends in /). git takes a folder with tracked files in it for one it does
// not ignore, and a file a negation (!…) takes back for one it does not.
func gitIgnores(root string, paths []string) error {
	var in bytes.Buffer
	want := map[string]bool{}
	for _, p := range paths {
		dir := strings.HasSuffix(p, "/")
		rel, err := filepath.Rel(root, strings.TrimSuffix(p, "/"))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if dir {
			rel += "/"
		}
		want[rel] = true
		in.WriteString(rel + "\n")
	}
	cmd := exec.Command("git", "check-ignore", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = root, secret.Environ(), &in
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return fmt.Errorf("checking that git ignores %s: %v %s", paths[0], err, strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		delete(want, strings.TrimSpace(line))
	}
	if len(want) == 0 {
		return nil
	}
	var missing []string
	for p := range want {
		missing = append(missing, p)
	}
	sort.Strings(missing)
	dir := filepath.ToSlash(filepath.Base(strings.TrimSuffix(paths[0], "/"))) + "/"
	return invalid{fmt.Errorf("git does not ignore %s, which hold the key, captures and private bodies: add the line %s to %s and remove lines that take parts of it back (!…); if git already tracks files in it, run git rm -r --cached %s. git check-ignore -v %s then names the line that ignores each. Run the command again after that",
		strings.Join(missing, ", "), dir, filepath.Join(root, ".gitignore"), dir, strings.Join(missing, " "))}
}

// WriteIgnores adds the layout's own folder to the project's .gitignore
// (made when missing) and .dockerignore (when there is one, or a Dockerfile
// or compose file that would copy the folder into an image). A file that
// has the entry already is left as it is; line endings are kept.
func (l Layout) WriteIgnores() error {
	if l.Ignored == "" {
		return nil
	}
	rel, err := filepath.Rel(l.Root, l.Ignored)
	if err != nil {
		return err
	}
	if err := addIgnore(filepath.Join(l.Root, ".gitignore"), filepath.ToSlash(rel)+"/", true, gitPattern); err != nil {
		return err
	}
	// A Docker build at the top of a monorepo copies the project's folder
	// too: every folder up to the top of the work tree that has a
	// .dockerignore or a Docker file gets the entry, relative to it.
	top := repository(l.Root)
	for dir := l.Root; ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(dir, l.Ignored)
		if err != nil {
			return err
		}
		if err := addIgnore(filepath.Join(dir, ".dockerignore"), filepath.ToSlash(rel)+"/", usesDocker(dir), strings.TrimSpace); err != nil {
			return err
		}
		if dir == top || dir == filepath.Dir(dir) {
			return nil
		}
	}
}

// usesDocker reports a Dockerfile or a compose file in dir.
func usesDocker(dir string) bool {
	for _, pattern := range []string{"Dockerfile*", "*.Dockerfile", "*.dockerfile", "compose.y*ml", "compose.*.y*ml", "docker-compose*.y*ml"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) > 0 {
			return true
		}
	}
	return false
}

// addIgnore appends entry to the ignore file at path, made when missing
// only if create. pattern reads a line as the file's reader does (git
// keeps leading whitespace, Docker trims it).
func addIgnore(path, entry string, create bool, pattern func(string) string) error {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && !create:
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	name := strings.TrimSuffix(entry, "/")
	for _, line := range strings.Split(string(raw), "\n") {
		switch pattern(line) {
		case name, entry, "/" + name, "/" + entry:
			return nil
		}
	}
	eol := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		eol = "\r\n"
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		raw = append(raw, eol...)
	}
	raw = append(raw, entry+eol...)
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return snapshot.WriteFile(path, raw, mode)
}

// Locate is the project folder and mode of a new project whose
// driftgate.yaml goes to path, which decides the mode; with an empty path
// the project is the working folder, laid out as mode says (default:
// local).
func Locate(path, mode string) (root, at string, err error) {
	if path == "" {
		if mode == "" {
			mode = BaselinesLocal
		}
		root, err = os.Getwd()
		return root, mode, err
	}
	file, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if root, at = locate(file); mode != "" && mode != at {
		return "", "", fmt.Errorf("%s is where baselines: %s keeps driftgate.yaml, not baselines: %s", path, at, mode)
	}
	return root, at, nil
}

// Create validates the configuration of a new project (Locate); the file is
// not written. A project that has a configuration already is refused.
func Create(c Config, path, mode string) (*Project, error) {
	root, mode, err := Locate(path, mode)
	if err != nil {
		return nil, err
	}
	if existing, err := configIn(root); err != nil || existing != "" {
		if err == nil {
			err = fmt.Errorf("%s already exists", existing)
		}
		return nil, err
	}
	if mode == BaselinesLocal {
		// A driftgate.yaml at the root without the key is git, and stays
		// readable by an earlier release.
		c.Baselines = mode
	}
	p, err := New(c, root)
	if err != nil {
		return nil, err
	}
	if path != "" {
		p.Layout.Config, _ = filepath.Abs(path)
	}
	if exists(p.Layout.Config) {
		return nil, fmt.Errorf("%s already exists", p.Layout.Config)
	}
	return p, nil
}

// Committed reports whether the project's files (configuration, catalog,
// allowlist, baselines) belong in its git repository, for the messages
// that say to commit them.
func (l Layout) Committed() bool { return l.Mode == BaselinesGit }
