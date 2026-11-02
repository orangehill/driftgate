package files

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Summary describes a copied tree: counts and relative paths, never content.
// The paths are the original names, which the copy keeps anyway.
type Summary struct {
	Files    int            `json:"files"` // placeholders written
	Dirs     int            `json:"dirs"`
	Kinds    map[string]int `json:"kinds"`              // placeholders per kind
	Fallback []string       `json:"fallback,omitempty"` // placeholders of the right type without the original's dimensions
	Symlinks []string       `json:"symlinks,omitempty"` // not followed, not copied
	Skipped  []Skipped      `json:"skipped,omitempty"`  // not copied
	Bytes    int64          `json:"bytes"`              // of the originals that were replaced
}

// Skipped is a file or directory that has no placeholder in the copy.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"` // "too large", "unreadable", "not a regular file"
}

// CopyTree copies the tree at src to dst, which must not exist or be empty,
// with a placeholder for every regular file under the same relative path.
// Symbolic links are not followed and not copied; files over the limits and
// files or directories that cannot be read are left out. All of them are
// listed in the summary. The tree is walked in lexical order, so the summary
// is the same on every run.
func CopyTree(ctx context.Context, src, dst string, opt Options) (*Summary, error) {
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(src); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", src)
	}
	if entries, err := os.ReadDir(dst); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s: target is not empty", dst)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if d, err := resolve(dst); err != nil {
		return nil, err
	} else if within(d, src) || within(src, d) {
		return nil, fmt.Errorf("%s and %s overlap", src, dst)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	// Originals are opened through the root, so that a path the walk listed
	// cannot lead out of the tree, even when a directory on it is replaced
	// by a link while the walk runs.
	root, err := os.OpenRoot(src)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	s := &Summary{Kinds: map[string]int{}}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, _ := filepath.Rel(src, path)
		rel = filepath.ToSlash(rel)
		if err != nil {
			if path == src {
				return err
			}
			s.Skipped = append(s.Skipped, Skipped{rel, "unreadable"})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			s.Symlinks = append(s.Symlinks, rel)
			return nil
		case d.IsDir():
			if path != src {
				s.Dirs++
			}
			perm := fs.FileMode(0o755)
			if info, err := d.Info(); err == nil {
				perm = info.Mode().Perm() | 0o700
			}
			return os.MkdirAll(target, perm)
		case !d.Type().IsRegular():
			s.Skipped = append(s.Skipped, Skipped{rel, "not a regular file"})
			return nil
		}
		info, err := copyFile(ctx, root, filepath.FromSlash(rel), target, opt)
		switch {
		case err == nil:
		case errors.Is(err, ErrTooLarge):
			s.Skipped = append(s.Skipped, Skipped{rel, "too large"})
			return nil
		case errors.Is(err, errNotRegular):
			s.Skipped = append(s.Skipped, Skipped{rel, "not a regular file"})
			return nil
		case errors.Is(err, errWrite), ctx.Err() != nil:
			return err
		default:
			s.Skipped = append(s.Skipped, Skipped{rel, "unreadable"})
			return nil
		}
		s.Files++
		s.Kinds[info.Kind]++
		s.Bytes += info.Size
		if info.Fallback != "" {
			s.Fallback = append(s.Fallback, rel)
		}
		return nil
	})
	return s, err
}

// resolve makes a path absolute with its symbolic links resolved, as far as
// it exists.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs, nil
	}
	r, err := resolve(parent)
	return filepath.Join(r, filepath.Base(abs)), err
}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// errWrite marks a failure to write the copy, which stops the walk; a
// failure to read an original only leaves that file out.
var errWrite = errors.New("writing the copy")

// errNotRegular marks an entry that is no longer a regular file when it is
// opened: it was replaced after the walk listed it.
var errNotRegular = errors.New("not a regular file")

// copyFile writes the placeholder of the file at rel under root. The entry
// may have been replaced since the walk listed it, by a link or a named pipe:
// it is opened without following a link or blocking, and read only when the
// opened file is the regular file that was at rel before the open.
func copyFile(ctx context.Context, root *os.Root, rel, target string, opt Options) (Info, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return Info{}, err
	}
	if !before.Mode().IsRegular() {
		return Info{}, fmt.Errorf("%s: %w", rel, errNotRegular)
	}
	f, err := root.OpenFile(rel, os.O_RDONLY|openNonblock|openNoFollow, 0)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(before, st) {
		return Info{}, fmt.Errorf("%s: %w", rel, errNotRegular)
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm()|0o600)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", errWrite, err)
	}
	bw := &trackWriter{w: bufio.NewWriterSize(out, 256<<10)}
	info, err := Placeholder(ctx, f, st.Size(), filepath.Base(rel), bw, opt)
	if err == nil {
		err = bw.w.Flush()
		bw.err = err
	}
	if cerr := out.Close(); err == nil {
		err = cerr
		bw.err = cerr
	}
	if err != nil {
		os.Remove(target)
		if bw.err != nil {
			return info, fmt.Errorf("%w: %v", errWrite, err)
		}
		return info, err
	}
	return info, nil
}

// trackWriter remembers a write error, to tell it from a read error.
type trackWriter struct {
	w   *bufio.Writer
	err error
}

func (t *trackWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}
