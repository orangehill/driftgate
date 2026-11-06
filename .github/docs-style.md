# Documentation style guide

How the pages under `docs/` are written. The readers are developers who
use DriftGate for the first time, and people who look one fact up. The
docs test (`go test ./internal/docscheck/`) checks the rules it can.

## Where a page belongs

- `docs/start/`: install, the tutorial, the first project.
- `docs/concepts/`: ideas and principles.
- `docs/guides/<group>/`: tasks, step by step.
- `docs/reference/`: every key, flag and rule, complete and precise.
- `docs/capabilities.md`, `docs/troubleshooting.md` and `docs/glossary.md` link into the rest.

## Example first

- Every guide section starts with the command or config block, then explains it.
- Every reference section for a file key starts with a minimal example.

## Sentence limits

- Guides, start and concept pages: at most 25 words per sentence.
- Reference pages and `docs/security.md`: at most 35 words per sentence.
- Paragraphs: at most 4 sentences in guides, 6 in reference.
- Words outside code spans count as written. A code span counts as one word.

## One idea per bullet

- A bullet holds one fact or one step.
- No bullet has more than two sentences.
- Lists nest at most one level.

## Tables

- Only in reference pages and the three index pages.
- Cells have at most 20 words. A longer explanation becomes an H3 under the table.
- No tables in guides, except the framework list in the OpenAPI guide.

## Headings

- H1 is the page title. A guide's title is a task; a reference page's title is the file or command name.
- H2 and H3 are tasks or questions in plain words. A command's reference section is exactly `## driftgate <command>`.
- No heading text repeats within a page, so no anchor gets a `-1`.
- Every key, flag or behaviour a reader may search for has its own heading, never only a table cell or a bold line.
- No bold pseudo-headings, and no numbers in headings.
- Published heading text is frozen: other pages and released binaries link its anchor. A rename keeps the old heading as a stub and adds it to `internal/docscheck/testdata/public-anchors.txt`.

## Code blocks

- `sh` for commands the reader types, without a `$` prompt. A comment after `#` says what the line does.
- `console` for real output: lines start with `$ driftgate …`, then the output. Cut lines with `...` on a line of their own. Output comes from a real run, or is marked `abridged`.
- Every `yaml` or `json` block has a check marker on the line above it: `<!-- check: KIND [files=a,b] -->`.
- KIND is `config`, `config-fragment`, `catalog`, `allowlist`, `exclusions`, `classify`, `routes-json`, `yaml`, `json` or `skip: <reason>`.
- Use `skip` only for another tool's format: a compose file, a GitHub workflow, an OpenAPI document, `appsettings.json`.
- `sql` for statements, `ini` for `.env` files.
- Placeholders are written `<name>` and are never real values.
- Long command lines wrap with ` \`.

## Opening and closing blocks

- Guides and start pages open, right under the H1, with one blockquote: `> **Before you start:** <prerequisite with a link>. **Time:** <n> minutes.`
- Every page ends with a rule (`---`) and bold lines, not headings, so they add no anchors:
  - `**Next:** [Title](path): <why, at most 12 words>`, on guides and start pages;
  - `**Related:** [A](path#anchor) · [B](path#anchor)`, at most 4 links on reference pages.
- A guide's Related line has at least one reference link. A reference section that has a guide links back with one line under its heading: `Task: [guide title](path)`.

## Say it once

- Every fact has one home: its reference page, or the concept page for an idea.
- Elsewhere, write at most one summary sentence and a link.
- Never copy a table, a list of values, an error-code list, a grant statement or a version list.

## Words

- Use the terms of the [glossary](../docs/glossary.md), with the meanings given there.
- Never "local mode": write "the local layout", or "the baseline `local`".
- "Service" in prose; `target` and `targets:` only as keys.
- "A request that changes data", not "a mutating request".
- Cut words that carry nothing: "note that", "it is important to", "in order to", "basically", "simply", "just", "as mentioned", "please", "actually", "of course".
- Active voice, present tense, and "you" in guides.

## Names in examples

The repository is public: never a client's or an internal name.

- Project: `shop`. Database names: `shop`, `shop_pristine`, `shop_anon`, `shop_prod_copy`.
- Hosts: names reserved for examples and tests, such as `shop.example`, `shop.test` or `localhost`, and `example.com`, `example.net` or `example.org`.
- Addresses: loopback, `203.0.113.x` and `2001:db8::/32`.
- E-mail: `@shop.example`, or `@….invalid` where DriftGate makes them.
- Ticket refs: `SHOP-<n>`, `API-<n>`, `APP-<n>`, `DEMO-<n>`.
- Paths: `~/Data/shop…`, `~/.config/driftgate/…`.

## Edge cases belong in the reference

- In a guide, anything with "unless", "except", a version range, an error code or a platform caveat goes to the reference.
- The guide links it: `Details: [title](path#anchor)`.

## Line budgets

- Start pages fit one screen (80 lines); the tutorial has 170.
- Guides have at most 150 lines, framework guides 180.
- Index and stub pages have at most 120 lines; reference pages have no limit.
- `internal/docscheck/testdata/budgets.txt` holds the budget of each page. A page over budget is split, or its detail moves to the reference.
