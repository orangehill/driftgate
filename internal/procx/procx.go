// Package procx starts the project's commands (restore, prepare, guards,
// command steps, token commands, bisect hooks) so that a time limit or an
// interrupt stops all of what they started, not only the command itself:
// a command runs in a process group of its own, and the whole group is
// killed when its context ends. Without that, a child that keeps the
// command's output open (the server `make restart` starts, a shell's job)
// keeps the wait going past any time limit, and outlives it.
package procx

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"
)

// WaitDelay is how long a command's output is still read after it ended or
// its context did: a child it left behind may hold the output open.
var WaitDelay = 5 * time.Second

// Command is exec.CommandContext for a command of the project, in a process
// group of its own that is killed when ctx ends. Run it with Run or
// Output.
func Command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, arg...)
	group(cmd)
	cmd.WaitDelay = WaitDelay
	return cmd
}

// Run runs a command made by Command. A command that ended well but left a
// process holding its output open (a server started in the background)
// ended well: its output after that is not read.
func Run(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	track(cmd, true)
	err := cmd.Wait()
	track(cmd, false)
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}

// Output is Run that returns the standard output, as exec.Cmd.Output does.
func Output(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("procx: Stdout already set")
	}
	var out limited
	cmd.Stdout = &out
	err := Run(cmd)
	return out.b, err
}

// limited collects output; it is a type of its own so Output does not hand
// exec a *bytes.Buffer it could read while the command writes.
type limited struct {
	mu sync.Mutex
	b  []byte
}

func (l *limited) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	return len(p), nil
}

var (
	mu      sync.Mutex
	running = map[*exec.Cmd]bool{}
)

func track(cmd *exec.Cmd, on bool) {
	mu.Lock()
	defer mu.Unlock()
	if on {
		running[cmd] = true
	} else {
		delete(running, cmd)
	}
}

// KillAll kills every command still running, with what it started: for a
// process that is about to end at once (a second Ctrl-C), whose commands
// would otherwise run on, outside the terminal's reach.
func KillAll() {
	mu.Lock()
	defer mu.Unlock()
	for cmd := range running {
		if cmd.Cancel != nil {
			cmd.Cancel()
		}
	}
}
