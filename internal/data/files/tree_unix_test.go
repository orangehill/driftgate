//go:build unix

package files

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// inTime fails the test when f does not return in time: opening a named
// pipe without a writer blocks.
func inTime(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked")
	}
}

func TestPlaceholderFileNamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "a.txt")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	inTime(t, func() {
		if _, err := PlaceholderFile(context.Background(), fifo, io.Discard, Options{}); err == nil {
			t.Error("no error for a named pipe")
		}
	})
}

// An entry the walk listed as a regular file may be something else when it
// is opened: a named pipe, or a link to a file outside or inside the tree.
// None of them is read.
func TestCopyFileReplacedEntry(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeTree(t, src, map[string][]byte{"other.txt": []byte(marker)})
	if err := os.WriteFile(outside, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	replace := map[string]func(path string) error{
		"pipe":         func(p string) error { return syscall.Mkfifo(p, 0o644) },
		"link outside": func(p string) error { return os.Symlink(outside, p) },
		"link inside":  func(p string) error { return os.Symlink("other.txt", p) },
	}
	for name, put := range replace {
		rel := name + ".txt"
		if err := put(filepath.Join(src, rel)); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dst, rel)
		inTime(t, func() {
			if _, err := copyFile(context.Background(), root, rel, target, Options{}); !errors.Is(err, errNotRegular) {
				t.Errorf("%s: err %v", name, err)
			}
		})
		if _, err := os.Lstat(target); err == nil {
			t.Errorf("%s: copied", name)
		}
	}
}
