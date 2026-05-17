---
name: doc-verification-checklist
description: STEP-0 preamble for doc-align reviewers — what to verify before voting approved=true and how to defeat façade fixes.
---

# Doc verification checklist — reviewers' STEP-0 preamble

Before you emit any verdict (especially `approved=true`), run this
checklist in order. Treat it as the load-bearing discipline of the
workflow — if you skip steps, the false-positive feedback loops
break down and the workflow converges on plausible-looking
nonsense.

## STEP 0 — Treat `input.doc_files[]` as the audit footprint

`input.doc_files[]` was produced by the deterministic `scan_docs`
tool node. It is NOT a suggestion; it is the contract.

- Echo `len(doc_files)` and `footprint_hash` in your reasoning
  notes (informally — proof that you looked at the actual scanner
  output, not your imagination of it).
- Build a working set: which entries in `doc_files` are NOT yet
  in `input.previous_audited_pairs` (the cumulative coverage from
  prior iterations)?

## STEP 1 — Audit uncovered files first

For each file in your working set (not-yet-audited entries of
`doc_files[]`), open it, read it, and check claims:

- Does each CLI command shown in the doc still exist in the code?
  Run `grep -RIn '"<flag-name>"' cmd/ pkg/cli/` or equivalent.
- Does each filepath or directory mentioned in prose still exist?
  Run `test -e <path> && echo yes || echo no`.
- Does each link target exist? `for l in <links>; do test -e
  $WORKSPACE/$l || echo BROKEN $l; done`.
- Does each function signature, default value, or behaviour claim
  match the code? Find the cited `code_anchor`; read it.
- If `go_comment_globs` is non-empty: do function comments still
  match what the function does?

For every (doc_path, code_anchor) pair you verified — pass or
fail — append `"doc_path::code_anchor"` to your output's
`audited_pairs[]`.

## STEP 2 — Re-verify any fixes from this iteration

If the prior iteration's fixer modified docs, you must re-grep
the cited `code_anchor` for each fix AFTER the fix and confirm
the new doc text matches the code:

1. For each blocker that `outputs.fix_*.summary` claims was
   fixed: read the doc at the fixed location.
2. Read the code at the cited `code_anchor`.
3. Ask: does the new doc text accurately reflect the current
   code? If only the wording changed but the doc still doesn't
   match the code, the fix is a façade — re-raise the blocker
   with `severity: high` and a note `"FAÇADE: <previous blocker
   description> — fix changed wording but doc still does not
   match code at <code_anchor>"`.

## STEP 3 — Mechanical code-touch check

Examine `outputs.fix_claude.code_files_touched` and
`outputs.fix_gpt.code_files_touched` from the most recent fix
iterations. Both fields MUST be `[]`.

If either is non-empty, raise an automatic blocker with:

- `mismatch_kind`: any value (use `stale_behavior_description`
  by default)
- `severity: high`
- `confidence: high`
- A note in `blockers[].suggested_fix`: `"Workflow contract
  violation: fixer touched <files>. Revert those files; doc-align
  must never modify code."`

## STEP 4 — Coverage gate on approval (v0.2.0 mechanical)

You may vote `approved=true` ONLY if:

- `len(blockers) == 0` after STEP 2's façade check
- All prior fix iterations have `code_files_touched == []` (the
  G4 gate from STEP 3)
- Your `audited_docs[]` plus `input.previous_audited_docs[]`
  cover the doc files you actually verified this iteration

The cumulative file-level coverage is **mechanically enforced
downstream**: the `streak_check` compute node calculates
`coverage_pct = unique(prior + this iter's audited_docs) * 100 /
doc_count` and blocks `stop=true` (workflow convergence) when
`coverage_pct < coverage_target_pct` (default 80%).

That means: if you vote `approved=true` on a partial audit (e.g.
17 of 51 docs verified, coverage 33%), the workflow will NOT
terminate — it alternates to the other family which is expected
to extend coverage. v0.1.0 lacked this gate; reviewer_claude
approved with ~13/51 audited and the streak nearly armed.

Your job here: be honest about `audited_docs`. List exactly the
doc files you verified this iteration. Don't pad the list; don't
omit a file you actually opened. The mechanical gate trusts the
union grow truthfully over iterations.

`input.coverage_pct` tells you the cumulative percentage BEFORE
your current iteration is folded in. Use it to plan: if coverage
is at 60% and the target is 80%, you need to add ~10 files (of
51) to `audited_docs` this iteration to meet the gate.

## STEP 5 — Self-critique

For each blocker you're about to emit, ask:

- Could a maintainer reading this find the cited `code_anchor`
  and verify the mismatch in under 60 seconds? If not, sharpen
  the `code_anchor` and `code_state` fields.
- Is this a real misalignment, or a style preference? Style
  preferences are not blockers — drop them or demote to
  informational notes you keep out of `blockers[]`.
- Could the doc be right and the **code** be the problem? If
  yes, set `is_code_bug=true` so the fixer escalates rather
  than rewriting a correct doc.

## STEP 6 — Confidence assessment

Rate your overall verdict's confidence:

- `high` — you can point at exact lines for every blocker, every
  audited pair was actually grepped, every code_anchor resolves.
- `medium` — strong evidence for blockers but you didn't audit
  every uncovered file (so you can't yet vote approved=true).
- `low` — intuition or impression-level; no concrete blockers
  you'd defend. Low-confidence rejections are treated as
  soft-approvals by `streak_check` — alternation continues without
  invoking the fixer.

Now and only now, emit the verdict.
