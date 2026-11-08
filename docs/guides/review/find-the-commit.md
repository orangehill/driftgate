# Find the commit that introduced a drift

> **Before you start:** have a comparison with drift at the bad commit. **Time:** 15 minutes, plus a run per commit.

`driftgate bisect <good> <bad>` searches the commits between a good and a
bad commit. It names the first commit for each drift group separately:
one commit may bring one change, and another a different one.

## Configure the bisect hooks

<!-- check: config-fragment -->
```yaml
bisect:
  checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && composer install -q && php artisan migrate --force && make restart"]
  ready: ["curl", "-sf", "http://localhost:8000/up"]
  finish: ["make", "restart"]        # at the end: back on your own checkout
```

DriftGate checks out each commit in a temporary git worktree. Your hooks
put the application on it:

- `checkout` builds or restarts the application from `$DRIFTGATE_BISECT_DIR`. When it fails, the commit is skipped.
- `ready` is repeated until the application answers. When it does not in time, the commit is skipped.
- `finish` runs once at the end.

- The state is restored on the database the configuration names. So the application in the worktree must use that database: MySQL, PostgreSQL or SQL Server ([SQLite](#projects-on-sqlite) is refused).

Every key and its default is under [bisect](../../reference/driftgate-yaml.md#bisect).

## Run bisect

```sh
driftgate run                          # at the bad commit: exit 1, drift groups [1] … [n]
driftgate bisect v2.3.0 HEAD           # every open group
driftgate bisect v2.3.0 HEAD --group 2 # one group
```

Compare first at the bad commit, so that bisect knows the drift groups.

- Before the search, it checks that the good commit has no drift in the chosen groups, and the bad commit has it. Otherwise it stops (exit 2), naming the group.
- For each commit it restores the state, captures the drifted requests, and compares them with the baseline.
- Without a comparison yet, `--only <glob>` names the requests to chase.

The catalog, allowlist and baseline come from your checkout, and your
working tree, branch and baseline are not touched. The temporary worktree
is removed, also after Ctrl-C.

## Read the answer

The answer per group is a commit. It is a range when only untested
commits remain between the last good and the first bad one. A commit is
untested when, for example, the application did not start. `--format json`
gives the same for a script. On a demo repository with three deliberate
changes, neutral commits and one broken commit, bisect named the three
exact commits.

## Projects on SQLite

An SQLite state is refused. The application in the worktree opens the
worktree's own file, which DriftGate does not restore. So each commit
would run on what the earlier ones wrote. Bisect such a project by hand:
`git bisect`, with `driftgate run` at each step.

---
**Next:** [Share a baseline with your team or CI](../workflow/share-baselines.md): when the team joins.
**Related:** [driftgate bisect](../../reference/commands/review.md#driftgate-bisect) · [bisect hooks](../../reference/driftgate-yaml.md#bisect)
