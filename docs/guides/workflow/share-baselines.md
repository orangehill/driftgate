# Share a baseline with your team or CI

> **Before you start:** have a baseline in the local layout ([set up your project](../../start/your-project.md)). **Time:** 15 minutes.

By default the baseline stays on your machine. When a team or a CI job
should check against it too, it goes into git.

## Move the baseline into git

```sh
driftgate baselines git
git add driftgate.yaml driftgate/
git commit -m "DriftGate baseline"
```

`driftgate baselines git` moves the configuration, the catalog, the
allowlist and the baselines into `driftgate.yaml` and `driftgate/`.

- Before it writes anything, it checks what would enter the repository, as `verify` does.
- The key file never enters git. Give each developer machine the key in `DRIFTGATE_KEY`, or `key:`; never a job that runs for pull requests, and the Action needs none ([why](../../security.md#keep-secrets-out-of-pull-request-jobs)).
- Private bodies stay in each machine's local store; a job without them compares the rest with `--skip-private`.

What the switch does exactly is under
[driftgate baselines](../../reference/commands/project.md#driftgate-baselines).

## Protect the baselines with CODEOWNERS

```
/driftgate/baselines/ @<owner>
```

A CODEOWNERS entry makes a change of a baseline need its owner's review.
The manifest and the acceptance log catch hand edits, but they are not a
signature ([protect the baselines](../../security.md#protect-the-baselines)).
An expectation file is part of the pull request that makes its drift, so
a reviewer sees each expected drift with its ticket and reason.

## Rebase the baseline after a merge

```sh
driftgate capture main
driftgate baselines rebase local --from main --reason "main merged API-7 and API-9" --allow-freeze
```

A baseline goes stale when changes reviewed in other pull requests are
merged: every later comparison shows their drift.

1. Check out the merged code, and capture it.
2. Make that capture the baseline's content with `baselines rebase`, with a reason.
3. Commit the baseline.

The rebase logs one line in the acceptance log, and closes the
expectations the new baseline holds. In `mode: freeze` it needs
`--allow-freeze`, which says the changes were reviewed where they were
made. What the capture must be is under
[driftgate baselines rebase](../../reference/commands/review.md#driftgate-baselines-rebase).

## Hand a capture to another developer

```sh
cp -R .driftgate/store/captures/v2 ~/handover/captures/
cp -R .driftgate/store/private ~/handover/
```

To let someone else compare a capture, or check a result, on their
machine, copy into the same project on theirs:

- the capture's folder, `captures/<tag>/` of the local store;
- the baseline, `baselines/<name>/`; with `baselines: git` it is in the repository already;
- the store's `private/` folder, which holds the private bodies, encrypted with the project key. Without it, their requests are compared without the body (exit 3, or `--skip-private`).

The private bodies can be read only with the project key: give it as
`DRIFTGATE_KEY`, or `key:`, on the other machine.

- Send the key through a secure channel, such as a password manager's shared vault (`key: { op: … }`). Never send it with the files, nor in the same message, chat or archive.
- Private bodies hold personal data. Copy them only to someone who may see that data.
- Keep them out of shared drives and tickets, and remove the copy when it is no longer needed.
- Public bodies were scanned for personal data, but a scan is not a proof.

---
**Next:** [Check a live server without writing to it](live-server.md): before and after a change on a server.
**Related:** [Where DriftGate keeps its files](../../concepts/project-layout.md) · [Security model](../../security.md) · [driftgate baselines](../../reference/commands/project.md#driftgate-baselines)
