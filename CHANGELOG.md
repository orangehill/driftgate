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
- A project made by an earlier `init` keeps its `templates:` and its baseline. `coverage` and `run` now list its routes whose validation errors no request checks. To check them, add `empty-body` to `templates:`, run `driftgate generate --write` and `driftgate run`, and add the new requests to the baseline ([upgrading](docs/reference/upgrading-driftgate.md#validation-errors-in-a-project-made-before-empty-body)). The coverage JSON gains `routes_without_invalid_body`, and the report's `not_covered` gains `no_invalid_body`.

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

- The command reference is split into pages per command group; `driftgate help <command>` links its page. A build from a checkout links the docs of the `development` branch. ([#24](https://github.com/orangehill/driftgate/pull/24))
- MySQL/MariaDB: state fingerprints and restores take far fewer round trips; over a link with 55 ms latency a full fingerprint takes 1.7 s instead of 10.6 s. Results are the same. The log says why the per-table counters are off and which grant is missing. ([#22](https://github.com/orangehill/driftgate/pull/22))
- Allowlist proposals are narrower: enum values as the words seen, digit widths kept, a stable UUID kept literal. ([#10](https://github.com/orangehill/driftgate/pull/10), [#15](https://github.com/orangehill/driftgate/pull/15))
- `key-extra` and `key-missing` drifts show the value, hidden for private bodies. ([#15](https://github.com/orangehill/driftgate/pull/15))
- An incomplete result (exit 3) is a JUnit error, so a CI that reads only JUnit does not show green. ([#19](https://github.com/orangehill/driftgate/pull/19))
- Framework tables are proposed for every driver: caches as `restore_empty`, sessions and queues excluded. ([#16](https://github.com/orangehill/driftgate/pull/16))
- Each side's project folder in responses, such as in stack traces, compares as one neutral folder. ([#16](https://github.com/orangehill/driftgate/pull/16))
- The two baseline captures are one second apart, so timestamps differ as in later runs. ([#16](https://github.com/orangehill/driftgate/pull/16))
- The route count of `discover` and `run` reads `13 routes (13 from laravel)`; `laravel 13` read as a framework version.
- Skipped command steps are counted apart under "Not covered by this result" (`N command steps skipped`), not as requests that change data. The report's `not_covered.mutating_skipped` no longer holds command steps; they are in the new `commands_skipped`.

### Fixed

- `classify.yaml`: `action: null` written without quotes on a `serialized-php` column now sets the column to NULL. It was read as no action, the default `empty-table`, and the copy lost all rows of the table. `action:` with nothing after it, or an empty string, is now refused; only a missing `action` means `empty-table`. ([#26](https://github.com/orangehill/driftgate/pull/26))
- `seed` records the fingerprint of the pristine copy itself (MySQL, MariaDB, PostgreSQL). A write during the seed no longer stops every later capture. ([#13](https://github.com/orangehill/driftgate/pull/13))
- MySQL 8: tables with a collation that is not the charset's default restore correctly. ([#14](https://github.com/orangehill/driftgate/pull/14))
- MySQL: no reconnect on every cycle, which was slow on servers without `skip-name-resolve`. ([#14](https://github.com/orangehill/driftgate/pull/14))
- A UUID allowlist proposal no longer breaks on the next UUID with letters. ([#10](https://github.com/orangehill/driftgate/pull/10))
- The stale-rule warning no longer lists `init`'s own defaults or rules that could not apply. Following it used to delete good rules. ([#15](https://github.com/orangehill/driftgate/pull/15), [#19](https://github.com/orangehill/driftgate/pull/19))
- Row tracking finds a table named in another letter case or schema. ([#16](https://github.com/orangehill/driftgate/pull/16))
- A capture that was already cancelled can no longer complete. ([#16](https://github.com/orangehill/driftgate/pull/16))
- MySQL/MariaDB: a seed or restore whose login may not create a trigger or view with its `DEFINER` (or may create no trigger while the binary log is on) now stops before it drops anything (a seed: before it drops the old pristine copy), names the object and explains the privilege needed. When the server refuses a drop instead, the triggers and views dropped before it are made again from the pristine copy, and the message names any that could not be. ([#25](https://github.com/orangehill/driftgate/pull/25))
- No fixed-id warning for an API version or a date in a path, such as `/api/2/users` or `/posts/2024/05/17`. ([#25](https://github.com/orangehill/driftgate/pull/25))
- `generate` no longer lists `json-errors` as skipped for a route without a parameter value when it still makes the JSON twins of the `not-found` or `invalid-param` requests. Configured without any error template, `json-errors` gets one note instead of making nothing silently. ([#25](https://github.com/orangehill/driftgate/pull/25))
- SQL Server 2017 and 2019: `clean` no longer fails with error 927 or 952 when another database of the same server is being restored at that moment. It tries again for up to a minute, as seeds and restores do. ([#30](https://github.com/orangehill/driftgate/pull/30))
- A changed validation error no longer passes unseen. `init` writes the `empty-body` template for Laravel and FastAPI, whose discovery reads the rules request bodies are validated against, so each route with rules gets a request with an empty body. `coverage` lists the covered routes whose body is validated but that no request sends an invalid body, and `run` counts them under "Not covered by this result". The list never changes the exit code.
- A read-only request that writes is found when the tokens command writes a new token on every run. The search for it compared every run with the state before the read-only requests and stopped with exit 3, "prepare is not repeatable", without naming the request. Each run is now compared with the state it started from. When the starting state is not repeatable and no single request explains a change, the capture still stops with exit 3, naming the change and what is not repeatable.
- Two long values that differ in the middle show the difference. `run`, `compare`, `review`, `review --group` and the group explanations cut both values to the same window around the first character where they differ; they showed only the beginnings, which were equal.
- In mode `freeze`, `run` and `review` no longer tell you to `accept` new requests, which that mode refuses. They print the `driftgate baselines rebase <baseline> --from <capture> --reason "…" --allow-freeze` command that adds them, and say that it takes the whole capture. Mode `evolve` still names `accept`.
- An entry written by hand no longer makes `generate` drop the generated requests of its route method silently. `generate` and `run` name each such route method in a note, with the entries written by hand and the generated requests they take the place of, such as error variants and JSON twins.
- A Set-Cookie format proposed by a repeated capture or the first `run` no longer comes from the masked value: the cookie's value is proposed as `[^;]*`, and a header masked whole (`Authorization`, a credential header) as `.+`. Formats such as `[0-9A-Za-z]+%\dX` matched only the mask's letters and broke on the next cookie.
- A header value that came or went under an allowlist rule, such as a cookie no longer set, reads as such: `header:set-cookie has 1 value instead of 2: laravel-session removed`, not "no longer matches its allowlist format". The drift kind stays `format`.

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
