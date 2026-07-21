---
name: anti-facade-fix-rules
description: Evidence and editing discipline for Doki v2's single documentation-alignment campaign.
---

# Anti-façade alignment rules

A façade edit looks plausible but is not grounded in the current repository.
Doki's manifest narrows the search; it does not replace semantic verification.

## 1. Verify before writing

Read or grep the live implementation at the candidate's evidence anchor before
changing the document. Record enough path, symbol, command output, or test
evidence in `summary` that another maintainer could repeat the check.

For `status: unverifiable`, first decide whether the value was intended as a
code reference at all. Historical prose, examples, product names, and external
concepts often look like symbols to a heuristic.

## 2. Derive the wording from current behavior

Treat code, tests, schemas, generated help, and repository configuration as the
current-state evidence. The old paragraph supplies context, not truth. Preserve
intent where it still matches the implementation and make the smallest edit
that removes the false claim.

Do not turn an exact, useful statement into vague prose merely to avoid a
verifiable mismatch.

## 3. Respect the writable set

Edit only Markdown in the immutable scanned footprint. When
`go_comment_globs` is explicitly non-empty, comment-only changes in matching Go
files are also allowed. Never change executable statements, tests, build files,
configuration, generated output, or unrelated files.

If `fail_log` names out-of-scope paths, revert those changes before doing more
work. `scope_check` evaluates the complete diff from the run base, including
commits already made.

## 4. Search the negative space

After renaming or removing a command, flag, path, symbol, heading, or concept,
search the full Markdown footprint for remaining stale forms. For example:

```bash
git grep -nF '<old identifier>' -- '*.md'
```

Inspect results rather than replacing blindly: historical records and clearly
labelled migration notes can legitimately retain an old name.

## 5. Validate the corrected surface

Use the closest available proof:

- run the actual command's `--help` for CLI names and defaults;
- check both target path and heading for Markdown links;
- parse or validate DSL/config examples with repository tooling;
- compile or test API snippets when a focused command exists;
- re-run the relevant search to prove a stale reference is gone.

The later build gate is a safety net, not a substitute for checking the edited
claim.

## 6. Commit each aligned document

Keep related corrections for one document together, including necessary
cross-reference edits, then commit before moving to the next document. Use a
semantic message and the trailer required by the campaign:

```text
docs(<area>): align <subject> with current behavior

Bot: docs-refresh
```

Do not maintain a parallel state file. Read `git log` on continuation passes;
the commits are the durable ledger.

## 7. Escalate only real code-side decisions

If evidence shows the doc is correct and code is wrong, leave the doc alone,
set `is_code_bug=true`, and report or file the finding. If neither state can be
established and the choice blocks further work, set `needs_human=true` and
explain the exact decision. Do not pause for ordinary editorial judgment.

## 8. Terminate honestly

Before `docs_aligned=true`, account for every candidate in the pass as fixed or
false positive, account for deferred documents when `chunked=true`, and ensure
no known real drift remains. State which anchors you verified and why dismissed
candidates were not code claims. A fresh manifest, the scope gate, and the
verification gate will independently check the result.
