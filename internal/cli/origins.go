package cli

import (
	"fmt"
	"path/filepath"

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
func setOrigins(p *config.Project, base, cur *snapshot.Set) (origins map[string][2][]string, notes []string, err error) {
	sets := [2]*snapshot.Set{base, cur}
	var recorded [2]map[string]string
	for i, s := range sets {
		meta, err := s.Meta()
		if err != nil {
			return nil, nil, err
		}
		if meta != nil {
			recorded[i] = meta.Origins
		}
	}
	for i := range sets {
		if len(recorded[i]) == 0 && len(recorded[1-i]) > 0 {
			return nil, nil, noOrigin(p, sets[i])
		}
	}
	for _, name := range p.TargetNames() {
		t := p.Services()[name]
		name = p.Canonical(name)
		o := [2]string{recorded[0][name], recorded[1][name]}
		if o[0] == "" || o[1] == "" || o[0] == o[1] {
			continue
		}
		if origins == nil {
			origins = map[string][2][]string{}
		}
		origins[name] = [2][]string{t.Origins(o[0]), t.Origins(o[1])}
		note := o[0] + " ≙ " + o[1]
		if len(p.Targets) > 1 {
			note = name + ": " + note
		}
		notes = append(notes, note)
	}
	return origins, notes, nil
}

// noOrigin says how to capture a set that records no origin again.
func noOrigin(p *config.Project, s *snapshot.Set) error {
	name := filepath.Base(s.Dir)
	if filepath.Dir(s.Dir) == filepath.Clean(p.Layout.Baselines) {
		return fmt.Errorf("baseline %s records no origin: capture it again: remove %s and run driftgate run --baseline %s", name, s.Dir, name)
	}
	return fmt.Errorf("capture %s records no origin: capture it again with driftgate capture %s --force", name, name)
}
