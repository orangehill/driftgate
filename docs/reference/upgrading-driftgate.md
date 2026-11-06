# Upgrading DriftGate

What to do after you update DriftGate in an existing project. Each
section names the case, what DriftGate says, and the one step that
settles it. What changed in each release is in the
[changelog](../../CHANGELOG.md).

## Baselines made before manifests

A baseline made before manifests has no `manifest.json`, and `verify`
cannot check it. Write its manifest once:

```sh
driftgate verify --baseline <name> --adopt
```

Details: [driftgate verify](commands/review.md#driftgate-verify).

## Personal data in an older baseline

An older release could keep public bodies with personal data or
credentials, or private bodies, in a committed baseline. `verify` now
fails on them (exit 1). Move them out of git once, with the project key
at hand:

```sh
driftgate verify --baseline <name> --migrate-private
```

It also names the private bodies of a baseline made before keyed refs by
a keyed hash. The values were committed: treat a credential as leaked.
Details: [What verify scans in a committed baseline](commands/review.md#what-verify-scans-in-a-committed-baseline).

## An older state fingerprint format

Triggers and views (MySQL, MariaDB) and the PostgreSQL objects are
fingerprinted in the current format. A pristine copy or a baseline whose
fingerprint has an older format keeps working while none of them
differs. Otherwise the capture stops with exit 3 and says so.

1. Run `driftgate seed`.
2. When the message names the baseline, run `driftgate verify --baseline <name> --migrate-state`.

Details: [Migrate a baseline of an earlier release](commands/review.md#migrate-a-baseline-of-an-earlier-release).

## Column collations after v1.0.2 on MySQL 8

On MySQL 8, a column can take its table's collation without naming it.
That is any such column of a table whose collation is not its character
set's default, such as `utf8mb3_unicode_ci` in a Laravel 5.5 table. A
seed and a restore now keep it that way.

Up to v1.0.2 the copy named the collation, so the copy printed
`CHARACTER SET … COLLATE …`. A restore then stopped with "left 1 items
different from the pristine copy", and the message adds that the
pristine copy no longer matches its record. That restore has already
rewritten the working tables from the copy:

1. Re-create the working database, from your migrations or your dump.
2. Run `driftgate seed`.

## Keys an earlier release refuses

A release refuses a key it does not know, so everyone working on the
project needs a release that knows the keys it uses. These keys came
later:

- `json-errors` in `templates:` ([json-errors](driftgate-yaml.md#json-errors));
- `known_failures:` in `exclusions.yaml` ([acknowledge a request that fails](exclusions-yaml.md#acknowledge-a-request-that-fails));
- the mapping form of a `seed`, `prepare` or guard command, with `run`, `dir` and `stdin` ([commands with a folder or standard input](driftgate-yaml.md#commands-with-a-folder-or-standard-input));
- `dir` and `stdin` of a catalog command step, which an earlier release refuses as unknown fields ([command steps](catalog-json.md#command-steps)).

To adopt `json-errors` in a project made before it:

1. Add it to `templates:`.
2. Run `driftgate generate --write`.
3. Run `driftgate run`, and `accept` the new requests.

`generate` and `run` say so once per local store, with a `note:`, to a
project whose `discover:` uses the `laravel`, `symfony`, `rails` or
`spring` adapter. A project that finds its routes only through `openapi`
gets no note.

## Line endings of baselines checked out before .gitattributes

`run`, `accept` and `verify` put a `.gitattributes` with `* -text` into
the baselines folder when it is missing. A checkout made before that
file may have its baseline files with CRLF line endings on Windows, and
every check then reports them as changed. Check them out again as they
were committed:

```sh
git rm -r -q --cached driftgate/baselines
git checkout HEAD -- driftgate/baselines
```

Details: [Line endings on Windows](commands/review.md#line-endings-on-windows).

## Catalogs without mutates

A catalog of an earlier release has no `mutates`, so its requests change
data only by their method. They are listed before each capture, and a
search sent as `POST` among them runs after the writes. Mark each one:
[Requests that change data only by their method](capture.md#requests-that-change-data-only-by-their-method).

---
**Related:** [Changelog](../../CHANGELOG.md) · [driftgate verify](commands/review.md#driftgate-verify) · [driftgate.yaml](driftgate-yaml.md)
