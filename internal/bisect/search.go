// Package bisect finds the commit that introduced a drift: a binary search
// over the commits between a known good and a known bad state, run for every
// drift group on its own, with the outcome of each tested commit shared
// between the groups.
package bisect

import (
	"errors"
	"sort"
)

// Probe is the outcome of one commit.
type Probe struct {
	Skip    string          // why the commit could not be tested (hook failed, application did not answer); empty when tested
	Present map[string]bool // groups (drift signatures) that drift at the commit
}

// Status of the answer for one group.
const (
	Found    = "found"      // one commit
	Range    = "range"      // one of several commits, the others between could not be tested
	AtGood   = "at-good"    // the group drifts already at the good commit
	NotAtBad = "not-at-bad" // the group does not drift at the bad commit
)

// Answer for one group. Commits are indexes into the path: 0 is the good
// commit, the last index the bad one.
type Answer struct {
	Group  string
	Status string
	First  int // the first commit with the drift (Found), or the last of the range (Range)
	From   int // with Range: the first commit that may have introduced it
	// Unsteady lists tested commits that contradict the answer: the group
	// drifts before First, or does not drift at or after it. The search
	// assumes a drift stays once it appears; with these, the commit named
	// is one place where it appears, not necessarily the first.
	Unsteady []int
}

// Searcher tests commits through Test, at most once each, and keeps every
// outcome for all groups.
type Searcher struct {
	Last int                        // index of the bad commit; the good one is 0
	Test func(i int) (Probe, error) // an error stops the search
	done map[int]Probe
	runs []int // commits tested, in order
}

// ErrEndpoint means the good or the bad commit could not be tested.
var ErrEndpoint = errors.New("the good or the bad commit could not be tested")

// Probe returns the outcome of commit i, testing it the first time.
func (s *Searcher) Probe(i int) (Probe, error) {
	if s.done == nil {
		s.done = map[int]Probe{}
	}
	if p, ok := s.done[i]; ok {
		return p, nil
	}
	p, err := s.Test(i)
	if err != nil {
		return Probe{}, err
	}
	s.done[i] = p
	s.runs = append(s.runs, i)
	return p, nil
}

// Tested returns the commits tested so far, in the order they were tested.
func (s *Searcher) Tested() []int { return append([]int{}, s.runs...) }

// Skipped returns the commits that could not be tested, in path order.
func (s *Searcher) Skipped() []int {
	var out []int
	for i, p := range s.done {
		if p.Skip != "" {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// Outcome returns what is known of commit i.
func (s *Searcher) Outcome(i int) (Probe, bool) {
	p, ok := s.done[i]
	return p, ok
}

// Endpoints tests the good and the bad commit; both must be testable.
func (s *Searcher) Endpoints() (good, bad Probe, err error) {
	if bad, err = s.Probe(s.Last); err != nil {
		return
	}
	if good, err = s.Probe(0); err != nil {
		return
	}
	if good.Skip != "" || bad.Skip != "" {
		err = ErrEndpoint
	}
	return
}

// Search finds the first commit where group drifts. Commits already tested
// (for another group) narrow the range without a new test; a new test goes
// to the untested commit nearest the middle, so a commit that cannot be
// tested is stepped around as git bisect skip does.
func (s *Searcher) Search(group string) (Answer, error) {
	good, bad, err := s.Endpoints()
	if err != nil {
		return Answer{}, err
	}
	a := Answer{Group: group}
	switch {
	case good.Present[group]:
		a.Status = AtGood
		return a, nil
	case !bad.Present[group]:
		a.Status = NotAtBad
		return a, nil
	}
	lo, hi := 0, s.Last // lo is known clean, hi known to drift
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		i, known := -1, false
		// A commit already tested costs nothing.
		for j := lo + 1; j < hi; j++ {
			if p, ok := s.done[j]; ok && p.Skip == "" && (i < 0 || nearer(j, i, mid)) {
				i, known = j, true
			}
		}
		if !known {
			for j := lo + 1; j < hi; j++ {
				if _, ok := s.done[j]; !ok && (i < 0 || nearer(j, i, mid)) {
					i = j
				}
			}
		}
		if i < 0 {
			break // everything between lo and hi was skipped
		}
		p, err := s.Probe(i)
		if err != nil {
			return Answer{}, err
		}
		switch {
		case p.Skip != "":
			continue
		case p.Present[group]:
			hi = i
		default:
			lo = i
		}
	}
	a.First = hi
	if a.Status = Found; hi-lo > 1 {
		a.Status, a.From = Range, lo+1
	}
	for i, p := range s.done {
		if p.Skip == "" && ((i < a.First && i > 0 && p.Present[group]) || (i >= a.First && !p.Present[group])) {
			a.Unsteady = append(a.Unsteady, i)
		}
	}
	sort.Ints(a.Unsteady)
	return a, nil
}

// nearer reports whether j is nearer to mid than i; the earlier commit wins
// a tie.
func nearer(j, i, mid int) bool {
	dj, di := abs(j-mid), abs(i-mid)
	return dj < di || (dj == di && j < i)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
