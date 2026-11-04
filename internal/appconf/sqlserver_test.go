package appconf

import (
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orangehill/driftgate/internal/dbdsn"
)

// TestSQLServerForms: every form a SQL Server client writes gives
// go-mssqldb's URL with the address, the login and the client's TLS.
func TestSQLServerForms(t *testing.T) {
	for _, tt := range []struct{ in, dsn, err string }{
		// ADO.NET (Microsoft.Data.SqlClient): its client is read from the
		// project, so here the default is unknown: encrypt and verify.
		{"Server=tcp:db.prod,1433;Initial Catalog=Shop;User ID=app;Password=p;Encrypt=True;TrustServerCertificate=False", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		{"Server=db.prod;Database=Shop;User Id=app;Password=p", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		{"Data Source=db.prod,14331;Initial Catalog=Shop;UID=app;PWD=p;Encrypt=Strict;TrustServerCertificate=true", "sqlserver://app:p@db.prod:14331?encrypt=strict", ""},
		{"Server=db.prod\\SHOP,1533;Database=Shop;User Id=app;Password=p;Encrypt=false", "sqlserver://app:p@db.prod:1533?encrypt=false", ""},
		{"Server=localhost,1433;Database=Shop;User Id=sa;Password=p;Encrypt=Optional;TrustServerCertificate=yes", "sqlserver://sa:p@localhost:1433?TrustServerCertificate=true&encrypt=false", ""},
		{"Server=.;Database=Shop;User Id=sa;Password=p;MultipleActiveResultSets=true", "sqlserver://sa:p@127.0.0.1:1433?encrypt=true", ""},
		{"Server=(local);Database=Shop;User Id=sa;Password=p;Trusted_Connection=False", "sqlserver://sa:p@127.0.0.1:1433?encrypt=true", ""},
		{"Server=[::1],1433;Database=Shop;User Id=sa;Password=p;Encrypt=yes", "sqlserver://sa:p@[::1]:1433?encrypt=true", ""},
		{"Server=db.prod,1433;User Id=app;Password=p;HostNameInCertificate=db.example.com", "sqlserver://app:p@db.prod:1433?encrypt=true&hostNameInCertificate=db.example.com", ""},
		{"Server=db.prod,1433;User Id=app;Password=p;Encrypt=Strict;ServerCertificate=/etc/db.pem", "sqlserver://app:p@db.prod:1433?encrypt=strict&serverCertificate=%2Fetc%2Fdb.pem", ""},
		{"Server=db.prod,1433;User Id=app;Password=p;Authentication=SqlPassword;Application Name=shop;Connect Timeout=5", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		// ODBC: the driver's default.
		{"Driver={ODBC Driver 18 for SQL Server};Server=tcp:db.prod,1433;Database=Shop;Uid=app;Pwd=p", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		{"Driver={ODBC Driver 17 for SQL Server};Server=db.prod;Database=Shop;Uid=app;Pwd=p", "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", ""},
		{"Driver={ODBC Driver 17 for SQL Server};Server=db.prod;Uid=app;Pwd={p;}}q};Encrypt=yes;TrustServerCertificate=yes", "sqlserver://app:p;%7Dq@db.prod:1433?TrustServerCertificate=true&encrypt=true", ""},
		{"Driver=FreeTDS;Server=db.prod;Port=1533;Uid=app;Pwd=p", "sqlserver://app:p@db.prod:1533?encrypt=true", ""}, // freetds.conf may ask for TLS
		{"Driver=FreeTDS;Server=db.prod;Port=1533;Uid=app;Pwd=p;Encryption=require", "sqlserver://app:p@db.prod:1533?TrustServerCertificate=true&encrypt=true", ""},
		{"Driver=FreeTDS;Server=db.prod;Uid=app;Pwd=p;Encryption=request", "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", ""},
		{"Driver={ODBC Driver 18 for SQL Server};Server=db.prod;Uid=app;Pwd=p;Encrypt=no", "sqlserver://app:p@db.prod:1433?encrypt=false", ""},
		{"odbc:server=db.prod;user id=app;password={a;b};encrypt=true", "sqlserver://app:a;b@db.prod:1433?encrypt=true", ""},
		// JDBC and Prisma: encrypt and verify by default.
		{"jdbc:sqlserver://db.prod:1433;databaseName=Shop;user=app;password={p;w}}};trustServerCertificate=true", "sqlserver://app:p;w%7D@db.prod:1433?TrustServerCertificate=true&encrypt=true", ""},
		{"jdbc:sqlserver://db.prod\\SHOP:1533;databaseName=Shop;user=app;password=p;encrypt=false", "sqlserver://app:p@db.prod:1533?encrypt=false", ""},
		{"jdbc:sqlserver://;serverName=db.prod;portNumber=1533;user=app;password=p;hostNameInCertificate=*.example.com;sslProtocol=TLSv1.3", "sqlserver://app:p@db.prod:1533?encrypt=true&hostNameInCertificate=%2A.example.com&tlsmin=1.3", ""},
		{"sqlserver://localhost:1433;database=Shop;user=SA;password=p;encrypt=true;trustServerCertificate=true", "sqlserver://SA:p@localhost:1433?TrustServerCertificate=true&encrypt=true", ""},
		{"sqlserver://db.prod:1433;database=Shop;user=app;password=p;encrypt=DANGER_PLAINTEXT", "sqlserver://app:p@db.prod:1433?encrypt=false", ""},
		{"sqlserver://db.prod:1433;database=Shop;user=app;password=p;trustServerCertificateCA=/ca.pem", "sqlserver://app:p@db.prod:1433?certificate=%2Fca.pem&encrypt=true", ""},
		// go-mssqldb's URL of the environment is written for DriftGate:
		// without encrypt it is encrypted and verified, not taken with the
		// client's default (which encrypts only the login, unverified).
		{"sqlserver://app:p@db.prod:1433?database=Shop", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		{"sqlserver://app:p@db.prod:1433?database=Shop&TrustServerCertificate=true", "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=true", ""},
		{"sqlserver://app:p@db.prod:1433?database=Shop&encrypt=false", "sqlserver://app:p@db.prod:1433?encrypt=false", ""},
		{"sqlserver://app:p%40q@db.prod/SHOP?database=Shop&port=1533&encrypt=true&TrustServerCertificate=true&app+name=x", "sqlserver://app:p%40q@db.prod:1533?TrustServerCertificate=true&encrypt=true", ""},
		{"sqlserver://app:p@db.prod?database=Shop&encrypt=strict&certificate=/ca.pem", "sqlserver://app:p@db.prod:1433?certificate=%2Fca.pem&encrypt=strict", ""},
		// Prisma's and go-mssqldb's plaintext: DriftGate still encrypts the login.
		{"sqlserver://app:p@db.prod:1433?encrypt=disable", "sqlserver://app:p@db.prod:1433?encrypt=false", ""},
		// Encrypt false with a certificate to verify the login with.
		{"Server=db,1433;User Id=app;Password=p;Encrypt=false;ServerCertificate=/s.pem", "sqlserver://app:p@db:1433?encrypt=false&serverCertificate=%2Fs.pem", ""},
		// Laravel's and Node's URLs: the path is the database.
		{"sqlsrv://app:p@db.prod:1433/shop?encrypt=no&trust_server_certificate=true", "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", ""},
		{"mssql://app:p@db.prod/shop", "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		// Refused.
		{"Server=db;Database=x;Integrated Security=SSPI", "", "Windows account"},
		{"Server=db,1433;Database=x;Trusted_Connection=yes", "", "Windows account"},
		{"Server=db,1433;User Id=CORP\\app;Password=p", "", "DOMAIN\\user"},
		{"Server=db,1433;User Id=app;Password=p;Authentication=ActiveDirectoryPassword", "", "Microsoft Entra ID"},
		{"Server=db,1433;User Id=app;Password=p;Authentication=Something", "", "names an Authentication"},
		{"Server=db,1433;User Id=app;Password=p;Column Encryption Setting=Enabled", "", "Always Encrypted"},
		{"Server=db,1433;User Id=app;Password=p;ApplicationIntent=ReadOnly", "", "read-only replica"},
		{"Server=db,1433;Failover Partner=db2;User Id=app;Password=p", "", "Failover Partner"},
		{"Server=np:\\\\db\\pipe\\sql\\query;User Id=app;Password=p", "", "named pipe"},
		{"Server=lpc:db;User Id=app;Password=p", "", "named pipe"},
		{"Server=(localdb)\\MSSQLLocalDB;User Id=app;Password=p", "", "LocalDB"},
		{"Server=.\\SQLEXPRESS;AttachDbFilename=|DataDirectory|\\app.mdf;User Id=app;Password=p", "", "attaches a database file"},
		{"Server=db\\SHOP;User Id=app;Password=p", "", "names an instance without its port"},
		{"Server=db,1433;User Id=app;Password=p;Encrypt=Sometimes", "", "the SQL Server setting Encrypt has a value DriftGate does not know"},
		{"Server=db,1433;User Id=app;Password=p;Network Library=dbnmpntw", "", "named pipe"},
		{"Server=db,99999;User Id=app;Password=p", "", "the port is not a number"},
		{"Server=db,1433;Database=Shop", "", "names no user"},
		{"Server=db,1433;User Id=app;Password=p;ServerCertificate=/s.pem;HostNameInCertificate=x", "", "ServerCertificate"},
		{"jdbc:sqlserver://db:1433;user=app;password=p;integratedSecurity=true", "", "Windows account"},
		{"jdbc:sqlserver://db:1433;user=app;password=p;authenticationScheme=JavaKerberos", "", "Kerberos"},
		{"jdbc:sqlserver://db:1433;user=app;password=p;trustStore=/ks.jks", "", "trust store"},
		{"jdbc:sqlserver://db\\SHOP;user=app;password=p", "", "without its port"},
		{"jdbc:sqlserver://db:1433;user=app;password={unterminated", "", "no closing }"},
		{"sqlserver://app:p@db.prod/SHOP?database=Shop", "", "without its port"},
		{"sqlserver://app:p@db.prod/Shop", "", "add the port"},
		{"sqlserver://app:p@db.prod:1433?database=Shop&failoverpartner=db2", "", "Failover Partner"},
		{"sqlserver://app:p@db.prod:1433?protocol=np", "", "named pipe"},
		{"sqlserver://app:p@db.prod:1433?database=a&Database=b", "", "gives a parameter twice"},
		{"sqlserver://app:p@db.prod:1433?encrypt=true;x=y", "", "holds a ;"},
		{"sqlsrv://app:p@db.prod/shop?encrypt=yes;x=y", "", "holds a ;"},
		{"sqlserver://app:p@db.prod:1433?frobnicate=1", "", "a parameter of the sqlserver:// URL is not supported"},
		{"Driver={ODBC Driver 18 for SQL Server};DSN=shop;Uid=app;Pwd=p", "", "ODBC data source"},
		{"Driver={ODBC Driver 18 for SQL Server};Server=db,1433;Uid=app;Pwd=p;Encrypt=yes;encrypt=no", "", "a key is given twice"},
		{"jdbc:sqlserver://db:1433;user=app;password=p;encrypt=true;Encrypt=false", "", "a key is given twice"},
		{"Driver={ODBC Driver 18 for SQL Server};Server=db;Port=1533;Uid=app;Pwd=p", "", "Port="},
		{"Server=db,1433;Data Source=evil,1433;User Id=app;Password=p;MultipleActiveResultSets=true", "", "gives the server (Server, Data Source, Address) twice"},
		{"Server=db,1433;Database=a;Initial Catalog=b;User Id=app;Password=p", "", "gives the database (Database, Initial Catalog) twice"},
		{"Server=db,1433;User Id=app;UID=other;Password=p", "", "gives the user"},
		{"Server=db,1433;User Id=app;Password=p;PWD=q", "", "gives the password"},
		{"Driver={ODBC Driver 18 for SQL Server};Server=db,1433;Address=evil;Uid=app;Pwd=p", "", "gives the server"},
		{"jdbc:sqlserver://db:1433;serverName=evil;user=app;password=p", "", "gives the server twice"},
		{"jdbc:sqlserver://db:1433;portNumber=1533;user=app;password=p", "", "two different ports"},
		{"Server=db;Database=a;Initial Catalog=b;User Id=app;Password=p", "", "gives the database"},
	} {
		got, err := Convert(tt.in, SQLServer)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: %s, %v; want an error with %q", tt.in, got, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.dsn {
			t.Errorf("%s: %s, %v; want %s", tt.in, got, err, tt.dsn)
		}
	}
	// go-mssqldb's own URL is rebuilt too: the driver ignores what it does
	// not know, so nothing passes unread.
	if got, err := Convert("sqlserver://app:p@db:1433?encrypt=true&log=1", SQLServer); err != nil || got != "sqlserver://app:p@db:1433?encrypt=true" {
		t.Errorf("go-mssqldb URL: %s, %v", got, err)
	}
}

// TestSQLServerNotOtherDrivers: a setting only SQL Server takes is not
// read as MySQL's or PostgreSQL's, and theirs are not read as SQL Server's.
func TestSQLServerNotOtherDrivers(t *testing.T) {
	for _, in := range []string{"Server=db,1433;User Id=app;Password=p", "jdbc:sqlserver://db:1433;user=app",
		"Driver={ODBC Driver 18 for SQL Server};Server=db;Uid=u;Pwd=p"} {
		if _, err := Convert(in, MySQL); err == nil || !strings.Contains(err.Error(), "names a sqlserver database, the driver is mysql") {
			t.Errorf("%s as MySQL: %v", in, err)
		}
	}
	for in, want := range map[string]string{
		"Server=db1,db2;Database=app;User ID=u;Password=p":     "mysql://u:p@db1:3306/",
		"Driver={MySQL ODBC 8.0 Driver};Server=db;Uid=u;Pwd=p": "mysql://u:p@db:3306/",
	} {
		if got, err := Convert(in, MySQL); err != nil || got != want {
			t.Errorf("%s: %s, %v; want %s", in, got, err, want)
		}
	}
}

// TestSQLServerProjects: the setting of each framework, with the client
// the project uses.
func TestSQLServerProjects(t *testing.T) {
	csproj := func(pkg, version string) string {
		return `<Project Sdk="Microsoft.NET.Sdk.Web"><ItemGroup><PackageReference Include="` + pkg + `" Version="` + version + `" /></ItemGroup></Project>`
	}
	const conn = `{"ConnectionStrings": {"Default": "Server=db.prod,1433;Database=Shop;User Id=app;Password=p"}}`
	for _, tt := range []struct {
		name  string
		files map[string]string
		ref   Ref
		env   map[string]string
		dsn   string
		db    string
	}{
		{"Microsoft.Data.SqlClient 5", map[string]string{"Api/Api.csproj": csproj("Microsoft.Data.SqlClient", "5.2.1"), "Api/appsettings.json": conn},
			Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:Default"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "Shop"},
		{"Microsoft.Data.SqlClient 3", map[string]string{"Api/Api.csproj": csproj("Microsoft.Data.SqlClient", "3.1.5"), "Api/appsettings.json": conn},
			Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:Default"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=false", "Shop"},
		{"EF Core 6", map[string]string{"Api/Api.csproj": csproj("Microsoft.EntityFrameworkCore.SqlServer", "6.0.33"), "Api/appsettings.json": conn},
			Ref{Source: AppSettings, File: "Api/appsettings.json", Key: "ConnectionStrings:Default"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=false", "Shop"},
		{"System.Data.SqlClient", map[string]string{"Api/Api.csproj": csproj("System.Data.SqlClient", "4.8.6"), "Api/appsettings.json": conn},
			Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:Default"}, nil, "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", "Shop"},
		{"central versions", map[string]string{"Api/Api.csproj": `<Project><ItemGroup><PackageReference Include="Microsoft.Data.SqlClient" /></ItemGroup></Project>`, "Api/appsettings.json": conn},
			Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:Default"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "Shop"},
		{"Laravel", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db.prod\nDB_DATABASE=shop\nDB_USERNAME=app\nDB_PASSWORD=p\n"},
			Ref{Source: Dotenv, File: ".env"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "shop"},
		{"Laravel host,port and instance", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db.prod\\SHOP,1533\nDB_USERNAME=app\nDB_PASSWORD=p\nDB_ENCRYPT=strict\n"},
			Ref{Source: Dotenv, File: ".env"}, nil, "sqlserver://app:p@db.prod:1533?encrypt=strict", ""},
		{"Laravel trust without config/database.php", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db.prod\nDB_USERNAME=app\nDB_PASSWORD=p\nDB_TRUST_SERVER_CERTIFICATE=true\nDB_ENCRYPT=no\n"},
			Ref{Source: Dotenv, File: ".env"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", ""},
		{"Laravel trust passed on", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db.prod\nDB_USERNAME=app\nDB_PASSWORD=p\nDB_TRUST_SERVER_CERTIFICATE=true\nDB_ENCRYPT=no\n",
			"config/database.php": "<?php return ['sqlsrv' => [\n // 'encrypt' => env('DB_ENCRYPT', 'yes'),\n 'trust_server_certificate' => env('DB_TRUST_SERVER_CERTIFICATE', 'false'),\n]];\n"},
			Ref{Source: Dotenv, File: ".env"}, nil, "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=true", ""},
		{"Laravel DB_URL", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=ignored\nDB_USERNAME=app\nDB_PASSWORD=p\nDB_URL=sqlsrv://u2:p2@db.prod:1533/shop?encrypt=strict\n"},
			Ref{Source: Dotenv, File: ".env"}, nil, "sqlserver://u2:p2@db.prod:1533?encrypt=strict", "shop"},
		{"Spring", map[string]string{"application.properties": "spring.datasource.url=jdbc:sqlserver://db.prod:1433;databaseName=Shop;encrypt=true;trustServerCertificate=false\nspring.datasource.username=app\nspring.datasource.password=p\n"},
			Ref{Source: Properties, File: "application.properties"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "Shop"},
		{"Django mssql", map[string]string{"settings.py": "DATABASES = {'default': {'ENGINE': 'mssql', 'NAME': 'shop', 'USER': 'app', 'PASSWORD': 'p', 'HOST': 'db.prod', 'PORT': '1433',\n 'OPTIONS': {'driver': 'ODBC Driver 17 for SQL Server', 'extra_params': 'TrustServerCertificate=yes'}}}\n"},
			Ref{Source: Python, File: "settings.py", Key: "default"}, nil, "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", "shop"},
		{"Django sql_server.pyodbc with an instance", map[string]string{"settings.py": "DATABASES = {'default': {'ENGINE': 'sql_server.pyodbc', 'NAME': 'shop', 'USER': 'app', 'PASSWORD': 'p', 'HOST': 'db.prod\\\\SHOP', 'PORT': '1533',\n 'OPTIONS': {'driver': 'ODBC Driver 18 for SQL Server'}}}\n"},
			Ref{Source: Python, File: "settings.py", Key: "default"}, nil, "sqlserver://app:p@db.prod:1533?encrypt=true", "shop"},
		{"Prisma", map[string]string{".env": "DATABASE_URL=\"sqlserver://db.prod:1433;database=shop;user=app;password={p;q};encrypt=true\"\n"},
			Ref{Source: Dotenv, File: ".env", Key: "DATABASE_URL"}, nil, "sqlserver://app:p;q@db.prod:1433?encrypt=true", "shop"},
		{"Rails", map[string]string{"config/database.yml": "development:\n  adapter: sqlserver\n  host: db.prod\n  port: 1433\n  database: shop\n  username: app\n  password: p\n"},
			Ref{Source: YAML, File: "config/database.yml", Key: "development"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "shop"},
		{"Go config.yaml", map[string]string{"config.yaml": "database:\n  url: sqlserver://app:p@db.prod:1433?database=shop&encrypt=true\n"},
			Ref{Source: YAML, File: "config.yaml", Key: "database"}, nil, "sqlserver://app:p@db.prod:1433?encrypt=true", "shop"},
		// A Go application's own setting without encrypt: its client,
		// go-mssqldb, encrypts the login only, without verifying.
		{"Go config.yaml without encrypt", map[string]string{"config.yaml": "database:\n  url: sqlserver://app:p@db.prod:1433?database=shop\n"},
			Ref{Source: YAML, File: "config.yaml", Key: "database"}, nil, "sqlserver://app:p@db.prod:1433?TrustServerCertificate=true&encrypt=false", "shop"},
		{"compose", map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    environment:\n      MSSQL_SA_PASSWORD: p@ss;w0rd\n      MSSQL_TCP_PORT: \"1533\"\n    ports: [\"14331:1533\"]\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, nil, "sqlserver://sa:p%40ss;w0rd@127.0.0.1:14331?TrustServerCertificate=true&encrypt=true", ""},
		{"compose on another address", map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/mssql/rhel/server:2019-latest\n    environment:\n      SA_PASSWORD: p\n    ports: [\"10.0.0.5:14331:1433\"]\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, nil, "sqlserver://sa:p@10.0.0.5:14331?encrypt=true", ""},
		{"compose host of a .env", map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=mssql\nDB_USERNAME=sa\nDB_PASSWORD=p\nDB_TRUST_SERVER_CERTIFICATE=true\n",
			"config/database.php": "<?php 'trust_server_certificate' => env('DB_TRUST_SERVER_CERTIFICATE', 'false'),\n",
			"compose.yaml":        "services:\n  mssql:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    ports: [\"14331:1433\"]\n"},
			Ref{Source: Dotenv, File: ".env", Host: "compose:mssql"}, nil, "sqlserver://sa:p@127.0.0.1:14331?TrustServerCertificate=true&encrypt=true", ""},
	} {
		root := writeTree(t, tt.files)
		dsn, db, err := tt.ref.Read(root, SQLServer, lookup(tt.env))
		if err != nil || dsn != tt.dsn || db != tt.db {
			t.Errorf("%s: %s %q, %v; want %s %q", tt.name, dsn, db, err, tt.dsn, tt.db)
		}
	}
}

// TestSQLServerProjectRefusals: what a project's setting asks for that
// DriftGate does not do is an error naming it.
func TestSQLServerProjectRefusals(t *testing.T) {
	for _, tt := range []struct {
		files map[string]string
		ref   Ref
		err   string
	}{
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    environment:\n      MSSQL_SA_PASSWORD_FILE: /run/secrets/sa\n    ports: [\"1433:1433\"]\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, "takes its password from a file (MSSQL_SA_PASSWORD_FILE)"},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    ports: [\"1433:1433\"]\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, "sets no MSSQL_SA_PASSWORD"},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/azure-sql-edge\n    environment:\n      MSSQL_SA_PASSWORD: p\n    ports: [\"1433:1433\"]\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, "Azure SQL Edge"},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: mcr.microsoft.com/mssql/server:2022-latest\n    environment:\n      MSSQL_SA_PASSWORD: p\n"},
			Ref{Source: Compose, File: "compose.yaml", Key: "db"}, "publishes no port for 1433"},
		{map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db\\SHOP\nDB_USERNAME=app\n"},
			Ref{Source: Dotenv, File: ".env"}, "DB_HOST names an instance and DB_PORT is not set"},
		{map[string]string{".env": "DB_CONNECTION=sqlsrv\nDB_HOST=db\nDB_USERNAME=app\nDB_ENCRYPT=maybe\n"},
			Ref{Source: Dotenv, File: ".env"}, ".env: DB_ENCRYPT: the SQL Server setting Encrypt has a value"},
		{map[string]string{"settings.py": "DATABASES = {'default': {'ENGINE': 'mssql', 'NAME': 'shop', 'USER': 'app', 'PASSWORD': 'p', 'HOST': 'db', 'OPTIONS': {'dsn': 'shop'}}}\n"},
			Ref{Source: Python, File: "settings.py", Key: "default"}, "ODBC data source"},
		{map[string]string{"settings.py": "DATABASES = {'default': {'ENGINE': 'mssql', 'NAME': 'shop', 'USER': 'app', 'PASSWORD': 'p', 'HOST': 'db', 'OPTIONS': {'extra_params': 'Authentication=ActiveDirectoryMsi'}}}\n"},
			Ref{Source: Python, File: "settings.py", Key: "default"}, "Microsoft Entra ID"},
	} {
		root := writeTree(t, tt.files)
		_, err := tt.ref.DSN(root, SQLServer, nil)
		if err == nil || !strings.Contains(err.Error(), tt.err) {
			t.Errorf("%+v: %v, want an error with %q", tt.ref, err, tt.err)
		}
	}
}

// TestSQLServerCertificateFiles: a certificate file a setting of the
// project names by a relative path is the project's, read past links; one
// named by an absolute path or in the home folder (~/) may be anywhere (a
// CA's certificate is often in a system folder), read past links too;
// .pem and .der only, a regular file. A DSN of the environment gives it by
// its absolute path or in the home folder.
func TestSQLServerCertificateFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	elsewhere := t.TempDir()
	outside := filepath.Join(elsewhere, "ca.pem")
	for _, f := range []string{outside, filepath.Join(elsewhere, "ca.crt")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(elsewhere, "link.pem")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "certs", "ca.der"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(elsewhere, "dir.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	real := func(f string) string {
		r, err := filepath.EvalSymlinks(f)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	const jdbc = "jdbc:sqlserver://db:1433;user=app;password=p;trustServerCertificateCA="
	for _, tt := range []struct{ file, want, err string }{
		{"certs/ca.pem", "", ""},
		{"certs/missing.pem", "", "a certificate file: certs/missing.pem: not found"},
		{"../ca.pem", "", "must be inside the project"},
		{outside, real(outside), ""},
		{link, real(outside), ""},
		{"~/certs/ca.der", real(filepath.Join(home, "certs", "ca.der")), ""},
		{"~/certs/missing.pem", "", "a certificate file"},
		{filepath.Join(elsewhere, "dir.pem"), "", "not a regular file"},
		{"certs/ca.crt", "", "a .pem or .der file"},
		{filepath.Join(elsewhere, "ca.crt"), "", "a .pem or .der file"},
	} {
		root := writeTree(t, map[string]string{".env": "X=\"" + jdbc + tt.file + "\"\n", "certs/ca.pem": "x", "certs/ca.crt": "x"})
		dsn, err := Ref{Source: Dotenv, File: ".env", Key: "X"}.DSN(root, SQLServer, nil)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%s: %s, %v; want an error with %q", tt.file, dsn, err, tt.err)
			}
			continue
		}
		if tt.want == "" {
			tt.want = real(filepath.Join(root, "certs", "ca.pem"))
		}
		if want := "sqlserver://app:p@db:1433?certificate=" + url.QueryEscape(tt.want) + "&encrypt=true"; err != nil || dsn != want {
			t.Errorf("%s: %s, %v; want %s", tt.file, dsn, err, want)
		}
	}
	if _, err := Convert(jdbc+"certs/ca.pem", SQLServer); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("a relative path in a DSN of the environment: %v", err)
	}
	want := "sqlserver://app:p@db:1433?certificate=" + url.QueryEscape(filepath.Join(home, "certs", "ca.der")) + "&encrypt=true"
	if dsn, err := Convert(jdbc+"~/certs/ca.der", SQLServer); err != nil || dsn != want {
		t.Errorf("a path in the home folder in a DSN of the environment: %s, %v; want %s", dsn, err, want)
	}
}

// TestSQLServerDjangoOptionsOnly: Django's OPTIONS are read for a SQL
// Server engine only.
func TestSQLServerDjangoOptionsOnly(t *testing.T) {
	root := writeTree(t, map[string]string{"settings.py": "DATABASES = {'default': {'ENGINE': 'django.db.backends.mysql', 'NAME': 'shop', 'USER': 'app', 'PASSWORD': 'p', 'HOST': 'db', 'OPTIONS': {'dsn': 'x', 'driver': 'y'}}}\n"})
	if dsn, err := (Ref{Source: Python, File: "settings.py", Key: "default"}).DSN(root, MySQL, nil); err != nil || dsn != "mysql://app:p@db:3306/" {
		t.Errorf("%s, %v", dsn, err)
	}
}

// TestFindSQLServer: Find offers the SQL Server settings of ASP.NET,
// Laravel and compose, with the notes on their server and TLS.
func TestFindSQLServer(t *testing.T) {
	for _, tt := range []struct {
		dir   string
		dbs   []string
		notes []string
		dsns  []string
	}{
		{
			dir: "aspnet",
			dbs: []string{
				"sqlserver localhost:14331/Shop sa <- appsettings Shop.Api/Shop.Api.csproj ConnectionStrings:Default",
				"sqlserver reports.internal:/Reports reader <- appsettings Shop.Api/Shop.Api.csproj ConnectionStrings:Reports",
			},
			notes: []string{"", ""},
			dsns:  []string{"sqlserver://sa:S3cr;et=x@localhost:14331?TrustServerCertificate=true&encrypt=true", "sqlserver://reader:S3cr-reports@reports.internal:1433?encrypt=true"},
		},
		{
			dir:   "laravel",
			dbs:   []string{"sqlserver db.internal:1533/shop shop <- dotenv .env "},
			notes: []string{"the setting leaves Encrypt to the application's client, which DriftGate cannot tell: DriftGate encrypts and verifies the server's certificate; for a self-signed one, set Encrypt and TrustServerCertificate in the setting"},
			dsns:  []string{"sqlserver://shop:S3cr%7Bx%7D%40y@db.internal:1533?TrustServerCertificate=true&encrypt=true"},
		},
		{
			dir:   "compose",
			dbs:   []string{"sqlserver 127.0.0.1:14331/ sa <- compose compose.yaml mssql"},
			notes: []string{"DriftGate trusts the container's own certificate on this machine (encrypt=true, TrustServerCertificate=true)"},
			dsns:  []string{"sqlserver://sa:S3cr-Passw0rd@127.0.0.1:14331?TrustServerCertificate=true&encrypt=true"},
		},
	} {
		root := filepath.Join("testdata", "sqlserver", tt.dir)
		found := Find(root, nil)
		if len(found.Notes) > 0 {
			t.Errorf("%s: notes %v", tt.dir, found.Notes)
		}
		if len(found.Databases) != len(tt.dbs) {
			t.Errorf("%s: %d databases, want %d: %+v", tt.dir, len(found.Databases), len(tt.dbs), found.Databases)
			continue
		}
		for i, d := range found.Databases {
			if got := short(d); got != tt.dbs[i] {
				t.Errorf("%s: %s, want %s", tt.dir, got, tt.dbs[i])
			}
			if d.Note != tt.notes[i] {
				t.Errorf("%s: note %q, want %q", tt.dir, d.Note, tt.notes[i])
			}
			dsn, err := d.Ref.DSN(root, d.Driver, nil)
			if err != nil || dsn != tt.dsns[i] {
				t.Errorf("%s: %s, %v; want %s", tt.dir, dsn, err, tt.dsns[i])
			}
			if s, err := dbdsn.Server(dbdsn.SQLServer, dsn); err != nil || s != d.Host+":"+map[bool]string{true: d.Port, false: "1433"}[d.Port != ""] {
				t.Errorf("%s: server %s, %v", tt.dir, s, err)
			}
		}
	}
}

// TestSQLServerNotes: Find says how DriftGate takes a setting's local
// server and a TLS it cannot tell or that verifies more than the client.
func TestSQLServerNotes(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Api/Api.csproj":       `<Project><ItemGroup><PackageReference Include="Microsoft.Data.SqlClient" /></ItemGroup></Project>`,
		"Api/appsettings.json": `{"ConnectionStrings": {"A": "Server=.;Database=Shop;User Id=sa;Password=p;MultipleActiveResultSets=true", "B": "Server=db,1433;Database=Shop;User Id=sa;Password=p;Encrypt=False"}}`,
	})
	found := Find(root, nil)
	if len(found.Databases) != 2 {
		t.Fatalf("%+v", found)
	}
	a, b := found.Databases[0].Note, found.Databases[1].Note
	if !strings.Contains(a, "the server is ., the local one") || !strings.Contains(a, "leaves Encrypt to the application's client") {
		t.Errorf("A: %s", a)
	}
	if !strings.Contains(b, "add TrustServerCertificate=true to the application's setting") {
		t.Errorf("B: %s", b)
	}
	if p, err := fromValue("sqlserver://app:p@db:1433/shop"); err != nil || p.name != "shop" || !strings.Contains(p.sqlServerNote(), "as Rails reads it") {
		t.Errorf("a path for the database: %q, %q, %v", p.name, p.sqlServerNote(), err)
	}
}

// TestSQLServerPasswordsSurvive: a password with the characters every
// form quotes comes out of each form as it went in.
func TestSQLServerPasswordsSurvive(t *testing.T) {
	for _, pw := range []string{`a;b`, `a@b`, `{a}`, `a=b`, `a'b`, `a"b`, `;@{}='"`, `}}{{`, `p w%41`, `a\b`, `a:b/c?d#e`} {
		ado := `Server=db,1433;User Id=app;Password="` + strings.ReplaceAll(pw, `"`, `""`) + `"`
		odbc := "Driver={ODBC Driver 18 for SQL Server};Server=db,1433;Uid=app;Pwd={" + strings.ReplaceAll(pw, "}", "}}") + "}"
		jdbc := "jdbc:sqlserver://db:1433;user=app;password={" + strings.ReplaceAll(pw, "}", "}}") + "}"
		goURL := "sqlserver://" + url.UserPassword("app", pw).String() + "@db:1433?encrypt=true"
		for _, in := range []string{ado, odbc, jdbc} {
			dsn, err := Convert(in, SQLServer)
			if err != nil {
				t.Errorf("%q: %v", pw, err)
				continue
			}
			u, err := url.Parse(dsn)
			if err != nil {
				t.Errorf("%q: %v", pw, err)
				continue
			}
			if got, _ := u.User.Password(); got != pw || u.User.Username() != "app" || u.Host != "db:1433" {
				t.Errorf("%q from %s: %q %q %q", pw, in, u.User.Username(), got, u.Host)
			}
		}
		p, err := fromValue(goURL)
		if err != nil || p.password != pw {
			t.Errorf("%q from the go-mssqldb URL: %q, %v", pw, p.password, err)
		}
	}
}

// tlsStrength orders TLS from none to the strictest, as a client takes
// encrypt (true, false, strict, disable) and trust: no TLS, the login
// encrypted without verifying, the login verified, all encrypted without
// verifying, all verified, TDS 8 (strict).
func tlsStrength(encrypt string, trust bool) int {
	switch encrypt {
	case "disable":
		return 0
	case "false":
		if trust {
			return 1
		}
		return 2
	case "strict":
		return 5
	}
	if trust {
		return 3
	}
	return 4
}

// appStrength is the strength of what an application's client does with
// encrypt and trust. With Encrypt false a client that never verifies the
// certificate (loginTrust: System.Data.SqlClient, ODBC Driver 17) encrypts
// the login without verifying; the others (Microsoft.Data.SqlClient, ODBC
// Driver 18, JDBC) verify it when the server encrypts, which DriftGate
// must not skip.
func appStrength(encrypt string, trust, loginTrust bool) int {
	if encrypt == "false" && loginTrust {
		trust = true
	}
	return tlsStrength(encrypt, trust)
}

// goMSSQLStrength is the strength of a DSN as go-mssqldb takes it: without
// encrypt it encrypts the login only and trusts any certificate; strict
// ignores TrustServerCertificate.
func goMSSQLStrength(t *testing.T, dsn string) int {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("%s: %v", dsn, err)
	}
	q := u.Query()
	enc, ok := q["encrypt"]
	if !ok {
		return tlsStrength("false", true)
	}
	trust := q.Get("TrustServerCertificate") == "true"
	return tlsStrength(enc[0], trust)
}

// TestSQLServerTLSNeverWeaker: for every Encrypt and TrustServerCertificate
// a setting can give, with every client default, the DSN is never weaker
// than what the application's client does, and keeps the certificates.
func TestSQLServerTLSNeverWeaker(t *testing.T) {
	encrypts := []string{"", "true", "false", "strict", "yes", "no", "Mandatory", "Optional", "1", "0"}
	trusts := []string{"", "true", "false", "yes", "no"}
	norm := map[string]string{"": "", "true": "true", "yes": "true", "mandatory": "true", "1": "true", "false": "false", "no": "false", "optional": "false", "0": "false", "strict": "strict"}
	clients := []struct {
		csproj, encrypt string
		loginTrust      bool
	}{
		{"Microsoft.Data.SqlClient\" Version=\"5.1.0", "true", false},
		{"Microsoft.Data.SqlClient\" Version=\"2.1.0", "false", false},
		{"Microsoft.EntityFrameworkCore.SqlServer\" Version=\"6.0.0", "false", false},
		{"System.Data.SqlClient\" Version=\"4.8.6", "false", true},
		{"Unknown.Package\" Version=\"1.0.0", "true", false}, // the client of an unknown default must not be weaker than verifying
	}
	rng := rand.New(rand.NewSource(1))
	for range 2000 {
		enc, trust, c := encrypts[rng.Intn(len(encrypts))], trusts[rng.Intn(len(trusts))], clients[rng.Intn(len(clients))]
		host := []string{"db.prod", "127.0.0.1", "localhost"}[rng.Intn(3)]
		cs := "Server=" + host + ",1433;User Id=app;Password=p;MultipleActiveResultSets=true"
		if enc != "" {
			cs += ";Encrypt=" + enc
		}
		if trust != "" {
			cs += ";TrustServerCertificate=" + trust
		}
		if rng.Intn(2) == 0 {
			cs += ";HostNameInCertificate=db.example.com"
		}
		root := writeTree(t, map[string]string{
			"Api/Api.csproj":       `<Project><ItemGroup><PackageReference Include="` + c.csproj + `" /></ItemGroup></Project>`,
			"Api/appsettings.json": `{"ConnectionStrings": {"A": "` + strings.ReplaceAll(cs, `"`, `\"`) + `"}}`,
		})
		dsn, err := Ref{Source: AppSettings, File: "Api/Api.csproj", Key: "ConnectionStrings:A"}.DSN(root, SQLServer, nil)
		if err != nil {
			t.Fatalf("%s: %v", cs, err)
		}
		appEnc := norm[strings.ToLower(enc)]
		if appEnc == "" {
			appEnc = c.encrypt
		}
		appTrust := norm[strings.ToLower(trust)] == "true"
		if appEnc == "strict" {
			appTrust = false
		}
		if got, want := goMSSQLStrength(t, dsn), appStrength(appEnc, appTrust, c.loginTrust); got < want {
			t.Errorf("%s (%s): %s is weaker (%d < %d)", cs, c.csproj, dsn, got, want)
		}
		if strings.Contains(cs, "HostNameInCertificate") && !strings.Contains(dsn, "hostNameInCertificate=db.example.com") {
			t.Errorf("%s: %s lost HostNameInCertificate", cs, dsn)
		}
	}
}

// TestSQLServerConvertGoURLNeverWeaker: a go-mssqldb URL of the
// environment or 1Password is written for DriftGate, so Convert never
// makes it weaker than what it says, and one without encrypt is encrypted
// (verified unless it says TrustServerCertificate).
func TestSQLServerConvertGoURLNeverWeaker(t *testing.T) {
	encrypts := []string{"", "true", "false", "strict", "yes", "no", "1", "0", "disable"}
	trusts := []string{"", "true", "false"}
	for _, enc := range encrypts {
		for _, trust := range trusts {
			q := url.Values{"database": {"Shop"}}
			if enc != "" {
				q.Set("encrypt", enc)
			}
			if trust != "" {
				q.Set("TrustServerCertificate", trust)
			}
			in := "sqlserver://app:p@db.prod:1433?" + q.Encode()
			dsn, err := Convert(in, SQLServer)
			if err != nil {
				t.Fatalf("%s: %v", in, err)
			}
			norm := map[string]string{"": "true", "true": "true", "yes": "true", "1": "true", "false": "false", "no": "false", "0": "false", "strict": "strict", "disable": "disable"}
			want := tlsStrength(norm[enc], trust == "true" && enc != "strict")
			if got := goMSSQLStrength(t, dsn); got < want {
				t.Errorf("%s: %s is weaker (%d < %d)", in, dsn, got, want)
			}
			if enc == "" && trust != "true" && dsn != "sqlserver://app:p@db.prod:1433?encrypt=true" {
				t.Errorf("%s: %s, want encrypt=true and the certificate verified", in, dsn)
			}
		}
	}
}

// FuzzSQLServerConnString: an ADO.NET or ODBC connection string with any
// server, password and further keys either gives a DSN that holds the
// password and the server as they were, with TLS never weaker than the
// setting asked for, or an error that holds neither.
func FuzzSQLServerConnString(f *testing.F) {
	f.Add("db,1433", "S3cr;et", "Encrypt=strict", false)
	f.Add("tcp:db.prod,14331", `S3cr"x'`, "TrustServerCertificate=yes;Encrypt=no", true)
	f.Add(`db\inst,1533`, "S3cr}}{", "Authentication=ActiveDirectoryPassword", true)
	f.Add("(localdb)\\x", "S3cr=@", "Integrated Security=true", false)
	f.Add("np:db", "pw", "", false)
	f.Fuzz(func(t *testing.T, server, password, extra string, odbc bool) {
		var in string
		if odbc {
			in = "Driver={ODBC Driver 18 for SQL Server};Server=" + server + ";Uid=app;Pwd={" + strings.ReplaceAll(password, "}", "}}") + "};" + extra
		} else {
			in = "Server=" + server + ";User Id=app;Password=\"" + strings.ReplaceAll(password, `"`, `""`) + "\";" + extra
		}
		checkSQLServerDSN(t, in, password, server, extra)
	})
}

// FuzzSQLServerJDBC: a JDBC or Prisma URL, as FuzzSQLServerConnString.
func FuzzSQLServerJDBC(f *testing.F) {
	f.Add("db:1433", "S3cr;et", "encrypt=strict", true)
	f.Add("db.prod", "S3cr}x{", "trustServerCertificate=true;encrypt=false", false)
	f.Add(`db\inst:1533`, "S3cr=@;", "integratedSecurity=true", true)
	f.Add("[::1]:1433", "pw", "trustStore=/x", true)
	f.Fuzz(func(t *testing.T, server, password, extra string, jdbc bool) {
		in := "sqlserver://" + server + ";user=app;password={" + strings.ReplaceAll(password, "}", "}}") + "};" + extra
		if jdbc {
			in = "jdbc:" + in
		}
		checkSQLServerDSN(t, in, password, server, extra)
	})
}

// checkSQLServerDSN converts in, a setting with password, and checks the
// result (see FuzzSQLServerConnString).
func checkSQLServerDSN(t *testing.T, in, password, server, extra string) {
	t.Helper()
	dsn, err := Convert(in, SQLServer)
	if err != nil {
		msg := err.Error()
		for _, secret := range []string{password, in, server} {
			// A short value may be a word of the message itself.
			if len(secret) >= 6 && strings.Contains(msg, secret) && !strings.Contains(sqlServerMessages, secret) {
				t.Fatalf("the error holds the input %q: %v", secret, err)
			}
		}
		return
	}
	u, perr := url.Parse(dsn)
	if perr != nil || u.Scheme != "sqlserver" {
		t.Fatalf("%q gave %q, which is no sqlserver:// URL", in, dsn)
	}
	// The password comes out as it went in, unless the other keys set one
	// too or the server is not a plain host[:port] (its text then shapes the
	// string another way).
	plain := strings.Trim(server, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_:,") == ""
	if lk := strings.ToLower(extra); plain && !strings.Contains(lk, "pw") && !strings.Contains(lk, "pass") {
		if got, _ := u.User.Password(); got != password {
			t.Fatalf("%q: password %q, want %q", in, got, password)
		}
	}
	q := u.Query()
	if len(q["encrypt"]) != 1 {
		t.Fatalf("%q: %s writes no encrypt", in, dsn)
	}
	// What the setting asked for, read back by the same rules, is never
	// stronger than the DSN.
	kv := fields{}
	goURL := false
	if _, rest, ok := strings.Cut(in, "sqlserver://"); ok && !jdbcForm(rest) {
		// A go-mssqldb URL: without encrypt its driver verifies nothing.
		goURL = true
		if u, err := url.Parse(strings.TrimPrefix(in, "jdbc:")); err == nil {
			for k, vs := range u.Query() {
				kv[fieldKey(k)] = vs[0]
			}
		}
	} else if ok {
		_, props, _ := strings.Cut(in, ";")
		kv, _ = parseJDBCProps(props)
	} else if odbc, err := parseODBC(in); err == nil && odbcSQLServer(odbc["driver"]) {
		kv = odbc
	} else {
		kv, _ = parseConnString(in)
	}
	var s sqlServerTLS
	_ = s.setAll(kv)
	if _, ok := kv["encrypt"]; goURL && !ok {
		s.loginTrust = true
	}
	asked := s.encrypt
	if asked == "" {
		asked = "false" // the weakest default of a client
	}
	if got, want := goMSSQLStrength(t, dsn), appStrength(asked, s.trust == "true" && asked != "strict" || s.tdsRequire && s.ca == "", s.loginTrust); got < want {
		t.Fatalf("%q: %s is weaker than the setting (%d < %d)", in, dsn, got, want)
	}
	if s.hostInCert != "" && q.Get("hostNameInCertificate") != s.hostInCert && asked != "disable" {
		t.Fatalf("%q: %s lost HostNameInCertificate", in, dsn)
	}
	if h, err := dbdsn.Server(dbdsn.SQLServer, dsn); err != nil || h != u.Host {
		t.Fatalf("%q: dbdsn reads %s as %s, %v", in, dsn, h, err)
	}
}

// sqlServerMessages holds the fixed text of the errors, which a short
// fuzzed value may match by chance.
var sqlServerMessages = strings.Join([]string{
	string(errSQLServerWindows), string(errSQLServerDomain), string(errSQLServerEntra), string(errSQLServerAuth),
	string(errSQLServerKerberos), string(errSQLServerAE), string(errSQLServerReadOnly), string(errSQLServerFailover),
	string(errSQLServerAttach), string(errSQLServerPipe), string(errSQLServerLocalDB), string(errSQLServerAdmin),
	string(errSQLServerTrustStore), string(errSQLServerODBCDSN), string(errSQLServerServer), string(errSQLServerPort),
	string(errSQLServerPorts), string(errSQLServerAmbiguous), string(errSQLServerSocket), string(errSQLServerNoUser),
	string(errSQLServerCertPair), string(errNoServer), string(errURL),
	"the DSN does not say which database it is; set the driver", "the DSN names a mysql database, the driver is sqlserver",
	"the DSN names a postgres database, the driver is sqlserver", "the SQL Server setting", "has a value DriftGate does not know (the value is not shown); set it to true, false or strict",
	"connection string: pair", "has no closing quote", "text after the quoted value of pair", "a property of the URL has no = (the value is not shown)",
	string(errSQLServerInstance), "not an ODBC connection string: every pair is key=value, separated by ;, a value with ; or } in {…} (the value is not shown)",
	"a {…} value of the URL has no closing } (the value is not shown)", "text after a {…} value of the URL (the value is not shown)", "names no database DriftGate knows",
}, "\n")
