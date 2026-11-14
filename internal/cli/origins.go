package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/orangehill/driftgate/internal/compare"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/snapshot"
)

// setOrigins finds, per service, the origins two snapshot sets were captured
// from, recorded in their meta.json. The comparison takes each side's own
// origins (and the aliases) as the same when they differ. notes describe
// those services for the report ("http://localhost:8080 ≙
// http://localhost:8081"); a service whose origin is not known for both
// sets, or is the same, is compared as it is. A set that records no origin
// at all against one that does is an error
// naming the command that captures it again: compared as it is, every
// absolute URL the application writes would drift.
//
// The sets' project folders (meta.json's roots) come with them: two
// checkouts of the project (git worktrees) are in different folders, and
// a response that writes the folder (a debug stack trace) is compared
// through them.
func setOrigins(p *config.Project, base, cur *snapshot.Set) (s *sides, notes []string, err error) {
	sets := [2]*snapshot.Set{base, cur}
	var recorded [2]map[string]string
	var roots, rowTables [2][]string
	for i, s := range sets {
		meta, err := s.Meta()
		if err != nil {
			return nil, nil, err
		}
		if meta != nil {
			recorded[i], roots[i], rowTables[i] = meta.Origins, meta.Roots, meta.RowTracking
		}
	}
	var origins map[string][2][]string
	for i := range sets {
		if len(recorded[i]) == 0 && len(recorded[1-i]) > 0 {
			return nil, nil, noOrigin(p, sets[i])
		}
	}
	for _, name := range p.TargetNames() {
		t := p.Services()[name]
		name = p.Canonical(name)
		o := [2]string{recorded[0][name], recorded[1][name]}
		if o[0] == "" || o[1] == "" {
			continue
		}
		if origins == nil {
			origins = map[string][2][]string{}
		}
		// The same origins too: the comparison replaces nothing then, and
		// what expectations digest is written without them.
		origins[name] = [2][]string{t.Origins(o[0]), t.Origins(o[1])}
		if o[0] == o[1] {
			continue
		}
		note := o[0] + " ≙ " + o[1]
		if len(p.Targets) > 1 {
			note = name + ": " + note
		}
		notes = append(notes, note)
	}
	// Two records of one folder in a home folder (~/…) may be of two
	// homes: compared through them too.
	home := slices.ContainsFunc(append(append([]string{}, roots[0]...), roots[1]...), func(r string) bool { return strings.HasPrefix(r, "~") })
	if origins == nil && slices.Equal(roots[0], roots[1]) && !home && (rowTables[0] == nil || rowTables[1] == nil || slices.Equal(rowTables[0], rowTables[1])) {
		return nil, notes, nil
	}
	return &sides{origins: origins, roots: roots, rowTables: rowTables, folders: compare.NewFolders(roots[0], roots[1])}, notes, nil
}

// sides are what the two sets of a comparison were captured from where
// they differ: the origins per service (setOrigins) and the project
// folders. nil: the same.
type sides struct {
	origins   map[string][2][]string
	roots     [2][]string
	rowTables [2][]string      // the tables whose rows each tracked (meta.RowTracking)
	folders   *compare.Folders // the forms of roots, made once for every request
	// learning: two captures of one state are compared, which learn the
	// values of every tracked row (compare.Side.LearnRowValues).
	learning bool
}

// noOrigin says how to capture a set that records no origin again.
func noOrigin(p *config.Project, s *snapshot.Set) error {
	name := filepath.Base(s.Dir)
	if filepath.Dir(s.Dir) == filepath.Clean(p.Layout.Baselines) {
		return fmt.Errorf("baseline %s records no origin: capture it again: remove %s and run driftgate run --baseline %s", name, s.Dir, name)
	}
	return fmt.Errorf("capture %s records no origin: capture it again with driftgate capture %s --force", name, name)
}
