package state

import (
	"context"
	"errors"
)

// This file holds the place of the SQL Server driver (sqlserver*.go) until
// it lands, which deletes it.

var errSQLServerStub = errors.New("SQL Server is not implemented yet")

var _ interface {
	Component
	Querier
} = (*SQLServer)(nil)

// SQLServer is a SQL Server database (not implemented yet).
type SQLServer struct{ o Options }

// NewSQLServer connects to the server. o.DSN is a sqlserver:// URL of the
// go-mssqldb driver; o.Working and o.Pristine are databases on that server.
func NewSQLServer(o Options) (*SQLServer, error) { return nil, errSQLServerStub }

func (s *SQLServer) Name() string  { return s.o.Name }
func (s *SQLServer) Partial() bool { return true }
func (s *SQLServer) Close() error  { return nil }

func (s *SQLServer) Fingerprint(context.Context, []string) (Fingerprint, error) {
	return nil, errSQLServerStub
}
func (s *SQLServer) Restore(context.Context, []string) error         { return errSQLServerStub }
func (s *SQLServer) Seed(context.Context) error                      { return errSQLServerStub }
func (s *SQLServer) Rows(context.Context, []string) (Rows, error)    { return nil, errSQLServerStub }
func (s *SQLServer) Query(context.Context, string) ([]string, error) { return nil, errSQLServerStub }
