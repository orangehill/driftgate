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
		// argv is never expanded: a $ outside a shell's script is part of
		// the value; inside one, only a bare reference stays.
		{[]string{"mysql", "-p", "$S3cret"}, `mysql -p ***`},
		{[]string{"env", "DB_PASSWORD=$S3cret", "x"}, `env DB_PASSWORD=*** x`},
		{[]string{"mysql", "-p$S3cret"}, `mysql -p***`},
		{[]string{"sh", "-c", `PGPASSWORD="pa\$\$S3cret" psql -h h`}, `sh -c "PGPASSWORD=*** psql -h h"`},
		{[]string{"sh", "-c", `DB_PASSWORD="S3cret$x" ./seed`}, `sh -c "DB_PASSWORD=*** ./seed"`},
		{[]string{"sh", "-c", `mysql -p'$S3cret'`}, `sh -c "mysql -p***"`},
		{[]string{"sh", "-c", `mysql -p"$(cat S3cret)"`}, `sh -c "mysql -p***"`},
		{[]string{"sh", "-c", `mysql -p"${DB_PASSWORD:-S3cret}"`}, ""},
		{[]string{"sh", "-c", "DB_PASSWORD=$DB_PASS ./seed"}, `sh -c "DB_PASSWORD=$DB_PASS ./seed"`},
		{[]string{"op", "run", "--", "bash", "-lc", "mysql -pS3cret"}, `op run -- bash -lc "mysql -p***"`},
		{[]string{"sh", "-c", `bash -c 'mysql -pS3cret'`}, `sh -c "bash -c \"mysql -p***\""`},
		// Quoted values with a space, glued or not.
		{[]string{"sh", "-c", "sqlcmd -U sa -P'S3 cret' -Q x"}, `sh -c "sqlcmd -U sa -P*** -Q x"`},
		{[]string{"sh", "-c", `mysql -u root -p"S3 cret" x`}, `sh -c "mysql -u root -p*** x"`},
		{[]string{"sh", "-c", "mysql --password 'S3 cret' x"}, `sh -c "mysql --password *** x"`},
		{[]string{"x", `--password="S3 cret"`}, `x --password=***`},
		{[]string{"env", "DB_PASSWORD=S3 cret", "x"}, `env DB_PASSWORD=*** x`},
		{[]string{"x", "Server=h;Password=my S3cret;"}, `x Server=h;Password=***;`},
		{[]string{"sh", "-c", `sqlcmd -C "Server=h;Password=my S3cret;"`}, `sh -c "sqlcmd -C \"Server=h;Password=***;\""`},
		{[]string{"sh", "-c", "PGPASSWORD=S3cret\tpsql"}, ""},
		// Other names of secrets.
		{[]string{"docker", "exec", "r", "redis-cli", "-a", "S3cret", "FLUSHALL"}, `docker exec r redis-cli -a *** FLUSHALL`},
		{[]string{"env", "REDISCLI_AUTH=S3cret", "redis-cli", "FLUSHALL"}, `env REDISCLI_AUTH=*** redis-cli FLUSHALL`},
		{[]string{"x", "--auth", "S3cret"}, `x --auth ***`},
		{[]string{"x", "-H", "Cookie: session=S3cret"}, `x -H "Cookie: ***"`},
		{[]string{"x", "https://h/x?sig=S3cret"}, `x https://h/x?sig=***`},
		{[]string{"x", "https://api/x?api_key=S3cret&b=1"}, `x https://api/x?api_key=***&b=1`},
		{[]string{"x", "--header=Authorization: Bearer S3cret"}, ""},
		{[]string{"sh", "-c", "curl -H 'Authorization:Bearer S3cret' h"}, `sh -c "curl -H \"Authorization:Bearer ***\" h"`},
		{[]string{"curl", "-d", `{"password":"S3cret","name":"a"}`}, `curl -d "{\"password\":***,\"name\":\"a\"}"`},
		{[]string{"x", "DATABASE_URL=mysql://u:S3cret@h/db"}, `x DATABASE_URL=mysql://u:***@h/db`},
		{[]string{"x", "-uuser:S3cret"}, `x -uuser:***`},
		{[]string{"openssl", "-passin", "pass:S3cret"}, `openssl -passin ***`},
		// What only looks like a secret stays.
		{[]string{"php", "artisan", "key:generate"}, `php artisan key:generate`},
		{[]string{"mkdir", "-p", "storage/app"}, `mkdir -p storage/app`},
		{[]string{"sh", "-c", "mkdir -p storage/app && mysql -pS3cret"}, `sh -c "mkdir -p storage/app && mysql -p***"`},
		{[]string{"psql", "-p", "5432", "-h", "h"}, `psql -p 5432 -h h`},
		{[]string{"docker", "run", "-p", "8080:80", "img"}, `docker run -p 8080:80 img`},
		{[]string{"docker", "exec", "db", "mysql", "-p", "1234"}, `docker exec db mysql -p ***`},
		{[]string{"x", "--primary-key", "id"}, `x --primary-key id`},
		// A secret's name, then = or :, then its value as words of their own
		// (a configuration a here-document writes, PowerShell); => of PHP.
		{[]string{"pwsh", "-c", "$env:PGPASSWORD = 'S3cret'; psql"}, `pwsh -c "$env:PGPASSWORD = ***; psql"`},
		{[]string{"pwsh", "-Command", "$Password = 'S3cret'; ./seed.ps1"}, `pwsh -Command "$Password = ***; ./seed.ps1"`},
		{[]string{"bash", "-c", "cat > ~/.my.cnf <<EOF\n[client]\npassword = S3cret\nEOF"}, `bash -c "cat > ~/.my.cnf <<EOF\n[client]\npassword = ***\nEOF"`},
		{[]string{"bash", "-c", "cat > c.yml <<EOF\npassword: S3cret\nEOF"}, `bash -c "cat > c.yml <<EOF\npassword: ***\nEOF"`},
		{[]string{"bash", "-c", "cat > c.json <<'EOF'\n{\n  \"password\": \"S3cret\"\n}\nEOF"}, ""},
		{[]string{"php", "artisan", "tinker", "--execute=User::first()->update(['password' => Hash::make('S3cret')])"}, ""},
		{[]string{"bash", "-c", "php artisan tinker --execute=\"User::factory()->create(['password' => bcrypt('S3cret')])\""}, ""},
		{[]string{"mysql", "-e", "SET PASSWORD FOR 'u'@'%' = PASSWORD('S3cret')"}, `mysql -e "SET PASSWORD FOR 'u'@'%' = PASSWORD(***)"`},
		{[]string{"bash", "-c", "mysql <<EOF\nSET PASSWORD FOR root = 'S3cret';\nEOF"}, ""},
		{[]string{"psql", "-c", "ALTER ROLE app PASSWORD $$S3cret$$"}, `psql -c "ALTER ROLE app PASSWORD ***"`},
		{[]string{"psql", "-c", "ALTER USER app PASSWORD E'S3cret'"}, `psql -c "ALTER USER app PASSWORD E***"`},
		{[]string{"git", "clone", "https://S3cret@github.com/a/b"}, `git clone https://***@github.com/a/b`},
		{[]string{"bash", "-c", `eval "mysql -pS3cret"`}, `bash -c "eval \"mysql -p***\""`},
		{[]string{"script", "-qc", "mysql -pS3cret", "/dev/null"}, `script -qc "mysql -p***" /dev/null`},
		{[]string{"npm", "config", "set", "//registry.npmjs.org/:_authToken", "S3cret"}, `npm config set //registry.npmjs.org/:_authToken ***`},
		// Flags that only look like they take a secret.
		{[]string{"cp", "-p", "a", "b"}, `cp -p a b`},
		{[]string{"cp", "-pr", "a", "b"}, `cp -pr a b`},
		{[]string{"mysql", "--skip-password", "app"}, `mysql --skip-password app`},
		{[]string{"psql", "--no-password", "app"}, `psql --no-password app`},
		{[]string{"pwsh", "-c", "Invoke-Sqlcmd -Password 'S3cret' -Query x"}, `pwsh -c "Invoke-Sqlcmd -Password *** -Query x"`},
		// A command ssh runs, a command substitution, a literal glued to a
		// reference, SQL that sets a password, shells of other kinds.
		{[]string{"ssh", "-p", "22", "host", "mysql -pS3cret db"}, `ssh -p 22 host "mysql -p*** db"`},
		{[]string{"sh", "-c", "ssh h 'sqlcmd -U sa -P S3cret'"}, `sh -c "ssh h \"sqlcmd -U sa -P ***\""`},
		{[]string{"sh", "-c", "x=$(mysql -pS3cret -e 'select 1')"}, ""},
		{[]string{"sh", "-c", "x=`mysql -pS3cret`"}, ""},
		{[]string{"sh", "-c", `mysql -p"$PW"S3cret`}, `sh -c "mysql -p***"`},
		{[]string{"sh", "-c", `mysql -p$PW'S3cret'`}, `sh -c "mysql -p***"`},
		{[]string{"sh", "-c", `mysql -p"$PW"`}, `sh -c "mysql -p\"$PW\""`},
		{[]string{"mysql", "-e", "ALTER USER root IDENTIFIED BY 'S3cret'"}, `mysql -e "ALTER USER root IDENTIFIED BY ***"`},
		{[]string{"sqlcmd", "-Q", "CREATE LOGIN x WITH PASSWORD=N'S3cret'"}, `sqlcmd -Q "CREATE LOGIN x WITH PASSWORD=***"`},
		{[]string{"sh", "-c", "psql <<SQL\nALTER ROLE x PASSWORD 'S3cret';\nSQL"}, ""},
		{[]string{"pwsh", "-Command", "sqlcmd -S h -U sa -P S3cret"}, `pwsh -Command "sqlcmd -S h -U sa -P ***"`},
		{[]string{"cmd", "/c", "mysql -pS3cret"}, `cmd /c "mysql -p***"`},
		{[]string{"redis-cli", "-u", "redis://:S3cret@h:6379"}, `redis-cli -u redis://:***@h:6379`},
		{[]string{"redis-cli", "AUTH", "S3cret"}, `redis-cli AUTH ***`},
		{[]string{"curl", "-b", "sid=S3cret"}, `curl -b ***`},
		{[]string{"curl", "-d", `{"password":"a\"S3cret"}`}, `curl -d "{\"password\":***}"`},
		{[]string{"sqlcmd", "-v", "Pw=S3cret"}, `sqlcmd -v Pw=***`},
		{[]string{"sh", "-c", "echo S3cret | docker login -u u --password-stdin"}, `sh -c "echo *** | docker login -u u --password-stdin"`},
		{[]string{"htpasswd", "-b", "f", "u", "S3cret"}, ""},
		{[]string{"git", "clone", "https://x-access-token:S3cret@github.com/o/r"}, `git clone https://x-access-token:***@github.com/o/r`},
		{[]string{"mongosh", "mongodb://u:S3cret@h/db?authSource=admin"}, `mongosh mongodb://u:***@h/db?authSource=admin`},
		{[]string{"sshpass", "-p", "1234", "ssh", "h"}, `sshpass -p *** ssh h`},
	} {
		got := Line(c.argv)
		if strings.Contains(got, "S3c") || strings.Contains(got, "S3 ") || strings.Contains(got, "cret") || strings.Contains(got, "p@ss") {
			t.Errorf("%q: %s shows a secret", c.argv, got)
		} else if c.want != "" && got != c.want {
			t.Errorf("%q: %s, want %s", c.argv, got, c.want)
		}
	}
}
