[← Bot runs](README.md)

# test-coverage (Testy) — run bilans

Universal test-coverage augmentation bot (ADR-058 v2). ONE `campaign` agent plans
and writes missing tests for a target area with the repo's own framework,
committing a `test:` commit per gap in stride; a deterministic verify gate
(`verify_build` writes the repo's real build+test into `verify.sh`, `verify_run`
re-runs it on the actual exit code) proves the suite passes, and `gate.converged`
closes a bounded continuation loop. The anti-façade guarantee now lives in the
campaign's termination contract, not a cross-family reviewer relay. Modelled on
feature-dev + whole-improve-loop's verify gate; stack knowledge lives in `skills/`
(test-coverage, verify-tests, test-types). Anti-façade is the design center: the
metric is meaningful tests that catch a real regression, NOT coverage %.

---

## 2026-07-07 — v2 dogfood on pkg/skilllib: 78.1%→93.2%, 35 mutation-verified tests, converged first pass (run 019f3d44-bf42)
- Status: **VALIDATED** — first live run of the v2 shape; the strengthened anti-façade floor proved out in real conditions.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8` · sandbox-full (worktree: auto).
- Method: CLI run, `--store-dir <workspace>/.iterion`, `--merge-into none`, target = "pkg/skilllib — frontmatter parser + layered store", `max_passes=3`, `--max-cost-usd 20 --max-duration 1h` (mono claude, forfait). 7m50s wall.
- Result: `finished`, `gate.converged=true` first pass. **2 semantic commits in stride** on `iterion/run/sonic-blast-fiberglyph-fb91`: `test(skilllib): lock down ScanFrontmatter parsing edge cases` (frontmatter_test.go, 14-case table — the parser previously had ZERO direct test) then `test(skilllib): cover store error paths, skip rules, and invariants` (store_edge_test.go), +327 test lines. Coverage 78.1%→93.2%, 35 tests, self-reported mutation-verified. Deterministic gate: suite green AND `new_test_code=true` — the diff-vs-RUN-BASE (reflog) measurement counted the in-stride commits correctly (the exact hole the v2 conversion hardened).
- Value: real — skilllib is fresh ADR-059 surface; the campaign honestly scoped the remainder ("only IO-fault injection branches left, low regression value") instead of padding.
- Findings / misses: none observed; termination contract fields all coherent.
- Engine hardening: none needed.
- Lessons for next run: a focused package target converges in one pass well under budget; whole-module targets should still expect multi-pass.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: ONE campaign agent writes mutation-proof tests committing each gap in stride; the deterministic gate re-runs the repo suite AND requires genuinely-new test code measured against the RUN BASE (reflog-oldest — in-stride commits count, pre-run history cannot fake it). The in-tree .test_coverage.verify.sh scratch + commit-time reset hack are gone. 13 nodes → 5 exec.
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-23 — scope-auto run + a NEW engine finding (run 019ef5d3)
- Status: **partial** — the bot's scope-auto path validated; run failed mid-loop on a
  distinct gpt-5.5-forfait engine limit (NOT a bot defect, NOT the accumulator fix).
- Method: `--store-dir .iterion --merge-into none`, **no `target`, no test-type vars**
  (the last untested path: bot picks BOTH scope and types). Engine binary @356053e8b
  (has the `tail()` fix).
- Result: **scope-auto + type-auto works** — with nothing specified, plan surveyed the
  repo ("479 test files; real gaps are zero-test packages"), **picked 3 zero-test
  packages** with branching logic + verifiable oracles (`shellquote` shell-injection
  boundary, `proc` iterion-binary locator, `dsl/types` enum→keyword `String()`),
  **skipped** trivial glue + Mongo-bound code (anti-façade), chose **unit** with
  mutation-test framing. act wrote 4 test files; **verify gate passed** first try;
  reviewer_claude approved; reviewer_gpt found a blocker → `fix_gpt` → **FAILED**.
- **`tail()` fix validated LIVE**: `streak_check` evaluated the capped accumulator
  twice with NO "unknown function" error — the engine resolves `tail()` correctly.
- **Failure cause (corrected by Run D below — see also the CORRECTION):** `fix_gpt`
  (claw `openai/gpt-5.5`, `session: inherit` + a multi-package diff) hit a genuine
  `context_length_exceeded` overflow on its first call, and the node-retry then hit a
  `400 {"detail":"Unsupported content type"}` and the retries exhausted → run failed.
  Run's 4 auto-picked tests left in the preserved worktree (unfinished — not repatriated).

## 2026-06-23 — CORRECTION + scope-auto validated end-to-end (run 019ef60f)
- Status: **validated** — re-ran the exact scope-auto config; converged cross-family to
  `done` (commit e3e0817 on its storage branch). The bot's scope-auto path is sound.
- **CORRECTION of the run 019ef5d3 finding:** the `400 {"detail":"Unsupported content
  type"}` is a **TRANSIENT chatgpt-forfait endpoint flake, NOT a compaction bug.** In
  019ef60f it hit `reviewer_gpt` on a `session: fresh` FIRST call (no compaction
  possible) and the executor's node-retry RECOVERED (2nd attempt approved → streak →
  commit → done). So run 019ef5d3 only died because the transient 400 coincided with a
  real `fix_gpt` overflow and the ~2 retries exhausted before a clean attempt.
