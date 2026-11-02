// Package action tests the shell steps of the GitHub Action (driftgate.sh).
package action

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// step runs one step of the action in project and returns its output.
func step(t *testing.T, name, project, runner string, env ...string) string {
	t.Helper()
	cmd := exec.Command("bash", "driftgate.sh", name)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + runner,
		"RUNNER_TEMP=" + runner,
		"INPUT_WORKING_DIRECTORY=" + project,
		"INPUT_CONFIG=driftgate.yaml",
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil && name != "finish" {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}

func read(runner, name string) string {
	raw, _ := os.ReadFile(filepath.Join(runner, "driftgate", name))
	return strings.TrimSpace(string(raw))
}

// project makes a project with a committed baseline.
func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "driftgate", "baselines", "local", "responses"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "driftgate.yaml"), []byte("version: 1\nproject: demo\n"), 0o644)
	return dir
}

func TestPrepareKey(t *testing.T) {
	// A random key of the job's own: the baseline's private bodies are never
	// on the runner, so the project key is not needed.
	dir, runner := project(t), t.TempDir()
	step(t, "prepare", dir, runner)
	if code, env := read(runner, "exit-code"), read(runner, "env.sh"); code != "" || !strings.Contains(env, "export DRIFTGATE_KEY=") {
		t.Fatalf("exit %q, env.sh:\n%s", code, env)
	}
	// A key that is set is not replaced.
	runner = t.TempDir()
	step(t, "prepare", dir, runner, "DRIFTGATE_KEY=c2VjcmV0")
	if code, env := read(runner, "exit-code"), read(runner, "env.sh"); code != "" || strings.Contains(env, "DRIFTGATE_KEY") {
		t.Fatalf("with a key: exit %q, env.sh:\n%s", code, env)
	}
}

