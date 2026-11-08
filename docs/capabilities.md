# What DriftGate can do

Every feature, one row each: when to use it, the guide that shows how,
and the reference with every rule. The last section says what DriftGate
does not do.

## Set up

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: setup --> Set up in one command | a new project, from zero to its first baseline | [guide](start/your-project.md) | [reference](reference/commands/project.md#driftgate-setup) |
| <!-- cap: init --> Configuration only | you go through the steps yourself | [guide](start/your-project.md#set-up-step-by-step-with-init) | [reference](reference/commands/project.md#driftgate-init) |
| <!-- cap: layouts --> Local or git layout | the baseline stays on your machine, or is shared | [guide](concepts/project-layout.md) | [reference](reference/driftgate-yaml.md#baselines) |
| <!-- cap: baselines-switch --> Switch layouts | the team or CI joins | [guide](guides/workflow/share-baselines.md) | [reference](reference/commands/project.md#driftgate-baselines) |
| <!-- cap: key --> The project key | private bodies are encrypted with it | [guide](concepts/project-layout.md#the-project-key) | [reference](reference/driftgate-yaml.md#key) |
| <!-- cap: worktrees --> A worktree per change | you check each change side by side | [guide](guides/workflow/worktrees.md) | [reference](reference/commands/project.md#setup-in-a-git-worktree) |
| <!-- cap: multi-service --> Several services | an API, a frontend or microservices in one check | [guide](guides/frameworks/multi-service.md) | [reference](reference/driftgate-yaml.md#target-and-targets) |

## Data and state

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: state-db --> Restore a database | every capture should start from the same data | [guide](concepts/how-it-works.md#why-every-capture-starts-from-the-same-data) | [reference](reference/state.md) |
| <!-- cap: seed --> The pristine copy | the seed data changed | [guide](start/your-project.md) | [reference](reference/commands/state.md#driftgate-seed) |
| <!-- cap: clean --> Remove pristine copies | you no longer need them | — | [reference](reference/commands/state.md#driftgate-clean) |
| <!-- cap: exclude --> Exclude tables | sessions, caches or logs must not be compared | [guide](guides/frameworks/laravel.md#the-database) | [reference](reference/state.md#database-components) |
| <!-- cap: restore-only --> Restore, never compare | the application writes a table by itself | [guide](guides/frameworks/aspnet-core.md#what-to-exclude) | [reference](reference/state.md#restore_only) |
| <!-- cap: restore-empty --> Empty a table at every restore | a cache must start empty | [guide](guides/frameworks/aspnet-core.md#what-to-exclude) | [reference](reference/state.md#restore_empty) |
| <!-- cap: row-tracking --> Track changed rows | you want to see which rows a request changes | [guide](concepts/how-it-works.md#what-counts-as-a-drift) | [reference](reference/state.md#database-components) |
| <!-- cap: state-commands --> Restore by commands | Redis, files or anything else holds state | [guide](guides/catalog/side-checks.md#check-files-on-disk-and-redis) | [reference](reference/state.md#restore-anything-else-with-commands) |
| <!-- cap: dsn-project --> The DSN from your settings | the project keeps it in `.env` or `appsettings.json` | [guide](start/your-project.md) | [reference](reference/database-settings.md#read-the-dsn-from-the-projects-own-settings) |
| <!-- cap: mysql --> MySQL and MariaDB | your database is MySQL or MariaDB | [guide](guides/frameworks/laravel.md#the-database) | [reference](reference/databases/mysql-mariadb.md) |
| <!-- cap: postgres --> PostgreSQL | your database is PostgreSQL | — | [reference](reference/databases/postgresql.md) |
| <!-- cap: sqlite --> SQLite | your database is a file | [guide](guides/frameworks/laravel.md#sqlite) | [reference](reference/databases/sqlite.md) |
| <!-- cap: sql-server --> SQL Server | your database is SQL Server 2017 or later | [guide](guides/frameworks/aspnet-core.md) | [reference](reference/databases/sql-server.md) |
| <!-- cap: guards --> Guards | mail or payments must not reach people | [guide](guides/catalog/side-checks.md#check-the-mail-the-application-sends) | [reference](reference/driftgate-yaml.md#seed-prepare-guards) |
| <!-- cap: prepare --> Prepare commands | something must run after every restore | [guide](guides/frameworks/laravel.md) | [reference](reference/driftgate-yaml.md#what-belongs-in-prepare) |
| <!-- cap: data-module --> Anonymized data | CI needs data like production, without personal data | [guide](guides/ci/test-data.md) | [reference](reference/commands/data.md#driftgate-data-anonymize) |
| <!-- cap: subset --> A sample of the data | a copy of everything is too large | [guide](guides/ci/test-data.md#make-the-copy) | [reference](reference/commands/data.md#driftgate-data-subset) |
| <!-- cap: data-classify --> Classify every column | the data module needs a rule for each column | [guide](guides/ci/test-data.md#classify-every-column) | [reference](reference/commands/data.md#driftgate-data-classify) |
| <!-- cap: data-verify --> Check an anonymized copy | an output must hold no personal data | [guide](guides/ci/test-data.md#make-the-copy) | [reference](reference/commands/data.md#driftgate-data-verify) |

## Routes and catalog

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: discover --> Find routes | the routes should come from the framework | [guide](guides/catalog/coverage.md) | [reference](reference/commands/catalog.md#driftgate-discover) |
| <!-- cap: generate --> Generate requests | every route should get a request | [guide](guides/catalog/coverage.md) | [reference](reference/commands/catalog.md#driftgate-generate) |
| <!-- cap: templates --> Request variants | clients depend on errors, JSON or anonymous answers | [guide](guides/catalog/coverage.md#add-the-variants-clients-depend-on) | [reference](reference/driftgate-yaml.md#templates) |
| <!-- cap: json-errors --> JSON error answers | an API client gets JSON errors | [guide](guides/catalog/coverage.md#add-the-variants-clients-depend-on) | [reference](reference/driftgate-yaml.md#json-errors) |
| <!-- cap: params --> Route parameter values | a route has `{id}` or another parameter | [guide](guides/catalog/coverage.md#give-route-parameters-values) | [reference](reference/driftgate-yaml.md#parameter-values) |
| <!-- cap: coverage --> Coverage | you want to see the routes without a check | [guide](guides/catalog/coverage.md) | [reference](reference/commands/catalog.md#driftgate-coverage) |
| <!-- cap: exclusions --> Exclude a route | a route is not checked on purpose | [guide](guides/catalog/coverage.md#exclude-a-route-on-purpose) | [reference](reference/exclusions-yaml.md#exclude-a-route) |
| <!-- cap: known-failures --> Known failures | a request already fails in the baseline | [guide](guides/review/fix-accept-or-expect.md#keep-a-failing-request-without-hiding-it) | [reference](reference/exclusions-yaml.md#acknowledge-a-request-that-fails) |
| <!-- cap: record --> Record a session | a browser, frontend or E2E suite sends requests | [guide](guides/catalog/record.md) | [reference](reference/commands/catalog.md#driftgate-record) |
| <!-- cap: har-import --> Import a HAR file | you recorded a session in a browser | [guide](guides/catalog/record.md#import-a-har-file-instead) | [reference](reference/commands/catalog.md#driftgate-har-import) |
| <!-- cap: sequences --> Sequences | requests must run in order, together | [guide](guides/catalog/hand-written-requests.md#check-the-login-itself) | [reference](reference/catalog-json.md#sequences) |
| <!-- cap: params-from --> A value from an earlier response | a path needs an id an earlier request made | [guide](guides/catalog/hand-written-requests.md#use-a-value-from-an-earlier-response) | [reference](reference/catalog-json.md#request-fields) |
| <!-- cap: multipart --> Upload a file | a route takes a real or synthetic file | [guide](guides/catalog/hand-written-requests.md#upload-a-file) | [reference](reference/catalog-json.md#multipart) |
| <!-- cap: uploads-dir --> The uploads folder | real upload files must stay outside git | [guide](guides/catalog/hand-written-requests.md#upload-a-file) | [reference](reference/driftgate-yaml.md#uploads) |
| <!-- cap: repeat --> Repeat a request | you test a rate limit | [guide](guides/catalog/hand-written-requests.md#test-a-rate-limit) | [reference](reference/catalog-json.md#repeat) |
| <!-- cap: command-steps --> Command steps | a worker, an import, a command or a schema-changing admin path | [guide](guides/catalog/hand-written-requests.md#run-a-queue-worker-or-another-command) | [reference](reference/catalog-json.md#command-steps) |
| <!-- cap: mutates --> Read-only POST | a search sent as `POST` only reads | [guide](guides/catalog/hand-written-requests.md#mark-a-request-that-only-reads) | [reference](reference/driftgate-yaml.md#requests) |
| <!-- cap: level-per-request --> A level per request | one request should be compared only so far | [guide](guides/catalog/hand-written-requests.md#compare-a-request-only-as-far-as-its-types) | [reference](reference/comparison.md#comparison-levels) |
| <!-- cap: observe --> Observe | a value no rule can describe | [guide](guides/catalog/hand-written-requests.md#measure-a-request-without-comparing-it) | [reference](reference/driftgate-yaml.md#observe) |
| <!-- cap: identities --> Tokens | requests need a signed-in user | [guide](guides/catalog/logins.md) | [reference](reference/driftgate-yaml.md#identities-and-tokens) |
| <!-- cap: side-checks --> Side checks | a schema, settings, files, Redis or mail must be checked | [guide](guides/catalog/side-checks.md) | [reference](reference/catalog-json.md#command-steps) |

## Capture and compare

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: run --> Run the check | after every change | [guide](start/tutorial.md) | [reference](reference/commands/check.md#driftgate-run) |
| <!-- cap: capture --> Capture under a tag | you compare two moments or two servers | [guide](guides/workflow/live-server.md) | [reference](reference/commands/check.md#driftgate-capture) |
| <!-- cap: repeat-capture --> Find changing values | values change by themselves between captures | [guide](guides/review/changing-values.md#find-the-values-that-change) | [reference](reference/commands/check.md#driftgate-capture) |
| <!-- cap: read-only --> Read-only capture | the server must not change | [guide](guides/workflow/live-server.md) | [reference](reference/commands/check.md#capture-only-the-requests-that-do-not-change-data) |
| <!-- cap: connect-to --> A fixed address | DNS does not yet point to the new server | [guide](guides/workflow/live-server.md#reach-a-new-server-before-dns-points-to-it) | [reference](reference/driftgate-yaml.md#connect_to) |
| <!-- cap: allow-remote --> A remote host | the server is outside the local network | [guide](guides/workflow/live-server.md) | [reference](security.md#allowed-hosts) |
| <!-- cap: concurrency --> Parallel requests | a capture of many read-only requests is slow | — | [reference](reference/capture.md#parallel-requests-and-concurrency-auto) |
| <!-- cap: rate-limits --> Rate limits | the application throttles a path | — | [reference](reference/driftgate-yaml.md#target-and-targets) |
| <!-- cap: retry --> Retries | a development server fails now and then under load | [guide](concepts/what-a-pass-proves.md#requests-that-passed-only-after-a-retry) | [reference](reference/capture.md#retries-of-requests-that-failed-under-load) |
| <!-- cap: compare --> Compare captures | two stacks or two moments | [guide](guides/workflow/server-upgrade.md) | [reference](reference/commands/check.md#driftgate-compare) |
| <!-- cap: origins --> Another port | a copy runs on another address | [guide](guides/workflow/worktrees.md) | [reference](reference/comparison.md#your-own-urls-on-another-port) |
| <!-- cap: html --> HTML pages | the application serves pages | [guide](guides/frameworks/nextjs-nuxt.md) | [reference](reference/comparison.md#html-pages) |
| <!-- cap: xlsx-csv --> Spreadsheets and CSV | the application exports files | [guide](guides/review/changing-values.md#allow-a-value-in-a-page-or-a-spreadsheet) | [reference](reference/comparison.md#spreadsheets-and-csv-files) |
| <!-- cap: side-effects --> Side effects | a request must not start writing elsewhere | [guide](concepts/how-it-works.md#what-counts-as-a-drift) | [reference](reference/comparison.md#side-effects-in-the-database) |
| <!-- cap: timings --> Response times | you want to see requests that became slower | — | [reference](reference/reports.md#response-times) |
| <!-- cap: show-private --> Private values locally | you need to see a private value | [guide](guides/review/read-the-report.md#values-shown-as-private) | [reference](reference/commands/check.md#driftgate-compare) |
| <!-- cap: skip-private --> A partial check | private bodies are not on this machine, as in CI | [guide](guides/ci/github-actions.md#private-bodies-in-ci) | [reference](reference/commands/check.md#driftgate-run) |

## Review

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: report --> Reports | a pull request needs a comment or a JUnit file | [guide](guides/ci/github-actions.md) | [reference](reference/commands/check.md#driftgate-report) |
| <!-- cap: kinds-line --> The kinds line | you want the drifts counted by kind | [guide](guides/review/read-the-report.md#the-lines-after-the-verdict) | [reference](reference/reports.md#the-count-of-drifts-by-kind) |
| <!-- cap: not-covered --> Gaps in a result | you want to know what a pass did not check | [guide](concepts/what-a-pass-proves.md) | [reference](reference/reports.md#what-the-result-does-not-cover) |
| <!-- cap: review --> Review | you decide what each drift group means | [guide](guides/review/fix-accept-or-expect.md) | [reference](reference/commands/review.md#driftgate-review) |
| <!-- cap: accept --> Accept | an intended change goes into the baseline | [guide](guides/review/fix-accept-or-expect.md#accept-an-intended-change) | [reference](reference/commands/review.md#driftgate-accept) |
| <!-- cap: expect --> Expect | a ticket intends a drift, and the baseline stays | [guide](guides/review/fix-accept-or-expect.md#expect-a-change-a-ticket-makes) | [reference](reference/commands/review.md#driftgate-expect) |
| <!-- cap: allowlist-header --> Header rules | a header changes on every request | [guide](guides/review/changing-values.md#allow-a-changing-header) | [reference](reference/allowlist-yaml.md#header-rules) |
| <!-- cap: allowlist-body --> Body rules | a value in a body changes by itself | [guide](guides/review/changing-values.md#allow-a-changing-value-in-a-body) | [reference](reference/allowlist-yaml.md#body-rules) |
| <!-- cap: allowlist-parts --> Part of a value | only a hash inside a value changes | [guide](guides/review/changing-values.md#allow-only-part-of-a-value) | [reference](reference/allowlist-yaml.md#body-rules) |
| <!-- cap: allowlist-any-length --> A list that grows | a list grows or shrinks by itself | [guide](guides/review/changing-values.md#let-a-list-grow-or-shrink) | [reference](reference/allowlist-yaml.md#body-rules) |
| <!-- cap: allowlist-raw --> The whole body | a page that is not JSON changes by itself | [guide](guides/review/changing-values.md#allow-a-value-in-a-page-or-a-spreadsheet) | [reference](reference/allowlist-yaml.md#the-whole-body) |
| <!-- cap: allowlist-cells --> Spreadsheet cells | an export writes its own time | [guide](guides/review/changing-values.md#allow-a-value-in-a-page-or-a-spreadsheet) | [reference](reference/allowlist-yaml.md#spreadsheet-and-csv-cells) |
| <!-- cap: array-key --> Pair rows by key (`key`) | rows come in another order, but items must match | [guide](guides/review/changing-values.md#rows-in-another-order-pair-by-key-or-allow-any-order) | [reference](reference/allowlist-yaml.md#array-rules) |
| <!-- cap: array-any-order --> Any order (`any_order`) | the order of a list does not matter | [guide](guides/review/changing-values.md#rows-in-another-order-pair-by-key-or-allow-any-order) | [reference](reference/allowlist-yaml.md#array-rules) |
| <!-- cap: allowlist-check --> Check the allowlist | a rule may no longer apply | [guide](guides/review/changing-values.md#find-rules-that-cannot-apply) | [reference](reference/commands/review.md#driftgate-allowlist-check) |
| <!-- cap: rebase --> Rebase | merged code should become the baseline | [guide](guides/workflow/share-baselines.md#rebase-the-baseline-after-a-merge) | [reference](reference/commands/review.md#driftgate-baselines-rebase) |
| <!-- cap: verify --> Verify baselines | you check baselines for edits and personal data | [guide](guides/workflow/share-baselines.md) | [reference](reference/commands/review.md#driftgate-verify) |
| <!-- cap: bisect --> Bisect | a step spans many commits | [guide](guides/review/find-the-commit.md) | [reference](reference/commands/review.md#driftgate-bisect) |

## CI and security

| Feature | Use it when | Guide | Reference |
|---|---|---|---|
| <!-- cap: github-action --> The GitHub Action | pull requests should run the check | [guide](guides/ci/github-actions.md) | [reference](security.md#what-a-pull-request-can-run-in-ci) |
| <!-- cap: pristine-cache --> Cache the pristine database | seeding in CI is slow | [guide](guides/ci/github-actions.md#cache-the-pristine-database) | [reference](reference/commands/state.md#driftgate-seed) |
| <!-- cap: private-bodies --> Private bodies | responses hold personal data | [guide](security.md#private-bodies) | [reference](reference/driftgate-yaml.md#storage-and-pii) |
| <!-- cap: pii-detector --> The personal-data detector | public responses must hold no personal data | [guide](guides/ci/test-data.md#make-the-responses-public) | [reference](security.md#the-personal-data-detector) |
| <!-- cap: release-verify --> Verify a release | you download a binary | [guide](start/install.md#verify-the-download) | [reference](security.md#release-verification) |
| <!-- cap: help-topics --> Help in the terminal | you need the fields of a file, or the kinds | — | [reference](reference/cli.md#get-help-in-the-terminal) |
| <!-- cap: drift-kinds --> Drift kinds | you need to know what a drift means | [guide](guides/review/read-the-report.md#one-drift-line) | [reference](reference/drift-kinds.md) |
| <!-- cap: key-create --> Create the key | a project has no key yet | [guide](start/tutorial.md) | [reference](reference/commands/project.md#driftgate-key-create) |
| <!-- cap: version --> The version | you report a problem | [guide](start/install.md#check-that-it-works) | [reference](reference/commands/project.md#driftgate-version) |

## What DriftGate does not do

- It compares only the requests in the catalog. What the catalog does not send is not checked ([what a pass proves](concepts/what-a-pass-proves.md)).
- It keeps no cookies between requests. A session that lives in a cookie needs a token ([sessions with cookies](guides/catalog/logins.md#sessions-with-cookies)).
- `params_from` fills a `{name}` in the path and its query string only, not headers or bodies ([request fields](reference/catalog-json.md#request-fields)).
- An identity sends only `Authorization: Bearer <token>` ([identities and tokens](reference/driftgate-yaml.md#identities-and-tokens)).
- Websockets and server-sent event streams are not recorded, and a capture compares plain HTTP responses only ([record](reference/commands/catalog.md#driftgate-record)).
- `bisect` does not support an SQLite state ([projects on SQLite](guides/review/find-the-commit.md#projects-on-sqlite)).
- SQL Server means SQL Server 2017 and later, not Azure SQL Database, Managed Instance or Synapse ([supported versions](reference/databases/sql-server.md#supported-versions)).
- The data module covers MySQL, MariaDB and PostgreSQL only, not SQLite or SQL Server ([data](reference/driftgate-yaml.md#data)).
- It compares data only on the same database server version: fingerprints differ across engines and versions ([fingerprints](reference/state.md#fingerprints)).
- It does not exercise uploads, admin paths or background jobs unless the catalog has requests or command steps for them.
- Response times are shown, never a gate.

---
**Related:** [Guides](guides/README.md) · [Reference](reference/README.md) · [Troubleshooting](troubleshooting.md)
