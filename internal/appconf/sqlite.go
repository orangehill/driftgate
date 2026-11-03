package appconf

import (
	"fmt"
	"strings"
)

// The settings of an SQLite database. Until appconf reads them
// (sqliteReady), a setting that names SQLite is noted as a database
// DriftGate cannot use, as before; the functions here are the places the
// reading goes.

// sqliteReady turns on the reading of SQLite settings: driverName,
// csprojDriver and the readers then give the driver SQLite, and the
// functions below read the file.
const sqliteReady = false

// isSQLiteValue reports a DSN that names an SQLite file: sqlite:…, or a
// file name ending in .db, .sqlite or .sqlite3 (fromValue).
func isSQLiteValue(v string) bool {
	return strings.HasPrefix(v, "sqlite:") || strings.HasSuffix(v, ".db") || strings.HasSuffix(v, ".sqlite") || strings.HasSuffix(v, ".sqlite3")
}

// fromSQLiteValue reads a DSN isSQLiteValue reports.
func fromSQLiteValue(v string) (parts, error) { return parts{other: "SQLite"}, nil }

// fromSQLiteURL reads a URL whose scheme names SQLite (sqlite://, file:),
// reached only when sqliteReady.
func fromSQLiteURL(v string) (parts, error) { return parts{other: "SQLite"}, nil }

// sqliteConnString reads an ADO.NET connection string of
// Microsoft.Data.Sqlite (Data Source=app.db, Filename=…); ok is false for
// one that is not.
func sqliteConnString(kv fields) (p parts, ok bool, err error) {
	if _, ok := kv["filename"]; ok {
		return parts{other: "SQLite"}, true, nil
	}
	server, _ := kv.get(hostKeys)
	switch ls := strings.ToLower(server); {
	case ls == ":memory:", strings.HasSuffix(ls, ".db"), strings.HasSuffix(ls, ".sqlite"), strings.HasSuffix(ls, ".sqlite3"):
		return parts{other: "SQLite"}, true, nil
	}
	return parts{}, false, nil
}

// laravelSQLite reads Laravel's SQLite setting of a .env file:
// DB_CONNECTION=sqlite with DB_DATABASE (default database/database.sqlite)
// or DB_URL; reached only when sqliteReady.
func laravelSQLite(file string, env map[string]string) (parts, error) {
	return parts{}, fmt.Errorf("%s: SQLite is not implemented yet", file)
}

// resolveSQLite turns the database file p names, as the framework of r's
// file takes it (relative to the project root, manage.py's folder,
// schema.prisma's, the .csproj's), into p.file: relative to the project
// root with /, inside it. Reached only when sqliteReady.
func resolveSQLite(root string, r Ref, p parts) (parts, error) {
	return parts{}, fmt.Errorf("%s: SQLite is not implemented yet", r.where())
}

// sqliteDSN is the DSN of an SQLite setting (parts.dsn), reached only when
// sqliteReady.
func (p parts) sqliteDSN(where string) (string, error) {
	return "", fmt.Errorf("%s: SQLite is not implemented yet", where)
}
