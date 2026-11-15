//go:build unix

// Package e2e runs the driftgate command as a user does, as a separate
// process: end to end (e2e_test.go), killed at the points that must leave
// everything recoverable (chaos_test.go), and two at a time on one store
// (concurrency_test.go). The test binary is the driftgate binary: run with
// DG_E2E_MAIN=1 it is driftgate's main, so no build step is needed.
package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/orangehill/driftgate/internal/chaos"
	"github.com/orangehill/driftgate/internal/cli"
)

// TestMain is driftgate when DG_E2E_MAIN is set. DG_E2E_CRASH=<point>[#n]
// kills it (SIGKILL: no cleanup of its own) the n-th time (default 1) it
// reaches the chaos point; <point>[#n]:TERM sends it SIGTERM there instead
// and lets it go on, as a stopped CI job or container does; <point>[#n]:HOLD
// holds it there: it creates the file $DG_E2E_HOLD.reached and waits until
// the file $DG_E2E_HOLD exists.
func TestMain(m *testing.M) {
	if os.Getenv("DG_E2E_MAIN") == "" {
		if os.Getenv("DRIFTGATE_KEY") == "" {
			key := make([]byte, 32)
			rand.Read(key)
			os.Setenv("DRIFTGATE_KEY", base64.StdEncoding.EncodeToString(key))
		}
		os.Exit(m.Run())
	}
	if spec := os.Getenv("DG_E2E_CRASH"); spec != "" {
		chaos.At = crashAt(spec)
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}

func crashAt(spec string) func(string) {
	spec, how, _ := strings.Cut(spec, ":")
	point, nth, _ := strings.Cut(spec, "#")
	n, err := strconv.Atoi(nth)
	if err != nil {
		n = 1
	}
	var hits atomic.Int64
	return func(p string) {
		if p != point || hits.Add(1) != int64(n) {
			return
		}
		switch how {
		case "TERM":
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		case "HOLD":
			hold := os.Getenv("DG_E2E_HOLD")
			os.WriteFile(hold+".reached", nil, 0o644)
			for {
				if _, err := os.Stat(hold); err == nil {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		default:
			syscall.Kill(os.Getpid(), syscall.SIGKILL)
			// The signal arrives. Not select {}: with no other goroutine
			// the runtime reports a deadlock (exit 2) when the signal is
			// late on a loaded machine; a sleeping goroutine is not one.
			for {
				time.Sleep(time.Hour)
			}
		}
	}
}

// result is one run of driftgate.
type result struct {
	code     int // the exit code; 128 + the signal for a killed process
	out, err string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", r.code, r.out, r.err)
}

// dg runs driftgate in dir with the extra environment env.
func dg(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	return start(t, dir, env, args...).wait(t)
}

// running is driftgate started in the background; its output goes to
// files, which the test may read while it runs.
type running struct {
	cmd            *exec.Cmd
	stdout, stderr string
}

func start(t *testing.T, dir string, env []string, args ...string) *running {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "DG_E2E_MAIN=1"), env...)
	tmp := t.TempDir()
	r := &running{cmd: cmd, stdout: filepath.Join(tmp, "stdout"), stderr: filepath.Join(tmp, "stderr")}
	for path, w := range map[string]*io.Writer{r.stdout: &cmd.Stdout, r.stderr: &cmd.Stderr} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		*w = f
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return r
}

// wait waits until it ends.
func (r *running) wait(t *testing.T) result {
	t.Helper()
	err := r.cmd.Wait()
	out, _ := os.ReadFile(r.stdout)
	errOut, _ := os.ReadFile(r.stderr)
	res := result{out: string(out), err: string(errOut)}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		res.code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.code = 128 + int(ws.Signal())
		}
	default:
		t.Fatal(err)
	}
	return res
}

// await waits until cond holds, at most a minute.
func await(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(time.Minute); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("waited a minute for %s", what)
		}
	}
}

// files are the files under dir by slash path, with the SHA-256 of each.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:8])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// leftovers lists what an interrupted write leaves under the dirs: files
// and folders being written (.tmp-…) and previous captures moved aside.
func leftovers(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && (strings.HasPrefix(d.Name(), ".tmp-") || strings.Contains(d.Name(), ".old-")) {
				out = append(out, path)
			}
			return err
		})
	}
	return out
}
