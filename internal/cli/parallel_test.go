package cli

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// parallel returns the error of the first item that fails and keeps the
// bytes in work within the budget; an item larger than the budget works
// next to the smaller ones, one such item at a time.
func TestParallel(t *testing.T) {
	defer func(w int, b int64) { compareWorkers, compareBudget = w, b }(compareWorkers, compareBudget)
	compareWorkers, compareBudget = 8, 100
	sizes := []int64{40, 40, 40, 500, 10, 60, 30, 0, 100, 20, 300, 50}
	var mu sync.Mutex
	var inWork, most, large, mostLarge int64
	firstLarge := false // item 3 works
	// The first large item works until a smaller item starts next to it:
	// by the order of events, not by how long a sleep lasts (under -race
	// the smaller items around it could all start and end within one).
	// Were it to wait for the smaller ones instead, none would start and
	// the test fails after a while rather than hanging.
	small := make(chan struct{})
	var smallOnce sync.Once
	beside := false // a smaller item started while the large one worked
	done := make([]bool, len(sizes))
	err := parallel(len(sizes), func(i int) int64 { return sizes[i] }, func(i int) error {
		big := sizes[i] > 100
		mu.Lock()
		if big {
			large++
			firstLarge = firstLarge || i == 3
		} else {
			inWork += sizes[i]
			if firstLarge {
				smallOnce.Do(func() { close(small) })
			}
		}
		most, mostLarge = max(most, inWork), max(mostLarge, large)
		mu.Unlock()
		if i == 3 {
			select {
			case <-small:
				beside = true
			case <-time.After(10 * time.Second):
			}
		} else {
			time.Sleep(time.Millisecond)
		}
		mu.Lock()
		firstLarge = firstLarge && i != 3
		if big {
			large--
		} else {
			inWork -= sizes[i]
		}
		done[i] = true
		mu.Unlock()
		return nil
	})
	if err != nil || most > 100 || mostLarge > 1 || !beside {
		t.Fatalf("err %v, at most %d bytes and %d large items in work, large next to small: %v", err, most, mostLarge, beside)
	}
	for i, d := range done {
		if !d {
			t.Fatalf("item %d not done", i)
		}
	}
	// A later item fails first; the error of the earlier one counts.
	err = parallel(20, func(int) int64 { return 1 }, func(i int) error {
		switch i {
		case 3:
			time.Sleep(20 * time.Millisecond)
			return errors.New("3")
		case 5:
			return errors.New("5")
		}
		return nil
	})
	if err == nil || err.Error() != "3" {
		t.Fatalf("error %v, want 3", err)
	}
	if err := parallel(0, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// After a large item the garbage is collected at once.
func TestParallelCollectsAfterLargeItem(t *testing.T) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	before := m.NumGC
	if err := parallel(1, func(int) int64 { return collectAfter }, func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m)
	if m.NumGC == before {
		t.Fatal("no collection after a large item")
	}
}
