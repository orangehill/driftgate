# Documentation style guide

How `README.md` and the pages under `docs/` are written: short, ordinary
docs for a command-line tool. The docs test (`go test ./internal/docscheck/`)
checks the rules it can.

## What goes in

Only what a user does or decides, and only once:

- commands and flags;
- keys, with one example;
- what a result means;
- limits that would lead a user to a wrong conclusion.

Leave out measured numbers and timings, how drivers work inside, why a
design was chosen, rare edge cases, and anything another page says.

## The pages

There are twelve pages, each with a line budget in
`internal/docscheck/testdata/budgets.txt`, and a `TOTAL` for all of them.
A new fact goes into the page it belongs to; a page over budget is cut,
not split. Every command has an H2 in `docs/commands.md`, and every key of
`driftgate.yaml` is named in `docs/configuration.md`.

## Writing

- At most 25 words per sentence. A code span counts as one word.
- One table rather than prose, and one example rather than three.
- Active voice, present tense, and "you".
- No heading text repeats within a page.
- No "before you start" boxes, no reading-path footers, no stub pages for old links.
- Do not use: "local mode", "snapshot", "golden file", "mutating", "note that", "in order to", "basically", "simply", "as mentioned", "please", "actually", "of course".
- Say "service" for a running application, "request" for a catalog entry, and "baseline" for stored answers.

## Code blocks

- `sh` for commands the reader types, without a `$` prompt.
- `console` for real output, starting with `$ driftgate …`.
- Every `yaml` or `json` block has a check marker on the line above it: `<!-- check: KIND [files=a,b] -->`.
- KIND is `config`, `config-fragment`, `catalog`, `allowlist`, `exclusions`, `classify`, `routes-json`, `yaml`, `json` or `skip: <reason>`.
- Another tool's format, such as a GitHub workflow, is checked as `yaml` or `json`.

## Names in examples

The repository is public: never a client's or an internal name.

- Project: `shop`. Database names: `shop`, `shop_pristine`, `shop_anon`, `shop_prod_copy`.
- Hosts: `shop.example`, `shop.test`, `localhost`, `example.com`.
- Addresses: loopback, `203.0.113.x` and `2001:db8::/32`.
- E-mail: `@shop.example`.
- Ticket refs: `SHOP-<n>`, `API-<n>`, `APP-<n>`, `DEMO-<n>`.
- Paths: `~/Data/shop…`.
