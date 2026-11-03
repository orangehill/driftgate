package state

import "context"

// Restore brings the given items of the working database back to the
// pristine copy; nil means everything.
//
// Placeholder: the item-level restore replaces this file.
func (s *SQLServer) Restore(ctx context.Context, items []string) error {
	return s.restoreDatabase(ctx)
}
