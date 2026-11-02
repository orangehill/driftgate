package config

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/secret"
)

func TestWithBaselines(t *testing.T) {
	for in, want := range map[string]string{
		"version: 1\nproject: p\nbaselines: git   # where\nmode: freeze\n": "version: 1\nproject: p\nbaselines: local   # where\nmode: freeze\n",
		"# shop\nversion: 1\nproject: p\nmode: freeze\n":                   "# shop\nversion: 1\nproject: p\nbaselines: local\nmode: freeze\n",
		"version: 1\r\nproject: p\r\nmode: freeze\r\n":                     "version: 1\r\nproject: p\r\nbaselines: local\r\nmode: freeze\r\n",
		"version: 1\nproject: p":                                           "version: 1\nproject: p\nbaselines: local\n",
	} {
		got, err := withBaselines([]byte(in), BaselinesLocal)
		if err != nil || string(got) != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := withBaselines([]byte("{version: 1, project: p}"), BaselinesLocal); err == nil {
		t.Error("a flow mapping without a project: line accepted")
	}
	// git: the key goes, as a root driftgate.yaml without it is git.
	for in, want := range map[string]string{
		"version: 1\nproject: p\nbaselines: local  # c\nmode: freeze\n": "version: 1\nproject: p\nmode: freeze\n",
		"version: 1\r\nproject: p\r\nbaselines: local\r\n":              "version: 1\r\nproject: p\r\n",
		"version: 1\nproject: p\n":                                      "version: 1\nproject: p\n",
	} {
		got, err := withBaselines([]byte(in), BaselinesGit)
		if err != nil || string(got) != want {
			t.Errorf("git %q: %q %v, want %q", in, got, err, want)
		}
	}
}

// switchProject is a project of baselines: local with a baseline, a
// capture, a private body and its key file, outside git; the home folder is
// a temporary one.
func switchProject(t *testing.T) (root string, key secret.Key) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(EnvStore, "")
	t.Setenv(secret.EnvKey, "")
	root = t.TempDir()
	dg := filepath.Join(root, LocalDir)
	writeFile(t, filepath.Join(dg, FileName), "# the shop\nversion: 1\nproject: shop\nbaselines: local  # comment\n")
	writeFile(t, filepath.Join(dg, "catalog.json"), `{"version":1,"requests":[]}`)
	writeFile(t, filepath.Join(dg, "baselines", "local", "responses", "a.json"), "{}")
	writeFile(t, filepath.Join(dg, "baselines", "local", "acceptances.jsonl"), "{\"reason\":\"first\"}\n")
	writeFile(t, filepath.Join(dg, "store", "captures", "run-1", "responses", "a.json"), "{}")
	writeFile(t, filepath.Join(dg, "store", "private", "ab.enc"), "sealed")
	if err := secret.CreateFile(filepath.Join(dg, "key")); err != nil {
		t.Fatal(err)
	}
	k, err := secret.Load("shop", secret.Source{}, filepath.Join(dg, "key"))
	if err != nil {
		t.Fatal(err)
	}
	return root, k
}

func runSwitch(t *testing.T, root, mode string) error {
	t.Helper()
	t.Chdir(root)
	s, err := PrepareSwitch("", mode)
	if err != nil {
		return err
	}
	return s.Run(context.Background(), func(*Project) error { return nil }, io.Discard, io.Discard)
}

// checkGit checks the project after its switch to git: everything moved,
// nothing left in .driftgate/, the key file not in the repository.
func checkGit(t *testing.T, root string) {
	t.Helper()
	home := os.Getenv("HOME")
	for _, f := range []string{
		filepath.Join(root, "driftgate", "catalog.json"),
		filepath.Join(root, "driftgate", "baselines", "local", "responses", "a.json"),
		filepath.Join(root, "driftgate", "baselines", "local", "acceptances.jsonl"),
		filepath.Join(home, ".driftgate", "shop", "captures", "run-1", "responses", "a.json"),
		filepath.Join(home, ".driftgate", "shop", "private", "ab.enc"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, LocalDir)); err == nil {
		t.Error(".driftgate/ is still there")
	}
	if _, err := os.Stat(filepath.Join(root, "driftgate", "key")); err == nil {
		t.Error("the key file went into the repository")
	}
	raw, _ := os.ReadFile(filepath.Join(root, FileName))
	if string(raw) != "# the shop\nversion: 1\nproject: shop\n" {
		t.Errorf("driftgate.yaml: %q", raw)
	}
	p, err := Load(filepath.Join(root, FileName))
	if err != nil || p.Layout.Mode != BaselinesGit {
		t.Fatalf("Load: %v", err)
	}
}

