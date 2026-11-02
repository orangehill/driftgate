package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/config"
)

// TestLockStoreSweep: taking the store removes what a killed process was
// writing, before the lock is given back, but not a file this process is
// writing, though its time may be a little before the process started
// (file systems keep a coarser clock).
func TestLockStoreSweep(t *testing.T) {
	store := t.TempDir()
	t.Setenv(config.EnvStore, store)
	p, err := config.New(config.Config{Version: config.Version, Project: "sweep", Mode: config.Freeze, Level: "values"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(store, "private", ".tmp-1")
	fresh := filepath.Join(store, "captures", "c1", ".tmp-2")
	for path, at := range map[string]time.Time{old: processStart.Add(-time.Minute), fresh: processStart.Add(-10 * time.Millisecond)} {
		write(t, path, "x")
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	swept.Delete(store)
	release, err := lockStore(context.Background(), p, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a killed process's file is still there: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("this process's file was removed: %v", err)
	}
}

// TestUnusableStoreExits3: a local store that cannot be made or written
// is a problem of the environment (exit 3) for every command that uses
// it, while a name that is not valid stays a configuration error (2).
func TestUnusableStoreExits3(t *testing.T) {
	p := newDemoProject(t, "")
	raw, _ := os.ReadFile(p.cfg)
	write(t, p.cfg, strings.Replace(string(raw), "mode: freeze", "mode: evolve", 1)) // accept takes drifts
	if code, out, errOut := runCLI(t, "capture", "c1", "--config", p.cfg, "--only", "news"); code != ExitClean {
		t.Fatalf("capture: exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, errOut := runCLI(t, "verify", "--config", p.cfg); code == ExitEnv {
		t.Fatalf("verify without baselines: exit %d\n%s\n%s", code, out, errOut)
	}
	file := filepath.Join(t.TempDir(), "file")
	write(t, file, "x")
	for _, c := range []struct {
		store string
		args  []string
		want  int
	}{
		{"", []string{"seed"}, ExitEnv},
		{"", []string{"capture", "c2"}, ExitEnv},
		{"", []string{"run"}, ExitEnv},
		{"", []string{"compare", "capture:c1", "--against", "capture:c1"}, ExitEnv},
		{"", []string{"compare", "c1", "--against", "c1"}, ExitEnv},
		{"", []string{"accept", "news", "--reason", "x"}, ExitEnv},
		{"", []string{"verify", "--baseline", "local"}, ExitEnv},
		{"", []string{"allowlist", "check"}, ExitEnv},
		{"ok", []string{"capture", "co:n"}, ExitConfig},
		{"ok", []string{"compare", "capture:a/b", "--against", "capture:c1"}, ExitConfig},
		{"ok", []string{"verify", "--baseline", "a/b"}, ExitConfig},
	} {
		if c.store == "" {
			t.Setenv(config.EnvStore, filepath.Join(file, "store")) // under a file: cannot be made
		} else {
			t.Setenv(config.EnvStore, filepath.Join(t.TempDir(), "store"))
		}
		code, out, errOut := runCLI(t, append(c.args, "--config", p.cfg)...)
		if code != c.want {
			t.Errorf("%v: exit %d, want %d\n%s\n%s", c.args, code, c.want, out, errOut)
		}
	}
}
