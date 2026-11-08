# Where DriftGate keeps its files

> **Before you start:** do the [tutorial](../start/tutorial.md). **Time:** 5 minutes.

DriftGate keeps a project's files in one folder, and chooses with one
key whether they stay on your machine or go into git.

## The files of a project

A new project (`driftgate init`) keeps all its files in `.driftgate/`,
which git ignores:

```
.driftgate/
  driftgate.yaml               the configuration
  routes.json                  route inventory, written by discover --write
  catalog.json                 request catalog, written by generate, edited by hand
  allowlist.yaml               values that may change, each with a format and a reason
  exclusions.yaml              routes deliberately not checked, each with a reason
  classify.yaml                column rules of the data module
  baselines/<name>/            baselines; change only through driftgate accept
  store/                       the local store: captures, private bodies, pristine metadata
  key                          the project key (driftgate key create), open to its owner only
```

Each file has a reference page; the [reference](../reference/README.md#files)
lists them.

## The local layout and the git layout

`baselines:` in `driftgate.yaml` chooses the layout.

| | `local` | `git` |
|---|---|---|
| `driftgate.yaml` | `.driftgate/driftgate.yaml` | `driftgate.yaml` at the project root |
| catalog, allowlist, routes, exclusions, baselines | `.driftgate/` | `driftgate/`, committed |
| local store | `.driftgate/store/` | `~/.driftgate/<project>/` |
| key without `key:` | `DRIFTGATE_KEY`, then `.driftgate/key` | `DRIFTGATE_KEY`, then the macOS Keychain |
| for | checks on this machine before a pull request | baselines shared through git, CI |

With the git layout, the files are committed, and the local store stays
outside the repository:

```
driftgate.yaml                 the configuration
driftgate/
  routes.json, catalog.json, allowlist.yaml, exclusions.yaml, classify.yaml
  baselines/<name>/
```

The rules of each layout, such as how DriftGate makes sure git ignores
`.driftgate/`, are under [baselines](../reference/driftgate-yaml.md#baselines).

## Which layout to choose

- **Local**, the default: you check each upgrade step on your machine before the pull request. Nothing goes into git, and each worktree has its own baseline.
- **Git**: the team or a CI job shares the baseline. `driftgate baselines git` moves the files into the repository; then commit `driftgate.yaml` and `driftgate/`.

Personal-data scanning, the encryption of private bodies and the state
fingerprint work the same in both layouts.

## The local store

The local store holds the captures, the encrypted private bodies and the
records of the pristine copies. It never enters git, not even encrypted
([store](../reference/driftgate-yaml.md#store)).

## The project key

The project key encrypts the private bodies, the response bodies that
may hold personal data. `driftgate key create` makes one; treat it like a
production password. Without it, private bodies cannot be read
([key](../reference/driftgate-yaml.md#key)).

## How commands find the project

Commands find the project from any folder inside it. They take the
nearest folder that has `.driftgate/driftgate.yaml` or `driftgate.yaml`,
up to the top of the git work tree. `--config <path>` names the file
instead.

A folder with both files is refused, with the commands that settle it.
That happens after checking out a branch from before a switch of
layouts. `.driftgate/` holds the store and the key, so it is moved aside,
never deleted.

---
**Next:** [What a green result proves](what-a-pass-proves.md): what a pass checks, and what it does not.
**Related:** [baselines](../reference/driftgate-yaml.md#baselines) · [driftgate baselines](../reference/commands/project.md#driftgate-baselines) · [Glossary](../glossary.md)
