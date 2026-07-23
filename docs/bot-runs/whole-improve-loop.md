# Willy — `whole-improve-loop` run bilans

Whole-repository improvement loop (ADR-058 v2). ONE `campaign` agent surveys the
workspace, makes fixes in place, and commits each unit in stride; a deterministic
verify gate (`verify_probe`/`verify_build` write the repo's real build+test into
`verify.sh`, `verify_run` re-runs it on the actual exit code) is the truth oracle,
and `gate.converged = <termination flag> ∧ gates green` closes a bounded
continuation loop. See [bots/whole-improve-loop/](../../bots/whole-improve-loop/).

> Convergence machinery (`campaign` → `verify_probe`/`verify_build` →
> `verify_run` → `gate`) is **shared with Billy** (`branch-improve-loop`), which
> migrated to the same v2 shape; its convergence to an asymptote is validated in
> [branch-improve-loop.md](branch-improve-loop.md). This page covers Willy's
> whole-repo specifics.

## 2026-07-07 — v2 on a RELIABILITY axis: converges (not capped) on 9 hang/leak fixes at the I/O boundary (run 019f3afc)

- Status: **validated** — v2 proven on a third axis (reliability), and this run
  **converged naturally** (`finished`, two consecutive same-family clean
  approvals) rather than being forfait-capped — the asymptote (ADR-057) working.
- Versions: bot v2.0.0 · iterion `c3131a58b` (launch) → rapatriated to
  `a872b6cdd`. Method: mono claude sonnet-5 (`--model agent=claude-sonnet-5`),
  sandbox `iterion-sandbox-full:edge`, `--merge-into none`, `--auto-resume 20`,
  `--max-duration 24h`, forfait cap 90. **Axis**: reliability. Scope: `pkg,cmd`.
- Result: **finished (converged, NOT capped)** — **9 commits** on
  `iterion/run/laser-twist-plasmasong-e022`, cherry-picked clean onto main
  (`3ebc708d7..a872b6cdd`). Footprint 14 files, **+176/−41**. Build+vet+gofmt +
  101-pkg `go test ./...` green; adversarial review **9/9 SOLID, SAFE-TO-MERGE**.
- Value: hardened the engine's **I/O boundary against hangs + leaks** — bounded
  every previously-unbounded external subprocess with a `context.WithTimeout`
  (all correctly plumbed into `exec.CommandContext` + `defer cancel()`): git
  (`pkg/git` 30s), worktree git ops (`pkg/runtime` 60s, signature `gitCmd →
  (*Cmd, CancelFunc)` propagated across 25 callers), fork `git worktree add`
  (`pkg/runview` 60s), `docker rm`/`git status` (`pkg/dispatcher` 15s), crontab
  read/write (`pkg/cli` 10s), keychain probe (`pkg/backend/detect` 5s). Plus a
  real **CDP session + read-pump goroutine leak** on WS proxy teardown
  (`runs_browser.go` — `Detach` idempotent, mutex-protected, deferred before
  `conn.Close`), and **surfacing previously-swallowed deferred flush errors** on
  upload staging (`runs_uploads.go` — correct close-err precedence, the Copy err
  still wins). Every fix makes a hung/failed external op a *visible error* rather
  than an indefinite wedge — squarely the "erreurs-explicites, no silent
  fallback" rule.
- Findings / misses: the one soft commit is `3122c1062` (fan_out_each "re-check
  item type") — the adversarial review confirmed it is **defensive but
  unreachable in practice** (the first loop at `fan_out_each.go:322` already
  errors on a non-map item), so the message slightly over-claims a bug fix;
  harmless, no new panic, kept. No façade elsewhere.
- Engine hardening: the run's *output* IS the hardening (9 reliability fixes to
  iterion itself, the self-host target).
- Lessons: a reliability axis **converges far faster than a quality axis** — 9
  commits then asymptote (`finished`) here vs the quality tour's 100 commits
  forfait-capped (run 019f2962) — because reliability defects at the I/O boundary
  are sparse and enumerable, whereas dedup/quality opportunities are near-endless.
  Reliability is an excellent **bounded, high-signal** axis for a single
  cost-capped window. Next reliability targets the sweep did *not* reach (scope
  was `pkg,cmd`): the studio/cloud packages, and concurrency-boundary review
  (channel/goroutine lifetimes) rather than just subprocess timeouts.

## 2026-07-03 — v2 on an AMBITIOUS axis: agent self-plans a 7-commit dedup refactor, build+test green, converges (run 019f28ae)

- Status: **validated** (ambitious) — v2 proven on non-trivial, judgment-heavy
  work, not just a mechanical sweep.
