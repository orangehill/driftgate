# classify.yaml

`classify.yaml` holds a rule for every column of every table of the data
module's source database. The [data commands](commands/data.md) read it.

The file is `.driftgate/classify.yaml`, or `driftgate/classify.yaml` with
`baselines: git` ([where the project's files are](cli.md#where-the-projects-files-are)).

- `driftgate data classify --write` proposes rules, marked `proposed: true`. A person checks each, and removes the mark.
- A column without a rule, a rule still proposed, or a rule for a table or column that no longer exists stops `anonymize` and `subset`.

## An example

<!-- check: classify -->
```yaml
version: 1
tables:
  users:
    columns:
      id: keep
      name: universal
      email: email
      password: password
      remember_token: token
      bio: free-text
      settings:
        kind: personal
        strategy: json
        paths:
          $.phone: universal
          $.theme: keep
        other: free-text
      avatar: file
      created_at: keep
  sessions:
    action: empty
    reason: Sessions of real users; the application makes new ones.
  cache:
    columns:
      key: keep
      value:
        kind: serialized-php
        action: empty-table
  telescope_entries:
    action: skip
    reason: Development tool data.
schema:
  view vip_customers: [email]
  table users:
    kinds: [email]
    reason: The column default is the shared support address.
```

## Table rules

<!-- check: classify -->
```yaml
version: 1
tables:
  sessions:
    action: empty
    reason: Sessions of real users; the application makes new ones.
```

| Key | Meaning |
|---|---|
| `action` | `copy` (default), `empty` or `skip` (below) |
| `columns` | column name → rule |
| `reason` | why |
| `proposed` | written by `data classify`; remove it once checked |

- `copy`: rows are copied, each column by its rule.
- `empty`: the structure without rows.
- `skip`: the table is left out.

## Column rules

<!-- check: classify -->
```yaml
version: 1
tables:
  users:
    columns:
      id: keep
      email: email
      settings:
        kind: personal
        strategy: json
        paths:
          $.phone: universal
        other: free-text
```

A column rule is one word for the plain cases, or a mapping. The word is
`keep`, `file`, `serialized-php`, or a strategy name, which means
`kind: personal`.

| Key | Meaning |
|---|---|
| `kind` | `keep`, `personal`, `serialized-php` or `file` (below) |
| `strategy` | for `personal`: one of the [strategies](#strategies) |
| `paths` | for `json`: a JSON path glob (starting with `$`) → strategy or `keep` |
| `other` | for `json`: strings under no path, `free-text` (default) or `keep` |
| `action` | for `serialized-php`: `empty-table` (default) or `null` (below) |
| `reason`, `proposed` | as for tables |

- `action: empty-table`, or no `action`, leaves all rows of the table out of the copy.
- `action: null` sets the column to NULL and keeps the rows. A bare `null` and `"null"` mean the same.
- `action:` with nothing after it, or `action: ""`, is refused: only a missing `action` means the default.

The kinds:

- `keep`: copied as is;
- `personal`: replaced by a strategy;
- `serialized-php`: PHP `serialize()` output;
- `file`: a path to an uploaded file; the file gets a placeholder.

## Strategies

| Strategy | Replacement |
|---|---|
| `universal` | a letter by a letter of the same script, a digit by a digit; length and punctuation kept |
| `email` | an address at an `.invalid` domain |
| `password` | a hash of the known test password `driftgate` |
| `token` | a token of the same length |
| `free-text`, `html` | only the personal parts of the text change (names, e-mails, phone numbers) |
| `json` | by path, as `paths` and `other` say |
| `null`, `empty` | `NULL` or an empty value |

- `universal` knows the Latin script with Serbian diacritics, and Cyrillic.
- The same value gets the same stand-in everywhere, deterministic under the key, so references and uniqueness hold.
- A bare `null` in YAML is the strategy `null`.

## Schema findings

`schema:` confirms what the detector finds in schema objects: comments,
defaults, views, triggers, functions and checks. These are copied as they
are.

- The key is the object as `data classify` names it: `view vip`, `table users`.
- The value is a list of kinds (`email`, `phone`, `jmbg`, `card`, `iban`), or a mapping with `kinds`, `reason` and `proposed`.
- Any other finding stops the run.

## subset

<!-- check: classify -->
```yaml
version: 1
tables:
  users: { columns: { id: keep, email: email } }
subset:
  nullable: pull
  tables:
    countries: all
    audit_log: none
    users:
      rows: 200
      order: latest
    orders:
      follow: [users]
      per_parent: 5
    news:
      where: "published = 1"
      percent: 10
  references:
    - from: comments(commentable_id)
      to: posts(id)
      when: { commentable_type: 'App\Models\Post' }
    - from: orders(coupon_id)
      nullable: "null"
```

`data subset` copies a sample: the rows `subset:` chooses as roots, the
rows that follow them, and every row they reference, transitively. So
the copy has no dangling reference.

| Key | Meaning |
|---|---|
| `nullable` | for a nullable reference to a row outside the sample: `pull` (default) or `null` (below) |
| `tables.<t>` | `all`, `none`, or a mapping. A table not listed has only the rows other kept rows reference |
| `rows` | at most this many roots |
| `percent` | roots: this share of the rows (above 0, at most 100), chosen by a hash of the primary key |
| `where` | roots: rows matching this SQL condition |
| `order` | `latest` (default, highest primary key first) or `first` |
| `follow` | parent tables: rows of this table that reference kept rows of those tables are kept too |
| `per_parent` | with `follow`: at most this many rows per parent (by `order`); 0 is all |
| `references` | references the database does not declare, written `table(column, …)` (below) |

- `nullable: pull` takes the row in; `nullable: null` sets the reference to NULL.
- A table rule needs `rows`, `percent`, `where` or `follow`, or is `all` or `none`.
- `references` are for applications without foreign keys, and for polymorphic columns.
- With `to` omitted, `from` is a foreign key of the database whose `nullable` rule changes.
- `when` limits a declared reference to rows whose columns have these values.

---
**Related:** [Commands: the data module](commands/data.md) · [data](driftgate-yaml.md#data) · [Anonymized test data](../guides/test-data.md)
