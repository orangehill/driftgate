# DriftGate in GitHub Actions

The DriftGate action (`action.yml` at the root of this repository) runs
`driftgate run` against the application started earlier in the job, compares
the responses with the baseline committed in the repository, and reports the
result:

- in the job summary, as markdown;
- as a comment on the pull request, updated on every push instead of a new
  comment each time;
- as a JUnit file for test reporters;
- as a workflow artifact (`driftgate-report`: the markdown, JUnit and the log).

The job fails unless the run is clean, with DriftGate's exit codes:

| exit | result | meaning |
|---|---|---|
| 0 | `clean` | no drift |
| 1 | `drift` | responses or routes differ from the baseline |
| 2 | `config-error` | configuration, catalog or baseline not usable (also: no baseline committed) |
| 3 | `environment-error` | the application, the database or the state could not be prepared; **not a regression**, the result cannot be trusted |
| 3 | `incomplete` | private requests could not be compared on the runner (below); the rest was compared and is in the report |

The report is written and the comment posted in every case, the failing ones
included. When the run stops before a comparison (exit 2 or 3), the report says
why and shows the end of the log.

[`driftgate.yml`](driftgate.yml) is a complete workflow for a Laravel API on
MariaDB: a service container for the database, the application started with
`php artisan serve`, then the action.

## Before the first run in CI

Make the baseline locally and commit it: `driftgate run` with no baseline
makes one, and in CI that would be made from the code under test and pass.
The action therefore stops with exit 2 when `driftgate/baselines/<baseline>`
is missing (`require-baseline: false` turns that off).

Use the same database server image and version in CI as for the baseline:
state fingerprints are compared only on the same server version.

## Inputs

| input | default | |
|---|---|---|
| `version` | `source` | `source` builds driftgate from the action's own ref (Go is set up by the action); a release tag downloads `driftgate-<os>-<arch>` from the action's releases |
| `binary` | | path to a driftgate already on the runner (then `version` is ignored) |
| `working-directory` | `.` | where driftgate runs; `config`, `junit` and the commands are relative to it |
| `config` | `driftgate.yaml` | |
| `baseline` | `local` | |
| `require-baseline` | `true` | exit 2 when the baseline is not committed |
| `args` | | more arguments for `driftgate run`, e.g. `--target api` or `--skip-private` |
| `store` | | `DRIFTGATE_STORE`; empty: a fresh folder per job |
| `comment` | `always` | `always`, `changes` (no new comment for a clean run; an existing one is still updated) or `off` |
| `comment-id` | `default` | keeps comments of several jobs (a matrix) apart |
| `pr-number` | | pull request to comment on for events other than `pull_request` |
| `github-token` | `github.token` | |
| `junit` | | also copy the JUnit report here |
| `summary` | `true` | report in the job summary |
| `upload` | `true` | upload the reports and the log |
| `artifact-name` | `driftgate-report` | must differ between jobs of a matrix |
| `pristine-cache-files` | | turns the pristine cache on (below) |
| `pristine-cache-key` | | extra key text, e.g. the database image |
| `pristine-dump` | | shell command: dump the working databases into `$DRIFTGATE_PRISTINE_DIR` |
| `pristine-load` | | shell command: load them back |

Outputs: `exit-code`, `result`, `report` (path of the markdown),
`junit` (path of the JUnit file), `pristine-cache-hit`.

Comments need `permissions: pull-requests: write`. A pull request from a fork
gets a read-only token; the action then only warns that it cannot comment.

## Pristine database cache

`driftgate seed` runs the project's seed commands (migrations, seeders, a
dump import) and makes the pristine copy of each database. On a large project
that takes minutes, on every run, because a CI database starts empty.

With `pristine-cache-files`, the action keeps a dump of the seeded databases
in the Actions cache, keyed on the hash of the listed files and on
`pristine-cache-key`:

- **cache miss**: `driftgate seed` runs in full, then `pristine-dump` writes
  the working databases (equal to their pristine copies right after the seed)
  into `$DRIFTGATE_PRISTINE_DIR`, and the folder is saved under the key;
