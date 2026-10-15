---
name: rgaa-audit
description: |
  The playbook of the rgaa-audit bot (Acci): who finds the non-conformities
  (the ultra11y engine), who judges the criteria a static pass cannot decide
  (you, one batch at a time), who refuses (the engine's own fold, and the
  bot's citation check), and what a run leaves behind. Read it first; the
  normative text of the standard is NOT here — the engine writes it per
  criterion into the briefs you are pointed at.
---

# Acci, from the inside

## Who does what

| who | what | how it is checked |
|---|---|---|
| **the engine** (ultra11y, pinned by version and sha256) | runs the static rules on the whole scope, decides every criterion it can, harvests the evidence for the others, writes one normative brief per open criterion, folds the verdicts, renders the French report and the issue set | its own integrity checks (`check`), the engine contract test in iterion, the fingerprint re-verified before every call |
| **the rendered layer** (optional) | scans the page sample in a real browser and merges what only a rendered page can show | reported as ok / degraded / skipped in the coverage annex |
| **you**, one batch of criteria | rule on the criteria the engine left « à évaluer », from the evidence it harvested and the source you open | the engine's fold refuses an unjustified verdict, an uncited conformity, an ungroundable non-conformity or an unknown word, BY NAME; the bot also refuses a citation outside the evidence you were shown, and drops a recommendation that stands on no anchor of it |
| **the second reader** | puts every stated non-conformity and every landed conformity on trial | an incomplete trial is never applied; the engine's own findings are never pruned on a reader's say-so |
| **the operator** (optional gate) | approves, or declares criteria undecidable | recorded with their identity in the engine's `--allow-undecided` contract |
| **the board judge** | transcribes the issue plan, verbatim, de-duplicated by title | a deterministic node holds the judge's own report to the plan (every planned title created or found, nothing failed) — a self-report, not a board read |

Nothing in this bundle restates a criterion. When you need the text of a
test, read the brief the engine wrote for that criterion.

## What a batch is

The open criteria are grouped by theme (the number before the dot) into
batches of a few criteria each, and every batch is ruled on by its own agent,
in parallel. You see only your batch: the ids, where the evidence and the
briefs are, the engine's `contract`, and — on a re-ask — the reason the
engine refused the previous verdict on each id. Verdicts for ids outside your
batch are dropped.

## What happens to your verdicts

1. They are written to a fragment file keyed by your batch (never merged by
   arrival order).
2. The merge keeps, per criterion, the latest round's verdict, and sends back
   any C or NA whose citations are not among the evidence anchors of that
   criterion.
3. The fold is PROBED on a copy of the audit: every refusal the engine names
   becomes a re-ask, once. Criteria the engine marks "no subject" and that
   are still open after the probe are re-asked too, because a `manual` there
   leaves the report inconsistent.
4. The real audit is folded exactly once. What is still refused stays
   « à évaluer » and is named in the annex. Before that fold, a conformity's
   citation whose snippet is retyped or missing is given its anchor's own
   snippet and selector where the engine accepts both versions and the
   restored text grounds: the second reader's trial grounds a conformity's
   citations as stored, with no fallback to the anchor.

## What a run leaves behind

Everything intermediate lives in the run directory, outside the audited
repository. The deliverables are the dated French report, the per-page grid
when pages were captured, the coverage annex (`<standard>-<date>-couverture.md`),
the verdict ledger (replayable on the next run) and the issue plan. The
audited tree is never modified.

## The rates, so you do not quote the wrong one

- the **RGAA conformance rate** printed by the report: validated criteria
  over validated + non-conforming + still to assess, NA excluded — the
  number an auditor quotes, provisional while anything is still to assess;
- the **automatic pass rate**: what the engine decided alone, no agent;
- the **core (WCAG) rate** echoed by the engine's JSON outputs.