// fakeDriftgate installs a driftgate that answers report with the given
// JSON (and a markdown report) the way the action calls it.
func fakeDriftgate(t *testing.T, runner, json string) {
	t.Helper()
	bin := filepath.Join(runner, "driftgate-bin")
	os.MkdirAll(bin, 0o755)
	script := `#!/usr/bin/env bash
case "$*" in
*"--format json"*) cat <<'JSON'
` + json + `
JSON
;;
*"--format markdown"*) echo "### DriftGate: report from driftgate" ;;
*"--format junit"*) echo '<testsuites/>' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "driftgate"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestIncomplete: exit 3 because private requests could not be compared on
// the runner keeps the comparison report and says why, with --skip-private.
func TestIncomplete(t *testing.T) {
	for _, c := range []struct {
		name, json, result string
		report             bool
	}{
		{"incomplete", `{"private_missing":["me","login"]}`, "incomplete", true},
		{"skipped on purpose", `{"private_missing":["me"],"skip_private":true}`, "environment-error", false},
		{"no private requests", `{}`, "environment-error", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, runner := project(t), t.TempDir()
			fakeDriftgate(t, runner, c.json)
			step(t, "prepare", dir, runner)
			os.WriteFile(filepath.Join(runner, "driftgate", "exit-code"), []byte("3\n"), 0o644)
			out := step(t, "report", dir, runner)
			if !strings.Contains(out, "output result="+c.result) {
				t.Fatalf("report output:\n%s", out)
			}
			md := read(runner, "report.md")
			if c.report != strings.Contains(md, "report from driftgate") {
				t.Fatalf("report.md:\n%s", md)
			}
			fin := step(t, "finish", dir, runner)
			if c.report && (!strings.Contains(fin, "incomplete: 2 private requests could not be compared on the runner") || !strings.Contains(fin, "--skip-private")) {
				t.Fatalf("finish:\n%s", fin)
			}
			if !c.report && !strings.Contains(fin, "environment error") {
				t.Fatalf("finish:\n%s", fin)
			}
		})
	}
}

// TestKeyInPullRequest: the commands of a pull request run with the job's
// environment, so a project key there gets a warning.
func TestKeyInPullRequest(t *testing.T) {
	dir, runner := project(t), t.TempDir()
	out := step(t, "prepare", dir, runner, "DRIFTGATE_KEY=c2VjcmV0", "GITHUB_EVENT_NAME=pull_request")
	if !strings.Contains(out, "::warning title=DriftGate::DRIFTGATE_KEY is set in a pull_request job") {
		t.Fatalf("no warning:\n%s", out)
	}
	if out := step(t, "prepare", dir, t.TempDir(), "GITHUB_EVENT_NAME=pull_request"); strings.Contains(out, "::warning") {
		t.Fatalf("warning without a key:\n%s", out)
	}
	if out := step(t, "prepare", dir, t.TempDir(), "GITHUB_EVENT_NAME=pull_request_target"); !strings.Contains(out, "use pull_request") {
		t.Fatalf("pull_request_target:\n%s", out)
	}
}

// TestPRNumber: the pull request number goes into API paths; anything but
// digits is refused before gh is called.
func TestPRNumber(t *testing.T) {
	dir, runner := project(t), t.TempDir()
	bin := t.TempDir()
	called := filepath.Join(runner, "gh-called")
	os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/usr/bin/env bash\ntouch "+called+"\n"), 0o755)
	os.MkdirAll(filepath.Join(runner, "driftgate"), 0o755)
	os.WriteFile(filepath.Join(runner, "driftgate", "report.md"), []byte("report\n"), 0o644)
	for _, pr := range []string{"1/../../../repos/x/y/pulls", "1 --method DELETE", "-1"} {
		out := step(t, "comment", dir, runner, "PATH="+bin+":"+os.Getenv("PATH"), "INPUT_PR_NUMBER="+pr, "GITHUB_REPOSITORY=o/r")
		if !strings.Contains(out, "pr-number must be a number") {
			t.Errorf("%q:\n%s", pr, out)
		}
		if _, err := os.Stat(called); err == nil {
			t.Fatalf("%q: gh was called", pr)
		}
	}
}

// TestRunOutputIsNoWorkflowCommand: the output of driftgate run (text from
// responses, the project's commands) is wrapped in stop-commands.
func TestRunOutputIsNoWorkflowCommand(t *testing.T) {
	dir, runner := project(t), t.TempDir()
	bin := filepath.Join(runner, "driftgate-bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "driftgate"), []byte("#!/usr/bin/env bash\necho '::warning::from a response'\n"), 0o755)
	step(t, "prepare", dir, runner)
	out := step(t, "run", dir, runner)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "::stop-commands::") || lines[2] != "::"+strings.TrimPrefix(lines[0], "::stop-commands::")+"::" {
		t.Fatalf("run output:\n%s", out)
	}
}

// release makes the files of a release (a stub binary, SHA256SUMS and a
// bundle) and stubs for gh and cosign; gh serves the files of the release,
// cosign records its arguments and exits with $COSIGN_EXIT.
type release struct {
	dir, stubs, runner string
}

const releaseBinary = "#!/usr/bin/env bash\necho driftgate v1.2.3\n"

func newRelease(t *testing.T, sums string, cosign bool) *release {
	t.Helper()
	r := &release{dir: t.TempDir(), stubs: t.TempDir(), runner: t.TempDir()}
	os.WriteFile(filepath.Join(r.dir, "driftgate-linux-amd64"), []byte(releaseBinary), 0o755)
	os.WriteFile(filepath.Join(r.dir, "SHA256SUMS"), []byte(sums), 0o644)
	os.WriteFile(filepath.Join(r.dir, "SHA256SUMS.sigstore.json"), []byte("{}\n"), 0o644)
	gh := `#!/usr/bin/env bash
echo "$*" >>"` + r.runner + `/gh-calls"
case "$1 $2" in
"release view")
	tag=v1.2.3
	[ "$3" != --repo ] && tag="$3"
	echo "$tag https://github.com/Owner/Repo/releases/tag/$tag"
	;;
