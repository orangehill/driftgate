package bisect

import (
	"errors"
	"math/bits"
	"reflect"
	"testing"
)

// history is a made-up path of commits: each group drifts from its commit
// on, and the skipped commits cannot be tested.
type history struct {
	last  int
	first map[string]int
	skip  map[int]bool
	// drifts overrides the rule for single commits: group → commit → drifts.
	drifts map[string]map[int]bool
}

func (h history) searcher() (*Searcher, *[]int) {
	var tested []int
	s := &Searcher{Last: h.last, Test: func(i int) (Probe, error) {
		tested = append(tested, i)
		if h.skip[i] {
			return Probe{Skip: "build failed"}, nil
		}
		p := Probe{Present: map[string]bool{}}
		for g, f := range h.first {
			p.Present[g] = i >= f
			if d, ok := h.drifts[g][i]; ok {
				p.Present[g] = d
			}
		}
		return p, nil
	}}
	return s, &tested
}

func TestSearchFindsEveryPosition(t *testing.T) {
	for last := 1; last <= 40; last++ {
		for first := 1; first <= last; first++ {
			s, tested := history{last: last, first: map[string]int{"g": first}}.searcher()
			a, err := s.Search("g")
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != Found || a.First != first || len(a.Unsteady) > 0 {
				t.Fatalf("last %d, first %d: %+v", last, first, a)
			}
			// The two ends, then a binary search over what lies between.
			if max := 2 + bits.Len(uint(last-1)); len(*tested) > max {
				t.Errorf("last %d, first %d: %d tests %v, want at most %d", last, first, len(*tested), *tested, max)
			}
		}
	}
}

func TestSearchSharesTests(t *testing.T) {
	h := history{last: 16, first: map[string]int{"a": 3, "b": 11, "c": 12}}
	s, tested := h.searcher()
	for g, want := range h.first {
		a, err := s.Search(g)
		if err != nil || a.Status != Found || a.First != want {
			t.Fatalf("%s: %+v %v", g, a, err)
		}
	}
	// Alone each group takes 2 + 4 tests; together they share the ends and
	// the commits tested for the others.
	if len(*tested) >= 3*6-4 {
		t.Errorf("%d tests for three groups: %v", len(*tested), *tested)
	}
	seen := map[int]bool{}
	for _, i := range *tested {
		if seen[i] {
			t.Errorf("commit %d tested twice: %v", i, *tested)
		}
		seen[i] = true
	}
	if !reflect.DeepEqual(s.Tested(), *tested) {
		t.Errorf("Tested %v, want %v", s.Tested(), *tested)
	}
}

func TestSearchStepsAroundSkips(t *testing.T) {
	// The middle commit cannot be built; its neighbours decide.
	h := history{last: 8, first: map[string]int{"g": 6}, skip: map[int]bool{4: true}}
	s, _ := h.searcher()
	a, err := s.Search("g")
	if err != nil || a.Status != Found || a.First != 6 {
		t.Fatalf("%+v %v", a, err)
	}
	if !reflect.DeepEqual(s.Skipped(), []int{4}) {
		t.Errorf("skipped %v", s.Skipped())
	}
}

func TestSearchReportsRange(t *testing.T) {
	// The commits just before the one that drifts cannot be tested: the
	// drift came with one of them or with it.
	h := history{last: 10, first: map[string]int{"g": 7}, skip: map[int]bool{5: true, 6: true}}
	s, _ := h.searcher()
	a, err := s.Search("g")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != Range || a.From != 5 || a.First != 7 {
		t.Fatalf("%+v", a)
	}

	// Nothing between the ends can be tested.
	h = history{last: 4, first: map[string]int{"g": 2}, skip: map[int]bool{1: true, 2: true, 3: true}}
	s, _ = h.searcher()
	if a, err = s.Search("g"); err != nil || a.Status != Range || a.From != 1 || a.First != 4 {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestSearchEnds(t *testing.T) {
	h := history{last: 6, first: map[string]int{"old": 0, "fixed": 3, "never": 7, "new": 2}}
	// "fixed" drifts at 3 to 5 and is gone at the bad commit.
	h.drifts = map[string]map[int]bool{"fixed": {6: false}}
	s, tested := h.searcher()
	for g, want := range map[string]string{"old": AtGood, "fixed": NotAtBad, "never": NotAtBad, "new": Found} {
		a, err := s.Search(g)
		if err != nil || a.Status != want {
			t.Errorf("%s: %+v %v, want %s", g, a, err, want)
		}
	}
	if (*tested)[0] != 6 || (*tested)[1] != 0 {
		t.Errorf("the ends are tested first: %v", *tested)
	}

	h = history{last: 5, first: map[string]int{"g": 2}, skip: map[int]bool{5: true}}
	s, _ = h.searcher()
	if _, err := s.Search("g"); !errors.Is(err, ErrEndpoint) {
		t.Errorf("an untestable bad commit: %v", err)
	}
	h = history{last: 5, first: map[string]int{"g": 2}, skip: map[int]bool{0: true}}
	s, _ = h.searcher()
	if _, err := s.Search("g"); !errors.Is(err, ErrEndpoint) {
		t.Errorf("an untestable good commit: %v", err)
	}
}

func TestSearchNamesUnsteadyDrift(t *testing.T) {
	// The drift appears at 2, is fixed at 4 and comes back at 6. Another
	// group's search tested 3 and 4, so the evidence is there.
	h := history{last: 8, first: map[string]int{"g": 2, "other": 4}, drifts: map[string]map[int]bool{"g": {4: false, 5: false}}}
	s, _ := h.searcher()
	if _, err := s.Search("other"); err != nil {
		t.Fatal(err)
	}
	a, err := s.Search("g")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != Found || len(a.Unsteady) == 0 {
		t.Fatalf("%+v", a)
	}
	for _, i := range a.Unsteady {
		p, _ := s.Outcome(i)
		if (i < a.First) != p.Present["g"] {
			t.Errorf("commit %d is not a contradiction of %+v", i, a)
		}
	}

	// A steady drift has none.
	s, _ = history{last: 8, first: map[string]int{"g": 5, "other": 2}}.searcher()
	s.Search("other")
	if a, _ := s.Search("g"); len(a.Unsteady) > 0 {
		t.Errorf("steady drift: %+v", a)
	}
}

func TestSearchStopsOnError(t *testing.T) {
	stop := errors.New("interrupted")
	n := 0
	s := &Searcher{Last: 8, Test: func(i int) (Probe, error) {
		if n++; n == 3 {
			return Probe{}, stop
		}
		return Probe{Present: map[string]bool{"g": i >= 5}}, nil
	}}
	if _, err := s.Search("g"); !errors.Is(err, stop) {
		t.Fatalf("error %v", err)
	}
	// The failed commit is not remembered as tested.
	if len(s.Tested()) != 2 {
		t.Errorf("tested %v", s.Tested())
	}
}
