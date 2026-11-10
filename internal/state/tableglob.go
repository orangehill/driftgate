package state

import "github.com/orangehill/driftgate/internal/glob"

// tableGlobs are a component's exclude, restore_only and restore_empty
// globs (Options), folded once as its driver compares table names: MySQL
// and PostgreSQL not at all, SQLite in ASCII (asciiLower), SQL Server in
// every letter (strings.ToLower). Each driver gives the matchers its names
// folded the same way, one form of a name per call. config's
// Component.matches applies the same rules.
//
// A driver holds a *tableGlobs its constructor makes: one made otherwise
// has none, and its first match or length check panics (nil) instead of
// matching nothing, and so excluding nothing.
type tableGlobs struct {
	exclude []string
	only    []string // restore_only and restore_empty: both are restore_only
	empty   []string // restore_empty
}

// newTableGlobs folds copies of o's globs (fold nil: as written); o's
// own slices stay as they are.
func newTableGlobs(o Options, fold func(string) string) *tableGlobs {
	folded := func(lists ...[]string) []string {
		var out []string
		for _, l := range lists {
			for _, g := range l {
				if fold != nil {
					g = fold(g)
				}
				out = append(out, g)
			}
		}
		return out
	}
	return &tableGlobs{
		exclude: folded(o.Exclude),
		only:    folded(o.RestoreOnly, o.RestoreEmpty),
		empty:   folded(o.RestoreEmpty),
	}
}

// excluded reports whether a glob of exclude matches name, folded as the
// globs are.
func (g *tableGlobs) excluded(name string) bool { return matchGlobs(g.exclude, name) }

// restoreOnly is excluded for restore_only (or restore_empty).
func (g *tableGlobs) restoreOnly(name string) bool { return matchGlobs(g.only, name) }

// restoreEmpty is excluded for restore_empty.
func (g *tableGlobs) restoreEmpty(name string) bool { return matchGlobs(g.empty, name) }

// matchGlobs reports whether a glob matches name.
func matchGlobs(globs []string, name string) bool {
	for _, g := range globs {
		if glob.Match(g, name) {
			return true
		}
	}
	return false
}
