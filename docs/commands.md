# Commands

`driftgate help <command>` shows a command's flags; `driftgate help <topic>` lists the keys of
`catalog`, `allowlist`, `exclusions`, `classify`, `config`, or the drift `kinds`.

## Common flags
These mean the same in every command that takes them. Flags go before or after the arguments.

| Flag | Meaning |
|---|---|
| `--config <path>` | the configuration file; default: `.driftgate/driftgate.yaml` or `driftgate.yaml` here or in the nearest folder above |
| `--target <service>` | with several services, work on these only (repeatable or comma-separated) |
| `--baseline <name>` | the baseline to use; usually `local` |
| `--only <glob>` | only requests whose ID matches |
| `--format <f>` | output format; default `text` |
| `--max-drifts N` | list up to N drifts per request (default 20) |
| `--show-private` | show values of private bodies, on your own machine only; never kept |
| `--skip-private` | private bodies not on this machine are not compared; without it the result is incomplete (exit 3) |
| `--write` | write the result to the file; without it, only print it |

## init
Writes `driftgate.yaml`, a catalog and an allowlist for the project in this folder.
| Flag | Meaning |
|---|---|
| `--project <name>` | project name; default: the folder name |
| `--baselines local\|git` | `local` (default): all in `.driftgate/`, ignored by git; `git`: committed ([layout](configuration.md#where-the-files-are)) |
| `--service <name>` | write only this service (repeatable or comma-separated) |

## setup
Takes a project from nothing to its first baseline. A second run goes on where the first stopped.
| Flag | Meaning |
|---|---|
| `--yes` | ask nothing; stop with an instruction where a value would be guessed |
| `--project <name>`, `--baselines local\|git` | as for `init` |
| `--driver mysql\|postgres\|sqlite\|sqlserver`, `--database <name>` | the server and the working database (SQLite: its file), when the settings do not say |
| `--url <address>` | where the application answers; default: from its settings |

## seed
Runs the `seed:` commands and keeps a pristine copy of every database under `state:`.
| Flag | Meaning |
|---|---|
| `--skip-commands` | take the data already loaded as pristine (a cached database in CI) |

## clean
`driftgate clean [<component>…]` removes the pristine copies `seed` made, and their records. It never touches a working database.
| Flag | Meaning |
|---|---|
| `--yes` | remove without asking; needed without a terminal |

## run
Routes, catalog, capture and compare in one command. The first run makes the baseline from two equal captures.
Each run captures into `run-<checkout>` and replaces the last one; to keep a capture, make it with `driftgate capture <tag>`.
| Flag | Meaning |
|---|---|
| `--baseline <name>`, `--skip-private`, `--target <service>` | [common flags](#common-flags); with `--target` the check is reported as partial |
| `--allow-mutating-without-restore` | send requests that change data without a state restore |
| `--read-only` | capture only requests that do not change data |

## discover
Finds the routes of the application and compares them with the route inventory. Exit 1 when they changed.
| Flag | Meaning |
|---|---|
| `--write`, `--target <service>` | [common flags](#common-flags) |

## generate
Generates catalog requests for routes that hand-written and imported requests do not cover.
| Flag | Meaning |
|---|---|
| `--write`, `--target <service>` | [common flags](#common-flags); `--write` not with `--check` |
| `--check`, `--all` | exit 1 when the catalog is stale; generate for every route, also covered ones |

## coverage
Lists the routes without a request that succeeds. Exit 1 when there are any.
| Flag | Meaning |
|---|---|
| `--format text\|json`, `--target <service>` | [common flags](#common-flags) |
| `--baseline <name>` | the baseline whose answers decide success; `none`: the catalog alone |

## har import
`driftgate har import <file.har>` adds the requests of a browser's HAR file to the catalog.
| Flag | Meaning |
|---|---|
| `--write`, `--target <service>` | [common flags](#common-flags); one service only |

## record
A recording proxy on `127.0.0.1`. Use the application through it, and it proposes catalog requests ([record](catalog.md#record)).
| Flag | Meaning |
|---|---|
| `--listen <addr>`, `--allow-non-loopback` | the address to listen on; another than loopback needs the second flag, and anyone who reaches it acts as you |
| `--write`, `--target <service>` | [common flags](#common-flags); `--write` adds the requests on stop |
| `--include <glob>`, `--exclude <glob>` | record only, or never, the paths that match |

## capture
`driftgate capture <tag>` restores the state, sends the catalog and stores the responses under the tag.
| Flag | Meaning |
|---|---|
| `--only <glob>`, `--baseline <name>`, `--target <service>` | [common flags](#common-flags); the state must match the baseline's |
| `--force`, `--quiet` | replace an existing capture of this tag; print no progress |
| `--restore once\|each` | restore once (default), or also before every sequence |
| `--repeat N` | capture N times and propose allowlist rules for values that differ |
| `--allow-remote`, `--allow-remote-mutations` | allow a service outside `allow_hosts`; the second also sends it requests that change data |
| `--allow-mutating-without-restore` | send requests that change data without a state restore |
| `--skip-fingerprint-check` | do not check the state; the capture is marked unreliable |
| `--calibrate` | decide `concurrency: auto` and record it in the baseline |
| `--read-only` | send only requests that do not change data |

## compare
`driftgate compare <tag>` compares a capture with a baseline or another capture. Exit 1 on drift.
| Flag | Meaning |
|---|---|
| `--against <baseline\|tag>` | what to compare with; default `local` |
| `--level status\|shape\|types\|values` | a stricter level for this comparison only |
| `--only`, `--target`, `--max-drifts`, `--show-private`, `--skip-private`, `--format text\|json\|junit\|markdown` | [common flags](#common-flags) |
| `--skip-fingerprint-check` | compare captures made with this flag |

## report
The last comparison again, such as for a pull-request comment. It exits 0 after drift too.
| Flag | Meaning |
|---|---|
| `--format text\|json\|junit\|markdown`, `--max-drifts N` | [common flags](#common-flags) |
| `--exit-code` | exit as `compare` did: 1 on drift, 3 when incomplete |

## allowlist check
Checks the allowlist without capturing: rules that reach nothing, and rules too broad or shadowed. Exit 1 on findings.
| Flag | Meaning |
|---|---|
| `--baseline <name>`, `--format text\|json` | [common flags](#common-flags) |

## review
Lists the drift groups of the last comparison, with the `accept` command for each.
| Flag | Meaning |
|---|---|
| `--group N`, `--show-private` | show one group with every request it touches; [common flags](#common-flags) |

## accept
`driftgate accept <request|glob|group:N>…` takes drifts into the baseline, in mode `evolve` ([how](review.md#accept-expect-or-rebase)).
| Flag | Meaning |
|---|---|
| `--reason "…"`, `--ref <ticket>` | required: why the change is right; optional: a ticket or link |
| `--comparison <id>` | the comparison `review` showed; a newer one is refused |

## expect
`driftgate expect <request|glob|group:N>…` records drifts a ticket intends, without changing the baseline. Works in mode `freeze` too.
| Flag | Meaning |
|---|---|
| `--ref <ticket>`, `--reason "…"`, `--comparison <id>` | the first two required; `--comparison` as for `accept` |
| `--drop <ref>`, `--import <file>`, `--baseline <name>` | remove a ref's expectations; import its file from another checkout; the baseline for both |

## verify
Checks baselines against their manifest and acceptance log. Exit 1 when they disagree.
| Flag | Meaning |
|---|---|
| `--baseline <name>`, `--adopt` | check this baseline only; write the manifest of a baseline made before manifests |
| `--migrate-private`, `--migrate-state` | once, for an older baseline: move its personal data out of git; record its state fingerprint anew |

## bisect
`driftgate bisect <good> <bad>` finds the first commit of each drift group ([bisect](review.md#bisect)).
| Flag | Meaning |
|---|---|
| `--group N`, `--expected` | chase this group only (repeatable); chase expected drifts too |
| `--only <glob>`, `--baseline <name>`, `--format text\|json` | [common flags](#common-flags) |

## data classify
Proposes rules for the columns `classify.yaml` does not cover ([test data](workflows.md#test-data)).
| Flag | Meaning |
|---|---|
| `--write`, `--sample N` | add the proposed rules to `classify.yaml`; rows sampled per table (default 200) |

## data subset
`data anonymize` of the rows `subset:` chooses, and every row they reference.
| Flag | Meaning |
|---|---|
| `--dump <file>`, `--no-target`, `--replace`, `--format text\|json` | as for `data anonymize` |

## data anonymize
Copies `data.source` into `data.target` and/or a dump, with personal values replaced. Nothing is written unless `data verify` passes.
| Flag | Meaning |
|---|---|
| `--dump <file>`, `--no-target` | also write a dump; with `--no-target`, only the dump |
| `--replace`, `--format text\|json` | drop an existing target database first; [common flags](#common-flags) |

## data verify
Checks an output of `data anonymize` for personal values. Exit 1 when it finds any.
| Flag | Meaning |
|---|---|
| `--target <database>`, `--dump <file>`, `--format text\|json` | the database (default `data.target`) or the dump to check |

## baselines
`driftgate baselines local|git` moves the project to the other layout. A switch to `git` first checks the baselines as `verify` does.

## baselines rebase
`driftgate baselines rebase <name>` makes a capture the content of a baseline, such as after a merge. Make the capture with `--repeat 2`; a capture made once is refused.
| Flag | Meaning |
|---|---|
| `--from <tag>`, `--reason "…"` | required: the capture, and why |
| `--ref <ticket>`, `--allow-freeze` | a ticket or link; rebase in mode `freeze` too |

## key create
Creates the project key: `.driftgate/key` with `baselines: local`, the macOS Keychain with `git` ([key](security.md#the-project-key)).

## version
Prints the version.

## Exit codes
| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | drift; also changed routes, uncovered routes, a stale catalog, allowlist or `verify` findings, personal data found |
| `2` | configuration or usage error; a public body with personal data |
| `3` | environment error or incomplete result: unreachable service, state mismatch, failed guard, no key, interrupted capture, nothing compared; not a regression: fix the cause and run again |

## Environment variables
| Variable | Meaning |
|---|---|
| `DRIFTGATE_KEY` | the project key (base64), when `key:` names no other source |
| `DRIFTGATE_STORE` | the local store; over `store:` |
| `DRIFTGATE_UPLOADS` | the uploads folder; over `uploads.dir` |
| `DRIFTGATE_BISECT_DIR`, `_COMMIT`, `_PROJECT`, `_REPO` | set by `bisect` for its hooks: the commit's worktree, the commit, the project folder in it, your checkout |
