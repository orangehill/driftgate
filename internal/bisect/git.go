package bisect

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/orangehill/driftgate/internal/secret"
)

// Commit is one commit of the searched path.
type Commit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// Repo is the user's git repository. Nothing here changes its working tree,
// index or branch: commits are checked out in a separate worktree.
type Repo struct {
	Dir string // top level of the user's checkout
}

// git runs a git command in the repository and returns its trimmed output.
func (r Repo) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = secret.Environ()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Open finds the repository that holds dir.
func Open(ctx context.Context, dir string) (Repo, error) {
	top, err := Repo{Dir: dir}.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return Repo{}, fmt.Errorf("%s is not in a git repository: %w", dir, err)
	}
	return Repo{Dir: top}, nil
}

// Resolve turns a revision (a branch, tag, hash, HEAD~3) into a commit hash.
func (r Repo) Resolve(ctx context.Context, rev string) (string, error) {
	if strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("%q is not a revision", rev)
	}
	h, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil || h == "" {
		return "", fmt.Errorf("%q is not a commit in %s", rev, r.Dir)
	}
	return h, nil
}

const logFormat = "--format=%H%x1f%h%x1f%s%x1f%an%x1f%cs"

func parseLog(out string) []Commit {
	var cs []Commit
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) == 5 {
			cs = append(cs, Commit{Hash: f[0], Short: f[1], Subject: f[2], Author: f[3], Date: f[4]})
		}
	}
	return cs
}

// Path lists the commits from good to bad along the first parents: good
// first, bad last. A merge is one step; the search names the merge, and a
// second bisect over the merged branch can go further.
func (r Repo) Path(ctx context.Context, good, bad string) ([]Commit, error) {
	if good == bad {
		return nil, fmt.Errorf("good and bad are the same commit")
	}
	if _, err := r.git(ctx, "merge-base", "--is-ancestor", good, bad); err != nil {
		return nil, fmt.Errorf("the good commit %.12s is not an ancestor of the bad commit %.12s", good, bad)
	}
	first, err := r.git(ctx, "log", "-1", logFormat, good)
	if err != nil {
		return nil, err
	}
	rest, err := r.git(ctx, "log", "--first-parent", "--reverse", logFormat, bad, "^"+good)
	if err != nil {
		return nil, err
	}
	path := append(parseLog(first), parseLog(rest)...)
	if len(path) < 2 || path[len(path)-1].Hash != bad {
		return nil, fmt.Errorf("no path of commits from %.12s to %.12s", good, bad)
	}
	return path, nil
}

// Worktree is a detached checkout of the repository in a temporary folder.
type Worktree struct {
	Dir  string
	repo Repo
	tmp  string
}

// AddWorktree checks out commit in a new worktree under the system's
// temporary folder.
func (r Repo) AddWorktree(ctx context.Context, commit string) (*Worktree, error) {
	tmp, err := os.MkdirTemp("", "driftgate-bisect-")
	if err != nil {
		return nil, err
	}
	// The real path: git records it, and the hook gets it.
	if real, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = real
	}
	w := &Worktree{Dir: filepath.Join(tmp, filepath.Base(r.Dir)), repo: r, tmp: tmp}
	if _, err := r.git(ctx, "worktree", "add", "--detach", w.Dir, commit); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return w, nil
}

// Checkout puts the worktree on a commit. Files the previous commit's build
// left that git does not ignore are removed; ignored ones (dependencies,
// build caches) stay, so a build does not start from nothing every time.
func (w *Worktree) Checkout(ctx context.Context, commit string) error {
	wt := Repo{Dir: w.Dir}
	if _, err := wt.git(ctx, "checkout", "--quiet", "--force", "--detach", commit); err != nil {
		return err
	}
	_, err := wt.git(ctx, "clean", "--quiet", "-fd")
	return err
}

// Remove deletes the worktree and its record in the repository. It works
// with a context of its own, so it also runs after an interrupt.
func (w *Worktree) Remove() error {
	ctx := context.Background()
	_, err := w.repo.git(ctx, "worktree", "remove", "--force", w.Dir)
	if rmErr := os.RemoveAll(w.tmp); err == nil {
		err = rmErr
	}
	if _, pruneErr := w.repo.git(ctx, "worktree", "prune"); err == nil {
		err = pruneErr
	}
	return err
}
