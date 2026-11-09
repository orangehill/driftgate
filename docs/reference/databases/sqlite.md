# SQLite

An SQLite database is a file of the project. DriftGate has SQLite built
in: [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) with
SQLite 3.54.1, SQLite's own C code translated to Go. So the binary needs
no CGO, and locks the file as the application's SQLite does.

- Any SQLite 3 file works, in WAL or rollback-journal mode.
- CI tests it on Linux, macOS and Windows, with Python's `sqlite3` as the application in another process.

## Configure an SQLite file

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: sqlite
    dsn: { dotenv: .env }       # DB_CONNECTION=sqlite; it must name working
    working: database/database.sqlite
    exclude: [sessions, cache, cache_locks, jobs, job_batches, failed_jobs]
```

| Key | Type | Meaning |
|---|---|---|
| `driver` | `sqlite` | |
| `working` | path, required | the database file the application opens, relative to the project root |
| `pristine` | path | its pristine copy; default a file of the local store |
| `dsn` | a file of the project \| `{env}` \| `{op}` | optional: the setting the application reads, such as `{ dotenv: .env }` |
| `exclude`, `restore_only`, `restore_empty`, `row_tracking` | | as for [other databases](../state.md#database-components) |

- `working` must exist when a command runs; the application's migrations make it.
- The default `pristine` is `pristine/<name>-<hash>.sqlite` in the local store, next to the record of its fingerprint. So every checkout and working file has its own.
- A `pristine` you set is another file inside the project. Seed creates it, and its folder when that does not exist yet.
- `pristine` may not be `working`, nor one of its `-wal`, `-shm` and `-journal` files, also in another letter case.
- The file a `dsn` names must be `working` ([database settings](../database-settings.md#read-the-dsn-from-the-projects-own-settings)). So a changed `DB_DATABASE` stops a command (exit 2) instead of restoring a file the application no longer uses.
- An `env` or `op` value of `dsn` is a path, a `file:` URI or an `sqlite:` URL.
- In `row_tracking`, a row without a primary key is named by its rowid (`rowid=7`).
- `allow_remote` is refused: there is no server.

## Which file a framework setting names

`working` and `pristine` are relative to the project root, and inside it,
also past symbolic links.

- They, and a path from the application's settings, are resolved as the system opens them. A `..` after a symbolic link goes up from where the link leads.
- A link out of the project is refused.
- A value in the MySQL driver's form (`user:password@tcp(host)/db`, `user@/db`) is MySQL's, even when the user reads like a scheme (`file:`, `sqlite:`). So in a DSN or URL, a folder ending in `@` needs `file:///…`; a plain path does in `DB_DATABASE`, `working:` and `--database`.

A relative path is taken as the framework takes it. Where the
application opens it from the folder it runs in, `init` and `setup` add a
note to check it.

