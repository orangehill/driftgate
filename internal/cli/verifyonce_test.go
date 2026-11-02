package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/baseline"
	"github.com/orangehill/driftgate/internal/config"
)

// TestCheckBaselineOncePerCommand: a command reads a baseline's files once;
// a file written since is read again, also within the command, and the
// next command reads everything again.
func TestCheckBaselineOncePerCommand(t *testing.T) {
	p := newDemoProject(t, "")
	if code, out, errOut := runCLI(t, "run", "--config", p.cfg); code != ExitClean {
		t.Fatalf("first run: %d\n%s\n%s", code, out, errOut)
	}
	proj, err := config.Load(p.cfg)
	if err != nil {
		t.Fatal(err)
	}
	base, err := proj.Baseline("local")
	if err != nil {
		t.Fatal(err)
	}
	forgetVerified()
	if err := checkBaseline(proj, base, "local"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base.Dir, "responses", "news.json")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	restore := func(content []byte) {
		t.Helper()
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}

	// Written during the command: read again.
	write(t, path, strings.Replace(string(orig), "news", "nevs", 1)+" ")
	if err := checkBaseline(proj, base, "local"); err == nil || !strings.Contains(err.Error(), "changed outside driftgate accept") {
		t.Fatalf("a file written during the command: %v", err)
	}
	restore(orig)
	if err := checkBaseline(proj, base, "local"); err != nil {
		t.Fatal(err)
	}

	// Within the command a verified baseline is not hashed again: an edit
	// that keeps the size and time of the file goes unseen here…
	edited := []byte(strings.Replace(string(orig), "news", "nevs", 1))
	restore(edited)
	if err := checkBaseline(proj, base, "local"); err != nil {
		t.Fatalf("the second check within a command read the files again: %v", err)
	}
	// …but the next command reads everything again.
	if code, _, errOut := runCLI(t, "compare", runTagFor(t, p.cfg), "--config", p.cfg); code != ExitEnv || !strings.Contains(errOut, "changed outside driftgate accept") {
		t.Fatalf("the next command: %d %s", code, errOut)
	}
}

// TestCopyTreeManifest: the manifest copyTree writes from the hashes taken
// while copying is the one computed from the copy, and the copy is whole.
func TestCopyTreeManifest(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "meta.json"), `{"version":1}`)
	write(t, filepath.Join(src, "timings.json"), `{"a":1}`)
	write(t, filepath.Join(src, ".DS_Store"), "finder")
	for _, name := range []string{"a", "b", "c"} {
		write(t, filepath.Join(src, "responses", name+".json"), `{"version":1,"id":"`+name+`"}`)
		write(t, filepath.Join(src, "bodies", sum(name)), strings.Repeat(name, 1<<16))
	}
	dst := filepath.Join(t.TempDir(), "baselines", "local")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, baseline.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := baseline.Write(dst); err != nil {
		t.Fatal(err)
	}
	if want, _ := os.ReadFile(filepath.Join(dst, baseline.ManifestFile)); string(got) != string(want) {
		t.Fatalf("manifest\n%s\nwant\n%s", got, want)
	}
	if res, err := baseline.Verify(dst); err != nil || !res.OK() || res.Files != 8 {
		t.Fatalf("verify: %+v %v", res, err)
	}
	for _, rel := range []string{".DS_Store", "bodies/" + sum("b")} {
		a, _ := os.ReadFile(filepath.Join(src, rel))
		b, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || string(a) != string(b) {
			t.Fatalf("%s not copied: %v", rel, err)
		}
	}
}
