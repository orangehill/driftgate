# Make the catalog cover every route

> **Before you start:** have a project with a route inventory ([set up your project](../../start/your-project.md)). **Time:** 20 minutes.

A route without a request in the catalog is never checked. `driftgate
coverage` lists those routes, so you can add a request or exclude the
route on purpose.

## See which routes are not covered

```sh
driftgate coverage
```

```console
$ driftgate coverage
3 routes: 3 covered, 0 excluded, 0 uncovered

every route is covered or excluded ✓
```

Each method of a route counts. It is covered only by a request meant to
succeed that did not fail in the baseline. Error variants such as
`not-found` do not cover it. The exit code is 1 while a route is left
([driftgate coverage](../../reference/commands/catalog.md#driftgate-coverage)).

## Give route parameters values

<!-- check: config-fragment -->
```yaml
params:
  - name: id
    value: "12"
  - name: product
    sql: "SELECT id FROM products WHERE active = 1 ORDER BY id LIMIT 3"
    foreach: true
```

`generate` cannot make a request for a route whose parameters have no
value: it names each missing parameter, with an example. Give it a fixed
value, a list, a query on the pristine data, or a value from an earlier
response. Then run `driftgate generate --write`
([parameter values](../../reference/driftgate-yaml.md#parameter-values)).

## Add the variants clients depend on

<!-- check: config-fragment -->
```yaml
templates: [ok, not-found, json, json-errors, anonymous]
```

Each template makes one more request per route method. `json` sends
`Accept: application/json`; the same error can look different without it.
`anonymous` sends a route that needs a login without one. Add the variants
your clients use, then run `driftgate generate --write`
([templates](../../reference/driftgate-yaml.md#templates)).

## Exclude a route on purpose

<!-- check: exclusions -->
```yaml
version: 1
routes:
  - route: '/telescope/*'
    reason: 'Laravel Telescope: a development tool.'
```

An exclusion in `.driftgate/exclusions.yaml` says why a route is not
checked. `generate` makes no requests for it, and `coverage` counts it as
excluded. An exclusion that matches no route is reported as stale
([exclude a route](../../reference/exclusions-yaml.md#exclude-a-route)).

## Keep a request that fails in the baseline

<!-- check: exclusions -->
```yaml
version: 1
routes: []
known_failures:
  - request: post-api-tree-reorder
    reason: Answers 500 since the move to nested sets; fixed in the Laravel 9 step.
    ref: SHOP-412
    status: 500
```

A request that already fails in the baseline leaves its route uncovered.
A known failure keeps the request in the catalog, still compared, and
lists it instead of failing on its route. Remove it once the fix is
accepted ([acknowledge a request that fails](../../reference/exclusions-yaml.md#acknowledge-a-request-that-fails)).

## Keep the catalog current in CI

```sh
driftgate generate --check     # exit 1 when the catalog is not what generate would write
driftgate coverage             # exit 1 when a route is not covered
```

Run both in CI next to the gate. A new route without a request then fails
the pull request that adds it
([driftgate generate](../../reference/commands/catalog.md#driftgate-generate)).

---
**Next:** [What a green result proves](../../concepts/what-a-pass-proves.md): what the gate still does not see.
**Related:** [driftgate coverage](../../reference/commands/catalog.md#driftgate-coverage) · [exclusions.yaml](../../reference/exclusions-yaml.md) · [templates](../../reference/driftgate-yaml.md#templates)
