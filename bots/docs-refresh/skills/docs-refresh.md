---
name: docs-refresh
description: Operating playbook for Doki v2 — one adaptive documentation-alignment campaign backed by deterministic manifest, scope, and build gates.
---

# docs-refresh — operating playbook

Doki aligns the repository's living documentation with the current code. It
does this with one adaptive `campaign` agent, not a reviewer/fixer relay. The
agent receives a bounded drift manifest, verifies each candidate against the
live tree, fixes the documentation one file at a time, and commits each aligned
document before moving on.

The deterministic nodes remain authoritative:

- `scan_docs` fixes the documentation footprint and can reuse the inter-run
  cache or take the exact-HEAD no-op path.
- `scan_code_surface` optionally inventories Cobra-like CLI and diagnostic
  surfaces.
- `build_manifest` extracts anchors, verifies what it can mechanically, and
  emits the severity-sorted, document-chunked working set.
- `scope_check` rejects changes outside the writable set.
- `verify_build` plus `verify_run` execute the repository's real verification
  command.
- `gate` converges only when scope and verification pass, the campaign reports
  alignment, and mechanical coverage meets `coverage_target_pct`.

## Inviolable rules

1. **Documentation follows code.** Verify the current implementation, then
   correct the documentation. Never change code logic to make an old claim true.
2. **Stay inside the writable set.** Edit only Markdown files in the scanned
   footprint. Go comment-only changes are additionally allowed when
   `go_comment_globs` is non-empty and the file matches those globs. Do not edit
   code statements, configuration, generated artifacts, or build files.
3. **Treat the footprint as immutable for the run.** `scan_docs` determines it
   before the campaign. The only exception is the documented zero-doc bootstrap:
   `author_docs` creates initial Markdown and `author_rescan` establishes the
   footprint used by the campaign.
4. **Verify before editing.** Read or grep the live code at the candidate's
   evidence anchor. A plausible rewrite without code evidence is a façade.
5. **Commit one aligned document at a time.** Use
   `docs(<area>): <alignment>` and end the body with `Bot: docs-refresh`.
   Git is the durable work ledger across continuation passes.
6. **Report completion truthfully.** `docs_aligned=true` means every surfaced
   candidate was fixed or shown to be a false positive and no real drift is
   expected on a fresh manifest pass.

## Working the manifest

Group `drift_candidates` by document. For every candidate in the current
document:

1. read its context and verify the cited claim in the current tree;
2. classify it using `doc-mismatch-taxonomy.md`;
3. make the smallest correction that makes the documentation true;
4. search the Markdown footprint for stale cross-references to renamed or
   removed concepts;
5. validate affected links, snippets, commands, or anchors;
6. commit the aligned document, then continue.

`status: drifted` is a high-signal mechanical mismatch. `status:
unverifiable` means only that the extractor could not decide; many such
candidates are legitimate prose, historical references, external links, or
illustrative identifiers. Inspect them rather than editing automatically.

When `chunked=true`, only the highest-priority documents are present in this
pass. Deferred documents reappear after the current chunk is cleared. Do not
declare global alignment while `docs_with_drift_count` shows work outside the
chunk. Setting `max_review_chunk_docs=0` disables document chunking; the name is
retained for compatibility with v1.

## Bootstrap and no-op paths

- If no documentation matches `doc_globs`, `author_docs` creates a grounded
  initial set under `docs_dir`; `author_rescan` then starts the normal campaign.
- If the audit cache matches the exact current Git HEAD, the tree is clean, and
  no issue explicitly requested work, `scan_docs` may route directly to `done`
  without invoking the campaign.

## Code bugs and human decisions

If the documentation is correct and the implementation is wrong, do not rewrite
the doc around the bug. Set `is_code_bug=true`, explain it in `human_note`, and
file a board finding when board tools are available. Set `needs_human=true` or
pause with `ask_user` only when an unresolved decision genuinely prevents the
campaign from continuing. Ordinary wording, severity, and false-positive
decisions belong to the campaign.

On a failed continuation pass, read `fail_log` first. Revert out-of-scope work
or address a verification failure caused by an allowed comment edit, then
continue from the commits already banked.
