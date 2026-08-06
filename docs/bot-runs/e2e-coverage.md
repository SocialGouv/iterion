[← Bot runs](README.md)

# e2e-coverage (Endy) — run bilans

Matrix-anchored e2e coverage completion bot (ADR-058 v2, sibling of Testy).
ONE `campaign` agent inventories an application's features into a committed
feature×coverage matrix and closes each gap with a deterministic e2e test in
the repo's own harness — one `test(e2e):` commit per feature, the matrix row
flipped in the same commit. The deterministic gate re-runs the repo's suite
AND enforces the matrix contract (parse, statuses, justified exceptions, and
the claims grep — an orphan claim is a red gate). Whole-app runs converge at
zero uncovered rows; scoped runs converge on scope-level completion.

---

## 2026-08-06 — V4 studio-ui: Playwright harness bootstrapped + 9 views covered + a real UI bug found (run 019fd6e6-32d8)
- Status: **VALIDATED** — converged in ONE campaign pass (83 min, **$37.6**), scoped target = the 9 `studio-ui` rows + operator-approved harness bootstrap.
- Result: 11 commits on `iterion/run/orbital-growl-crystalbloom-2659` (26 files, 1 268 insertions, **zero product-code changes**): `@playwright/test` harness in `studio/e2e/` (fixture-workspace seeding via the REST API + store, real built server on a temp store, no LLM credential), Taskfile target `test:e2e:ui` (skips cleanly when no browser is installed — `task check` stays credential-free), 9 view specs (run console, board drag-persist, launch, gallery+builder, editor parse→edit→unparse round-trip, pipelines cap, dispatcher lifecycle, secrets seal/delete, browser pane preview trigger), an order-independence hardening pass, and a docs note of the house rules. **24 Playwright tests, re-run post-merge by the operator: 24 passed in 29.8 s.**
- **Real bug found by the suite**: `CostPreviewChip` dereferenced `data.nodes.length` while `/api/runs/preview-cost` answers `{"nodes": null}` for a workflow with no LLM node — the whole Launch view crashed into its error boundary, so a tool+compute-only bot could not be launched from the studio at all. The campaign honoured the contract (product code untouched): it committed a deterministic KNOWN-BUG tripwire test asserting the defect, with the positive assertion ready in a comment. Fixed post-merge by the operator (`fix(studio)` 457374ddd) and the tripwire flipped to the positive contract.
- Lessons: the "bootstrap the harness + cover the family" compound target works in one pass when the operator pre-arbitrates the harness decision; the KNOWN-BUG tripwire pattern (assert the defect deterministically, positive assertion in a comment) is worth folding into the e2e-coverage skill.

## 2026-08-06 — V3 cloud family: 3 gaps closed deterministically via existing fakes (run 019fd6ae-ca4a)
- Status: **VALIDATED** — converged in one campaign pass (53 min, **$18.5**), scoped target = the 3 `cloud` rows, "no heavy new test deps" constraint honoured.
- Result: 3 `test(e2e):` commits + 1 lint fix on `iterion/run/mirage-thwack-beamspire-f903`: DLQ admin surface (super-admin gate + audit trail), `migrate to-cloud` blob upload (walker + S3 wire, `pkg/store/blob/s3_roundtrip_test.go`), cross-replica Valkey state (OAuth/CSRF, board run-tokens, rate buckets). None ended `excluded` — the existing seams sufficed. Post-merge: e2e + server + cloud suites green.
- Friction → bot hardening: this run's `verify_build` found the PREVIOUS run's `verify.sh` in the shared per-project scratch, pinned to a dead worktree path (it noticed and rewrote it — no harm). Hardened in the bot: the verify prompt now mandates overwrite + `$PWD`-relative commands (`fix(e2e-coverage)` 0af0da7b8).

## 2026-08-06 — V2 cli family: 4 gaps in one pass, first-try green gate (run 019fd687-9cc6)
- Status: **VALIDATED** — converged in one campaign pass (35 min, **$12.6**), scoped target = the 4 `cli` rows (incl. the `cli.dispatch` row the V1 pertinence audit had reopened).
- Result: 4 `test(e2e):` commits on `iterion/run/onyx-glide-starforge-e9d1`: `--model/--backend` node re-targeting, `--auto-resume` recovery loop, `bench asymptote` convergence curve, `iterion dispatch` daemon boot loop. Scoped-run convergence semantics worked exactly as designed (out-of-scope rows untouched). Post-merge e2e suite green (171 s).

