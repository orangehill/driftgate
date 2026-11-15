package cli

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/appconf"
	"github.com/orangehill/driftgate/internal/config"
	"github.com/orangehill/driftgate/internal/state"
)

// The tests that need a server take it from DRIFTGATE_TEST_SQLSERVER, a
// sqlserver:// URL of a login that may create logins and databases (sa in
// CI). They reach it through the "sqlserver" driver the state package
// registers; their databases and logins are named dgt_cli_*.

func sqlServerTestDSN(t *testing.T) *url.URL {
	t.Helper()
	v := os.Getenv("DRIFTGATE_TEST_SQLSERVER")
	if v == "" {
		t.Skip("DRIFTGATE_TEST_SQLSERVER not set")
	}
	u, err := url.Parse(v)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// sqlServerOpen opens database db ("" is master) of the server of u as
// the login user (u's own when "").
func sqlServerOpen(t *testing.T, u *url.URL, db, user, password string) *sql.DB {
	t.Helper()
	c := *u
	if user != "" {
		c.User = url.UserPassword(user, password)
	}
	q := c.Query()
	if db == "" {
		db = "master"
	}
	q.Set("database", db)
	c.RawQuery = q.Encode()
	state.AllowNegativeSerial() // as NewSQLServer: SQL Server 2017's own certificate
	d, err := sql.Open("sqlserver", c.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func sqlServerName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "dgt_cli_" + hex.EncodeToString(b)
}

func sqlServerExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if err := state.RetryInTransition(func() error { _, err := db.Exec(s); return err }); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// sqlServerLogin makes the database work with a table and the SQL login
// login without any role (no user in work either); both are removed at
// the end, with the copies and backup files a seed of work leaves.
func sqlServerLogin(t *testing.T, master *sql.DB, work, login, password string) {
	t.Helper()
	sqlServerExec(t, master, "CREATE DATABASE "+qsName(work),
		"CREATE LOGIN "+qsName(login)+" WITH PASSWORD = "+sqlString(password)+", CHECK_POLICY = OFF",
		"CREATE TABLE "+qsName(work)+".dbo.items (id int IDENTITY PRIMARY KEY, name nvarchar(40) NOT NULL)",
		"INSERT INTO "+qsName(work)+".dbo.items (name) VALUES (N'a'), (N'b')")
	t.Cleanup(func() {
		// The backup files a seed wrote, as the history of the server
		// knows them (deleting a file needs sysadmin, as the test login is).
		rows, err := master.Query(`SELECT DISTINCT f.physical_device_name FROM msdb.dbo.backupset s
			JOIN msdb.dbo.backupmediafamily f ON f.media_set_id = s.media_set_id WHERE s.database_name LIKE @p1`, work+"%")
		if err == nil {
			var files []string
			for rows.Next() {
				var f string
				if rows.Scan(&f) == nil {
					files = append(files, f)
				}
			}
			rows.Close()
			for _, f := range files {
				master.Exec("EXEC master.sys.xp_delete_file 0, " + sqlString(f))
			}
		}
		for _, db := range []string{work, work + "_pristine", work + "_pristine_dgnew", work + "_pristine_dgx", work + "_owned"} {
			master.Exec("IF DB_ID(" + sqlString(db) + ") IS NOT NULL BEGIN ALTER DATABASE " + qsName(db) +
				" SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE " + qsName(db) + " END")
		}
		master.Exec(`DECLARE @k nvarchar(max) = N''; SELECT @k += N'KILL ' + CAST(session_id AS nvarchar(10)) + N'; '
			FROM sys.dm_exec_sessions WHERE login_name = @p1; EXEC (@k)`, login)
		master.Exec("DROP LOGIN " + qsName(login))
	})
}

// aspnetSQLServer is an ASP.NET Core project on Microsoft.Data.SqlClient
// whose connection string names work on the server of u as login (the
// password quoted, as ADO.NET quotes a value with ;).
func aspnetSQLServer(u *url.URL, work, login, password string) map[string]string {
	host, port := u.Hostname(), u.Port()
	return map[string]string{
		"src/Shop.Api/Shop.Api.csproj": `<Project Sdk="Microsoft.NET.Sdk.Web"><ItemGroup><PackageReference Include="Microsoft.Data.SqlClient" Version="5.2.0" /></ItemGroup></Project>`,
		"src/Shop.Api/appsettings.json": `{"ConnectionStrings": {"Default": "Server=` + host + `,` + port + `;Database=` + work + `;User Id=` + login +
			`;Password='` + strings.ReplaceAll(password, "'", "''") + `';Encrypt=True;TrustServerCertificate=True"}}`,
	}
}

// TestSetupSQLServerGrant: setup on an ASP.NET Core application whose
// login may not create databases proposes the grant, and the seed it
// starts fails with the permission error and the grant again. Once an
// administrator ran the grant, as printed, the next setup makes the
// pristine copy (owned by the login, read-only) and goes on. The grant
// may run again; on a database the login owns it adds nothing. The
// optional grant gives the login the server's performance state.
func TestSetupSQLServerGrant(t *testing.T) {
	u := sqlServerTestDSN(t)
	master := sqlServerOpen(t, u, "", "", "")
	work, login, password := sqlServerName(), sqlServerName(), "S3cr;et@{pw}-Dg1"
	sqlServerLogin(t, master, work, login, password)
	root := setupDir(t, aspnetSQLServer(u, work, login, password))
	appURL := "http://127.0.0.1:" + closedPort(t)

	code, out, errOut := runCLI(t, "setup", "--yes", "--url", appURL)
	g := sqlServerGrant(config.Component{Driver: config.SQLServer, Working: work, Pristine: work + "_pristine"}, login)
	if code != ExitEnv || !strings.Contains(out, "state: db sqlserver "+work+" on "+u.Hostname()+":"+u.Port()+", user "+login) ||
		!strings.Contains(out, "grant it (as an administrator): "+g) || !strings.Contains(out, "optional, for restores that skip unchanged data") ||
		!strings.Contains(out, "from a COPY_ONLY backup of "+work) ||
		!strings.Contains(errOut, "(4063)") || !strings.Contains(errOut, "the user may not create the pristine database; grant it (as an administrator): "+g) {
		t.Fatalf("setup without the grant: %d\n%s\n%s", code, out, errOut)
	}
	noSecret(t, root, out, errOut)
	if _, c := loadState(t, root); c.Driver != config.SQLServer || c.Working != work || c.Pristine != work+"_pristine" ||
		c.DSN != (config.Secret{AppSettings: "src/Shop.Api/Shop.Api.csproj", Key: "ConnectionStrings:Default"}) {
		t.Errorf("state db: %+v", c)
	}

	// A user in the working database is not enough: creating the copy
	// needs dbcreator.
	sqlServerExec(t, master, "USE "+qsName(work)+"; CREATE USER "+qsName(login)+" FOR LOGIN "+qsName(login)+"; ALTER ROLE [db_owner] ADD MEMBER "+qsName(login)+";")
	code, out, errOut = runCLI(t, "setup", "--yes", "--url", appURL)
	if code != ExitEnv || !strings.Contains(errOut, "permission denied") || !strings.Contains(errOut, "grant it (as an administrator): "+g) {
		t.Fatalf("setup without dbcreator: %d\n%s\n%s", code, out, errOut)
	}

	// The administrator runs the grant, twice: it may run again.
	sqlServerExec(t, master, g, g)
	code, out, errOut = runCLI(t, "setup", "--yes", "--url", appURL)
	if code != ExitEnv || !strings.Contains(out, "pristine copy of db: ") || !strings.Contains(errOut, "the application does not answer at "+appURL) {
		t.Fatalf("setup after the grant: %d\n%s\n%s", code, out, errOut)
	}
	var owner string
	var readOnly bool
	if err := master.QueryRow("SELECT SUSER_SNAME(owner_sid), is_read_only FROM sys.databases WHERE name = @p1", work+"_pristine").Scan(&owner, &readOnly); err != nil || owner != login || !readOnly {
		t.Errorf("pristine copy: owner %q, read-only %v, %v", owner, readOnly, err)
	}
	if code, out, errOut = runCLI(t, "setup", "--yes", "--url", appURL); !strings.Contains(out, "the pristine copy is there already; kept") || strings.Contains(out, "grant it") {
		t.Errorf("setup with the pristine copy made: %d\n%s\n%s", code, out, errOut)
	}

	// On a database the login owns, it is dbo: no user, no role to add.
	owned := work + "_owned"
	sqlServerExec(t, master, "CREATE DATABASE "+qsName(owned), "ALTER AUTHORIZATION ON DATABASE::"+qsName(owned)+" TO "+qsName(login))
	og := sqlServerGrant(config.Component{Driver: config.SQLServer, Working: owned}, login)
	sqlServerExec(t, master, og, og)

	// The optional grant: the statement of the server's version.
	var major int
	if err := master.QueryRow("SELECT CAST(SERVERPROPERTY('ProductMajorVersion') AS int)").Scan(&major); err != nil {
		t.Fatal(err)
	}
	perm, fast := "VIEW SERVER STATE", sqlServerFast(login)
	if major >= 16 {
		perm = "VIEW SERVER PERFORMANCE STATE"
	}
	stmt := "GRANT " + perm + " TO " + qsName(login) + ";"
	if !strings.Contains(fast, stmt) {
		t.Fatalf("%s: no %s", fast, stmt)
	}
	sqlServerExec(t, master, stmt)
	var has int
	if err := sqlServerOpen(t, u, work, login, password).QueryRow("SELECT HAS_PERMS_BY_NAME(NULL, NULL, @p1)", perm).Scan(&has); err != nil || has != 1 {
		t.Errorf("%s after the grant: %d %v", perm, has, err)
	}
}

// TestSetupSQLServerMessages: without a server, setup on SQL Server
// settings: the driver comes from the setting, the TLS note is shown for
// the user to check, a setting DriftGate refuses says why without its
// password, and sa of a compose service is taken first and needs no grant.
func TestSetupSQLServerMessages(t *testing.T) {
	port := closedPort(t)
	appURL := "http://127.0.0.1:" + closedPort(t)
	csproj := `<Project Sdk="Microsoft.NET.Sdk.Web"></Project>` // no client package: its Encrypt default is not known
	settings := func(cs string) map[string]string {
		return map[string]string{"src/Shop.Api/Shop.Api.csproj": csproj,
			"src/Shop.Api/appsettings.json": `{"ConnectionStrings": {"Default": "` + cs + `"}}`}
	}

	// Encrypt left to the client: DriftGate verifies the certificate, and
	// says how to trust a self-signed one.
	root := setupDir(t, settings("Server=127.0.0.1,"+port+";Database=Shop;User Id=shop;Password=S3cr-x"))
	code, out, errOut := runCLI(t, "setup", "--yes", "--url", appURL)
	if code != ExitEnv || !strings.Contains(out, "state: db sqlserver Shop on 127.0.0.1:"+port+", user shop") ||
		!strings.Contains(out, "grant it (as an administrator): USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop]; USE [Shop];") ||
		!strings.Contains(out, "narrower, instead of the grant above: USE [master]; GRANT CREATE ANY DATABASE TO [shop]; ALTER AUTHORIZATION ON DATABASE::[Shop] TO [shop];") ||
		!strings.Contains(out, "check by hand:") || !strings.Contains(out, "verifies the server's certificate") {
		t.Fatalf("setup: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); c.Driver != config.SQLServer || c.Working != "Shop" || c.Pristine != "Shop_pristine" {
		t.Errorf("state db: %+v", c)
	}
	noSecret(t, root, out, errOut)

	// A Windows login is refused with why, and setup stops before state:.
	root = setupDir(t, settings("Server=127.0.0.1,"+port+";Database=Shop;Integrated Security=true;Password=S3cr-x"))
	code, out, errOut = runCLI(t, "setup", "--yes", "--url", appURL)
	if !strings.Contains(out+errOut, "DriftGate signs in with a SQL Server login only") {
		t.Errorf("setup on a Windows login: %d\n%s\n%s", code, out, errOut)
	}
	noSecret(t, root, out, errOut)

	// sa of the compose service the setting names comes first: it can
	// create the pristine copy, so no grant is proposed.
	files := settings("Server=localhost," + port + ";Database=Shop;User Id=app;Password=S3cr-x;TrustServerCertificate=True")
	files["compose.yaml"] = "services:\n  db:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    environment:\n      ACCEPT_EULA: \"Y\"\n      MSSQL_SA_PASSWORD: S3cr-sa\n    ports:\n      - \"" + port + ":1433\"\n"
	root = setupDir(t, files)
	code, out, errOut = runCLI(t, "setup", "--yes", "--url", appURL)
	if !strings.Contains(out, "user sa (compose.yaml, service db)") || !strings.Contains(out, "its user can create the pristine copy") || strings.Contains(out, "grant it") ||
		!strings.Contains(out, "DriftGate trusts the container's own certificate on this machine") {
		t.Errorf("setup with a compose service: %d\n%s\n%s", code, out, errOut)
	}
	if _, c := loadState(t, root); c.Driver != config.SQLServer || c.DSN.Compose == "" {
		t.Errorf("state db: %+v", c)
	}
	noSecret(t, root, out, errOut)
}

// TestSQLServerGrantText: the grant names the login and the working
// database quoted, as an administrator pastes it; sa needs none.
func TestSQLServerGrantText(t *testing.T) {
	c := config.Component{Driver: config.SQLServer, Working: "Sh]op", Pristine: "Sh]op_pristine"}
	g := grant(c, "o'neil]x")
	for _, want := range []string{"USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [o'neil]]x]; USE [Sh]]op];",
		"IF SUSER_SID(N'o''neil]x') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [o'neil]]x] FOR LOGIN [o'neil]]x];",
		"ALTER ROLE [db_owner] ADD MEMBER [o'neil]]x];"} {
		if !strings.Contains(g, want) {
			t.Errorf("grant without %q:\n%s", want, g)
		}
	}
	for _, sa := range []string{"sa", "SA"} {
		if g := grant(c, sa); g != "" {
			t.Errorf("grant for %s: %q", sa, g)
		}
	}
	if g := grant(c, ""); !strings.Contains(g, "[<user>]") {
		t.Errorf("grant without a user: %q", g)
	}
	if n := sqlServerNarrow(c, "o'neil]x"); !strings.Contains(n, "USE [master]; GRANT CREATE ANY DATABASE TO [o'neil]]x]; ALTER AUTHORIZATION ON DATABASE::[Sh]]op] TO [o'neil]]x];") ||
		!strings.Contains(n, "USE [Sh]]op]; DROP USER [o'neil]]x];") || !strings.Contains(n, "restore over it or drop it") {
		t.Errorf("narrow grant: %q", n)
	}
	if f := sqlServerFast(""); !strings.Contains(f, "GRANT VIEW SERVER PERFORMANCE STATE TO [<user>];") || !strings.Contains(f, "GRANT VIEW SERVER STATE TO [<user>];") {
		t.Errorf("fast: %q", f)
	}
}

// TestSQLServerDescribe: a SQL Server setting is described by its
// database, server and login; the local server forms are one server;
// state: without a setting names a sqlserver:// DSN.
func TestSQLServerDescribe(t *testing.T) {
	d := appconf.Database{Driver: appconf.SQLServer, Name: "Shop", Host: "127.0.0.1", Port: "1433", User: "app",
		File: "src/Shop.Api/appsettings.json", Key: "ConnectionStrings:Default", Ref: appconf.Ref{Source: appconf.AppSettings, File: "src/Shop.Api/appsettings.json", Key: "ConnectionStrings:Default"}}
	if got := describe(d); got != "sqlserver Shop on 127.0.0.1:1433, user app (src/Shop.Api/appsettings.json ConnectionStrings:Default)" {
		t.Errorf("describe: %s", got)
	}
	local := d
	local.Host, local.Port = "localhost", ""
	if server(local) != server(d) {
		t.Errorf("%s and %s are one server", server(local), server(d))
	}
	block := stateBlock(d, true, appconf.Tables{})
	for _, want := range []string{"    driver: sqlserver\n", "    working: Shop\n", "    pristine: Shop_pristine   #"} {
		if !strings.Contains(block, want) {
			t.Errorf("state block without %q:\n%s", want, block)
		}
	}
	d.Ref = appconf.Ref{}
	if block = stateBlock(d, true, appconf.Tables{}); !strings.Contains(block, "sqlserver://user:pass@host:1433?database=Shop") {
		t.Errorf("state block without a setting:\n%s", block)
	}
}

// TestSetupSQLServerWorktree: a worktree whose settings name the main
// worktree's SQL Server database, in any case, is refused: SQL Server
// takes database names case-insensitively. One of its own is taken.
func TestSetupSQLServerWorktree(t *testing.T) {
	port := closedPort(t)
	settings := func(db string) string {
		return `{"ConnectionStrings": {"Default": "Server=127.0.0.1,` + port + `;Database=` + db + `;User Id=app;Password=S3cr-x;TrustServerCertificate=True"}}`
	}
	main := setupDir(t, map[string]string{".gitignore": ".driftgate/\nappsettings.json\n",
		"Shop.Api.csproj":  `<Project Sdk="Microsoft.NET.Sdk.Web"><ItemGroup><PackageReference Include="Microsoft.Data.SqlClient" Version="5.2.0" /></ItemGroup></Project>`,
		"appsettings.json": settings("Shop")})
	write(t, filepath.Join(main, ".driftgate", "driftgate.yaml"), `version: 1
project: shop
baselines: local
target:
  base_url: http://127.0.0.1:1
state:
  db:
    driver: sqlserver
    dsn: {appsettings: Shop.Api.csproj, key: 'ConnectionStrings:Default'}
    working: Shop
    pristine: Shop_pristine
`)
	write(t, filepath.Join(main, ".driftgate", "baselines", "local", "acceptances.jsonl"), "")

	wt := worktree(t, main, map[string]string{"appsettings.json": settings("SHOP")})
	code, out, errOut := runCLI(t, "setup", "--yes")
	if code != ExitConfig || !strings.Contains(errOut, "this worktree's settings name the main worktree's database Shop") {
		t.Fatalf("setup on the main worktree's database: %d\n%s\n%s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(wt, ".driftgate", "driftgate.yaml")); err == nil {
		t.Error("the project was taken on the main worktree's database")
	}

	write(t, filepath.Join(wt, "appsettings.json"), settings("Shop_wt"))
	code, out, errOut = runCLI(t, "setup", "--yes")
	if !strings.Contains(out, "took .driftgate") || !strings.Contains(out, "state db: working Shop_wt, pristine Shop_wt_pristine") {
		t.Fatalf("setup on a database of its own: %d\n%s\n%s", code, out, errOut)
	}
}

// TestSQLServerDenied: the grant is proposed for SQL Server's permission
// errors and a login without a user in the working database, not for a
// failed login, a missing server or a refusal of DriftGate's.
func TestSQLServerDenied(t *testing.T) {
	for msg, want := range map[string]bool{
		"seed of db failed: state db: restore of Shop as Shop_pristine_dgnew: mssql: CREATE DATABASE permission denied in database 'master'. (262)":       true,
		"seed of db failed: state db: backup of Shop to /var/opt/mssql/data/x.bak: mssql: BACKUP DATABASE permission denied in database 'Shop'. (262)":    true,
		`seed of db failed: mssql: Cannot open database "Shop" that was requested by the login. Using the user default database "master" instead. (4063)`: true,
		`seed of db failed: mssql: The server principal "app" is not able to access the database "Shop" under the current security context. (916)`:        true,
		"seed of db failed: mssql: User does not have permission to RESTORE database 'Shop'. (3110)":                                                      true,
		"seed of db failed: mssql: Login failed for user 'app'. (18456)":                                                                                  false,
		"seed of db failed: mssql: VIEW SERVER PERFORMANCE STATE permission was denied on object 'server', database 'master'. (300)":                      false,
		"seed of db failed: unable to open tcp connection with host '127.0.0.1:1433': dial tcp 127.0.0.1:1433: connect: connection refused":               false,
		"seed of db failed: state db: working database Shop does not exist":                                                                               false,
	} {
		if denied().MatchString(msg) != want {
			t.Errorf("%s: %v", msg, !want)
		}
	}
}
