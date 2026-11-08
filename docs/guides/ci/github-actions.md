# Run the gate in GitHub Actions

> **Before you start:** commit a baseline ([share a baseline](../workflow/share-baselines.md)). **Time:** 30 minutes.

The action runs `driftgate run` against the application the job started,
and compares with the committed baseline. It reports in a pull-request
comment, updated on every push, the job summary, a JUnit file and an
artifact. The artifact `driftgate-report` holds the markdown, the JUnit file and the log.
See the [complete workflow](../../../examples/github-actions/driftgate.yml) and [every input](../../../examples/github-actions/README.md).

## Before the first job

1. **Make the baseline locally and commit it.** In CI, `driftgate run` without a baseline would make one from the code under test, and pass. The action stops with exit 2 when `driftgate/baselines/<baseline>` is missing; `require-baseline: false` turns that off.
2. **Use the same database server image and version** as for the baseline: state fingerprints are comparable only on the same server version.
3. **Decide what CI can compare**: private bodies never reach the runner ([below](#private-bodies-in-ci)).

## Add the job

<!-- check: yaml -->
```yaml
on: [pull_request, push]
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

- `version: source`, the default, builds DriftGate from the action's own ref in about a minute. A release tag or `latest` installs the binary only when its signature and checksum verify. `binary:` uses one on the runner.
- The seed runs inside the action, so the application must be up before the action starts.
- With several services in a matrix, give each job its own `comment-id` and `artifact-name`.

Outputs: `exit-code`, `result` (`clean`, `drift`, `config-error`, `environment-error`, `incomplete`), `report`, `junit`, `pristine-cache-hit`. Exit 3 is an environment error or an incomplete result, not a regression.

## Keys and secrets in CI

The job needs **no project key**: the action uses a fresh store and a
random key for every job.

- Never set `DRIFTGATE_KEY` (the action warns), production secrets or DSNs in a job that runs for pull requests, and never use `pull_request_target`. A pull request can change every command DriftGate runs ([security](../../security.md#keep-secrets-out-of-pull-request-jobs)).
- A configuration with `key.op` needs 1Password on the runner. Use `key.env` instead, or no `key:`.

## Private bodies in CI

Private bodies stay on the machine that captured them, so without them
the run is incomplete (exit 3). With `--skip-private`, the job compares
the status, headers, public bodies and side effects of every request
([private bodies](../../security.md#private-bodies)).

So decide which requests CI checks:

- Mark requests whose responses hold no personal data as public: `storage.public` in `driftgate.yaml`, or `"storage": "public"` in the catalog. The personal-data detector still scans every public body.
- Check private requests locally, where the bodies are.
- Or run CI against an [anonymized database](test-data.md), so that the responses hold no personal data and can be public.

## Cache the pristine database

<!-- check: yaml -->
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

A CI database starts empty, so the seed can take minutes per job. The
action keeps a dump of the seeded databases in the Actions cache, keyed
on the files the seed reads and `pristine-cache-key`.

- On a miss, the seed runs in full, then `pristine-dump` saves the working databases.
- On a hit, `pristine-load` loads the dump, and `seed --skip-commands` makes the pristine copies from it. A failed load warns and seeds in full.
- The key must cover everything the seed reads, and the image. A stale dump cannot pass as other data: the fingerprint check stops the capture (exit 3). It can hide a change to the seed itself, hence the key.

### SQLite

<!-- check: yaml -->
```yaml
          pristine-cache-key: sqlite
          pristine-dump: cp database/database.sqlite "$DRIFTGATE_PRISTINE_DIR/app.sqlite"
          pristine-load: cp "$DRIFTGATE_PRISTINE_DIR/app.sqlite" database/database.sqlite
```

An SQLite database needs no service; the cache is a copy of the file,
with the same `pristine-cache-files`. Load it before the application
starts: an open file is kept when it is replaced.

### SQL Server

<!-- check: yaml -->
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

This is the service as DriftGate's own CI starts it. `sqlcmd` is in
`/opt/mssql-tools18` (`/opt/mssql-tools` on 2017); `-C` trusts the image's
certificate. The cache is a backup of the working database, copied out,
and restored over it on a hit:

<!-- check: yaml -->
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

The copied file belongs to root; SQL Server 2019 and later read it as
`mssql` while it is readable by all (`docker cp` keeps mode 644).

## Other CI systems

```sh
export DRIFTGATE_STORE="$(mktemp -d)"
export DRIFTGATE_KEY="$(head -c32 /dev/urandom | base64)"   # a random key for this job, never the project key
driftgate run --skip-private; code=$?
driftgate report --format markdown > driftgate-report.md
driftgate report --format junit > driftgate-junit.xml
exit $code
```

The action wraps `driftgate run` and `driftgate report`; elsewhere, run
them in the job ([report formats](../../reference/reports.md#report-formats)).

---
**Next:** [Make anonymized test data for CI](test-data.md): public responses that CI can compare.
**Related:** [Exit codes](../../reference/cli.md#exit-codes) · [Security model](../../security.md) · [driftgate verify](../../reference/commands/review.md#driftgate-verify)
