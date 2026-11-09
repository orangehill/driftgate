# Make anonymized test data for CI

> **Before you start:** have a copy of production data on a server you control. **Time:** 1 hour.

Private bodies never leave the machine that captured them, so CI compares
only public responses. The data module makes a copy of a database with
personal values replaced, so the responses on it can be public.

It is pseudonymisation, not anonymisation: run it locally or on your own
runner ([the data module](../../security.md#the-data-module)). It supports
MySQL, MariaDB and PostgreSQL, not yet SQLite or SQL Server.

## Configure the source and the target

<!-- check: config-fragment -->
```yaml
data:
  driver: mysql                       # MariaDB too; or postgres
  source: { dsn: { env: DG_SRC_DSN }, database: shop_prod_copy }
  target: { database: shop_anon }     # same server unless target.dsn is set
  files:                              # optional: uploaded files get placeholders
    source: ~/Data/shop-storage
    target: ~/Data/shop-storage-anon
```

The source is a copy of production data, and is only read. The target is
a database the module drops and writes again. Values are replaced under
the project key, or `data.key`: the same input, key and rules give a
byte-identical dump ([data](../../reference/driftgate-yaml.md#data)).

## Classify every column

```sh
driftgate data classify            # exit 1: columns without a rule
driftgate data classify --write    # writes the proposals to .driftgate/classify.yaml
```

`classify` proposes a rule per column from its name, type and a sample of
its content, and a table action for tables such as sessions. Sampled
values are never written. Check each proposal, and remove its
`proposed: true` mark.

<!-- check: classify -->
```yaml
version: 1
tables:
  users:
    columns:
      id: keep
      name: universal
      email: email
      phone: universal
      password: password
      remember_token: token
      about: free-text
      created_at: keep
  sessions:
    action: empty
    reason: Sessions of real users.
```

A column without a rule, such as one new after a migration, stops
`anonymize` and `subset` with exit 2. So does a proposal nobody confirmed,
or a rule for a column that no longer exists. Personal-looking literals
in schema objects (comments, defaults, views, triggers, functions) must be
confirmed under `schema:` too ([classify.yaml](../../reference/classify-yaml.md)).

## Make the copy

```sh
driftgate data anonymize --dump out/shop-anon.sql             # the whole database
driftgate data subset --no-target --dump out/shop-sample.sql   # a sample
```

A sample has closed references: the rows `subset:` chooses, and every
row they reference, also for applications without foreign keys.

<!-- check: classify -->
```yaml
version: 1
tables:
  users: { columns: { id: keep, email: email } }
subset:
  tables:
    countries: all
    users: { rows: 200 }
    orders: { follow: [users], per_parent: 5 }
```

The output is made only when two independent checks find nothing:

- the personal-data detector over every value and the schema;
- a search for every original personal value, regardless of case, script, diacritics, encodings and case endings.

A database target is written under a temporary name and renamed after
the checks, so a failure leaves the previous target as it was.
`driftgate data verify --dump out/shop-sample.sql` runs the same checks
again; it needs the source ([data commands](../../reference/commands/data.md)).

## Seed from the copy

<!-- check: config-fragment -->
```yaml
state:
  db:
    driver: mysql
    dsn: { env: DG_DB_DSN }
    working: shop
seed:
  - ["sh", "-c", "mysql shop < \"$DG_SEED_DUMP\""]
  - ["php", "artisan", "migrate", "--force"]
```

Seed the state database from the dump, locally and in CI. Keep the dump
where your data policy allows, not necessarily in git; CI reads it there.

## Make the responses public

<!-- check: config-fragment -->
```yaml
storage:
  default: public
pii:
  allow: ['[^@\s]+@[^@\s]+\.invalid']   # stand-in e-mails are at .invalid domains
```

On the anonymized copy, mark the requests public, so that their bodies
are committed and CI can compare them.

- The detector still scans every public body. Stand-ins keep the shape of the originals, so a stand-in phone number still looks like a phone number.
- The capture then stores that body as private, names the request and the kind with a masked value, and exits 2.
- Decide per finding: keep the request private, or allow the value pattern in `pii.allow` once you are sure the data comes from the copy.

Make the baseline locally on the copy (`driftgate run`), and commit it.

## Run on the copy in CI

```sh
driftgate run          # in CI too: no --skip-private, every body is public
```

Load the same dump in the seed, and run without `--skip-private`. With
public bodies there is nothing to skip: the whole catalog is compared. The fingerprint check stops the run (exit 3) if the data differ from
the baseline's, such as after a new dump
([the GitHub Action](github-actions.md)).

---
**Next:** [Exit codes](../../reference/cli.md#exit-codes): what a CI job's result means.
**Related:** [classify.yaml](../../reference/classify-yaml.md) · [data](../../reference/driftgate-yaml.md#data) · [The data module](../../security.md#the-data-module)