// TestSwitchToGitNeedsTheKey: the key file never goes into the repository,
// so baselines: git must find the same key elsewhere first; otherwise
// nothing is written.
func TestSwitchToGitNeedsTheKey(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return nil, secret.ErrNoKey }
	err := runSwitch(t, root, BaselinesGit)
	if !errors.Is(err, secret.ErrNoKey) || !strings.Contains(err.Error(), "security add-generic-password -s driftgate -a shop -w") || strings.Contains(err.Error(), "driftgate key create") {
		t.Fatalf("no key for git: %v", err)
	}
	other := bytes.Repeat([]byte{7}, secret.KeySize)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return other, nil }
	if err := runSwitch(t, root, BaselinesGit); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("another key for git: %v", err)
	}
	for _, f := range []string{FileName, "driftgate", filepath.Join(LocalDir, journalName)} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("%s written", f)
		}
	}
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	checkGit(t, root)
}

// TestSwitchInterrupted: a switch stopped at any point leaves a project
// that refuses to run until driftgate baselines <mode> finishes it, and
// finishing gives what an uninterrupted switch gives, back and forth.
func TestSwitchInterrupted(t *testing.T) {
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	defer func() { chaos.At = nil }()
	for _, point := range []string{"baselines.copy", "baselines.flip", "baselines.remove"} {
		t.Run(point, func(t *testing.T) {
			root, k := switchProject(t)
			loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
			type stop struct{}
			chaos.At = func(p string) {
				if p == point {
					panic(stop{})
				}
			}
			func() {
				defer func() {
					if r := recover(); r != (stop{}) {
						t.Fatalf("no stop at %s: %v", point, r)
					}
				}()
				runSwitch(t, root, BaselinesGit)
			}()
			chaos.At = nil
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "driftgate baselines git finishes it") {
				t.Fatalf("a command during the switch: %v", err)
			}
			if err := runSwitch(t, root, BaselinesLocal); err == nil || !strings.Contains(err.Error(), "finish it first") {
				t.Fatalf("the other way during the switch: %v", err)
			}
			if err := runSwitch(t, root, BaselinesGit); err != nil {
				t.Fatalf("finishing: %v", err)
			}
			checkGit(t, root)

			// And back, stopped at the same point: the key file comes
			// from where git mode finds the key.
			t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
			chaos.At = func(p string) {
				if p == point {
					panic(stop{})
				}
			}
			func() {
				defer func() { recover() }()
				runSwitch(t, root, BaselinesLocal)
			}()
			chaos.At = nil
			if err := runSwitch(t, root, BaselinesLocal); err != nil {
				t.Fatalf("finishing the switch back: %v", err)
			}
			dg := filepath.Join(root, LocalDir)
			for _, f := range []string{"catalog.json", "baselines/local/acceptances.jsonl", "store/private/ab.enc", "store/captures/run-1/responses/a.json"} {
				if _, err := os.Stat(filepath.Join(dg, f)); err != nil {
					t.Error(err)
				}
			}
			for _, f := range []string{FileName, "driftgate", filepath.Join(LocalDir, journalName)} {
				if _, err := os.Stat(filepath.Join(root, f)); err == nil {
					t.Errorf("%s is still there", f)
				}
			}
			t.Setenv(secret.EnvKey, "")
			if got, err := secret.Load("shop", secret.Source{}, filepath.Join(dg, "key")); err != nil || !bytes.Equal(got, k) {
				t.Errorf("key file after the switch back: %v", err)
			}
			if raw, _ := os.ReadFile(filepath.Join(root, ".gitignore")); string(raw) != ".driftgate/\n" {
				t.Errorf(".gitignore: %q", raw)
			}
		})
	}
}

