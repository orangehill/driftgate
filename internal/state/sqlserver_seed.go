package state

import (
	"context"
	"fmt"
)

// Seed makes the current working database the new pristine copy.
//
// Placeholder: the seed replaces this file.
func (s *SQLServer) Seed(ctx context.Context) error {
	return fmt.Errorf("state %s: seeding a SQL Server database is not implemented yet", s.o.Name)
}
