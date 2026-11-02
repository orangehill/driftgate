package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/orangehill/driftgate/internal/procx"
)

// stopSignals are the signals that stop a command: Ctrl-C, and SIGTERM from
// a CI job that is cancelled or a container that is stopped.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// exit ends the process on a second signal; a test replaces it.
var exit = os.Exit

// signalContext is the context of a long-running command: the first signal
// cancels it, so the command's deferred cleanup runs (a capture moved aside
// is put back, a bisect worktree is removed, bisect.finish runs); a second
// one ends the process at once, with the shell's code for the signal. stop
// releases the signals and returns once the note about a signal is written,
// so it never interleaves with the command's own last lines.
func signalContext(errOut io.Writer) (ctx context.Context, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	done, finished := make(chan struct{}), make(chan struct{})
	signal.Notify(sigs, stopSignals...)
	go func() {
		defer close(finished)
		var sig os.Signal
		select {
		case sig = <-sigs:
		case <-done:
			return
		}
		cancel()
		if errOut != nil {
			fmt.Fprintf(errOut, "\ndriftgate: %v: stopping and cleaning up (again to stop at once)\n", sig)
		}
		select {
		case sig = <-sigs:
			procx.KillAll() // in groups of their own, the terminal's signal does not reach them
			exit(signalCode(sig))
		case <-done:
		}
	}()
	return ctx, func() {
		signal.Stop(sigs)
		close(done)
		cancel()
		<-finished
	}
}

// signalCode is the exit code a shell gives a process the signal ended:
// 128 + the signal's number (130 for Ctrl-C, 143 for SIGTERM).
func signalCode(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 130
}
