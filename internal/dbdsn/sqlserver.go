package dbdsn

import "errors"

// errSQLServer is the error of every SQL Server DSN until its parser lands.
var errSQLServer = errors.New("dsn: SQL Server is not implemented yet")

// sqlServerHosts lists every host the SQL Server driver may connect to
// with the DSN, as the driver reads it (Hosts); "" is never one, as SQL
// Server takes TCP only. No error carries the DSN.
//
// Stub: the SQL Server DSN parser replaces it.
func sqlServerHosts(dsn string) ([]string, error) { return nil, errSQLServer }

// sqlServerServer names the server an SQL Server DSN connects to, without
// the user or password (Server): host:port. No error carries the DSN.
//
// Stub: the SQL Server DSN parser replaces it.
func sqlServerServer(dsn string) (string, error) { return "", errSQLServer }
