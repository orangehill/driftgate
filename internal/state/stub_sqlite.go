package state

import (
	"context"
	"errors"
)

// This file holds the place of the SQLite driver (sqlite*.go) until it
// lands, which deletes it.

var errSQLiteStub = errors.New("SQLite is not implemented yet")

var _ interface {
	Component
	Querier
} = (*SQLite)(nil)

// SQLite is an SQLite database (not implemented yet).
type SQLite struct{ o Options }

// NewSQLite opens the database file. o.Working and o.Pristine are absolute
// paths the caller resolved (the working file inside the project, past
// symbolic links); o.DSN is not used.
func NewSQLite(o Options) (*SQLite, error) { return nil, errSQLiteStub }

func (s *SQLite) Name() string  { return s.o.Name }
func (s *SQLite) Partial() bool { return true }
func (s *SQLite) Close() error  { return nil }

func (s *SQLite) Fingerprint(context.Context, []string) (Fingerprint, error) {
	return nil, errSQLiteStub
}
func (s *SQLite) Restore(context.Context, []string) error         { return errSQLiteStub }
func (s *SQLite) Seed(context.Context) error                      { return errSQLiteStub }
func (s *SQLite) Rows(context.Context, []string) (Rows, error)    { return nil, errSQLiteStub }
func (s *SQLite) Query(context.Context, string) ([]string, error) { return nil, errSQLiteStub }
