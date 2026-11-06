# PostgreSQL

How DriftGate seeds, restores and fingerprints a PostgreSQL database.
What every driver shares is in [State](../state.md).

## How seed and restore work

A restore copies the whole database from the pristine one, as a
template. `seed` marks the pristine database it makes with the
database's comment.

- A restore or seed makes the new database under the name `<name>__driftgate_new`, and swaps it in only when the copy is complete. A failed or interrupted restore leaves the working database as it was.
- The old database is renamed `<name>__driftgate_old`, and dropped while the capture goes on. From PostgreSQL 15 a drop forces a checkpoint.
- For that moment the server holds a third copy, so keep room for it.
- A working database that is missing is restored in full.
- PostgreSQL before 15 has no file copy of a database; restores use the default copy.
- A restore ends the application's sessions on the working database.
- `seed` needs `CREATEDB` ([what the database user needs](../state.md#what-the-database-user-needs)).

## What the copy keeps

The copy keeps what a copy of a template does not, read from the working
database before every restore and at seed:

- the working database's owner;
- its settings: `ALTER DATABASE … SET` and `ALTER ROLE … IN DATABASE … SET`;
- its privileges: `GRANT … ON DATABASE`;
- its comment and connection limit.

The pristine copy gets the working database's settings, not those of the
pristine database it replaces.

- The application's role thus keeps its rights. From PostgreSQL 15, the schema `public` belongs to the database's owner.
- Its sessions keep their `timezone` or `search_path`.
- The DSN's user needs the rights to give them: a superuser, or a member of the owner's role.

## What the fingerprint covers

Besides the tables, views, triggers and sequences every driver covers
([fingerprints](../state.md#fingerprints)), a PostgreSQL fingerprint
covers:

- functions and procedures;
- enum types, with their values in order;
- domains and schemas;
- materialized views, with their rows;
- row security and policies.

Objects of extensions are left out. Those objects are fingerprinted in
the current format; an older format is under
[an older state fingerprint format](../upgrading-driftgate.md#an-older-state-fingerprint-format).

## restore_only tables cost a full copy

PostgreSQL restores by copying the whole database. So a write to a
`restore_only` table alone makes the next restore copy it all, where an
excluded table is left as it is.

- That costs about 53 ms for a small database, 0.2 s for 20 MB, and seconds for gigabytes.
- It comes at every restore after such a write; with `--restore each`, before every unit.

`restore_empty` tables alone are emptied in place instead:
`TRUNCATE … RESTART IDENTITY`, waiting at most 2 s for a lock, then
checked against the pristine copy. That took 54 ms against 244 ms for
22 MB, independent of the database's size.

The whole database is copied instead when:

- a session holds a lock on them longer, such as a transaction left open after it read the cache (the copy ends it);
- a table has a `TRUNCATE` trigger;
- or the check fails.

A seed refuses a restore_empty table with a `TRUNCATE` trigger, and one
that a table which is neither restore_empty nor excluded inherits from.
Use `restore_only` there for tables that change rarely.

---
**Related:** [State: restoring the data](../state.md) · [restore_only](../state.md#restore_only) · [Database connection settings](../database-settings.md)
