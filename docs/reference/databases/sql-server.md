# SQL Server

DriftGate has SQL Server built in: the core of
[go-mssqldb](https://pkg.go.dev/github.com/microsoft/go-mssqldb), a pure
Go driver. So the binary needs no CGO and no client library.

## Supported versions

- SQL Server 2017 and later: Developer, Express, Standard and Enterprise, on Linux, Windows or in a container.
- Not Azure SQL Database, Azure SQL Managed Instance or Synapse: their databases cannot be backed up to a file and restored from it.
- CI tests SQL Server 2017, 2019, 2022 and 2025 (Developer, on Linux in Docker). SQL Server on Windows is not tested there.

SQL Server 2017 makes itself a certificate with a negative serial number.
Go refuses to read it by default, also with `TrustServerCertificate`, so
no connection to it would work.

- When a command makes its first SQL Server component, DriftGate turns on `GODEBUG=x509negativeserial=1` for its own process.
- From then on, for the whole process, such a certificate is read: the service's HTTPS included, and the commands it starts, which inherit it.
- Its chain and host name are verified as any other's.
- A project without SQL Server never has it, and an `x509negativeserial` you set in `GODEBUG` yourself is kept.

## Configure SQL Server

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlserver
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }
    working: Shop
    exclude: [dbo.SessionCache, "HangFire.*"]
```

| Key | Type | Meaning |
|---|---|---|
| `driver` | `sqlserver` (`mssql` is accepted) | |
| `dsn` | `{env}` \| `{op}` \| a file of the project | the server and an SQL login ([DSN and TLS](#dsn-and-tls)) |
| `working` | string, required | the database the application uses |
| `pristine` | string | its pristine copy, a database on the same server; default `<working>_pristine` |
| `exclude` | list of globs | as for [other databases](../state.md#database-components); matches `schema.name` or `name` |
| `restore_only`, `restore_empty` | list of globs | as for other databases; matched as `exclude` matches tables |
| `row_tracking` | list of tables | as for other databases; `schema.name`, or `name` in `dbo` |
| `allow_remote` | bool | as for other databases |

- `dsn` is a go-mssqldb URL (`sqlserver://user:pass@host:1433?encrypt=true`), an ADO.NET, ODBC or JDBC connection string, or the application's setting ([the application's settings](#the-applications-settings)).
- Database names are compared without letter case, as SQL Server does.
- `pristine` may not be `working` in any letter case, and has at most 122 characters. DriftGate makes `<pristine>_dgnew` and `<pristine>_dgx` next to it.
- An `exclude` glob matches without letter case. It also leaves out views and sequences of those names.
- Views and sequences are not `restore_only`.
- A `row_tracking` table needs a primary key.

## What the login needs

DriftGate signs in with an SQL Server login: a user name and password.
The server must allow SQL Server authentication, as the container images
do. Windows accounts (`Integrated Security`, `Trusted_Connection`,
`DOMAIN\user`), Kerberos and NTLM, and Microsoft Entra ID
(`Authentication=ActiveDirectory…`) are refused by name: the driver is
built without them.

The login needs:

- the server role `dbcreator`. It creates the pristine copy, renames and drops the copies it made (it owns them), and restores the working database from a backup.
- `db_owner` in the working database, or to own it. It backs the database up and restores it, table by table and, when it must, as a whole.

`dbcreator` reaches every database of the server, not only these. Its
login may alter any of them (take it offline, set it to single user),
restore over any of them (`RESTORE … WITH REPLACE`) and drop any of them,
though it cannot read their data.

