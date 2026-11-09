# Commands: routes and the catalog

The commands that find the routes, write the catalog and show what it
does not cover. The files they write are described under
[catalog.json](../catalog-json.md) and
[exclusions.yaml](../exclusions-yaml.md).
Paths are those of the local layout ([where the project's files are](../cli.md#where-the-projects-files-are)).

## driftgate discover

```
driftgate discover [--write] [--target <service>]
```

Finds the routes of the application and compares them with the route
inventory (exit 1 when they changed).

| Flag | Meaning |
|---|---|
| `--write` | write the routes found as the new inventory (`.driftgate/routes.json`) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

Routes come from the adapters under
[`discover:`](../driftgate-yaml.md#discover) in `driftgate.yaml`.

```console
$ driftgate discover
3 routes (3 from command)
routes unchanged ✓
```

## driftgate generate

```
driftgate generate [--write] [--check] [--all] [--target <service>]
```

Generates catalog requests for the routes that hand-written and imported
requests do not cover.

| Flag | Meaning |
|---|---|
| `--write` | write the generated catalog (not with `--check`) |
| `--check` | exit 1 when the catalog is not what generate would write (not with `--write`) |
| `--all` | generate requests for every route, also the covered ones |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- It makes one request per route method and template (`templates:`; default `ok`).
- Entries written by hand or imported stay untouched. A route method that a hand-written entry already reaches is skipped, unless `--all`.
- `generate` and `run` name each route method skipped so in a `note:`, with the entries that reach it and the requests `generate` would make there, such as its error variants and JSON twins.
- Parameter values come from `params:`. For routes it cannot make, it names every missing parameter, grouped by reason, with an example.
- The same input always gives a byte-identical catalog.
- The summary says how many generated requests `json-errors` made.
- A project whose `discover:` uses the `laravel`, `symfony`, `rails` or `spring` adapter, and whose `templates:` lack `json-errors`, gets a one-line `note:` to add it ([templates](../driftgate-yaml.md#templates-generate-params)). The note comes once per local store, from `generate` or `run`; `generate --check` without a store says nothing.

```sh
driftgate generate --check     # CI: is the committed catalog current?
```

## driftgate coverage

```
driftgate coverage [--format text|json] [--baseline <name>] [--target <service>]
```

Lists the routes of the inventory without a catalog request that
succeeds (exit 1 when there are any).

| Flag | Meaning |
|---|---|
| `--format text\|json` | output format (default text) |
| `--baseline <name>` | the baseline whose answers decide which requests succeed (default `local`) |
| `--target <service>` | work on this service only (repeatable or comma-separated) |

- `--baseline`: `local` is checked when it exists; a baseline named must exist (exit 2); `none` uses the catalog alone.
- Each method of a route counts separately; `PUT` and `PATCH` of one route count as one.
- A method is covered only by a request that should succeed. Error variants (`not-found`, `invalid-param` …) do not cover it.
- Exclude a route on purpose in `.driftgate/exclusions.yaml`, with a reason.
- `coverage` reads the answers from the baseline's folder; it needs neither the local store nor the key.

```console
$ driftgate coverage
3 routes: 3 covered, 0 excluded, 0 uncovered

every route is covered or excluded ✓
```

### When a request counts as failed

With a baseline, a request covers its route only when it did not fail
there, or when the baseline has no answer for it yet. A comparison checks
that answers stay equal, so an error already in the baseline (an endpoint
that answers 500) would otherwise pass on every run.

A request fails when:

- it got no answer: a timeout, a connection error, or repeated sends cut short;
- it answered a server error (5xx), or one of its repeated sends (`repeat`) did;
- it is meant to succeed and answered a client error (4xx), or a redirect to a sign-in page. That is a location whose last path segment is `login`, `signin`, `sign-in`, `sign_in`, `log-in` or `logon`.

Meant to succeed are the requests the `ok` and `json` templates
generate. The exception is a route that takes a body without a sample:
it is listed as a route without a sample body.

- Error variants and command steps never fail here.
- A request written by hand or imported may mean a client error (a wrong password, a rate limit). Its 4xx covers the route, which is then listed as reached only by requests that answered an error, never as a failure.
- A validation error that Laravel answers with a redirect back (to `/` for a request without a `Referer`) cannot be told from a redirect the route means. The `json` template sends the same request as an API client, which gets the 422.

### What coverage lists

- Routes whose requests meant to succeed all failed in the baseline, with those requests and their statuses: not covered (exit 1). Fix the route, then `accept` the new answer in mode `evolve`. In mode `freeze`, make the baseline again or rebase it on a capture of the fix (`baselines rebase --allow-freeze`). Or acknowledge it as a known failure.
- Requests that failed in the baseline, also on routes another request covers (a warning).
- Known failures (`known_failures:` in [exclusions.yaml](../exclusions-yaml.md)): the request stays in the catalog and is compared as usual. Its route is neither covered nor failed (`… with known failures` in the summary).
- Known failures that are stale, no longer match their `status`, or are broad, as [exclusions.yaml](../exclusions-yaml.md) describes. A stale one also matches no request of any service.
- Requests that reach no route, stale exclusions, and routes without a sample body.
- Covered routes whose body is validated (they have a sample body, from validation rules or OpenAPI) but whose validation error no request shows. Their validation errors are not compared, so a changed validation message passes. The list never changes the exit code.
  - A request counts when the baseline recorded it as refused (400 or 422): one of the `empty-body` template, its `json-errors` twin, or one written by hand or imported.
  - Without a baseline answer, an `empty-body` request counts. For Laravel, Symfony, Rails and Spring Boot only its JSON twin does: they answer a request without `Accept: application/json` with a redirect back.
  - Add `empty-body` to `templates:`, and `json-errors` for those four frameworks. Then run `driftgate generate --write`.

An exclusion that `init` wrote for the framework (Debugbar, Telescope,
Horizon …) is not reported stale while it keeps `init`'s route and
reason: the project need not have that part.

### Coverage as JSON

`--format json` gives:

- `baseline_checked`;
- `failed_in_baseline`: `route`, and `requests` with `id`, `status` and `detail`;
- `requests_failed_in_baseline`, with `known_status` when a known failure no longer matches;
- `known_failures`, with `reason` and `ref`;
- `known_failure_routes`, `stale_known_failures`, `broad_known_failures` and `only_error_answers`;
- `routes_without_invalid_body`: the covered routes whose validation errors no request checks;
- `stale_exclusions`, without the exclusions `init` wrote for the framework.

## driftgate har import

```
driftgate har import <file.har> [--target <service>] [--write]
```

Adds the requests of a HAR file, recorded in a browser, to the catalog.

| Flag | Meaning |
|---|---|
| `--target <service>` | the service the requests belong to (once; needed with several services) |
| `--write` | write the catalog; without it the requests are only listed |

What becomes an entry is under
[What becomes a catalog entry](#what-becomes-a-catalog-entry).

```sh
driftgate har import ~/Downloads/admin-session.har            # list
driftgate har import ~/Downloads/admin-session.har --write    # add to the catalog
```

## driftgate record

```
driftgate record [--listen <addr>] [--target <service>] [--write]
    [--include <glob>] [--exclude <glob>] [--allow-non-loopback]
```

A recording proxy on 127.0.0.1. Use the application through it (a
browser, a frontend, an E2E suite), and it proposes catalog requests for
what it saw. Responses are never kept.

| Flag | Meaning |
|---|---|
| `--listen <addr>` | the address to listen on (default `127.0.0.1:8088`; loopback only) |
| `--target <service>` | the service to forward to (once; needed with several services) |
| `--write` | on stop, add the new requests to the catalog; without it they are only listed |
| `--include <glob>` | record only paths that match (repeatable or comma-separated) |
| `--exclude <glob>` | do not record paths that match, besides static files and dev-server noise (repeatable or comma-separated) |
| `--allow-non-loopback` | allow a `--listen` address other than loopback (anyone who reaches it acts as you) |

- Every request goes to the service's `target.base_url` (http or https), which must be in `allow_hosts`.
- The proxy itself is plain HTTP: point the browser, the frontend's API URL or the E2E suite's base URL at `http://127.0.0.1:8088`.
- Static files, event streams and websockets pass through unrecorded. So does the noise of development servers: `/_next/static`, `/_next/image`, `/_nuxt`, `/build`, Vite's own paths, `node_modules`, the favicon, `robots.txt` and the web manifest.
- It answers only for its own address (`Host` `127.0.0.1`, `[::1]` or `localhost` with its port) and refuses requests from other sites ([security](../../security.md)).
- It runs until Ctrl-C or SIGTERM. Then it lists the new entries, with the routes they reach and which of them were uncovered before.
- Recording only proposes requests: `capture` records their responses on a restored state.
- Exit `0`; `2` for a non-loopback `--listen` without the flag, or a service outside `allow_hosts`.

The task is in [the guide](../../guides/catalog/record.md).

```sh
driftgate record                                  # list what the session adds
driftgate record --write --exclude '/admin/*'     # add it to the catalog on Ctrl-C
```

## What becomes a catalog entry

`har import` and `record` share one pipeline: the same traffic gives the
same entries.

- Only requests to the application are taken, without static files, and never cookies or tokens.
- Uploaded files are replaced by synthetic files of the same type.
- With a route inventory, `/users/1` and `/users/2` become one request `/users/{id}` with `params: {id: "1"}`.
- A token that matches a configured identity gives the request that identity.
- With `identities:` or `tokens:` configured, `har import` runs the token command or `op read` to recognise identities. When that fails, identities stay unrecognised, and it says so.
- Duplicates are dropped by method, route, query parameter names, identity and body shape, not only identical requests.
- An entry with an identity is private. So is one whose response, any of them, held personal data or is not in the file whole.

### Requests left out because they carry a credential

A request that carries a credential or personal data is left out and
listed with the reason, to be added by hand. The check looks in the
query, the body, the form fields and the path.

- By name: a query parameter, JSON key or form field named like `password`, `currentPassword`, `authToken`, `api_key`, `jwt`, `ticket`, `nonce`, `csrf`, `otp`, `hash`, or `code` alone or with a word like `reset` or `verification`.
- By shape: a JWT, or a run of 20 or more characters that looks random. That is hex from 32 characters, or a mix of upper case, lower case and digits.
- A UUID, a 24-character hex ID and a camelCase name with a number at the end are not taken for credentials.

The shape check is a heuristic: it misses a short or unusual token, so
review what `--write` adds.

---
**Related:** [catalog.json](../catalog-json.md) · [exclusions.yaml](../exclusions-yaml.md) · [Command line basics](../cli.md)
