# Troubleshooting

Find the message as DriftGate prints it; `…` stands for the parts that
vary. Exit codes are under [exit codes](commands.md#exit-codes).

## Setup

| Message | Cause | Fix |
|---|---|---|
| `the application does not answer at …` | The application is not running at `base_url`. | Start it, or set `base_url`, then run `driftgate setup` again. |
| `the catalog has no requests: configure discover: …` | No routes were found. | Configure `discover:`, or [write requests by hand](catalog.md#hand-written-requests). |
| `2 runs on the same state gave different responses.` | A value changes by itself, such as a time or a token. | Add the proposed [allowlist rules](review.md#allowlist-rules) with a reason, and run again. |
| `no baseline was made: add the proposed allowlist entries (with reasons) and run again` | The two first captures differ. | As above. |
| `there is no baseline yet: the first run makes it from every request, so it takes no --read-only …` | The first `run` got `--read-only`. | Make the baseline without `--read-only`. |
| `git does not ignore …, which hold the key, captures and private bodies: …` | `.gitignore` lacks `.driftgate/`, or git tracks files in it. | Add the line the message names; untrack files with `git rm -r --cached`. |
| `… names the database …, but working is …` | The application's setting and `working:` name different databases, as in a worktree. | Make them agree ([worktrees](workflows.md#worktrees)). |
| `generate.identity … is not an identity and no tokens source is configured` | `generate.identity` names no identity. | Add it under `identities:`, or configure `tokens:`. |
| The server refuses to create the pristine database | The database login may not create databases. | Grant the rights in [what every database needs](databases.md#what-every-database-needs). |

## Capture and state

| Message | Cause | Fix |
|---|---|---|
| `no token for identity …` | A request names an identity without a token. | Give it one ([identities and tokens](configuration.md#identities-and-tokens)). |
| `the catalog has requests that change data (…), but no state with a restore command is configured` | No `state:` component can restore the data. | Configure [`state:`](configuration.md#state), or pass `--allow-mutating-without-restore`. |
| `state … has no pristine copy yet; run driftgate seed` | The database was never seeded. | Run `driftgate seed`; `run` seeds by itself. |
| `restore of … left … items different from the fingerprint driftgate seed recorded of the pristine copy` | The application wrote during the seed or the restore. | Stop queue workers and schedulers, then run `driftgate seed` again. |
| `the schema of the working database changed since the pristine copy was made: …` | A migration ran since the seed; the restore would undo it. | Add the migrate command to `prepare:`, or run `driftgate seed` ([databases](databases.md#what-every-database-needs)). |
| `… the restore undid it, and prepare did not make it again …` | The change comes from another branch or was made by hand. | Make it again, then seed; seeding now, or deleting the named record, accepts the undo. |
| `… changed again since an earlier run whose restore it outlived …` | The schema changed again after a run stopped between its restore and its check. | Run `driftgate seed`, or delete the named record to accept the undo. |
| `the state differs from the one the baseline was captured from (…)` | The pristine copy holds other data than the baseline's. | Seed the baseline's data, or make a new baseline. |
| `guard "…" failed: …` | A guard's check did not pass, such as mail not going to a log. | Fix the setting it checks ([guards](configuration.md#seed-prepare-and-guards)). |
| `read-only requests changed …; finding which …` | A `GET` or another read-only request writes data. | Mark it `mutates: true` under [`requests:`](configuration.md#storage-observe-and-requests), then `driftgate generate --write`. |
| `warning: the state changed between the restore and the end of prepare: …` | A `prepare` command writes data before every capture. | Move it to `seed`. |
| `--read-only covers the requests only, not the project's commands: prepare: …` | `prepare:` may write to a server that must not change, and no `state:` restores it. | Leave `prepare:` out for a [live server](workflows.md#live-server). |
| `… incomplete … (interrupted?); capture it again with --force` | The capture was interrupted. | Capture again with `--force`. |
| `another driftgate is using the store …; waiting until it is done (Ctrl-C to stop)` | Another command holds the local store. | Wait for it, or stop it. |
| `personal data or credentials found in … responses marked public; they were stored as private …` | The detector found personal data in a public body. | Mark the request private, or allow the value ([private bodies](security.md#private-bodies)). |
| `state …: another connection held a lock on … for …` | SQLite: a long transaction or a worker of the application. | End the transaction or stop the worker, then run again. |
| `… the server is a Galera cluster node, which creates no sequence with a cache …` | MariaDB Galera cannot copy these sequences. | Make them `NOCACHE` and InnoDB, or exclude them ([MySQL and MariaDB](databases.md#mysql-and-mariadb)). |
| `… stayed in the middle of a restore, offline, single-user or in transition …` | SQL Server 2019 and earlier: another database of the server is in transition. | Bring it online, set `MULTI_USER`, or drop it ([SQL Server](databases.md#sql-server)). |

## Compare and review

| Message | Cause | Fix |
|---|---|---|
| `nothing was compared: no request was selected …` | `--only` or `--target` matches no request ID. | Check the glob against the catalog. |
| `nothing was compared: the baseline has none of the selected requests …` | The baseline was made for another catalog, or IDs were renamed. | Make the baseline again. |
| `… has private bodies, encrypted with the project key, and comparing them needs it …` | The project key is missing. | Set `DRIFTGATE_KEY` or `key:` ([the project key](security.md#the-project-key)). |
| `baseline … changed outside driftgate accept (…); run driftgate verify` | A baseline file was edited, removed, or checked out with CRLF line endings. | Run `driftgate verify`, then restore the baseline from git. |
| `mode is freeze: the API must stay as the baseline has it, so drifts are not accepted; …` | `accept` in mode `freeze`. | Fix the code, [expect](review.md#accept-expect-or-rebase) the drift, or set `mode: evolve`. |
| `accept needs --reason "…": …` | No reason was given. | Pass `--reason`. |
| `… takes only part of the drifts of … requests; a request is accepted with all of its drifts or not at all` | The groups selected cover only part of a request. | Select the other groups too, or name the requests. |
| `there is no comparison yet: run driftgate compare or driftgate run first` | `review`, `accept` or `expect` ran before any comparison. | Run `driftgate run`. |
| `it applies to no request of the catalog that is compared …` | `allowlist check`: the rule names a renamed or removed request. | Fix or remove the rule. |
| `item-order` drifts after a database change | A list has no `ORDER BY`. | Add `ORDER BY`, or an [`any_order` rule](review.md#allowlist-rules). |
| Every absolute URL drifts | The application writes its own URLs under another origin. | List it under [`target.aliases`](configuration.md#target). |
| Every web form `POST` answers 419 | DriftGate keeps no session, so it sends no CSRF token. | Exempt those routes ([Laravel](frameworks.md#laravel)). |
| A validation message changed, but the run passed | No request sends an invalid body. | Add `empty-body` and `json-errors` to [`templates:`](catalog.md#generate-and-templates). |

## CI

| Message | Cause | Fix |
|---|---|---|
| `the result is incomplete: … could not be compared here (the private body is not on this machine) …` | Private bodies stay on the machine that captured them. | Pass `--skip-private`, or use [test data](workflows.md#test-data) without personal data. |
| `generate --check` exits 1 | The committed catalog is not what `generate` writes. | Run `driftgate generate --write` and commit. |
| `… routes: … covered, … excluded, … uncovered` with exit 1 | A route has no request that succeeds. | Add a request, exclude the route, or list a known failure ([coverage](catalog.md#coverage-and-exclusions)). |
| `Known failures that are stale (remove them from exclusions.yaml)` | A known failure succeeds now. | Remove it from `exclusions.yaml`. |
