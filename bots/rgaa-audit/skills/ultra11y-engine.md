---
name: ultra11y-engine
description: |
  The ultra11y engine as this bot uses it: what it decided before you were
  started, what it hands you (evidence files, normative briefs, the verdict
  contract), how its fold will judge what you return, the files of the run
  directory, and the pin. You do not call the engine yourself — the
  deterministic nodes do — but you read what it wrote.
---

# The ultra11y engine, from inside Acci

The engine is [SocialGouv/ultra11y](https://github.com/SocialGouv/ultra11y)
(MIT). The bot pins ONE version, by its number and by the sha256 of its entry
file, in `ultra11y.pin` next to the bot; the `prepare` node fingerprints a
candidate before anything of it runs, and every later call re-verifies the
file. You never run it: you hold read-only tools, and every engine call in
this bot is a deterministic node.

## The division of labour

- **Decided before you start**: every criterion the static rules can decide
  on source — missing alternatives, unlabelled fields, empty links and
  buttons, frames without a title, heading skips, invalid ARIA, and so on.
  Those findings are counted and anchored `file:line`; a second opinion from
  you is not recorded anywhere, so do not re-audit them.
- **Yours**: the criteria the engine left « à évaluer » — relevance,
  purpose, order, consistency: judgement over the evidence it harvested.
- **A real browser's** (the rendered layer, when it ran): computed contrast,
  visible focus, zoom, reflow, hover content. Items flagged
  `needsRenderedEvidence` usually belong here: without a rendered page they
  are `manual` with the reason `needs-rendered-dom`, never a guessed C.

## What the engine hands you

| file | what it is |
|---|---|
| one evidence file per criterion | the engine's harvest for one criterion: `evidence[]` (`file`, `line`, `selector`, `snippet`, `note`), `signals[]` (rule hits), `population` (`classes`, `occurrences`, `files`, `pages` — how many subjects it found), `testIds`, `needsRenderedEvidence`, `evidenceComplete` |
| one brief per criterion | the criterion's own numbered tests, the standard's testing methodology and glossary, rendered by the engine at run time — the ONLY normative text in the run |
| the `contract` in your input | the verdict vocabulary and what each verdict must carry; it wins over this page if the two ever disagree |

Your batch input lists the absolute path of every evidence file and brief
it hands you (a null brief path names a criterion the engine decided
statically): open exactly those, never a path reconstructed from memory or
rebuilt from a directory of the run.

A `population` of zeros means the engine found no subject for the criterion
in the audited scope: NA is the expected verdict, confirmed by one grep.

## How the fold judges you

The engine's `verify --apply` is the gate on your work, and it is partial:
every acceptable verdict lands, every refusal is named — a C without a
citation, an NC without a groundable finding, a NA without a justification,
a `manual` without one of the two reasons, a word outside the vocabulary.
Named refusals come back to you once, with the engine's reason. The bot
also checks that:

- a citation must be one of the anchors of THIS criterion's own `evidence[]`
  (the engine grounds a citation on the source file, which would accept a
  stray line);
- report counts and the RGAA rate agree with the audit document.
  ultra11y 5.45.0 leaves coverage gaps open without marking them NA.

The fold refuses a verdict for a criterion it already decided with structured
JSON. Verdicts outside your batch are discarded before it runs. A NA on a
criterion with harvested evidence must cite that evidence.

## The run directory

`audit-latest.json` (the pack audit: `findings[]`, `criteria[]`,
`residualRisks[]`; its root `conformancePct` is the automatic pass rate),
`items/`, `adjudicate/` (the briefs), `ADJUDICATE.todo.json` and
`ADJUDICATE.verdicts.json` (the engine's worklist and verdict skeleton),
`adjudication.json` (the merged verdicts), `fragments/` (one file per batch),
`probe/` (the folds on copies), `ledger/` (the replayed ledger, then the
merged one), `draft/` and `refute/` (the draft report and its trial),
`out/` (the business summary, technical annex and other deliverables), `layers/` (one status file per layer, the source
of the coverage annex).

## Reporting a drift

The contract this bot relies on is pinned by
`bots/rgaa_audit_engine_contract_test.go` in iterion, on the real engine.
A behaviour that differs from this page is a contract change: open an issue
on SocialGouv/ultra11y with the reproduction, and read
`docs/agents/testing/engine-contracts.md` before bumping the pin.
