package bisect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/orangehill/driftgate/internal/procx"
	"github.com/orangehill/driftgate/internal/secret"
)

// Environment variables the hooks get.
const (
	EnvDir     = "DRIFTGATE_BISECT_DIR"     // the worktree, on the commit
	EnvCommit  = "DRIFTGATE_BISECT_COMMIT"  // the full hash of the commit
	EnvProject = "DRIFTGATE_BISECT_PROJECT" // the project folder (driftgate.yaml) inside the worktree
	EnvRepo    = "DRIFTGATE_BISECT_REPO"    // the user's checkout, e.g. to copy an ignored .env from
)

// Hook runs a command without a shell, from dir, with env added to the
// environment and a time limit. Its output goes to a file, not a pipe: a
// command that starts the application in the background leaves it running
// without holding the hook open. A hook that runs out of time is killed
// with everything it started (procx), so an application it was restarting
// does not go on serving the old build to the next test. The last lines are shown when it fails;
// the error does not repeat the command, which the caller names.
func Hook(ctx context.Context, dir string, argv, env []string, limit time.Duration, log io.Writer) error {
	if len(argv) == 0 {
		return nil
	}
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	out, err := os.CreateTemp("", "driftgate-hook-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	cmd := procx.Command(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(secret.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second
	err = procx.Run(cmd)
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("no result within %s", limit)
	}
	if raw, rerr := os.ReadFile(out.Name()); rerr == nil && log != nil && len(raw) > 0 {
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		fmt.Fprintln(log, strings.Join(lines, "\n"))
	}
	return err
}

// Wait repeats the ready command until it succeeds or the time is up. Its
// output is not shown: failing is what it does until the application
// answers.
func Wait(ctx context.Context, dir string, argv, env []string, limit time.Duration) error {
	if len(argv) == 0 {
		return nil
	}
	deadline := time.Now().Add(limit)
	pause := 100 * time.Millisecond
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("no success within %s", limit)
		}
		if Hook(ctx, dir, argv, env, min(left, 30*time.Second), nil) == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-time.After(min(pause, time.Until(deadline))):
		case <-ctx.Done():
			return ctx.Err()
		}
		pause = min(pause*2, time.Second)
	}
}
