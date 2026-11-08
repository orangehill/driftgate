# Command line basics

What every `driftgate` command has in common. Each command has its own
section in the [command pages](../commands.md).

## Get help in the terminal

```sh
driftgate help                 # synopsis of every command
driftgate help <command>       # one command with its flags (also: <command> --help)
driftgate help catalog         # the fields of catalog.json
driftgate help allowlist       # the rules of allowlist.yaml, then allowlist check
driftgate help exclusions      # the keys of exclusions.yaml
driftgate help classify        # the rules of classify.yaml
driftgate help config          # the top-level keys of driftgate.yaml
driftgate help kinds           # every kind of drift, one line each
```

`driftgate help` ends with links to the documentation, to
[what DriftGate can do](../capabilities.md), and to
[troubleshooting](../troubleshooting.md). The help of a command ends with
a `Docs:` line. It links the command's
section in the pages of the release you run, also after `go install …@<tag>`.
A build from a checkout, or a `dev-…` build of a commit, links the
`development` branch; the GitHub Action's build and a release candidate
link `main`. An unknown command or help
topic names the page of
[what DriftGate can do](../capabilities.md).

## Common flags

| Flag | Meaning |
|---|---|
| `--config <path>` | the configuration file; every command except `version` takes it |
| `--target <service>` | with several services, work on the named ones only (repeatable or comma-separated) |

- Without `--config`, a command finds the project from the folder it runs in ([how](#where-the-projects-files-are)).
- Without `--target`, every command works on all services. `run` reports a check limited by `--target` as partial.
- Flags go before or after the positional arguments: `driftgate compare before --only 'get-*'` works.

## Where the project's files are

The paths in these pages are those of the local layout
(`baselines: local`): everything is in the project's `.driftgate/`
folder. With `baselines: git`, the same files are in `driftgate/`,
`driftgate.yaml` is at the project root, and the local store is in
`~/.driftgate/<project>` ([baselines](driftgate-yaml.md#baselines)).

Commands find the project from any folder inside it
([how](../concepts/project-layout.md#how-commands-find-the-project)).

## Which baseline a command uses

Baselines have names, and the default name is `local`.

- `setup` and the first `run` make the baseline `local`.
- `run`, `capture`, `compare`, `allowlist check` and `bisect` use `local` unless told otherwise; `bisect` only before there is a comparison.
- A baseline with another name (`run --baseline release`) must be named in every command that uses it: `--baseline <name>`, or `--against <name>` for `compare`.
- `accept` and `review` work on the last comparison and its baseline.
- `verify` checks every baseline unless `--baseline` names one.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | clean |
| `1` | drift, or another finding that fails the check (below) |
| `2` | configuration error (below) |
| `3` | environment error or an incomplete result: the result cannot be trusted, and it is not a regression (below) |

Exit `1` means one of:

- drift in responses or side effects, other than [expected](commands/review.md#driftgate-expect) drift;
- changed routes (`discover`);
- uncovered routes (`coverage`), also routes whose requests failed in the baseline;
- a stale catalog (`generate --check`);
- findings of `allowlist check`;
- a baseline that disagrees with its manifest, or holds personal data or credentials in git (`verify`);
- personal data in an output (`data verify`);
- columns without a rule (`data classify` without `--write`).

Exit `2` means the configuration, catalog, allowlist or baseline is not
usable. It also means a usage error, a missing `--reason`, or a public
body with personal data.

Exit `3` means one of:

- the application or the database cannot be reached;
- a file or the local store cannot be read or written (a full disk, permissions);
- the state fingerprint does not match the baseline;
- a guard failed;
- no encryption key;
- a baseline changed outside `accept`;
- an interrupted capture;
- private requests whose body is not on this machine, without `--skip-private`;
- a comparison that compared nothing: `--only` matched no request, or the baseline has none of the requests.

`compare` and `run` are the gate. `report` exits `0` also after drift,
unless [`--exit-code`](commands/check.md#use-report-as-a-gate) is given.

## Environment variables

| Variable | Meaning |
|---|---|
| `DRIFTGATE_KEY` | the project key (32 bytes, base64), when `key:` names no other source |
| `DRIFTGATE_STORE` | the local store folder; takes precedence over `store:` |
| `DRIFTGATE_UPLOADS` | the uploads folder; takes precedence over `uploads.dir` |
| `DRIFTGATE_BISECT_DIR`, `DRIFTGATE_BISECT_COMMIT` | set by `bisect` for its hooks: the worktree of the commit under test, and the commit |

Commands DriftGate runs never receive `DRIFTGATE_KEY` or the variable
named by `key.env`.

## What happens when you press Ctrl-C

Ctrl-C or SIGTERM (a cancelled CI job, `docker stop`) stops a
long-running command cleanly. These are `run`, `capture`, `seed`,
`bisect`, `discover`, `generate`, `data …` and `record`.

- A capture that was replacing another puts the previous one back.
- `bisect` removes its worktree and runs `bisect.finish`.
- The project's commands still running are stopped with everything they started. Each runs in a process group of its own, so a command that reads from the terminal, such as a `sudo` password prompt, cannot run there.
- A second signal ends DriftGate at once (exit 130 or 143).

A command killed outright (SIGKILL, out of memory) cannot clean up:

- A capture leaves the previous capture of its tag aside. The next capture of the tag puts it back first.
- An `accept` or a first `run` writes a baseline on a copy next to it (`.driftgate/baselines/.tmp-*`). The next `accept` or `run` removes the copy; the baseline is as it was.
- Killed in the moment the copy replaces the baseline, the old baseline is whole in `.tmp-old-*/baseline`. Every command stops (exit 3) until it is put back, or restored from git.

## When a command waits for another one

The commands that write the local store take a lock on it
(`<store>/.lock`): `run`, `capture`, `seed`, `accept`, `expect`,
`baselines rebase` and `bisect`.

- With `baselines: git`, the worktrees of one project share the store; with `baselines: local`, each has its own.
- A second such command waits until the first one is done, and says which process it waits for. Ctrl-C stops the wait.
- The lock goes with the process, also when it is killed.
- With the lock, a command removes the temporary files that killed processes left in the store, before it ends. It removes only those older than a few seconds before it started.

---
**Related:** [Commands](../commands.md) · [Glossary](../glossary.md) · [Troubleshooting](../troubleshooting.md)
