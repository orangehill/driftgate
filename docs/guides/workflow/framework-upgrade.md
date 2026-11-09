# Upgrade a framework step by step

> **Before you start:** [set up your project](../../start/your-project.md) on the old version. **Time:** 30 minutes, then a few minutes per step.

You move an application across framework and language versions without
changing what clients get, such as from Laravel 5.5 to 13. The same catalog and baseline hold before and after: DriftGate
works over HTTP, whatever the language or framework.

## Make the baseline on the old version

```sh
driftgate setup        # configuration, key, pristine copy, routes, catalog, the baseline
```

Run it on the version in production, with data that stands for
production: a dump, or an [anonymized copy](../ci/test-data.md). Keep
`mode: freeze` while nothing is meant to change. To go step by step
instead, see [set up step by step with init](../../start/your-project.md#set-up-step-by-step-with-init).

## Make the catalog cover what clients use

```sh
driftgate coverage     # every route method has a request that should succeed, or an exclusion
```

Before the first step:

- `driftgate coverage` must be clean ([coverage](../../reference/commands/catalog.md#driftgate-coverage)).
- Add the variants clients depend on: `json`, `anonymous`, one per identity ([templates](../../reference/driftgate-yaml.md#templates)). The same error can look completely different without `Accept: application/json`.
- Write by hand what `generate` cannot make: login sequences, uploads with real files, queued jobs as command steps, rate limits with `repeat` ([catalog.json](../../reference/catalog-json.md)).

## Upgrade one step and run

```sh
driftgate run          # against the same baseline, on the same restored data
```

Upgrade one version, or one dependency, per branch. Then run the check.
A small step gives a few drifts with one cause.

## Typical drift after an upgrade

- `type`: a driver returns native numbers now (`$.data[*].id: type string → int`).
- `number-literal`, `encoding`: the same value written differently (`1.0` for `1`, `\/` for `/`, pretty-printed JSON).
- `key-order`, `key-missing`, `key-extra`: the serializer changed.
- `item-order`: the same rows in another order, after a database or ORM change. The note names the array rule to add, where the order does not matter to clients.
- `header-…`: cookies, cache headers, content types.
- `status`: a validation or authorization rule behaves differently.
- `side-effect`: a request writes other tables than in the baseline, even with the same response. Rows and columns only with [`row_tracking`](../../reference/state.md#database-components).

Each kind is explained in [Drift kinds](../../reference/drift-kinds.md).
`driftgate review` groups the drifts, and names the route, the
controller action and its file and line (Laravel, Rails, Symfony, Django,
Next.js and Nuxt).

## Fix, accept or expect

```sh
driftgate review
# mode: evolve, the change is right: accept it into the baseline
driftgate accept group:3 --reason "Dates are ISO 8601 since Laravel 7; clients updated in APP-512" --ref APP-512
# or, mode: freeze, a ticket intends it: expect it, the baseline stays
driftgate expect group:3 --ref APP-512 --reason "Dates are ISO 8601 since Laravel 7"
```

- Usually clients should see no change, so most drift is fixed in the code: cast the value, restore the old serializer setting.
- An intended change is accepted, in `mode: evolve`. The acceptance log then explains every difference between the old and the new version.
- In `mode: freeze`, a ticket expects its change instead: the gate passes that exact drift and nothing else.

How to choose is in [Fix, accept or expect a drift](../review/fix-accept-or-expect.md).

## When the cause is not obvious

```sh
driftgate bisect v2.3.0 HEAD
```

When a step spans many commits, `bisect` names the first commit of each
drift group ([find the commit](../review/find-the-commit.md)).

## Keep the gate in CI

Locally, `driftgate run` before each pull request is the check. To let CI
run it too:

1. Move the baseline into git, and commit it ([share a baseline](share-baselines.md)).
2. Run the [GitHub Action](../ci/github-actions.md) on every pull request of the upgrade, with `--skip-private`.

The job needs no key, and never gets the project key
([keep secrets out of pull-request jobs](../../security.md#keep-secrets-out-of-pull-request-jobs)).
With `--skip-private` it compares the status, headers, public bodies and
side effects of every request. A later step then cannot bring back a
difference there that an earlier step fixed.

Private bodies, the default for generated requests, are compared only
by `driftgate run` on a developer machine
([private bodies](../../security.md#private-bodies)). So keep running it
locally before each pull request.

## Upgrade the server too

Upgrade the server, such as the database, PHP or the web server, in a
step of its own ([upgrade the server](server-upgrade.md)). Values that
depend on timing, such as rate limits or caches, are
[allowed by format](../review/changing-values.md), measured with
[`observe`](../../reference/driftgate-yaml.md#observe), or tested with
[`repeat`](../../reference/catalog-json.md#repeat).

---
**Next:** [Work on each change in its own worktree](worktrees.md): one worktree per upgrade step.
**Related:** [Drift kinds](../../reference/drift-kinds.md) · [driftgate run](../../reference/commands/check.md#driftgate-run) · [How DriftGate works](../../concepts/how-it-works.md)
