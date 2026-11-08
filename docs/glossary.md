# Glossary

Each term has one meaning in these pages. A definition is one or two
sentences; "More" links the full description.

## The check

### Baseline
The stored responses that each capture is compared with. Baselines have names; the default name is `local`. More: [baseline names](reference/cli.md#which-baseline-a-command-uses).

### Capture
One run of the catalog against the application, stored under a tag in the local store. More: [driftgate capture](reference/commands/check.md#driftgate-capture).

### Catalog
The list of requests that every capture sends, in `catalog.json`. More: [catalog.json](reference/catalog-json.md).

### Request
One entry of the catalog, named by its unique request ID, which reports, `--only` and globs use. More: [catalog.json](reference/catalog-json.md).

### Route
A method and path the application serves, such as `GET /api/orders/{id}`. More: [driftgate discover](reference/commands/catalog.md#driftgate-discover).

### Route inventory
The routes of the application, in `routes.json`, written by `discover --write`. More: [driftgate discover](reference/commands/catalog.md#driftgate-discover).

### Coverage
Which routes have a request meant to succeed. `driftgate coverage` lists the routes that have none. More: [driftgate coverage](reference/commands/catalog.md#driftgate-coverage).

### Drift
One difference between a capture and the baseline. More: [drift kinds](reference/drift-kinds.md).

### Drift kind
The class of a drift, such as `status` or `item-order`. More: [drift kinds](reference/drift-kinds.md).

### Drift group
Drifts of the same kind at the same place, across requests, numbered `[1]`, `[2]` and so on. `accept`, `expect` and `bisect` take a group by its number. More: [driftgate review](reference/commands/review.md#driftgate-review).

### Side effect
A change a request made to the data. A capture finds it by comparing fingerprints before and after the request. More: [state](reference/state.md).

### Last comparison
The result of the latest `compare` or `run`. `report`, `review`, `accept`, `expect` and `bisect` read it. More: [driftgate compare](reference/commands/check.md#the-last-comparison).

### Pass
A comparison with no drift other than expected ones. It exits with code 0. More: [exit codes](reference/cli.md#exit-codes).

### Verdict
The outcome of a comparison: a pass, drift, or an incomplete result. It sets the exit code: 0, 1 or 3. More: [exit codes](reference/cli.md#exit-codes).

### Incomplete result
A result that cannot be trusted, such as a comparison that compared nothing. It exits with code 3 and is never a pass. More: [exit codes](reference/cli.md#exit-codes).

### Mode
`freeze` or `evolve`. In `freeze`, every drift fails unless a ticket expects it; in `evolve`, a drift may be accepted. More: [mode](reference/driftgate-yaml.md#mode).

### Level
How far a comparison goes: `status`, `shape`, `types` or `values`. Each level includes the ones before it. More: [level](reference/driftgate-yaml.md#level).

## Data and state

### State
The data the application runs on: its databases and anything else a capture restores. Every capture starts from the same state. More: [state](reference/state.md).

### State component
One entry under `state:`: a database, or anything else restored by commands. More: [state](reference/state.md).

### Working database
The database the application uses, named by `working:`. A capture restores it from the pristine copy. More: [state](reference/state.md).

### Pristine copy
A copy of the working database made by `driftgate seed`. Every restore copies the data back from it. More: [state](reference/state.md).

### Seed
The `seed:` commands load the data; `driftgate seed` runs them and keeps the pristine copy. More: [driftgate seed](reference/commands/state.md#driftgate-seed).

### Restore
Put the state back to how the seed left it: databases from the pristine copy, other components by their `restore` command. Every capture starts from the same data. More: [state](reference/state.md).

### Fingerprint
A hash of the state. A capture stops with exit code 3 when the restored state does not match the baseline's fingerprint. More: [state](reference/state.md).

### Guard
A command that checks the environment before a capture sends requests that change data, such as that mail goes to a log. A guard that fails stops the capture. More: [seed, prepare, guards](reference/driftgate-yaml.md#seed-prepare-guards).

### Prepare
Commands that run after every restore, such as clearing a cache. More: [seed, prepare, guards](reference/driftgate-yaml.md#seed-prepare-guards).

### Read-only request
A request that does not change data. By default these are `GET`, `HEAD` and `OPTIONS` requests; they run first. More: [catalog.json](reference/catalog-json.md).

### Request that changes data
A request with any other method, or with `"mutates": true`. After the read-only requests and one restore, these run one after another, each on the data the earlier ones left (`--restore once`, the default). More: [catalog.json](reference/catalog-json.md).

## Rules and decisions

### Allowlist rule
An entry in `allowlist.yaml` that lets one value change, within a format and with a reason. More: [allowlist.yaml](reference/allowlist-yaml.md).

### Exclusion
A route that is not checked on purpose, with a reason, in `exclusions.yaml`. More: [exclusions.yaml](reference/exclusions-yaml.md).

### Known failure
A request that fails in the baseline, acknowledged in `exclusions.yaml`. It is still sent and compared. More: [exclusions.yaml](reference/exclusions-yaml.md).

### Observe
Measure a request (status, time, size) without comparing it: `"observe": true` in the catalog, or `observe:` in `driftgate.yaml` for generated requests. More: [observe](reference/driftgate-yaml.md#observe).

### Accept
Take the drifts of the last comparison into the baseline, with a reason. Only in mode `evolve`. More: [driftgate accept](reference/commands/review.md#driftgate-accept).

### Acceptance log
The file `acceptances.jsonl` of a baseline: one line for each change, each line chained to the one before. More: [driftgate accept](reference/commands/review.md#driftgate-accept).

### Expectation
A drift recorded for a ticket with `driftgate expect`. A comparison passes the exact drift and fails on any other; the baseline does not change. More: [driftgate expect](reference/commands/review.md#driftgate-expect).

### Ref
The ticket or link given with `--ref` to `expect`, `accept` and `baselines rebase`. More: [driftgate expect](reference/commands/review.md#driftgate-expect).

### Rebase
Make a capture the new content of a baseline, such as after a merge. More: [driftgate baselines rebase](reference/commands/review.md#driftgate-baselines-rebase).

### Manifest
The file `manifest.json` of a baseline: the SHA-256 of each of its files. `driftgate verify` checks the baseline against it. More: [driftgate verify](reference/commands/review.md#driftgate-verify).

## Files and storage

### Local layout
`baselines: local`: every file is in the project's `.driftgate/` folder, which git ignores. New projects use it. More: [baselines](reference/driftgate-yaml.md#baselines).

### Git layout
`baselines: git`: `driftgate.yaml` and the `driftgate/` folder are committed, so the team and CI share the baselines. More: [baselines](reference/driftgate-yaml.md#baselines).

### Local store
The folder that holds captures, private bodies and pristine metadata. It never enters git. More: [store](reference/driftgate-yaml.md#store).

### Project key
The key that encrypts private bodies. `driftgate key create` makes one. More: [key](reference/driftgate-yaml.md#key).

### Private body
A response body kept encrypted in the local store, never in the repository. Generated requests are private by default. More: [storage and pii](reference/driftgate-yaml.md#storage-and-pii).

### Public body
A response body stored as it is, after a scan for personal data. More: [storage and pii](reference/driftgate-yaml.md#storage-and-pii).

### Personal-data detector
The scan of every public body for e-mail addresses, phone numbers, a Serbian personal ID number (with its check digit), card numbers and IBANs. More: [storage and pii](reference/driftgate-yaml.md#storage-and-pii).

### Worktree
A second checkout of the repository made with `git worktree add`, often one per change. More: [baselines](reference/driftgate-yaml.md#baselines).

### Main worktree
The first checkout of the repository. In the local layout, `driftgate setup` in another worktree takes the project files from it. More: [baselines](reference/driftgate-yaml.md#baselines).

## Requests and services

### Service
One application the project checks, configured under `target:`, or one entry under `targets:`. More: [target and targets](reference/driftgate-yaml.md#target-and-targets).

### Identity
A user that requests are sent as, named under `identities:`. More: [identities and tokens](reference/driftgate-yaml.md#identities-and-tokens).

### Token
The secret sent for an identity as `Authorization: Bearer <token>`. It comes from the environment, 1Password, a command or a file. More: [identities and tokens](reference/driftgate-yaml.md#identities-and-tokens).

### Sequence
Requests that run in order, together, such as a login and the requests that use its session. More: [catalog.json](reference/catalog-json.md).

### Command step
A catalog entry that runs a program instead of sending a request, such as a queue worker. More: [command steps](reference/catalog-json.md#command-steps).

### Template
A variant of a request that `generate` makes for every route, such as `ok`, `not-found` or `json`. More: [templates, generate, params](reference/driftgate-yaml.md#templates-generate-params).

### Adapter
The part of `discover` that finds the routes of one framework, such as `laravel` or `openapi`. More: [discover](reference/driftgate-yaml.md#discover).

### Origin
The `scheme://host[:port]` of a service. Captures from different origins are compared as if they had one origin. More: [target and targets](reference/driftgate-yaml.md#target-and-targets).

### Alias
Another origin under which the application writes its own URLs, listed under `aliases:`. More: [target and targets](reference/driftgate-yaml.md#target-and-targets).

### Retry
Sending a read-only request again after it failed under load. The report always shows a request that passed only after a retry. More: [retries](reference/capture.md#retries-of-requests-that-failed-under-load).

### Side check
A check of what the application does besides answering, such as its schema, files, Redis keys or mail, in the same gate. More: [side checks](guides/catalog/side-checks.md).

## Words we do not use

- "local mode": say "the local layout", or "the baseline `local`" for the name.
- "target" for a running application: say "service". `target` and `targets:` are only keys.
- "snapshot", "golden file" or "recording" for a baseline: say "baseline". "Record" is only the `record` command.
- "test" for a catalog entry: say "request".
- "mutating" in prose: say "a request that changes data".

---
**Related:** [Drift kinds](reference/drift-kinds.md) · [Commands](commands.md) · [What DriftGate can do](capabilities.md)