// TestSwitchWithEnvStore: DRIFTGATE_STORE stays where it is, so the
// project's own .driftgate/store would be left behind, or, as a file of the
// project, copied into the repository: the switch refuses and says what to
// do. Without the variable the store moves.
func TestSwitchWithEnvStore(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	t.Setenv(EnvStore, filepath.Join(t.TempDir(), "env-store"))
	err := runSwitch(t, root, BaselinesGit)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "unset DRIFTGATE_STORE") {
		t.Fatalf("switch with DRIFTGATE_STORE: %v", err)
	}
	for _, f := range []string{"driftgate/store", FileName, filepath.Join(LocalDir, journalName)} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("%s written", f)
		}
	}
	t.Setenv(EnvStore, "")
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	checkGit(t, root)
	if _, err := os.Stat(filepath.Join(root, "driftgate", "store")); err == nil {
		t.Error("the store went into the repository")
	}
}

// TestSwitchKeepsSharedStore: the store of baselines: git is shared by
// the project's checkouts; a switch adds to it and never removes or
// replaces what is there. Another set of the same name with other content
// stops the switch before anything is written.
func TestSwitchKeepsSharedStore(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	shared := filepath.Join(os.Getenv("HOME"), ".driftgate", "shop")
	writeFile(t, filepath.Join(shared, "captures", "other-checkout", "responses", "b.json"), "{}")
	writeFile(t, filepath.Join(shared, "private", "cd.enc"), "sealed by another branch's baseline")
	writeFile(t, filepath.Join(shared, "captures", "run-1", "responses", "a.json"), `{"other":true}`)
	err := runSwitch(t, root, BaselinesGit)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(shared, "captures", "run-1")) || !strings.Contains(err.Error(), "nothing was moved") {
		t.Fatalf("conflicting capture: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, LocalDir, journalName)); err == nil {
		t.Error("journal written")
	}
	// The same content is no conflict.
	writeFile(t, filepath.Join(shared, "captures", "run-1", "responses", "a.json"), "{}")
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	checkGit(t, root)
	for _, f := range []string{"captures/other-checkout/responses/b.json", "private/cd.enc"} {
		if _, err := os.Stat(filepath.Join(shared, f)); err != nil {
			t.Errorf("shared store lost %s: %v", f, err)
		}
	}
}

// TestSwitchKeyFileAgainstEnvKey: the key file is compared with the key
// git mode reads, not the key both modes would take from DRIFTGATE_KEY:
// another key there keeps the key file, the only copy of the project key.
func TestSwitchKeyFileAgainstEnvKey(t *testing.T) {
	root, _ := switchProject(t)
	other := bytes.Repeat([]byte{7}, secret.KeySize)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(other))
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return other, nil } // what DRIFTGATE_KEY gives
	if err := runSwitch(t, root, BaselinesGit); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Fatalf("switch under another DRIFTGATE_KEY: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, LocalDir, "key")); err != nil {
		t.Fatalf("key file: %v", err)
	}
}

// TestSwitchRefusesBothConfigs: after a checkout of a branch from before a
// switch both configurations are there; a switch must not copy the older
// files over the newer ones, and says how to settle it.
func TestSwitchRefusesBothConfigs(t *testing.T) {
	root, k := switchProject(t)
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	writeFile(t, filepath.Join(root, FileName), "version: 1\nproject: shop\n")
	writeFile(t, filepath.Join(root, "driftgate", "catalog.json"), `{"version":1,"requests":[]}`)
	writeFile(t, filepath.Join(os.Getenv("HOME"), ".driftgate", "shop", "private", "old.enc"), "sealed")
	t.Chdir(root)
	for _, mode := range []string{BaselinesLocal, BaselinesGit} {
		if err := runSwitch(t, root, mode); err == nil || !strings.Contains(err.Error(), "git rm -r driftgate.yaml driftgate/") {
			t.Errorf("baselines %s with both configurations: %v", mode, err)
		}
	}
	for _, f := range []string{"store/private/ab.enc", "store/captures/run-1/responses/a.json", "baselines/local/acceptances.jsonl", "key"} {
		if _, err := os.Stat(filepath.Join(root, LocalDir, f)); err != nil {
			t.Errorf("lost .driftgate/%s: %v", f, err)
		}
	}
}

