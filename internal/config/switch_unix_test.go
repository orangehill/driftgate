//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/storelock"
)

// TestSwitchHoldsLockWhileRemoving: the old place is removed while the
// stores stay locked, so a command waiting for the lock never writes into
// it.
func TestSwitchHoldsLockWhileRemoving(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	stores := []string{filepath.Join(root, LocalDir, "store"), filepath.Join(os.Getenv("HOME"), ".driftgate", "shop")}
	checked := 0
	chaos.At = func(point string) {
		if point != "baselines.remove" {
			return
		}
		for _, dir := range stores {
			// flock locks belong to an open file: another open of the lock
			// file is refused while the switch holds it.
			f, err := os.Open(filepath.Join(dir, storelock.FileName))
			if err != nil {
				t.Errorf("lock file of %s: %v", dir, err)
				continue
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Errorf("%s not locked while the old place is removed: %v", dir, err)
			}
			f.Close()
		}
		checked++
	}
	defer func() { chaos.At = nil }()
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no removal")
	}
}
