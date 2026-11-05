# ASP.NET Core and EF Core on SQL Server

An ASP.NET Core API whose data is in SQL Server through Entity Framework
Core. The example is a project `Shop.Api` in `src/Shop.Api/`, run with
`dotnet run`, on SQL Server 2022 in a container. What DriftGate restores
and how is under [SQL Server](../configuration.md#sql-server).

## 1. The database

A container on this machine is the simplest database to restore: `sa` may
create the pristine copy, and when DriftGate reads the compose service it
trusts the container's self-signed certificate on a loopback address
only.

```yaml
# compose.yaml
services:
  mssql:
    image: mcr.microsoft.com/mssql/server:2022-latest
    environment:
      ACCEPT_EULA: "Y"
      MSSQL_PID: Developer
      MSSQL_SA_PASSWORD: ${MSSQL_SA_PASSWORD}
    ports: ["127.0.0.1:1433:1433"]
```

The application's connection string, in `appsettings.Development.json` or,
better, in its user secrets
(`dotnet user-secrets set "ConnectionStrings:Default" "…"`):

```json
{
  "ConnectionStrings": {
    "Default": "Server=localhost,1433;Database=Shop;User Id=sa;Password=…;TrustServerCertificate=True"
  }
}
```

EF Core 7 and later (Microsoft.Data.SqlClient 4 and later) encrypt and
verify the server's certificate by default, so the container's own
certificate needs `TrustServerCertificate=True` in the application's
string. DriftGate reads the client from the `.csproj` and connects with
the same TLS, never a weaker one: with EF Core 6 or System.Data.SqlClient
it follows their defaults instead (the table under
[SQL Server](../configuration.md#sql-server)).

## 2. init and setup

In the folder of the solution:

```sh
driftgate setup
```

`setup` recognises ASP.NET (`Microsoft.NET.Sdk.Web`) and writes:

- an `openapi` adapter: `/openapi/v1.json` (Microsoft.AspNetCore.OpenApi,
  .NET 9 and later) or `/swagger/v1/swagger.json` with Swashbuckle (see
  [frameworks with an OpenAPI description](openapi.md));
- `base_url` from `Properties/launchSettings.json` (the first profile with
  `commandName: Project`, the one `dotnet run` takes);
- `state:` from the connection string: `appsettings.json`, then
  `appsettings.<environment>.json` and, in Development, the user secrets,
  with the launch profile's `ConnectionStrings__*` over them. A compose
  service of the same server comes first when its `sa` can create the
  copy.

It then asks before it makes the pristine copy (`Shop_pristine`, a
read-only database on the same server), and goes on to the first
baseline ([setup](../commands.md#setup)). The configuration it ends with
looks like this:

<!-- check: config-fragment -->
```yaml
target:
  base_url: http://localhost:5123
discover:
  - adapter: openapi
    url: /openapi/v1.json
state:
  db:
    driver: sqlserver
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }
    working: Shop
    exclude: [dbo.DataProtectionKeys, "HangFire.*"]
    restore_empty: [dbo.SessionCache]
seed:
  - ["dotnet", "ef", "database", "update", "--project", "src/Shop.Api"]
```

- The password stays in the user secrets; `driftgate.yaml` names only the
  project file and the key, read when a command runs.
- `seed:` makes the data the captures start from: the migrations, then
  the application's own seeder if it has one. `setup` takes the data
  already in the database (`seed --skip-commands`); run the migrations
  first.

## 3. A login on a shared server

`sa` is for a container on this machine. On a shared development server,
give DriftGate's connection string a login of its own, limited to the
working database and the copies it makes. An administrator creates it
and runs what `setup` prints (the last line is the optional grant):

```sql
CREATE LOGIN [shop] WITH PASSWORD = N'…';
USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop];
USE [Shop];
IF SUSER_SID(N'shop') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [shop] FOR LOGIN [shop];
IF SUSER_SID(N'shop') <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER [shop];
USE [master]; GRANT VIEW SERVER PERFORMANCE STATE TO [shop];   -- optional: fast fingerprints
```

Without the last grant every fingerprint reads every table, which on a
large database is the slow part of a capture. Windows sign-in
(`Trusted_Connection`, `Integrated Security`) is refused: DriftGate signs
in with an SQL login only.

## 4. What to exclude

Exclude what the application writes on every request or outside
requests and what a reset would disturb while it runs; its writes would
make every fingerprint read it. `setup` proposes the last two from the
`.csproj`:

- the Data Protection key ring when it is kept in the database
  (`PersistKeysToDbContext`), which the application writes when it makes
  or rotates a key;
- background job tables (Hangfire's `HangFire` schema), and the tables a
  `BackgroundService` writes on its own.

The table of `AddDistributedSqlServerCache` (made by `dotnet sql-cache
create`), `dbo.SessionCache` above, is a cache: `restore_empty` empties
it at every restore and never compares it, so a cached value does not
carry over from one capture to the next
([restore_only](../configuration.md#restore_only)). Used for sessions,
excluding it keeps a browser's session through restores.

An excluded table keeps its definition in the pristine copy, without
rows, as does a restore_empty one; a restore_only table keeps its rows
there.

## 5. Concurrency tokens

A `rowversion` column (`[Timestamp] public byte[] RowVersion`, or
`IsRowVersion()`) gets a new value for every row a restore writes, so its
value in a response differs between captures, while the rows are the
same. Allow it, with its format (8 bytes in base64):

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: $*.rowVersion
    format: '[A-Za-z0-9+/]{11}='
    reason: SQL Server gives every row a restore writes a new rowversion; EF Core's concurrency token.
```

An `ETag` made from it needs a header rule of the same kind. Identity
columns and sequences are set back with the rows: the id the application
hands out after a restore is the one it handed out when the baseline was
captured.

## 6. Restores and the application's connections

Most requests change rows; a restore then copies back only the changed
tables, in one transaction, and the application's pooled connections stay
open. A change a restore cannot take back table by table (a view, a
procedure, a new table, a migration the application applies at start-up
with `Database.Migrate()`) restores the whole database from the pristine
copy's backup, which ends the application's connections: the first
request on each pooled connection can then fail once, as with any client
whose server ended its session, and the pool connects again. DriftGate
sends a read-only `GET` answered with 5xx then once more, as many as
the sessions the restore ended (a worker or an SSMS window on the
database counts too, so close such tools while you capture), and the report notes each and counts them (a 5xx
that stays is kept); a request that changes data is never sent again, so a write
that is the first request after such a restore can fail: turn pooling
off in development (`Pooling=False` in the connection string) when that
happens. The log says `restoring the whole database` when it happens. Apply
migrations in `seed:` rather than at start-up, and run `driftgate seed`
after adding one.

## 7. EF Core on SQLite

With `UseSqlite` and a connection string such as `Data Source=app.db`,
the database is a file. `setup` reads it from `appsettings.json` and takes
a relative `Data Source` from the `.csproj`'s folder, where `dotnet run`
runs:

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }
    working: src/Shop.Api/app.db
seed:
  - ["dotnet", "ef", "database", "update", "--project", "src/Shop.Api"]
```

- The pristine copy is a file of DriftGate's store; nothing is granted.
- Restores write inside the file while the application keeps it open; it
  sees the restored data on its next query, and its pooled connections
  stay open (there is no restore that ends them).
- `Cache=Shared` is fine; `Mode=Memory` (an in-memory database) and an
  encrypted file (`Password=`) are refused, and so is a file that
  `|DataDirectory|` names: give the path. See
  [SQLite](../configuration.md#sqlite).

## 8. The first run

```sh
driftgate run
```

The first run captures twice and makes the baseline. The template's
weather forecast returns random data, so the first `run` refuses the
baseline and proposes rules for it; delete the template's endpoint, or
allow the values with a reason.
