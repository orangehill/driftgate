# Workflows

## Framework upgrade

Make the baseline on the version in production, then upgrade one step at
a time. A small step gives a few drifts with one cause.

1. On the old version, run `driftgate setup`. Keep `mode: freeze` while nothing is meant to change.
2. Run `driftgate coverage` until it is clean. Add `json`, `anonymous` and `identities` templates, and hand-written requests for logins, uploads and jobs ([catalog](catalog.md)).
3. Upgrade one version, or one dependency, per branch.
4. Run `driftgate run`. It compares with the same baseline, on the same restored data.
5. Read the drifts with `driftgate review`. Fix the code, or accept or expect the change ([review](review.md#accept-expect-or-rebase)).
6. When a step spans many commits, `driftgate bisect <good> <bad>` names the first commit ([bisect](review.md#bisect)).
7. Repeat from step 3.

Typical drift after an upgrade:

| Kind | Usual cause |
|---|---|
| `type` | A driver returns native numbers (`string → int`). |
| `number-literal`, `encoding` | The same value written differently (`1.0`, `\/`, pretty-printed JSON). |
| `key-order`, `key-missing`, `key-extra` | The serializer changed. |
| `item-order` | Rows without `ORDER BY` come in another order. |
| `status` | A validation or authorization rule behaves differently. |
| `side-effect` | The request writes other tables than before. |

## Server upgrade

Upgrade the database, PHP or web server in a step of its own, with the
same code. Run the same data on both stacks and compare two captures.

```sh
# old stack, right after driftgate seed: dump the pristine data
mysqldump --single-transaction shop > ~/Data/shop.sql
driftgate capture old
# point base_url and state: at the new stack, and load ~/Data/shop.sql there
driftgate seed --skip-commands
driftgate capture new --skip-fingerprint-check
driftgate compare new --against old --skip-fingerprint-check
```

- Fingerprints differ between engines and versions. So the check that both sides hold the same data is lost.
- Prove the data yourself: diff the schema from `information_schema.COLUMNS`, and sorted per-table dumps from both servers.
- `accept` and `expect` do not apply to two captures. Name each difference in the pull request.
- Keep the new stack as the baseline: capture again with `--repeat 2 --force`, then rebase.

```sh
driftgate capture new --repeat 2 --skip-fingerprint-check --force
driftgate baselines rebase local --from new --reason "Moved to MySQL 8.4" --ref SHOP-12 --allow-freeze
```

Never compare the same code against newer data: every data change shows
as drift.

## Worktrees

Each git worktree checks its change against the main worktree's
baseline. This needs `baselines: local`.

```sh
git worktree add ../shop-step-7 -b step-7
cd ../shop-step-7
# give it its own database and address in .env: DB_DATABASE=shop_step7, APP_URL=http://127.0.0.1:8001
driftgate setup
```

- Setup copies the main worktree's configuration, catalog, baselines and key.
- It makes the pristine copy of the worktree's own database, and makes no new baseline.
- Settings that name the main worktree's database stop setup (exit 2).
- An SQLite file is not in git; setup offers to fill the worktree's file from the main pristine data.
- Carry a ticket's expectations with `driftgate expect --import <file>` ([expect](commands.md#expect)).

## Share a baseline

By default the baseline stays on your machine. To share it with a team or
CI, move it into git.

```sh
driftgate baselines git
git add driftgate.yaml driftgate/
git commit -m "DriftGate baseline"
```

- The key never enters git. Give each developer it in `DRIFTGATE_KEY` or `key:`.
- Private bodies stay in each machine's store. Others compare the rest with `--skip-private`.
- Protect `/driftgate/baselines/` with a CODEOWNERS entry.
- After a merge, rebase the baseline onto a capture of the merged code. Capture it twice, as `run` does for a new baseline; a capture made once is refused.

```sh
driftgate capture main --repeat 2 --force
driftgate baselines rebase local --from main --reason "Merged SHOP-7 and SHOP-9" --allow-freeze
```

To hand over one capture, copy its `captures/<tag>/` folder and the
store's `private/` folder. Send the key through a password manager, never
with the files ([security](security.md#the-project-key)).

## Live server

Check a server that must not change, such as production, before and
after a change.

<!-- check: config-fragment -->
```yaml
level: types
target:
  base_url: https://shop.example
  connect_to: 203.0.113.10        # the new server; allow_hosts names this address
  allow_hosts: [203.0.113.10]
identities:
  admin: { env: DG_TOKEN_ADMIN }  # a token issued beforehand; a login would write
```

```sh
driftgate capture live-before --read-only
# make the change on the server
driftgate capture live-after --read-only
driftgate compare live-after --against live-before
```

| Rule | Why it matters |
|---|---|
| No `state:` | Nothing is restored, seeded or fingerprinted, and no side effect is recorded. |
| No `prepare:` | Without `state:`, `--read-only` refuses it: it may write, and nothing restores what it writes. With `state:` it runs, and should write only to data the restore brings back. |
| No `tokens.command` | It runs as configured and may write. |
| `level: types` | The data is not yours. A pass proves the contract, not the behaviour. |
| `--read-only` | Sends only requests that do not change data: safe methods unless `mutates: true`, and `mutates: false`. |
| `connect_to` | Keeps the host name for `Host` and TLS; `allow_hosts` names the address. |

A page's own requests (a filter, a widget) are checked only when the
catalog has them. Record them on a copy with `driftgate record --write`.

## CI

### GitHub Action

Commit the baseline first ([share a baseline](#share-a-baseline)). The
action runs `driftgate run` and reports in a pull-request comment, the job
summary and an artifact.

<!-- check: yaml -->
```yaml
on: [pull_request]
permissions:
  contents: read
  pull-requests: write
jobs:
  driftgate:
    runs-on: ubuntu-latest
    services:
      db:
        image: mariadb:11.4           # the server the baseline was made on
        env: { MARIADB_ROOT_PASSWORD: dg, MARIADB_DATABASE: shop }
        ports: ["3306:3306"]
    env:
      DG_DB_DSN: mysql://root:dg@127.0.0.1:3306/
    steps:
      - uses: actions/checkout@v4
      # install and start the application on 127.0.0.1
      - uses: orangehill/driftgate@v1.0.2
        with:
          version: v1.0.2
          args: --skip-private
          junit: driftgate-junit.xml
```

| Input | Meaning |
|---|---|
| `version` | `source` (default, builds from the action's ref), a release tag, or `latest`; a download must verify |
| `binary` | a driftgate binary on the runner; `version` is then ignored |
| `working-directory`, `config` | where DriftGate runs, and the path of `driftgate.yaml` |
| `baseline`, `require-baseline` | the committed baseline (default `local`); a missing one stops with exit 2 |
| `args` | extra arguments for `driftgate run`, separated by spaces |
| `store` | `DRIFTGATE_STORE`; empty gives a new store and a random key per job |
| `comment`, `comment-id`, `pr-number` | `always`, `changes` or `off`; tell matrix jobs apart; the pull request outside `pull_request` |
| `junit`, `summary`, `upload`, `artifact-name` | copy the JUnit file, write the job summary, upload the reports |
| `pristine-cache-files`, `pristine-cache-key` | cache the seeded database, keyed on the seed's files and the image |
| `pristine-dump`, `pristine-load` | shell commands that save and load that dump in `$DRIFTGATE_PRISTINE_DIR` |

Outputs: `exit-code`, `result` (`clean`, `drift`, `config-error`,
`environment-error`, `incomplete`), `report`, `junit`, `pristine-cache-hit`.

- Use the same database image and version as the baseline: fingerprints are comparable only there.
- Never give a pull-request job the project key, production secrets or production DSNs. Never use `pull_request_target` ([security](security.md#what-a-pr-job-may-run)).
- Private bodies never reach the runner. Mark bodies without personal data public, or run on [test data](#test-data).

### Other CI

```sh
export DRIFTGATE_STORE="$(mktemp -d)"
export DRIFTGATE_KEY="$(head -c32 /dev/urandom | base64)"   # random per job, never the project key
code=0; driftgate run --skip-private || code=$?
driftgate report --format markdown > driftgate-report.md
driftgate report --format junit > driftgate-junit.xml
exit $code
```

## Test data

The data module copies a MySQL, MariaDB or PostgreSQL database with
personal values replaced. Responses on the copy can be public, so CI
compares them. It is pseudonymisation: run it locally or on your own
runner.

| Step | Command |
|---|---|
| Configure `data:` | source (a copy of production, only read) and target ([configuration](configuration.md#uploads-bisect-and-data)) |
| Classify every column | `driftgate data classify --write`, then check each rule and remove `proposed: true` |
| Copy everything | `driftgate data anonymize --dump out/shop-anon.sql` |
| Or copy a sample | `driftgate data subset --no-target --dump out/shop-sample.sql` |
| Check an output again | `driftgate data verify --dump out/shop-sample.sql` |

<!-- check: classify -->
```yaml
version: 1
tables:
  users:
    columns:
      id: keep
      email: email
      name: universal
      password: password
      remember_token: token
      about: free-text
  sessions:
    action: empty
    reason: Sessions of real users.
subset:
  tables:
    countries: all
    users: { rows: 200 }
    orders: { follow: [users], per_parent: 5 }
```

- A column without a confirmed rule stops `anonymize` and `subset` (exit 2).
- The output is written only when the detector and a search for every original value find nothing.
- Seed the state database from the dump, then set `storage.default: public` and make the baseline.
- In CI, run without `--skip-private`: every body is public.
- The detector still scans public bodies. Allow stand-in patterns with `pii.allow`.

Every command and flag is in [commands](commands.md#data-classify).
