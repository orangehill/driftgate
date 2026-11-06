# Side checks in the same gate

A gate that compares HTTP responses misses what an application does
besides answering: the schema its migrations make, files it leaves on
disk, keys in Redis, the mail it sends, or a class that only the
production install lacks. These recipes move such checks into DriftGate,
so they end up in the same baseline, report and acceptance log as the
responses.

## Output of a command: a schema dump, a compiler

A [command step](../reference/catalog-json.md#command-steps) with `"output": true`
compares what a program prints, after the `output_ignore` patterns are
removed. Its exit code is compared too, so a command that starts failing
is a drift.

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "schema", "command": ["sh", "-c", "mysqldump --no-data --skip-comments shop | sed 's/ AUTO_INCREMENT=[0-9]*//'"],
   "output": true, "storage": "public"},
  {"id": "typecheck", "command": ["npx", "tsc", "--noEmit", "--pretty", "false"],
   "output": true, "storage": "public"},
  {"id": "lint", "command": ["yarn", "lint"],
   "output": true, "output_ignore": ["(?m)^.*Done in [0-9.]+s\\.$"], "storage": "public"}
 ]
}
```

- A schema dump of the restored database shows a column type, index or
  default that a step changed (a framework upgrade that makes other DDL
  from the same migrations shows once the data is seeded again with
  `driftgate seed`). Leave out what changes by itself (`AUTO_INCREMENT`
  counters, comments with dates).
- A compiler or static checker: its warnings are the output. When a tool
  prints how long it took (yarn: `Done in 2.31s.`), remove only that
  line, anchored on both ends (`(?m)^.*Done in [0-9.]+s\.$`; the
  patterns apply to the whole output): a looser pattern such as `\d+s`
  also removes numbers in the messages that matter.
- Command steps run after the read-only requests, one at a time, on the
  restored state. Their output is public only when it holds no personal
  data (it is scanned like a public body); otherwise use `"storage":
  "private"`.

## Settings of the stack

When the server changes (PHP, the database, the web server), its settings
change behaviour that no response shows until a limit is hit: an upload
over a smaller `upload_max_filesize`, a request over a smaller
`max_allowed_packet`, compression turned off. A command step that prints
only the settings that matter makes a changed one a drift you see in the
comparison; against a baseline it is fixed, accepted (`driftgate accept`)
or expected (`driftgate expect --ref <ticket>` in freeze mode).

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "php-settings", "command": ["php", "-r", "foreach ([\"memory_limit\",\"upload_max_filesize\",\"post_max_size\",\"opcache.enable\"] as $k) echo $k, \"=\", ini_get($k), \"\\n\";"],
   "output": true, "storage": "public"},
  {"id": "db-settings", "command": ["mysql", "--defaults-extra-file=/home/dev/.config/driftgate/shop-db.cnf", "-N", "-e", "SHOW VARIABLES WHERE Variable_name IN ('sql_mode','character_set_server','collation_server','max_allowed_packet','time_zone')"],
   "output": true, "storage": "public"},
  {"id": "web-settings", "command": ["sh", "-c", "out=$(nginx -T 2>&1) || { printf '%s\\n' \"$out\" >&2; exit 1; }; printf '%s\\n' \"$out\" | grep -E '^[[:space:]]*(gzip|client_max_body_size)[[:space:];]' | LC_ALL=C sort"],
   "output": true, "storage": "public"}
 ]
}
```

- Name the settings one by one. Never print `php -i`, `phpinfo()` or the
  whole `SHOW VARIABLES`: they hold secrets (passwords, keys in the
  environment) and values that differ on every machine, which only add
  noise.
- PHP's command line reads its own `php.ini` and never limits the time a
  script runs (`max_execution_time` is always `0` there): run the step
  where the application runs (in its container), and for the settings of
  the web server's PHP (`max_execution_time` among them) use a small
  route that prints the same settings, as a catalog request.
- The database step names its server and user in an option file outside
  the repository, as its first option (`--defaults-extra-file`, with
  `[client]` `host`, `port`, `user`, `password`, open to its owner only),
  or runs the server's own client in its container (`docker compose exec
  -T db mysql …`). Never a password in the command: `catalog.json` is in
  the repository.
- `nginx -T` reads the whole configuration (it needs the rights to read
  it): when it fails, the step fails with its message instead of printing
  nothing, and a setting no longer there is a line gone from the output.
- A setting whose value could be sensitive (a path with a user name, a
  host) goes into a step with `"storage": "private"`.

## Files on disk and Redis

A `state:` component without a driver is restored and fingerprinted by
commands ([state](../reference/state.md)). Every capture then starts
from the same files and keys, and a request that writes to them shows a
side effect `<component>/<component>` (the fingerprint is one item: the
SHA-256 of the command's output).

<!-- check: config-fragment -->
```yaml
state:
  files:
    restore: ["rsync", "-a", "--delete", "storage-pristine/app/", "storage/app/"]
    fingerprint: ["sh", "-c", "cd storage/app && find . -type f -print0 | sort -z | xargs -0 shasum -a 256"]
  redis:
    restore: ["sh", "-c", "redis-cli -n 1 FLUSHDB >/dev/null && redis-cli -n 1 --pipe < driftgate/redis-seed.txt"]
    fingerprint: ["sh", "-c", "redis-cli -n 1 --scan | sort | while read -r k; do printf '%s ' \"$k\"; redis-cli -n 1 --raw DUMP \"$k\" | shasum -a 256; done"]
