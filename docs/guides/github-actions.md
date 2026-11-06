# CI with the GitHub Action

The action (`action.yml` at the root of this repository) runs `driftgate
run` against the application started earlier in the job, compares with the
baseline committed in the repository, and reports the result as one
pull-request comment (updated on every push), in the job summary, as a
JUnit file and as a workflow artifact (`driftgate-report`: the markdown,
the JUnit file and the log).

A complete workflow for a Laravel API on MariaDB is in
[`examples/github-actions/driftgate.yml`](../../examples/github-actions/driftgate.yml),
and every input is described in
[`examples/github-actions/README.md`](../../examples/github-actions/README.md).

## Before the first job

1. **Make the baseline locally and commit it.** `driftgate run` without a
   baseline makes one; in CI it would be made from the code under test and
   pass. The action stops with exit 2 when `driftgate/baselines/<baseline>`
   is missing (`require-baseline: false` turns that off).
2. **Use the same database server image and version** as for the baseline:
   state fingerprints are comparable only on the same server version.
3. **Decide what CI can compare** (below): private bodies never reach the
   runner.

## The job

<!-- check: skip: a GitHub Actions workflow -->
```yaml
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
  pull-requests: write
jobs:
  driftgate:
    runs-on: ubuntu-latest
    services:
      db:
        image: mariadb:11.4           # the server the baseline was made on
        env: { MARIADB_ROOT_PASSWORD: dg, MARIADB_DATABASE: app }
        ports: ["3306:3306"]
    env:
      DG_DB_DSN: mysql://root:dg@127.0.0.1:3306/   # state.db.dsn: { env: DG_DB_DSN }
    steps:
      - uses: actions/checkout@v4
      # … install, start the application on 127.0.0.1 …
      - uses: orangehill/driftgate@v1.0.2   # pin a release tag or a commit SHA
        with:
          version: v1.0.2
          args: --skip-private
          junit: driftgate-junit.xml
```

- `version`: `source` (default) builds DriftGate from the action's own ref
  (about a minute; Go is set up by the action); a release tag or `latest`
  downloads the binary and installs it only when the Sigstore signature of
  `SHA256SUMS` and the checksum verify. `binary:` uses one already on the
  runner.
- The seed runs inside the action (`driftgate run` seeds when the pristine
  copy is missing); the application must be up before the action starts.
- With several services in a matrix, give each job its own `comment-id` and
  `artifact-name`.

Outputs: `exit-code`, `result` (`clean`, `drift`, `config-error`,
`environment-error`, `incomplete`), `report`, `junit`,
`pristine-cache-hit`. Exit 3 is reported as an environment error or an
incomplete result, not a regression.

## Keys and secrets

The job needs **no project key**. The action uses a fresh store for every
job and, unless `DRIFTGATE_KEY` is set, a random key, which encrypts the
private bodies captured in that job. Do not set `DRIFTGATE_KEY` in a job
that runs for pull requests (the action warns), and keep production
secrets and DSNs out of it: a pull request can change every command
DriftGate runs ([security](../security.md)). A configuration with `key.op`
needs 1Password on the runner; use `key.env` instead, or no `key:`.

Never use `pull_request_target`.

## Private bodies in CI

The baseline names private bodies only by a keyed hash; the bodies stay on
the machine that captured them. On the runner:

- every request whose baseline body is private is compared **without its
  body** (status, headers and side effects only) with the note `private
  body not on this machine`, and listed in the report;
- the run exits **3** (`incomplete`), even when the public requests are
  clean;
- `args: --skip-private` accepts a partial check: the exit code comes from
  what could be compared, a private request's status and headers included.
  If nothing could be compared at all, the result stays incomplete.

So decide which requests CI checks:

- mark requests whose responses hold no personal data as public
  (`storage.public` in `driftgate.yaml`, or `"storage": "public"` in the
  catalog); the personal-data detector still scans every public body;
- check private requests locally, where the bodies are;
- or run CI against an [anonymized database](test-data.md), so that
  responses hold no personal data and can be public.

## Pristine database cache

`driftgate seed` (migrations, seeders, a dump import) can take minutes on
every job, because a CI database starts empty. With `pristine-cache-files`
the action keeps a dump of the seeded databases in the Actions cache, keyed
on the hash of the files the seed reads and on `pristine-cache-key`:

