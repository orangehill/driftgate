// Package storelock keeps two DriftGate processes from writing one local
// store at the same time: two worktrees of a project share the store, and a
// capture in one would otherwise move aside, replace or remove the other's
// capture of the same tag. The lock is advisory, a lock on <store>/.lock
// that the system releases when the process ends, also when it is killed;
// the file itself stays and holds the process ID of the last holder.
package storelock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileName is the lock file in the store.
const FileName = ".lock"

// errBusy means another process holds the lock.
var errBusy = errors.New("store locked")

// poll is how often a waiting process tries again.
var poll = 100 * time.Millisecond

// held are the locks of this process, by store, with the number of holders:
// a command that captures in-process (run, bisect) takes the lock once more.
var (
	mu   sync.Mutex
	held = map[string]*lock{}
)

type lock struct {
	f *os.File
	n int
}

// Acquire takes the lock of the store dir, waiting while another process
// holds it; busy, when set, is called once before the wait with the other
// process's ID (0 when unknown). The wait ends with ctx. release gives the
// lock back.
func Acquire(ctx context.Context, dir string, busy func(pid int)) (release func(), err error) {
	path := filepath.Join(dir, FileName)
	mu.Lock()
	defer mu.Unlock()
	if l, ok := held[path]; ok {
		l.n++
		return func() { unlock(path) }, nil
	}
	f, err := take(path)
	if errors.Is(err, errBusy) && busy != nil {
		busy(Holder(dir))
	}
	for errors.Is(err, errBusy) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
		f, err = take(path)
	}
	if err != nil {
		return nil, err
	}
	// The holder's ID, for the message of a process that waits.
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	held[path] = &lock{f: f, n: 1}
	return func() { unlock(path) }, nil
}

func unlock(path string) {
	mu.Lock()
	defer mu.Unlock()
	l, ok := held[path]
	if !ok {
		return
	}
	if l.n--; l.n == 0 {
		l.f.Close() // closing the file releases the lock
		delete(held, path)
	}
}

// Holder is the process ID written in the lock file of dir: the process that
// holds the lock, or held it last; 0 when unknown.
func Holder(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}
