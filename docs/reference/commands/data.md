# Commands: the data module

The commands that make anonymized test data from a source database. They
need [`data:`](../driftgate-yaml.md#data) in `driftgate.yaml` and the
rules of [classify.yaml](../classify-yaml.md). The task
is in [anonymized test data](../../guides/ci/test-data.md).

## driftgate data classify

```
driftgate data classify [--write] [--sample N]
```

Proposes rules for the columns `classify.yaml` does not cover.

| Flag | Meaning |
|---|---|
| `--write` | add the proposed rules to `classify.yaml` |
| `--sample N` | rows sampled per table (default 200) |

- Exit 1 when there are columns or schema findings without a rule (without `--write`).
- Proposed rules carry `proposed: true` until a person removes the mark.
- Sampled values are never written.

```sh
driftgate data classify --write
```

## driftgate data anonymize

```
driftgate data anonymize [--dump <file>] [--no-target] [--replace]
    [--format text|json]
```

Copies the source database of `data:` into its target database, a dump,
or both, with personal values replaced by the rules of `classify.yaml`.
The output is made only if `data verify` finds nothing in it.

| Flag | Meaning |
|---|---|
| `--dump <file>` | also write a dump to this file |
| `--no-target` | write only the dump (needs `--dump`) |
| `--replace` | drop an existing target database and write it again |
| `--format text\|json` | output format (default text) |

- It stops with exit 2 when a column has no rule, a rule is still proposed or stale, or a schema object holds unconfirmed personal-looking literals.
- A failure leaves an existing target database as it was.

```sh
driftgate data anonymize --dump out/shop-anon.sql
```

## driftgate data subset

```
driftgate data subset [--dump <file>] [--no-target] [--replace]
    [--format text|json]
```

Runs `data anonymize` on a sample with closed references: the rows that
`subset:` in `classify.yaml` chooses, and every row they reference.

| Flag | Meaning |
|---|---|
| `--dump <file>` | also write a dump to this file |
| `--no-target` | write only the dump (needs `--dump`) |
| `--replace` | drop an existing target database and write it again |
| `--format text\|json` | output format (default text) |

It needs a [`subset:`](../classify-yaml.md#subset) section in
`classify.yaml`. It stops where
[data anonymize](#driftgate-data-anonymize) stops.

```sh
driftgate data subset --no-target --dump out/shop-sample.sql
```

## driftgate data verify

```
driftgate data verify [--target <database>] [--dump <file>]
    [--format text|json]
```

Checks an output of `anonymize` against the source: the personal-data
detector over every value, and every original personal value looked
for.

| Flag | Meaning |
|---|---|
| `--target <database>` | the database to check (default `data.target`) |
| `--dump <file>` | the dump to check: one that `anonymize` or `subset` wrote (a file without its header line, such as a `mysqldump` or `pg_dump` output, is refused) |
| `--format text\|json` | output format (default text) |

Exit 1 when it finds personal data. `anonymize` and `subset` run the
same checks before they make their output.

```sh
driftgate data verify --dump out/shop-sample.sql
```

---
**Related:** [data](../driftgate-yaml.md#data) · [classify.yaml](../classify-yaml.md) · [Anonymized test data](../../guides/ci/test-data.md)
