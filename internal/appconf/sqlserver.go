package appconf

import (
	"fmt"
	"strings"
)

// The settings of a SQL Server database. Until appconf reads them
// (sqlServerReady), a setting that names SQL Server is noted as a database
// DriftGate cannot use, as before; the functions here are the places the
// reading goes.

// sqlServerReady turns on the reading of SQL Server settings: driverName,
// csprojDriver and the readers then give the driver SQLServer, and the
// functions below read the setting.
const sqlServerReady = false

// sqlServerValue reads a DSN only SQL Server's clients write, before
// fromValue tries the other forms: a JDBC URL (sqlserver://host;
// databaseName=…, the jdbc: prefix already cut), a go-mssqldb URL, an
// ODBC string. ok is false for any other DSN.
func sqlServerValue(v string) (p parts, ok bool, err error) { return parts{}, false, nil }

// fromSQLServerURL reads a URL whose scheme names SQL Server (sqlserver://,
// mssql://), reached only when sqlServerReady.
func fromSQLServerURL(v string) (parts, error) { return parts{other: "SQL Server"}, nil }

// sqlServerConnString reads an ADO.NET connection string of
// Microsoft.Data.SqlClient: one with keys or a server form only SQL Server
// takes (Trusted_Connection, MultipleActiveResultSets, AttachDbFilename,
// tcp:, np:, (localdb), host\instance). ok is false for one that is not.
func sqlServerConnString(kv fields) (p parts, ok bool, err error) {
	for _, k := range []string{"trustedconnection", "multipleactiveresultsets", "attachdbfilename"} {
		if _, ok := kv[k]; ok {
			return parts{other: "SQL Server"}, true, nil
		}
	}
	server, _ := kv.get(hostKeys)
	switch ls := strings.ToLower(server); {
	case strings.HasPrefix(ls, "tcp:"), strings.HasPrefix(ls, "(localdb)"), strings.HasPrefix(ls, "np:"), strings.Contains(ls, `\`):
		return parts{other: "SQL Server"}, true, nil
	}
	return parts{}, false, nil
}

// sqlServerImage reports a compose image that runs SQL Server
// (imageDriver); repo is the image without tag or digest, in lower case.
func sqlServerImage(repo string) bool { return false }

// sqlServerDatabase reads the SQL Server service name of a compose file
// (composeProject.database), reached only when sqlServerImage reports one.
func (cp *composeProject) sqlServerDatabase(root, name string, env Lookup) (parts, error) {
	return parts{}, fmt.Errorf("%s: service %s: SQL Server is not implemented yet", cp.file, printableName(name))
}

// laravelSQLServer completes Laravel's sqlsrv setting of a .env file, p
// (DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME, DB_PASSWORD read), with
// what only SQL Server takes (DB_ENCRYPT, DB_TRUST_SERVER_CERTIFICATE) and
// DB_URL; reached only when sqlServerReady.
func laravelSQLServer(file string, env map[string]string, p parts) (parts, error) {
	return parts{}, fmt.Errorf("%s: SQL Server is not implemented yet", file)
}

// sqlServerDSN is the DSN of a SQL Server setting (parts.dsn): a sqlserver://
// URL of the go-mssqldb driver, with the TLS of the application's client
// (p.raw holds the keys the shared options leave out). Reached only when
// sqlServerReady, or for a component whose driver is sqlserver and whose
// setting does not say which database it is.
func (p parts) sqlServerDSN(where string) (string, error) {
	return "", fmt.Errorf("%s: SQL Server is not implemented yet", where)
}
