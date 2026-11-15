package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// compareWorkers is how many requests compare works on at a time (0: one
// per CPU, at most maxCompareWorkers), and compareBudget bounds the bytes
// of the bodies being compared at a time: a parsed body takes five to ten
// times its size. Bodies of up to a few hundred KB, most of an API's, are
// still compared several at a time.
var (
	compareWorkers       = 0
	compareBudget  int64 = 8 << 20
)

// maxCompareWorkers caps the default: more workers only hold more buffers.
// Measured on 622 and 494 real requests and on 600 synthetic ones with
// bodies of up to 20 MB, four are as fast as ten and use less memory.
const maxCompareWorkers = 4

// collectAfter is the size of an item after which parallel collects
// garbage at once. The collector lets the heap grow to twice what was live
// at its last cycle: after a large item that would leave room for the next
// one on top of the memory the first one no longer needs.
const collectAfter = 4 << 20

// parallel runs do for 0…n-1 on compareWorkers goroutines. Work starts in
// order, each item once the bodies already in work leave room for its size;
// an item larger than the whole budget works next to the smaller ones, one
// such item at a time. After an error no new item starts; the error
// returned is the one of the first item that failed, as when the items run
// one after another.
func parallel(n int, size func(i int) int64, do func(i int) error) error {
	workers := compareWorkers
	if workers <= 0 {
		workers = min(runtime.GOMAXPROCS(0), maxCompareWorkers)
	}
	budget := newBudget(compareBudget)
	errs := make([]error, n)
	var failed sync.Once
	stop := make(chan struct{})
	next := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range next {
				w := size(i)
				if errs[i] = guarded(do, i); errs[i] != nil {
					failed.Do(func() { close(stop) })
				}
				budget.release(w)
				if w >= collectAfter {
					runtime.GC()
				}
			}
		})
	}
feed:
	for i := 0; i < n; i++ {
		budget.acquire(size(i))
		select {
		case next <- i:
		case <-stop:
			budget.release(size(i))
			break feed
		}
	}
	close(next)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// guarded runs one item: a panic in it is its error (exit 3), so that a
// body the code cannot handle fails the command with what went wrong
// instead of ending the process from a goroutine.
func guarded(do func(i int) error, i int) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = envErr(fmt.Errorf("internal error, please report it: %v\n%s", v, debug.Stack()))
		}
	}()
	return do(i)
}

// budget is a weighted semaphore over bytes. A weight larger than the whole
// budget does not count against it: one such item at a time works next to
// the smaller ones, instead of waiting for them all and stopping all other
// work while it runs.
type budget struct {
	mu    sync.Mutex
	freed sync.Cond
	total int64
	free  int64
	large bool // an item larger than total is in work
}

func newBudget(total int64) *budget {
	b := &budget{total: total, free: total}
	b.freed.L = &b.mu
	return b
}

func (b *budget) acquire(n int64) {
	n = max(0, n)
	b.mu.Lock()
	if n > b.total {
		for b.large {
			b.freed.Wait()
		}
		b.large = true
	} else {
		for b.free < n {
			b.freed.Wait()
		}
		b.free -= n
	}
	b.mu.Unlock()
}

func (b *budget) release(n int64) {
	n = max(0, n)
	b.mu.Lock()
	if n > b.total {
		b.large = false
	} else {
		b.free += n
	}
	b.mu.Unlock()
	b.freed.Broadcast()
}
