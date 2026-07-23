---
name: doc-verification-checklist
description: Per-pass verification checklist for Doki v3's adaptive campaign and deterministic truth gates.
---

# Documentation verification checklist

Run this checklist during each campaign pass and once more before reporting
the termination contract.

## 1. Read the pass state

Read `hints_note` and the advisory hints (each `{doc, line, kind, value,
note}`; kinds: `missing_path`, `dead_link`, `dead_anchor`,
`unmentioned_area`). They are telemetry-grade help, not your working set:
counts never gate the run, and an empty or degraded report ("nothing
scannable") says nothing about alignment.

Read `fail_log` before new work. It contains either a scope violation or the
previous deterministic verification failure. Also inspect `git log` so you do
not repeat documents committed by an earlier pass, and check the dismissals
ledger to see what earlier passes already settled.

## 2. Build a document-grouped todo

Group your findings by document — the hints you accept, plus everything your
own survey of the docs and code surfaces. Prioritise high-signal drift and
paths near `recently_changed_code_files`, then work the list. Re-prioritise
as you learn; the list is yours, no pipeline decides it for you.

Then explore beyond the hints: skim each doc against the code it describes,
run the entry points' help output, diff recent changes. The scan only sees
paths and links; wrong defaults, stale behaviour descriptions, outdated
examples, and missing capability docs are yours to find.

## 3. Adjudicate every issue

For each issue:

1. read the cited document line in context;
2. resolve the current implementation, declaration, command, target, or test;
3. decide `fix`, `dismiss`, `promise`, or `code bug` (the four outcomes —
   see the playbook);
4. retain reproducible evidence for the final summary.

Never infer removal solely from an empty grep if a rename or generated
surface could explain it. Search likely registrations, callers,
history-facing migration notes, and runtime help as appropriate.

## 4. Make and prove the smallest correction

For a real documentation mismatch:

- change the claim to the exact verified behavior;
- search all scoped Markdown for old names and cross-references;
- validate links, heading anchors, commands, defaults, and snippets affected
  by the edit;
- review the diff for accidental scope or style churn;
- commit the aligned document with the required semantic message and
  `Bot: docs-refresh` trailer.

For a false positive, do not edit merely to silence a hint — record the
dismissal in the ledger with the reason.

For a code bug, do not align the document to broken behavior. Set
`is_code_bug=true`, describe it in `human_note`, and use the findings handoff.

## 5. Check scope and verification risk

Before finishing the pass:

- inspect changed paths: only `.md` files may have changed;
- undo anything named by a prior scope failure.

`scope_check` is deterministic and runs after the campaign, so do not claim
its result in advance. Leave the tree in a state it can prove.

## 6. Fill the termination contract truthfully

- `docs_aligned`: true only when a fresh survey of the docs against the code
  would find no remaining real drift and no significant missing
  documentation — everything surfaced is fixed (committed), dismissed to the
  ledger, or recorded as a promise; false when real work remains.
- `commits_this_pass`: exact number of commits created in this pass.
- `drift_remaining`: concise remaining work, empty only when aligned.
- `is_code_bug`: true if at least one doc-right/code-wrong case was found.
- `needs_human` and `human_note`: only for an actual operator decision.
- `summary`: documents committed, evidence used, dismissals recorded,
  findings handed off, and relevant pre-existing failures.

The deterministic gate converges only when `docs_aligned` is true and
`scope_check.scope_ok` is true — nothing else (a docs-only bot can't break
the build, so there is no build gate). If either condition fails, the
continuation loop sends a fresh advisory report or `fail_log` to the next
campaign pass.
