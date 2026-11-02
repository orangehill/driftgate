package httpx

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
)

// A body of unknown length cannot be read into a buffer of its size: grown
// as it is read, the buffer of a large body takes up to twice its size, and
// in pieces copied together at the end it does too. A capture holds bodies
// of tens of megabytes; read off the heap, into a region of address space
// large enough for any body (MaxBody), only the pages the body fills take
// memory, it is held once, and Release gives it back without waiting for a
// collection (to the system, or to the pool for the next body); a body of
// known length goes there too, so it is not garbage on the heap afterwards. A
// small body (most are) stays on the heap: of one of unknown length
// smallBody bytes are read first.
const smallBody = 64 << 10

// region is memory mapped outside the Go heap for one body.
type region struct {
	once sync.Once
	m    *mapping
}

// mapping is address space for one body at a time (MaxBody and room for
// the tag of the encryption in place); touched is how much of it has been
// written, and so takes memory.
type mapping struct {
	mem     []byte
	touched int
}

// One released mapping is kept for the next body, when its body was up
// to keepBytes and no other body is off the heap: its pages are already
// there, while a fresh mapping costs a page fault for every page a body
// fills (an 8 MB body: twice the time of its read). Kept only while no
// other body is held, it adds to the memory only between bodies, and the
// next body takes it; a larger body's mapping is unmapped at once.
// FreePool gives it back; a capture calls it when it ends.
const keepBytes = 8 << 20

var pool struct {
	mu   sync.Mutex
	kept *mapping
}

// regions counts the regions not yet released.
var regions atomic.Int64

// Unreleased is the number of bodies read off the heap whose results are
// not released yet: after a capture, 0.
func Unreleased() int { return int(regions.Load()) }

// newRegion takes the kept mapping, or maps a new one.
func newRegion() (*region, bool) {
	pool.mu.Lock()
	m := pool.kept
	pool.kept = nil
	pool.mu.Unlock()
	if m == nil {
		mem, ok := mapRegion(MaxBody + 1 + bytes.MinRead)
		if !ok {
			return nil, false
		}
		m = &mapping{mem: mem}
	}
	regions.Add(1)
	return &region{m: m}, true
}

func (g *region) free() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		m := g.m
		g.m = nil
		pool.mu.Lock()
		if regions.Add(-1) == 0 && m.touched <= keepBytes && pool.kept == nil {
			pool.kept, m = m, nil
		}
		pool.mu.Unlock()
		if m != nil {
			unmapRegion(m.mem)
		}
	})
}

// FreePool unmaps the mapping kept for a later body.
func FreePool() {
	pool.mu.Lock()
	m := pool.kept
	pool.kept = nil
	pool.mu.Unlock()
	if m != nil {
		unmapRegion(m.mem)
	}
}

// readOffHeap reads a body of the given length (-1: unknown): on the heap
// when it is small, else into a region. Without regions (another system,
// or a mapping that fails) the body is read on the heap after all.
func readOffHeap(r io.Reader, length int64) ([]byte, *region, error) {
	var head []byte
	n := 0
	if length < 0 {
		head = make([]byte, smallBody)
		var end bool
		var err error
		if n, end, err = fill(r, head); err != nil {
			return nil, nil, err
		}
		if end {
			return append(make([]byte, 0, n+bytes.MinRead), head[:n]...), nil, nil
		}
	}
	// Room for one byte over the most the body may hold, which tells a body
	// that is too large.
	most := MaxBody
	if length >= 0 {
		most = int(length)
	}
	g, ok := newRegion()
	if !ok {
		// Read as readBody does without regions; bytes.Buffer leaves room
		// at the end (at least bytes.MinRead) for an encryption in place,
		// as a region does, so a private body is not sealed into a copy.
		b := bytes.NewBuffer(make([]byte, 0, n+min(max(int(length), 0), maxPrealloc)+bytes.MinRead))
		b.Write(head[:n])
		_, err := b.ReadFrom(r)
		return b.Bytes(), nil, err
	}
	mem := g.m.mem
	copy(mem, head[:n])
	m, _, err := fill(r, mem[n:most+1])
	size := n + m
	g.m.touched = max(g.m.touched, size)
	if err != nil {
		g.free()
		return nil, nil, err
	}
	return mem[: size : size+bytes.MinRead], g, nil
}

// fill reads into b until b is full or the reader ends (end); a read that
// fails is an error, also after part of b was read.
func fill(r io.Reader, b []byte) (n int, end bool, err error) {
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err == io.EOF {
			return n, true, nil
		}
		if err != nil {
			return n, false, err
		}
	}
	return n, false, nil
}