- **cache hit**: `pristine-load` loads the dump into the working databases,
  then `driftgate seed --skip-commands` makes the pristine copies from them
  and records their fingerprints, without running the seed commands.

If the load fails, or the driftgate in use has no `--skip-commands` yet, the
action warns and seeds in full.

The dump and load commands are yours, so they use the database's own tools,
best from inside the service container, where the tool matches the server:

<!-- check: skip: the same inputs for two databases, one after the other -->
```yaml
# MySQL / MariaDB
pristine-dump: docker exec ${{ job.services.db.id }} mariadb-dump -uroot -pdg --single-transaction --routines --triggers --databases app > "$DRIFTGATE_PRISTINE_DIR/app.sql"
pristine-load: docker exec -i ${{ job.services.db.id }} mariadb -uroot -pdg < "$DRIFTGATE_PRISTINE_DIR/app.sql"

# PostgreSQL
pristine-dump: docker exec ${{ job.services.db.id }} pg_dump -U postgres -Fc app > "$DRIFTGATE_PRISTINE_DIR/app.dump"
pristine-load: docker exec -i ${{ job.services.db.id }} pg_restore -U postgres -d app --clean --if-exists --no-owner < "$DRIFTGATE_PRISTINE_DIR/app.dump"
```

What keeps it safe:

- The key must cover **everything the seed reads**: migrations, seeders,
  factories, dump files, `driftgate.yaml` (its seed commands) and the database
  image (`pristine-cache-key`). The globs are relative to the repository root.
  A file left out means a stale dump.
- A stale dump cannot pass silently as different data: every capture checks
  the restored state against the fingerprint recorded with the baseline and
  stops with exit 3 when they differ. What a stale dump can hide is a change
  to the seed in the pull request itself (data that would no longer match the
  baseline); that is why the key must list the seed's files.
- Caches made on a pull request are visible only to that pull request; caches
  from the default branch are shared.

## Private bodies

Personal data never goes into git, not even encrypted. Bodies stored as
private (marked in the catalog, or found by the personal-data detector) are
encrypted in the local store of the machine that captured them
(`~/.driftgate/<project>/`); the committed baseline names each one only by a
keyed hash (HMAC under a key derived from the project key), which says
nothing about the content without the key. The runner therefore never has the
private bodies of the baseline, and DriftGate does not compare a body by its
hash alone:

- every request whose baseline body is private is **skipped**, with the note
  `private body not on this machine`, and listed in the report;
- the run then exits **3** with the result `incomplete`: the public requests
  were compared and the report shows them, but the check is not complete.
  Compare the private requests where the bodies are (on the machine that made
  the baseline), or capture against an anonymized database, so that responses
  hold no personal data and nothing needs to be private;
- `args: --skip-private` accepts a partial check until then: the private
  requests are reported as skipped and the exit code comes from the rest
  (0 or 1).

<!-- check: yaml -->
```yaml
- uses: orangehill/driftgate@v1.0.2
  with:
    # Private requests are checked locally; CI checks the public ones.
    args: --skip-private
```

The action uses a fresh store per job and, unless `DRIFTGATE_KEY` is set, a
random key for it, which encrypts the private bodies captured in the job; no
secret is needed. A configuration with `key.op` needs 1Password on the runner;
set `key.env` instead.

## Docker image

The `Dockerfile` at the root builds a small image with only the static
driftgate binary (non-root, CA certificates, no shell). Commands from
`driftgate.yaml` (seed, restore, discover adapters) must be in the image that
runs driftgate, so for those copy the binary into the application's image:

```dockerfile
COPY --from=driftgate /usr/local/bin/driftgate /usr/local/bin/driftgate
```

## Self-test

`.github/workflows/action-selftest.yml` (run by hand) starts the demo API
(`tools/demoserver`) and runs the action on `examples/demo`: green by default,
red with exit 1 with `break: true` (the demo's v2 with planted drift). With
`pr` set it comments on that pull request.
