---
name: coverage-matrix
description: THE CONTRACT of the feature×coverage matrix file — exact table format, allowed statuses, which statuses require test references or justifications, test-reference syntax, and what the deterministic gate enforces (orphan claims are a red gate). Read before creating or editing the matrix.
---

# coverage-matrix — the contract the deterministic gate parses

The matrix is a **committed markdown file** (path = the run's
`matrix_path` var, default `docs/e2e-coverage-matrix.md`) that is at once
the feature inventory, the living todo, the done-oracle and the audit
trail. A deterministic gate parses it on every pass — deviations from
this contract are a **red gate**, so follow it exactly.

## File shape

1. The file MUST contain the literal marker string `e2e-coverage-matrix`
   somewhere — put it in an HTML comment near the top, with the contract
   version:

   ```markdown
   <!-- e2e-coverage-matrix: v1 — machine-parsed; see the coverage-matrix skill of the e2e-coverage bot -->
   ```

2. Exactly ONE matrix table in the file, with this **exact** column set,
   in this order (the gate binds on the exact header — a near-miss header
   is "no matrix table found"):

   ```markdown
   | ID | Feature | Family | Status | Tests | Notes |
   |---|---|---|---|---|---|
   ```

3. Every row carries all six cells. **Escape any `|` inside a cell**
   (`auto \| none`, or reword) — an unescaped pipe splits the row, and a
   short row is a hard error, never a silently skipped feature.
4. Prose (scope, legend, exclusion policy, per-family commentary) lives
   above or below the table. Blank lines between row groups are tolerated,
   but a non-table line ends the table.
5. **No other table in the file may have both a `Feature` and a `Status`
   column** — a second one is an error, not a tie the gate breaks
   silently. Give legends and summaries a different column set.
6. An **example** table (like the one below) must stay inside a fenced
   code block; the gate blanks fences before parsing so an illustration
   can never be mistaken for the matrix.

## Statuses (the only allowed values)

| Status | Meaning | Requires |
|---|---|---|
| `covered-deterministic` | Exercised end-to-end by a CI-runnable, credential-free test | ≥1 resolving test reference |
| `covered-live` | Exercised end-to-end only in the repo's opt-in/live layer | ≥1 resolving test reference AND a justification in Notes (why deterministic is impossible) |
| `unit-only` | Deliberately terminal at unit level — an e2e would only re-test the harness | a justification in Notes |
| `excluded` | Not testable in this repo's harness at all | the reason in Notes |
| `uncovered` | Real gap — remaining work | nothing (Notes may hold the plan) |

Anything else (typos, blank status on a feature row) is a red gate.
`unit-only` and `excluded` without a justification are a red gate — the
matrix must never encode a silent skip.

## Test references (the `Tests` cell)

Comma-separated, **plain text** — no markdown links, no line breaks.
Three accepted forms:

- `TestName (relative/path/to/file)` — preferred: the gate checks the
  file exists AND contains `TestName`.
- `relative/path/to/file` — the gate checks the file exists.
- `TestName` — bare name: the gate greps the whole tree
  (`git grep --untracked`) for it.

At least one reference per `covered-*` row must resolve, or the row is
an **ORPHAN CLAIM** and the gate is red. What the gate requires of a
reference:

- it must land in a **real test file** — a directory, a doc, a source
  file, or the matrix citing itself resolves nothing;
- a **bare name** must be at least 4 characters and read like a test
  identifier (`test`/`spec`/`scenario`/`should`), and must be found
  *inside* a test file — a one-word citation used to match half the tree.

Resolution stays existence-level: citing a real-but-unrelated test passes
the grep and violates the doctrine in [[e2e-coverage]]. Pertinence is on
your honour and on the operator's review of the matrix diff — an audit of
this matrix measured a 4-in-30 miss rate on that axis, so cite the test
that would FAIL if the promise broke, not the nearest one that mentions
the feature.

## Row lifecycle

- **Born** at inventory time, `uncovered` (unless mapping finds real
  pre-existing coverage — then cite it and set the covered status
  directly; that is legitimate no-new-code work).
- **Flipped** to a terminal status in the SAME commit as the test that
  justifies it (`test(e2e): <feature>` carries both the test file and
  the matrix row edit).
- **Refreshed** row-by-row on later passes; IDs are stable forever (see
  [[feature-inventory]]). Never mass-rewrite.

## Scope and convergence

- The gate counts `uncovered` rows over the WHOLE matrix.
- A **whole-application run** (`target` empty) converges only when that
  count is zero.
- A **scoped run** (`target` set) converges on scope-level completion —
  out-of-scope rows may stay `uncovered`; they are the backlog the next
  run picks up.

## Example

```markdown
<!-- e2e-coverage-matrix: v1 — machine-parsed; see the coverage-matrix skill of the e2e-coverage bot -->

# E2E coverage matrix

| ID | Feature | Family | Status | Tests | Notes |
|---|---|---|---|---|---|
| cli.run-budget-override | run: per-run budget cap flags override the file | cli | covered-deterministic | TestRunBudgetOverride (e2e/budget_test.go) | |
| runtime.await-best-effort | convergence: `await: best_effort` proceeds on partial branches | runtime | uncovered | | plan: stub one failing branch |
| backends.live-model-quality | model output quality on real providers | backends | covered-live | TestLive_ModelQuality (e2e/live_test.go) | essence of the feature IS the live model; no deterministic equivalent |
| util.slugify | slug normalisation helper | util | unit-only | | pure function, fully asserted in util/slug_test.go; e2e adds no risk coverage |
| integrations.third-party-oauth | real OAuth consent flow with provider X | integrations | excluded | | needs a real provider tenant; no sandbox exists |
```
