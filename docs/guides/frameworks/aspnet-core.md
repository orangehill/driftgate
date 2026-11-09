# ASP.NET Core and EF Core

> **Before you start:** [install DriftGate](../../start/install.md), and have the API running with `dotnet run`. **Time:** 40 minutes.

An ASP.NET Core API with its data in SQL Server through EF Core: the
project `Shop.Api` in `src/Shop.Api/`, on SQL Server 2022 in a container.

## Start SQL Server in a container

<!-- check: yaml -->
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

A local container is the simplest database to restore: `sa` may create
the pristine copy. Its self-signed certificate is trusted on a loopback
address only.

Keep the connection string in the user secrets (`dotnet user-secrets set
"ConnectionStrings:Default" "…"`) or `appsettings.Development.json`:

<!-- check: json -->
```json
{
  "ConnectionStrings": {
    "Default": "Server=localhost,1433;Database=Shop;User Id=sa;Password=…;TrustServerCertificate=True"
  }
}
```

EF Core 7 and later encrypt and verify the server's certificate by
default, so the container needs `TrustServerCertificate=True`. DriftGate
reads the client from the `.csproj`, and connects with the same TLS,
never a weaker one ([DSN and TLS](../../reference/databases/sql-server.md#dsn-and-tls)).

## Run setup

```sh
driftgate setup        # in the folder of the solution
```

`setup` recognises ASP.NET (`Microsoft.NET.Sdk.Web`), and writes:

- an `openapi` adapter: `/openapi/v1.json` (.NET 9 and later), or `/swagger/v1/swagger.json` with Swashbuckle ([OpenAPI](openapi.md));
- `base_url` from `Properties/launchSettings.json`: the first profile with `commandName: Project`, which `dotnet run` takes;
- `state:` from the connection string, read as the application reads it ([appsettings](../../reference/database-settings.md#appsettings)).

It asks before it makes the pristine copy, `Shop_pristine` on the same
server, then makes the first baseline. The configuration then reads:

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

- The password stays in the user secrets; `driftgate.yaml` names only the project file and the key.
- `seed:` runs the migrations, then the application's seeder if it has one. `setup` takes the data already there, so migrate first.

## A login on a shared server

```sql
CREATE LOGIN [shop] WITH PASSWORD = N'…';
USE [master]; ALTER SERVER ROLE [dbcreator] ADD MEMBER [shop];
USE [Shop];
IF SUSER_SID(N'shop') NOT IN (SELECT sid FROM sys.database_principals WHERE sid IS NOT NULL) CREATE USER [shop] FOR LOGIN [shop];
IF SUSER_SID(N'shop') <> (SELECT sid FROM sys.database_principals WHERE name = N'dbo') ALTER ROLE [db_owner] ADD MEMBER [shop];
USE [master]; GRANT VIEW SERVER PERFORMANCE STATE TO [shop];   -- optional: fast fingerprints
```

`sa` is for a container on this machine. On a shared development server,
give DriftGate a login of its own, limited to the working database and
its copies. An administrator runs what `setup` prints, as above.

- Without the last grant, every fingerprint reads every table, the slow part of a capture on a large database.
- Windows sign-in (`Trusted_Connection`, `Integrated Security`) is refused: DriftGate signs in with an SQL login only ([what the login needs](../../reference/databases/sql-server.md#what-the-login-needs)).

## What to exclude

Exclude what the application writes on every request or on its own, as
`exclude:` above does; its writes would make every fingerprint read it.
`setup` proposes these from the `.csproj`:

- the Data Protection key ring kept in the database (`PersistKeysToDbContext`), which the application writes when it makes or rotates a key;
- background job tables (Hangfire's `HangFire` schema), and the tables a `BackgroundService` writes.

The table of `AddDistributedSqlServerCache` is a cache: `restore_empty`
empties it at every restore. Used for sessions, excluding it instead
keeps a browser's session through restores
([restore_only](../../reference/state.md#restore_only)).

## Allow rowversion values

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: $*.rowVersion
    format: '[A-Za-z0-9+/]{11}='
    reason: SQL Server gives every row a restore writes a new rowversion; EF Core's concurrency token.
```

A `rowversion` column (`[Timestamp]`, `IsRowVersion()`) gets a new value
for every row a restore writes. Allow it with its format, 8 bytes in
base64; an `ETag` made from it needs a header rule too. Identity columns
and sequences are set back with the rows.

## When pooled connections fail after a restore

```ini
Pooling=False          # in the development connection string, if a write fails after a restore
```

Some changes, such as a migration applied at start-up with
`Database.Migrate()`, make DriftGate restore the whole database; the log
says `restoring the whole database`. That ends the application's
connections, and the first request on each can fail once. A read-only `GET` is sent again, and the report notes it; a
request that changes data is not. Apply migrations in `seed:`, not at
start-up ([pooled connections after a whole restore](../../reference/databases/sql-server.md#pooled-connections-after-a-whole-restore)).

## EF Core on SQLite

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

With `UseSqlite` and `Data Source=app.db`, the database is a file, taken
from the `.csproj`'s folder. Restores write inside it, and pooled
connections stay open. `Cache=Shared` is fine ([SQLite](../../reference/databases/sqlite.md)).

## The first run

```sh
driftgate run
```

The template's weather forecast returns random data, so the first run
proposes rules for it. Delete the template's endpoint, or allow the
values with a reason.

---
**Next:** [Read a drift report](../review/read-the-report.md): what the first comparison shows.
**Related:** [SQL Server](../../reference/databases/sql-server.md) · [State: restoring the data](../../reference/state.md) · [Upgrade a framework](../workflow/framework-upgrade.md)