- Versions: bot v2.0.0 · iterion `e66918bc2`. Method: mono claude sonnet-5,
  sandbox full, `--merge-into none`, cap 90. **Axis**: open-ended code-quality /
  real-duplication convergence. Scope: `pkg/dispatcher/tracker` (the 3 forge
  adapters the architect survey flagged as duplicated, #2).
- Result: **finished (converged, not capped)**, **7 commits on
  `iterion/run/ember-bound-ashglyph-10d3`**, each a distinct real dedup the agent
  identified and named itself: converge github/gitlab/forgejo issue-ID parsing →
  `parsePrefixedID`; REST plumbing → shared `restClient`; UpdateState label-diff →
  `applyLabelDiff`; state-mapping → `resolveLabelSelector`; RefreshStates loop →
  `refreshStateByID`; constructor defaulting; inline byte-identical `resolveState`
  wrappers. 8 files, +263/−245. **`go test ./pkg/dispatcher/tracker` GREEN** — the
  refactor preserved each adapter's per-provider auth/sentinels/wire structs
  (the verify gate held). main untouched, ~0 forfait cost.
- Value: validates the ADR-058 thesis on AMBITIOUS work — given the axis +
  autonomy + a verify gate, the agent self-plans (living todo) a coherent
  multi-commit refactor, judges real-vs-incidental duplication, and keeps it
  green — exactly the operator's manual campaign, minimal framing. This IS the
  architect-survey #2 finding, landed autonomously.
- Lessons: v2 handles self-directed refactor campaigns, not just mechanical
  sweeps. Cherry-pick the tracker dedup to main after review (it's a genuine
  quality win). Ready for a whole-repo quality run when forfait allows.

## 2026-07-03 — v2 MINIMAL-FRAMING (ADR-058) live-proven: one agent, its natural flow, clean in-stride commits (run 019f288b)

- Status: **validated** — the v2 campaign bot (ADR-058, "lean on the agent")
  proven live. ONE agent, given the axis + standing autonomy, worked in its
  natural flow (explore → living todo → per-site fix/build/test/commit in stride)
  and converged — no rigid enumerate/transform/review graph, no worktree scratch.
- Versions: bot whole-improve-loop **v2.0.0** (8 nodes) · iterion `e66918bc2`.
- Method: `iterion run`, sandbox full, mono claude sonnet-5
  (`--model agent=claude-sonnet-5`), `--merge-into none`, `ITERION_FORFAIT_CAP_PCT=90`,
  `--auto-resume 20`. **Axis**: route hand-rolled tmp+rename writes through
  `store.WriteFileAtomic`. Scope: `pkg/memory,pkg/sessionboard` (2 known real
  sites still present on main — the earlier 019f27d4 fixes landed on a storage
  branch, main untouched).
- Result: **status finished**, **2 commits on `iterion/run/arctic-sparkle-midnightkazoo-6feb`**,
  one per site, **each with a precise semantic message the AGENT wrote itself**:
  `refactor(sessionboard): route spec writes through store.WriteFileAtomic` +
  `refactor(memory): route quota sidecar writes through store.WriteFileAtomic`.
  Both real WriteFileAtomic conversions, branch build-green, main untouched.
  Cost: negligible (7d forfait stayed at 86%).
- Value: closes the arc. The v1 axis-sweep (16 nodes) STALLED on a whole-repo run
  (019f2829: enumerate ran 75min without finishing → 0 commits). v2 removes the
  framing that caused it: the agent commits in stride, so work lands continuously.
  The v1 "workspace" commit-label bug is GONE — v2 has no deterministic
  commit_item node; the agent writes its own semantic messages (the manual
  pattern). This is the operator's proven Claude-Code campaign, encoded with
  minimal framing (ADR-058).
- Lessons for next run: v2 is ready for a broader/whole-repo axis run when forfait
  allows — the enumerate stall is gone, so `--auto-resume` + incremental commits
  should carry a long campaign that banks work continuously and resumes from git.
  Next: align branch_improve_loop as the branch-scoped campaign (ADR-058 rollout).

## 2026-07-03 — LIVE PROOF of the ADR-057 axis-sweep: enumerates + transforms + commits a real cross-cutting refactor across 4 packages, converges (run 019f27d4)

- Status: **validated** — the axis-driven work-list sweep (ADR-057, the operator's
  manual campaign pattern encoded) proven end-to-end on a real axis. The bot now
  applies a determined improvement axis across the whole codebase, site by site,
  verified and committed — a GLOBAL cross-cutting change chunked review could not
  express.
- Versions: bot whole-improve-loop v1.0.0 (axis-sweep) · iterion `a1b3a83f8`
  (run predates the same-day scratch-relocation `e75db90af`, engine loop-edge fix
  `4b3243074`, and commit-label fix `f68d90aac` — all landed after).
- Method: `iterion run` (static ITERION_BIN), sandbox `iterion-sandbox-full:edge`,
  dual review (opus-4.8 ⇄ gpt), worktree:auto, `--store-dir $PWD/.iterion`,
  `--auto-resume 20`, `--merge-into none`. **Axis** (`improvement_prompt`): "route
  every hand-rolled temp-file + os.Rename write lacking an fsync through
  store.WriteFileAtomic". Scope: `pkg/dispatcher,pkg/memory,pkg/sessionboard,pkg/runtime`.
- Flow (node trace): `enumerate` → work-list of **4 concrete sites** (exactly the
  ones the architect survey named: quota_fs.go / manager_state.go /
  sessionboard/store.go / bundle.go) → per item: `next_item → transform →
  verify_build → verify_run → alt → reviewer_{gpt,claude} → review_gate →
  commit_item → advance` → `re_enumerate` finds none → **done**.
- Result: **4 incremental commits on `iterion/run/magnetic-surge-prismpunk-bd28`**,
  one per site, each a genuine `os.Rename`/`CreateTemp`→`store.WriteFileAtomic`
  durability conversion, **build+test-verified before each commit** and
  cross-family reviewed (2 claude + 2 gpt, alternating by sweep-iteration parity).
  Converged and terminated. main untouched. Cost: ~15% of the 5h forfait.
- Value: the inversion completed — the whole-improve-loop does the operator's
  proven manual Claude-Code campaign (axis → enumerate sites → fix each → commit
  incrementally → converge), amplified by a deterministic per-item verify gate +
  cross-family review. Contrast with the same-day 019f2247 (chunked review: 9h,
  48 iterations, 0 commits, never converged): 019f27d4 landed 4 verified commits
  and terminated.
- Findings / misses / fixes it surfaced:
  1. **commit_item message = "workspace"** — `LABEL={{item_title}}` (unquoted
     env-prefix) word-split a title with spaces/colons → empty → fallback. Fixed
     `f68d90aac` (quoted `LABEL="..."`). The changes themselves were correct.
  2. **Scratch files in the worktree** — worklist.json/state/verify.sh/log were
     written to the worktree (visible as `??`). Fixed `e75db90af`
     (`${PROJECT_SCRATCH_DIR}` out-of-tree).
  3. **Engine loop-edge merge bug** (surfaced authoring the bot) — fixed
     `4b3243074`, benefits every loop bot.
- Lessons for next run: the 4 WriteFileAtomic commits on the storage branch are
  genuinely good durability fixes (from the survey backlog) — cherry-pick to main
  after review. Re-run with the fixed bot to confirm commit_item now carries the
  real item title. The survey's candidate axes (`.iterion/candidate-axes.md`:
  twin Memory/Mongo stores, tracker↔forge HTTP core, etc.) are the next campaigns.

## 2026-07-03 — LIVE PROOF of the ADR-055 redesign: converges + lands a real fix + terminates (runs 019f2750, 019f275e)

- Status: **validated** — the redesign (increments 1 + 2a + 2c) proven live on
  bounded scopes: the bot now **converges, lands verified commits, and stops**,
  inverting the same-day 019f2247 failure (9h / 48 iterations / 0 commits /
  never converged).
