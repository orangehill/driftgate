# Check what the application does besides answering

> **Before you start:** have a catalog and a baseline ([set up your project](../../start/your-project.md)). **Time:** 15 minutes per check.

A gate that compares HTTP responses misses what else the application
does. That is the schema, files on disk, keys in Redis, the mail sent,
and a class only the production install lacks. These recipes put such checks into the same
baseline, report and acceptance log.

## Compare the output of a command

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "schema", "command": ["sh", "-c", "mysqldump --no-data --skip-comments shop | sed 's/ AUTO_INCREMENT=[0-9]*//'"],
   "output": true, "storage": "public"},
  {"id": "schema-sqlite", "command": ["sqlite3", "database/database.sqlite", "SELECT type, name, sql FROM sqlite_schema ORDER BY type, name"],
   "output": true, "storage": "public"},
  {"id": "typecheck", "command": ["npx", "tsc", "--noEmit", "--pretty", "false"],
   "output": true, "storage": "public"},
  {"id": "lint", "command": ["yarn", "lint"],
   "output": true, "output_ignore": ["(?m)^.*Done in [0-9.]+s\\.$"], "storage": "public"}
 ]
}
```

A [command step](../../reference/catalog-json.md#command-steps) with
`"output": true` compares what a program prints, without what
`output_ignore` matches. Its exit code is compared too.

- A schema dump shows a column type, index or default a step changed. Leave out what changes by itself: `AUTO_INCREMENT` counters, dated comments.
- A migration run on the working database is undone by the next restore: after a framework upgrade, seed again with `driftgate seed` to see it.
- On SQLite, order the schema (`ORDER BY type, name`). A restore re-creates a changed table at the end of `sqlite_schema`: the order changes, not the schema ([how a restore writes](../../reference/databases/sqlite.md#how-a-restore-writes-inside-the-file)).
- For a compiler or static checker, the warnings are the output. Remove only a timing line, anchored on both ends: a looser pattern such as `\d+s` also removes numbers that matter.
- Command steps run after the read-only requests, one at a time, on the restored state.
- Their output is scanned like a public body. Use `"storage": "private"` when it holds personal data.

## Check the settings of the stack

<!-- check: catalog -->
```json
{
 "version": 1,
 "requests": [
  {"id": "php-settings", "command": ["php", "-r", "foreach ([\"memory_limit\",\"upload_max_filesize\",\"post_max_size\",\"opcache.enable\"] as $k) echo $k, \"=\", ini_get($k), \"\\n\";"],
   "output": true, "storage": "public"},
  {"id": "db-settings", "command": ["mysql", "--defaults-extra-file=~/.config/driftgate/shop-db.cnf", "-N", "-e", "SHOW VARIABLES WHERE Variable_name IN ('sql_mode','character_set_server','collation_server','max_allowed_packet','time_zone')"],
   "output": true, "storage": "public"},
  {"id": "web-settings", "command": ["sh", "-c", "out=$(nginx -T 2>&1) || { printf '%s\\n' \"$out\" >&2; exit 1; }; printf '%s\\n' \"$out\" | grep -E '^[[:space:]]*(gzip|client_max_body_size)[[:space:];]' | LC_ALL=C sort"],
   "output": true, "storage": "public"}
 ]
}
```

When the server changes, its settings change behaviour that no response
shows until a limit is hit: a smaller `upload_max_filesize` or
`max_allowed_packet`, compression turned off. A step that prints the
settings that matter makes a changed one a drift.

- Name the settings one by one. Never print `php -i`, `phpinfo()` or the whole `SHOW VARIABLES`: they hold secrets, and values that differ on every machine.
- PHP's command line reads its own `php.ini`, and never limits a script's time. Run the step where the application runs; for the web server's PHP, use a small route that prints the same settings.
- The database step reads its server and user from an option file outside the repository, as its first option, open to its owner only. Or it runs the server's client in its container (`docker compose exec -T db mysql …`). Never a password in the command: the catalog is in git.
- `nginx -T` needs the rights to read the whole configuration. When it fails, the step fails with its message, instead of printing nothing. A setting no longer there is a line gone from the output.
- A setting whose value could be sensitive, such as a path with a user name, goes into a step with `"storage": "private"`.

## Check files on disk and Redis

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

A `state:` component without a driver is restored and fingerprinted by
commands ([how](../../reference/state.md#restore-anything-else-with-commands)).
Every capture then starts from the same files and keys. A request that
writes them shows the side effect `<component>/<component>`. The
fingerprint is one item: the SHA-256 of the command's output.

- The side effect says only that the component changed. To see which file, put a command step that prints the hashes after the request, in a sequence. The first line that differs names the file.
- List only the folders the application writes to.
- Give Redis a database number of its own (`-n 1`), so the restore cannot flush another application's keys. `DUMP` covers the value and its type.
- Keep keys that hold a time, or that every request sets (sessions, rate limits), out of the fingerprint. Use another database, or `--pattern` on `--scan`.

## Check the mail the application sends

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
   "body": {"email": "admin@shop.example"}, "mutates": true, "storage": "private"},
  {"id": "forgot-password-mail", "command": ["cat", "storage/logs/mail.log"], "output": true,
   "output_ignore": ["Message-ID: <[^>]*>", "Date: .*", "boundary=\"?[\\w.=-]+\"?", "--[\\w.=-]{20,}", "\\[\\d{4}-\\d{2}-\\d{2}[^\\]]*\\]"],
   "storage": "private"}
 ],
 "sequences": [
  {"id": "password-mail", "restore": "before", "requests": ["forgot-password", "forgot-password-mail"]}
 ]
}
```

Write mail to a file instead of sending it, and compare the file with a
command step in the sequence that sends it.

- Laravel: `MAIL_MAILER=log` with `MAIL_LOG_CHANNEL=mail`, a `single` log channel writing `storage/logs/mail.log`. Symfony: the `null://null` transport and a listener that writes the message. Rails: `delivery_method = :file`. Django: the `filebased` email backend.
- The guard stops the capture when mail would go out to people. The `mail` component empties the file on every restore.
- `output_ignore` removes what differs on every send: the message ID, dates, MIME boundaries, the log line's time. Remove a reset link's token the same way (`token=[\w-]+`).
- Mail holds names and addresses, so the step's output is private.

## Check the production install

```sh
git worktree add ../shop-prod HEAD && cd ../shop-prod
composer install --no-dev --optimize-autoloader   # npm ci --omit=dev, bundle install --without development test …
cp ../shop/.env .env                              # then its own DB_DATABASE and APP_URL (another port)
php artisan config:cache && php artisan route:cache
driftgate setup && driftgate run                  # this install against the main worktree's baseline
```

A development install has packages production lacks, and the application
can depend on one unnoticed. Capture a copy installed as production does,
in a [worktree](../workflow/worktrees.md) of the same commit.

- Setup there takes the main worktree's project and baseline, and the worktree's own database and address. Absolute URLs are compared through both [origins](../../reference/comparison.md#your-own-urls-on-another-port).
- `seed:` commands that need development packages do not run there. Seed from a dump, or take the data as it is: `driftgate seed --skip-commands`.
- To make the pristine copy always from the production install, put the install commands first in the worktree's `seed:`.
- A second service under `targets:` does not fit: its requests would need other IDs and a baseline of their own.

---
**Next:** [Find the commit that introduced a drift](../review/find-the-commit.md): when a check drifts and the cause is not obvious.
**Related:** [command steps](../../reference/catalog-json.md#command-steps) · [State: restoring the data](../../reference/state.md) · [Upgrade the server](../workflow/server-upgrade.md)
