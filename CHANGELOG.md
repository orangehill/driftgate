# Changelog

Every DriftGate release and what it changes for you. The newest is first.
Changes that are merged but not released yet are under Unreleased.

The format follows Keep a Changelog, and versions follow semantic
versioning. Read **Upgrading** before you update an existing project.

## [Unreleased]

### Upgrading

- Read-only requests that time out or hit a load error are now sent again. Set `target.retry: 0` to keep the old behaviour. ([#17](https://github.com/orangehill/driftgate/pull/17))
- Guards now run after the restore and `prepare`, before the first request, and again before the first request that changes data when read-only requests ran in between. What a guard checks must be in place once `prepare` has run: a guard that relies on the read-only requests is no longer supported. ([#19](https://github.com/orangehill/driftgate/pull/19))
- `seed` now records the pristine copy's own fingerprint. If a capture says the state differs after you update, run `driftgate seed` once. ([#13](https://github.com/orangehill/driftgate/pull/13))
- MySQL 8: a pristine copy seeded by an earlier release may fail its restore. Re-create the working database, then run `driftgate seed`. ([#14](https://github.com/orangehill/driftgate/pull/14))
- A baseline whose failed requests leave routes uncovered now makes `run` exit 1. The baseline is kept. ([#17](https://github.com/orangehill/driftgate/pull/17))
- Drift group numbers start at `[1]`. Tools that read `groups[].n` see other numbers. ([#15](https://github.com/orangehill/driftgate/pull/15))
- An array whose items only change order reports one `item-order` drift, not one per position. ([#17](https://github.com/orangehill/driftgate/pull/17))
- Reports and `meta.json` gain new fields. Other existing fields are unchanged, and older baselines compare as before. ([#15](https://github.com/orangehill/driftgate/pull/15), [#16](https://github.com/orangehill/driftgate/pull/16), [#17](https://github.com/orangehill/driftgate/pull/17), [#19](https://github.com/orangehill/driftgate/pull/19))
- `driftgate clean` refuses a MySQL/MariaDB pristine copy made by an earlier release, which lacks the new mark. Run `driftgate seed` once to mark it, or remove it by hand. ([#19](https://github.com/orangehill/driftgate/pull/19))
- Existing configurations and allowlists work as before. All new keys are optional. ([#16](https://github.com/orangehill/driftgate/pull/16))
- The binary is about 6.3 MB larger, for the SQLite and SQL Server drivers. ([#15](https://github.com/orangehill/driftgate/pull/15))
- MySQL/MariaDB: each database component holds one more connection (13 instead of 12), for reading many definitions in one query. ([#22](https://github.com/orangehill/driftgate/pull/22))
- A project made by an earlier `init` keeps its `templates:` and its baseline. `coverage` and `run` now list its routes whose validation errors no request checks. To check them, add `empty-body` to `templates:` (and `json-errors` for Laravel, Symfony, Rails and Spring Boot), run `driftgate generate --write` and `driftgate run`, and add the new requests to the baseline ([templates](docs/catalog.md#generate-and-templates)). The coverage JSON lists them under `routes_without_invalid_body`, and the report under `not_covered.no_invalid_body`.

### Added

- SQLite state support: seed, restore and fingerprint, as for MySQL and PostgreSQL. The data commands (`classify`, `anonymize`, `subset`, `verify`) do not support SQLite and SQL Server yet. ([#15](https://github.com/orangehill/driftgate/pull/15))
- SQL Server 2017 and later state support, with an SQL login. After a restore of the whole database, the first read-only request on each dead application connection is sent once more, and the report says so. ([#15](https://github.com/orangehill/driftgate/pull/15))
- `restore_only` and `restore_empty` for tables the application writes on its own, such as caches, sessions and queues. ([#16](https://github.com/orangehill/driftgate/pull/16))
- `target.retry` (0–3, on by default) sends a failed read-only request again. A request that passed only after a retry is shown as such. ([#17](https://github.com/orangehill/driftgate/pull/17))
- `driftgate expect` reports a drift a ticket expects as expected, not hidden. ([#17](https://github.com/orangehill/driftgate/pull/17))
- `--read-only` on `capture` and `run` sends only requests that do not change data, for a server that must not change. ([#19](https://github.com/orangehill/driftgate/pull/19))
- `target.connect_to` sends requests to a fixed address and keeps the host name, like `curl --connect-to`. ([#19](https://github.com/orangehill/driftgate/pull/19))
- `driftgate clean` removes the pristine copies that `seed` made. It never touches a working database. ([#19](https://github.com/orangehill/driftgate/pull/19))
- `baselines rebase` makes a capture the baseline's content after an intended data change (capture it with `--skip-fingerprint-check`). ([#17](https://github.com/orangehill/driftgate/pull/17))
- The result says what it does not cover, such as routes without a request. ([#17](https://github.com/orangehill/driftgate/pull/17))
- Reports count drifts per kind. ([#19](https://github.com/orangehill/driftgate/pull/19))
- Warnings when `prepare:` changed the restored state, and when a request that changes data has a fixed numeric id in its path. ([#17](https://github.com/orangehill/driftgate/pull/17), [#19](https://github.com/orangehill/driftgate/pull/19))
- `init` writes a `seed:` example for the database it finds. For Laravel, Symfony, Rails and Spring it asks for JSON error responses. ([#15](https://github.com/orangehill/driftgate/pull/15), [#17](https://github.com/orangehill/driftgate/pull/17))
- `requests:` sets `mutates` and a body per route, without editing the catalog. ([#16](https://github.com/orangehill/driftgate/pull/16))
- A command step can name its `dir` and a `stdin` file. ([#17](https://github.com/orangehill/driftgate/pull/17))
- `review --show-private`, and `compare run` for this checkout's run capture. ([#15](https://github.com/orangehill/driftgate/pull/15))
- `--repeat` keeps the median response time of each request. ([#17](https://github.com/orangehill/driftgate/pull/17))
- `driftgate help <command>` ends with a link to its docs for the release you run. New help topics `catalog` and `allowlist`. ([#17](https://github.com/orangehill/driftgate/pull/17), [#20](https://github.com/orangehill/driftgate/pull/20))
- `driftgate help kinds` lists every drift kind; the docs gain a glossary and a drift kinds reference. ([#23](https://github.com/orangehill/driftgate/pull/23))
- `driftgate help exclusions`, `help classify` and `help config` list the keys of those files; every help topic links its own reference page, and `driftgate help` names them all. ([#26](https://github.com/orangehill/driftgate/pull/26))
- Detection reads NestJS/TypeORM `DB_TYPE`. Laravel closure routes with a FormRequest get a body from its rules. ([#15](https://github.com/orangehill/driftgate/pull/15), [#16](https://github.com/orangehill/driftgate/pull/16))

### Changed

- `--read-only` takes `prepare:` when `state:` is configured: `prepare` should write only to data the restore brings back. Without `state:` it is still refused.
- `data verify --dump` reads only a dump that starts with the header line `data anonymize` and `data subset` write (format 1 or 2, for the source's database type). Other files, such as a `mysqldump` or `pg_dump` output, are refused unless they carry that line. Dumps with CRLF line endings are read. A format 1 dump in which a comment at the end of a statement hid its `;` is refused with a message to make the dump again, instead of being read with the next statements' rows as text.
- The command reference is split into pages per command group; `driftgate help <command>` links its page. A build from a checkout links the docs of the `development` branch. ([#24](https://github.com/orangehill/driftgate/pull/24))
- MySQL/MariaDB: state fingerprints and restores take far fewer round trips; over a link with 55 ms latency a full fingerprint takes 1.7 s instead of 10.6 s. Results are the same. The log says why the per-table counters are off and which grant is missing. ([#22](https://github.com/orangehill/driftgate/pull/22))
- Allowlist proposals are narrower: enum values as the words seen, digit widths kept, a stable UUID kept literal. ([#10](https://github.com/orangehill/driftgate/pull/10), [#15](https://github.com/orangehill/driftgate/pull/15))
- `key-extra` and `key-missing` drifts show the value, hidden for private bodies. ([#15](https://github.com/orangehill/driftgate/pull/15))
- An incomplete result (exit 3) is a JUnit error, so a CI that reads only JUnit does not show green. ([#19](https://github.com/orangehill/driftgate/pull/19))
- Framework tables are proposed for every driver: caches as `restore_empty`, sessions and queues excluded. ([#16](https://github.com/orangehill/driftgate/pull/16))
- Each side's project folder in responses, such as in stack traces, compares as one neutral folder. ([#16](https://github.com/orangehill/driftgate/pull/16))
- The two baseline captures are one second apart, so timestamps differ as in later runs. ([#16](https://github.com/orangehill/driftgate/pull/16))
- The route count of `discover` and `run` reads `13 routes (13 from laravel)`; `laravel 13` read as a framework version.
- "Not covered by this result" names every request the comparison skipped, in every format, by what it is (request that changes data, read, command step) and why (no baseline, not sent by `capture --read-only`, private body not on this machine …). In JSON this is `not_covered.skipped`, a list with `what`, `reason`, `count` and `names`.

### Fixed

- A migration run on the working database since the seed no longer passes unseen: the first restore undid it, and the capture ran on the old schema. A capture, probe or `verify --migrate-state` now compares the schemas first and stops with exit 3 before restoring; with `prepare:` it goes on when `prepare` makes the change again. A run that stops between its restore and that check (a failing `prepare`, a cancel, a kill) leaves a record beside the pristine copy's, which the next run checks and a seed removes. `setup` writes `prepare: [php artisan migrate --force]` for Laravel when `state:` is on. A MySQL/MariaDB restore that fails or is stopped ends its copy statements on the server and makes the dropped triggers, sequences and views again.
- PostgreSQL: a capture and `verify --migrate-state` compare the run's first whole copy of the database with the seed's record, whether the first restore or the reset before a request that changes data made it, so a pristine copy changed since the seed stops the run, also in items the restore did not find different.
- PostgreSQL: when the role may not end another session on a database that a restore or seed copies or renames, the run now stops after about 5 seconds with "being accessed by other users" followed by the permission error. It used to stop after about 25 seconds without the permission error. A restore or seed no longer tries to end autovacuum workers, which the server ends itself, so a worker the role may not end no longer leaves the other sessions running. The copy no longer retries other errors, and a cancel while a session is in the way stops it at once.
- `data anonymize`/`data subset` publish (PostgreSQL) no longer stops at once with a permission error when an autovacuum worker is on the staging database; the server ends it. A session the role may not end stops publish after about 5 s with "being accessed by other users" plus the permission error, instead of retrying for minutes.
- SQL Server: a working database in `RECOVERY_PENDING`, `SUSPECT` or another state that is not online, offline or restoring stops the run with its state, instead of a restore of the whole database over it.
- Guidance from `coverage`, `run` and `generate`. In mode freeze, the lists of requests that failed in the baseline give the full `driftgate baselines rebase … --allow-freeze` command, on a capture with no other drift, instead of saying to accept the new answers, which freeze refuses. `generate` notes each template the framework needs and the project lacks, `empty-body` as well as `json-errors`, once per template. `coverage` ticks "every route is covered or excluded" only when it lists nothing to fix; after such lists it says to fix them. The exit code is unchanged.
- `data anonymize` and `data subset` dumps: a statement whose last line ends in a comment (`--`, or `#` on MySQL), such as a trigger or routine definition, no longer hides the `;` after it, which made `mysql`/`psql` and `data verify` read it together with the next statements. Dumps now say `dump format 2`. `data verify` also reads MySQL `#` comments as comments: an apostrophe in one no longer opens a string.
- `data verify` with a dump: an empty string at the start of an INSERT's rows, before any other quoted value, was read as NULL. An empty value holds nothing to check, so no finding was missed.
- `classify.yaml`: `action: null` written without quotes on a `serialized-php` column now sets the column to NULL. It was read as no action, the default `empty-table`, and the copy lost all rows of the table. `action:` with nothing after it, or an empty string, is now refused; only a missing `action` means `empty-table`. ([#26](https://github.com/orangehill/driftgate/pull/26))
- `seed` records the fingerprint of the pristine copy itself (MySQL, MariaDB, PostgreSQL). A write during the seed no longer stops every later capture. ([#13](https://github.com/orangehill/driftgate/pull/13))
- MySQL 8: tables with a collation that is not the charset's default restore correctly. ([#14](https://github.com/orangehill/driftgate/pull/14))
- MySQL: no reconnect on every cycle, which was slow on servers without `skip-name-resolve`. ([#14](https://github.com/orangehill/driftgate/pull/14))
- A UUID allowlist proposal no longer breaks on the next UUID with letters. ([#10](https://github.com/orangehill/driftgate/pull/10))
- The stale-rule warning no longer lists `init`'s own defaults or rules that could not apply. Following it used to delete good rules. ([#15](https://github.com/orangehill/driftgate/pull/15), [#19](https://github.com/orangehill/driftgate/pull/19))
- Row tracking finds a table named in another letter case or schema. ([#16](https://github.com/orangehill/driftgate/pull/16))
- A capture that was already cancelled can no longer complete. ([#16](https://github.com/orangehill/driftgate/pull/16))
- MySQL/MariaDB: a seed or restore whose login may not create a trigger or view with its `DEFINER` (or may create no trigger while the binary log is on) now stops before it drops anything (a seed: before it drops the old pristine copy), names the object and explains the privilege needed. When the server refuses a drop instead, the triggers and views dropped before it are made again from the pristine copy, and the message names any that could not be. ([#25](https://github.com/orangehill/driftgate/pull/25))
- MySQL/MariaDB: `anonymize`, `subset`, `verify` and `classify` read the source again from a new snapshot when the server stops that read for a lock (error 1213 or 1205), up to three times, before anything is written. The output is the same as without the stop.
- MySQL/MariaDB: `verify`, also the one `anonymize` and `subset` run, notes a retried read of the target when the server stops it for a lock. A retry of the target's read before the write, earlier in the same run, used up that note without printing it.
- MySQL/MariaDB: a fingerprint, seed or restore, the read of changed rows, and a `params:` query that the server stops for a lock (error 1213 or 1205) run again from the start, up to three times, so they still read the current state. A Galera node stops even a read this way when another session alters a table.
- MariaDB: a restore or seed reads the column details of the state database only. It read them for every database on the server, which was slow with many databases (0.5 s instead of 1 ms with 1,000 tables), and a Galera node stopped it when another database changed. Two tables whose names differ only in case (`users` and `Users`, with `lower_case_table_names=0`) no longer get each other's columns, primary key or row count.
- MariaDB: a sequence that has cycled keeps its cycle count when seed, restore, `anonymize` or `subset` copy it. The copy used to start the count again, so the pristine copy differed from the working database.
- MariaDB Galera: seed, restore, `anonymize` and `subset` copy sequences again; every sequence copy failed (1235). A sequence the node cannot create, with a cache and an increment other than 0 or not stored by InnoDB, now stops a seed or restore before anything is changed, and the message names it. `anonymize` and `subset` name such a sequence of the source in the same way before they write anything to a target on a Galera node.
- No fixed-id warning for an API version or a date in a path, such as `/api/2/users` or `/posts/2024/05/17`. ([#25](https://github.com/orangehill/driftgate/pull/25))
- `generate` no longer lists `json-errors` as skipped for a route without a parameter value when it still makes the JSON twins of the `not-found` or `invalid-param` requests. Configured without any error template, `json-errors` gets one note instead of making nothing silently. ([#25](https://github.com/orangehill/driftgate/pull/25))
- SQL Server 2017 and 2019: `clean` no longer fails with error 927 or 952 when another database of the same server is being restored at that moment. It tries again for up to a minute, as seeds and restores do. ([#30](https://github.com/orangehill/driftgate/pull/30))
- SQL Server 2017 and 2019: a seed, restore, fingerprint or `clean` that another database's restore stopped is now tried again also when its first try took longer than a minute, such as the seed of a large database. The minute counts from the first such stop.
- SQL Server 2017 and 2019: a seed, restore, fingerprint or `clean` is also tried again when another database of the same server is being set single-user (error 924) or taken offline (error 942), as another checkout's seed, restore or `clean` does. Before, the command failed at once.
- SQL Server 2017 and 2019: after a restore of the whole database stopped and left the working database offline or restoring (a killed capture), the next capture's restore no longer fails with error 942 or 927 about the working database while other databases of the server are being restored. It tries again, bounded by the same minute, and the restore completes.
- SQL Server 2017 and 2019: a seed, restore, fingerprint or `clean` that another database's restore stopped no longer fails when its next try gets an error without a reason ("Request failed but didn't provide reason"). That try is waited out as the stop before it, within the same minute.
- SQL Server 2017 and 2019: `generate`'s `params` query of the pristine copy is tried again, as seeds and restores are, when another database's restore stops it (927, 952). When a retry gives up after the minute, the error names the database that stayed restoring, offline or single-user and says how to end that.
- SQL Server: a seed, restore, fingerprint, `clean` or `generate` query that the server chose as a deadlock victim (error 1205) runs again, up to three more times per retry (bounded in total when one operation runs inside another), instead of failing at once. A restore is still checked against the pristine copy's fingerprint after it.
- A changed validation error no longer passes unseen. `init` writes the `empty-body` template for Laravel and FastAPI, whose discovery reads the rules request bodies are validated against, so each route with rules gets a request with an empty body. `coverage` lists the covered routes whose body is validated but whose validation error no request shows (for Laravel, Symfony, Rails and Spring Boot, which redirect a browser, only a request asking for JSON or one the baseline recorded as 400 or 422 counts), and `run` counts them under "Not covered by this result". The list never changes the exit code.
- A read-only request that writes is found when the tokens command writes a new token on every run. The search for it compared every run with the state before the read-only requests and stopped with exit 3, "prepare is not repeatable", without naming the request. Each run is now compared with the state it started from. When the starting state is not repeatable and no single request explains a change, the capture still stops with exit 3, naming the change and what is not repeatable.
- Two long values that differ in the middle show the difference. `run`, `compare`, `review`, `review --group` and the group explanations cut both values to the same window around the first character where they differ; they showed only the beginnings, which were equal.
- In mode `freeze`, `run` and `review` no longer tell you to `accept` new requests, which that mode refuses. They print the `driftgate baselines rebase <baseline> --from <capture> --reason "…" --allow-freeze` command that adds them, and say that it takes the whole capture. The command is printed only when nothing else drifted; next to a drift they say to fix or expect it first, so the rebase never takes a regression into the baseline. A comparison that is not of a capture against a baseline (two captures, two baselines) gets the general form of the command. Mode `evolve` still names `accept`.
- An entry written by hand no longer makes `generate` drop the generated requests of its route method silently. `generate` and `run` name each such route method in a note, with the entries written by hand and the generated requests they take the place of, such as error variants and JSON twins.
- A Set-Cookie format proposed by a repeated capture or the first `run` no longer comes from the masked value: the cookie's value is proposed as text without `;` of the lengths seen (`[^;]{40,44}`), so a cookie emptied or set to `deleted` fails it, and a header masked whole (`Authorization`, a credential header) as `.+`. A format such as `[0-9A-Za-z]+%\dX` described the mask's letters, not the cookie, and failed on a value of another shape.
- A header value that came or went under an allowlist rule, such as a cookie no longer set, reads as such: `header:set-cookie has 1 value instead of 2: laravel-session removed`, not "no longer matches its allowlist format". The drift kind stays `format`.
- `drift-kinds.md` and `driftgate help kinds` gave `1.0 for 1` as a `number-literal`; the comparison reports it, as intended, as a `type` drift (`int → float`), which level `types` already catches. The docs now say so, and give same-type examples for `number-literal` (`1.0` → `1.00`, `1250.5` → `1.2505e3`). Detection is unchanged.
- The shortened drift values no longer start or end inside a character the eye sees as one: an accent stays on its letter, and a joined emoji, a skin tone or a variation selector stays whole.

### Security

- Every printed command hides passwords, tokens and secrets, also inside shell scripts and SQL. ([#15](https://github.com/orangehill/driftgate/pull/15))
- SQL Server connections never use weaker TLS than the client asks for. ([#15](https://github.com/orangehill/driftgate/pull/15))
- `meta.json` records a folder in a person's home as `~/…`, so it names no person. ([#16](https://github.com/orangehill/driftgate/pull/16))
- With `connect_to`, `allow_hosts` and the remote-mutation guard check the address actually dialled. ([#19](https://github.com/orangehill/driftgate/pull/19))

## [1.0.2] - 2026-11-02

### Changed

- The GitHub Action is listed on the Marketplace as "DriftGate API Regression Gate". `uses: orangehill/driftgate` is unchanged. ([#4](https://github.com/orangehill/driftgate/pull/4))
- The README has badges and a short demo of a baseline and a drift report. ([#1](https://github.com/orangehill/driftgate/pull/1), [#5](https://github.com/orangehill/driftgate/pull/5))

## [1.0.1] - 2026-11-02

### Added

- First public release.

[Unreleased]: https://github.com/orangehill/driftgate/compare/v1.0.2...development
[1.0.2]: https://github.com/orangehill/driftgate/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/orangehill/driftgate/releases/tag/v1.0.1
