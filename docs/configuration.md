# Configuration

Every file DriftGate reads now has its own reference page. This page
keeps the old headings, so older links still work.

| File | Reference |
|---|---|
| `driftgate.yaml` | [driftgate.yaml](reference/driftgate-yaml.md), [state](reference/state.md), [database settings](reference/database-settings.md), [databases](reference/README.md#databases) |
| `allowlist.yaml` | [allowlist.yaml](reference/allowlist-yaml.md) |
| `exclusions.yaml` | [exclusions.yaml](reference/exclusions-yaml.md) |
| `catalog.json` | [catalog.json](reference/catalog-json.md) |
| `classify.yaml` | [classify.yaml](reference/classify-yaml.md) |

## What to use for what

| Task or symptom | What to use |
|---|---|
| a value that may change on every request (a time, a token) | a [body or header rule](reference/allowlist-yaml.md), with a format and a reason |
| the same header differs on every request (another server stack) | a [header rule](reference/allowlist-yaml.md#header-rules); the [line on one change](reference/reports.md#one-change-in-almost-every-request) proposes it |
| rows in another order (`item-order`; another database server or ORM) | an [array rule](reference/allowlist-yaml.md#array-rules): `key` pairs the items, `any_order` allows the order |
| uploading a file | a [`multipart`](reference/catalog-json.md#multipart) catalog entry with a fixture from the uploads folder |
| a queue worker, a command, an import or export, an admin path that changes the schema | a [command step](reference/catalog-json.md#command-steps), or a catalog request for the path; [side checks](guides/side-checks.md) |
| measured, not compared (a value no rule can describe) | [`observe`](reference/driftgate-yaml.md#observe) |
| what the gate does not check | `driftgate coverage`, and the [not-covered summary](reference/reports.md#what-the-result-does-not-cover) after every comparison |
| two server stacks (database, PHP, web server) | [upgrading the server](guides/workflow/server-upgrade.md) |

The header rule's line names `server`, `x-powered-by` and `via` with the
exact new value. Any other header that changed is fixed, accepted or
expected. `driftgate help catalog`, `help allowlist`, `help exclusions`,
`help classify` and `help config` list the fields of those files.

## driftgate.yaml
Moved to [driftgate.yaml](reference/driftgate-yaml.md).
### Top-level keys
Moved to [Top-level keys](reference/driftgate-yaml.md#top-level-keys).
### baselines
Moved to [baselines](reference/driftgate-yaml.md#baselines).
### store
Moved to [store](reference/driftgate-yaml.md#store).
### key
Moved to [key](reference/driftgate-yaml.md#key).
#### Handing a capture to another developer
Moved to [Hand a capture to another developer](guides/workflow/share-baselines.md#hand-a-capture-to-another-developer).
### target and targets
Moved to [target and targets](reference/driftgate-yaml.md#target-and-targets).
#### connect_to
Moved to [connect_to](reference/driftgate-yaml.md#connect_to).
#### Retries
Moved to [Retries of requests that failed under load](reference/capture.md#retries-of-requests-that-failed-under-load).
### identities and tokens
Moved to [identities and tokens](reference/driftgate-yaml.md#identities-and-tokens).
### state
Moved to [State: restoring the data](reference/state.md).
#### restore_only
Moved to [restore_only](reference/state.md#restore_only).
#### The framework's own tables
Moved to [The framework's own tables](reference/state.md#the-frameworks-own-tables).
#### Database DSNs
Moved to [Database connection settings](reference/database-settings.md).
#### SQLite
Moved to [SQLite](reference/databases/sqlite.md).
#### SQL Server
Moved to [SQL Server](reference/databases/sql-server.md).
### seed, prepare, guards
Moved to [seed, prepare, guards](reference/driftgate-yaml.md#seed-prepare-guards).
### discover
Moved to [discover](reference/driftgate-yaml.md#discover).
### templates, generate, params
Moved to [templates, generate, params](reference/driftgate-yaml.md#templates-generate-params).
### storage and pii
Moved to [storage and pii](reference/driftgate-yaml.md#storage-and-pii).
### observe
Moved to [observe](reference/driftgate-yaml.md#observe).
### requests
Moved to [requests](reference/driftgate-yaml.md#requests).
### uploads
Moved to [uploads](reference/driftgate-yaml.md#uploads).
### bisect
Moved to [bisect](reference/driftgate-yaml.md#bisect).
### data
Moved to [data](reference/driftgate-yaml.md#data).

## allowlist.yaml
Moved to [allowlist.yaml](reference/allowlist-yaml.md).

## exclusions.yaml
Moved to [exclusions.yaml](reference/exclusions-yaml.md).

## catalog.json
Moved to [catalog.json](reference/catalog-json.md).
### multipart
Moved to [multipart](reference/catalog-json.md#multipart).
### repeat
Moved to [repeat](reference/catalog-json.md#repeat).
### command steps
Moved to [command steps](reference/catalog-json.md#command-steps).

## classify.yaml
Moved to [classify.yaml](reference/classify-yaml.md).
### subset
Moved to [subset](reference/classify-yaml.md#subset).

---
**Related:** [Command line basics](reference/cli.md) · [Glossary](glossary.md) · [Commands](commands.md)
