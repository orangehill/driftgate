# Anonymized test data for CI

Private response bodies never leave the machine that captured them, so CI
can compare only public responses. The data module (`driftgate data`) makes
a copy of a database in which personal values are replaced, so that the
application's responses on it hold no real personal data and can be
captured as public.

It is pseudonymisation (GDPR / Serbian ZZPL), not anonymisation: whoever
has the key and the original data can link the two. Run it locally or on
your own runner, never on a shared CI runner with production data.

The module is tested on MySQL, MariaDB and PostgreSQL, and supports only
those for now: `data.driver` is `mysql` or `postgres`. A project on SQLite
or SQL Server can use its state with every other command, not the data
module yet.

## 1. Source and target

The source is a copy of production data on a server you control; it is only
read. The target is a database the module drops and writes again.

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

Values are replaced under the project key (or `data.key`). The same input,
key and rules give a byte-identical dump.

## 2. Classify every column

```sh
driftgate data classify            # exit 1: columns without a rule
driftgate data classify --write    # writes proposals to .driftgate/classify.yaml (driftgate/ with baselines: git)
```

`classify` proposes a rule for every column by its name, type and a sample
of its content (sampled values are never written), and a table action for
tables such as sessions. Every proposal is marked `proposed: true`: check
it, change it if needed, and remove the mark.

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

A column without a rule (new after a migration, say), a proposal nobody
confirmed, or a rule for a column that no longer exists stops `anonymize`
and `subset` with exit 2. Personal-looking literals in schema objects
(comments, defaults, views, triggers, functions) must be confirmed under
`schema:` too. All rules: [configuration, classify.yaml](../configuration.md#classifyyaml).

## 3. Make the copy

The whole database:

```sh
driftgate data anonymize --dump out/shop-anon.sql
```

Or a sample with closed references (the rows `subset:` chooses, and every
row they reference, also for applications without foreign keys):

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

```sh
driftgate data subset --no-target --dump out/shop-sample.sql
```

Before the output is made, two independent checks run over it: the
personal-data detector over every value and the schema, and a search for
every original personal value (regardless of case, script, diacritics,
encodings and case endings). The output is made only when both find
nothing. A database target is written under a temporary name and renamed
after verification, so a failure leaves the previous target as it was.
`driftgate data verify --dump out/shop-sample.sql` runs the same checks
again on demand (it needs the source).

## 4. Seed from the copy

Use the dump as the seed of the state database, locally and in CI:

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

The dump is pseudonymised data: keep it where your data policy allows (not
necessarily in git) and give CI access to it from there.

## 5. Make the responses public

On the anonymized copy, mark the requests public so that their bodies are
committed and CI can compare them:

<!-- check: config-fragment -->
```yaml
storage:
  default: public
pii:
  allow: ['[^@\s]+@[^@\s]+\.invalid']   # stand-in e-mails are at .invalid domains
```

The detector still scans every public body. Stand-ins keep the shape of
the originals, so a stand-in phone number still looks like a phone number:
the capture then stores that body as private, names the request and the
kind with a masked value, and exits 2. Decide per finding: keep the request
private, or allow the value pattern in `pii.allow` once you are sure the
data comes from the anonymized copy. Make the baseline locally on the
anonymized data (`driftgate run`) and commit it.

## 6. In CI

Load the same dump in the seed, and run without `--skip-private`: with
public bodies there is nothing to skip, and the whole catalog is compared.
The state fingerprint check stops the run (exit 3) if the data differ from
the data the baseline was captured from, for example after a new dump.