// TestSwitchMovesOnlyProjectFiles: a file of the user's in .driftgate/
// (a tokens file) does not go into the repository; the switch says it
// stays.
func TestSwitchMovesOnlyProjectFiles(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	writeFile(t, filepath.Join(root, LocalDir, "tokens.json"), `{"admin":"secret"}`)
	t.Chdir(root)
	s, err := PrepareSwitch("", BaselinesGit)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := s.Run(context.Background(), func(*Project) error { return nil }, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "driftgate", "tokens.json")); err == nil {
		t.Error("tokens.json went into driftgate/")
	}
	if _, err := os.Stat(filepath.Join(root, LocalDir, "tokens.json")); err != nil {
		t.Errorf("tokens.json: %v", err)
	}
	if !strings.Contains(out.String(), filepath.Join(LocalDir, "tokens.json")+" stay where they are") {
		t.Errorf("output:\n%s", out.String())
	}
	for _, f := range []string{"driftgate/catalog.json", "driftgate/baselines/local/acceptances.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Error(err)
		}
	}
}

// TestSwitchConfigText: a quoted baselines: value is rewritten; a
// configuration the switch cannot rewrite stops it before anything is
// written, so no half-done switch blocks the project.
func TestSwitchConfigText(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	cfg := filepath.Join(root, LocalDir, FileName)
	writeFile(t, cfg, "{version: 1, project: shop, baselines: local}\n")
	if err := runSwitch(t, root, BaselinesGit); err == nil || !strings.Contains(err.Error(), "set it by hand") {
		t.Fatalf("flow mapping: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, LocalDir, journalName)); err == nil {
		t.Fatal("journal written")
	}
	if _, err := Load(cfg); err != nil {
		t.Fatalf("the project is not usable: %v", err)
	}
	writeFile(t, cfg, "version: 1\nproject: shop\nbaselines: \"local\"\n")
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(root, FileName)); string(raw) != "version: 1\nproject: shop\n" {
		t.Errorf("driftgate.yaml: %q", raw)
	}
}

// TestCurrent: a project loaded before a switch is no longer current
// after it, so a command that waited for the store lock writes nothing.
func TestCurrent(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	p, err := Load(filepath.Join(root, LocalDir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Current(); err != nil {
		t.Fatal(err)
	}
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	if err := p.Current(); err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "run the command again") {
		t.Fatalf("after the switch: %v", err)
	}
}

// TestSwitchBackAndForth: this checkout's own run capture and last
// comparison, and a pristine record a later seed replaced, are no
// conflict when the project switches back to a store that has older ones.
func TestSwitchBackAndForth(t *testing.T) {
	root, k := switchProject(t)
	defer func(f func(string, secret.Source) (secret.Key, error)) { loadGitKey = f }(loadGitKey)
	loadGitKey = func(string, secret.Source) (secret.Key, error) { return k, nil }
	t.Setenv(secret.EnvKey, base64.StdEncoding.EncodeToString(k))
	p, err := Load(filepath.Join(root, LocalDir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	id := p.CheckoutID()
	own := func(store, v string) {
		writeFile(t, filepath.Join(store, "captures", "run-"+id, "responses", "a.json"), v)
		writeFile(t, filepath.Join(store, "last-compare-"+id+".json"), v)
		writeFile(t, filepath.Join(store, "pristine", "db-1.json"), v)
	}
	local := filepath.Join(root, LocalDir, "store")
	shared := filepath.Join(os.Getenv("HOME"), ".driftgate", "shop")
	own(local, "1")
	writeFile(t, filepath.Join(shared, "last-compare-0000000000.json"), "another checkout's")
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatal(err)
	}
	if err := runSwitch(t, root, BaselinesLocal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(local, "last-compare-0000000000.json")); err == nil {
		t.Error("another checkout's last comparison came along")
	}
	own(local, "3")
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(local, "pristine", "db-1.json"), future, future)
	if err := runSwitch(t, root, BaselinesGit); err != nil {
		t.Fatalf("back to git: %v", err)
	}
	for _, f := range []string{filepath.Join("captures", "run-"+id, "responses", "a.json"), "last-compare-" + id + ".json", filepath.Join("pristine", "db-1.json")} {
		if raw, _ := os.ReadFile(filepath.Join(shared, f)); string(raw) != "3" {
			t.Errorf("%s: %q, want the newer one", f, raw)
		}
	}
}
