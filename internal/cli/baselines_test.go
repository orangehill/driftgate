package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/secret"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// TestBaselinesSwitch: driftgate baselines moves a project between local
// and git and back; the baseline, its acceptance log and the private bodies
// go along, so the next run compares as before.
func TestBaselinesSwitch(t *testing.T) {
	p := localDemoProject(t)
	root := p.cfg
	write(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	t.Setenv(secret.EnvKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Chdir(root)
	cli := func(want int, args ...string) string {
		t.Helper()
		code, out, errOut := runCLI(t, args...)
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s\n%s", args, code, want, out, errOut)
		}
		return out
	}
	cfg := filepath.Join(root, ".driftgate", "driftgate.yaml")
	raw, _ := os.ReadFile(cfg)
	write(t, cfg, strings.Replace(string(raw), "mode: freeze", "mode: evolve", 1))
	cli(ExitClean, "run")
	p.api.SetVersion("v2")
	cli(ExitDrift, "run")
	cli(ExitClean, "accept", "*", "--reason", "v2")
	verified := cli(ExitClean, "verify")
	if !strings.Contains(verified, "OK     baseline local") || !strings.Contains(verified, " 1 acceptances") {
		t.Fatalf("verify: %s", verified)
	}
	home := os.Getenv("HOME")
	var err error

	out := cli(ExitClean, "baselines", "git")
	if !strings.Contains(out, "commit driftgate.yaml and driftgate/") {
		t.Errorf("to git: %s", out)
	}
	raw, err = os.ReadFile(filepath.Join(root, "driftgate.yaml"))
	if err != nil || strings.Contains(string(raw), "baselines:") || !strings.Contains(string(raw), "project: demo") {
		t.Fatalf("driftgate.yaml: %v\n%s", err, raw)
	}
	if _, err := os.Stat(filepath.Join(root, ".driftgate")); err == nil {
		t.Error(".driftgate/ is still there")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".driftgate", "demo", "private")); len(entries) == 0 {
		t.Error("the private bodies did not move to the store of baselines: git")
	}
	if got := cli(ExitClean, "verify"); got != verified {
		t.Errorf("verify after the switch to git:\n%s\nbefore:\n%s", got, verified)
	}
	if out := cli(ExitClean, "run"); !strings.Contains(out, "No drift") {
		t.Errorf("run with baselines: git:\n%s", out)
	}
	if out := cli(ExitClean, "baselines", "git"); !strings.Contains(out, "already") {
		t.Errorf("second switch: %s", out)
	}

	out = cli(ExitClean, "baselines", "local")
	if !strings.Contains(out, "git sees driftgate.yaml and driftgate/ removed") || !strings.Contains(out, "rm -rf "+filepath.Join(home, ".driftgate", "demo")) {
		t.Errorf("to local: %s", out)
	}
	for _, f := range []string{"driftgate.yaml", "driftgate"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("%s is still there", f)
		}
	}
	fi, err := os.Stat(filepath.Join(root, ".driftgate", "key"))
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("key file: %v %v", fi, err)
	}
	if got := cli(ExitClean, "verify"); got != verified {
		t.Errorf("verify after the switch back:\n%s\nbefore:\n%s", got, verified)
	}
	// The key comes from the key file now.
	t.Setenv(secret.EnvKey, "")
	if out := cli(ExitClean, "run"); !strings.Contains(out, "No drift") {
		t.Errorf("run with baselines: local again:\n%s", out)
	}
}

// TestBaselinesSwitchRefusesLeak: what would enter git passes verify's
// scan first; a finding stops the switch before anything is written.
func TestBaselinesSwitchRefusesLeak(t *testing.T) {
	root := localDemo(t)
	write(t, filepath.Join(root, ".gitignore"), ".driftgate/\n")
	t.Setenv(secret.EnvKey, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	t.Chdir(root)
	if code, out, errOut := runCLI(t, "run"); code != ExitClean {
		t.Fatalf("run: %d\n%s\n%s", code, out, errOut)
	}
	// A baseline of an older DriftGate with a token in a header, consistent
	// with its manifest.
	base := filepath.Join(root, ".driftgate", "baselines", "local")
	set := &snapshot.Set{Dir: base}
	rec, err := set.Record("news")
	if err != nil || rec == nil {
		t.Fatalf("record: %v", err)
	}
	rec.Headers = append(rec.Headers, [2]string{"authorization", "Bearer s3cr3t-t0ken-value"})
	if err := snapshot.WriteJSON(filepath.Join(base, "responses", "news.json"), rec); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(base); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "baselines", "git")
	if code != ExitConfig || !strings.Contains(out, "would publish in git") || !strings.Contains(errOut, "nothing was moved") {
		t.Fatalf("switch with a leak: %d\n%s\n%s", code, out, errOut)
	}
	for _, f := range []string{"driftgate.yaml", "driftgate", ".driftgate/switching"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			t.Errorf("%s written", f)
		}
	}
	if code, _, errOut := runCLI(t, "verify"); code != ExitDrift {
		t.Errorf("the project is not as it was: %d %s", code, errOut)
	}
}