- I initially mis-attributed the 400 to aggressive force-compaction orphaning a
  `function_call_output` and shipped `dropOrphanedToolResults` (commit dadfc49b2) — that
  was WRONG (it can't even run for a `session: fresh` reviewer) and was **reverted**.
  LESSON: don't ship a fix to shared LLM-client code on an unreproduced hypothesis;
  `reviewer_gpt` being `session: fresh` already ruled out compaction. (See
  [project_claw_gpt5_context_overflow_fix] memory.)
- Residual (real but minor, NOT fixed): transient forfait 400s could get more retry
  attempts so they don't coincide-and-exhaust; and `fix_gpt` `session: inherit` overflow
  on big multi-package diffs (gpt-5.5-forfait small window) — long-term mitigated by
  explore-mode-style read-on-demand (Willy ADR-045). The bot itself is validated across
  all 4 paths.

## 2026-06-23 — type-selection validation: bot-chooses + multi-type (runs 019ef53b + 019ef54d)
- Status: **validated** — two more clean cross-family convergences exercising the
  type-selection paths the first run didn't.
- Versions: bot 0.1.0 · iterion @ d665317 (post-merge of this work)
- Method: both `--store-dir .iterion` (visible in the operator's studio) `--merge-into none`.
  - **Run A — "bot chooses"** (`019ef53b`, nova-mosh-prismfox): `--var target=pkg/secrets`,
    NO test-type checkboxes. The bot **chose Unit only**, explicitly: *"operator left
    all types unset → I choose"*, and **excluded the Mongo stores** as "integration
    territory, out of scope without a harness", citing the anti-façade doctrine. Targeted
    security-relevant pure logic (path-traversal rejection, tenant isolation, OAuth-kind
    validation, Codex auth fallback). Converged cross-family → commit **2663ac6** on
    `iterion/run/nova-mosh-prismfox-d157`: 5 test files, 342 insertions, coverage 46.3%→
    (~70% on the testable surface). The *modest* jump is the right signal — it covered only
    what's meaningfully testable instead of writing façade Mongo tests to game the %.
  - **Run B — multi-type unit+integration** (`019ef54d`, wonky-thrash-riffboi):
    `--var test_unit=true --var test_integration=true --var target=pkg/store`. The plan
    addressed **both** types, correctly categorized: Unit = pure in-memory helpers
    (`IsTerminal`, tenant/watched-issue, snapshot-ref); Integration = `FilesystemRunStore`
    crossing the real FS via the repo's existing `tmpStore()` helper (CAS status writes,
    checkpoint round-trips, event-range reads, `PublishInboxEvent`) — "matches the house
    style of existing `store_test.go`". 10 funcs / 58 assertions; coverage **62.7%→70.7%**.
    Converged cross-family → commit **bf775d6** on `iterion/run/wonky-thrash-riffboi-b057`.
- Value: confirms the two selection paths the first run left untested — auto type-choice
  (with honest exclusion of un-harnessed code) and explicit multi-type (unit+integration
  split matched to the repo's helpers). All 3 dogfoods (pkg/log, pkg/secrets, pkg/store)
  converged cross-family with the deterministic gate passing first try (no repair loop).
  The OpenAI forfait held for both (reviewer_gpt ~$0.02 each).
- Findings:
  - **prepare_commit session-fork is consistently dropped** even on a fresh (non-resume)
    run: "parent session has no recorded provider fingerprint" → it starts a fresh session
    and re-reads `git diff HEAD` to build the commit. ROOT CAUSE: the
    `streak_check -> prepare_commit with {_session_id: …}` edge carries the session id but
    NOT `_session_fingerprint`, and the fork-safety check at
    [claude_code.go:1888](../../pkg/backend/delegate/claude_code.go) requires it. **Pre-existing
    and shared with feature-dev** (same fork pattern), benign (the commit is correct; minor
    extra cost re-reading the tree). Optional fix: add
    `_session_fingerprint: "{{outputs.simplify._session_fingerprint}}"` to that edge to
    restore cheap inheritance (do in a worktree; verify it resolves + same-provider only).
  - Run B's prepare_commit labeled the commit subject "unit coverage" though it includes
    integration tests — cosmetic (the fresh-session prepare_commit lacks the plan's
    type breakdown; the fork fix above would also tighten this).
  - The dogfood tests live on their storage branches (2663ac6, bf775d6); `git merge` them
    into a feature branch if you want the pkg/secrets + pkg/store coverage (not auto-merged;
    `--merge-into none`).
- Lessons for next run: e2e still unexercised (needs a target with a real e2e harness);
  consider a run that lets the bot pick the SCOPE too (empty `target`).

## 2026-06-23 — first dogfood, pkg/log unit (runs 019ef4fa + 019ef505)
- Status: **validated** — full cross-family convergence to a clean `test:`
  commit. (Surfaced + fixed one engine bug along the way; the GPT half was
  briefly blocked by an exhausted OpenAI forfait, resolved by switching Codex
  account + `iterion resume`.)
- Versions: bot 0.1.0 · iterion (working tree on `main` @ e2cd45c + this work)
- Method: `iterion run bots/test-coverage/main.bot --var target=pkg/log --var
  test_unit=true --merge-into none`; backends claude_code (opus-4-8) for
  plan/act/simplify/reviewer_claude/prepare_commit, claw `openai/gpt-5.5` for
  reviewer_gpt; sandbox `iterion-sandbox-full:edge`, `worktree: auto`,
  network open.
- Result: **converged to `done`.** plan → act → simplify → **verify gate PASSED
  first try** (`passed=true, new_test_code=true`, 537 ms, no repair loop) →
  reviewer_claude **approved (high, 0 blockers, 7 areas)** → [forfait 429,
  resumed] → reviewer_gpt **approved (high, 0 blockers, family=gpt)** →
  streak_check **stop** (cross-family double approval) → prepare_commit picked
  **only `pkg/log/log_test.go`** (correctly excluded an unrelated catalog
  regen line) → commit_changes → done. Commit **aac2ab2** on storage branch
  `iterion/run/019ef505…` (`--merge-into none`, main untouched); message
  `test(log): cover Truncate rune-boundary and writer levels` (+386 lines).
  Cost ≈ **$2.9** total, ~13 min wall across the initial run + resume.
- Value: **high.** Produced 12 meaningful unit tests for `pkg/log`
  (`pkg/log/log_test.go` +399 lines) — 51 assertions, 38 table cases, targeting
  edge/error paths (nil-receiver safety, JSON marshal-failure drops the line,
  UTF-8-safe byte cut, level resolution precedence, emoji gating). **Coverage
  58.1% → 99.2%.** Repatriated to the main tree and independently verified
  (build + `go test` + vet + gofmt all green, 99.2%).
- Findings / anti-façade behaviour:
  - The agent **self-mutation-checked** during `act`: "disabling the
    `safeByteCut` walk-back made TestSafeByteCut/TestTruncate/TestBlockPreview
    fail … restoring it passed. Proves the tests catch a real regression." The
    skill's mutation-test doctrine reached the implementer concretely.
  - reviewer_claude applied the mutation lens and approved with high confidence
    + zero blockers — no façades to catch (the deterministic gate + plan kept
    quality high upstream).
  - The deterministic `verify_run_tests` gate passed first try
    (`passed=true, new_test_code=true`, 537 ms) — no repair loop needed.
  - `extra_test_kinds`/checkbox substitution rendered correctly in prompts
    (`unit: true …`); empty selection would have let the bot choose.
- Engine hardening:
  - **claude_code accepted "API Error: 5xx" as a successful result** (run 1,
    019ef4fa): an Anthropic 529 overload was rendered by the CLI as the `plan`
    node's *result text*, so the node "succeeded" with a non-plan and `act`
    inherited a poisoned session and hung. Fixed: `isTransientAPIErrorResult`
    in [pkg/backend/delegate/claude_code.go](../../pkg/backend/delegate/claude_code.go)
    re-types a transient API-error-as-result (429/5xx/529/connectivity, short +
    `API Error:` prefix) to `ErrTransient` so the executor retries; 4xx
    client/auth errors still surface. 21-case unit test
    ([claude_code_apierror_test.go](../../pkg/backend/delegate/claude_code_apierror_test.go)).
  - **Skill gap (fixed inline):** in the sandbox, `devbox run` fails
    (`~/.cache/devbox` unwritable); the agent adapted to plain `go test`, and
    `skills/verify-tests.md` now documents the `XDG_CACHE_HOME=/tmp` retry →
    direct-toolchain fallback.
  - Minor: the engine auto-regenerates the bot catalog into the worktree at run
    start, adding a 1-line unrelated diff (sec-audit-deps `scope_notes`).
    Harmless — `prepare_commit` excludes non-test files by design.
- Lessons for next run:
  - reviewer_gpt/fix_gpt require a working non-Anthropic backend. The ChatGPT
    **forfait** quota (`429 usage limit reached`) is a hard cap a retry can't
    clear — switching the Codex account (or setting `OPENAI_API_KEY`) + `iterion
    resume --run-id … --file … --force` picks up the new creds (the resumed
    sandbox re-mounts `~/.codex`) and closes the loop from the checkpoint.
  - On **resume**, the `prepare_commit` session-fork is dropped ("parent session
    has no recorded provider fingerprint") and it starts fresh — harmless, it
    re-reads `git diff HEAD` to build the commit. Expected resume-path behaviour.
  - `iterion resume` has **no `--timeout` flag** (unlike `run`).
  - Bot is validated end-to-end. Next: a multi-type run (integration/e2e) on a
    repo with a real e2e harness, and a no-checkbox "bot chooses" run, to
    exercise the type-selection + scope-auto paths.
