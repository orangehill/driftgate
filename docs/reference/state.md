# State: restoring the data

Every capture starts from the same data. The `state:` key of
[driftgate.yaml](driftgate-yaml.md#state) names what is restored before a
capture, and this page says how. Each driver has a page of its own:
[MySQL and MariaDB](databases/mysql-mariadb.md),
[PostgreSQL](databases/postgresql.md), [SQLite](databases/sqlite.md) and
[SQL Server](databases/sql-server.md).

## Database components

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { env: DG_DB_DSN }
    working: shop
    exclude: [sessions, cache]
```

Each entry under `state:` is a component, named by a simple name: a
database with a built-in driver, or
[anything else through commands](#restore-anything-else-with-commands).
A database is MySQL, MariaDB, PostgreSQL or SQL Server here; an SQLite
file takes the keys of [SQLite](databases/sqlite.md#configure-an-sqlite-file).

| Key | Type | Meaning |
|---|---|---|
| `driver` | `mysql` \| `postgres` \| `sqlite` \| `sqlserver` | the database server; `mysql` covers MariaDB |
| `dsn` | `{env}` \| `{op}` \| a file of the project | the server, never written in the file ([DSNs](database-settings.md)) |
| `working` | string, required | the database the application uses |
| `pristine` | string | its pristine copy; default `<working>_pristine` |
| `exclude` | list of globs | tables never fingerprinted or restored (sessions, caches, logs) |
| `restore_only` | list of globs | tables restored to the pristine copy's rows, but never fingerprinted ([below](#restore_only)) |
| `restore_empty` | list of globs | `restore_only` tables restored to no rows ([below](#restore_empty)) |
| `row_tracking` | list of tables | also record which rows and columns each request changes, without values |
| `allow_remote` | bool | allow a database host that is neither local nor named in some service's `allow_hosts` |

- `driver` also accepts `mariadb` for MySQL, and `mssql` for SQL Server.
- `pristine` must differ from `working`; on MySQL, MariaDB and SQL Server also in letter case.
- A `restore_only` table is no side effect and no part of the state a baseline is checked against. It is not an excluded table.
- A `restore_empty` table's pristine copy leaves its rows out, as an excluded table's: a cache, for example.
- A `row_tracking` table is not an excluded or restore_only table.
- A capture records which tables it tracked. Rows of a table the baseline did not track (`row_tracking` came after it) are not compared, and a note says to make the baseline again.
- `allow_remote` is under [Where the database may be](#where-the-database-may-be).

## How seed and restore work

- `driftgate seed`, or the first `run`, copies `working` to `pristine`. It records the fingerprint of the pristine copy, not of `working`, which the application may have changed since the copy.
- Seed while the application does not write. `seed` warns when `working` differs from the copy right after it was made.
- A capture restores `working` from `pristine` and checks it against the fingerprint. It stops with exit 3 when the data differ from the data the baseline was captured from.
- How a restore copies the data depends on the driver; the driver pages give the details.
  - MySQL and MariaDB: only the tables that changed, with their counters, triggers and views.
  - PostgreSQL: the whole database, from the pristine one as a template.
  - SQL Server: the tables and sequences that changed, else the whole database from the pristine copy's backup.

`seed` replaces only a pristine database it made itself. It marks the
one it makes: a table `driftgate_pristine` on MySQL and MariaDB, the
database's comment on PostgreSQL, an extended property of the database
on SQL Server.

- A database under the `pristine` name that has objects and no mark is not dropped. Seed stops and names it.
- A pristine database made by an earlier release has no mark. It is replaced when DriftGate's record of it names it.
- The record of the pristine fingerprint is removed right before the old pristine database is dropped or swapped out. A seed that fails or is interrupted after that leaves no half-made copy that a capture would trust: the capture asks for `driftgate seed`, and `run` seeds by itself.
- A seed that fails before that (no working database, a copy that cannot be made) leaves the old copy and its record as they were.

## What the database user needs

`seed` creates the pristine database, which an application's own login
often may not. Without these rights `seed` stops with the server's
refusal.

- MySQL and MariaDB need `CREATE` and `DROP` on the pristine database: `GRANT ALL ON shop_pristine.* TO 'shop'@'%'`, or `CREATE` on `*.*`. Triggers and views whose `DEFINER` is another account need more ([which](databases/mysql-mariadb.md#triggers-and-views-with-a-definer)).
- PostgreSQL needs `CREATEDB`: `ALTER ROLE shop CREATEDB`.
- SQL Server needs a login with `dbcreator` and `db_owner` in the working database, not `sysadmin` ([what the login needs](databases/sql-server.md#what-the-login-needs)).

Give the DSN a user limited to the working and pristine databases (for
PostgreSQL, with `CREATEDB`). Do not use a superuser, nor a user with
MySQL's `FILE` privilege.

- The one exception is a disposable database in a local container, a compose service on this machine. Its root, `POSTGRES_USER` or `sa` may be used, as `driftgate setup` does when the application's setting names that service.
- On a shared server, always use a limited user.
- A PostgreSQL restore ends the application's sessions on the working database.

Optional rights that make fingerprints faster are on the driver pages,
such as [the optional PROCESS privilege](databases/mysql-mariadb.md#the-optional-process-privilege)
on MySQL and MariaDB.

## Where the database may be

The database must be on this machine (a local address or a socket), or
on a host that some service's `allow_hosts` names. A private address is
not enough. Otherwise set `allow_remote: true` on the component.

- Every host the driver may connect to is checked: PostgreSQL `host=`, several hosts, `PG*` variables.
- Every connection is checked on the address it goes to.

The rule and its reasons are in the
[security model](../security.md#databases-must-be-copies).

## Fingerprints

A fingerprint is a hash of the state. Fingerprints are comparable only
on the same database server version.

- They cover tables (definition and rows), views, triggers and sequences.
- On MySQL and MariaDB, triggers and views are covered with the collation they were made with, which a restore keeps.
- PostgreSQL and SQL Server cover more objects: see [PostgreSQL](databases/postgresql.md#what-the-fingerprint-covers) and [SQL Server](databases/sql-server.md#what-the-fingerprint-covers).
- A request that changes one of them is a side effect, and the next restore brings it back.

A pristine copy or a baseline whose fingerprint has an older format
keeps working while nothing it covers differs; what to do otherwise is in
[an older state fingerprint format](upgrading-driftgate.md#an-older-state-fingerprint-format).

## restore_only

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { dotenv: .env }
    working: shop
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
```

What the application writes on every request or outside requests (a
session or cache table, a job queue) has two other choices. `exclude`
never compares and never restores it: sessions and queued jobs carry
over from one capture to the next. Tracking restores it, but makes it a
side effect of every request. `restore_only` restores such tables like
the others and never compares them.

- `driftgate seed` keeps their rows in the pristine copy (an excluded table's are left out), and records their fingerprint apart from the others'.
- A restore reads them every time, and restores those that differ from the pristine copy's record, with the items that changed.
- They are no item of a fingerprint. A write to them is no side effect and no difference from the baseline's state. The check after the restore leaves them out, as the application may write them at any moment.
- A write to them never hides a change to another table: the other tables are fingerprinted as without them.
- A table is excluded or restore_only, not both, and not under `row_tracking`.
- Names follow `exclude`: globs; SQLite and SQL Server without letter case; PostgreSQL and SQL Server with or without the schema.
- PostgreSQL: the sequences of their `serial` and identity columns are restore_only too.
- SQLite: the shadow tables of a restore_only virtual table are restore_only too; those of a fingerprinted one never are, whatever their names.
- A trigger of a restore_only table stays an item.
- SQL Server: a restore of the whole database takes them from the pristine copy's backup. They are not saved and copied back as excluded tables are.
- Changing `restore_only` or `restore_empty` needs `driftgate seed`, and a capture says so. The pristine copy holds the rows of the tables restore_only names, not of excluded or restore_empty ones.
- A table moved from tracking to them is no longer in the state a baseline was captured from. Make the baseline again; a capture says so.

The check costs one read of those tables per restore. Restoring them all
every time instead was measured slower: sessions of 200 rows, a cache of
1,000 and jobs of 50, next to twenty tables of 100 rows.

| Database | Check | Restore of all three |
|---|---|---|
| SQLite | 0.58 ms | 0.88 ms |
| MySQL | 5.1 ms | 73 ms |
| PostgreSQL | 17 ms | 53 ms |

On PostgreSQL a write to a restore_only table costs a copy of the whole
database ([restore_only tables cost a full copy](databases/postgresql.md#restore_only-tables-cost-a-full-copy)).

### restore_empty

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { env: DG_DB_DSN }
    working: shop
    restore_empty: [cache, cache_locks]
```

`restore_empty` implies `restore_only`: everything above holds for its
tables, whose pristine content is no rows, as for a cache.

- The seed leaves their rows out of the pristine copy, as an excluded table's. The cache the developer's application filled, with entries that never expire, does not become the data every capture starts from.
- Every restore empties them when they have rows.
- A table that refers to their rows by a foreign key stops the seed on SQL Server and PostgreSQL.

## The framework's own tables

`init` and `setup` propose, for every driver, the tables the framework
keeps its own state in, from the project's settings. `setup` asks
(default yes); without a terminal it needs `--yes`, or stops.

- Excluded: what a reset would disturb while DriftGate runs and no response depends on, such as the developer's own session in the browser, a running queue worker, or monitoring.
- `restore_empty`: a cache, whose entries (the counters of a rate limit too) would otherwise carry over from one capture to the next and change its responses.

Laravel (`artisan`), for the setting of the default connection:

- `sessions` is excluded when `SESSION_DRIVER` is `database` on that connection, or unset with `database` as the default. The default comes from `config/session.php`, else the framework's since Laravel 11.
- `jobs` is excluded in the same way for `QUEUE_CONNECTION` (`QUEUE_DRIVER` before Laravel 5.7) and `config/queue.php`.
- The tables `SESSION_TABLE` and `DB_QUEUE_TABLE` name are excluded instead, when set.
- `job_batches` is excluded, and `failed_jobs` unless `QUEUE_FAILED_DRIVER` is `null`, `file` or `dynamodb`.
- Telescope's tables (`telescope_entries`, `telescope_entries_tags`, `telescope_monitoring`) and Pulse's (`pulse_values`, `pulse_entries`, `pulse_aggregates`) are excluded when `composer.json` requires them.
- `cache` and `cache_locks` are `restore_empty` when `CACHE_STORE` (`CACHE_DRIVER` before Laravel 11) is `database` in the same way. So are the tables `DB_CACHE_TABLE` and `DB_CACHE_LOCK_TABLE` name.

Django:

- `django_session` is excluded, with database sessions.
- The `LOCATION` of a `DatabaseCache` is `restore_empty`.

Rails, for Solid tables in the primary database (`db/schema.rb` or
`db/structure.sql`; a new Rails 8 application keeps them in databases of
their own in production):

- `solid_queue_*` and `solid_cable_messages` are excluded, and `sessions` with `activerecord-session_store` in the `Gemfile`.
- `solid_cache_entries` is `restore_empty`.

ASP.NET, for every `.csproj` of the solution, in the root and up to
three folders below it:

- Hangfire's schema is excluded: `HangFire.*` on SQL Server with `Hangfire` or `Hangfire.SqlServer`, `hangfire.*` on PostgreSQL with `Hangfire.PostgreSql`.
- `DataProtectionKeys` is excluded with `Microsoft.AspNetCore.DataProtection.EntityFrameworkCore`.

## Restore anything else with commands

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: postgres
    dsn: { env: DG_PG_DSN }
    working: shop
    pristine: shop_pristine
  files:
    restore: ["rsync", "-a", "--delete", "storage-pristine/", "storage/app/"]
    fingerprint: ["sh", "-c", "find storage/app -type f | sort | xargs shasum"]
```

Anything else (Redis, files, a search index) is a component with
`restore`, required, and optionally `fingerprint`: a command whose
output must be the same after every restore
([recipes](../guides/catalog/side-checks.md)). `dsn`, `working`, `pristine`,
`exclude`, `restore_only`, `restore_empty` and `row_tracking` need a
driver.

A cache emptied on every restore (`FLUSHALL`) means every request runs on
a cold cache ([what that leaves unchecked](../concepts/what-a-pass-proves.md#a-cache-emptied-at-every-restore)).

A catalog with requests that change data needs at least one component
with a restore. Otherwise `capture` and `run` refuse it (exit 2), unless
`--allow-mutating-without-restore`
([requests that change data](capture.md#requests-that-change-data)).

## Supported database versions

Versions tested in CI:

- MariaDB 10.3, 10.6 and 11.4;
- MySQL 8.0 and 8.4;
- PostgreSQL 13, 16 and 17;
- SQL Server 2017, 2019, 2022 and 2025.

Version notes are on the driver pages, such as
[generated and virtual columns on MariaDB](databases/mysql-mariadb.md#generated-and-virtual-columns-on-mariadb).

---
**Related:** [driftgate seed](commands/state.md#driftgate-seed) · [state in driftgate.yaml](driftgate-yaml.md#state) · [Database connection settings](database-settings.md) · [Security model](../security.md)
