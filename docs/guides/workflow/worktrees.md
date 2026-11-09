# Work on each change in its own worktree

> **Before you start:** have a project with a baseline in the main checkout ([set up your project](../../start/your-project.md)). **Time:** 10 minutes per worktree.

A git worktree per change, such as one per upgrade step, lets you check
each change against the same baseline, side by side.

## Set up a worktree

```sh
git worktree add ../shop-api-7 -b api-7
cd ../shop-api-7
driftgate setup
```

Setup in the new worktree takes the main worktree's project and
baseline. `driftgate run` there then compares the worktree's code with
the main worktree's baseline. This needs the local layout; what setup
takes, and when it stops, is under
[setup in a git worktree](../../reference/commands/project.md#setup-in-a-git-worktree).

## Give the worktree its own database and address

```ini
# .env of the worktree
APP_URL=http://127.0.0.1:8001
DB_DATABASE=shop_api7
```

Give the worktree's application a database and an address of its own,
in its own settings, before you run setup there. Setup then asks to use
them (`--yes` agrees), and makes that database's pristine copy. Without them it stops: its runs
would restore the main worktree's database.

## Carry expectations to another worktree

```sh
driftgate expect --import ../shop-api-7/.driftgate/baselines/local.expectations/API-7.jsonl
```

Each worktree has its own `.driftgate/`, so a ticket's expectations stay
where they were recorded. `expect --import` takes a ref's file into this
worktree, after checking its chain, seal and baseline
([take expectations from another worktree](../../reference/commands/review.md#take-expectations-from-another-worktree)).

## SQLite files in a worktree

```sh
driftgate setup        # offers to copy the main worktree's pristine data into the new file
```

Git does not carry an SQLite file. Name a file of the worktree's own in
its settings, and setup offers to fill it from the main worktree's
pristine data ([SQLite in setup](../../reference/commands/project.md#when-setup-finds-an-sqlite-file)).

---
**Next:** [Share a baseline with your team or CI](share-baselines.md): when more people check against it.
**Related:** [Setup in a git worktree](../../reference/commands/project.md#setup-in-a-git-worktree) · [Where DriftGate keeps its files](../../concepts/project-layout.md)
