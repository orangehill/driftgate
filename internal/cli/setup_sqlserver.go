package cli

import (
	"fmt"
	"strings"

	"github.com/orangehill/driftgate/internal/config"
)

// sqlServerGrant is the statement that lets the login user create the
// pristine copy of the SQL Server component c (grant): the server role
// dbcreator, which makes the creator the owner of the copy.
//
// Placeholder of the SQL Server setup, which completes it (db_owner in
// working, describe, messages).
func sqlServerGrant(c config.Component, user string) string {
	return fmt.Sprintf("ALTER SERVER ROLE [dbcreator] ADD MEMBER [%s];", strings.ReplaceAll(user, "]", "]]"))
}
