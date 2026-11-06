# Commands

Every command now has its section in one of the command pages, grouped by
task. What all commands share is in [Command line basics](reference/cli.md).
This page keeps the old headings, so older links still work.

- [Set up a project](reference/commands/project.md) · [The pristine copy](reference/commands/state.md) · [Routes and the catalog](reference/commands/catalog.md)
- [Capture and compare](reference/commands/check.md) · [Review and record results](reference/commands/review.md) · [The data module](reference/commands/data.md)

## Common flags
Moved to [Common flags](reference/cli.md#common-flags).

## Interrupts and the local store
Moved to [What happens when you press Ctrl-C](reference/cli.md#what-happens-when-you-press-ctrl-c) and [When a command waits for another one](reference/cli.md#when-a-command-waits-for-another-one).

## Exit codes
Moved to [Exit codes](reference/cli.md#exit-codes).

## init
[driftgate init](reference/commands/project.md#driftgate-init): write `driftgate.yaml`, a catalog and an allowlist from the frameworks found.

## setup
[driftgate setup](reference/commands/project.md#driftgate-setup): set up a new project from zero to its first baseline.

## seed
[driftgate seed](reference/commands/state.md#driftgate-seed): run the `seed:` commands and keep a pristine copy.

## clean
[driftgate clean](reference/commands/state.md#driftgate-clean): remove the pristine copies `seed` made.

## run
[driftgate run](reference/commands/check.md#driftgate-run): routes, catalog, capture and compare in one command.

## discover
[driftgate discover](reference/commands/catalog.md#driftgate-discover): find the routes and compare them with the route inventory.

## generate
[driftgate generate](reference/commands/catalog.md#driftgate-generate): generate catalog requests for routes not yet covered.

## coverage
[driftgate coverage](reference/commands/catalog.md#driftgate-coverage): list the routes without a request that succeeds.

## har import
[driftgate har import](reference/commands/catalog.md#driftgate-har-import): add the requests of a HAR file to the catalog.

## record
[driftgate record](reference/commands/catalog.md#driftgate-record): a recording proxy that proposes catalog requests.

## capture
[driftgate capture](reference/commands/check.md#driftgate-capture): restore the state, send the catalog, store the responses.

### Read-only check of a live server
Moved to [Capture only the requests that do not change data](reference/commands/check.md#capture-only-the-requests-that-do-not-change-data).

## compare
[driftgate compare](reference/commands/check.md#driftgate-compare): compare a capture with a baseline or another capture. The report is explained in [Reading a report in detail](reference/reports.md).

## report
[driftgate report](reference/commands/check.md#driftgate-report): write the last comparison again, for example as a pull-request comment.

## allowlist check
[driftgate allowlist check](reference/commands/review.md#driftgate-allowlist-check): check the allowlist without capturing anything.

## review
[driftgate review](reference/commands/review.md#driftgate-review): the drift groups of the last comparison, with the command for each.

## accept
[driftgate accept](reference/commands/review.md#driftgate-accept): take drifts of the last comparison into its baseline.

## expect
[driftgate expect](reference/commands/review.md#driftgate-expect): record drifts a ticket intends, without changing the baseline.

## verify
[driftgate verify](reference/commands/review.md#driftgate-verify): check baselines against their manifest and acceptance log.

## bisect
[driftgate bisect](reference/commands/review.md#driftgate-bisect): find the first commit of each drift group.

## data classify
[driftgate data classify](reference/commands/data.md#driftgate-data-classify): propose rules for the columns `classify.yaml` does not cover.

## data anonymize
[driftgate data anonymize](reference/commands/data.md#driftgate-data-anonymize): copy the source with personal values replaced.

## data subset
[driftgate data subset](reference/commands/data.md#driftgate-data-subset): anonymize a sample with closed references.

## data verify
[driftgate data verify](reference/commands/data.md#driftgate-data-verify): check an output of anonymize for personal data.

## baselines
[driftgate baselines](reference/commands/project.md#driftgate-baselines): move the project to the local or the git layout.

## baselines rebase
[driftgate baselines rebase](reference/commands/review.md#driftgate-baselines-rebase): make a capture the content of a baseline.

## key create
[driftgate key create](reference/commands/project.md#driftgate-key-create): create the project key.

## version
[driftgate version](reference/commands/project.md#driftgate-version): print the version.

## Environment variables
Moved to [Environment variables](reference/cli.md#environment-variables).

---
**Related:** [Command line basics](reference/cli.md) · [Glossary](glossary.md) · [Configuration](configuration.md)
