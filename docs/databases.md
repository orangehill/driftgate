# Databases

A database under `state:` is restored to its pristine copy before every
capture. This page covers the four built-in drivers. The keys are in
[configuration](configuration.md#state), the idea in
[how it works](how-it-works.md#state).

## What every database needs

| Need | What to do |
|---|---|
| A copy, not production | A restore rewrites tables. Point `dsn` only at a database you may reset. |
| A local host | The host must be local, a socket, or in some service's `allow_hosts`. Otherwise set `allow_remote: true` on the component. |
| A DSN outside the file | `dsn` comes from `{env}`, `{op}` or the project's settings file ([DSN sources](configuration.md#dsn-sources)). |
| Rights to create the copy | `driftgate seed` creates the pristine database; the rights per engine are below. |
| Quiet tables excluded | Sessions, jobs and logs go in `exclude`; a cache in `restore_empty`. `init` and `setup` propose them. |
| A fresh seed after changes | Run `driftgate seed` after a migration, or after changing `restore_only` or `restore_empty`. |

- `seed` copies `working` to `pristine` (default `<working>_pristine`). Seed while the application does not write.
- `seed` replaces only a pristine copy it made itself; it never drops a database it did not mark.
- A capture checks the restored data against the pristine fingerprint. Different data stops it with exit 3.
- Before its first restore, a run compares the working schema with the pristine copy's. A migration run since the seed stops it with exit 3. With `prepare:` it goes on when `prepare` makes the change again. Rows and counters never count.
- The schema check does not cover data-only migrations, command components, MySQL routines and events, or an index on an excluded SQL Server table. On PostgreSQL it does not cover default privileges, publications, event triggers, large objects, database settings, statistics or comments on materialized views.
- PostgreSQL restores the whole database: a capture and `verify --migrate-state` also compare the restored database with the seed's record, which a pristine copy changed since fails. The throughput probe does not.
- Fingerprints are comparable only on the same server version. A server upgrade needs a new seed and baseline ([server upgrade](workflows.md#server-upgrade)).
- `driftgate clean` removes the pristine copies seed made ([clean](commands.md#clean)).

Give the DSN a user limited to the working and pristine databases. It
is the real limit of what a `params` query can do ([security](security.md#remote-and-live-databases)).

Versions tested in CI: MariaDB 10.3, 10.6, 11.4; MySQL 8.0, 8.4;
PostgreSQL 13, 16, 17; SQL Server 2017, 2019, 2022, 2025.

## MySQL and MariaDB

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql                 # also MariaDB
    dsn: { dotenv: .env, host: "127.0.0.1:3306" }
    working: shop
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
```

A DSN URL is `mysql://user:pass@host:3306/`. Percent-encode `@ : / ? # %`
in the user and password. The database in the URL is ignored.

| URL parameter | Meaning |
|---|---|
| `tls` | `true` (verify), `skip-verify`, `preferred` or `false` |
| `ssl-mode` | as the `mysql` client: `DISABLED` … `VERIFY_IDENTITY` |
| `ssl-ca` | a PEM file of the CAs to verify with |
| `timeout`, `readTimeout`, `writeTimeout` | durations such as `10s` |

Rights of the DSN user:

```sql
GRANT ALL ON shop_pristine.* TO 'shop'@'%';
```

| Right | Needed for |
|---|---|
| `CREATE`, `DROP` on the pristine database | `seed` |
| `SET_ANY_DEFINER` (MySQL 8.2+), `SET USER` (MariaDB) or `SUPER` | triggers and views whose `DEFINER` is another account |
| `SESSION_VARIABLES_ADMIN` (MySQL) or `BINLOG ADMIN` (MariaDB) | keeping restores out of the binary log |
| `PROCESS`, optional | faster fingerprints after writes |

Limits to know:

- Best: create triggers and views with the application's login as `DEFINER`. Otherwise a restore may stop and name the object.
- Without the binary log rights, restores are logged. A replica of this server then receives every restore.
- A Galera node refuses some MariaDB sequences. Seed stops and names them: make them `NOCACHE`, InnoDB, or exclude them.
- In a container, start the server with `--skip-name-resolve`. Otherwise every connection is slow.
- DriftGate keeps up to 13 connections per component open. Check `max_connections` with several components.

## PostgreSQL

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: postgres
    dsn: { env: DG_PG_DSN }       # postgres://shop:secret@127.0.0.1:5432/
    working: shop
    exclude: [sessions]
```

The URL takes PostgreSQL's own parameters (`sslmode`, `sslrootcert`, …).
libpq's keyword form (`host=… dbname=… user=…`) works too.

| Right | Needed for |
|---|---|
| `CREATEDB` (`ALTER ROLE shop CREATEDB`) | `seed` and restores |
| superuser, or member of the owner's role | keeping the working database's owner, settings and grants |

Limits to know:

- A restore copies the whole database from the pristine one and swaps it in. Keep room for a third copy.
- A restore ends the application's sessions on the working database. The application must reconnect.
- A write to a `restore_only` table makes the next restore copy the whole database. `restore_empty` tables are emptied in place.
- A `restore_empty` table with a `TRUNCATE` trigger, or one another table inherits from, stops the seed.
- The fingerprint also covers functions, enums, domains, schemas, materialized views and policies. Extension objects are left out.
- A partition the application creates at run time is a new table to the schema check. Seed after it, or let `prepare` create it.

## SQLite

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env }         # optional; it must name working
    working: database/database.sqlite
    exclude: [sessions, jobs, job_batches, failed_jobs]
    restore_empty: [cache, cache_locks]
```

| Key | Meaning |
|---|---|
| `working` | the file the application opens, relative to the project root; it must exist |
| `pristine` | optional; default a file in the local store, one per checkout |
| `dsn` | optional; a path, a `file:` URI, an `sqlite:` URL, or the application's settings file |

There are no users or rights. Limits to know:

- The file must be inside the project and on a local disk. Network shares, in-memory and encrypted databases are refused.
- Locks do not hold across NFS, SMB or Docker Desktop file sharing. Run the application on this machine.
- A relative path is taken as the framework takes it. Laravel 10 and earlier open a relative `DB_DATABASE` from `public/`; make it absolute.
- In rollback-journal mode, a fingerprint or seed makes application writes wait. Use WAL, or exclude what background workers write.
- Rails 8 has one file per database. Leave the cache, queue and cable files out of `state:`.
- `allow_remote` is refused: there is no server.

## SQL Server

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlserver
    dsn: { appsettings: src/Shop.Api/Shop.Api.csproj, key: "ConnectionStrings:Default" }
    working: Shop
    exclude: [dbo.SessionCache, "HangFire.*"]
```

SQL Server 2017 and later. Azure SQL Database, Managed Instance and
Synapse are not supported. The DSN is `sqlserver://user:pass@host:1433`,
or an ADO.NET, ODBC or JDBC connection string.

The login is an SQL login (user and password). Windows, Kerberos and
Entra ID sign-ins are refused. It needs:

| Right | Needed for |
|---|---|
| server role `dbcreator` | creating, restoring and dropping the copies |
| `db_owner` in the working database | backing it up and restoring it |
| `VIEW SERVER PERFORMANCE STATE` (2022+) or `VIEW SERVER STATE`, optional | faster fingerprints; waiting for open transactions |

`driftgate setup` prints the grants for a login. `dbcreator` reaches
every database on the server; `sa` is for a disposable container only.

The narrower grant gives the login only the databases it owns:

```sql
USE [master]; GRANT CREATE ANY DATABASE TO [shop]; ALTER AUTHORIZATION ON DATABASE::[Shop] TO [shop];
```

TLS is never weaker than the application's own client. A self-signed
certificate needs `TrustServerCertificate=true`, in the application's
setting or the DSN.

Limits to know:

- A changed view, procedure, table definition or permission restores the whole database from a backup. That ends the application's connections.
- After a whole restore, a request that fails once on a dead pooled connection is sent again. Close SSMS and IDE clients while you capture.
- Let the application sign in as a plain database user, not as `sa` or DriftGate's login.
- Refused: availability groups, replication, change data capture, ledger databases, Always Encrypted and row-level security policies.
- A non-excluded table with a foreign key to an excluded table stops the seed.
- Use the `SIMPLE` recovery model; restores are fully logged.
- A working database in `RECOVERY_PENDING`, `SUSPECT` or another state that is not online, offline or restoring stops the run. Repair it first.
- Seed keeps two backup files, `driftgate-*.bak`, in the backup folder. Remove them yourself after dropping the pristine copy.
- On Apple Silicon, run the `linux/amd64` image; 2017 and 2019 may stop under emulation.
