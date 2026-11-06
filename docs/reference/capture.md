# How a capture runs

What [`driftgate capture`](commands/check.md#driftgate-capture) and
`run` do between the restore and the stored responses.

## The order of requests

Read-only requests run first, in parallel up to `concurrency`. Requests
that change data, sequences, command steps and repeated requests run
after them, one at a time, after a restore.

## Restore once or before each sequence

`capture --restore` decides how often the state is restored.

- `once`, the default: restore before the read-only requests, and once more before the requests that change data. Those then run one after another, each on the data the ones before it left.
- A sequence with `restore: before` and a repeated request get their own restore, also with `once`.
- `each`: restore before every sequence and every request that changes data.

## Requests that change data

### Requests that change data only by their method

Requests that change data only because of their method are listed before
the capture. They have no `mutates` in the catalog, as in a catalog of an
earlier release. A search sent as `POST` among them runs after the
writes, and sees their data.

Say `mutates: false` for those that only read, and `mutates: true` for
the others:

- for generated entries, under [`requests`](../configuration.md#requests) in `driftgate.yaml`, then run `driftgate generate --write`; a later `generate` would write the catalog entry again;
- for entries written by hand, in the catalog.

### A fixed id in a path that changes data

A request that changes data with a fixed id in its path, such as
`PUT /api/users/17`, is warned of before the capture, one line per
request; `generate` warns too. The id is neither a `{parameter}` nor
from `params_from`. On another copy of the data that id may be a real
record, and the status would not show it. Take the id from a response
with [`params_from`](../configuration.md#templates-generate-params)
instead. The exit code stays.

### A read-only request that changed data

A read-only request that changed data is recorded with its side effects,
and the capture prints the `requests:` entries that mark it
`mutates: true`. On a server that is not isolated (shared with other
developers, users or background jobs), such a side effect may be their
traffic, not the request's. Check it before you mark the request.

## Guards and prepare during a capture

When requests change data, the
[guards](../configuration.md#seed-prepare-guards) are checked after the
restore and `prepare`, which may set up what they check.

- They run before the first request, and again right before the first request that changes data when read-only requests ran in between.
- A guard that fails stops the capture there (exit 3), in seconds rather than after the read-only requests.
- Guards only check. A write of one becomes part of the state the read-only requests start from; a restore removes it before the requests that change data. It may be reported as a change `prepare` made.

When `prepare` changed the restored state, the capture warns and lists
what it changed. The exit code stays.

## When the state does not match the baseline

When the restored state differs from the state the baseline was captured
from, the capture stops with exit 3 and lists the tables that differ.
The pristine copy holds other data: a seed since, other seed data, or
migrations.

- Seed the data the baseline was captured from.
- Or make a new baseline of this data: move the baseline's folder away, and the next `driftgate run` makes it (`--baseline <name>` for another than `local`).
- When the data changed on purpose, you can also capture with `--skip-fingerprint-check`, and make that capture the baseline's content with [`driftgate baselines rebase`](commands/review.md#driftgate-baselines-rebase).

When the items that differ are tables made `restore_only` since, it says
that the configuration changed: make the baseline again.

## Parallel requests and concurrency auto

A service with `concurrency: auto` captures with the setting its
baseline recorded, and one request at a time while none is recorded.
`run` decides it when it makes the baseline; `capture --calibrate`
decides it for an existing one.

1. After the sequential captures agree, a probe sends up to the first 32 read-only requests at 1, 2, 4 and 8 at once.
2. It takes the smallest setting within 10% of the best throughput.
3. A capture at that setting is compared with the sequential one of the same restored state.
4. The setting is kept only when the two are identical; otherwise it stays 1, naming the requests that differed.
5. It is recorded with its evidence in the baseline's `meta.json`; commit it with `baselines: git`.

When a later capture at that setting changes data that no single
read-only request explains, the baseline is put back to 1, and the
capture runs again one request at a time.

## Retries of requests that failed under load

A read-only `GET`, `HEAD` or `OPTIONS` request whose send times out,
loses its connection or gets a `502`, `503` or `504` is sent again. By
default that happens once, after the other read-only requests, one at a
time ([`retry`](../configuration.md#retries)).

- Its record keeps the first send's outcome.
- The capture lists such requests after its summary.
- A capture at a measured `concurrency: auto` that needed a retry the sequential capture did not counts as different from it.

How a report shows them is under
[Requests that passed only after a retry](reports.md#requests-that-passed-only-after-a-retry).

## Personal data in a public response

A public response in which the
[personal-data detector](../configuration.md#storage-and-pii) finds
something is stored encrypted as private, and reported with a masked
value. The capture exits 2.

---
**Related:** [driftgate capture](commands/check.md#driftgate-capture) · [state](../configuration.md#state) · [Reading a report in detail](reports.md)
