# Fix, accept or expect a drift

> **Before you start:** [read a drift report](read-the-report.md). **Time:** 10 minutes.

Every drift that is left after the allowlist gets one of three answers:
fix the code, accept the change, or expect it for a ticket.

## Decide what a drift means

```sh
driftgate review                 # the drift groups, with the command for each
driftgate review --group 3       # one group, with every request it touches
```

- **Not intended:** fix the code until the old answer comes back. This is the usual case during an upgrade: clients should see no change.
- **Intended, and the team may change the baseline:** accept it, in `mode: evolve`.
- **Intended by a ticket, while the baseline stays:** expect it, in `mode: freeze` or `evolve`.

A value that changes on every run is none of these: it gets an
[allowlist rule](changing-values.md).

## Accept an intended change

```sh
driftgate accept group:3 --reason "Dates are ISO 8601 since Laravel 7" --ref APP-512
```

`accept` takes the drifts into the baseline. A request is accepted whole:
`review` names all the groups a request falls into in one command.

- The reason, the ticket, who and when go into the baseline's acceptance log.
- `--comparison <id>`, as `review` prints it, makes sure you accept what you reviewed.
- With `baselines: git`, commit the baseline and its log.

What `accept` refuses is under
[driftgate accept](../../reference/commands/review.md#driftgate-accept).

## Expect a change a ticket makes

```sh
driftgate expect group:3 --ref APP-512 --reason "Dates are ISO 8601 since Laravel 7"
```

`expect` records exactly these drifts for the ticket. The baseline does
not change. The gate then passes that exact drift and nothing else.

- An expectation holds only for the same values: a header that changes again fails.
- A side effect cannot be expected.
- When the ticket is merged and the baseline is [rebased](../workflow/share-baselines.md#rebase-the-baseline-after-a-merge), the expectations it now holds are closed.
- `driftgate expect --drop APP-512 --reason "…"` ends a ticket's expectations, such as one that stays unmet.

How an expectation matches is under
[driftgate expect](../../reference/commands/review.md#driftgate-expect).

## Accept a new request

```sh
driftgate accept get-api-reports --reason "New report endpoint"
```

A request the baseline has no answer for is skipped and listed, not
failed. `accept` it by its ID to add its answer to the baseline. A
request the capture no longer has (`not-captured`) leaves the baseline
the same way, by its ID.

In `mode: freeze`, `accept` is refused, also for a new request. There,
capture the whole catalog and take the capture into the baseline with
`driftgate baselines rebase local --from <tag> --reason "…" --allow-freeze`,
which adds the new requests' answers too
([baselines rebase](../../reference/commands/review.md#driftgate-baselines-rebase)).

## Keep a failing request without hiding it

A request that already fails in the baseline can stay in the catalog,
still compared, as a known failure in `exclusions.yaml`. How:
[keep a request that fails in the baseline](../catalog/coverage.md#keep-a-request-that-fails-in-the-baseline).

---
**Next:** [Make the catalog cover every route](../catalog/coverage.md): check what the gate does not see.
**Related:** [driftgate accept](../../reference/commands/review.md#driftgate-accept) · [driftgate expect](../../reference/commands/review.md#driftgate-expect) · [mode](../../reference/driftgate-yaml.md#mode)
