---
name: ultra11y-engine
description: |
  How to drive the ultra11y accessibility engine from inside this bot: the
  division of labour between the static engine and your judgment, the files
  a run leaves in the run directory, the subcommands and flags that matter,
  the exit codes, and the two gates that will refuse your work. Read this
  before touching the worklist.
---

# The ultra11y engine, from inside Ally

The engine is one self-contained `.mjs` — no install, no API key, Node ≥ 22.18.
The bot resolved a **pinned** version before you were started; the exact
invocation was handed to you as `engine_cmd`. Use that string, never a bare
`ultra11y` or an unpinned `npx`.

```sh
npm_config_update_notifier=false        # npm's update notice otherwise lands in parsed stdout
<engine_cmd> --help                     # the full surface
```

## The division of labour — and why you exist

Of the 55 WCAG 2.2 AA success criteria:

| who decides | how many | what |
|---|---|---|
| **the engine**, statically | the machine-detectable ones | missing `alt`/`lang`/`title`, unlabelled fields, empty links/buttons, icon-only controls, iframes without title, tables without headers, heading-level skips, duplicate ids, invalid ARIA, positive `tabindex`, autoplay/`blink`/`marquee` |
| **you**, from harvested evidence | ~38 | alt-text *relevance*, link purpose in context, reading and tab order, caption accuracy — judgment, not pattern-matching |
| **a real browser** (`scan`) | 14 | computed contrast, visible focus, zoom/reflow, content on hover |

The engine's half is already done and counted when you start. **Do not re-audit
it.** A defect it found is anchored `file:line` with a stable id and a
criterion; you cannot improve that by looking again, and a second opinion from
you is not recorded anywhere.

The third row matters as much as the first: this bot launches **no browser**.
Those 14 criteria are not yours to decide either. They are `manual` with reason
`needs-rendered-dom`, and the report prints them as residual risks.

## The run directory

Everything intermediate lives in `run_dir`, outside the audited repository —
the engine contains its writes to `--out`, so an audit leaves the target
checkout byte-identical. What you will find there:

| file | what it is |
|---|---|
| `audit-latest.json` | the `AuditResult`: `findings[]` (stable `findingId`, `criteriaId`, `severity`, `file`, `line`), `criteria[]`, `residualRisks[]`, `conformancePct` |
| `ADJUDICATE.todo.json` | **your worklist** — one item per criterion the engine could not decide, pre-loaded with evidence, plus the `contract` you must satisfy |
| `ADJUDICATE.md` | the same worklist, readable |
| `adjudication.json` | what **you** write back |

`conformancePct` is the **automatic static pass rate**, not a conformance rate.
Never present it as "the site is N% accessible".

## Reading the evidence

Each worklist item arrives with the evidence the engine already harvested for
that criterion — every image's alt value, every link's text plus its nearest
heading, literal colour pairs, control labels. Each carries `file`, `line`,
`selector` and a `snippet`.

Start there. Open the cited file with `read_file` when the snippet is not
enough to rule — for a link whose purpose depends on the surrounding sentence,
or an alt attribute whose relevance depends on what the page is about, it
usually is not. `glob` and `grep` are available for tracing a component to its
definition.

When the `mcp__ultra11y__*` tools resolve, they are the same engine without the
shell round-trip: `ultra11y_criteria` looks a criterion up offline (its official
wording, its tests, its automatability class), `ultra11y_read` reads source, and
`ultra11y_adjudicate` re-renders the worklist. If they are absent, `bash` with
`engine_cmd` does all of it — nothing about the run depends on MCP.

## Useful subcommands

```sh
<engine_cmd> criteria 1.4.3                  # one success criterion, offline
<engine_cmd> criteria --standard rgaa --theme 8   # a country pack's theme
<engine_cmd> criteria --standard rgaa --glossary "pertinent"   # what the standard DEFINES
```

The glossary matters more than it looks: under a country standard, words like
« pertinent » or « si nécessaire » have *normative* definitions, and a test
turns on them. Look the term up rather than reasoning from the everyday sense.

## Exit codes

| | 0 | 1 | 2 |
|---|---|---|---|
| `audit` | reported, or clean | findings ≥ `--fail-on` | usage / bad input |
| `verify --apply` | folded | **your adjudication was refused** | usage / bad input |
| `check` | the report is sound | integrity failure | report missing / bad input |

Read the exit code from the process, not from a pipeline: `cmd --json | head`
reports `head`'s status, which is how a failing gate reads as a pass.

## The two gates that will refuse you

Both are deterministic tool nodes in this bot. Neither is a model, neither can
be talked round, and both run after you finish.

1. **`verify --apply`** — fail-closed on your adjudication. A null verdict, a
   `C`/`NA` with no justification, an `NC` whose finding does not ground or
   cites no normative test, a criterion missing entirely: each is refused by
   name and the run stops. Nothing you wrote reaches a report.
2. **`check`** — integrity of the produced report. Every cited criterion must
   resolve, every NA must be justified, the pass-rate maths must be sound. A
   report citing an invented criterion cannot ship.

They exist because a report is an auditor's deliverable someone will rely on.
Treat them as the floor, not the target: they can catch a fabricated citation,
they cannot catch a `C` you did not actually verify. That one is on you — see
`ultra11y-adjudication`.
