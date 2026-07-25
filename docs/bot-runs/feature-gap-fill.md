[← Bot runs](README.md)

# feature-gap-fill (Fini) — bilans

Gap-driven feature completer. A specialisation of feature_dev: the input is a
structured `gap_spec` ("here is what's implemented, here is what's missing"),
not a greenfield prompt. Fini reads the partial implementation and, in ONE
`campaign` agent, completes the missing parts committing each unit in stride,
gated by the deterministic verify gate (`verify_build` writes the repo's real
build+test into `verify.sh`, `verify_run` re-runs it on the actual exit code) with
`gate.converged` closing a bounded continuation loop. Inputs are typically the
`type:feature-gap` issues filed by the adr-cartograph (Adry) bot.

## 2026-07-07 — SANDBOXED path validated end-to-end after root-causing native:221edac8 (run 019f3e27)
- Status: **VALIDATED (sandboxed)** — closes the sandbox blocker from the morning's dogfood party. Same gap_spec as the party runs, full sandbox (`iterion-sandbox-full:edge`), zero delegate retries.
- Versions: bot v2.0.0 · iterion `dev+a239f80eb` (the fix stack below).
- Method: CLI run from a fixture clone at a **neutral path** (`/tmp/iterion-probe-221e/fini-fixture3` — NOT the Claude scratchpad, see defect 3), `--store-dir <workspace>/.iterion`, `--merge-into none`, `post_to_board=false`, `--max-cost-usd 8`. ~9 min wall.
- Result: `finished`; campaign `gap_closed=true, commits_this_pass=1, needs_human=false` → verify_run `passed=true` → gate `converged=true`; storage branch `iterion/run/magneto-chase-vortexvape-5c36` @ 3d7996637b9d `feat(user): validate empty name and email without '@', add table-driven tests`; **0 delegate_retry / 0 delegate_error** on the whole run (vs 100% cold-abort pre-fix); container cleaned up, no leaked claude processes.
- Engine hardening — native:221edac8 was THREE stacked defects, all closed:
  1. **`~/.claude.json` not mounted** (root cause): host_state carried the `~/.claude` dir but not the sibling top-level config file; in-container claude saw the host's config backups (inside the mounted dir) with no config → manual-restore stderr loop, zero stdout forever → 90s cold-abort every attempt. Fix `a239f80eb` (+ regression test). Bisect proof: prompt-arg claude printed the restore error; stream-json hung mute; with the mount the init frame arrived in 3s.
  2. **The leak**: aborting the docker-exec client never killed the in-container claude; in-executor retries stacked (3-5 observed). Fix `ec31b056f` (pidfile-then-exec wrapper + deferred TERM→KILL) + `834a53a5c` (wrapper self-cleans on respawn). Proven live: exactly 1 claude + 1 pidfile across retries.
  3. **Dogfood-method artifact** (not an iterion bug): probe fixtures lived under `/tmp/claude-1000/…` (the operator-agent scratchpad); docker creates the bind's parents ROOT-OWNED in-container, shadowing claude's own temp root `/tmp/claude-$UID` → silent pre-init hang even with 1+2 fixed. Surgical A/B: same fixture at `/tmp/probe-fixture` boots in 3s, under `/tmp/claude-1000/` hangs. Now documented in CLAUDE.md (dogfood section).
- Side-finding: native:e6cd506e — sandboxed board MCP HTTP endpoint unreachable on Linux docker (no host-gateway alias + loopback-bound listener); claude tolerates it (tools absent, no hang).
- Lessons for next run: sandboxed dogfoods are unblocked; always clone fixtures to a neutral path; `ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT` takes a Go duration (`5m`), not a bare number.