`sa` or another `sysadmin` needs nothing more; use it only for a local,
disposable server, such as a container on this machine.
[`driftgate setup`](../commands/project.md#when-the-database-user-may-not-create-a-database)
prints what an administrator runs for any other login. Every statement
may run again:

```sql
USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop];
USE [Shop];
IF SUSER_SID(N'shop') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [shop] FOR LOGIN [shop];
IF SUSER_SID(N'shop') <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER [shop];
```

The login's own permissions are the real limit of what DriftGate's
queries can do ([security](../../security.md)).

### The narrower grant

`setup` prints a narrower grant too. Instead of both roles, it gives the
right to create databases and the ownership of the working database:

```sql
USE [master]; GRANT CREATE ANY DATABASE TO [shop]; ALTER AUTHORIZATION ON DATABASE::[Shop] TO [shop];
```

- The login then reaches only the databases it owns: the working database and the copies it makes.
- Seeds, and restores item by item and as a whole, work. Altering, restoring over or dropping any other database is refused.
- It is not the default because it changes the working database's owner, which an application may rely on. The login becomes `dbo` there.
- Whatever ran as the old owner, such as `EXECUTE AS OWNER` or cross-database ownership chaining, changes with it.
- It fails (error 15110) while the login has a user in that database. Drop it first (`USE [Shop]; DROP USER [shop];`); as `dbo` the login needs none.

Either grant lets the login restore any backup file the server can read
into a new database of its own, and so read it. Keep the backups of
other databases out of the server's reach, its backup folder included.

### Fast fingerprints

An optional permission, which `setup` proposes with the grant:
`GRANT VIEW SERVER PERFORMANCE STATE TO [shop];` on SQL Server 2022 and
later, `VIEW SERVER STATE` on 2017 and 2019.

- With it, a fingerprint of data that did not change is reused, and one after a change reads only the tables written to ([how fast](#how-fast-a-fingerprint-is)).
- A fingerprint also waits for the application's open transactions ([transactions](#transactions)).
- Without it the result is the same, but every fingerprint reads every table, which takes longer on a large database. The log says so once.

Either permission also lets the login itself read every session of the
server and the SQL text it runs or ran (`sys.dm_exec_requests`, the plan
cache), literals included. DriftGate's queries refuse those views, but
grant it only to a login the server's other users may trust with that.

## DSN and TLS

DriftGate's DSN is go-mssqldb's URL,
`sqlserver://user:pass@host:1433?encrypt=true`. A DSN of the environment
or 1Password may also be:

- an ADO.NET connection string: `Server=tcp:db,1433;Database=Shop;User Id=shop;Password=…`;
- an ODBC one: `Driver={ODBC Driver 18 for SQL Server};Server=…`;
- a JDBC URL: `jdbc:sqlserver://db:1433;databaseName=Shop;user=…;password=…`.

Each is read, and the URL written anew from what DriftGate reads.
go-mssqldb ignores a parameter it does not know, so a mistyped `encrypt`
would otherwise go unnoticed.

- A URL parameter DriftGate does not know, one given twice, and a `;` in the query are refused. Percent-encode a `;` as `%3B`.
- The URL's database is ignored; `working` names it.
- A go-mssqldb URL of the environment or 1Password is written for DriftGate, not taken from an application. Without `encrypt` it is encrypted and the certificate verified (`encrypt=true`); for a self-signed certificate, add `TrustServerCertificate=true`.

The TLS DriftGate connects with is never weaker than the application's
own client. `Encrypt` (`true`, `yes`, `Mandatory`; `false`, `no`,
`Optional`; `Strict`), `TrustServerCertificate`,
`HostNameInCertificate`, a minimum TLS version and certificate files are
passed on. A value DriftGate cannot map is an error, never left out.

- **`Encrypt` off.** DriftGate still encrypts the sign-in, as every client does, and never signs in without TLS. `encrypt=disable` and Prisma's `DANGER_PLAINTEXT` are taken as off.
- A server that answers it cannot encrypt (`ENCRYPT_NOT_SUP`) is refused before the password is sent. go-mssqldb alone would answer that by sending the password in clear.
- After the sign-in the data is not encrypted. go-mssqldb verifies the certificate the sign-in is encrypted with.
- Some clients never verify it with `Encrypt` off, and DriftGate does not either (`TrustServerCertificate=true`). They are System.Data.SqlClient, ODBC Driver 17 and earlier, a Go application's go-mssqldb setting without `encrypt`, and FreeTDS with `encryption = off` or `request`.
- For the other clients, a server with a self-signed certificate needs `TrustServerCertificate=true` in the application's own setting; `init` and `setup` say so.
- **A setting that does not say** (`Encrypt` unset, a client DriftGate cannot tell) is encrypted and verified. For a self-signed certificate, set `Encrypt` and `TrustServerCertificate` in the setting.
- FreeTDS's `encryption = require` encrypts without verifying, unless a CA is given.
- **A compose service** on a loopback address of this machine is trusted with its own self-signed certificate (`encrypt=true&TrustServerCertificate=true`), unless the setting says otherwise. On any other address it is verified.
- **`Strict`** (TDS 8) always verifies, and drops `TrustServerCertificate`. CI checks it on SQL Server 2025. SQL Server 2022 and 2019 on Linux close a TDS 8 connection, and DriftGate does not fall back to another encryption.
- JDBC's `sslProtocol=TLSv1.3` is `tlsmin=1.3`.

`tools/e2e/sqlserver-tls.sh`, run in CI for every version, starts SQL
Server with a certificate of a test CA and forced encryption. It checks
that a verified connection works, and that a wrong CA, host or
`hostNameInCertificate` is refused without the password in any message.

### The TLS DriftGate uses when Encrypt is unset

When the setting leaves `Encrypt` unset, the client's default is taken:

| Client | Told by | DriftGate connects with |
|---|---|---|
| Microsoft.Data.SqlClient 4 and later | its `PackageReference` in the `.csproj` | `encrypt=true` (encrypted, certificate verified) |
| Microsoft.Data.SqlClient before 4 | the `.csproj` | `encrypt=false` |
| EF Core (`Microsoft.EntityFrameworkCore.SqlServer`) 7 and later | the `.csproj` | `encrypt=true` |
| EF Core before 7 | the `.csproj` | `encrypt=false` |
| System.Data.SqlClient | the `.csproj` | `encrypt=false&TrustServerCertificate=true` |
| ODBC Driver 18 and later | `Driver=` (also Django's `OPTIONS` `driver`) | `encrypt=true` |
| ODBC Driver 17 and earlier, SQL Server Native Client, the old `SQL Server` driver | `Driver=` | `encrypt=false&TrustServerCertificate=true` |
| mssql-jdbc (10.2 and later) | `jdbc:sqlserver://` | `encrypt=true` |
| Prisma | `sqlserver://host:1433;database=…` | `encrypt=true` |
| tedious, node-mssql | `mssql://` | `encrypt=true` |
| go-mssqldb | `sqlserver://user:pass@host:1433` in a Go application's own setting file | `encrypt=false&TrustServerCertificate=true` |
| FreeTDS (its ODBC driver, `dblib://`, Rails' TinyTDS), pdo_sqlsrv (Laravel), pyodbc without a known driver | | `encrypt=true` |
| a go-mssqldb URL or an ADO.NET connection string of the environment or 1Password, any other | | `encrypt=true` |

For FreeTDS, pdo_sqlsrv and pyodbc, `freetds.conf` or the ODBC driver
decides, which DriftGate does not read.

### Certificate files

A certificate file is a CA (go-mssqldb's `certificate`, Prisma's
`trustServerCertificateCA`) or the server's own certificate
(`ServerCertificate`, compared byte for byte).

- The server's own certificate does not go together with `HostNameInCertificate` or a CA.
- The file is a regular `.pem` or `.der` file. Convert a `.cer` or `.crt` with `openssl x509`.
- A setting of the project names one by a path relative to the project, which stays inside it, also past symbolic links. Or it names one anywhere by an absolute path or one in the home folder (`~/certs/ca.pem`), such as a system folder (`/etc/ssl/certs`).
- A DSN of the environment or 1Password names one by an absolute path, or one in the home folder.
- The driver only reads it as a certificate; no message shows what it holds.
- A Java trust store (`trustStore`) is refused: give the CA as a PEM file.

## The application's settings

`init` and `setup` find SQL Server in the settings below, and write the
`dsn:` that reads them
([from the project's own settings](../database-settings.md#read-the-dsn-from-the-projects-own-settings)).

- The server is `host`, `host,port`, `tcp:host,port` or `host\instance,port`.
- The local server (`.`, `(local)`, none) is taken as TCP to `127.0.0.1`, with a note: a Windows client may reach it through shared memory.

| Framework | `dsn:` |
|---|---|
| ASP.NET, EF Core | `appsettings: <project>.csproj`, `key: "ConnectionStrings:<name>"` |
| Laravel | `dotenv: .env` |
| Spring, Quarkus, Micronaut | `properties: …` |
| Django | `python: settings.py` |
| Prisma | `dotenv: .env`, `key: DATABASE_URL` |
| Rails | `yaml: config/database.yml` |
| Go | `yaml: config.yaml` |
| Node | `dotenv: .env`, `key: …` |
| compose | `compose: compose.yaml`, `key: <service>` |

What each reads:

- **ASP.NET, EF Core**: the connection string, as above, and the client from the `.csproj` ([the ASP.NET Core guide](../../guides/frameworks/aspnet-core.md)).
- **Laravel**: `DB_CONNECTION=sqlsrv`, `DB_HOST` (also `host\instance` or `host,port`), `DB_PORT` (default 1433), `DB_DATABASE`, `DB_USERNAME` and `DB_PASSWORD`, with `DB_URL` (`sqlsrv://`) over them.
- Laravel's `DB_ENCRYPT` and `DB_TRUST_SERVER_CERTIFICATE` count only when `config/database.php` passes them on; Laravel ships those lines commented out. Without a `config/database.php`, a value that weakens TLS is left out, with a note.
- **Spring, Quarkus, Micronaut**: `jdbc:sqlserver://host:1433;databaseName=…;encrypt=…` (also `serverName`, `portNumber`, `instanceName`), with the user and password keys.
- **Django**: `ENGINE` `mssql` (mssql-django) or `sql_server.pyodbc`; `HOST` (also `host\instance`) and `PORT`; `OPTIONS` `driver`, whose default `Encrypt` counts, and `extra_params` (ODBC keys). An ODBC `dsn` is refused.
- **Prisma**: `sqlserver://host:1433;database=…;user=…;password=…;encrypt=…`, with a value that holds `;` in `{…}`.
- **Rails**: `adapter: sqlserver` (activerecord-sqlserver-adapter), `host`, `port`, `database`, `username`, `password`.
- **Go**: a go-mssqldb URL with `?database=…`. A path with a port is taken for the database, as Rails reads it, with a note; go-mssqldb takes it for an instance. A path without a port is refused.
- **Node**: `mssql://user:pass@host:1433/db` (tedious, node-mssql).
- **compose**: an `mcr.microsoft.com/mssql/server` (or `mssql/rhel/server`) service. It signs in as `sa` with `MSSQL_SA_PASSWORD` (or `SA_PASSWORD`), at the port it publishes for `MSSQL_TCP_PORT` (default 1433).
- The image names no database: `setup` takes the application's, when its setting names the same server. A password from a file (`MSSQL_SA_PASSWORD_FILE`) and Azure SQL Edge are refused.

A key that names the server, database, user, password or port twice with
different values is refused, such as `Server` and `Data Source`, or
`Database` and `Initial Catalog`: the clients differ in which one wins.
Microsoft's ODBC drivers do not read `Port=` (write `Server=host,1433`);
FreeTDS's does.

## What DriftGate refuses

Each refusal comes with a message that names it, and no value:

- other sign-ins than an SQL login ([what the login needs](#what-the-login-needs));
- Always Encrypted (`Column Encryption Setting`), whose columns DriftGate cannot read;
- other ways to the server than TCP to one host and port: named pipes (`np:`, `Network Library`), shared memory (`lpc:`), LocalDB, the dedicated admin connection (`admin:`) and an ODBC data source (`DSN=`);
- a `Failover Partner`, `ApplicationIntent=ReadOnly` (a read-only replica cannot be restored), `AttachDbFilename` and `User Instance`;
- a named instance without its port: the SQL Server Browser would choose the port over UDP, so add `,port`;
- servers older than SQL Server 2017, Azure SQL Database, Managed Instance and Synapse;
- as `working`: a system database (`master`, `model`, `msdb`, `tempdb`);
- as `working`: a database in an availability group, with change data capture, in replication (published, subscribed, a distributor), or a ledger database;
- a server trigger on `CREATE`, `ALTER` or `DROP DATABASE`, which may refuse what a seed and a restore do: disable it while DriftGate runs;
- a table with row-level security, an enabled security policy with a filter or block predicate: it hides rows from DriftGate's reads, or blocks its writes. Turn the policy off in this database, or exclude the table;
- memory-optimized tables with `DURABILITY = SCHEMA_ONLY` that are not excluded, as their rows are in no backup;
- excluded memory-optimized tables whose rows are kept (`SCHEMA_AND_DATA`): SQL Server does not let their rows be saved through a restore of the whole database.

Change tracking is noted, not refused: a restore resets the rows, not
the change tracking versions an application syncs from.

## The pristine copy and seed

The pristine copy is a second database on the same server,
`<working>_pristine` by default, read-only (`READ_ONLY`). An extended
property, `driftgate_pristine`, marks it as DriftGate's. The mark names
the working database, the server (`@@SERVERNAME`) and the backup made of
it. A restore uses only a pristine copy with that mark for this working
database and this server, and stops when another seed replaced it while
DriftGate ran.

`driftgate seed`:

1. backs up the working database with `BACKUP … WITH COPY_ONLY`, so the application's own backup chain stays as it was. The backup is compressed, except on Express, with checksums.
2. restores that backup as `<pristine>_dgnew`, every file moved to a new name in the server's data folder (`dg_<hash>_<file>.mdf`, `.ndf`, `.ldf`), never over an existing database.
3. deletes the rows of the excluded tables there, without firing a trigger or running a cascade.
4. backs `<pristine>_dgnew` up into one of two backup files in the server's default backup folder, the one the current copy does not use. So that copy keeps a valid backup until the new one replaces it. A restore of the whole database restores from it.
5. shrinks the copy's log: the backup keeps the working database's log size, and the read-only copy needs none of it.
6. marks the copy, drops the old pristine copy, renames the new one and makes it read-only.

- The backup files are `driftgate-<hash>-a.bak` and `-b.bak`; on SQL Server 2017 they are in the data folder.
- A seed stopped while the step 2 `RESTORE` runs drops the copy, and the next seed drops one that is left.
- When the process is killed before SQL Server registered the copy, its `dg_*` files can stay in the data folder. Only a login with access to that folder can delete them.
- In step 3, a table that is not excluded and whose rows refer to rows of an excluded table by a trusted foreign key stops the seed. Exclude it too, or drop the key.
- A seed that stops anywhere leaves the old copy, or none, and a `<pristine>_dgnew` the next seed drops.
- A database under the `pristine` name that DriftGate did not make (one with objects, schemas or users and no mark) is never dropped.
- The working database is only read.

The server needs room for the pristine copy (about the working database
without its excluded rows) and the two backup files. During a seed it
also needs room for the new copy next to the old one. During a restore
of the whole database it needs room for a copy of the excluded tables'
rows. Count about twice the working database plus twice its excluded
data.

DriftGate cannot delete the backup files; that needs `sysadmin`. When a
project no longer uses DriftGate, drop the pristine database and delete
its two files yourself ([remove the backup files](#remove-the-backup-files)).

## Restore table by table

A restore brings back only the items that differ, in one transaction on
the working database (`XACT_ABORT`, a lock wait of at most 30 s). So the
application sees all of a restore or none of it, and keeps its
connections.

- The database's DDL triggers and the DML triggers of the tables it writes are disabled.
- The foreign keys that refer to those tables are set to `NOCHECK`, as are the constraints the pristine rows need not meet, as they are not trusted. So no trigger fires and no cascade runs.
- A table is emptied (`TRUNCATE` where SQL Server allows it, else `DELETE`), and its rows copied from the pristine copy with `IDENTITY_INSERT`.
- A table with a primary key and 5,000 rows or more gets only the rows that differ, deleted and copied by key, unless more than a quarter changed.
- Identity columns are set back where the pristine copy's stand (`DBCC CHECKIDENT … RESEED`), and sequences with `ALTER SEQUENCE … RESTART WITH`.
- Constraints are turned on again as they were: `WITH CHECK` where the pristine copy's are trusted, and disabled ones stay disabled. Then the triggers are.
- Identity and sequence values are compared with the pristine copy's before the commit.
- Full-text indexes are waited for, up to a minute, while SQL Server fills them again.

A large restore in one transaction is logged in full. With the `FULL`
recovery model, the working database's log keeps it until a log backup,
and grows. A development database is best in `SIMPLE` recovery.

## Restore of the whole database

Anything a restore cannot bring back item by item restores the whole
working database from the pristine copy's backup instead; the log says
why. That is:

- a view, procedure, function, trigger, type, synonym, schema, partition function or scheme, XML schema collection, rule or default that changed;
- a table created, dropped or with another definition;
- the `database:` item: users, roles, permissions, database options;
- a memory-optimized, temporal, graph, ledger, external or FileTable table, or one with a column of Always Encrypted or of a CLR type of its own;
- an identity column that cannot be set back where the pristine copy's stands.

Such a restore:

1. checks that the backup file is the one the mark names, and that the working database still has the files it had at the seed;
2. saves the rows of the excluded tables, and the excluded sequences, into `<pristine>_dgx`, a database of DriftGate's own (marked);
3. ends every other session of the working database (`SET OFFLINE WITH ROLLBACK IMMEDIATE`). A `sysadmin` login ends them first, which saves about 3 s per session;
4. restores the backup `WITH REPLACE` onto the working database's own files. Its owner, `TRUSTWORTHY` and `DB_CHAINING` are kept, as they live in `master`. Service Broker is turned on again when it was on, and the recovery model is set back;
5. copies the saved rows back, drops `<pristine>_dgx`, and checks every item against the pristine copy's fingerprint.

Measured on small databases, such a restore takes about 0.8 s.

- A restore of the whole database that stops halfway leaves the working database restoring or offline, unusable until the next restore (a capture) completes it.
- That restore copies the saved rows back too, and a seed refuses to run before it.
- A working database taken offline by someone else is brought online first.
- In any other state, with excluded tables whose rows DriftGate did not save, the restore stops rather than lose them.

### Pooled connections after a whole restore

The application's pooled connections to the working database are ended.
The first use of each one can fail once (go-mssqldb reports `Read: EOF`,
other clients a closed connection), and the pool connects again. Such a
failure is usually a 500 from the application.

DriftGate counts the sessions of other programs the restore ends, not its
own. It sends as many requests answered with 5xx once more:

- in the read-only part of the same capture that follows its first restore;
- or, after the last restore of a capture, in the next capture. The count is kept in the local store, at most 100, with the component, its working database, a hash of its server's address and the time.
- That count is used once, by a capture of the same database within an hour. The store is shared by every checkout of the project.
- Such a request is a `GET` or `HEAD` to a service on an allowed host. It is not a `repeat:`, a step of a sequence, or one the baseline knows writes (a `GET` can write).
- Each is sent at most twice, and the second answer is kept. A 5xx that stays is the application's; only a load failure such as a `503` is then [retried](../capture.md#retries-of-requests-that-failed-under-load).
- Every report notes it: `sent again …: the first answer was 500, after the restore of db ended 3 database sessions of the application`, and a count of such requests in every format ([reports](../reports.md#requests-sent-again-after-a-restore)).
- DriftGate sees the sessions of other logins only with `VIEW SERVER STATE`. Without it, it counts only those of an application that signs in as DriftGate's own login.

The count is of every session of other programs the restore ended in the
working database, not only the application's pool. A queue worker or an
open SSMS window adds to it. Close tools such as SSMS, Azure Data Studio
or a database client of the IDE on the working database while you
capture: each of their sessions allows one more request to be sent again.

- A request that changes data is never sent again. If it is the first after such a restore and fails this way, let the application's pool check a connection before it uses it, or keep no idle connections.
- `prepare` runs right after the restore, before any request. A command of its that asks the application over HTTP (a warm-up `curl`) meets the ended connections first, and stops the capture.
- Run such a step in a process of its own (`php artisan …`), or let it try again (`curl -sf --retry 3`, which retries a `500`).

### Limits while the database is RESTRICTED_USER

From step 2 until the rows are copied back, the working database is
`RESTRICTED_USER`. The application cannot connect, so none of its writes
to excluded tables is lost. The limits of that:

- A login that is a member of `db_owner`, `dbcreator` or `sysadmin` still gets in. Rows it commits to excluded tables after step 2 reads them, and before step 3 ends its session, are lost.
- Rows such a login writes after the restore are kept in a table with a primary key: a saved row with the same key is not copied back. In a table without a primary key (or with a computed column), its rows are replaced by the saved ones.
- Let the application sign in with a login that is only a user of the database ([the application's login](#the-applications-login)).
- A restore stopped while the database is `RESTRICTED_USER` (DriftGate killed) leaves it so, until the next restore or capture lets every login in again. `ALTER DATABASE … SET MULTI_USER` does it by hand.
- A working database you made `RESTRICTED_USER` or `SINGLE_USER` yourself stays so. `RESTRICTED_USER` is kept by the RESTORE itself.
- `SINGLE_USER` is set again at the end of the restore of the whole database, after its check, and only while the database is still the `RESTRICTED_USER` the RESTORE left. A `db_owner` session that got in meanwhile is ended.
- With excluded tables, the user access is noted in `<pristine>_dgx`, so a later restore that completes a stopped one sets it again too. Without excluded tables there is no note, and a restore stopped halfway leaves the database as the pristine copy's backup is (`MULTI_USER`).
- A capture never gets that far with a `SINGLE_USER` database. It fingerprints the database before its first restore and refuses it there, since a fingerprint reads on several sessions, and says what to do (`SET MULTI_USER`).

## What the fingerprint covers

The fingerprint covers:

- `table:<schema>.<name>`: its columns with every attribute, identity and where it stands, indexes, and constraints with their trusted and disabled flags;
- of the table also: temporal, graph and memory-optimized settings, row-level security policies, permissions, extended properties, and its rows;
- `view:`, `procedure:`, `function:` and `trigger:`, with a database's DDL trigger as `trigger:database.<name>`;
- `sequence:`, with where it stands;
- `type:`, `synonym:`, `schema:`, `rule:`, `default:`, `partition_function:`, `partition_scheme:` and `xml_schema:`;
- `database:` (below).

`database:` holds:

- the database's users and roles, with their members;
- the permissions and extended properties that belong to no other item: on the database itself, on `dbo` and on the other built-in schemas;
- the options that change what a query does: compatibility level, collation, `READ_COMMITTED_SNAPSHOT`, `ALLOW_SNAPSHOT_ISOLATION`, the ANSI defaults, `QUOTED_IDENTIFIER`, recursive and nested triggers, forced parameterization …;
- the database scoped configurations.

A difference in `database:` restores the whole database.

- A row is encoded on the server as its values' own bytes, and hashed by DriftGate (SHA-256). `CHECKSUM` is not used.
- So case and trailing spaces count, whatever the collation compares equal, as do a float's `-0` and last bit and a `datetime2`'s precision. NULL and `''` differ.
- A computed column counts by its definition.
- A column of a type DriftGate cannot read stops the fingerprint, naming it.
- Not covered: statistics (they change how a query runs, not what it returns), excluded tables, views and sequences, and the mark.
- Nor a sequence's `start_value`. A restore item by item sets it with `RESTART WITH`, and it decides only where a later `RESTART` without a value starts.

### rowversion values

`rowversion` values are not part of a table's rows. A restore writes rows
anew, and SQL Server gives them new values above the database's
`@@DBTS`, which only moves on. So a restored table has other values after
every restore.

- A response that shows them (an `ETag`, EF Core's concurrency token, a `timestamp` column in JSON) differs between captures, and needs an [allowlist](../allowlist-yaml.md) rule.
- An `UPDATE` that only gives a row a new `rowversion` is still a change: a table whose values moved since DriftGate's own last restore counts as changed.

### How fast a fingerprint is

A fingerprint is taken at one of three costs. The first one of a command
reads every table.

| Fingerprint | When | Time |
|---|---|---|
| reused | nothing moved since the last one | 1.5 ms |
| per table | the tables whose write counters moved are read again | 23 ms (one table of 100 written to) |
| full | every table read, on up to four connections | 0.39–0.56 s for 57.8 MB |

- "Nothing moved" covers the end of the log (after `sp_flush_log`), every identity column and sequence, and the database's options. A rolled-back `INSERT` and a cached sequence move them without a log record.
- A fingerprint is never reused with memory-optimized tables, nor while the application writes an excluded table (sessions, cache, jobs) on every request: the log moves, and the per-table cost applies.
- The write counters are `sys.dm_db_index_operational_stats`, `sys.dm_db_index_usage_stats`, the rows and the dates.
- Without `VIEW SERVER PERFORMANCE STATE`, every fingerprint is full ([fast fingerprints](#fast-fingerprints)).
- Memory-optimized tables, and tables with a `text`, `ntext` or `image` column, are read every time: some of their writes move no counter DriftGate reads.
- After a restart of the server, every table is read once.

A restore of a small table with one changed row takes 11–12 ms; of one
row in a table of a million rows (by key), about 190 ms. A seed of a
small database takes 0.7–1.3 s. Measured with SQL Server 2022 Developer
in Docker, emulated (`linux/amd64` under Rosetta on an Apple M4); the
server's side is several times faster on x86.

## Transactions

- A fingerprint reads committed data only (`READ COMMITTED`). It waits up to 10 s for a transaction of the application that wrote to the database and is still open.
- It reads the tables again when something committed while it read them, so it never mixes rows from before and after a commit.
- Without `VIEW SERVER PERFORMANCE STATE` it cannot see the open transactions. On a database with `READ_COMMITTED_SNAPSHOT`, a transaction that wrote before the reading and commits during it is not noticed.
- A restore holds its locks until it commits. The application's queries on the tables it writes wait for it.

## What to exclude

Exclude what the application writes on every request or outside
requests: session tables, job tables (Hangfire's `HangFire.*`), logs. A
cache (ASP.NET's SQL Server distributed cache, `dotnet sql-cache
create`) may be `restore_empty`.

- `init` and `setup` propose Hangfire's tables and the Data Protection keys ([the framework's own tables](../state.md#the-frameworks-own-tables)).
- An excluded table keeps its definition in the pristine copy, without rows.
- A restore item by item does not touch it. A restore of the whole database saves its rows and copies them back.
- A table that is not excluded and whose rows refer to an excluded table's rows stops the seed ([the pristine copy and seed](#the-pristine-copy-and-seed)).
- A foreign key of an excluded table to restored rows that are gone stays, no longer trusted, and the log says so.

## Queries for params

Queries (`params` `sql:`) run on the pristine copy (read-only), in a
transaction that is rolled back. A query must be one `SELECT` (or
`WITH … SELECT`) statement, in ASCII outside its quoted texts and names.
It may not have:

- comments, `INTO`, or any other statement's keyword. T-SQL needs no `;` between statements, so `SELECT 1 DROP TABLE x` is refused;
- `NEXT VALUE FOR`, which moves a sequence outside the transaction;
- names of three or four parts (another database or server); for `schema.table.column`, use an alias;
- the system procedures, functions and views that reach files, programs, other servers, sessions or logins: `sp_…`, `xp_…`, `fn_…`, `dm_…`, `OPENROWSET`, `OPENDATASOURCE`, `OPENQUERY`, `sys.sql_logins` ….

## Run SQL Server for DriftGate

### Errors 927 and 952

SQL Server 2019 and earlier stop a statement of one database with error
927 ("Database '…' cannot be opened. It is in the middle of a restore")
or 952 ("is in transition"). That happens when another database of the
server is restored, created or renamed at that moment, even for a
statement that names only its own database: another checkout's seed, or
a restore elsewhere.

The same happens with error 924 ("can only have one user at a time")
when another database is set single-user, as a seed or a drop does, and
with error 942 ("cannot be opened because it is offline") when another
database is offline, as a restore of the whole database takes its working
database.

- A seed, restore, fingerprint, `clean`, `generate` query or read of rows that fails so about another database runs again; the log says so once.
- It runs again for up to one minute, counted from the first such failure. After that it fails with the last error, which names the database. A database left in that state for good (restoring, offline or single-user) fails it every time: complete its restore, bring it online, give it `MULTI_USER` back, or drop it.
- A restore also runs again when the error names its own working database that a stopped restore left offline or restoring, within the same minute.
- Each stops in a state the next run completes.
- The application's own queries can fail the same way on those versions.

### Deadlocks

On every SQL Server version, a seed, restore, fingerprint, `clean` or
`generate` query that the server chooses as a deadlock victim (error
1205) runs again, up to three times. A retry that already gave up
inside another operation is not run again by it, so the total stays
bounded. The server rolled its transaction back, and a restore is still checked against the pristine copy's
fingerprint. One that keeps losing fails with error 1205.

### The application's login

Let the application sign in with a login that is only a user of its
database (`db_datareader`, `db_datawriter`, what its migrations need).
Not `sa`, and not DriftGate's login. During a restore of the whole
database, a member of `db_owner`, `dbcreator` or `sysadmin` still gets
in. Its writes to excluded tables in that time can be lost
([limits](#limits-while-the-database-is-restricted_user)).

Run the grants in a container with its own `sqlcmd`. Use
`/opt/mssql-tools` on SQL Server 2017; `-C` trusts the container's own
certificate:

```sh
docker compose exec mssql /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -Q "…"
```

### The sa password

The `sa` password of a container must have at least eight characters of
three kinds: upper case, lower case, digits, symbols. Otherwise the
container stops at its first start, and only its log
(`docker compose logs mssql`) says why.

### Apple Silicon

There is no SQL Server image for ARM. Run
`mcr.microsoft.com/mssql/server` for `linux/amd64` (`platform:
linux/amd64` in compose), which Docker Desktop runs under Rosetta.

- It is slower, and SQL Server 2017 and 2019 may stop under emulation.
- Azure SQL Edge, once the ARM choice, is retired and refused. Use the `mssql/server` image, or a SQL Server on another machine (`allow_remote: true`).

### Remove the backup files

A seed keeps two backups of the pristine copy: `driftgate-<hash>-a.bak`
and `-b.bak` in the server's default backup folder, `/var/opt/mssql/data`
in the image. To remove them after dropping the pristine copy:

```sh
docker compose exec mssql sh -c 'rm /var/opt/mssql/data/driftgate-*.bak'
```

A container without a volume loses them with its databases when it is
removed. The next `driftgate seed` makes them again.

### Certificate errors

A certificate error (`certificate signed by unknown authority`,
`certificate is not valid for …`) means the server's certificate is
verified, as the application's client does. Nothing makes DriftGate's
TLS weaker than the application's.

- For a container's or a development server's self-signed certificate, set `TrustServerCertificate=true` in the DSN, on a loopback address only, as the application's setting does.
- For a server with a certificate of your own CA, give the CA's file: `certificate=/etc/ssl/certs/my-ca.pem`, or `~/…`.
- For a certificate that names another host, set `hostNameInCertificate=<that name>`.

---
**Related:** [State: restoring the data](../state.md) · [Database connection settings](../database-settings.md) · [ASP.NET Core guide](../../guides/frameworks/aspnet-core.md) · [Security model](../../security.md)