- Versions: bot whole-improve-loop v0.7.0 · iterion `7f96e620f` (main)
- Method: `iterion run` CLI (static `ITERION_BIN`), sandbox
  `iterion-sandbox-full:edge`, dual review (opus-4.8 ⇄ gpt), worktree:auto,
  `--auto-resume 20`, `--merge-into none`, `--store-dir $PWD/.iterion`
  (observable in the operator's studio). NB: `iterion run` WITHOUT `--store-dir`
  persists to a per-bot store `~/.iterion/projects/<bot-key>`, NOT the workspace
  `.iterion` — pass `--store-dir` explicitly for studio observability (the
  CLAUDE.md "omit --store-dir" note is misleading; a 404 in the studio diffs
  panel is the symptom).
- **Run 019f2750** (scope `pkg/reviewtopology`, default grid): converged in **2
  cross-family reviews (~2 min)**, both approved, `done`. **0 commits** — the
  package was already clean, so the empty-commit guard correctly committed
  nothing. Proves **termination** (the old "grinds forever" bug is dead) but the
  target was too clean to exercise the fix→commit path.
- **Run 019f275e** (scope `pkg/dispatcher/native/boardops,pkg/backend/forfait`,
  explicit code-quality axis): num_chunks=1. Flow:
  `reviewer_claude(REJECT: "Dead exported code boardops.Tools()" + redundant
  guard) → fix_claude(applies fix) → reviewer_gpt → reviewer_claude(APPROVE) →
  verify_build → verify_run → commit_changes → done`. **Landed commit
  `b3e4c174e`** on storage branch `iterion/run/pixel-blink-cryomantle-a741`:
  removed the dead `boardops.Tools()` + simplified `ToolsFor`/`Call` (−10 net
  lines), **build+test verified** before commit. main untouched.
- Value: proves the full real-agent inversion — find real blocker → fix →
  cross-family re-review → deterministic verify gate → verified commit →
  terminate. Combined cost of both runs: **~+6% of the 5h forfait**.
- Note on commit_unit vs commit_changes: both runs were single-unit
  (num_chunks=1), so the sole unit's convergence is the stop → it finalizes via
  verify_build/verify_run/**commit_changes** (by design — commit_unit fires for
  units 0..n-1). The per-unit `commit_unit` path (multi-unit) is proven by the
  e2e tests `PerUnitConvergesCommitsAndAdvances` (commit_unit fires exactly
  twice on a 3-unit run) + `PerUnitVerifyGatesCommit`.
- Lessons for next run: to exercise `commit_unit` live, use a scope large enough
  for ≥2 chunks with fixable issues in the non-last chunk. Remaining redesign
  work: ADR-055 increment 2b (adaptive coherent-unit worker replacing blind
  byte-slice chunks) — needs its own live dogfood.

## 2026-07-03 — whole-repo production-readiness, mono sonnet-5: real hardening but did NOT converge → harvested + triggered ADR-055 redesign (run 019f2247)

- Status: **partial** — produced genuinely good hardening, but the bot
  **committed none of it** and **did not converge**; harvested by hand.
- Versions: bot whole-improve-loop v0.5.0 · iterion `8bedc7353` (start) →
  `85ea12d7f` (counting-truth fix landed mid-analysis)
- Method: `iterion run` then 11× manual `iterion resume`, mono claude
  (sonnet-5 fixer + sonnet-5 reviewer), whole repo (no scope), sandbox,
  `--merge-into none`, forfait (claude_code OAuth). Budget raised across
  resumes to `--max-duration 24h`.
- Result: **~9h real active time** (displayed as ~15h before the counting
  fix — an overnight OS-suspend was counted as active), **48 loop
  iterations**, **2.46M tokens**, **0 self-commits** (commit fires only on
  global streak convergence, never reached). Verdict trend oscillated (28
  reject / 23 approve; still a fresh high-confidence blocker at iteration
  50) → the whole-repo streak gate `clean_streak >= num_chunks+1` is
  **unreachable** (a real repo always has another real issue). Stopped with
  SIGINT (clean `failed_resumable` checkpoint) and **harvested build-green**
  to branch `iterion/willy-harvest-20260703` (`52b602886`).
- Value: real, but 0 landed by the bot. New `pkg/backend/secretguard`
  (layered secret-leak defence engine, multi-encoding deterministic match +
  heuristic detector), a `generation.go` OOM guard on streamed
  text/thinking accumulation, ~55% net-new test coverage. Cherry-pick
  per-file after review (harvest commit notes an ADR-053 numbering
  collision the run introduced).
- Findings / misses: reviewer findings were genuine (e.g. `NewSubagentRunner`
  never invoked in prod = dead code) but the blind-chunk design means a
  reviewer can flag a symbol as dead that is live in an unseen chunk — a
  fragmentation false-positive risk.
- Engine hardening surfaced by this run:
  1. **Counting skew (FIXED, `85ea12d7f`)** — active-duration re-derived
     from wall-clock event timestamps counted a 5h46m OS suspend; now uses
     the engine's `SharedBudget` CLOCK_MONOTONIC elapsed (suspend-excluded,
     thinking-counted), stamped per-event as `Event.ActiveMs`. Also the
     48-vs-49 iteration off-by-one (UI showed physical exec index; now shows
     semantic `loop_iteration`) + a run-level `⟳ loop current/max` chip.
  2. **Structural (ADR-055, redesign in progress)** — whole-repo-sweep +
     terminal-commit + blind-chunk-review makes the bot **worse than a
     manual Claude Code loop**: it never converges and lands nothing.
     ADR-055 (`docs/adr/055-unit-convergent-adaptive-improve-loop.md`)
     inverts this: per-unit convergence + **incremental commit** + per-unit
     multi-model verify + bounded completion. Pilot increment 1 (incremental
     commit + per-unit convergence) in progress.
  3. **11 manual resumes** on transient backend errors → Chantier B
     (in-executor retry + bounded, forfait-cap-aware run-level auto-resume)
     in progress.
- Lessons for next run: on a whole repo, **harvest — don't chase**
  convergence (it won't come); the value is real long before the streak
  saturates. The redesign (ADR-055) is the real fix — until it lands, scope
  Willy to a bounded subset (`scope_globs`) so per-chunk convergence is
  actually reachable, or expect to harvest by hand.

## 2026-07-02 — validated the launch-time per-node model/backend override (mono fable/sonnet) (run 019f2236)

- Status: **validated** (feature end-to-end); review loop **converged** (2 clean
  passes, `stop=true`, no oscillation); run **finished** in 12.8m (verify_build on
  sonnet-5 + verify_run passed).
- Versions: bot whole-improve-loop v0.5.0 · iterion `e64513b` (main)
- Method: `iterion run` (CLI, background, static binary), sandbox
  `iterion-sandbox-full:edge`, worktree:auto, store = workspace `.iterion`
  (studio :5199 observable). Flags exercising the new feature:
  ```
  --var scope_globs=pkg/reviewtopology --review-mode mono \
  --backend 'judge=claude_code' --model 'judge=claude-fable-5' \
  --backend 'agent=claude_code' --model 'agent=claude-sonnet-5' \
  --merge-into none
  ```
- Result: reviewer (fable-5) approved the chunk with a genuine agentic review
  (read both files in full, probed 2 edge cases, no blockers, high confidence);
  2nd pass same → `clean_streak=2` → `stop`. verify_build (sonnet-5) + verify_run
  passed. No fix to the scoped `pkg/reviewtopology` (it was clean), but
  `commit_changes` swept up one incidental change the engine made in the worktree:
  a **bot-catalog regeneration** (`iterion-bot-catalog.md`, 5/5, adding the
  `review_mode`/`mono_family` vars — the committed catalog had gone stale since
  ADR-052). Landed on storage branch `iterion/run/feral-jive-auroraflux-9e69`
  (`24fa534`, `--merge-into none`); verified byte-identical to `iterion bots
  regen-catalog` and **rapatriated to main** @22e5aae03.
- **Value**: proved the per-node/-group override
  (`pkg/backend/model.ModelOverrides`, studio dropdowns / CLI `--model`/`--backend`
  / HTTP `model_overrides`). Log evidence: `Delegation started [reviewer_gpt]:
  backend=claude_code` + `claude … --model claude-fable-5` (backend AND model
  override, by **kind selector** `judge=`/`agent=`, top-precedence over the
  node's DSL `backend: claw`/`model: openai/gpt-5.5`). Composes with `--review-mode`.
- **Finding (topology on a forfait-only host)**: `reviewtopology` resolves
  families from detected **provider** creds (API-key style), NOT from the
  `claude_code` forfait OAuth (a *backend* cred). With only codex ChatGPT-OAuth
  present, the sole detected family is **gpt**, so `--review-mode mono` (and
  `auto`) route to `reviewer_gpt`/`fix_gpt` — and `InjectIfDeclared` clobbers any
  explicit `--var mono_family=claude` with the resolver's pick. The per-node
  **backend override is exactly the escape hatch**: re-target the running (gpt-
  family) nodes onto `claude_code` to run claude models (fable/sonnet) on the
  forfait. (First attempt `--model judge=claude-fable-5` alone failed —
  `reviewer_gpt`'s DSL backend stayed `claw`, which rejects a bare
  `claude-fable-5`: claw needs `provider/model`. Fixed by also overriding the
  backend. Lesson: when overriding a claude model onto a claw-default node,
  override the backend too — or use the `anthropic/` prefix for claw.)
- Misses / bot quirks (unrelated to the feature): `verify_build` invokes a
  `Skill verify-build` that the bundle doesn't ship → non-fatal tool error, the
  sonnet agent recovers and runs the build directly.
- Lessons for next run: **DONE** — `reviewtopology` now treats a usable
  `claude_code` forfait as a "claude" family (@7d44eaea7), so on a forfait-only
  host `--review-mode mono` alone routes to claude (no manual backend override
  needed). Still open: let an explicit `--var mono_family` win over the resolver
  (today `InjectIfDeclared` always overwrites it).

## 2026-07-01 — dogfood surfaced (and fixed) a claw+gpt explore-mode façade; grounded reviews restored (runs 019f1cf7, 019f1d24)

- Status: **high value** — the run itself is secondary; it exposed a real engine
  bug that made every `claw` (gpt-5.5) reviewer/fixer ungrounded. Fixed + validated;
  run resumed on GLM-5.2 to converge after the OpenAI forfait capped mid-sweep.
- Versions: bot 0.5.0 · iterion `a2a20ab` + fix (this change)
- Method: `claude_code` opus-4-8 (reviewer_claude + fixers) · `claw` `openai/gpt-5.5`
  (reviewer_gpt) · **`--sandbox none`** (see caveat 1) · `--merge-into none` ·
  store = operator `.iterion` (visible in studio) · `scope_globs` = the recently
  hardened `pkg/runtime` core (engine*/branch/convergence/fan_out*/events*/
  special_node/recovery_dispatch/workspace_safety/helpers) → **7 chunks**
  (bounded so a watched run can converge).

