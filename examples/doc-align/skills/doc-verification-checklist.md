---
name: doc-verification-checklist
description: STEP-0 preamble for doc-align reviewers — how to triage the deterministic drift manifest before voting approved=true.
---

# Doc verification checklist — reviewers' STEP-0 preamble

Before you emit any verdict (especially `approved=true`), walk this
checklist in order. Under v0.15.0 the workflow's enumeration phase
is deterministic (the `build_manifest` tool); your job is the
semantic pass over the candidates it could not auto-resolve. Skip
steps at your peril — the cross-family alternation and the
coverage gate both rely on this discipline.

## STEP 0 — Read the manifest summary

`input.total_anchors`, `input.verified_anchors`,
`input.drifted_anchors`, `input.unverifiable_anchors`, and
`input.manifest_coverage_pct` describe the deterministic state of
the doc/code alignment. Note them in your reasoning; they ground
every subsequent judgment.

The mechanical coverage is `verified_anchors * 100 / total_anchors`
— if it's below `input.coverage_target_pct`, the convergence gate
won't fire even if you vote `approved=true`. Push the fixer toward
resolving drifts.

### Chunk awareness (v0.16.0)

Three further fields tell you whether this iter is seeing the
entire drift set or a chunk of it:

- `input.docs_with_drift_count` — how many distinct docs still
  carry at least one drift candidate (before chunking).
- `input.chunk_doc_count` — how many distinct docs landed in
  this iter's slice.
- `input.chunked` — `true` when the chunk excluded some docs
  (i.e. `chunk_doc_count < docs_with_drift_count`).
- `input.max_review_chunk_docs` — the active cap (default 30).

When `chunked=true`, the candidates you see are the highest-
severity slice; the deferred docs roll into the next iter as the
fixer clears this chunk. This is not a coverage gap to flag —
the workflow expects multi-iter convergence on large doc
footprints. Your job is unchanged: adjudicate the chunk you have.
The STEP 6 coverage gate uses `manifest_coverage_pct` which spans
ALL docs, so chunking cannot terminate the workflow early.

## STEP 1 — Walk `input.drift_candidates`

Each entry is `{doc, line, kind, value, status, evidence, excerpt}`.
You must decide one of three actions per candidate:

- **confirm** — real drift. Raise a blocker pointed at
  `doc::value` with a `suggested_fix` that edits the DOC (never the
  code). Severity = high for drifted CLI surface / diagnostic codes
  (deterministic miss, high signal); medium for drifted file_ref;
  low for unverifiable symbol_ref unless the doc context makes the
  drift obvious.
- **dismiss** — false positive. Skip the blocker; do NOT add to
  `audited_pairs`. The manifest will keep listing it but the bot
  won't act on it. Add the candidate to `audited_pairs` if you
  actively investigated it (i.e. spent a tool call confirming the
  dismiss), so the cross-family reviewer can spot-check.
- **code_bug** — the doc is correct, the code is wrong (e.g. a
  capability the doc describes was removed in error). Raise a
  blocker with `is_code_bug: true` and `escalate_to_human: true`
  in your output; the fixer will refuse and call `ask_user`.

Always add the candidate to `audited_pairs` as `doc::value` when
you take action B or C, and (encouraged) when you actively
investigated A. The cross-family reviewer's STEP-3 spot-check
randomises 3 entries from your `audited_pairs` and re-greps them.

## STEP 2 — Spot-check the top drifts

Your `tool_max_steps` budget is 25 (v0.15.0). Spend it on the
candidates with the highest stakes:

1. **Drifted `cli_command` / `cli_flag` / `diagnostic`** — the
   manifest verified these against `scan_code_surface`'s output,
   which is itself a deterministic scan. A miss here almost always
   means the doc names something that doesn't exist (rename,
   removal, typo). Read the cited doc line to confirm the context;
   then grep the live code to confirm the absence.
2. **Drifted `file_ref` for paths in active code roots** — a
   missing `pkg/foo/bar.go` mentioned in `docs/architecture.md` is
   nearly always real drift (rename, refactor). For `examples/`
   refs, check `.archive/examples/` before raising.
