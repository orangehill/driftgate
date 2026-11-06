# Upgrading a framework or language version

A typical use: moving an API across several framework and language
versions (for example Laravel 5.5 / PHP 7.3 → Laravel 13 / PHP 8.3)
without changing what clients get.

## 1. Baseline on the old version

On the version in production, with data that stands for production (a
dump, or an [anonymized copy](test-data.md)):

```sh
driftgate setup        # configuration, key, pristine copy, routes, catalog, the baseline
```

(In steps: `driftgate init` writes `.driftgate/driftgate.yaml` with the
database it found commented out; check it, then `driftgate setup` turns
`state:` on after asking and goes on, or uncomment it yourself and run
`driftgate seed --skip-commands` and `driftgate run`.)

Before the first step, make the catalog cover what clients use:

- `driftgate coverage` must be clean: every route method has a request
  that should succeed, or an exclusion with a reason;
- add the variants clients depend on: `json` (`Accept: application/json`;
  the same error can look completely different without it), `anonymous`,
  one per identity;
- add hand-written entries for flows `generate` cannot make: login
  sequences, uploads with real files, queued jobs as command steps,
  throttling with `repeat` (see [catalog.json](../configuration.md#catalogjson)).

Keep `mode: freeze` while nothing is meant to change. With `baselines:
local` (the default) the baseline stays in `.driftgate/` on this machine,
which is all the step-by-step checks before each pull request need. When
a team or CI should compare against it too, `driftgate baselines git`
moves it into the repository (`driftgate.yaml` and `driftgate/`); commit
it then, and protect `driftgate/baselines/` with a CODEOWNERS entry
([baselines](../commands.md#baselines)).

## 2. One step at a time

Upgrade one version (or one dependency) per branch, then:

```sh
driftgate run
```

against the same baseline, on the same restored data. Typical drift of an
upgrade:

- `type`: a value's JSON type changed, e.g. `$.data[*].id: type string → int`
  after a database driver started returning native numbers;
- `number-literal`, `encoding`: the same value written differently (`1.0`
  for `1`, `\/` for `/`, pretty-printed JSON);
- `key-order`, `key-missing`, `key-extra`: serialization changes;
- `item-order`: the same rows in another order, typical when the database
  or the ORM changes (a list without `ORDER BY`, or one on a column with
  equal values). It is one drift per array, with a note that names the
  [array rule](../configuration.md#allowlistyaml) to add: `key` pairs the
  items by an id and still reports a changed item, `any_order` allows the
  order. Add it only where the order does not matter to clients;
- `header-*`: cookies, cache headers, content types;
- `status`: a validation or authorization rule behaves differently;
- `side-effect`: a request writes to other tables, rows or columns than
  before, even when its response is the same.

`driftgate review` groups the drifts and explains each group with the
route, the controller action and its file and line (Laravel, Rails,
Symfony, Django, Next.js and Nuxt).

## 3. Fix or accept

The goal is usually that clients see no change, so most drift is fixed in
the code (cast the value, restore the old serializer setting). When a
change is intended, switch to `mode: evolve` and accept it with a reason:

```sh
driftgate accept group:3 --reason "Dates are ISO 8601 since Laravel 7; clients updated in APP-512" --ref APP-512
```

Every acceptance is logged in `acceptances.jsonl` with who, when, why and
the drift signatures, so the baseline's history explains every difference
between the old and the new version.

In mode freeze a change a ticket intends is expected instead: the baseline
stays, and the gate passes that exact drift and nothing else.

```sh
driftgate expect group:3 --ref APP-512 --reason "Dates are ISO 8601 since Laravel 7"
```

When the ticket is merged and the baseline is rebased on the merged code
(`driftgate baselines rebase local --from <capture> --reason "…"
--allow-freeze`), the expectations it now holds are closed; `driftgate
expect --drop APP-512 --reason "…"` removes one that is no longer wanted.

## 4. When the cause is not obvious

If a step spans many commits, [`driftgate bisect`](bisect.md) names the
first commit of each drift group separately.

## 5. Keep it in CI

Locally, `driftgate run` before each pull request is the check. To have CI
run it too, the baseline must be in the repository: `driftgate baselines
git`, commit, and give the job the key in `DRIFTGATE_KEY` (the key file
never enters git). Then run the [GitHub Action](github-actions.md) on
every pull request of the upgrade, so a later step cannot bring back a
difference an earlier step fixed. Private bodies stay on the machine that
captured them; the job compares the rest (`--skip-private`).

## Upgrading the server: database, PHP, web server

Upgrade the server in a step of its own, never together with the code:
then every drift comes from the server. Typical drift: `item-order` (rows
of a list without `ORDER BY` come in another order), `type` and
`number-literal` (a driver returns numbers and decimals otherwise),
`header-value` on every request (`server`, `x-powered-by`), and error
pages and limits of the web server or PHP (`client_max_body_size`,
`upload_max_filesize`).

**Comparing two stacks (A/B).** Run the same code with the same data on
the old stack and the new one, each at its own address, and compare the
captures:

```sh
# on the old stack, right after a restore (driftgate seed): the pristine data
mysqldump --single-transaction shop > /tmp/shop.sql
driftgate capture old
# point target.base_url and state: at the new stack, load /tmp/shop.sql there
driftgate seed --skip-commands              # the pristine copy on the new server, without seed: commands
driftgate capture new --skip-fingerprint-check
driftgate compare new --against old --skip-fingerprint-check
```

- Take the dump from the pristine data, right after a restore or seed,
  not after a capture or `prepare` changed it. Seed the new server with
  `--skip-commands`: seed commands such as `migrate:fresh` would replace
  the data just loaded.
- The responses are compared as always; a page that names its own
  address is compared with the other side's address taken as its own.
- The state fingerprint is specific to the database engine and version
  (MariaDB 10.3 and MySQL 8.4 write the same rows differently). Each side
  still checks its own data against its own pristine fingerprint after
  every restore; what is lost is the check that both sides hold the same
  data. A capture made with `--skip-fingerprint-check` is marked
  unreliable, and compare takes it only with the same flag. A drift may
  then come from the data, and a green result does not show that the
  data were the same.
- Until DriftGate can compare data across engines, prove it otherwise,
  and say how in the pull request:
  - the schema: `SELECT table_name, column_name, ordinal_position,
    column_type, character_set_name, collation_name, is_nullable,
    column_default FROM information_schema.COLUMNS WHERE table_schema =
    'shop' ORDER BY table_name, ordinal_position` on both servers, then
    `diff`. A difference from loading the dump into the other engine (a
    collation such as `uca1400` or `0900`, a JSON column written
    normalised, the sandbox line a newer `mariadb-dump` writes) is part of
    the change: name it in the pull request;
  - the rows: `SELECT COUNT(*)` of every table (not `TABLE_ROWS`, an
    estimate), then a dump of the data of each table on both servers with
    the same client binary, one row per line, sorted the same way:
    `mysqldump --no-create-info --skip-extended-insert --skip-comments
    --skip-dump-date --skip-triggers --hex-blob --set-gtid-purged=OFF
    shop <table> | LC_ALL=C sort` (MySQL 8's client against MariaDB also
    `--column-statistics=0`), then `diff`. Sorting the lines, not the
    rows by `ORDER BY`, keeps the collations of the two servers out of
    it. With `SELECT` instead, set `time_zone = '+00:00'` on both
    sessions first; `FLOAT` and `DOUBLE` columns may print differently
    with the same stored value.
- A difference found here is reviewed in the comparison and named in the
  pull request: a comparison of two captures is not against a baseline,
  so `accept` and `expect` do not apply to it (and `accept` refuses
  captures made with `--skip-fingerprint-check`).
- The same header on every request (`server`) is one change: the line
  after the comparison says how many requests drift in something else
  and gives the header rule that allows the new value, to add when the
  new stack is meant to answer so.
- Settings of the stack (PHP's `memory_limit` or `upload_max_filesize`,
  database variables, the web server's `client_max_body_size`) change
  behaviour without a response showing it until a limit is hit: a
  [command step that prints the chosen settings](side-checks.md#settings-of-the-stack)
  makes a difference a drift you see in the comparison.
- When the new stack is the one to keep, record the switch in the
  baseline: capture it again with `--repeat 2 --skip-fingerprint-check
  --force` (the tag exists)
  (its runs must agree; the capture records the new server's fingerprint)
  and
  `driftgate baselines rebase local --from new --reason "Moved to MySQL
  8.4 and PHP 8.3" --ref <ticket> --allow-freeze`, which logs it as one
  line of the acceptance log ([baselines rebase](../commands.md#baselines-rebase)).
  Alternatively make a new baseline on the new stack with `driftgate run
  --baseline <name>`; every later command then needs `--baseline <name>`.
  Then go on with the code steps.

## Notes

- The same catalog and baseline hold before and after the upgrade:
  DriftGate works over HTTP and does not depend on the application's
  language or framework.
- Fingerprints are comparable only on the same database server version:
  see [upgrading the server](#upgrading-the-server-database-php-web-server).
- Values that depend on timing (rate limits within a minute, caches) are
  either allowed by format, measured with `observe`, or tested with
  `repeat` on a restored state.
