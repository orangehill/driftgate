# exclusions.yaml

`exclusions.yaml` lists the routes that are not checked on purpose, and
acknowledges requests that fail in the baseline. Every entry has a
reason.

The file is `.driftgate/exclusions.yaml`, or `driftgate/exclusions.yaml`
with `baselines: git` ([where the project's files are](cli.md#where-the-projects-files-are)).

## Exclude a route

<!-- check: exclusions -->
```yaml
version: 1
routes:
  - route: '/telescope/*'
    reason: 'Laravel Telescope: a development tool.'
  - route: 'DELETE /api/account'
    reason: Deletes the logged-in user; checked by hand before each release.
  - target: web
    route: '/api/draft/*'
    reason: Preview mode of the CMS.
```

| Key | Meaning |
|---|---|
| `route` | a `METHOD /path` glob; without a method it matches every method. Required |
| `reason` | required |
| `target` | only routes of this service; empty: every service |

- [`coverage`](commands/catalog.md#driftgate-coverage) counts an excluded route as excluded.
- `generate` makes no requests for it.
- An exclusion that matches no route is reported as stale.

## Acknowledge a request that fails

<!-- check: exclusions -->
```yaml
version: 1
routes: []
known_failures:
  - request: post-api-tree-reorder
    reason: Answers 500 since the move to nested sets; fixed in the Laravel 9 step.
    ref: SHOP-412
    status: 500
  - request: 'post-api-uploads-*'
    reason: Uploads break until the storage driver is replaced.
    status: 5xx
```

`known_failures:` acknowledges requests that fail in the baseline without
dropping them, such as a 500 kept for years, or an upload broken until a
later upgrade step.

| Key | Meaning |
|---|---|
| `request` | a request ID or glob. Required |
| `reason` | required |
| `ref` | a ticket or link |
| `status` | the failure acknowledged (below); without it, any failure |

`status` is one of:

- a status, such as `500`, also among the sends of a repeated request;
- a class: `5xx` or `4xx`;
- `error`: no answer;
- `timeout`: no answer in time.

What a known failure does:

- `generate` keeps the request, and `capture` and `compare` treat it as any other request.
- `coverage` and `run` list it, instead of failing on its route ([coverage](commands/catalog.md#what-coverage-lists)).
- An exclusion would drop the request, and its answer would no longer be compared at all.

How a request is matched:

- A request takes the first known failure whose glob matches it.
- When that one names a status its failure does not have (the 500 became a 404, the timeout a refused connection), the request is reported as failed, with a note saying its known failure changed.
- A known failure whose requests succeed now, or that matches no request, is reported as stale. Remove it once the fix is accepted.
- A known failure that acknowledges failures on more than 5 routes is reported as broad: such a glob also hides the new failures it reaches.

A release before `known_failures:` refuses the file
([keys an earlier release refuses](upgrading-driftgate.md#keys-an-earlier-release-refuses)).

---
**Related:** [driftgate coverage](commands/catalog.md#driftgate-coverage) · [catalog.json](catalog-json.md) · [What the result does not cover](reports.md#what-the-result-does-not-cover)
