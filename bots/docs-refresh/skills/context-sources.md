---
name: context-sources
description: Reading the run's optional product-context material (reference repos, tracker snapshot) — what it grounds, how to read it per backend, and the inviolable rules (read-only, code wins, ground or mark)
---

# Context sources — reading the run's reference material

Some runs carry PRODUCT CONTEXT: reference material an operator attached
to the run (via `context_git` repos and/or a tracker board snapshot),
materialized deterministically before pass 1 under
`${scratch_dir}/context/`. When the user prompt's "Product context"
line is empty, this skill does not apply — run without it.

## What it is

- `context/<slug>/…` — shallow clones of the reference repositories
  named in `context_git`. They hold whatever the operator committed
  there: reference PDFs, functional specs, online-help exports. PDFs
  have been pre-extracted at ingest: each `<file>.pdf` has a
  `<file>.pdf.txt` sidecar next to it (`pdftotext -layout`).
- `context/jira/board-<id>.md` — the tracker board snapshot: one
  section per issue (key, type, status, summary, trimmed description).
- `context/manifest.json` — what was materialized, and any source
  that failed (a failed configured source fails the node loudly, so
  an entry here means the run refused to start under-grounded).

## What it grounds

The context describes what the product IS for its users — business
rules, vocabulary, planned behaviour, the demand side. The repo's code
describes what it DOES. The docs align with the CODE (docs follow
code, never the reverse); the context steers the ENRICHMENT half:

- what deserves a page (a Jira epic naming a user journey the docs
  never mention),
- what a cryptic code path means in product terms (an online-help
  section describing the very form the code renders),
- which business constraint explains a validation rule.

## Reading it per backend

- claude_code: the Read tool opens PDFs (paged) and any absolute path
  natively; the `.txt` sidecars read like any text file.
- claw (or any backend whose file reader is text-only or
  workspace-contained): reach the context through `bash` — `cat` the
  sidecars, `pdftotext -layout <file.pdf> -` when a sidecar is missing
  (poppler ships in the bot's devbox). The scratch directory is
  outside the workspace: `read_file` will refuse it, `bash` will not.

## Inviolable rules

1. **Read-only.** Never copy, move, or commit a context file into the
   target repository. The writeable set is `.md` in the repo
   (doc-scope-enumeration); reference material is not content.
2. **Code still wins.** A context fact the code contradicts is drift
   in the CONTEXT (stale spec, closed issue still open elsewhere) —
   do not write it into the docs; surface it in `drift_remaining`.
3. **Ground or mark.** A claim only the context supports (nothing in
   the repo verifies it) is either marked `[à confirmer]` at the
   claim or left out — the same evidential bar as every other edit.
4. **The context is not scope.** It informs the plan like the advisory
   hints do: a starting point you may contradict, never a checklist.
