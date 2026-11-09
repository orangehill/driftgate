# Troubleshooting

Find the symptom, read its cause, and follow the link. Messages are
quoted as DriftGate prints them, so you can search this page for a word
of yours.

## The first run

### The first run makes no baseline: the two captures differ
A value changes by itself between captures. Add the allowlist entries the run proposes, with a reason, and run again ([changing values](guides/review/changing-values.md#find-the-values-that-change)).

### "the application does not answer at …"
The application is not running at `base_url`. Start it, or set `base_url` to where it answers, then run again ([when setup stops](start/your-project.md#when-setup-stops)).

### "the catalog has no requests"
No routes were found. Configure `discover:`, or write catalog entries by hand ([discover](reference/driftgate-yaml.md#discover)).

### "no token for identity …"
A catalog request names an identity without a token. Give it one from the environment, 1Password, a command or a file ([logins](guides/catalog/logins.md)).

### A read-only request changed data
A `GET` wrote something. Mark it `mutates: true` ([read-only requests](guides/catalog/hand-written-requests.md#mark-a-request-that-only-reads); [details](reference/capture.md#a-read-only-request-that-changed-data)).

## Exit code 3: the result is incomplete or the environment failed

### "private body not on this machine"
The private bodies are in another machine's local store. Pass `--skip-private`, or run the check where they are ([private bodies in CI](guides/ci/github-actions.md#private-bodies-in-ci)).

### "the working database was changed outside DriftGate"
A migration ran on the working database since the seed, and the restore would undo it. Add the migrate command to `prepare:`, or run `driftgate seed` ([a migration run outside DriftGate](reference/capture.md#a-migration-run-outside-driftgate)).

### The restored state differs from the baseline's state
The pristine copy holds other data than the baseline was made from. Seed that data again, make a new baseline, or rebase the baseline on a capture of the new data ([when the state does not match](reference/capture.md#when-the-state-does-not-match-the-baseline)).

### "restore of … left … items different from the fingerprint"
The restored data does not match the pristine copy's fingerprint. The message says which cause it found:

- The pristine copy changed since the seed, or the application wrote during the seed: run `driftgate seed` again while the application does not write.
- The application wrote during the restore: stop its background writers, such as queue workers or schedulers, during the capture.
- On MySQL 8, a pristine copy from up to v1.0.2 named a column's collation: re-create the working database, then seed ([column collations](reference/upgrading-driftgate.md#column-collations-after-v102-on-mysql-8)).

### A comparison compared nothing
`--only` matched no request, or the baseline has none of the catalog's requests. Check the glob and the baseline ([the verdict line](reference/reports.md#the-verdict-line)).

### A guard failed
A guard's check did not succeed, such as mail not going to a log. Fix the setting it checks ([guards](reference/driftgate-yaml.md#seed-prepare-guards)).

### "baseline … changed outside driftgate accept …; run driftgate verify"
Every command that reads the baseline (`capture`, `compare`, `run`, `accept`, `expect`, `rebase`, `bisect`) refuses one whose files no longer match its manifest. Run `driftgate verify`: it names the hand edit, the deleted file or the edited log line, and exits 1. Then restore the baseline from git ([driftgate verify](reference/commands/review.md#driftgate-verify)).

### A command stops: a baseline is in `.tmp-old-*`
An `accept` or a first `run` was killed while it replaced the baseline. Put `.tmp-old-*/baseline` back, or restore it from git ([Ctrl-C](reference/cli.md#what-happens-when-you-press-ctrl-c)).

### No encryption key
Private bodies need the project key. Run `driftgate key create`, or set `DRIFTGATE_KEY` ([the project key](concepts/project-layout.md#the-project-key)).

### "state … has no pristine copy yet"
Run `driftgate seed`; `run` seeds by itself ([driftgate seed](reference/commands/state.md#driftgate-seed)).

## Exit code 2: configuration

### "git does not ignore …"
`.driftgate/` holds the key, captures and private bodies, so git must ignore it. Add the line the message names to `.gitignore` ([baselines](reference/driftgate-yaml.md#baselines)).

### accept is refused
`mode: freeze`, a missing `--reason`, part of a request's drifts, or a stale comparison ([what accept refuses](reference/commands/review.md#what-accept-refuses)).

### A switch of layouts stopped halfway (`.driftgate/switching`)
Run `driftgate baselines <mode>` with the same mode again; it finishes the switch ([a switch that stopped halfway](reference/commands/project.md#a-switch-that-stopped-halfway)).

### `--read-only` is refused because `prepare:` is configured
`prepare` runs after every restore and may write. Leave it out for a server that must not change ([live server](guides/workflow/live-server.md#configure-a-server-that-must-not-change)).

### Personal data in a public body
The detector found personal data in a response stored as public. The body is kept private; mark the request `storage: private`, or allow the value ([the personal-data detector](security.md#the-personal-data-detector)).

### The setting names another database than `working`
A restore would reset a database the application does not run on. Make the setting and `working:` agree ([database settings](reference/database-settings.md#read-the-dsn-from-the-projects-own-settings)).

### setup stops in a worktree
The worktree's settings name the main worktree's database, or none. Give the worktree a database of its own ([worktrees](guides/workflow/worktrees.md#give-the-worktree-its-own-database-and-address)).

## Exit code 1 without a regression

### coverage fails: routes not covered, or failed in the baseline
A route has no request that succeeds. Add one, exclude the route, or acknowledge a known failure ([coverage](guides/catalog/coverage.md)).

### `generate --check` fails in CI
The committed catalog is not what `generate` writes. Run `driftgate generate --write` and commit ([keep the catalog current](guides/catalog/coverage.md#keep-the-catalog-current-in-ci)).

### verify reports changes after a checkout on Windows
`verify` exits 1, naming files that differ only in their line endings. Git checked the baseline files out with CRLF line endings. Check them out again with the two git commands under [line endings on Windows](reference/commands/review.md#line-endings-on-windows).

## Drift you did not expect

### The same rows in another order (`item-order`)
Another database server or ORM returns a list without `ORDER BY` in another order ([rows in another order](guides/review/changing-values.md#rows-in-another-order-pair-by-key-or-allow-any-order)).

### Many `array-length` and null or object drifts after loading another dump
The two sides ran on different data. Capture both sides from the same dump ([data newer than the baseline](guides/workflow/server-upgrade.md#data-newer-than-the-baseline)).

### The `server` header drifts on every request
Another server stack answers. The report's line gives the header rule for the new value ([one change in almost every request](reference/reports.md#one-change-in-almost-every-request)).

### Every absolute URL drifts
The application writes its own URLs under an origin DriftGate does not know, such as its `APP_URL`. List it under `aliases` ([your own URLs on another port](reference/comparison.md#your-own-urls-on-another-port)).

### Paths in error pages differ between checkouts
A stack trace names the project's folder. Capture both sides with the same release, which records the folder, so `compare` takes the two folders as one ([paths of the project folder](reference/comparison.md#paths-of-the-project-folder)).

### A 500 on the first request after a restore (SQL Server)
A whole restore ended the application's pooled connections ([pooled connections](reference/databases/sql-server.md#pooled-connections-after-a-whole-restore)).

### Timeouts and 503 under load
A development server fails now and then under a capture's load. It is retried once by default ([retries](reference/capture.md#retries-of-requests-that-failed-under-load)).

### A changed file still answers the old way (PHP opcache)
PHP's opcache keeps the old code for a while ([opcache](guides/frameworks/laravel.md#phps-opcache-delays-your-changes)).

### A live server answers differently from the capture
A warm cache answers differently from a cold one ([a cache emptied at every restore](concepts/what-a-pass-proves.md#a-cache-emptied-at-every-restore)).

### `rowversion` values differ on every capture
SQL Server writes new `rowversion` values on every write. Add the allowlist rule for them ([allow rowversion values](guides/frameworks/aspnet-core.md#allow-rowversion-values)).

### prepare changed the restored state
A `prepare` command writes data before every capture ([what belongs in prepare](reference/driftgate-yaml.md#what-belongs-in-prepare)).

### An allowlist rule is listed as unused
The value it names was renamed, removed, or did not change this time, like a render time that is often the same ([find rules that cannot apply](guides/review/changing-values.md#find-rules-that-cannot-apply)).

## A green result that misses something

### A validation message changed, but the run passed
No request sends an invalid body to that route, so its validation errors are not compared. `coverage` and `run` list such routes. Add `empty-body` to `templates:` (and `json-errors` for Laravel, Symfony, Rails and Spring Boot), run `driftgate generate --write`, and add the new requests to the baseline ([validation errors](reference/upgrading-driftgate.md#validation-errors-in-a-project-made-before-empty-body)).

### Requests the frontend sends are not checked
They are not in the catalog. Record them ([record](guides/catalog/record.md)).

### Uploads, jobs or commands are not checked
The catalog has no request or command step for them ([what a pass does not check](concepts/what-a-pass-proves.md#what-a-pass-does-not-check)).

### A request that needs a cookie session answers 401
DriftGate keeps no cookies between requests. Give the identity a token ([sessions with cookies](guides/catalog/logins.md#sessions-with-cookies)).

### Every web POST answers 419
DriftGate keeps no session, so it sends no CSRF token. Exempt those routes, or compare only the 419 ([Laravel](guides/frameworks/laravel.md#web-forms-answer-419)).

## Slow or stuck

### Fingerprints are slow on MySQL in Docker
The server looks up the name of every client. Start it with `--skip-name-resolve` ([run the server in a container](reference/databases/mysql-mariadb.md#run-the-server-in-a-container)).

### Fingerprints are slow on SQL Server
Grant the login `VIEW SERVER PERFORMANCE STATE` on SQL Server 2022 and later, or `VIEW SERVER STATE` on 2017 and 2019 ([fast fingerprints](reference/databases/sql-server.md#fast-fingerprints)).

### "database is locked" (SQLite)
The application held a lock for longer than DriftGate waits, 10 seconds ([locks](reference/databases/sqlite.md#locks-and-the-applications-writers)).

### A command waits for another process
Another command holds the lock on the local store. Wait for it, or stop it; Ctrl-C stops the wait ([when a command waits](reference/cli.md#when-a-command-waits-for-another-one)).

### "… stayed in the middle of a restore, offline, single-user or in transition"
SQL Server 2019 and earlier stopped the command for another database of the server, and it tried again for a minute. Bring that database online, give it `MULTI_USER` back, complete its restore, or drop it ([errors 927 and 952](reference/databases/sql-server.md#errors-927-and-952)).

### Errors 927 or 952 on SQL Server
On SQL Server 2019 and earlier, another database of the server was being restored, created or renamed at that moment ([errors 927 and 952](reference/databases/sql-server.md#errors-927-and-952)).

## Databases

### "… Galera cluster node, which creates no sequence with a cache …"
A Galera node cannot create these MariaDB sequences, so a seed or a restore stops before it changes anything. So do `data anonymize` and `subset` when their target is on such a node ("target … is on a Galera cluster node"). For the state, make the sequences `NOCACHE` or `INCREMENT BY 0` and InnoDB, or exclude them. For the data module, change them so in the source, or write to a server without Galera ([MySQL and MariaDB](reference/databases/mysql-mariadb.md#how-seed-and-restore-work)).

### "not a dump of driftgate data anonymize or subset for …"
`data verify --dump` reads only a dump that `anonymize` or `subset` wrote, with its header line. Check that dump, or the target database ([data verify](reference/commands/data.md#driftgate-data-verify)).

### "a statement runs into the next one …"
A dump made by an earlier release hid a `;` behind a comment. Make the dump again with this release ([data verify](reference/commands/data.md#driftgate-data-verify)).

### seed fails: the user may not create a database
Grant the rights setup prints ([what the database user needs](reference/state.md#what-the-database-user-needs)).

### "certificate signed by unknown authority"
The server's certificate is verified, as the application's client does, and it does not pass ([certificate errors](reference/databases/sql-server.md#certificate-errors)).

### The SQL Server container stops at its first start
The `sa` password needs eight characters of three kinds; the container's log says why ([the sa password](reference/databases/sql-server.md#the-sa-password)).

---
**Related:** [What DriftGate can do](capabilities.md) · [Exit codes](reference/cli.md#exit-codes) · [Glossary](glossary.md)
