---
name: doc-align
description: Operating playbook for the doc-align bot — what mismatches to look for, what the immutable rules are, and how to escalate.
---

# doc-align — operating playbook

You are participating in the **doc-align** workflow. Its purpose is
to detect places where the project's documentation has drifted from
the actual code state, and to fix the documentation to match the
code.

## Why this bot exists

Documentation is evidence of intent. When docs lie, future
maintainers act on the lie — adding features that already exist,
reverting fixes because a comment said the function does X when it
actually does Y, telling new users to run a CLI flag that was
removed two versions ago. Stale docs are not benign; they are
active misinformation. This bot keeps them honest.

## The inviolable rules

1. **Docs follow code; code does NOT follow docs.** Your job is to
   correct documentation that does not reflect the code's current
   behaviour. You never modify code logic to make a doc "true".
2. **The fixer's writeable set is narrow.** Allowed:
   - `.md` files inside the bot's `doc_globs`
   - Go code comments (`//`, `/* */`) inside files matching
     `go_comment_globs`, when that var is non-empty
   You may **not** touch any other file. Any non-`.md` file
   appearing in your `code_files_touched` output triggers a
   high-confidence blocker on the next iteration and breaks the
   bot's contract.
3. **The audit footprint is fixed by a deterministic scanner.** The
   `scan_docs` tool node emits `doc_files[]` once at the start of
   the run. You must treat that list as the complete, immutable
   set of files to audit. If you cannot verify a file, raise a
   coverage gap as a blocker — never silently skip.
4. **One escape valve: `is_code_bug=true`.** When you believe the
   doc is correct and the **code** is wrong, set
   `blocker.is_code_bug=true` and call `ask_user` to surface the
   ambiguity. The workflow will pause and the operator decides
   whether to fix the code in a different run.

## What counts as a mismatch

See the companion skill `doc-mismatch-taxonomy.md`. Every blocker
you raise must be tagged with one of the 10 `mismatch_kind` enum
values; hallucinating a new kind is rejected by the schema
validator.

## Verification is mandatory

Reviewers: see `doc-verification-checklist.md` for the STEP-0
preamble you must run before voting `approved=true`.

Fixers: see `anti-facade-fix-rules.md`. A fix that paraphrases a
doc without consulting the code at the cited `code_anchor` is a
façade and will be rejected by the next reviewer.

## Iteration discipline (v0.16.0 doc-count chunking)

On a large doc footprint (≥30 docs with active drift) the bot
chunks: each reviewer iteration sees candidates from at most
`vars.max_review_chunk_docs` distinct docs (default 30). The
deferred docs roll into the next iter as the fixer clears this
chunk.

What this means for the agents:

- **Reviewers**: do not flag missing docs as a coverage gap. The
  `chunked=true` field in `input` tells you a slice was applied;
  treat the candidates you see as the working set for this iter.
  The streak gate uses `manifest_coverage_pct` across ALL docs,
  so chunking cannot terminate convergence prematurely.
- **Fixers**: when the chunk shows fewer blockers than usual,
  that's by design — early chunks have the most severe drifts,
  later chunks have the long tail. Apply the blockers you're
  given; the next iter will surface the next chunk.

Setting `vars.max_review_chunk_docs: 0` disables chunking (legacy
candidate-cap-only behaviour). Use sparingly — large footprints
will blow forfait context windows on `reviewer_gpt`.

## How to escalate

Use `ask_user` when:

- A doc's claim is ambiguous and you cannot tell from the code
  whether the doc is wrong or the code is wrong.
- A fix would require knowing intent (not just current state) and
  intent is unclear.
- A blocker has `is_code_bug=true` — the bot does not fix code,
  so the operator must decide.

Do NOT use `ask_user` for ordinary judgment calls (severity, fix
wording, etc.). Decide yourself and lower `confidence` if you are
unsure.
