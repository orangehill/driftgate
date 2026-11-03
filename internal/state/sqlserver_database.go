package state

import (
	"context"
	"fmt"
)

// restoreDatabase brings the whole working database back from the backup
// of the pristine copy (the fallback of Restore).
//
// Placeholder: the database restore replaces this file.
func (s *SQLServer) restoreDatabase(ctx context.Context) error {
	return fmt.Errorf("state %s: restoring a SQL Server database is not implemented yet", s.o.Name)
}
