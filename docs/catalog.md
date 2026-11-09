# The catalog

`catalog.json`, next to `driftgate.yaml`, lists the requests every capture sends, in order.
`generate` writes most entries; you add the flows it cannot make.

<!-- check: catalog -->
```json
{"version": 1,
 "requests": [
  {"id": "post-api-orders", "method": "POST", "path": "/api/orders", "identity": "admin",
   "body": {"sku": "A1"}, "mutates": true, "storage": "private"},
  {"id": "get-api-orders-id", "method": "GET", "path": "/api/orders/{id}", "identity": "admin",
   "params_from": {"id": {"request": "post-api-orders", "path": "$.data.id"}}, "storage": "private"}],
 "sequences": [{"id": "order", "restore": "before", "requests": ["post-api-orders", "get-api-orders-id"]}]}
```

## Request fields

| Key | Meaning |
|---|---|
| `version` | `1` |
| `requests` | the requests, in order |
| `sequences` | requests run in order, together ([below](#sequences)) |
| `id` | unique name: letters, digits, `. _ -`; unique also ignoring case |
| `target` | the service, with several services |
| `method` | HTTP method; default `GET` |
| `path` | starts with `/`; may hold `{name}` parameters and a query string |
| `params` | values of the path parameters |
| `params_from` | a parameter from an earlier response of the same sequence: `{"request": "<id>", "path": "$.data.id"}` |
| `request` | of `params_from`: the earlier request's ID |
| `headers` | `[[name, value], …]`; the first of a name replaces DriftGate's own |
| `body` | a JSON body |
| `identity` | send with this identity's bearer token ([below](#identities-and-tokens)) |
| `mutates` | `true` when the request changes data; default: every method but `GET`, `HEAD`, `OPTIONS` |
| `storage` | `public` or `private`, required ([private bodies](security.md#private-bodies)) |
| `level` | `status`, `shape`, `types` or `values` for this request only |
| `observe` | `true`: measured (status, time, size), never compared |
| `source` | written by `generate`: `route`, `template` or `import` |
| `route`, `template`, `import` | of `source`; an entry without `template` is yours |
| `multipart` | a `multipart/form-data` body ([below](#multipart)); not with `body` |
| `repeat` | send N times, 1 to 10000 ([below](#hand-written-requests)) |
| `command`, `dir`, `stdin`, `output`, `output_ignore` | a command step ([below](#command-steps)) |

Requests that change data run after the read-only ones, one at a time. They need a `state:` component with a restore ([configuration](configuration.md#state)).

## Generate and templates

`driftgate generate --write` makes requests for the routes `discover` found.
`templates:` in `driftgate.yaml` says which variants it makes per route method.

| Template | Request |
|---|---|
| `ok` | the route with its parameter values (default) |
| `not-found` | required path parameters set to `999999999` |
| `invalid-param` | required path parameters set to `driftgate-invalid` |
| `anonymous` | a route that needs a login, sent without one |
| `identities` | a route that needs a login, once per other identity |
| `json` | with `Accept: application/json` |
| `empty-body` | a route that takes a body, sent with `{}`; without it a changed validation message passes |
| `json-errors` | each error variant once more with `Accept: application/json` |

- A parameter without a value gets no request; set it under `params:` ([configuration](configuration.md#discovery-and-generation)).
- `generate` replaces its own entries on every run. An entry you write replaces the generated ones of its route method.
- Do not edit a generated entry: set `mutates` or `body` under `requests:` in `driftgate.yaml`.

## Record

`driftgate record --write` starts a proxy on `http://127.0.0.1:8088`. It proposes a request for every distinct request a browser, frontend or E2E suite sends through it.
Point the client's base URL at the proxy. Stop it with Ctrl-C. `har import <file.har>` reads a browser's HAR file the same way.

- `/api/users/1` and `/api/users/2` are one entry: requests differ by method, route, query names, identity and body shape.
- A login or a request with personal data is left out and reported, to add by hand.

## Hand-written requests

Write by hand what `generate` cannot make: a login, a real upload, a queued job, a rate limit.
A new request is skipped until you `accept` its first answer ([review](review.md#accept-expect-or-rebase)).

| Need | Write |
|---|---|
| a login check | `"body": {"email": "admin@shop.example", "password": "secret"}` |
| a rate limit | `"repeat": 61`: compares the last response and all statuses, such as `200×60, 429×1` |
| a search sent as `POST` | `"mutates": false` |

## Sequences

A sequence has an `id`, its `requests` in order, and optionally `"restore": "before"`.
A request is in at most one sequence; `capture --restore each` restores before every one. DriftGate keeps no cookies: later requests sign in with their `identity`, not a login's session.

## Multipart

<!-- check: catalog -->
```json
{"version": 1, "requests": [
  {"id": "import-csv", "method": "POST", "path": "/api/imports", "mutates": true, "storage": "public",
   "multipart": {"fields": [["kind", "orders"]], "files": [{"field": "file", "synthetic": "csv"}]}}]}
```

| Key | Meaning |
|---|---|
| `fields` | the form fields, `[[name, value], …]` |
| `files`, `field` | the files, each with its form `field` and `path` or `synthetic` |
| `path` | a real file, relative to the uploads folder ([configuration](configuration.md#uploads-bisect-and-data)) |
| `ref` | keyed hash of a real file; the first capture prints it, then you set it |
| `synthetic` | a small valid file: `csv`, `gif`, `jpeg`, `json`, `pdf`, `png`, `txt` or `xlsx` |
| `name`, `type` | the file name and content type sent |

A request with a real file must be `private`; the file never enters the repository.

## Command steps

<!-- check: catalog -->
```json
{"version": 1, "requests": [
  {"id": "queue-work", "command": ["php", "artisan", "queue:work", "--stop-when-empty"],
   "output": true, "output_ignore": ["\\d{4}-\\d{2}-\\d{2} \\d{2}:\\d{2}:\\d{2}"], "storage": "public"}]}
```

A command step runs a program (argv, no shell) instead of a request, such as a queue worker in a sequence.
Its exit code and side effects are compared; its output only with `"output": true`, minus `output_ignore` matches. `dir` and `stdin` are relative to the project folder.

## Identities and tokens

A request with `"identity": "admin"` is sent with `Authorization: Bearer <token>`.
The token comes from `identities:` or `tokens:` in `driftgate.yaml` ([configuration](configuration.md#identities-and-tokens)).

- A request whose identity has no token stops the capture.
- A tokens command runs after every restore. Make it print fixed tokens, or each run differs ([Laravel](frameworks.md#laravel)).

## Coverage and exclusions

`driftgate coverage` lists routes without a request that succeeds in the baseline (exit 1 when any).
Error variants do not cover a route. `exclusions.yaml`, next to the catalog, excludes routes and acknowledges failures.

<!-- check: exclusions -->
```yaml
version: 1
routes:
  - route: '/telescope/*'
    reason: A development tool.
known_failures:
  - request: post-api-tree-reorder
    status: 500
    ref: SHOP-412
    reason: Answers 500 in the baseline; fixed in a later upgrade step.
```

| Key | Meaning |
|---|---|
| `routes` | routes not checked: no generated requests, counted as excluded |
| `route` | a `METHOD /path` glob; without a method, every method |
| `known_failures` | requests that fail in the baseline: still compared, listed instead of failing coverage |
| `request` | a request ID or glob |
| `status` | `500`, `5xx`, `4xx`, `error` or `timeout`; without it, any failure |
| `ref`, `target`, `reason` | a ticket; only this service; why (required) |

An entry that matches nothing, or whose request succeeds now, is reported as stale.