## 2026-08-06 — V1 inventory + quick wins on iterion itself: 290-row matrix, 10 test commits, gate caught a real contract break (run 019fd613-26f8)
- Status: **VALIDATED** — first live run of the bot, converged (`gate.converged=true`) after one budget-raise resume.
- Versions: bot v0.1.0 · iterion `f3de1569f` (worktree branch) · unsandboxed (`ITERION_SANDBOX_DEFAULT=none`, matching the recent local-dogfood pattern; `worktree: auto` is the isolation).
- Method: CLI run from the authoring worktree, `--store-dir <main>/.iterion` (studio-visible), `--merge-into none`, `--max-cost-usd 40 --max-duration 2h`, `--var max_passes=3`, scoped target = "inventory + reconcile the 3 partial coverage docs + close the 3-5 highest-value quick wins". 113 min wall, **$33.9**, 2 campaign passes + 1 resume.
- Result: **15 commits on `iterion/run/019fd613…` (2 739 insertions, all additive)**: the 359-line `docs/e2e-coverage-matrix.md` (290 rows, supersession pointers added to the three partial docs), **10 `test(e2e):` commits** (~2 100 test lines: runs questions/answer ADR-081 surface, `--max-*` budget overrides, secret lifecycle, `--preset` precedence, report, issue lifecycle, diagram, skill library, memory export/import/du, `--recipe` overlay), plus a matrix-row repair, a flake root-cause fix and a lint fix. Merged ff-only into the worktree branch; `task check` green after merge (lint 0 issues, e2e suite 197s green).
- Value: **high** — the matrix is the first complete, machine-checkable inventory of iterion's feature surface (only 15 honest `uncovered` rows remain: cli ×3, cloud ×3, studio-ui ×9 — the studio UI has no in-tree browser harness); the 10 new tests lock down the CLI operator surface that had zero e2e.
- **The deterministic gate proved itself live**: pass-1 verify_run reported `matrix_ok=false` on a REAL defect — an unescaped `|` in the `sandbox.host-state` row (`auto|none`) split the markdown row so "sandbox" landed in the Status column. The fail_log carried the exact row + reason back; pass 2 repaired it (`202e2bf09`). Suite-green + matrix-red is exactly the independence the two floors were designed for.
- **Anti-façade spot-checks (operator, post-merge)**: 2/2 mutation kills — (a) `RunSecretRemove` stubbed to claim success without deleting → `TestSecretSetListRemoveRoundTrip` FAILS; (b) `--max-cost-usd` override silently ignored in `ir.ApplyBudgetOverrides` → `TestRunBudgetOverrideCapsTheRun` FAILS. Both green again on revert.
- **Pertinence review (operator, 4-row sample of `covered-deterministic`)**: 2 clean, 1 defensible (`dsl.worktree-field` cites a pkg test that enters via `.bot` source text — the matrix's documented "front door per family" methodology), **1 miss**: `runtime.supervisor` cited two brick unit tests for a composed steer claim → corrected to `covered-live` (the composed path is LLM-driven); `cli.dispatch` also narrowed (config-building covered, daemon boot loop honestly `uncovered`). Lesson: the claims grep catches *existence*, not *pertinence* — a sample audit of pkg-cited covered rows belongs in the operator review ritual.
- Baseline discipline observed live: the campaign bisected a mid-run full-suite failure (`TestProcessBoardCardCarriesPRLaunchContext`, pkg/server) against a pre-work baseline worktree and correctly skipped it — confirmed post-run: it fails identically on the untouched main checkout (env-dependent on this host, pre-existing).
- Engine hardening: none needed — zero engine defects surfaced.
- Frictions / lessons for next run:
  - **Duration, not cost, was the binding budget**: the run hit my 2-h CLI cap at the final verify_run (90% guard) → `failed_resumable` → `iterion resume --max-duration 3h` converged in 4 min. Keep the DSL default (3 h) for V1-style runs; the banked-in-stride design made the interruption costless.
  - Pass 2 over-delivered (9 commits vs "3-5 quick wins") — high-value but the scope text should say "then STOP" explicitly if tighter runs are wanted.
  - The campaign left a detached helper worktree at `/tmp/iterion-baseline` (its baseline bisect); harmless but `git worktree prune`-worthy — a cleanup note could join the campaign contract.
