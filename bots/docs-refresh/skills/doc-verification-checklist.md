---
name: doc-verification-checklist
description: Per-pass verification checklist for Doki v2's adaptive campaign and deterministic convergence gates.
---

# Documentation verification checklist

Run this checklist during each campaign pass and once more before reporting the
termination contract.

## 1. Read the pass state

Record the manifest totals: documents, total anchors, verified anchors, drifted
anchors, unverifiable anchors, and `manifest_coverage_pct`. Mechanical coverage
is:

```text
verified / (verified + drifted)
```

Unverifiable anchors are deliberately excluded from that denominator. The gate
still requires coverage to meet `coverage_target_pct`, so real drift must be
fixed rather than merely dismissed in prose.

Read `fail_log` before new work. It contains either a scope violation or the
previous deterministic verification failure. Also inspect `git log` so you do
not repeat documents committed by an earlier pass.

## 2. Understand chunking

Compare `docs_with_drift_count`, `chunk_doc_count`, `chunked`, and
`max_review_chunk_docs`.

- With `chunked=false`, the bounded candidate list is the current working set.
- With `chunked=true`, only a severity-prioritised slice of documents is shown;
  deferred documents will surface after this slice is cleared.

Chunking is not evidence that deferred docs are clean. Do not report global
alignment while known documents remain outside the slice.

## 3. Build a document-grouped todo

Group all `drift_candidates` by `doc`, preserving the useful manifest fields:
`line`, `kind`, `value`, `status`, `evidence`, and `excerpt`. Prioritise
high-signal CLI/diagnostic drift and paths near
`recently_changed_code_files`, but finish all candidates in the pass.

Do not re-enumerate the whole repository as a substitute for the manifest. Do
broaden a search when verifying a moved symbol, replacement path, or stale
cross-reference.

## 4. Adjudicate every candidate

For each candidate:

1. read the cited document line in context;
2. resolve the current implementation, declaration, command, target, or test;
3. decide `fix`, `false positive`, or `code bug`;
4. retain reproducible evidence for the final summary.

Treat statuses correctly:

- `drifted`: mechanical verification failed; confirm context, then normally
  correct the document.
- `unverifiable`: extraction could not decide; investigate, but leave valid
  prose, archives, illustrative identifiers, and external concepts unchanged.

Never infer removal solely from an empty grep if a rename or generated surface
could explain it. Search likely registrations, callers, history-facing
migration notes, and runtime help as appropriate.

## 5. Make and prove the smallest correction

For a real documentation mismatch:

- change the claim to the exact verified behavior;
- search all scoped Markdown for old names and cross-references;
- validate links, heading anchors, commands, defaults, and snippets affected by
  the edit;
- review the diff for accidental scope or style churn;
- commit the aligned document with the required semantic message and
  `Bot: docs-refresh` trailer.

For a false positive, do not edit merely to silence the extractor. Explain why
the value is valid in `summary`.

For a code bug, do not align the document to broken behavior. Set
`is_code_bug=true`, describe it in `human_note`, and use the findings handoff.

## 6. Check scope and verification risk

Before finishing the pass:

- inspect changed paths against the scanned Markdown footprint;
- ensure any opted-in Go edit changes comments only and matches
  `go_comment_globs`;
- undo anything named by a prior scope failure;
- distinguish new verification failures from the declared or cheaply
  established baseline.

`scope_check` and `verify_run` are deterministic and run after the campaign, so
do not claim their result in advance. Leave the tree in a state they can prove.

## 7. Fill the termination contract truthfully

- `docs_aligned`: true only when every surfaced candidate is fixed or a verified
  false positive and a fresh manifest is expected to find no real drift; false
  when work or deferred real drift remains.
- `commits_this_pass`: exact number of commits created in this pass.
- `drift_remaining`: concise remaining work, empty only when aligned.
- `is_code_bug`: true if at least one doc-right/code-wrong case was found.
- `needs_human` and `human_note`: only for an actual operator decision.
- `summary`: documents committed, evidence used, false positives dismissed,
  findings handed off, and relevant pre-existing failures.

The deterministic gate converges only when `docs_aligned` is true,
`scope_check.scope_ok` is true, `verify_run.passed` is true, and manifest
coverage meets its target. If any condition fails, the continuation loop sends
a fresh manifest or `fail_log` to the next campaign pass.
