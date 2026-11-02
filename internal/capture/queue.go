package capture

import (
	"runtime"
	"sync"

	"github.com/orangehill/driftgate/internal/catalog"
	"github.com/orangehill/driftgate/internal/httpx"
)

// queueBudget bounds the bytes of the response bodies that wait to be
// written, over all services of a capture: room for a few bodies keeps the
// workers sending, more only holds memory.
var queueBudget int64 = 16 << 20

// collectAfter is the size of a body after whose write the capture collects
// garbage at once. The collector lets the heap grow to twice what was live
// at its last cycle: after a large body that would leave room for the next
// one on top of the memory the first one no longer needs.
const collectAfter = 4 << 20

// collect collects garbage once a body of the given size is written and
// dropped, unless it was read off the heap: Release gave its memory back
// already, and the scan and the write of such a body leave little garbage.
func collect(res httpx.Result, size int64) {
	if size >= collectAfter && !res.OffHeap() {
		runtime.GC()
	}
}

// writeQueue writes the responses of the read-only requests of one service
// in the background: a worker sends its next request while the previous
// response is checked for personal data, encrypted and stored. It has one
// writer per worker, so with one worker the records are written, and the
// progress lines printed, in catalog order. After a write fails no other
// is started; the worker's next add returns the error.
type writeQueue struct {
	r    *runner
	mem  *budget
	jobs chan queued
	wg   sync.WaitGroup

	mu      sync.Mutex
	idle    sync.Cond // pending fell to 0, or a write failed
	pending int       // responses queued or being written
	err     error     // of the first write that failed
}

type queued struct {
	req  catalog.Request
	res  httpx.Result
	size int64
}

func newWriteQueue(r *runner, writers int, mem *budget) *writeQueue {
	q := &writeQueue{r: r, mem: mem, jobs: make(chan queued, writers)}
	q.idle.L = &q.mu
	for range writers {
		q.wg.Go(q.write)
	}
	return q
}

func (q *writeQueue) write() {
	for j := range q.jobs {
		size := j.size
		q.mu.Lock()
		err := q.err
		q.mu.Unlock()
		if err == nil {
			err = q.r.write(j.req, j.res)
		}
		j.res.Release()
		q.mem.release(size)
		collect(j.res, size)
		q.mu.Lock()
		if q.err == nil {
			q.err = err
		}
		q.pending--
		if q.pending == 0 || q.err != nil {
			q.idle.Broadcast()
		}
		q.mu.Unlock()
	}
}

// add queues a response once the bodies waiting leave room for its body;
// the queue releases it once written. After an error the response is not
// queued, and the caller releases it.
func (q *writeQueue) add(req catalog.Request, res httpx.Result) error {
	q.mu.Lock()
	err := q.err
	q.mu.Unlock()
	if err != nil {
		return err
	}
	size := int64(len(res.Body))
	q.mem.acquire(size)
	q.mu.Lock()
	q.pending++
	q.mu.Unlock()
	q.jobs <- queued{req, res, size}
	return nil
}

// drain waits until every response queued so far is written: a request
// whose parameters come from an earlier response reads its record.
func (q *writeQueue) drain() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.pending > 0 && q.err == nil {
		q.idle.Wait()
	}
	return q.err
}

// close waits for the writers and returns the error of the first write
// that failed.
func (q *writeQueue) close() error {
	close(q.jobs)
	q.wg.Wait()
	return q.err
}

// budget is a weighted semaphore over bytes; a weight larger than the whole
// budget takes all of it.
type budget struct {
	mu    sync.Mutex
	freed sync.Cond
	total int64
	free  int64
}

func newBudget(total int64) *budget {
	b := &budget{total: total, free: total}
	b.freed.L = &b.mu
	return b
}

func (b *budget) clamp(n int64) int64 { return max(0, min(n, b.total)) }

func (b *budget) acquire(n int64) {
	n = b.clamp(n)
	b.mu.Lock()
	for b.free < n {
		b.freed.Wait()
	}
	b.free -= n
	b.mu.Unlock()
}

func (b *budget) release(n int64) {
	n = b.clamp(n)
	b.mu.Lock()
	b.free += n
	b.mu.Unlock()
	b.freed.Broadcast()
}
