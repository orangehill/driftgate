# How DriftGate works

> **Before you start:** do the [tutorial](../start/tutorial.md). **Time:** 10 minutes.

DriftGate records how a running application answers, and checks after
every change that it still answers the same way. It works from the
outside, over HTTP, and needs nothing added to the application.

## The check from start to end

```
discover → generate → coverage → seed → restore → capture → compare → review
 routes     catalog    gaps      pristine  same     send the  each     accept,
                                 copy      data     catalog   drift    expect or fix
```

`driftgate run` does the steps up to `compare` in one command
([the steps run takes](../reference/commands/check.md#the-steps-run-takes)).
`review`, `accept` and `expect` are yours.

## What a baseline is

A [baseline](../glossary.md#baseline) is the stored answers of every
request in the catalog: status, headers and body, as they were. Every
later capture is compared with it, literally: `42` instead of `"42"` is a
drift. The baseline changes only when you accept a change.

## Why every capture starts from the same data

Before each capture, DriftGate restores the databases to the pristine
copy, so every capture runs on the same data. A difference in the
answers then comes from the code, not from the data.

- A fingerprint of the restored state proves the data was the same. When it was not, the capture stops (exit 3) instead of reporting drift.
- DriftGate also records what each request changed in the data. A request that starts writing elsewhere is a drift, even when its answer is the same.

Details: [State: restoring the data](../reference/state.md).

## What counts as a drift

A [drift](../glossary.md#drift) is any difference between a capture and
the baseline: a status, a header, a key, a type, a value, a side effect.
Each drift has a [kind](../reference/drift-kinds.md), a place and both
values. Nothing is hidden unless an [allowlist rule](../glossary.md#allowlist-rule)
says that this one value may change, within a format and with a reason.

## Which level to use

The [level](../reference/driftgate-yaml.md#level) says how far a
comparison goes: `status`, `shape`, `types` or `values`.

- Locally the data is identical on every capture, so `values` is the default, and should stay. Value bugs are the usual upgrade regressions: money, a status, a date, sorting, a wrong record, a translation, rounding, an empty name.
- On data DriftGate does not control, such as a live server or newer data, set `level: types` in `driftgate.yaml`, or `"level": "types"` on a catalog entry. Add allowlist rules for the fields that change by themselves.
- A pass there means the contract is unchanged, not the behaviour.
- The fingerprint check of the restored state never produces drift; side effects do. It proves the data was the same; when it was not, the capture stops.

## Why the first run captures twice

A value can change by itself: a time, a token, a random ID. The first
`run` captures twice on the same data, and makes the baseline only when
both captures agree. Otherwise it proposes allowlist rules for what
differed, and you add a reason.

## Who may change the baseline: freeze and evolve

- In `mode: freeze` (the default), every drift fails, unless a ticket [expects](../glossary.md#expectation) exactly that drift. Nothing is accepted.
- In `mode: evolve`, an intended drift may be [accepted](../glossary.md#accept), with a reason, into the baseline.

Every acceptance is a line in the baseline's acceptance log
([mode](../reference/driftgate-yaml.md#mode)).

## The three principles

### Never a false pass

A green result must mean the checked answers did not change. A result
that cannot be trusted is incomplete (exit 3), never green: one on other
data, or one that compared nothing
([what a pass proves](what-a-pass-proves.md)).

### Local first, CI second

The main use is local: checking each step of an upgrade on your machine,
before the pull request. The baseline then stays on your machine
([the local layout](project-layout.md#the-local-layout-and-the-git-layout)).
Running the same check in CI is optional.

### One upgrade step at a time

Upgrade one version, or one dependency, per step, and run the check after
each. A small step gives a few drifts with one cause, which you can fix
or accept
([upgrade a framework](../guides/framework-upgrade.md)).

---
**Next:** [Set up your own project](../start/your-project.md): run setup on your application.
**Related:** [Glossary](../glossary.md) · [Where DriftGate keeps its files](project-layout.md) · [What a green result proves](what-a-pass-proves.md)
