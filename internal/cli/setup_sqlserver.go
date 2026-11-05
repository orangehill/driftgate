package cli

import (
	"fmt"
	"strings"

	"github.com/orangehill/driftgate/internal/config"
)

// A SQL Server database is restored by the application's own login: the
// pristine copy is a database DriftGate restores from a COPY_ONLY backup
// of the working one, on the same server. The login needs the server role
// dbcreator (it creates, renames and drops the copies, which it then
// owns) and db_owner in the working database (it backs it up, restores
// it table by table and, when it must, as a whole). sa, a sysadmin, needs
// nothing. dbcreator reaches every database of the server (it may take
// any offline, restore over it or drop it), so setup also offers the
// narrower grant (sqlServerNarrow), which changes the working database's
// owner and is therefore not the default.

// sqlServerGrant is what an administrator runs for the login user to make
// and restore the pristine copy of the SQL Server component c (grant): in
// master, the server role dbcreator; in the working database, a user for
// the login (when it has none yet) in the role db_owner (unless the login
// owns the database: it is dbo there). Every statement may run again. ""
// for sa, which may do all of it already.
func sqlServerGrant(c config.Component, user string) string {
	if strings.EqualFold(user, "sa") {
		return ""
	}
	u, n := qsName(user), sqlString(user)
	return fmt.Sprintf("USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER %s; USE %s; "+
		"IF SUSER_SID(%s) NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER %s FOR LOGIN %s; "+
		"IF SUSER_SID(%s) <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER %s;",
		u, qsName(c.Working), n, u, u, n, u)
}

// sqlServerNarrow is the grant setup offers instead of sqlServerGrant:
// CREATE ANY DATABASE, and the login made the owner of the working
// database (dbo there), reach only the databases the login owns. It
// changes the owner, which an application may rely on, and fails while
// the login has a user in the working database.
func sqlServerNarrow(c config.Component, user string) string {
	if user == "" {
		user = "<user>"
	}
	u := qsName(user)
	return fmt.Sprintf("dbcreator lets the login take any database of the server offline, restore over it or drop it; narrower, instead of the grant above: "+
		"USE [master]; GRANT CREATE ANY DATABASE TO %s; ALTER AUTHORIZATION ON DATABASE::%s TO %s; "+
		"it makes %s the owner of %s (dbo there), which the application may rely on not changing; drop the login's user in %s first if it has one (USE %s; DROP USER %s;)",
		u, qsName(c.Working), u, u, c.Working, c.Working, qsName(c.Working), u)
}

// sqlServerFast is the grant setup proposes besides sqlServerGrant, which
// a restore does without: the server's performance state, from which a
// fingerprint of data that did not change is reused (the end of the log,
// open transactions, SQL Server's per-table write counters); without it
// every table is read again before every request that changes data.
// SQL Server 2022 split the permission off VIEW SERVER STATE, which
// covers it; older versions know only that one.
func sqlServerFast(user string) string {
	if user == "" {
		user = "<user>"
	}
	u := qsName(user)
	return fmt.Sprintf("optional, for restores that skip unchanged data (without it every table is read again before every request that changes data): USE [master]; GRANT VIEW SERVER PERFORMANCE STATE TO %s; on SQL Server 2022 and later, GRANT VIEW SERVER STATE TO %s; on 2017 and 2019", u, u)
}

// sqlServerCopy says, before setup asks to make the pristine copy of a SQL
// Server database, where it is made: a database on the same server, and
// the backup files DriftGate keeps of it in the server's own folder.
func sqlServerCopy(c config.Component) string {
	return fmt.Sprintf("%s is restored on the server from a COPY_ONLY backup of %s (the application's backup chain stays as it is); DriftGate keeps two backup files of it in the server's default backup folder", c.Pristine, c.Working)
}

// qsName is a SQL Server name in brackets, ] doubled.
func qsName(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

// sqlString is a SQL Server Unicode string literal, ' doubled.
func sqlString(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sameDatabase reports whether a and b name one database of driver: SQL
// Server takes the names of databases case-insensitively by default, so a
// worktree on Shop is on the main worktree's shop.
func sameDatabase(driver, a, b string) bool {
	if driver == config.SQLServer {
		return strings.EqualFold(a, b)
	}
	return a == b
}
