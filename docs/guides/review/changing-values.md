# Allow values that change by themselves

> **Before you start:** [read a drift report](read-the-report.md). **Time:** 15 minutes.

An allowlist rule lets one value that changes by itself, such as a time
or a token, change within a format, with a reason. All else is compared.

## Find the values that change

```sh
driftgate capture check --repeat 2
```

It captures twice on the same data, and proposes an entry for every value
that differed; the first `run` does too. Check each format, write the
reason, and add it to `.driftgate/allowlist.yaml`.

## Allow a changing header

<!-- check: allowlist -->
```yaml
version: 1
headers:
  - name: set-cookie
    format: 'session=[0-9a-f]{40}; Path=/; HttpOnly'
    reason: A new session id on every login.
```

The header may change, but only to a value the whole format matches.
A proposed `set-cookie` format has one alternative per cookie name. An
`expires=` date in it is always a date format, never the date seen.
`ignore_case: true` instead lets a value change only in letter case
([header rules](../../reference/allowlist-yaml.md#header-rules)).

## Allow a changing value in a body

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: $.meta.generated_at
    format: '\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z'
    reason: Time the response was built.
```

`path` is a glob over JSON paths: `$.items[*].updated_at` covers every
item. A value equal to the baseline's is never a drift, whatever the
format ([body rules](../../reference/allowlist-yaml.md#body-rules)).

## Allow only part of a value

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: html*
    format: '/build/assets/[\w-]+\.js'
    parts: true
    reason: Asset URLs carry a build hash; the rest of the page is still compared.
```

With `parts: true`, only the parts the format matches may change, such
as a build hash inside a URL. The rest of the value is still compared.

## Let a list grow or shrink

<!-- check: allowlist -->
```yaml
version: 1
body:
  - path: $.items
    any_length: true
    reason: The feed shows entries of the last hour.
```

`any_length` lets the array change length; a price in an item is still compared.

## Rows in another order: pair by key or allow any order

<!-- check: allowlist -->
```yaml
version: 1
arrays:
  - path: $.data
    key: id
    reason: Users are listed newest first; a new user is one new item.
```

After a database or ORM change, rows can come in another order: an
`item-order` drift. The best fix is an `ORDER BY` in the code.

- `key` pairs the items by an id. A new, missing or changed item is still reported, and so is a changed order.
- `any_order: true` allows the order, where it does not matter to clients ([array rules](../../reference/allowlist-yaml.md#array-rules)).

## Allow a value in a page or a spreadsheet

<!-- check: allowlist -->
```yaml
version: 1
body:
  - request: get-api-users-export
    path: xlsx:Users!A1
    format: 'Exported \d{2}\.\d{2}\.\d{4} \d{2}:\d{2}'
    reason: The export writes its own time in the header cell.
```

A path also reaches into a page (`html>body>p@class`), a cell (`xlsx:Users!A1`,
`csv!A1`), or a whole body that is not JSON (`$raw`; [body rules](../../reference/allowlist-yaml.md#body-rules)).

## Find rules that cannot apply

```sh
driftgate allowlist check
```

It finds rules that reach nothing, apply to no request, are too broad, or
never decide. Each report also lists the rules that changed nothing
([allowlist check](../../reference/commands/review.md#driftgate-allowlist-check)).

---
**Next:** [Fix, accept or expect a drift](fix-accept-or-expect.md): what to do with what is left.
**Related:** [allowlist.yaml](../../reference/allowlist-yaml.md) · [Rows in another order](../../reference/comparison.md#rows-in-another-order) · [Drift kinds](../../reference/drift-kinds.md)
