# Finding the commit that introduced a drift

`driftgate bisect <good> <bad>` searches the commits between a good and a
bad commit and names the first commit for each drift group separately: one
commit may bring one change and another a different one. Checked on a demo
repository: three deliberate changes in different commits, among neutral
commits and one broken commit, gave the three exact commits.

## Configure the hooks

DriftGate checks out each commit in a separate temporary git worktree. Your
hooks put the application on it:

<!-- check: config-fragment -->
```yaml
bisect:
  checkout: ["sh", "-c", "cd \"$DRIFTGATE_BISECT_DIR\" && composer install -q && php artisan migrate --force && make restart"]
  ready: ["curl", "-sf", "http://localhost:8000/up"]
  finish: ["make", "restart"]        # at the end: back on your own checkout
  timeout: 10m                       # limit of checkout (default 10m)
  ready_timeout: 1m                  # default 1m
```

- The hooks run without a shell (write `sh -c` explicitly), from the
  project folder, with `DRIFTGATE_BISECT_DIR` (the worktree) and
  `DRIFTGATE_BISECT_COMMIT` set.
- `checkout` builds or restarts the application from the worktree; when it
  fails, the commit is skipped.
- `ready` is repeated until it succeeds; when it does not within
  `ready_timeout`, the commit is skipped.
- `finish` runs once at the end.

## Run it

At the bad commit, compare first so that bisect knows the drift groups:

```sh
driftgate run                          # exit 1: drift groups [1] … [n]
driftgate bisect v2.3.0 HEAD           # every open group
driftgate bisect v2.3.0 HEAD --group 2 # one group
driftgate bisect v2.3.0 HEAD --format json
```

- Before the search it checks that the good commit has no drift in the
  chosen groups and the bad commit has it; otherwise it stops (exit 2) and
  says which group does not fit.
- For each commit it restores the state, captures the drifted requests and
  compares them with the baseline.
- Without a comparison yet, `--only <glob>` names the requests to chase,
  and the groups are found at the bad commit.

The answer per group is a commit, or a range when only commits that could
not be tested (the application did not start) remain between the last good
and the first bad one.

Your working tree, branch and baseline are not touched; the temporary
worktree is removed, also after Ctrl-C. The catalog, allowlist and baseline
come from your checkout.
