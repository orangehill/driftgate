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

## Notes

- The same catalog and baseline hold before and after the upgrade:
  DriftGate works over HTTP and does not depend on the application's
  language or framework.
- Fingerprints are comparable only on the same database server version.
  Upgrade the database server in a separate step: restore the data on the
  new server, make a new baseline from the old application version there,
  then continue.
- Values that depend on timing (rate limits within a minute, caches) are
  either allowed by format, measured with `observe`, or tested with
  `repeat` on a restored state.