## 2026-07-07 — v2 dogfood: gap closed + tests green in 4m06s (run 019f3d6e, after a sandbox saga on 019f3d4d-07b8)
- Status: **VALIDATED** (no-sandbox variant) — the v2 mechanism is proven end to end; the SANDBOXED path is blocked by an engine bug filed as native:221edac8 (since closed — see the 019f3e27 bilan above).
- Versions: bot v2.0.0 · iterion `dev+239203525cc8`.
- Method: CLI run FROM the fixture repo (cwd = the target — lesson below), `--store-dir <workspace>/.iterion`, `--merge-into none`, `--sandbox none`, gap_spec = User.Validate stub + missing validations + table tests, `--max-cost-usd 15`. 4m06s wall, converged first pass.
- Result: `finished`, `gate.converged=true`. **1 commit in stride** (`feat(user): validate empty name and email without '@'` + `Bot: feature-gap-fill` trailer) on `iterion/run/pixel-leap-borealroar-d544` — all 3 missing items on the existing seam, granularity defensible (same function + its table test). Deterministic verify: go build + go test green. Functional proof from the delivered branch: `TestUserValidate` 3/3 subtests PASS.
- Findings / misses (the saga — 3 lessons, all operator/engine, none bot-contract):
  1. **Launching a sandboxed bot against a fixture repo requires cwd = the fixture** (or the engine's workdir), NEVER `--var workspace_dir=<path>`: the sandbox mounts the RUN's workspace, so an out-of-tree workspace_dir doesn't exist in-container. First launch (019f3d4a) mis-launched that way, cancelled.
  2. **Engine bug native:221edac8**: on the properly-launched sandboxed run (019f3d4d-07b8), the in-container claude emitted ZERO bytes for 90s repeatedly; each cold-abort LEAKED the claude subprocess in-container (4-5 stacked), compounding. Auth is healthy in an identical throwaway container (claude auth status OK, 2.1.175) and the same bot ran perfectly with `--sandbox none` — the sandboxed delegate stdin/stream path is the suspect. Testy's sandboxed run worked 90min earlier, so it may be load/ordering sensitive.
  3. `ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT` takes a Go duration (`5m`), NOT bare millis — `300000` parses as invalid → silent 90s fallback (worth a startup warning; candidate small fix).
- Engine hardening: native:221edac8 (subprocess leak + stream-abort) filed severity:high with the full evidence.
- Lessons for next run: keep `--sandbox none` for fixture-repo dogfoods until 221edac8 lands; re-run the sandboxed variant afterwards to close the loop.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: ONE campaign agent closes the gap spec (living todo from a seam survey, one verified commit per missing item, preservation discipline + Adry ADR-ownership in the contract, findings→board) against the deterministic verify_build/verify_run gate + bounded continuation_loop. 12 nodes → 5 exec; the survey/plan/act/simplify chain and the round_robin cross-family review/fix loop are retired.
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-14 — clone transport validation, SANDBOXED (run 019ec75f)

- **Status: validated** — Fini produced a real, correct, tested security fix
  from Adry's gap spec and converged cleanly. Independently verified (tests run
  on host).
- **Versions:** bot 0.1.0 · iterion @ `030031f6` (main)
- **Method:** `ITERION_OPENAI_USE_OAUTH=1 ./iterion run
  bots/feature-gap-fill/main.bot --var gap_spec='<08aaf4ef body>' --store-dir
  .iterion --merge-into none`. gap_spec = Adry's `bot-marketplace-shallow-clone`
  issue (`Add transport validation for bot marketplace shallow clone sources`).
  **Ran SANDBOXED** (`iterion-sandbox-full:edge`, `worktree: auto`) — unlike the
  019ec599 run which forced `--sandbox none`. Forfait forced.
- **Result: converged + committed.** survey_existing → plan → act → simplify →
  **2 review cycles** (reviewer_claude→streak_check→reviewer_gpt→streak_check,
  cross-family double-approval, **0 fixer iterations, no oscillation**) →
  prepare_commit → commit. **$3.52 / ~16 min / 58k tokens.** Committed
  `a7f44eb3` ("feat(git): gate git clone sources to safe https/ssh transports",
  **4 files +154/-2**) on storage branch `iterion/run/chrome-surge-scalarsmol-9428`.
  I reviewed + validated (`go test ./pkg/git ./pkg/botinstall` → ok) then
  FF-merged to main. Board 08aaf4ef → done.
- **Value: high — real security hardening.** `ValidateCloneSource`
  (pkg/git/safety.go) allowlists `https://`/`ssh://`/scp-like and rejects the
  `::` remote-helper marker (catches `ext::` arbitrary-command-exec) +
  `file://`/`git://`/`http://`/bare paths; `ShallowClone` now calls it (keeping
  `--` as flag-injection defense-in-depth). `clone_test.go` tests 6 accept + 12
  reject cases incl. the error-message acceptance criterion. Survey was
  excellent: found ADR-020 as the doc home, identified the HTTP marketplace
  routes as the actual untrusted surface, noted `ValidateRelPath`/
  `ValidateBranchName` pre-existed (stayed in scope).