"release download")
	dir=""
	while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir="$2"; shift; done
	cp "` + r.dir + `"/* "$dir"/
	;;
*) exit 1 ;;
esac
`
	os.WriteFile(filepath.Join(r.stubs, "gh"), []byte(gh), 0o755)
	if cosign {
		os.WriteFile(filepath.Join(r.stubs, "cosign"), []byte("#!/usr/bin/env bash\necho \"$*\" >\""+r.runner+"/cosign-args\"\nexit ${COSIGN_EXIT:-0}\n"), 0o755)
	}
	return r
}

// install runs the install step; only the stubs and the system tools are on
// the PATH, so a cosign of the machine is not used.
func (r *release) install(t *testing.T, version string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "driftgate.sh", "install")
	cmd.Env = append([]string{
		"PATH=" + r.stubs + ":/usr/bin:/bin",
		"HOME=" + r.runner,
		"RUNNER_TEMP=" + r.runner,
		"RUNNER_OS=Linux",
		"RUNNER_ARCH=X64",
		"INPUT_VERSION=" + version,
		"ACTION_PATH=" + r.runner,
		"ACTION_REPOSITORY=owner/repo",
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *release) installed() bool {
	_, err := os.Stat(filepath.Join(r.runner, "driftgate-bin", "driftgate"))
	return err == nil
}

func sumOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// TestReleaseInstall: a release binary is installed only when its checksum
// is in SHA256SUMS and, with cosign, the signature names the release
// workflow of the repository at that tag.
func TestReleaseInstall(t *testing.T) {
	good := sumOf(releaseBinary) + "  driftgate-linux-amd64\n" + sumOf("other") + "  driftgate-darwin-arm64\n"
	tampered := sumOf(releaseBinary+"tampered") + "  driftgate-linux-amd64\n"
	identity := "--certificate-identity https://github.com/Owner/Repo/.github/workflows/release.yml@refs/tags/v1.2.3"
	for _, c := range []struct {
		name, version, sums string
		cosign              bool
		env                 []string
		ok                  bool
		want                string
	}{
		{"checksum without cosign", "v1.2.3", good, false, nil, true, "its signature is not"},
		{"checksum and signature", "v1.2.3", good, true, nil, true, "is signed by https://github.com/Owner/Repo/.github/workflows/release.yml@refs/tags/v1.2.3"},
		{"latest", "latest", good, true, nil, true, "driftgate-linux-amd64 " + sumOf(releaseBinary) + " matches SHA256SUMS"},
		{"tampered binary", "v1.2.3", tampered, false, nil, false, "checksum mismatch for driftgate-linux-amd64"},
		{"tampered with cosign", "v1.2.3", tampered, true, nil, false, "checksum mismatch for driftgate-linux-amd64"},
		{"not in SHA256SUMS", "v1.2.3", sumOf("other") + "  driftgate-darwin-arm64\n", false, nil, false, "no single checksum for driftgate-linux-amd64"},
		{"listed twice", "v1.2.3", good + good, false, nil, false, "no single checksum"},
		{"bad signature", "v1.2.3", good, true, []string{"COSIGN_EXIT=1"}, false, "does not verify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRelease(t, c.sums, c.cosign)
			out, err := r.install(t, c.version, c.env...)
			if (err == nil) != c.ok || !strings.Contains(out, c.want) {
				t.Fatalf("err %v, want ok=%v and %q:\n%s", err, c.ok, c.want, out)
			}
			if r.installed() != c.ok {
				t.Fatalf("installed %v:\n%s", r.installed(), out)
			}
			code := read(r.runner, "exit-code")
			if c.ok && code != "" || !c.ok && code != "3" {
				t.Fatalf("exit-code %q:\n%s", code, out)
			}
			if c.ok && !strings.Contains(out, "driftgate v1.2.3") {
				t.Fatalf("version not shown:\n%s", out)
			}
			if c.cosign && c.ok {
				args, _ := os.ReadFile(filepath.Join(r.runner, "cosign-args"))
				if !strings.Contains(string(args), identity) || !strings.Contains(string(args), "--certificate-oidc-issuer https://token.actions.githubusercontent.com") {
					t.Fatalf("cosign %s", args)
				}
			}
		})
	}
}

// TestReleaseInstallBadVersion: a version that is not a tag or latest is
// refused before gh sees it, so it cannot pass gh an option.
func TestReleaseInstallBadVersion(t *testing.T) {
	for _, v := range []string{"v1.2.3;rm", "--web", "--repo=evil/repo", "1.2.3", "v1.2"} {
		r := newRelease(t, sumOf(releaseBinary)+"  driftgate-linux-amd64\n", false)
		out, err := r.install(t, v)
		if err == nil || r.installed() || !strings.Contains(out, "version must be source, latest or a release tag") || read(r.runner, "exit-code") != "2" {
			t.Fatalf("%s: err %v:\n%s", v, err, out)
		}
		if _, err := os.Stat(filepath.Join(r.runner, "gh-calls")); err == nil {
			t.Fatalf("%s: gh was called", v)
		}
	}
}

// TestReleaseInstallNoBundle: with cosign, a release without the Sigstore
// bundle is refused rather than installed on its checksum alone.
func TestReleaseInstallNoBundle(t *testing.T) {
	r := newRelease(t, sumOf(releaseBinary)+"  driftgate-linux-amd64\n", true)
	os.Remove(filepath.Join(r.dir, "SHA256SUMS.sigstore.json"))
	out, err := r.install(t, "v1.2.3")
	if err == nil || r.installed() || !strings.Contains(out, "has no SHA256SUMS.sigstore.json") {
		t.Fatalf("err %v:\n%s", err, out)
	}
}