### The bug — claw+gpt-5.5 "explore-mode façade" (the headline finding)
In explore mode the reviewer must READ the chunk files (source is not inlined) and
report `files_reviewed`. reviewer_gpt (claw / gpt-5.5) made **zero tool calls**,
fabricated a plausible verdict from priors, and self-reported `files_reviewed`
paths it never opened — sailing through the engagement gate (which trusts the
self-reported list). reviewer_claude (claude_code) read the files and behaved
correctly. So Willy's GPT half of the cross-family review was a **façade**, and the
"no logs for reviewer_gpt in the studio" symptom was just its consequence (no tool
calls → nothing to stream).

Root cause (verified with a 1-node probe + code trace): iterion always sent
`tool_choice: nil` (→ provider default "auto"), and **gpt-5.5 under "auto" declines
to use provided tools** and answers directly, even when the prompt says "you MUST
call read_file". Claude is agentic under "auto"; gpt-5.5 is not. The sub-agent's
first hypothesis (a missing `ToolChoice` passthrough in claw's OpenAI
chat/completions path) is a **real latent bug** but not the operative cause here:
ChatGPT-OAuth always routes to `/v1/responses`, which *does* forward ToolChoice —
so nil→auto was the actual lever.

### The fix — `ForceInitialToolUse` (agentic parity for claw)
`pkg/backend/model`: new `GenerationOptions.ForceInitialToolUse`. The tool loop pins
`tool_choice="any"` on the first turn (and until the first tool call lands), then
reverts to auto so the model can finish. Enabled for every tool-equipped claw
agent/judge (`claw_backend.go`). The `/v1/responses` path maps "any"→"required", so
the force reaches the ChatGPT-forfait endpoint.
- Validated: the probe's gpt-5.5 now calls `read_file`; 2 new unit tests
  (`TestGenerateTextDirect_ForceInitialToolUse` + negative control); and in the real
  Willy run **reviewer_gpt now reads both chunk files and emits `tool_started`/
  `tool_called` events** → activity is visible in the studio (original symptom
  fixed). Its verdict became specific + grounded (a real `max_duration`-after-
  resource-wait deadline gap in `engine_exec.go`), vs the earlier hand-wavy claim.
- Also trimmed claw per-step `tokens` logging to DEBUG (noise; totals stay on the
  `Node finished` line) and corrected the stale `main.bot:137` comment (Willy runs in
  a worktree by default, it does not edit the live tree).

### Value the run produced (in the worktree, pre-convergence)
Grounded cross-family review of `pkg/runtime` → real edits: `convergence.go`,
`engine_exec.go` (the rem<=0 duration-budget guard after `acquireResources`),
`fan_out_each.go` simplification, `workspace_safety.go` hardening, + added tests
(`fan_out_each_test.go`, `parallel_test.go`, `worktree_default_test.go`). Most other
chunks were reviewed **CLEAN** with genuine cross-references (not a rubber-stamp).

### Caveats / follow-ups
1. **Sandboxed runs still need the fix in-container.** The first sandboxed relaunch
   (019f1cf7) still showed 0 gpt tool calls — the `iterion __claw-runner` inside the
   container ran a stale binary. `--sandbox none` (in-process, fresh binary) was used
   to validate + deliver. To help the operator's *normal* (sandboxed) Willy runs,
   rebuild the sandbox image with the fixed iterion (or verify `addClawBinaryMount`
   mounts the fresh host binary and that `/usr/local/bin/iterion` wins on PATH).
2. **OpenAI forfait cap.** reviewer_gpt hit a 429 usage-limit after ~13 passes ->
   `recovery_pause`. Resumed with `--answer acknowledge_recovery=continue --force`
   and `ITERION_VIBE_MODEL_GPT=anthropic/glm-5.2` + ZAI mode (both reviewers on
   GLM-5.2 via z.ai) to converge. Cross-family diversity is paused in that fallback.
3. **Latent claw bug** (not hit here): OpenAI **chat/completions** + Foundry
   providers drop `ToolChoice` (only `/v1/responses` + anthropic/bedrock/vertex
   forward it) — API-key-mode gpt with forced tools would not be forced. Fix in
   `.works/claw-code-go` `internal/api/providers/openai/provider.go` + `foundry/`.
4. **Engagement gate is still self-reported.** `streak_check` trusts `files_reviewed`
   subset of `chunk_file_list`, not real tool telemetry. Forcing initial tool use
   makes the model actually read (observed: full 2/2-file coverage per chunk), so this
   is no longer urgent — but grounding the gate on real read telemetry would make it
   Goodhart-proof for any future backend.

