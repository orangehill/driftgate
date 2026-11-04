package procx

import (
	"strings"
	"testing"
)

// TestLine: every password, secret, token or key of a command is ***, in
// its own argument or inside sh -c; a reference to a variable stays, and
// so does what is not a secret.
func TestLine(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string // "" when only no secret may show
	}{
		{[]string{"sh", "-c", `gunzip -c dump.sql.gz | mysql "app"`, ""}, `sh -c "gunzip -c dump.sql.gz | mysql \"app\"" ""`},
		{[]string{"php", "artisan", "migrate:fresh", "--seed", "--force"}, `php artisan migrate:fresh --seed --force`},
		{[]string{"mysql", "-u", "root", "-p", "S3cret"}, `mysql -u root -p ***`},
		{[]string{"mysql", "-uroot", "-pS3cret", "app"}, `mysql -uroot -p*** app`},
		{[]string{"mysql", "--user=root", "--password", "S3cret"}, `mysql --user=root --password ***`},
		{[]string{"mysql", "-p", "-S3cret"}, `mysql -p ***`},
		{[]string{"mysql", "--password=-S3cret"}, `mysql --password=***`},
		{[]string{"mysql", "-p-S3cret"}, `mysql -p***`},
		{[]string{"psql", "postgresql://u:S3cret@h/db"}, `psql postgresql://u:***@h/db`},
		{[]string{"psql", "postgres://u:p@ss@h/db"}, `psql postgres://u:***@h/db`},
		{[]string{"psql", "postgres://u:S3c/ret@h/db"}, `psql postgres://u:***@h/db`},
		{[]string{"psql", "host=h password=S3cret dbname=x"}, `psql "host=h password=*** dbname=x"`},
		{[]string{"psql", "host=h password='S3 cret' dbname=x"}, ""},
		{[]string{"sh", "-c", "PGPASSWORD=S3cret psql -h h"}, `sh -c "PGPASSWORD=*** psql -h h"`},
		{[]string{"sh", "-c", "mysql -u root -pS3cret < dump.sql"}, `sh -c "mysql -u root -p*** < dump.sql"`},
		{[]string{"sh", "-c", "mysql -u root --password='S3 cret' < dump.sql"}, ""},
		{[]string{"sh", "-c", `mysql -u root --password="S3cret" < dump.sql`}, ""},
		{[]string{"env", "DB_PASSWORD=S3cret", "php", "artisan", "migrate"}, `env DB_PASSWORD=*** php artisan migrate`},
		{[]string{"env", "MYSQL_PWD=S3cret", "mysql"}, `env MYSQL_PWD=*** mysql`},
		{[]string{"env", "API_KEY=S3cret", "x"}, `env API_KEY=*** x`},
		{[]string{"env", "AWS_SECRET_ACCESS_KEY=S3cret", "x"}, `env AWS_SECRET_ACCESS_KEY=*** x`},
		{[]string{"sqlcmd", "-S", "h", "-U", "sa", "-P", "S3cret"}, `sqlcmd -S h -U sa -P ***`},
		{[]string{"sqlcmd", "-S", "h", "-U", "sa", "-PS3cret"}, `sqlcmd -S h -U sa -P***`},
		{[]string{"sh", "-c", "sqlcmd -S db -U sa -P S3cret -Q x"}, `sh -c "sqlcmd -S db -U sa -P *** -Q x"`},
		{[]string{"x", "Server=h;User Id=sa;Password=S3cret;"}, `x "Server=h;User Id=sa;Password=***;"`},
		{[]string{"x", "Server=h;User Id=sa;Pwd={S3;cret};"}, ""},
		{[]string{"x", "sqlserver://sa:S3cret@h:1433?database=x"}, `x sqlserver://sa:***@h:1433?database=x`},
		{[]string{"x", "sqlserver://h?user+id=sa&password=S3cret"}, `x sqlserver://h?user+id=sa&password=***`},
		{[]string{"curl", "-u", "user:S3cret", "http://h"}, `curl -u user:*** http://h`},
		{[]string{"sh", "-c", "curl -u user:S3cret http://h"}, `sh -c "curl -u user:*** http://h"`},
		{[]string{"curl", "-H", "Authorization: Bearer S3cret", "http://h"}, `curl -H "Authorization: Bearer ***" http://h`},
		{[]string{"mysqldump", "--password=S3cret"}, `mysqldump --password=***`},
		{[]string{"redis-cli", "-a", "S3cret"}, `redis-cli -a ***`},
		{[]string{"sh", "-c", "redis-cli -h r -a S3cret FLUSHALL"}, `sh -c "redis-cli -h r -a *** FLUSHALL"`},
		{[]string{"x", "--db-password", "S3cret"}, `x --db-password ***`},
		{[]string{"x", "--token", "S3cret"}, `x --token ***`},
		{[]string{"x", "--passphrase=S3cret"}, `x --passphrase=***`},
		{[]string{"sh", "-c", "export DB_PASS=S3cret; ./seed"}, `sh -c "export DB_PASS=***; ./seed"`},
		{[]string{"sh", "-c", "mysql -p$DB_PASS"}, `sh -c "mysql -p$DB_PASS"`},
		{[]string{"sh", "-c", `sqlcmd -U sa -P "$MSSQL_SA_PASSWORD"`}, `sh -c "sqlcmd -U sa -P \"$MSSQL_SA_PASSWORD\""`},
		{[]string{"x", "mysql://root:S3cret@h/db"}, `x mysql://root:***@h/db`},
		{[]string{"ls", "-a", "dir"}, `ls -a dir`},
	} {
		got := Line(c.argv)
		if strings.Contains(got, "S3c") || strings.Contains(got, "S3 ") || strings.Contains(got, "cret") || strings.Contains(got, "p@ss") {
			t.Errorf("%q: %s shows a secret", c.argv, got)
		} else if c.want != "" && got != c.want {
			t.Errorf("%q: %s, want %s", c.argv, got, c.want)
		}
	}
}
