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

- for generated entries, under [`requests`](driftgate-yaml.md#requests) in `driftgate.yaml`, then run `driftgate generate --write`; a later `generate` would write the catalog entry again;
- for entries written by hand, in the catalog.

### A fixed id in a path that changes data

A request that changes data with a fixed id in its path, such as
`PUT /api/users/17`, is warned of before the capture, one line per
request; `generate` warns too. The id is neither a `{parameter}` nor
from `params_from`. On another copy of the data that id may be a real
record, and the status would not show it. Take the id from a response
with [`params_from`](driftgate-yaml.md#templates-generate-params)
instead. The exit code stays.

Two kinds of number are not taken for an id:

- an API version: one or two digits right after a segment `api` or `v` in any letter case, followed by a non-empty segment that is not a number (`/api/2/users`; `/api/2/` is an id);
- a full date: a year from 1900 to 2099, a month `01` to `12` and a day `01` to `31`, as in `/posts/2024/05/17`.

Any other number is taken for an id: `/api/17`, `/stats/2024`, `/users/2024/5`.

### A read-only request that changed data

A read-only request that changed data is recorded with its side effects,
and the capture prints the `requests:` entries that mark it
`mutates: true`. On a server that is not isolated (shared with other
developers, users or background jobs), such a side effect may be their
traffic, not the request's. Check it before you mark the request.

To find that request, the capture sends the read-only requests again, in
groups, each group after a restore, `prepare` and the tokens command.

- Each group is compared with the state it started from. A tokens command that writes a new token on every run therefore does not hide the request; the capture notes that the starting state is not repeatable.
- When no single request explains a change and the starting state was not repeatable, the capture stops with exit 3. It names the change and what is not repeatable: the requests did not run on the same state, so the change is not passed as an order effect.
- When the starting state is repeatable, such a change is reported as one that no single request makes alone (an order effect or background work).

## Guards and prepare during a capture

When requests change data, the
[guards](driftgate-yaml.md#seed-prepare-guards) are checked after the
restore and `prepare`, which may set up what they check.

- They run before the first request, and again right before the first request that changes data when read-only requests ran in between.
- A guard that fails stops the capture there (exit 3), in seconds rather than after the read-only requests.
- Guards only check. A write of one becomes part of the state the read-only requests start from; a restore removes it before the requests that change data.
- Such a write may be reported as a change `prepare` made.

The capture fingerprints the state right after the restore and after
`prepare`. When the two differ, it warns that the state changed between
the restore and the end of `prepare`, and names the changed tables or
components.

- A `tokens.command` runs right after `prepare`. What it writes, such as the tokens of a login, is not taken for `prepare`'s.
- With both configured, the first capture takes one more fingerprint between them. The later restores (before the requests that change data, `--restore each`) are checked only without a `tokens.command`.
- Only components with a fingerprint are seen. The warning never changes the exit code.

## A migration run outside DriftGate

Every capture starts with a restore from the pristine copy. Before that
restore, the capture compares the working database with the pristine
copy, and stops with exit 3 when a migration ran on it since the seed:

- a table, view, trigger or other object was made or dropped;
- the definition of a table changed: its columns, keys or indexes;
- the definition of a view, trigger, function or type changed;
- the rows of a migrations table changed, such as Laravel's `migrations`, Rails' `schema_migrations` or Django's `django_migrations`.

The restore would undo that change, and the capture would run on the old
schema. Rows the application wrote never stop a capture, nor does a
migration that was rolled back.

- Without `prepare:`, nothing is restored, and the working database stays as it is.
- With `prepare:`, the restore and `prepare` run first. Each changed item must then differ from the pristine copy again, as after a migrate command. Otherwise the capture stops.

To go on, do one of these:

1. Add the migrate command to [`prepare:`](driftgate-yaml.md#what-belongs-in-prepare). Every capture then runs the migrations after the restore, and the baseline stays valid.
2. Or run `driftgate seed` to make the pristine copy anew, then follow [the next section](#when-the-state-does-not-match-the-baseline): the baseline was captured from the old copy.

A `--read-only` capture takes no `prepare:`, so seed instead. The check
reads definitions only when rows changed, of the working database; the
seed recorded the pristine copy's. For 100 changed tables it takes under
1 ms on SQLite, about 2 ms on MySQL and 13 ms on PostgreSQL.

## When the state does not match the baseline

When the restored state differs from the state the baseline was captured
from, the capture stops with exit 3 and lists the tables that differ.
The pristine copy holds other data: a seed since, other seed data, or
migrations.

- Seed the data the baseline was captured from.
- Or make a new baseline of this data: move the baseline's folder away, and the next `driftgate run` makes it (`--baseline <name>` for another than `local`).
- When the data changed on purpose, such as by a migration, make the new data the baseline's with the steps below.

```sh
driftgate capture migrated --skip-fingerprint-check
driftgate compare migrated --skip-fingerprint-check   # review each drift
driftgate baselines rebase local --from migrated --reason "Adds posts.slug"   # add --allow-freeze in mode: freeze
```

The capture without the check is marked unreliable, and only a
comparison with the same flag takes it. [`driftgate baselines rebase`](commands/review.md#driftgate-baselines-rebase)
then makes it the baseline's content, with the new state.

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

A development server under the load of a capture now and then times
out, drops a connection, or answers `502`, `503` or `504` once. A
read-only request of a safe method (`GET`, `HEAD`, `OPTIONS`) whose send
fails so is sent again, up to [`retry`](driftgate-yaml.md#retry) times
(default once), within `rate_limits`.

- The resends wait until the other read-only requests are done, and go one at a time. Without the load, a resend that passes is evidence, not luck.
- The capture lists such requests after its summary.
- A capture at a measured `concurrency: auto` that needed a retry the sequential capture did not counts as different from it.

No retry is spent on what cannot recover. These keep their first
response:

- a request whose baseline response failed the same way: the same status, a failed request on both sides, or a retry in the baseline that did not recover;
- the timeouts of a service, and its `504`s that took as long as `timeout`, once one of its resends, sent alone, timed out again;
- the `502`, `503` and `504` answers of a service once two of its resends got one again. An application in maintenance mode answers `503` to everything.

Not retried at all:

- a `500`: an exception of the application, such as the first call after a restore on a cold cache, stays a drift;
- any other method, also with `"mutates": false`;
- requests that change data, also a `GET` the baseline knows writes (its response there has side effects);
- sequences, and so no request that takes a parameter from another response;
- command steps and repeated requests;
- a refused connection: the application is not running.

A request already sent again after a restore that ended the
application's database sessions (SQL Server,
[whole restore](databases/sql-server.md#restore-of-the-whole-database))
is retried only when that second answer is a load failure. Its record
then keeps both: `resent` with the first answer, `retry` with the second.
A resend that fails in another way than load, such as a refused
connection or an interrupt, did not recover.

A retry never makes a failure pass unseen.

- The record of the response keeps the first send's outcome, and whether a later send recovered: `"retry": {"sends": 2, "status": 503, "ms": 41, "recovered": true}`, or `"error": "timeout"`. It is never compared, but shown everywhere.
- The capture names the requests answered only after a retry. `run` warns when it makes a baseline from such a response, and so does `accept`.
- `compare` and `report` show a request that passed only after a retry as a class of its own in every format ([how](reports.md#requests-that-passed-only-after-a-retry)). It counts as passed: the exit code does not change.
- A request that failed in every send is never counted as passed after a retry. It shows as failed in all its sends. When it passes anyway (the baseline failed the same way, or an expected drift), every format names it as such.

A request that is retried in every run fails now and then for a reason
of its own. Set `retry: 0` to see each failure as a drift.

## Personal data in a public response

A public response in which the
[personal-data detector](driftgate-yaml.md#storage-and-pii) finds
something is stored encrypted as private, and reported with a masked
value. The capture exits 2.

---
**Related:** [driftgate capture](commands/check.md#driftgate-capture) · [state](state.md) · [Reading a report in detail](reports.md)