### Lessons for next run
- On a fresh `.bot` change, `iterion resume` needs `--force` (source hash changes).
- For a watched, converging dogfood, scope tight (<= ~7 chunks): `num_chunks + 1`
  clean-streak + `loop_max = num_chunks + 15` means big scopes can't converge in one
  run. `pkg/runtime,pkg/server` was 42 chunks (won't converge); one package ~= 16-20.
- Force the store with an explicit `--store-dir <workspace>/.iterion`: a bare
  `iterion run <bot>` keyed the store off the *bot path* (`~/.iterion/projects/...`),
  invisible to the operator's studio.

## 2026-06-23 — code-quality axis was a no-op; fixed → 14 real cleanups + a sandbox-claw bug (runs 019ef545, 019ef550)

- Status: **partial** (run 2 produced strong value, ended `failed_resumable` on a reviewer context overflow before convergence)
- Versions: bot 0.3.0 · iterion `d665317` (run binary built from branch `feat/budget-override-flag` @ `5ae01478e`)
- Method: `claude_code` opus-4-8 (reviewer_claude + both fixers) · `claw` `openai/gpt-5.5` (reviewer_gpt) · `worktree: auto` · sandbox `iterion-sandbox-full:edge` · **`--max-cost-usd 120 --max-duration 4h`** (via the new at-run budget-override flag — no `.bot` edit) · `--merge-into none` · store = operator `.iterion` (visible in studio) · whole repo (no `scope_globs`) → **28 chunks**.

### Run 1 (019ef545) — diagnostic: the axis was a no-op
Willy approved **every** chunk (0 rejections, 0 fixes, ~$2) and cruised toward a
clean convergence having changed nothing. Root cause: Willy's reviewer is a
*production-readiness* reviewer whose anti-false-positive rule says
"style/naming/minor-optimisation/missing-doc are **not** blockers" — exactly the
class a code-quality axis targets. It *found* the issues (dead `SwitchTeamWithCookie`,
drifted `RotateSession`, dup dotenv parser…) but filed them under `fix_plan` as
"recommended non-blocking cleanups for a future quality pass" and set
`approved=true`. The fixer had the same bias (pushback "not production-blocking").
So an axis-scoped run can converge on **unimproved** code. Cancelled after diagnosis.

### Bot fix (branch `fix/willy-code-quality-axis`)
When `improvement_prompt` is non-empty the axis now **redefines "blocking"**: a
concrete, evidence-backed on-axis finding is a blocker the fixer applies *this*
pass; the "style not a blocker" carve-out is suspended for on-axis items. Mirrored
in the fixer's pushback rules. Convergence guards kept (concrete + smallest-change
+ no taste / no re-litigation → clean chunks still approve, streak still settles).

### Run 2 (019ef550) — the fix works
**14 chunks rejected→fixed**, real on-axis cleanups: net **−259 lines across 21
files** (dead code in pkg/auth, pkg/dispatcher, pkg/dsl/ir; dup slug-retry;
zero-caller exported `FromIdent`; misleading docs; redundant wrappers). Cumulative
worktree **build + vet + package tests green**. Cost **$44.12**, ~1.5h. Harvested
to branch `willy/iterion-code-quality-2026-06-23` (cleanups as one commit).

### Engine/bug findings
1. **Bot demote-and-defer bug** → fixed (above).
2. **Real bug Willy *found* in iterion** (off-axis, separated to its own commit):
   `pkg/backend/delegate/io.go` `ToIOTask`/`FromIOTask` silently dropped
   `CursorFragments`, `PresetFragment`, `SystemPromptMode`, `Ultracode`,
   `SecretFiles`, `UserContent`, `RepoRoot`, `Iteration`. The runner builds the
   prompt via `Task.BuildSystemPrompt()` *after* `FromIOTask` and cannot
   re-resolve cursors/preset, so **sandboxed claw silently lost cursor
   calibration + launch-preset bias** (and zero `SystemPromptMode` → `Standalone`
   instead of claw's `AuthoredBase` → adaptivity-parity regression). Fix carries
   them over the wire **with a proper round-trip mutation test** the fixer wrote
   in the same commit (`TestIOTaskRoundTrip` asserts every new field survives
   `ToIOTask→JSON→FromIOTask`). Verified by code-trace that the runner consumes
   them via `Task.BuildSystemPrompt()` after `FromIOTask`.
3. **`devbox` couldn't run inside the sandbox → FIXED in the engine.** The
   `devbox run -- …` convention died with `mkdir: cannot create directory
   '/home/.../.cache/devbox': Permission denied` (run 019ef550), so the fixer
   fell back to bare `go`. Root cause: `host_state: auto` lays a user-owned
   tmpfs at `$HOME`, but the Go-cache binds nested under it
   (`$HOME/.cache/go-build`, `$HOME/go/pkg/mod`) made docker create the parents
   `$HOME/.cache` / `$HOME/go` as `root:root`, shadowing the writable tmpfs so
   devbox couldn't mkdir its cache. Fixed by `homeNestedBindParents` in
   [pkg/runtime/sandbox_mounts.go](../../pkg/runtime/sandbox_mounts.go), which
   also lays a user-owned tmpfs at each nested-bind parent — the whole `$HOME`
   subtree is now writable and devbox is first-class. `verify-build` skill
   updated to prefer `devbox run` again.
4. **Reviewer context overflow on large repos → MITIGATED.** Run 2 ended
   `failed_resumable` when `reviewer_gpt` (gpt-5.5 forfait) hit
   `context_length_exceeded` at review_loop 11. Measured: the accumulated
   `cumulative_scanned_areas`+`prior_pushback` was only ~4 K tokens — **not** the
   cause; the dominant input is the inline `chunk_content` (was ≤ 30 K tokens) vs
   gpt-5.5 forfait's effective window (well below the API's). **Fix (A+B,
   configurable):** lowered `max_review_chunk_tokens` default 30000 → **16000**
   (fits the default forfait reviewer with head-room), and added model-adaptive
   sizing — `reviewer_context_tokens` (+ `reviewer_context_percent`, default 45):
   when set, the chunk budget is capped at that %-of-window and the `MAX_CHUNKS`
   rebudget can no longer re-inflate past the ceiling (big repos take more passes
   instead of a bigger chunk). **Deeper fix SHIPPED + validated (ADR-045,
   `dc22b626c`, v0.5.0):** new `context_mode` (default `explore`) — snapshot_chunk
   writes a per-pass chunk INDEX markdown and the reviewer reads the listed files
   on demand via its read tools instead of inlining the source, removing the
   context-window ceiling on chunk size (a package/file bigger than the window is
   now reviewable). Anti-Goodhart guard folded into `streak_check` (no graph
   node): a pass counts toward convergence only if `files_reviewed` is a non-empty
   subset of the chunk's files. `inline` mode kept as fallback. Validated by run
   **019ef5d5** (scoped pkg/log): converged in explore mode — index written,
   verbatim `files_reviewed`, `engaged=true` every pass, real doc-nit fixes,
   stop → verify → commit → done, $7.50. Convergence note: an axis-blocker
   quality reviewer finds a *long tail of distinct genuine nits* (each resets the
   streak) — it still asymptotes once they're exhausted (pkg/log: 2 distinct doc
   fixes → converged at review_loop ~5), but a quality-axis run is thorough, not
   instant. (This long tail is the axis-blocker behaviour, orthogonal to explore.)
