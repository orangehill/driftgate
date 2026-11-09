# SQL Server

DriftGate has SQL Server built in: the core of
[go-mssqldb](https://pkg.go.dev/github.com/microsoft/go-mssqldb), a pure
Go driver. So the binary needs no CGO and no client library.

## Supported versions

- SQL Server 2017 and later: Developer, Express, Standard and Enterprise, on Linux, Windows or in a container.
- Not Azure SQL Database, Azure SQL Managed Instance or Synapse: their databases cannot be backed up to a file and restored from it.
- CI tests SQL Server 2017, 2019, 2022 and 2025 (Developer, on Linux in Docker). SQL Server on Windows is not tested there.

SQL Server 2017 makes itself a certificate with a negative serial
number, which Go refuses by default, also with `TrustServerCertificate`.

- When a command makes its first SQL Server component, DriftGate sets `GODEBUG=x509negativeserial=1` for its own process and the commands it starts, the service's HTTPS included. Chains and host names are still verified.
- A project without SQL Server never has it, and an `x509negativeserial` you set yourself is kept.

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

- `exclude`, `restore_only`, `restore_empty`, `row_tracking` and `allow_remote` work as for [other databases](../state.md#database-components). Their globs match `schema.name` or `name`, without letter case; `row_tracking` names `schema.name`, or `name` in `dbo`, and needs a primary key.
- An `exclude` glob also leaves out views and sequences of those names; views and sequences are not `restore_only`.
- `dsn` is a go-mssqldb URL, an ADO.NET, ODBC or JDBC connection string, or the application's setting ([the application's settings](#the-applications-settings)).
- Database names are compared without letter case, as SQL Server does. `pristine` may not be `working` in any letter case, has at most 122 characters, and gets `<pristine>_dgnew` and `<pristine>_dgx` next to it.

## What the login needs

DriftGate signs in with an SQL Server login: a user name and a password,
which the server must allow, as the container images do. Windows
accounts (`Integrated Security`, `Trusted_Connection`, `DOMAIN\user`),
Kerberos, NTLM and Microsoft Entra ID (`Authentication=ActiveDirectory…`)
are refused by name: the driver is built without them.

| Right | Why |
|---|---|
| the server role `dbcreator` | creates the pristine copy, renames and drops its own copies, restores the working database |
| `db_owner` in the working database, or owning it | backs it up, and restores it item by item or whole |

- `dbcreator` reaches every database of the server: its login may take any offline, set it to single user, restore over it or drop it, though not read its data.
- `sa` or another `sysadmin` needs nothing more; use it only for a local, disposable server, such as a container.
- [`driftgate setup`](../commands/project.md#when-the-database-user-may-not-create-a-database) prints what an administrator runs for any other login. Every statement may run again:

```sql
USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop];
USE [Shop];
IF SUSER_SID(N'shop') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [shop] FOR LOGIN [shop];
IF SUSER_SID(N'shop') <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER [shop];
```

The login's own permissions are the real limit of what DriftGate's
queries can do ([security](../../security.md#the-queries-of-params)).

### The narrower grant

```sql
USE [master]; GRANT CREATE ANY DATABASE TO [shop]; ALTER AUTHORIZATION ON DATABASE::[Shop] TO [shop];
```

`setup` prints this too: the right to create databases and the
ownership of the working database, instead of both roles.

- The login then reaches only the databases it owns: the working database and its copies. Seeds and both kinds of restore work.
- It is not the default because the working database's owner changes: the login becomes `dbo` there, and whatever ran as the old owner (`EXECUTE AS OWNER`, cross-database ownership chaining) changes with it.
- It fails (error 15110) while the login has a user in that database. Drop it first (`USE [Shop]; DROP USER [shop];`); as `dbo` the login needs none.
- Either grant lets the login restore any backup file the server can read into a database of its own, and so read it. Keep other databases' backups out of the server's reach, its backup folder included.

### Fast fingerprints

`GRANT VIEW SERVER PERFORMANCE STATE TO [shop];` on SQL Server 2022 and
later, or `VIEW SERVER STATE` on 2017 and 2019, is optional; `setup`
proposes it with the grant.

- With it, a fingerprint of unchanged data is reused, and one after a change reads only the tables written to ([how fast](#how-fast-a-fingerprint-is)). It also waits for the application's open transactions ([transactions](#transactions)).
- Without it the result is the same, but every fingerprint reads every table; the log says so once.
- Either permission lets the login read every session of the server and its SQL text (`sys.dm_exec_requests`, the plan cache), literals included. DriftGate's queries refuse those views, but grant it only to a login the server's other users may trust with that.

## DSN and TLS

DriftGate's DSN is go-mssqldb's URL,
`sqlserver://user:pass@host:1433?encrypt=true`. A DSN of the environment
or 1Password may also be an ADO.NET connection string
(`Server=tcp:db,1433;Database=Shop;User Id=shop;Password=…`), an ODBC one
(`Driver={ODBC Driver 18 for SQL Server};Server=…`) or a JDBC URL
(`jdbc:sqlserver://db:1433;databaseName=Shop;user=…;password=…`).

- Each is read, and the URL written anew, as go-mssqldb would silently ignore a mistyped parameter such as `encrypt`.
- A URL parameter DriftGate does not know, one given twice, and a `;` in the query are refused; percent-encode a `;` as `%3B`.
- The URL's database is ignored; `working` names it.
- A go-mssqldb URL of the environment or 1Password without `encrypt` is encrypted and verified; for a self-signed certificate, add `TrustServerCertificate=true`.

The TLS DriftGate connects with is never weaker than the application's
own client. `Encrypt` (`true`, `yes`, `Mandatory`; `false`, `no`,
`Optional`; `Strict`), `TrustServerCertificate`,
`HostNameInCertificate`, a minimum TLS version and certificate files are
passed on; a value DriftGate cannot map is an error.

- **`Encrypt` off:** the sign-in is still encrypted, never sent without TLS. `encrypt=disable` and Prisma's `DANGER_PLAINTEXT` count as off. A server that answers it cannot encrypt (`ENCRYPT_NOT_SUP`) is refused before the password is sent, which go-mssqldb alone would send in clear. The data after the sign-in is not encrypted.
- With `Encrypt` off, the clients that never verify the certificate are followed (`TrustServerCertificate=true`): System.Data.SqlClient, ODBC Driver 17 and earlier, a Go application's go-mssqldb setting without `encrypt`, and FreeTDS with `encryption = off` or `request`.
- For other clients, a self-signed certificate needs `TrustServerCertificate=true` in the application's own setting; `init` and `setup` say so.
- **A setting that does not say** (`Encrypt` unset, a client DriftGate cannot tell) is encrypted and verified ([below](#the-tls-driftgate-uses-when-encrypt-is-unset)).
- FreeTDS's `encryption = require` encrypts without verifying, unless a CA is given.
- **A compose service** on a loopback address is trusted with its own self-signed certificate (`encrypt=true&TrustServerCertificate=true`), unless the setting says otherwise; on any other address it is verified.
- **`Strict`** (TDS 8) always verifies, and drops `TrustServerCertificate`. SQL Server 2022 and 2019 on Linux close a TDS 8 connection, and DriftGate does not fall back.
- JDBC's `sslProtocol=TLSv1.3` is `tlsmin=1.3`.

`tools/e2e/sqlserver-tls.sh`, run in CI for every version, starts SQL
Server with a test CA's certificate and forced encryption. It checks that
a verified connection works, and that a wrong CA, host or
`hostNameInCertificate` is refused without the password in any message;
CI checks `Strict` on SQL Server 2025.

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
(`ServerCertificate`, compared byte for byte), which does not go with
`HostNameInCertificate` or a CA.

- It is a regular `.pem` or `.der` file; convert a `.cer` or `.crt` with `openssl x509`. A Java trust store (`trustStore`) is refused.
- A project setting names one by a path relative to the project, which stays inside it, also past symbolic links, or anywhere by an absolute path or `~/…` (a system CA in `/etc/ssl/certs`).
- A DSN of the environment or 1Password names one by an absolute path, or one in the home folder.
- The driver only reads it as a certificate; no message shows what it holds.

## The application's settings

`init` and `setup` find SQL Server in these settings, and write the
`dsn:` that reads them
([from the project's own settings](../database-settings.md#read-the-dsn-from-the-projects-own-settings)).
The server is `host`, `host,port`, `tcp:host,port` or `host\instance,port`.
The local server (`.`, `(local)`, none) is taken as TCP to `127.0.0.1`,
with a note, as a Windows client may reach it through shared memory.

- **ASP.NET, EF Core** (`appsettings: <project>.csproj`, `key: "ConnectionStrings:<name>"`): the connection string, and the client from the `.csproj` ([the ASP.NET Core guide](../../guides/frameworks/aspnet-core.md)).
- **Laravel** (`dotenv: .env`): `DB_CONNECTION=sqlsrv`, `DB_HOST` (also `host\instance` or `host,port`), `DB_PORT` (default 1433), `DB_DATABASE`, `DB_USERNAME` and `DB_PASSWORD`, with `DB_URL` (`sqlsrv://`) over them.
- Laravel's `DB_ENCRYPT` and `DB_TRUST_SERVER_CERTIFICATE` count only when `config/database.php` passes them on, which ships commented out. Without that file, a value that weakens TLS is left out, with a note.
- **Spring, Quarkus, Micronaut** (`properties: …`): `jdbc:sqlserver://host:1433;databaseName=…;encrypt=…` (also `serverName`, `portNumber`, `instanceName`), with the user and password keys.
- **Django** (`python: settings.py`): `ENGINE` `mssql` (mssql-django) or `sql_server.pyodbc`; `HOST` (also `host\instance`) and `PORT`; `OPTIONS` `driver`, whose default `Encrypt` counts, and `extra_params` (ODBC keys). An ODBC `dsn` is refused.
- **Prisma** (`dotenv: .env`, `key: DATABASE_URL`): `sqlserver://host:1433;database=…;user=…;password=…;encrypt=…`, with a value that holds `;` in `{…}`.
- **Rails** (`yaml: config/database.yml`): `adapter: sqlserver` (activerecord-sqlserver-adapter), `host`, `port`, `database`, `username`, `password`.
- **Go** (`yaml: config.yaml`): a go-mssqldb URL with `?database=…`. A path with a port is taken for the database, as Rails reads it, with a note (go-mssqldb takes it for an instance); a path without a port is refused.
- **Node** (`dotenv: .env`, `key: …`): `mssql://user:pass@host:1433/db` (tedious, node-mssql).
- **compose** (`compose: compose.yaml`, `key: <service>`): an `mcr.microsoft.com/mssql/server` (or `mssql/rhel/server`) service, signed in as `sa` with `MSSQL_SA_PASSWORD` (or `SA_PASSWORD`), at the port it publishes for `MSSQL_TCP_PORT` (default 1433). The image names no database: `setup` takes the application's, when its setting names the same server. `MSSQL_SA_PASSWORD_FILE` and Azure SQL Edge are refused.
- A setting that names the server, database, user, password or port twice with different values (`Server` and `Data Source`, `Database` and `Initial Catalog`) is refused: the clients differ in which wins.
- Microsoft's ODBC drivers do not read `Port=` (write `Server=host,1433`); FreeTDS's does.

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

- The pristine copy is a read-only database on the same server, `<working>_pristine` by default.
- An extended property, `driftgate_pristine`, marks it with the working database, the server (`@@SERVERNAME`) and its backup.
- A restore uses only a copy marked for this working database and server, and stops when another seed replaced it meanwhile.

`driftgate seed` only reads the working database. It:

- backs it up `WITH COPY_ONLY`, so the application's own backup chain stays; compressed (not on Express), with checksums;
- restores that backup as `<pristine>_dgnew`, under new file names in the data folder (`dg_<hash>_<file>.mdf`, `.ndf`, `.ldf`), never over an existing database;
- deletes the excluded tables' rows there, without firing a trigger or running a cascade, and shrinks its log;
- backs the copy up into the one of two files (`driftgate-<hash>-a.bak`, `-b.bak`, in the default backup folder; on 2017 the data folder) that the current copy does not use, which a whole restore later reads;
- then marks it, drops the old copy, renames the new one and makes it read-only.

What can go wrong:

- A table that is not excluded and refers to an excluded table's rows by a trusted foreign key stops the seed. Exclude it too, or drop the key.
- A seed that stops anywhere leaves the old copy, or none, and a `<pristine>_dgnew` the next seed drops. Stopped during its `RESTORE`, it drops the copy itself.
- Killed before SQL Server registered the copy, its `dg_*` files can stay in the data folder; only a login with access to it can delete them.
- A database under the `pristine` name that DriftGate did not make (objects, schemas or users, and no mark) is never dropped.

- The server needs room for the copy (the working database without its excluded rows) and the two backup files.
- A seed also needs room for a new copy next to the old one, and a whole restore for the excluded rows: count about twice the working database plus twice its excluded data.
- Deleting the backup files needs `sysadmin`; when a project stops using DriftGate, drop the pristine database and [remove the files](#remove-the-backup-files) yourself.

## Restore table by table

- A restore brings back only the items that differ, in one transaction on the working database (`XACT_ABORT`, at most 30 s of lock wait).
- The application sees all of it or none, and keeps its connections.

- No trigger fires and no cascade runs: the DDL triggers and the written tables' DML triggers are disabled, and the foreign keys and untrusted constraints set to `NOCHECK`.
- A table is emptied (`TRUNCATE`, else `DELETE`) and filled from the pristine copy (`IDENTITY_INSERT`). One with a primary key and 5,000 rows or more gets only the rows that differ, by key, unless more than a quarter changed.
- Identity columns (`DBCC CHECKIDENT … RESEED`) and sequences (`ALTER SEQUENCE … RESTART WITH`) are set back to the pristine copy's, and checked before the commit.
- Constraints and triggers come back as they were: trusted ones `WITH CHECK`, disabled ones stay disabled.
- Full-text indexes are waited for, up to a minute.
- The restore is logged in full: with the `FULL` recovery model the log grows until a log backup, so use `SIMPLE` for a development database.

## Restore of the whole database

Anything a restore cannot bring back item by item restores the whole
working database from the pristine copy's backup; the log says why:

- a view, procedure, function, trigger, type, synonym, schema, partition function or scheme, XML schema collection, rule or default that changed;
- a table created, dropped or with another definition;
- the `database:` item: users, roles, permissions, database options;
- a memory-optimized, temporal, graph, ledger, external or FileTable table, or one with a column of Always Encrypted or of a CLR type of its own;
- an identity column that cannot be set back where the pristine copy's stands.

Such a restore takes about 0.8 s on a small database.

- It first checks that the backup file is the one the mark names, and that the working database still has the files it had at the seed.
- It saves the excluded tables' rows and sequences in `<pristine>_dgx`, a marked database of its own, and copies them back afterwards.
- It ends every other session of the working database (`SET OFFLINE WITH ROLLBACK IMMEDIATE`); a `sysadmin` login ends them first, which saves about 3 s per session.
- It restores `WITH REPLACE` onto the database's own files. Its owner, `TRUSTWORTHY` and `DB_CHAINING` stay, as they live in `master`; Service Broker and the recovery model are set back.
- Then every item is checked against the pristine copy's fingerprint.
- Stopped halfway, it leaves the working database restoring or offline until the next restore (a capture) completes it, saved rows included; a seed refuses to run before that.
- A working database taken offline by someone else is brought online first. In any other state, with excluded rows DriftGate did not save, the restore stops rather than lose them.

### Pooled connections after a whole restore

- The application's pooled connections to the working database are ended.
- The first use of each can fail once (go-mssqldb `Read: EOF`, other clients a closed connection), usually as a 500, and the pool connects again.

DriftGate counts the sessions of other programs the restore ended, and
sends that many requests answered with 5xx once more:

- in the read-only part of the capture after its first restore, or, after a capture's last restore, in the next capture of the same database within an hour. The count (at most 100, with the component, database, a hash of the server's address and the time) is kept in the local store, shared by every checkout.
- only a `GET` or `HEAD` to a service on an allowed host, not a `repeat:`, a sequence step, or one the baseline knows writes;
- at most twice, keeping the second answer. A 5xx that stays is the application's; only a load failure such as a `503` is then [retried](../capture.md#retries-of-requests-that-failed-under-load).
- Every report notes it, as `sent again …: the first answer was 500, after the restore of db ended 3 database sessions of the application`, and counts such requests in every format ([reports](../reports.md#requests-sent-again-after-a-restore)).
- Sessions of other logins are seen only with `VIEW SERVER STATE`; without it, only an application that signs in as DriftGate's login is counted.

- Every ended session counts, a queue worker or an open SSMS window too, so close tools such as SSMS, Azure Data Studio or an IDE's database client while you capture.

- A request that changes data is never sent again. If one fails so, let the pool check a connection before use, or keep no idle connections.
- A `prepare` command that asks the application over HTTP (a warm-up `curl`) meets the ended connections first and stops the capture. Run it in a process of its own (`php artisan …`), or let it retry (`curl -sf --retry 3`).

### Limits while the database is RESTRICTED_USER

While the excluded rows are saved and copied back, the working database
is `RESTRICTED_USER`, so the application cannot connect and lose writes.

- A member of `db_owner`, `dbcreator` or `sysadmin` still gets in. Its commits to excluded tables between the save and the end of its session are lost.
- Its rows written after the restore are kept in a table with a primary key, where a saved row with the same key is not copied back. In a table without one, or with a computed column, they are replaced by the saved ones.
- So let the application sign in with a login that is only a user of the database ([the application's login](#the-applications-login)).
- A restore killed meanwhile leaves the database `RESTRICTED_USER` until the next restore or capture; `ALTER DATABASE … SET MULTI_USER` does it by hand.
- A `RESTRICTED_USER` or `SINGLE_USER` you set yourself stays: `RESTRICTED_USER` is kept by the RESTORE, and `SINGLE_USER` is set again at the end, after the check, ending a `db_owner` session that got in.
- With excluded tables, the user access is noted in `<pristine>_dgx`, so a later restore that completes a stopped one sets it too. Without them, a stopped restore leaves the backup's `MULTI_USER`.
- A capture refuses a `SINGLE_USER` database before its first restore, as a fingerprint reads on several sessions, and says to `SET MULTI_USER`.

## What the fingerprint covers

- `table:<schema>.<name>`: its columns with every attribute, identity and where it stands, indexes, constraints with their trusted and disabled flags, temporal, graph and memory-optimized settings, row-level security policies, permissions, extended properties, and its rows;
- `view:`, `procedure:`, `function:` and `trigger:`, with a database's DDL trigger as `trigger:database.<name>`;
- `sequence:`, with where it stands;
- `type:`, `synonym:`, `schema:`, `rule:`, `default:`, `partition_function:`, `partition_scheme:` and `xml_schema:`;
- `database:`: its users and roles with their members, and the permissions and extended properties of no other item (of the database, `dbo` and the built-in schemas).
- Also in `database:`: the database scoped configurations, and the options that change what a query does. Those are compatibility level, collation, `READ_COMMITTED_SNAPSHOT`, `ALLOW_SNAPSHOT_ISOLATION`, the ANSI defaults, `QUOTED_IDENTIFIER`, recursive and nested triggers, forced parameterization ….

A difference in `database:` restores the whole database.

- A row is encoded on the server as its values' own bytes and hashed by DriftGate (SHA-256), not with `CHECKSUM`. So case and trailing spaces count whatever the collation compares equal, as do a float's `-0` and last bit and a `datetime2`'s precision.
- What the other drivers share, such as NULL against `''` and computed columns, is under [fingerprints](../state.md#fingerprints).
- A column of a type DriftGate cannot read stops the fingerprint, naming it.
- Not covered: statistics, excluded tables, views and sequences, the mark, and a sequence's `start_value`, which decides only where a `RESTART` without a value starts.

### rowversion values

- `rowversion` values are not part of a table's rows. A restore writes rows anew, and SQL Server gives them new values above the database's `@@DBTS`, so they differ after every restore.
- A response that shows them (an `ETag`, EF Core's concurrency token, a `timestamp` column in JSON) needs an [allowlist](../allowlist-yaml.md) rule.
- An `UPDATE` that only gives a row a new `rowversion` is still a change: a table whose values moved since DriftGate's last restore counts as changed.

### How fast a fingerprint is

| Fingerprint | When | Time |
|---|---|---|
| reused | nothing moved since the last one | 1.5 ms |
| per table | the tables whose write counters moved are read again | 23 ms (one table of 100 written to) |
| full | every table read, on up to four connections | 0.39–0.56 s for 57.8 MB |

The tiers are those of [fingerprints](../state.md#fingerprints).

- "Nothing moved" covers the end of the log (after `sp_flush_log`), every identity column and sequence, and the database's options; a rolled-back `INSERT` and a cached sequence move them without a log record.
- Never reused with memory-optimized tables, nor while the application writes an excluded table on every request: then the per-table cost applies.
- The write counters are `sys.dm_db_index_operational_stats`, `sys.dm_db_index_usage_stats`, the rows and the dates. Without [fast fingerprints](#fast-fingerprints), every fingerprint is full.
- Memory-optimized tables, and tables with a `text`, `ntext` or `image` column, are read every time. After a server restart every table is read once.
- A restore of one changed row: 11–12 ms in a small table, about 190 ms in a table of a million rows (by key). A seed of a small database: 0.7–1.3 s. Measured with SQL Server 2022 Developer in Docker under Rosetta on an Apple M4; x86 is several times faster.

## Transactions

- A fingerprint reads committed data only (`READ COMMITTED`). It waits up to 10 s for a transaction of the application that wrote to the database and is still open.
- It reads the tables again when something committed while it read them, so it never mixes rows from before and after a commit.
- Without `VIEW SERVER PERFORMANCE STATE` it cannot see the open transactions. On a database with `READ_COMMITTED_SNAPSHOT`, a transaction that wrote before the reading and commits during it is not noticed.
- A restore holds its locks until it commits. The application's queries on the tables it writes wait for it.

## What to exclude

Exclude what the application writes on every request or outside
requests: session tables, job tables (Hangfire's `HangFire.*`), logs. A
cache (ASP.NET's SQL Server distributed cache, `dotnet sql-cache create`)
may be `restore_empty`.

- `init` and `setup` propose Hangfire's tables and the Data Protection keys ([the framework's own tables](../state.md#the-frameworks-own-tables)).
- An excluded table keeps its definition in the pristine copy, without rows. A restore item by item does not touch it; a whole restore saves its rows and copies them back.
- A foreign key of an excluded table to restored rows that are gone stays, no longer trusted, and the log says so.
- A table that is not excluded and refers to an excluded table's rows stops the seed ([the pristine copy and seed](#the-pristine-copy-and-seed)).

## Queries for params

- Queries (`params` `sql:`) run on the pristine copy (read-only), in a transaction that is rolled back.
- A query must be one `SELECT` (or `WITH … SELECT`) statement, in ASCII outside its quoted texts and names.

It may not have:

- comments, `INTO`, or any other statement's keyword. T-SQL needs no `;` between statements, so `SELECT 1 DROP TABLE x` is refused;
- `NEXT VALUE FOR`, which moves a sequence outside the transaction;
- names of three or four parts (another database or server); for `schema.table.column`, use an alias;
- the system procedures, functions and views that reach files, programs, other servers, sessions or logins: `sp_…`, `xp_…`, `fn_…`, `dm_…`, `OPENROWSET`, `OPENDATASOURCE`, `OPENQUERY`, `sys.sql_logins` ….

## Run SQL Server for DriftGate

### Errors 927 and 952

SQL Server 2019 and earlier stop a statement of one database with error
927 ("Database '…' cannot be opened. It is in the middle of a restore")
or 952 ("is in transition") while another database of the server is
restored, created or renamed, such as by another checkout's seed.

Errors 924 ("can only have one user at a time") and 942 ("cannot be
opened because it is offline") stop it the same way, while another
database is set single-user or taken offline.

- A seed, restore, fingerprint, `clean`, `generate` query or read of rows that fails so runs again; the log says so once.
- It runs again for up to one minute, counted from the first such failure. Then it fails with the last error, which names the database.
- A database left restoring, offline or single-user for good fails it every time: complete its restore, bring it online, give it `MULTI_USER` back, or drop it.
- A restore also runs again, within the same minute, when the error names its own working database that a stopped restore left offline or restoring.
- Each stops in a state the next run completes.
- The application's own queries can fail the same way on those versions.

### Deadlocks

On every SQL Server version, a seed, restore, fingerprint, `clean` or
`generate` query that the server chooses as a deadlock victim (error
1205) runs again, up to three times. The server rolled its transaction
back, and a restore is still checked against the pristine copy's
fingerprint. A retry that gave up inside another operation is not run
again by it, so the total stays bounded. One that keeps losing fails
with error 1205.

### The application's login

Let the application sign in with a login that is only a user of its
database (`db_datareader`, `db_datawriter`, what its migrations need),
not `sa` and not DriftGate's login. During a whole restore, a member of
`db_owner`, `dbcreator` or `sysadmin` still gets in
([limits](#limits-while-the-database-is-restricted_user)). Run the grants
in a container with its own `sqlcmd` (`/opt/mssql-tools` on 2017; `-C`
trusts the container's certificate):

```sh
docker compose exec mssql /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P "$MSSQL_SA_PASSWORD" -Q "…"
```

### The sa password

The `sa` password of a container needs eight characters of three kinds
(upper case, lower case, digits, symbols); otherwise the container stops
at its first start, and only `docker compose logs mssql` says why.

### Apple Silicon

There is no SQL Server image for ARM: run `mcr.microsoft.com/mssql/server`
for `linux/amd64` (`platform: linux/amd64`), which Docker Desktop runs
under Rosetta. It is slower, and SQL Server 2017 and 2019 may stop under
emulation. Azure SQL Edge, once the ARM choice, is retired and refused;
use another machine's SQL Server (`allow_remote: true`) instead.

### Remove the backup files

```sh
docker compose exec mssql sh -c 'rm /var/opt/mssql/data/driftgate-*.bak'
```

A seed keeps two backups of the pristine copy, `driftgate-<hash>-a.bak`
and `-b.bak`, in the default backup folder (`/var/opt/mssql/data` in the
image). Remove them after dropping the pristine copy. A container without
a volume loses them when it is removed; the next seed makes them again.

### Certificate errors

A certificate error (`certificate signed by unknown authority`,
`certificate is not valid for …`) means the server's certificate is
verified, as the application's client does; nothing makes DriftGate's
TLS weaker than the application's.

- A self-signed certificate of a container or a development server: `TrustServerCertificate=true` in the DSN, on a loopback address only, as the application's setting does.
- A certificate of your own CA: `certificate=/etc/ssl/certs/my-ca.pem`, or `~/…`.
- A certificate that names another host: `hostNameInCertificate=<that name>`.

---
**Related:** [State: restoring the data](../state.md) · [Database connection settings](../database-settings.md) · [ASP.NET Core guide](../../guides/frameworks/aspnet-core.md) · [Security model](../../security.md)