3. **Unverifiable symbol_ref in prose-heavy docs** — usually a
   false positive. Spot-check 1-2 to confirm before bulk
   dismissing the category.

Do NOT enumerate — that's the manifest's job. Do NOT read entire
docs to verify a single anchor — read the cited line ±5.

## STEP 3 — Adversarial spot-check of prior audits

Before voting `approved=true`, pick 3 random entries from
`loop.review_loop.previous_output.audited_pairs` (the previous
reviewer's claims, possibly from a different family) and re-grep
the cited `value` yourself. Confirm the previous reviewer's
verdict held.

If your spot-check finds drift the previous reviewer missed:

- The previous reviewer's verification was a façade or sloppy.
- Raise a blocker with `severity: high` and `suggested_fix`
  describing the gap: e.g. "FAÇADE-SPOT-CHECK: the previous
  review claimed `docs/foo.md::Handler` was verified, but
  `Handler` no longer matches in `pkg/bar.go`."

The cost is 3 grep calls; the alternation honesty mechanism
becomes mechanical instead of statistical.

## STEP 4 — Mechanical code-touch check (G4 gate)

Examine `input.prior_code_files_touched_claude` and
`input.prior_code_files_touched_gpt`. Both fields MUST be `[]`.

If either is non-empty, raise an automatic blocker with:

- `mismatch_kind`: any value (use `stale_behavior_description`
  by default)
- `severity: high`
- `confidence: high`
- `suggested_fix`: "Workflow contract violation: fixer touched
  <files>. Revert those files; doc-align must never modify code."

## STEP 5 — Scope-honesty check (v0.13.0)

`input.cumulative_chronic_paths` lists doc paths the fixer has
tried to silently edit ≥3 times despite the scope-enforcer
reverting them. If non-empty:

- Raise ONE blocker per element with `mismatch_kind:
  stale_behavior_description`, severity:high, suggested_fix
  pointing at the path and noting "chronic out-of-scope edit;
  fixer must drop this file from its working set."
- If empty: stay silent on the topic. Do NOT pre-emptively warn
  the fixer about scope — the gate by construction defers to the
  ≥3-revert threshold.

## STEP 6 — Coverage gate on approval

You may vote `approved=true` ONLY if:

- `len(blockers) == 0` after STEP 1's candidate pass.
- `input.prior_code_files_touched_*` are both `[]` (G4 gate from
  STEP 4).
- `input.manifest_coverage_pct >= input.coverage_target_pct`
  (mechanical, computed from the manifest's
  verified_anchors / total_anchors ratio — you cannot game it).

If coverage is below threshold but blockers is empty, you've hit
the "deterministic miss" case: the manifest still finds drifts but
none rose to a blocker in your judgment. Vote `approved=false` with
`confidence: low` so the streak gate treats this as a soft-approval
and alternates without invoking the fixer — the cross-family
reviewer gets a fresh look.

## STEP 7 — Anchor consistency self-check

For each blocker you're about to emit, confirm the
`(anchor_kind, code_anchor)` pair is self-consistent per the
table in `doc-mismatch-taxonomy.md`:

- `symbol`     → anchor MUST contain `<path>:<identifier>`
- `line_range` → anchor MUST end with `:N` or `:N-M`
- `removed`    → anchor MUST mention removal (e.g.
                `<no longer exists>`)
- `external`   → anchor MUST be a path/URL without `:N` line
                markers

Drop blockers you can't classify cleanly — a malformed blocker
routes a fix-iteration session for nothing.

## STEP 8 — Self-critique + confidence

For each blocker:

- Could a maintainer find the cited `code_anchor` and verify the
  mismatch in under 60 seconds? If not, sharpen the fields.
- Is this a real misalignment, or a style preference? Style
  preferences are not blockers.

Rate your confidence:

- `high` — every blocker has exact line evidence, every
  spot-checked anchor resolved.
- `medium` — strong evidence for blockers but you didn't audit
  every candidate (so you can't vote approved=true).
- `low` — intuition-level. Low-confidence rejections are
  treated as soft-approvals by `streak_check`; alternation
  continues without invoking the fixer.

Now and only now, emit the verdict.
