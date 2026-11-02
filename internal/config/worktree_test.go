package config

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestAdoptFromMainWorktree: a worktree added with git worktree add has
// no .driftgate/; commands name the main worktree's project, and Adopt
// copies its configuration, files, baselines, key and the private bodies
// those name, not its captures or other private bodies.
func TestAdoptFromMainWorktree(t *testing.T) {
	t.Setenv(EnvStore, "")
	t.Setenv(secret.EnvKey, "")
	main := t.TempDir()
	gitInit(t, main)
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	root := filepath.Join(main, "api")
	dg := filepath.Join(root, LocalDir)
	writeFile(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	writeFile(t, filepath.Join(root, "app.php"), "<?php\n")
	git(main, "add", ".")
	git(main, "commit", "-q", "-m", "app")
	writeFile(t, filepath.Join(dg, FileName), "# shop\nversion: 1\nproject: shop\nbaselines: local\n")
	writeFile(t, filepath.Join(dg, "catalog.json"), `{"version":1,"requests":[]}`)
	writeFile(t, filepath.Join(dg, "baselines", "local", "acceptances.jsonl"), "{\"reason\":\"first\"}\n")
	if err := secret.CreateFile(filepath.Join(dg, "key")); err != nil {
		t.Fatal(err)
	}
	src, err := Load(filepath.Join(dg, FileName))
	if err != nil {
		t.Fatal(err)
	}
	store, err := src.Private()
	if err != nil {
		t.Fatal(err)
	}
	named, err := store.Put([]byte(`{"email":"a@b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Put([]byte(`{"email":"x@y.z"}`))
	if err != nil {
		t.Fatal(err)
	}
	rec := &snapshot.Record{Version: snapshot.Version, ID: "me", Status: 200, Body: snapshot.Body{Storage: snapshot.Private, Ref: named, Size: 17}}
	if err := snapshot.WriteJSON(filepath.Join(dg, "baselines", "local", "responses", "me.json"), rec); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dg, "store", "captures", "run-1", "responses", "me.json"), "{}")

	wt := filepath.Join(t.TempDir(), "wt")
	git(main, "worktree", "add", "-q", wt, "-b", "change")
	here := filepath.Join(wt, "api")
	sub := filepath.Join(here, "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// git names the main worktree by its long path; t.TempDir may be a
	// short (8.3) one on Windows.
	real, _ := filepath.EvalSymlinks(root)
	if _, err := Find(sub); !errors.Is(err, ErrNoConfig) || !strings.Contains(err.Error(), root) && !strings.Contains(err.Error(), real) || !strings.Contains(err.Error(), "driftgate setup in "+here) {
		t.Fatalf("not found in the worktree: %v", err)
	}
	h, m := MainWorktreeProject(sub)
	if h != here || (m != root && m != real) {
		t.Fatalf("MainWorktreeProject: %s %s", h, m)
	}
	if h, m := MainWorktreeProject(root); h != "" || m != "" {
		t.Errorf("the main worktree itself: %s %s", h, m)
	}
	p, err := Adopt(m, h)
	if err != nil {
		t.Fatal(err)
	}
	wdg := filepath.Join(here, LocalDir)
	if p.Root != here || p.Layout.Mode != BaselinesLocal || p.Project != "shop" {
		t.Fatalf("adopted: %+v", p.Layout)
	}
	for _, f := range []string{FileName, "catalog.json", "baselines/local/acceptances.jsonl", "baselines/local/responses/me.json", "store/private/" + named + ".enc"} {
		if _, err := os.Stat(filepath.Join(wdg, f)); err != nil {
			t.Error(err)
		}
	}
	for _, f := range []string{"store/private/" + other + ".enc", "store/captures"} {
		if _, err := os.Stat(filepath.Join(wdg, f)); err == nil {
			t.Errorf("%s copied", f)
		}
	}
	a, _ := os.ReadFile(filepath.Join(dg, "key"))
	b, _ := os.ReadFile(filepath.Join(wdg, "key"))
	if !bytes.Equal(a, b) {
		t.Error("key not copied")
	}
	set, err := p.Baseline("local")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := set.Private.Get(named); err != nil || string(body) != `{"email":"a@b.c"}` {
		t.Errorf("private body in the worktree: %q %v", body, err)
	}
	if _, err := Adopt(m, h); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second adopt: %v", err)
	}
	out, _ := exec.Command("git", "-C", wt, "status", "--porcelain", "-uall").Output()
	if len(out) > 0 {
		t.Errorf("git sees:\n%s", out)
	}
}
