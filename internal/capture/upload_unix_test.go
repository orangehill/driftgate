//go:build unix

package capture

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestOpenUpload: a file of the uploads folder is opened, also through a
// relative link that stays inside it; a link out of the folder, an absolute
// link, a missing file and a named
// pipe (which must not block) are refused.
func TestOpenUpload(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write := func(path, s string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "orders", "a.csv"), "a")
	write(filepath.Join(outside, "secret.txt"), "secret")
	os.Symlink(filepath.Join("orders", "a.csv"), filepath.Join(dir, "inside.csv"))
	os.Symlink(filepath.Join(dir, "orders", "a.csv"), filepath.Join(dir, "absolute.csv")) // os.Root cannot tell where it leads
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "out.csv"))
	os.Symlink(outside, filepath.Join(dir, "outdir"))
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.csv"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"orders/a.csv", "inside.csv"} {
		f, size, err := openUpload(dir, rel)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		if string(data) != "a" || size != 1 {
			t.Errorf("%s: %q, size %d", rel, data, size)
		}
	}
	for rel, want := range map[string]string{
		"out.csv":           "leads out of the uploads folder",
		"outdir/secret.txt": "leads out of the uploads folder",
		"../x":              "leads out of the uploads folder",
		"absolute.csv":      "leads out of the uploads folder",
		"missing.csv":       "not on this machine",
		"pipe.csv":          "not a regular file",
	} {
		done := make(chan error, 1)
		go func() {
			f, _, err := openUpload(dir, rel)
			if err == nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v, want %q", rel, err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the read blocks", rel)
		}
	}
}