```

- The file fingerprint names every file with the SHA-256 of its content:
  a new, removed or changed file is a side effect of the request that
  wrote it. The side effect says only that the component changed; to see
  which file and how, put a command step that prints the hashes after
  the request in a sequence (`"command": ["sh", "-c", "cd storage/app &&
  find . -type f -print0 | sort -z | xargs -0 shasum -a 256"], "output":
  true`): its output is compared, and the first line that differs names
  the file. List only the folders the application writes to.
- Redis: use a database number of its own (`-n 1`) so the restore cannot
  flush another application's keys. `DUMP` covers the value and its type;
  keys that hold a time (a cache entry with the time it was written) or
  that the application sets on every request (sessions, rate limits) do
  not belong in the fingerprint: keep them in another database, or leave
  them out of the `--scan` with `--pattern`.
- A fingerprint must give the same output after every restore; a capture
  checks it.

## Mail

Mail is a response too, sent elsewhere. Write it to a file instead of
sending it, and compare the file with a command step in the sequence that
sends it.

<!-- check: config-fragment -->
```yaml
state:
  mail:
    restore: ["sh", "-c", ": > storage/logs/mail.log"]
guards:
  - name: mail goes to a log
    command: ["sh", "-c", "grep -q '^MAIL_MAILER=log' .env && grep -q '^MAIL_LOG_CHANNEL=mail' .env"]
```

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "forgot-password", "method": "POST", "path": "/api/password/forgot",
   "body": {"email": "admin@demo.test"}, "mutates": true, "storage": "private"},
  {"id": "forgot-password-mail", "command": ["cat", "storage/logs/mail.log"], "output": true,
   "output_ignore": ["Message-ID: <[^>]*>", "Date: .*", "boundary=\"?[\\w.=-]+\"?", "--[\\w.=-]{20,}", "\\[\\d{4}-\\d{2}-\\d{2}[^\\]]*\\]"],
   "storage": "private"}
 ],
 "sequences": [
  {"id": "password-mail", "restore": "before", "requests": ["forgot-password", "forgot-password-mail"]}
 ]
}
```

- Laravel: `MAIL_MAILER=log` with `MAIL_LOG_CHANNEL=mail`, and a log
  channel `mail` that writes `storage/logs/mail.log` (a `single` driver
  with that path). Other frameworks have the same: Symfony's
  `null://null` transport with a mailer event listener that writes the
  message, Rails' `delivery_method = :file`, Django's
  `EMAIL_BACKEND = "django.core.mail.backends.filebased.EmailBackend"`.
- The guard stops the capture before the first request that changes data
  when mail would go out to people.
- The `mail` component empties the file on every restore, so the step
  shows only the mail of its sequence. `output_ignore` removes what differs
  on every send: the message ID, dates, MIME boundaries and the log line's
  time. A token in a reset link changes on every send too: remove it the
  same way (`token=[\w-]+`).
- Mail holds names and addresses: the step's output is private.

## The production install

A development install has packages production lacks (debug bars, test
helpers, faker in seeders), and an application can depend on one of them
without anyone noticing. Capture a copy installed as production installs
it, and compare it with the same baseline.

With `baselines: local` that copy is a git worktree of the same commit:

```sh
git worktree add ../shop-prod HEAD
cd ../shop-prod
composer install --no-dev --optimize-autoloader   # npm ci --omit=dev, bundle install --without development test …
cp ../shop/.env .env                              # then: its own database (DB_DATABASE=shop_prod) and APP_URL (another port)
php artisan config:cache && php artisan route:cache
driftgate setup                                   # takes the main worktree's project, baseline and key
driftgate run                                     # this install against the main worktree's baseline
```

- `setup` in a worktree takes the main worktree's configuration and
  baseline, and offers to make the worktree's own database (from its
  `.env`) `working:` and `pristine:` ([setup](../reference/commands/project.md#driftgate-setup)); the
  address its `APP_URL` gives becomes `base_url`. Responses that write
  absolute URLs are compared through both origins
  ([target and targets](../reference/driftgate-yaml.md#target-and-targets)).
- `seed:` commands that need development packages (a seeder that uses a
  fake-data library) do not run in this install. Seed it from a dump
  instead, or take the data as it is: `driftgate seed --skip-commands`.
  Put the install commands first in the worktree's `seed:` when the
  pristine copy should always be made from the production install.
- A second service under `targets:` does not fit: catalog requests belong
  to one service each, so the same requests would need other IDs and a
  baseline of their own.
