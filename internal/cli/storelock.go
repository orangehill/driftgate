package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
	"github.com/orangehill/driftgate/internal/storelock"
)

// processStart is when this process started: a file being written that is
// older was left by a process that was killed.
var processStart = time.Now()

// staleMargin: a file this process writes at once may carry a time before
// processStart, as file systems take the time from a coarser clock (Linux:
// a few milliseconds; seen with a capture of a fast local API). Only a file
// older by this much is taken for a leftover; a younger one is removed by a
// later command.
const staleMargin = 2 * time.Second

var swept sync.Map // stores whose leftovers this process removed

// lockStore takes the lock of the project's local store for a command that
// writes it (capture, run, seed, accept, import, bisect): two worktrees of
// a project share the store, and two captures of one tag would move and
// remove each other's sets. A command that finds the store locked waits,
// saying so, until the other one is done or ctx ends (Ctrl-C). Once it has
// the lock it removes what writes of killed processes left in the store
// (no other process writes it while this one holds the lock). A store in
// the project's own folder is locked only once git ignores the folder.
func lockStore(ctx context.Context, p *config.Project, errOut io.Writer) (release func(), err error) {
	dir, err := p.WriteStore()
	if err != nil {
		return nil, failed(err)
	}
	release, err = storelock.Acquire(ctx, dir, func(pid int) {
		who := "another driftgate"
		if pid > 0 {
			who = fmt.Sprintf("another driftgate (pid %d)", pid)
		}
		fmt.Fprintf(errOut, "%s is using the store %s; waiting until it is done (Ctrl-C to stop)\n", who, dir)
	})
	if err != nil {
		return nil, envErr(fmt.Errorf("waiting for the store %s: %w", dir, err))
	}
	if err := p.Current(); err != nil {
		release()
		return nil, failed(err)
	}
	if _, done := swept.LoadOrStore(dir, true); !done {
		// In the background: a store of many captures takes a while to
		// read, and only files older than this process are removed. The
		// lock is given back once it is done, so a short command (accept)
		// does not end before it.
		var sweep sync.WaitGroup
		sweep.Go(func() { snapshot.RemoveStale(dir, processStart.Add(-staleMargin)) })
		unlock := release
		release = func() {
			sweep.Wait()
			unlock()
		}
	}
	return release, nil
}