- **Engine/forfait health:** clean run — **no `StructuredOutput: no such tool`
  error** (d8e8dde1 holding on schema+tools claude_code nodes), no forfait
  flakiness, no retries. **Sandboxed claude_code + reviewer_gpt (claw/forfait)
  both ran cleanly in-container** — the sandbox path is viable for Fini now
  (contrast the 019ec599 bilan's "host-mode until ~/.codex mounted" caveat).

### Findings / misses
- **Fini cannot run the test suite in-sandbox** (`devbox cache permission-denied`
  + host `go` can't fetch the 1.26.0 toolchain in the container). Reviewers
  approved by *reading* the diff; tests were never *executed* by Fini. So a
  human/CLI must `go test` on the host before merge (I did → pass). Known
  "devbox silently broken in sandbox" limitation.
- **08aaf4ef ↔ 50bbe258 interaction conflict (Adry gap-spec coupling).**
  50bbe258 asks to test `ShallowClone` hermetically via a `file://` URL — but
  08aaf4ef's new gate **rejects `file://`**, obsoleting that approach. The
  empty/whitespace-guard intent is now covered by `clone_test.go`
  (`ValidateCloneSource` is the guard); the actual-clone cases (with/without
  ref, stderr-wrap) can't be tested hermetically post-gate (would need a
  `cloneArgs(url,ref,dest)` extraction or a test seam). **50bbe258 needs
  re-scope; left in inbox, not run as-is.** Lesson: when Adry files multiple
  gap tickets on one function, one fix can invalidate another's approach.
- Good bot judgment on out-of-scope items: flagged `http://` rejection as a
  possible config need (internal cleartext servers) and the `go.mod` `yaml.v3`
  indirect→direct drift, both as *observations*, not scope creep.

## 2026-06-14 — first dogfood, file-diff size-cap gap (run 019ec599)

- **Status: validated** — Fini produced real, correct, tested code from a gap
  spec and converged cleanly. NOT a façade (verified independently).
- **Versions:** bot 0.1.0 · iterion @ `03f398e2` (main)
- **Method:** `iterion run bots/feature-gap-fill/main.bot --var gap_spec='<…>'
  --sandbox none --merge-into none`, gap_spec = the **file-diff-payload**
  issue Adry filed (`Cap file contents loaded for Monaco diff payloads`).
  Launched under `devbox run` with `~/.local/bin` on PATH so the host run had
  both `go` (devbox) and `claude` (host) — sandbox forced off because this
  worktree's engine predates the concurrent sandboxed-claw fixes and `~/.codex`
  (forfait) is not mounted into the container. `worktree: auto` still isolated
  the Go edits; forfait forced (`ITERION_OPENAI_USE_OAUTH=1`).
- **Result: converged + committed.** Flow: survey_existing → plan → act →
  simplify → reviewer_claude + reviewer_gpt **both approved cross-family on the
  first pass (0 fixer iterations)** → prepare_commit → commit_changes → done
  (~18 min, 12:07→12:25). Committed `88943d4b` ("feat(git): cap diff payload
  reads to avoid OOM on oversized files", **5 files, +267/-30**) on the
  storage branch `iterion/run/magneto-whomp-etherspark-4f0a` — **NOT on main**
  (`--merge-into none`), preserved for human review. The engine recorded
  `final_commit` + `final_branch` and removed the worktree cleanly.
- **Value: high — a genuine OOM fix.** The implementation is substantive and
  correct: an `errOversized` sentinel + `diffPayloadCap` (reuses
  `untrackedReadCap`'s 5 MiB), the reading primitives (`readWorktreeFile`,
  `showAt`) return oversized **before** loading the blob into memory, both
  sides blanked + `Oversized=true`, oversize-wins-over-binary — meeting all the
  issue's acceptance criteria. It also ADDED `pkg/git/diff_test.go` (160 lines:
  `TestDiffOversizedWorktree`, `TestDiffOversizedHead`), exactly as the
  acceptance criteria required.
- **Anti-façade verification:** I checked out `88943d4b` into a throwaway
  worktree and ran `go build ./pkg/git/` + `go test ./pkg/git/` independently
  → **builds clean, tests pass** (`ok 1.162s`). Real work, not a reported
  parity façade.
- **Pipeline validated:** the full A→C handoff works end-to-end — **Adry found
  the gap → filed a `type:feature-gap` issue → Fini completed it** with tested
  code. This is the architecture-evolution loop the suite was designed for.

### Lessons for next run
- Host run (`--sandbox none`) needs BOTH `go` and `claude` on PATH: launch
  under `devbox run` (go) with `~/.local/bin` appended (claude). The sandbox
  path is cleaner *if* `~/.codex` (forfait) is mounted into the container —
  it currently is not, so host-mode is the reliable forfait path until that's
  wired (or an API key is used).
- The implementation lands on a storage branch (`--merge-into none`) for human
  review — the operator merges it (or routes it through adr-rechallenge) rather
  than Fini pushing to main. Correct default for an autonomous code-mutating bot.
- Convergence was clean (0 fixers) because the gap_spec was precise (Adry's
  issue carried evidence + acceptance criteria). A vague gap_spec would lean
  harder on the review-fix loop.
