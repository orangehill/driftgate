# Reference

Every reference page, one line each. Each page is complete for its
subject; guides link here for the rules and edge cases.

## Commands

- [Command line basics](cli.md): help, common flags, exit codes, environment variables, Ctrl-C and the store lock.
- [Set up a project](commands/project.md): `init`, `setup`, `key create`, `baselines`, `version`.
- [The pristine copy](commands/state.md): `seed`, `clean`.
- [Routes and the catalog](commands/catalog.md): `discover`, `generate`, `coverage`, `har import`, `record`.
- [Capture and compare](commands/check.md): `run`, `capture`, `compare`, `report`.
- [Review and record results](commands/review.md): `review`, `accept`, `expect`, `baselines rebase`, `verify`, `allowlist check`, `bisect`.
- [The data module](commands/data.md): `data classify`, `data anonymize`, `data subset`, `data verify`.

## Files

- [driftgate.yaml](driftgate-yaml.md): the configuration, every top-level key.
- [State: restoring the data](state.md): database components, seed and restore, fingerprints, `restore_only`.
- [Database connection settings](database-settings.md): DSNs, and reading them from the project's own settings.
- [allowlist.yaml](allowlist-yaml.md): the values that may change.
- [catalog.json](catalog-json.md): the requests every capture sends.
- [exclusions.yaml](exclusions-yaml.md): routes not checked, and known failures.
- [classify.yaml](classify-yaml.md): the column rules of the data module.

## How it works in detail

- [How a capture runs](capture.md): order, restores, retries, guards.
- [How responses are compared](comparison.md): levels, headers, bodies, pages, spreadsheets, side effects.
- [Reading a report in detail](reports.md): every line of a report, and each format.
- [Drift kinds](drift-kinds.md): what each kind means, and how it is usually fixed.
- [Upgrading DriftGate](upgrading-driftgate.md): what to do after you update DriftGate in a project.
- [Security model](../security.md): what a pull request can run, secrets, personal data.

## Databases

- [MySQL and MariaDB](databases/mysql-mariadb.md)
- [PostgreSQL](databases/postgresql.md)
- [SQLite](databases/sqlite.md)
- [SQL Server](databases/sql-server.md)

---
**Related:** [Documentation](../README.md) · [Glossary](../glossary.md) · [Commands](../commands.md)
