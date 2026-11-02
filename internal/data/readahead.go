package data

import (
	"context"
	"database/sql"
	"runtime"
	"sync"
)

// batchRows is how many rows readAhead hands out at once, at most.
const batchRows = 256

// batchBytes ends a batch early: rows of large text fill a batch of a few
// rows, so that the batches read ahead stay many and small.
const batchBytes = 1 << 20

// aheadBytes bounds the memory of the batches read ahead and not yet
// handed to each (with what work made of them, several times as much for
// free text): the reader waits while the batches in flight hold this much.
// A batch larger than that still proceeds, alone.
const aheadBytes = 64 << 20

// rowBatch is rows read ahead (values as bytes, nil for NULL) and what a
// worker made of them.
type rowBatch struct {
	rows [][][]byte
	size int // the bytes the rows hold, about
	work any // what work made
	err  error
	done chan struct{}
}

// byteBudget is the bytes of the batches in flight (aheadBytes).
type byteBudget struct {
	mu   sync.Mutex
	free sync.Cond
	used int
}

// take waits until n bytes fit in the budget, or nothing is in flight.
func (u *byteBudget) take(n int) {
	u.mu.Lock()
	for u.used > 0 && u.used+n > aheadBytes {
		u.free.Wait()
	}
	u.used += n
	u.mu.Unlock()
}

func (u *byteBudget) give(n int) {
	u.mu.Lock()
	u.used -= n
	u.mu.Unlock()
	u.free.Signal()
}

// readAhead runs a query on a goroutine of its own and hands its rows out
// in batches: work runs on a worker per CPU (what does not depend on the
// order of rows, such as the detector over free text), and each gets every
// batch in the order of the rows, after its work, on the calling
// goroutine. An error of work or each stops the read.
func readAhead(ctx context.Context, c *sql.Conn, q string, cols int, work func(*rowBatch) error, each func(*rowBatch) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows, err := c.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	n := runtime.GOMAXPROCS(0)
	jobs := make(chan *rowBatch, n)
	order := make(chan *rowBatch, 2*n)
	var budget byteBudget
	budget.free.L = &budget.mu
	var workers sync.WaitGroup
	for range n {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for b := range jobs {
				if work != nil {
					b.err = work(b)
				}
				close(b.done)
			}
		}()
	}
	defer workers.Wait()
	var readErr error
	go func() {
		defer close(order)
		defer close(jobs)
		defer rows.Close()
		raw := make([]sql.RawBytes, cols)
		ptrs := make([]any, cols)
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		b := &rowBatch{done: make(chan struct{})}
		var arena []byte // the values of a batch, one allocation for many
		keep := func(r []byte) []byte {
			if r == nil {
				return nil
			}
			if cap(arena)-len(arena) < len(r) || arena == nil {
				arena = make([]byte, 0, max(64<<10, len(r)))
				b.size += cap(arena)
			}
			arena = append(arena, r...)
			return arena[len(arena)-len(r) : len(arena) : len(arena)]
		}
		send := func() bool {
			budget.take(b.size)
			select {
			case order <- b:
			case <-ctx.Done():
				budget.give(b.size)
				return false
			}
			select {
			case jobs <- b:
			case <-ctx.Done():
				b.err = ctx.Err()
				close(b.done)
				return false
			}
			b = &rowBatch{done: make(chan struct{})}
			arena = nil
			return true
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				readErr = err
				return
			}
			row := make([][]byte, cols)
			for i, r := range raw {
				row[i] = keep(r)
			}
			b.rows = append(b.rows, row)
			b.size += cols * 24 // the slices of the row
			if (len(b.rows) >= batchRows || b.size >= batchBytes) && !send() {
				return
			}
		}
		if readErr = rows.Err(); readErr == nil && len(b.rows) > 0 {
			send()
		}
	}()
	var stop error
	for b := range order {
		if stop != nil {
			budget.give(b.size)
			continue // until the reader stops
		}
		<-b.done
		if stop = b.err; stop == nil {
			stop = each(b)
		}
		budget.give(b.size)
		if stop != nil {
			cancel()
		}
	}
	if stop != nil {
		return stop
	}
	if readErr != nil {
		return readErr
	}
	return ctx.Err()
}
