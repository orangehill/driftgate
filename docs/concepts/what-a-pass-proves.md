# What a green result proves

> **Before you start:** read [How DriftGate works](how-it-works.md). **Time:** 5 minutes.

A pass (`No drift ✓`, exit 0) proves one thing exactly. The requests in
the catalog got the same answers, and made the same changes to the data,
as in the baseline. This page says what that covers.

## What a pass checks

- the status, headers and body of every request in the catalog, at its [level](../reference/driftgate-yaml.md#level);
- what each request changed in the data: its [side effects](../glossary.md#side-effect);
- the exit code, and with `"output": true` the output, of each [command step](../reference/catalog-json.md#command-steps);
- that every capture ran on the same data: the [state fingerprint](../glossary.md#fingerprint).

## What a pass does not check

A pass proves nothing about a request that is not in the catalog.

- It does not exercise file uploads, unless the catalog has [`multipart`](../reference/catalog-json.md#multipart) requests with fixtures.
- It does not reach admin paths that change the schema.
- It does not reach code that only background jobs, queue workers or commands run.
- It does not check requests a frontend sends by itself, unless you [record](../guides/catalog/record.md) them into the catalog.

Command steps with `"output": true` and state fingerprints bring the rest
into the gate ([side checks](../guides/catalog/side-checks.md)). The table of
[what DriftGate can do](../capabilities.md) names the feature for each.

## Why a result can be incomplete

A result that cannot be trusted is incomplete (exit 3), never green.

- The restored data differs from the baseline's.
- A comparison compared nothing: a typo in `--only`, renamed request IDs, or a baseline made for another catalog.
- Private bodies are not on this machine, without `--skip-private`.

An incomplete result is not a regression: fix the cause and run again
([exit codes](../reference/cli.md#exit-codes)).

## Gaps the report lists after every comparison

After the verdict, every report counts what the result does not cover:

- routes without a request meant to succeed;
- requests whose answer in the baseline is already an error;
- requests only observed, and requests that change data that were skipped;
- known failures.

The exit code never depends on them. `driftgate coverage` lists the
routes ([what the result does not cover](../reference/reports.md#what-the-result-does-not-cover)).

## Requests that passed only after a retry

A read-only request that failed under load is sent again, once by
default. If it then passes, every report says so: it is never shown as a
plain pass. A request that needs a retry in every run fails for a reason
of its own
([retries](../reference/capture.md#retries-of-requests-that-failed-under-load)).

## A cache emptied at every restore

A cache emptied on every restore (`FLUSHALL`) means every request runs
on a cold cache.

- Behaviour that only shows with a warm cache, such as a value read from the cache instead of the database, is never checked.
- A server that answers from a warm cache may differ from the capture for that reason alone. Compare a live server's answer twice, or empty its cache first, before you take such a difference for a change.

How other components are restored is in
[restore anything else with commands](../reference/state.md#restore-anything-else-with-commands).

---
**Next:** [Where DriftGate keeps its files](project-layout.md): the local layout, the store and the key.
**Related:** [Reading a report in detail](../reference/reports.md) · [Exit codes](../reference/cli.md#exit-codes) · [Drift kinds](../reference/drift-kinds.md)
