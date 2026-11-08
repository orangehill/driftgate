# Read a drift report

> **Before you start:** do the [tutorial](../../start/tutorial.md). **Time:** 10 minutes.

`driftgate run` and `driftgate compare` end with a report. This page
reads one, from the top.

## The verdict and the exit code

```console
$ driftgate run
...
capture:run-b299c63a4c38 against baseline:local: 3 compared, 0 passed, 3 drifted, 0 skipped, 0 observed
```

The summary counts the requests. The verdict line, `No drift ✓` or the
drifted requests above it, matches the exit code:

- `0`: clean;
- `1`: drift;
- `3`: the result is incomplete, and cannot be trusted. It is never green.

Every exit code is under [exit codes](../../reference/cli.md#exit-codes).

## One drift line

```
DRIFT  get-api-news-id
         type            $.entry.id  string → int
```

Each request that drifted gets a `DRIFT` block. Each line in it is one
drift: its [kind](../../reference/drift-kinds.md), its place in the
response, and the old and new value.

## Drift groups

```
Groups:
  [1] 3 drifts in 3 requests: header x-powered-by is new
      GET /api/news  (+2 more routes)
```

Drifts of the same kind at the same place are one group, often one
cause. `driftgate review` lists the groups with the command for each,
and `accept group:N` and `expect group:N` take a group by its number
([drift groups](../../reference/reports.md#drift-groups-and-their-numbers)).

## Values shown as (private)

```
         value           $.entry.published_at  (private) → (private)
```

A [private body](../../glossary.md#private-body) never shows its values
in a report. On your own machine, `driftgate review --show-private` shows
them, and keeps nothing. Generated requests are private by default.

## The lines after the verdict

```
kinds: header-extra 3, key-extra 3, type 3, value 3
catalog: 3 requests: 2 reads, 1 mutating, 0 command steps; 0 of them multipart
```

These lines are information; the exit code never depends on them.

- `kinds:` counts the drifts by kind.
- A line on one change in almost every request names that change, such as a new `server` header.
- `catalog:` and the list under it say what this result does not cover.
- Response times show requests that became much slower.

Each line is explained in
[Reading a report in detail](../../reference/reports.md).

## See every drift

```sh
driftgate compare run --max-drifts 1000         # every drift, not the first 20 per request
driftgate report --format json > report.json    # every drift, for a script
driftgate review --group 3                      # one group, with every request it touches
```

By default a request lists up to 20 drifts, and a request whose status
changed is summarized in one line. These commands show the rest.

---
**Next:** [Allow values that change by themselves](changing-values.md): the allowlist.
**Related:** [Reading a report in detail](../../reference/reports.md) · [Drift kinds](../../reference/drift-kinds.md) · [driftgate review](../../reference/commands/review.md#driftgate-review)
