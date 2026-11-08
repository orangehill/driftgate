# Upgrade the server: database, PHP, web server

> **Before you start:** have a baseline of the old stack ([upgrade a framework](framework-upgrade.md)). **Time:** 1 hour.

You move the same code to a new database server, PHP version or web
server, and prove that clients get the same answers.

## Upgrade the server in a step of its own

Never upgrade the server together with the code: then every drift comes
from the server. Typical drift:

- `item-order`: rows of a list without `ORDER BY` come in another order;
- `type`, `number-literal`: a driver returns numbers and decimals differently;
- `header-value` on every request: `server`, `x-powered-by`;
- error pages and limits of the web server or PHP: `client_max_body_size`, `upload_max_filesize`.

The same header on every request is one change. The line after the
comparison says how many requests drift in something else. It gives the
header rule that allows the new value
([one change in almost every request](../../reference/reports.md#one-change-in-almost-every-request)).

## Compare the old and the new stack

```sh
# on the old stack, right after a restore (driftgate seed): the pristine data
mysqldump --single-transaction shop > /tmp/shop.sql
driftgate capture old
# point target.base_url and state: at the new stack, and load /tmp/shop.sql there
driftgate seed --skip-commands              # the pristine copy on the new server
driftgate capture new --skip-fingerprint-check
driftgate compare new --against old --skip-fingerprint-check
```

Run the same code with the same data on both stacks, each at its own
address, and compare the captures.

- Take the dump from the pristine data, right after a restore or a seed, not after a capture or `prepare` changed it.
- Seed the new server with `--skip-commands`. Seed commands such as `migrate:fresh` would replace the data just loaded.
- A page that names its own address is compared with the other side's address taken as its own.
- The fingerprint is specific to the engine and version: MariaDB 10.3 and MySQL 8.4 write the same rows differently. So the check that both sides hold the same data is lost.
- A capture made with `--skip-fingerprint-check` is marked unreliable, and `compare` takes it only with the same flag.

A drift may then come from the data, and a green result does not show
that the data were the same. Prove that below.

## Prove the data is the same on both servers

Until DriftGate compares data across engines, prove it yourself, and say
how in the pull request.

### Compare the schema

```sql
SELECT table_name, column_name, ordinal_position, column_type, character_set_name,
       collation_name, is_nullable, column_default
FROM information_schema.COLUMNS WHERE table_schema = 'shop'
ORDER BY table_name, ordinal_position;
```

Run it on both servers, then `diff` the two outputs. A difference from
loading the dump into the other engine is part of the change. Examples
are a collation such as `uca1400` or `0900`, a JSON column written
normalised, and the sandbox line a newer `mariadb-dump` writes. Name it in the pull
request.

### Compare the rows

```sh
mysqldump --no-create-info --skip-extended-insert --skip-comments --skip-dump-date \
  --skip-triggers --hex-blob --set-gtid-purged=OFF shop <table> | LC_ALL=C sort
```

- Count the rows of every table with `SELECT COUNT(*)`, not `TABLE_ROWS`, which is an estimate.
- Dump the data of each table on both servers with the same client binary, one row per line, sorted the same way. Then `diff`.
- MySQL 8's client against MariaDB also needs `--column-statistics=0`.
- Sorting the lines, not the rows by `ORDER BY`, keeps the collations of the two servers out of it.
- With `SELECT` instead, set `time_zone = '+00:00'` on both sessions first. `FLOAT` and `DOUBLE` columns may print differently with the same stored value.

A difference found here is reviewed in the comparison and named in the
pull request. A comparison of two captures is not against a baseline, so
`accept` and `expect` do not apply to it.

## Data newer than the baseline

Checking the same code against newer data is a setup error: every
difference in the data shows as drift. Make both sides run on the same
data instead:

1. Load the same dump on both stacks, as above.
2. Or capture one server before and after the deploy, both times on the same copy:

```sh
# load the copy into the working database
driftgate seed --skip-commands                 # the copy becomes the pristine data
driftgate capture pre --skip-fingerprint-check
# deploy; load the same copy again, and run the deploy's migrations on it
driftgate seed --skip-commands                 # the migrated copy becomes the pristine data
driftgate capture post --skip-fingerprint-check
driftgate compare post --against pre --skip-fingerprint-check
```

With `state:`, every capture restores the pristine copy, so `seed
--skip-commands` after each load keeps the loaded copy, migrations
included. Without `state:`, as for a [live server](live-server.md),
leave out the two `seed` lines.

When neither is possible, `any_length` rules per path let lists grow or
shrink ([changing values](../review/changing-values.md)). A green result
there proves nothing about behaviour that depends on the data
([which level to use](../../concepts/how-it-works.md#which-level-to-use)).

## Check the settings of the stack

Settings such as PHP's `memory_limit` or `upload_max_filesize`, database
variables, and the web server's `client_max_body_size` change behaviour.
No response shows it until a limit is hit. A
[command step that prints the chosen settings](../side-checks.md#settings-of-the-stack)
makes a difference a drift you see in the comparison.

## Keep the new stack as the baseline

```sh
driftgate capture new --repeat 2 --skip-fingerprint-check --force
driftgate baselines rebase local --from new --reason "Moved to MySQL 8.4 and PHP 8.3" --ref APP-12 --allow-freeze
```

When the new stack is the one to keep, record the switch in the
baseline. Capture again with `--repeat 2`, so its runs must agree, and
`--force`, as the tag exists. The capture records the new server's
fingerprint, and the rebase logs the switch as one line of the
acceptance log ([baselines rebase](../../reference/commands/review.md#driftgate-baselines-rebase)).

Or make a new baseline on the new stack with
`driftgate run --baseline <name>`; every later command then needs
`--baseline <name>`. Then go on with the code steps.

---
**Next:** [Find the commit that introduced a drift](../review/find-the-commit.md): when a step spans many commits.
**Related:** [State: restoring the data](../../reference/state.md) · [Fingerprints](../../reference/state.md#fingerprints) · [driftgate compare](../../reference/commands/check.md#driftgate-compare)
