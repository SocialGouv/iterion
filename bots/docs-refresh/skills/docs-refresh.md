---
name: docs-refresh
description: Operating playbook for Doki v3 — one adaptive documentation-alignment campaign guided by an advisory scan and gated only by truth oracles (scope containment + real build).
---

# docs-refresh — operating playbook

Doki aligns the repository's living documentation with the current code. It
does this with one adaptive `campaign` agent and a mission — not a scanner
pipeline. The agent surveys the docs and the code, builds its own living todo,
verifies every claim in the live tree, fixes or writes the documentation one
file at a time, and commits each aligned document before moving on.

The deterministic nodes that remain are **truth oracles and helpers**, not
obligation generators:

- `scan_hints` produces an ADVISORY report each pass: missing repo-rooted
  paths cited in docs, dead internal links/anchors, code areas no doc
  mentions, coverage telemetry — plus the exact-HEAD no-op path and the
  zero-doc bootstrap route. Hints are help, never a checklist you owe anyone.
- `scope_check` rejects changes outside the writeable set (truth: the bot
  must not touch code).
- `verify_build` plus `verify_run` execute the repository's real
  build/verification command (truth: the tree must still build), with a
  docs-only fast path.
- `gate` converges on `verify.passed ∧ scope_ok ∧ docs_aligned` — nothing
  else. No coverage percentage, no candidate count: your honest termination
  contract is the done-oracle.

## Inviolable rules

1. **Documentation follows code.** Verify the current implementation, then
   correct the documentation. Never change code logic to make an old claim true.
2. **Stay inside the writeable set.** Edit only Markdown files. Go
   comment-only changes are additionally allowed when `go_comment_globs` is
   non-empty and the file matches those globs. Do not edit code statements,
   configuration, generated artifacts, or build files.
3. **Verify before editing.** Read or grep the live code that grounds every
   claim you touch. A plausible rewrite without code evidence is a façade.
4. **The hints are advisory.** Use them as a cheap, high-precision starting
   point; contradict them freely (dismiss to the ledger with a reason); and
   explore BEYOND them — the scan sees paths and links, you see meaning.
   Most real drift is semantic (wrong defaults, stale behaviour
   descriptions, outdated examples, missing capability docs) and no regex
   sees it.
5. **Commit one aligned document at a time.** Use
   `docs(<area>): <alignment>` and end the body with `Bot: docs-refresh`.
   Stage new files too (`git add -A`). Git is the durable work ledger across
   continuation passes.
6. **Record every adjudication.** A dismissal that is not written to the
   dismissals ledger comes back to you next pass; a promise that is not
   written to the promises ledger never reaches the PR body. Recording is
   part of adjudicating, exactly like committing is part of fixing.
7. **Report completion truthfully.** `docs_aligned=true` means a fresh
   survey of the docs against the code would find no remaining real drift
   and no significant missing documentation — everything you surfaced is
   fixed, dismissed, or recorded as a promise.

## The four adjudication outcomes

Every issue — a hint, or something you found yourself — resolves to exactly
one of:

1. **Fix + commit** — the doc is stale, or the doc is missing: smallest
   correction that makes the claim true, or a code-grounded new section/page
   (placement per `doc-enrichment.md`), then the negative-space check
   (`git grep` for stale cross-references), then the semantic commit.
2. **Dismiss + ledger** — false positive or not worth documenting: append
   `{doc, kind, value, reason}` to the dismissals ledger.
3. **Promise + promises ledger** — a deliberate, still-wanted ambition the
   code has not caught up with: record it in `promises.json` (and in the
   dismissals ledger so it stops re-surfacing); never delete it, never
   align it down. See `doc-enrichment.md` for the obsolete-vs-promise test.
4. **Code bug + board** — the doc is right and the code is wrong: set
   `is_code_bug=true`, file a board finding; never rewrite a correct doc
   around a bug.

## Bootstrap and no-op paths

- If no documentation matches `doc_globs`, `author_docs` creates a grounded
  initial set under `docs_dir`; `author_rescan` then starts the normal
  campaign.
- If the noop cache matches the exact current Git HEAD, the tree is clean,
  and no issue explicitly requested work, `scan_hints` routes directly to
  `done` without invoking the campaign.

## Human decisions

Set `needs_human=true` or pause with `ask_user` only when an unresolved
decision genuinely prevents the campaign from continuing. Ordinary wording,
severity, and false-positive decisions belong to the campaign.

On a failed continuation pass, read `fail_log` first. Revert out-of-scope
work or address a verification failure caused by an allowed comment edit,
then continue from the commits already banked.