6. **`.devbox/` swept into the run commit → FIXED (`.gitignore`).** Now that
   devbox runs in-sandbox (operator's HOME-nested-bind fix), it regenerates
   `.devbox/` in the worktree and `commit_changes`' `git add -A` committed it
   (run 019ef5d5). Added `.devbox/` to `.gitignore` (same family as `.tmp-*/` /
   `.gomodcache/`).
5. **New capability used:** `iterion run --max-cost-usd` (+ `--max-tokens`,
   `--max-duration`, `--max-iterations`, `--max-parallel-branches`) on `run` and
   `resume` — set the $120 ceiling without editing the bot (branch
   `feat/budget-override-flag`).

### Lessons for next run
- Bound the reviewer feedback context (finding #4) **before** the next whole-repo
  run, or it will re-overflow on resume at the same point.
- Whole-repo single runs do not converge (streak needs `num_chunks+1` clean; 14
  fixes keep resetting it) — they make **bounded** progress. `commit_changes`
  only runs on convergence, so on a non-converged run the fixes stay **uncommitted
  in the preserved worktree** and must be harvested by hand (done here).
- The fix made Willy genuinely useful on a quality axis; pair it with a bounded
  scope (`scope_globs`) for affordable, convergent per-area passes.

## 2026-06-15 — deterministic build/test gate shipped + validated live; 2 more pkg/store fixes; placeholder root cause (runs 019ec9d5, 019eca0d)

Follow-up to the 019ec7ed run below. Implemented the recommended **deterministic
build/test gate** (the converge-on-broken backstop), re-ran Willy to exercise it,
and the gate **caught a real regression — its own e2e-test gap.**

### The gate (commit `9419b12f`)
Per the CLAUDE.md "skill-guided + deterministic gate" doctrine, between
`streak_check`'s `stop` and `commit_changes`:
- **`verify_build`** (adaptive agent) reads the new `skills/verify-build.md`,
  detects the repo's OWN build+test tooling (honouring pinned toolchains), writes
  `.whole_improve_loop.verify.sh`, runs it, and fixes breakage the review fixes
  introduced.
- **`verify_run`** (deterministic tool, no LLM) re-runs that script and gates on
  the REAL exit code. Green → commit; red → bounded `verify_loop(3)` back to fix;
  still red → `fail`; no script → skipped+passed but surfaced. Universal: no
  language/PM named in the DSL. validate 12 nodes/21 edges; `verify_run` logic
  unit-tested (pass/fail/skip).

### Run 019ec9d5 (pkg/store re-run) — 2 more real fixes + the placeholder recurs
- chunks 0 & 2 clean (run-1 hardening holds); chunk 1 → 2 blockers; chunk 3 → 2 blockers.
- **`LoadRun` heal-on-read mutex race** + **`OpenRunFile` intermediate-component
  symlink TOCTOU** — both genuine, fixed correctly (the chunk-1 fix_gpt finally
  honoured the FINALIZE guard: applied=true, regression tests, honest verify note),
  host-tested green. Commit **`c7d1f195`** (+ openat-style `openRunFileAt` walk,
  `ensurePlainDirNoSymlink`).
- **The "work in progress" placeholder recurred on chunk 3** despite the FINALIZE
  prompt guard. Root cause is NOT the prompt: the claw fixer's *self-verify*
  mandate, against a sandbox **missing iterion's pinned Go 1.26**, made it loop on
  a doomed build until it exhausted `tool_max_steps` and was cut off mid-task — the
  empty final turn is what `parseSDKOutput`/the formatting pass renders as the
  placeholder. The chunk-3 mongo edit was left half-done (build-broke) → reverted;
  its 2 blockers (`mongo/memory.go` WriteDocument concurrency + `MongoMemoryStore`
  TenantID fail-close) are **deferred findings** (gpt's fix plans captured).
- **Mitigation (commit `2f987b3c`)**: FINALIZE "verify ONCE — don't loop on a
  doomed build"; `fix_gpt tool_max_steps 30→45`. Cancelled the run (broken tree +
  recurring placeholder + the rich-package convergence was uncertain/costly).

### Run 019eca0d (pkg/clock) — gate validated live, end to end
Tiny clean scope (1 chunk → converges in 2 cross-family passes) purely to fire
the gate. Converged → `verify_build` (prepared=true) → **`verify_run` passed,
exit_code 0** → `commit_changes` → **done**. The gate fired, built iterion
in-sandbox (its script self-handles GOTOOLCHAIN/devbox, vendor mode, writable
GOCACHE, a flake-guard), and committed. **Crucially `verify_build` caught a real
regression**: commit `9419b12f` added the verify nodes but never stubbed them in
`e2e/whole_improve_loop_test.go`, so the 4 convergence e2e tests would route to
`fail` — invisible to the chunk reviewers, but `verify_build`'s `go test ./...`
caught it and authored the `stubVerifyGate` fix (repatriated as **`fb503a8f`**).
The converge-on-broken backstop working against the gate's own gap.

### Findings / misses
1. **Sandbox image lacks iterion's pinned Go 1.26** (`full:edge` ships 1.24).
   Blocks in-sandbox build/test for every node, makes `verify_build` slow+costly
   (~33 min / ~$3.91 fighting the toolchain on this run), and is what induced the
   placeholder. **Real fix = publish the sandbox image with Go 1.26** (infra
   follow-up); the gate is sound, the environment isn't.
2. **`verify_build` ran `rm -f .git; git init`** to bootstrap a repo for the e2e
   worktree tests (the worktree's `.git` *file* points outside the sandbox mount),
   which **severed the operator's worktree** and stranded the run's commits
   (recovered by hand). **Guarded (commit `487b0c10`)**: the verify-build skill now
   forbids destroying/recreating `.git` and says to SKIP git-dependent tests when
   git is unavailable. (Also: the sandbox should mount the worktree's real git dir.)
3. **The placeholder is ultimately an engine bug** — a claw delegate that did tool
   work but ends with an empty final turn should not be rendered as an
   `applied=false` "work in progress" by the formatting pass; it should reflect the
   work or report step-exhaustion honestly. **Deferred engine follow-up**
   (`pkg/backend/delegate/parse.go` + the claw backend).
4. **Gate cost on iterion**: `verify_build` + `verify_run` build+test the WHOLE
   repo at convergence. Cheap when the toolchain is present (verify_run reused the
   warmed cache in ~45s); expensive when it isn't (finding 1).

### Lessons for next run
- The gate delivers exactly its promise (caught a cross-file/test break the chunk
  reviewers structurally cannot). Land the **Go-1.26 sandbox image** so it (and the
  in-loop fixers' self-verify) run fast instead of fighting the toolchain.
- The FINALIZE "verify-once" guard + the engine empty-output fix together should
  end the placeholder; until the engine fix lands, prefer routing fixes to
  `claude_code`, or accept the bot-side mitigation.
- Don't run code-mutating bots in a git **worktree** until the sandbox mounts the
  worktree's real `.git` (or the skill's no-bootstrap guard is confirmed) — see
  finding 2.

### Follow-ups fixed (2026-06-15, same session)

All deferred items above are now fixed + verified:

- **Finding 1 — sandbox Go 1.26** (`af07835f`): `iterion-sandbox-full` now
  installs Go 1.26.4 from the official tarball (was apt's 1.24). A `go 1.26`
  go.mod builds in-sandbox with no per-run GOTOOLCHAIN fetch — which is what
  starved the fixer's step budget and produced the placeholder. Built + verified
  locally (`go version` → go1.26.4); CI publishes it on push to main.
- **Finding 3 — claw placeholder (engine)** (`4bfa4830`): the claw recovery
  pass now appends a `finalizeReminder` so a tool loop that ended without
  committing to JSON (narrated, or cut off at MaxSteps) reports the state it
  ACTUALLY reached instead of a coerced "work in progress" placeholder.
- **Skill no-bootstrap guard** (`487b0c10`, finding 2) shipped during recovery.
- **Mongo blockers** (`0473021d`, from run 019ec9d5): `validateCloudTenant`
  fail-close at every entry point (host-tested) + `WriteDocument` compare-and-swap
  on `revision` with bounded retry (verified against a real Mongo —
  `TestWriteDocumentConcurrent_Mongo`: 12 writers → revision 12 + no quota drift;
  the test fails on the old unconditional path, proving it catches the bug).

Remaining smaller nicety (not blocking): full step-exhaustion *telemetry* (a
distinct `StepsExhausted` signal on the delegate Result for the event log) —
the recovery-honesty fix + the Go-1.26 image already remove the placeholder's
cause and symptom.

## 2026-06-14 — scope_globs shipped + pkg/store hardening + fixer-placeholder finding (run 019ec7ed, cancelled-for-value)

- Status: **partial by design** — cancelled mid-sweep once it had produced its
  value (real fixes + clear findings) rather than run to convergence on a tree
  the fixer had silently broken. Converted directly into repatriated commits.
- Versions: bot whole-improve-loop 0.3.0 **+ new `scope_globs` var** · iterion
  base `2707ea2f`, fixes `ec2752ca..3be59a70` (worktree `worktree-willy-improve`).
- Method: CLI `iterion run` (a separate process — dodges the `task studio:dev`
  watchexec self-kill), `--var scope_globs=pkg/store` (the new var), into the
  operator's visible store (`/.iterion`, studio :4891). Backends: reviewer/fix
  `claude_code` opus-4-8 (max) + `claw` openai/gpt-5.5 (high) via ChatGPT
  forfait. `sandbox-full:edge`. Workspace = the worktree (Willy has no
  `worktree: auto`, so it edits its `workspace_dir` directly).
- Result: NOT run to convergence (cancelled after 3 chunks, ~$3.79, ~30 min).
  `scope_globs` pruned pkg/store to **4 chunks / 51 files / loop_max 19**
  (whole-repo iterion is **29 chunks / loop_max 44** — the cost the var fixes).
  chunk 0 clean (claude); chunk 1 → **4 real blockers** (gpt) → fix_gpt; chunk 2
  clean (claude). Repatriated by hand to the worktree branch.

### Value
- **`scope_globs` works end-to-end** — fixes the focused-run cost finding from the
  019ec598 bilan below. A focused pkg/store pass cost **~$3.79** instead of paying
  the whole-repo sweep (~$20-60 to crawl to the chunks you care about). This is the
  way to dogfood Willy affordably: scope to a package, get a converging run for a
  few dollars.
- **4 genuine pkg/store production-readiness blockers**, all verified real against
  the code, repatriated as `a79ffa76` (+ `hardening_test.go` regression tests the
  fixer's own plan called for but never wrote):
  - **B1** run dir/run.log created `0755`/`0644` (logs hold prompts/outputs/secrets →
    world-readable) + path built before run-ID validation → private `dirPerm`/`filePerm`
    + validate-first; `TeeRunLog(storeRoot, runID)`.
  - **B2** `AppendEvent`/`scanMaxSeqLocked` skipped `SanitizePathComponent`
    (traversal-defense asymmetry vs Load*/Artifact/Interaction).
  - **B3** `CreateRun` clobbered an existing `run.json` (run-ID reuse/race reset
    status/checkpoint) → exclusive create (`WriteFileAtomicNew` hard-link, `fs.ErrExist`).
  - **B4** a torn final JSONL line after a crash lost the first post-resume event →
    separate-with-newline repair.

### Findings / misses (bot + engine)
1. **fix_gpt returns a "work in progress" placeholder and ships unverified edits.**
   The fixer edited 5 files (9.3 min, 29k tokens) but its final turn produced empty
   output (`raw_output_len: 0`, `formatting_pass_used: true`), so iterion's
   formatting pass synthesized `applied=false` + "validating and fixing". Two
   regressions rode through unverified: **B4** `ReadAt` on a write-only fd (EBADF →
   broke *every* event append; 6 test failures) and **B1** a missed caller in
   `pkg/cli/resume.go` (build break). Root cause: the **review** prompt forbids
   provisional verdicts; the **fix** prompt did not. Fixed — FINALIZE guard in
   `fix_system` (`3be59a70`): no placeholder + self-verify with the project's own
   build/test + update ALL callers.
2. **The loop cannot catch a fixer that breaks the build (no deterministic post-fix
   gate).** Reviewers review their chunk's *source*; a cross-file build break
   (`resume.go`, outside the pkg/store scope) or a runtime-only bug (B4) is invisible,
   so the loop rebuilt a clean streak on a broken tree and would have
   **converged-on-broken**. The fix is the CLAUDE.md "deterministic gate" pattern:
   run the repo's OWN build/test as a `tool`/`compute` gate after fixes / before
   `commit_changes`, degrading the run if red. Universality-constrained (the
   build/test command is per-repo) → skill-guided detect + deterministic gate, like
   sec-audit-source's `scan_health`. **Recommended follow-up — not yet implemented.**
3. **scope_globs ↔ out-of-scope callers**: a scoped review can't see a signature
   change's callers outside the scope. The FINALIZE "update ALL callers" clause
   mitigates from the fixer side; the build gate (2) is the deterministic backstop.

### Engine / repo hardening produced
- `ec2752ca` + `deec5543` — `scope_globs` (feature + README); `f3df9cc7` — catalog regen.
- `a79ffa76` — pkg/store B1-B4 hardening + 4 regression tests.
- `3be59a70` — `fix_system` FINALIZE guard (no placeholder + self-verify).

### Lessons for next run
- Don't trust fix_gpt's `applied` flag or summary — **always run a build/test gate on
  the worktree before repatriating** (until the deterministic gate node lands).
- gpt-5.5 is a strong **reviewer** (4 real, well-analyzed blockers) but an unreliable
  **fixer** at the budget ceiling (placeholder + unverified edits). Until the build
  gate exists, prefer routing fixes to `claude_code` even for gpt-found blockers.
- For a clean convergence demo, scope tighter and raise `max_review_passes`; the
  persisted streak carries multi-run convergence regardless.

## 2026-06-14 — convergence machinery re-confirmed + path-scope finding (run 019ec598, cancelled)

- Status: **partial** (machinery confirmed; cancelled before the scoped edit —
  by design, see finding). Run on a clean iterion clone via the C082 worktree
  studio (non-watchexec, so no self-kill), `improvement_prompt` scoped to
  "pkg/log/ only", `merge_into=none`.
- Machinery: **confirmed healthy.** `alt` round-robin → `reviewer_claude`/
  `reviewer_gpt` → `streak_check` → `snapshot_chunk` turned correctly; reviewers
  emitted clean cross-family verdicts and `streak_check` accumulated approvals (4
  chunks swept, `review_loop=2`). No oscillation, no crash (Willy has a python
  state node but it does NOT parse json arrays from env, so it's immune to the
  Seki-class shape bug).
- **Finding — no path-scope glob → focused runs pay full-repo cost.** A
  `pkg/log/`-only `improvement_prompt` does NOT prune the chunk set: Willy still
  chunks the WHOLE repo and the reviewers no-op every non-pkg/log chunk
  (`"No action required... zero pkg/log/ source files"`) at ~$0.5/review/chunk.
  iterion has ~30+ packages, so a single-package focus would burn ~$30 of review
  to reach the one relevant chunk. `improvement_prompt`/`scope_notes` are prose
  (the WHAT), not a path filter (the WHERE). Recommended enhancement: add a
  `scope_globs` var (like sec-audit-source's `code_scope_globs`) that prunes the
  chunk plan, so focused improvements skip irrelevant packages. Cancelled here
  once the machinery + this finding were clear, to avoid the full-repo spend.
- Note: Willy's improvement *value* (catching/fixing a real dropped error) was
  already validated in the 2026-06-13 run below; this run targeted convergence +
  the scope behaviour, not re-proving value. Willy does not emit to the board.
- Lessons for next run: for a single-package improvement, either accept the
  full-repo sweep cost or use a different tool; pushing for a `scope_globs` var
  is the real fix. Whole-repo axes (e.g. "all error handling") are Willy's
  intended sweet spot, where the full sweep is correct.

## 2026-06-13 — bounded error-handling dogfood (run 019ec0c8)

- Status: **partial — core value validated, full convergence NOT reached** (the run
  killed itself, see finding #1).
- Versions: bot whole-improve-loop 0.3.0 · iterion 9197bcfd (v0.14.0)
- Method: launched via Studio `POST /api/runs`, scoped to a low-risk axis
  (`improvement_prompt` = surgical Go error-wrapping / nil-checks; `scope_notes`
  = minimal diffs; `max_review_passes=3`), `--merge-into none`, default
  `workspace_dir`. Backends: `claude_code` opus-4-8 (reviewer/fix), `claw` gpt-5.5
  (other family). `sandbox-full:edge`. ~3.7 min, ~$1.15, ~18k tokens counted before
  the run was cancelled.
- Result: `snapshot_chunk` (chunked iterion into **22 chunks / 1515 files / 5.8M
  est tokens**) → `alt` → `reviewer_claude` (found a real blocker) → `streak_check`
  → `fix_claude` (applied a correct fix) → **cancelled by a watchexec-triggered
  studio restart** (`error: "server drained: studio process shutting down"`,
  `failed_resumable` at `fix_claude`, review_loop=1).

### Value (the core loop works and finds real issues)
- **Reviewer found a genuine, on-axis bug**: `cmd/iterion/scan_shards.go`
  (`dispatchCloud`) had `req, _ := http.NewRequestWithContext(...)` — a silently
  dropped request-construction error. Precise `file (func)` localisation.
- **Fixer applied a correct, surgical fix**: `req, err := …; if err != nil {
  r.Error = …; return }`, matching the file's existing error-handling style.
  Compiles + `go test ./cmd/iterion` green. **Integrated to main as `4c525a6e`.**
- So Willy's value proposition (cross-family review finds real issues; fixer makes
  correct surgical edits) is demonstrated even though the run didn't finish.

### Findings / misses
1. **Willy self-kills under `task studio:dev` (CRITICAL — dogfood infra).** Willy
   edits the **live main working tree** (it has `sandbox:` but **no `worktree:
   auto`**, and no per-run worktree was created — confirmed `.iterion/worktrees/`
   empty for this run). `task studio:dev` runs the backend under
   `watchexec -r -e go -w cmd -w pkg -w vendor`. So the instant `fix_claude` wrote
   `cmd/iterion/scan_shards.go`, watchexec restarted the studio backend, which
   drained the in-flight run → `context canceled` → `failed_resumable`. **Any
   code-editing bot that touches `cmd/`/`pkg/` on the live tree will be cancelled by
   its own edits under the dev server.** Mitigations: run such bots against a
   non-watchexec studio (built `iterion studio`/`server`), or via a CLI
   `iterion run` in an independent process, or on an out-of-tree workspace copy.
2. **No worktree isolation (design tension — engine/bot).** Willy mutates the
   operator's actual checkout and (by design) leaves the edits uncommitted for
   review — but with no isolation it (a) pollutes the live tree, (b) self-destructs
   under file-watchers (#1), (c) risks losing edits on any restart. Billy
   (`branch-improve-loop`) and Featurly (`feature-dev`) use `worktree: auto` +
   a commit step. **Recommendation:** give Willy `worktree: auto` + a commit-on-
   convergence step (consistent with Billy), or at minimum document loudly that it
   edits the live tree. ADR-level decision, not a quick patch.
   **Evaluated 2026-06-13 — deferred.** The clean `worktree: auto` move has a real
   conflict: Willy's cross-run convergence relies on `.whole_improve_loop.state`
   (cursor + clean_streak) persisted at the **workspace root** to amortize the
   num_chunks-deep sweep across re-dispatches (issue-#12 / ADR-011). A `worktree: auto`
   worktree is created fresh from HEAD and **removed on finalize**, so that state would
   vanish each run → cross-run streak amortization breaks (every run re-sweeps from
   cursor 0). Doing it correctly means relocating the state off the ephemeral worktree
   (run-store or parent repo) **and** adding a commit step — a genuine ADR, not a patch.
   Since #1 is also solvable operationally (CLI launch / non-watchexec studio), the
   worktree change is deferred pending that ADR rather than rushed.
3. **Chunk grouping can exceed `max_review_chunk_tokens` ~7× (coverage).** Chunk 0
   was **218K est tokens / 149 files** against the 30K default budget; the renderer
   then hard-caps content at `budget*4+4096` (~124K chars), emitting
   `"... [chunk content truncated at the char cap] ..."` — so files grouped past the
   cap are **silently unreviewed** even though they count in `file_count`. The
   per-chunk grouping (by directory) doesn't split a group that overflows the
   budget. Worth bounding chunk size to the budget (or splitting oversize groups).

### Engine hardening
- `cmd/iterion/scan_shards.go` dropped-error fix — committed `4c525a6e`.
- Findings #1–#3 are recommendations (watchexec-incompat documented in CLAUDE.md;
  worktree-isolation + chunk-budget are deferred design/engine follow-ups).

### Lessons for next run
- **Do not dogfood Willy (or any live-tree code-editing bot) under
  `task studio:dev`** — its own edits trip watchexec and cancel the run. Use a
  non-watchexec studio or a CLI launch in a separate process.
- A whole-iterion convergence run is heavy (22 chunks / 5.8M tokens) and won't reach
  the cross-family asymptote under a small `max_review_passes`. For a convergence
  validation, point Willy at a **bounded** workspace (as Billy was pointed at a
  bounded branch diff), and raise the budget.
- Willy's reviewer/fixer quality is high; its weak points are *operational*
  (isolation + watcher interaction), not the LLM loop itself.