<!-- check: skip: part of a GitHub Actions workflow -->
```yaml
      - uses: orangehill/driftgate@v1.0.2
        with:
          pristine-cache-files: |
            database/migrations/**
            database/seeders/**
            database/factories/**
            driftgate.yaml
          pristine-cache-key: mariadb-11.4
          pristine-dump: >-
            docker exec ${{ job.services.db.id }}
            mariadb-dump -uroot -pdg --single-transaction --routines --triggers --databases app
            > "$DRIFTGATE_PRISTINE_DIR/app.sql"
          pristine-load: >-
            docker exec -i ${{ job.services.db.id }} mariadb -uroot -pdg
            < "$DRIFTGATE_PRISTINE_DIR/app.sql"
```

- Cache miss: the seed runs in full, then `pristine-dump` saves the
  working databases.
- Cache hit: `pristine-load` loads the dump, then `driftgate seed
  --skip-commands` makes the pristine copies from it without running the
  seed commands. If the load fails, the action warns and seeds in full.
- The key must cover everything the seed reads, and the database image. A
  stale dump cannot pass silently as different data: every capture checks
  the restored state against the fingerprint recorded with the baseline and
  stops with exit 3. What a stale dump can hide is a change to the seed in
  the pull request itself, hence the key.

### SQLite

An SQLite database needs no service: the file is in the checkout (or made
by the seed). The pristine cache is a copy of the file:

<!-- check: skip: part of a GitHub Actions workflow -->
```yaml
      - uses: orangehill/driftgate@v1.0.2
        with:
          pristine-cache-files: |
            database/migrations/**
            database/seeders/**
            database/factories/**
            driftgate.yaml
          pristine-cache-key: sqlite
          pristine-dump: cp database/database.sqlite "$DRIFTGATE_PRISTINE_DIR/app.sqlite"
          pristine-load: cp "$DRIFTGATE_PRISTINE_DIR/app.sqlite" database/database.sqlite
```

Load it before the application starts: an application that has the file
open keeps the old one when it is replaced.

### SQL Server

The service as DriftGate's own CI starts it; `sqlcmd` is in
`/opt/mssql-tools18` (`/opt/mssql-tools` on 2017), and `-C` trusts the
image's own certificate:

<!-- check: skip: part of a GitHub Actions workflow -->
```yaml
    services:
      mssql:
        image: mcr.microsoft.com/mssql/server:2022-latest
        env:
          ACCEPT_EULA: "Y"
          MSSQL_PID: Developer
          MSSQL_SA_PASSWORD: Dg-Passw0rd-2026     # a test server's password
        ports: ["1433:1433"]
        options: >-
          --health-cmd="/opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P Dg-Passw0rd-2026 -Q 'SELECT 1' -b"
          --health-interval=5s --health-retries=30
    env:
      DG_DB_DSN: sqlserver://sa:Dg-Passw0rd-2026@127.0.0.1:1433?encrypt=true&TrustServerCertificate=true
```

The pristine cache is a backup of the working database, made in the
container and copied out, and restored over it on a hit:

<!-- check: skip: part of a GitHub Actions workflow -->
```yaml
          pristine-cache-key: mssql-2022
          pristine-dump: >-
            docker exec ${{ job.services.mssql.id }} /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P Dg-Passw0rd-2026 -b
            -Q "BACKUP DATABASE [app] TO DISK = N'/var/opt/mssql/data/app.bak' WITH COPY_ONLY, INIT, FORMAT"
            && docker cp ${{ job.services.mssql.id }}:/var/opt/mssql/data/app.bak "$DRIFTGATE_PRISTINE_DIR/app.bak"
          pristine-load: >-
            docker cp "$DRIFTGATE_PRISTINE_DIR/app.bak" ${{ job.services.mssql.id }}:/var/opt/mssql/data/app.bak
            && docker exec ${{ job.services.mssql.id }} /opt/mssql-tools18/bin/sqlcmd -C -S localhost -U sa -P Dg-Passw0rd-2026 -b
            -Q "RESTORE DATABASE [app] FROM DISK = N'/var/opt/mssql/data/app.bak' WITH REPLACE"
```

A file copied into the container belongs to root; SQL Server 2019 and
later run as the `mssql` user, which reads it as long as it is readable by
all (`docker cp` keeps the runner's mode, 644 by default).

## Other CI systems

The action is a wrapper around `driftgate run` and `driftgate report`:

```sh
export DRIFTGATE_STORE="$(mktemp -d)"
export DRIFTGATE_KEY="$(head -c32 /dev/urandom | base64)"
driftgate run --skip-private; code=$?
driftgate report --format markdown > driftgate-report.md
driftgate report --format junit > driftgate-junit.xml
exit $code
```