- **Laravel** `.env` (`DB_CONNECTION=sqlite`): `DB_DATABASE`, or the file of `DB_URL` / `DATABASE_URL`; unset, `database/database.sqlite`.
- A relative `DB_DATABASE` is taken from the project root, with a note. Laravel 11 and later open it from the folder PHP runs in when it is there, else from the root.
- Laravel 10 and earlier open it only from the folder PHP runs in: `public/` under `php artisan serve` and PHP-FPM. Leave it unset, or make it absolute.
- **Django** `settings.py` (`django.db.backends.sqlite3`): `BASE_DIR / 'db.sqlite3'`, `str(BASE_DIR / …)` or `os.path.join(BASE_DIR, …)`. `BASE_DIR` is as `startproject` writes it: `Path(__file__).resolve().parent.parent`, or `os.path.dirname` of `__file__`.
- A relative Django string is taken from `manage.py`'s folder. Other code is not run: set `working:`.
- **Symfony** `.env` (`DATABASE_URL="sqlite:///%kernel.project_dir%/var/data.db"`): `%kernel.project_dir%` is the Symfony project's folder. That is the folder of the `.env` file, or of the one above `config/` for a file under it.
- **Prisma** (`provider = "sqlite"` in `prisma/schema.prisma`): the `file:` URL of the variable its `url = env("…")` names, relative to `prisma/`, as Prisma takes it.
- **Rails** `config/database.yml` (`adapter: sqlite3`): relative to the Rails root; every SQLite database of `development` ([Rails 8](#rails-8-databases)).
- **ASP.NET** `appsettings.json` (`Data Source=app.db`): relative to the `.csproj`'s folder, where `dotnet run` runs.
- Any other setting: relative to the project root.

## The file must be on this machine

SQLite's locks keep a restore and the application's writes apart. So the
file must be on a local disk of the machine DriftGate runs on, opened by
an application on that machine.

- The locks do not hold across network file systems (NFS, SMB).
- Docker Desktop's file sharing (macOS, Windows) is not documented to keep them. `init` and `setup` note a compose service that mounts the file's folder into its container.
- Run the application on this machine, or on Linux, where a bind mount is the same file system.

Refused, besides a file outside the project:

- an in-memory database (`:memory:`, `mode=memory`, SQLAlchemy's `sqlite://`);
- a network share (`\\server\share\…`);
- a URL with a host, other than `file://localhost/`;
- an encrypted file (SQLCipher, a `Password=` of Microsoft.Data.Sqlite);
- a file that `|DataDirectory|` names.

## How a restore writes inside the file

A restore writes inside the working file, in one transaction, as the
application's own SQLite does. The file is never copied, renamed or
deleted, nor are its `-wal`, `-shm` and `-journal` files.

The application's open connections keep the file they opened. A copy or a
rename over it would leave them reading and writing the old file, with
`-wal` and `-shm` files that no longer match it. That would be a pass on
data that is not the restored data, and lost writes.

- Only the items that differ are restored, in one `BEGIN IMMEDIATE` transaction with the pristine file attached read-only. The application sees all of a restore or none of it.
- A table with the pristine definition is emptied and filled again with `INSERT … SELECT`, which SQLite copies as raw records. A table without an `INTEGER PRIMARY KEY` keeps its hidden rowids.
- The table gets its `sqlite_sequence` value back. A changed table is copied whole.
- A table whose definition (its or its indexes' `CREATE` statement) differs is dropped and created from the pristine file.
- Views and triggers are made again from it, and `user_version` and `application_id` set to its own.
- Nothing a restore does runs the application's logic. Foreign keys are off on DriftGate's connections, so deletes do not cascade.
- The triggers of the tables it writes are dropped before and created again after, inside the transaction.
- Excluded tables, SQLite's statistics and the mark of the pristine copy are not touched.
- A working file replaced on disk (deleted and made again, a copy written over it) is noticed at the next fingerprint, restore or seed. DriftGate then opens the new file and reads it in full.

## How seed copies the file

`driftgate seed` copies one moment of the working file (`VACUUM INTO`)
into `<pristine>.driftgate-new`. The working file is only read. Then it:

1. marks the copy with a table `driftgate_pristine`;
2. empties its excluded tables with `secure_delete`, so the rows of sessions and tokens do not stay in its free pages;
3. sets it to rollback mode (`DELETE`);
4. writes it to disk, and renames it over the pristine file.

A seed that stops before the rename leaves the old copy. A file at the
`pristine` path is replaced only when it has the mark, is empty, or is
the one DriftGate's record names. Any other file is someone's data:
seed stops and names it.

## What the fingerprint covers

- `table:<name>`: its `CREATE` statement, those of its indexes, its `sqlite_sequence` value and its rows;
- `view:` and `trigger:`;
- `pragma:user_version` and `pragma:application_id`.

- Every value is hashed with its storage class, so `1`, `1.0`, `'1'` and `x'31'` differ. What all drivers share is under [fingerprints](../state.md#fingerprints).
- A virtual table (FTS5, R*Tree) counts by its `CREATE` statement, and its rows by its shadow tables.
- A module DriftGate's SQLite does not have stops a restore, with the advice to exclude the table.
- Not covered: SQLite's statistics (`sqlite_stat1` …, which `ANALYZE` and `PRAGMA optimize` write) and temporary objects.
- A trigger of an excluded table is covered: its body may write any table.

## How fast a fingerprint is

The [tiers](../state.md#fingerprints), with page digests between reused
and per table (rows):

| Fingerprint | When | 1 MB | 50 MB | 500 MB |
|---|---|---|---|---|
| reused | nothing committed since the last one | 22 µs | 28 µs | 54 µs |
| page digests | something was written, but every table's pages hash as at its last reading | 0.87 ms | 30 ms | 302 ms |
| rows | the tables whose pages changed are read again | 3.0 ms | 150 ms | 1.13 s |
| full | every table read | 3.5 ms | 167 ms | 1.67 s |

- "Nothing committed" is told by `PRAGMA data_version`, the schema version, the file's identity, size and time, and no restore of DriftGate's own.
- Page digests are enough when only an excluded table was written.
- The rows case above changed one row of the large table.
- A database session or cache table written on every request moves `data_version` every time. The page digests then keep the fingerprint from reading the other tables.

Seed of 1, 50 and 500 MB: 12.8 ms, 124 ms and 1.33 s. A restore of a
small table with one changed row takes about 0.35 ms; of a table of about
490 MB, 4.5 s. Measured on an Apple M4 in WAL mode, on a file with one
large table with an index, twenty small tables and an excluded session
table.

## Locks and the application's writers

DriftGate's connections wait up to 10 s for a lock the application holds
(`busy_timeout`). They never change the file's journal mode or locking
mode, and never checkpoint its WAL.

- A fingerprint first waits for a write the application has under way (`BEGIN IMMEDIATE`). When one committed meanwhile, it looks again after 5 ms, up to 20 times, so a request that ends with several transactions is seen whole.
- Holding the write lock only for that moment, it starts read transactions on up to five connections. They then read one moment of the file while the application goes on.
- In WAL mode, the application's writers are not held up by a fingerprint or a seed.
- Rollback mode (`DELETE`) is SQLite's default, which Laravel and Django keep; Rails sets WAL. There a reading transaction keeps writers from committing.
- So for the duration of a scan or of a seed's copy, an application write waits, or fails with `database is locked` without a busy timeout. Exclude what background workers write, or switch the file to WAL.
- A restore is one write transaction: the application's writers wait for it, and its readers see the data before or after it.

## What to exclude

Exclude what the framework writes on every request or outside requests,
in the same file: sessions and jobs. A cache is `restore_empty`.

- `init` and `setup` propose them ([the framework's own tables](../state.md#the-frameworks-own-tables)).
- An excluded table keeps its definition in the pristine copy, without rows.
- `exclude`, `restore_only` and `restore_empty` may name tables the file does not have.

## Queries for params

Queries (`params` `sql:`) run on the pristine file, opened read-only
(`mode=ro`, `query_only`, defensive mode), on a connection that lets only
reads compile.

- Besides what the other drivers refuse, `ATTACH`, `DETACH`, `PRAGMA`, `VACUUM` and `REINDEX` are refused.
- So are the tables that read the file's pages: `sqlite_dbpage`, `dbstat`, `sqlite_dbdata`, `sqlite_dbptr`.
- So are the functions `load_extension`, `fts3_tokenizer`, `readfile`, `writefile` and `edit`.
- A `DATETIME` value comes back as the text SQLite holds.

## Rails 8 databases

Rails 8 has a file per database: `primary`, `cache`, `queue` and
`cable`. A new application splits them in production only.

- Each file of `development` is a component of its own. `init` and `setup` write the primary, and list the others.
- The cache, queue and cable databases (Solid Cache, Solid Queue, Solid Cable) change without a request changing data. Leave them out of `state:`, or exclude their tables; the cache's may be `restore_empty`.

---
**Related:** [State: restoring the data](../state.md) · [Database connection settings](../database-settings.md) · [driftgate seed](../commands/state.md#driftgate-seed)
