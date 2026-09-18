# secured-renovacy (Renovacy) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

## 2026-09-18 — DSL profile 2 dogfood on an npm fixture: patch batch + minor solo, Phase 2 converged first pass (run 01a0b32b-2d7e)
- Status: **validated** (no-sandbox variant, two-package scope) — both Phase-1 paths and the Phase-2 campaign behaved as designed in 16.8 min.
- Versions: bot secured-renovacy 0.2.3 (`dsl: 2`, the wave-4 PR of #1344) · iterion v3.162.2 (`d98abd04d`; the engine did not change in this wave).
- Method: CLI run FROM a two-dependency npm fixture (`ms` 2.1.1 patch-outdated, `debug` 4.3.0 minor-outdated carrying the real ReDoS advisory fixed in 4.3.1; two assertions in `npm test`), `--sandbox none --merge-into none --no-interactive`, `--max-cost-usd 12 --max-duration 40m`, vars `scope=patch,minor max_packages_per_run=2 max_review_passes=1 update_scope=libraries`. Every agent on claude_code/claude-opus-5 as declared (p2_campaign through the `zai` provider default).
- Result: `finished`, $3.51, 454k tokens, 28 node executions, storage branch `iterion/run/pixel-spin-owlspark-d4c5` (two commits: `chore(deps): batch update 1 patch upgrades` e2d34b7 = ms 2.1.3, `chore(deps): update debug to 4.4.3` 180db5f), +11/−18 lines on package.json + package-lock.json. Phase 1: detect_stack → discover (both found) → the patch batch upgraded and committed ms (its audit note to the run's artifact-files) → bucket_families (none) → solo: debug through the intel fan-out (changelog_review 3 min and security_audit 2 min in parallel: safe, no alignment needed) → upgrade → install → validate_upgrade (stable) → commit → audit note → package_loop iteration 1 → select_candidate has_more=false (cap 2) → phase2_decider → p2_campaign (a minor was attempted). Phase 2: the campaign re-reviewed the cumulative diff with real checks (manifest↔lockfile consistency, `npm test`, audit) and reported clean without inventing fixes; p2_verify_build wrote verify.sh (exit 0, "ok - 2 assertions"); p2_verify_run green; the gate converged first pass; the SBOM (2 packages) went to artifact-files `renovacy/sbom-180db5faff21.json`.
- Value: real — two correct upgrades with per-package audit notes and an SBOM, and a Phase 2 that reviewed rather than rubber-stamped.
- Findings / misses: the three new exhaustion exits are not on this path; their witness is the strict dry run (the all-true pass no longer dies LOOP_EXHAUSTED at mark_family_attempted — it runs to the bot's own iteration ceiling). The loop caps now follow `max_packages_per_run` (one above it), so select_candidate's own exit reports the cap and only a back-edge declined for budget reaches the exits. The audit notes and the SBOM live in the run's artifact-files, not in the tree — the 07/07 bilan's "docs/renovacy/… inside the workspace" describes the earlier shape. Miss: none attributable to the bot.
- Engine hardening: none needed.
- Lessons for next run: a scope above the default 30 needs only `max_packages_per_run`, and the family fast-track only `max_families_per_run`; nothing else bounds the two loops any more.

## 2026-07-07 — P1+P2 dogfood on an npm fixture: 2 clean upgrade commits, real advisory handled, P2 campaign converged (run 019f3d7b)
- Status: **VALIDATED** (no-sandbox variant, small-fixture scope) — Phase 1's per-package pipeline and the NEW v2 Phase 2 both behaved as designed end to end in 10m50s.
- Versions: bot v0.2.0 · iterion `dev+239203525cc8` · `--sandbox none` (sec image path blocked by native:221edac8).
- Method: CLI run FROM an npm fixture (ms 2.1.1 patch-outdated + debug 4.3.0 minor-outdated carrying the real GHSA ReDoS advisory fixed in 4.3.1), `--merge-into none`, update_scope=libraries, scope=patch,minor, max_packages_per_run=2, max_review_passes=1, `--max-cost-usd 20`.
- Result: `finished`, P2 `p2_gate.converged=true`, SBOM emitted (docs/renovacy/sbom-c86e7111dfb9.json, 2 packages) on `iterion/run/thunder-hunt-orbitcrest-c493`. **Phase 1**: discover found both; the patch batch committed ms (`chore(deps): batch update 1 patch upgrades`, audit-trail amended in); the minor solo took debug through intel (safe; the START version's ReDoS advisory correctly did not block the clean 4.4.3 target) → upgrade → validate (stable, high) → `chore(deps): update debug to 4.4.3`. **Phase 2 (the ADR-058 conversion under test)**: phase2_decider routed to p2_campaign (minor attempted ⇒ no fast-track); the campaign REVIEWED the cumulative diff with real checks (npm ci --dry-run sync, tree dedup verified — nested ms removed, root 2.1.3 satisfies debug's ^2.1.3 —, npm test, npm audit 0 vulns, ReDoS closed) and reported `review_clean=true, commits_this_pass=0` without inventing fixes; deterministic p2_verify_run re-ran the suite (real exit 0) and the gate converged first pass.
- Value: real — two correct dependency upgrades with per-package audit trails + SBOM, and the new Phase 2 proved it reviews rather than rubber-stamps or fabricates.
- Findings / misses: select_candidate re-picked ms in a redundant SOLO right after the batch had landed it (attempted_after_batch ledger gap) — one wasted intel/upgrade cycle, no duplicate commit thanks to the empty-commit guard; filed as a board finding (severity low).
- Engine hardening: none new (the sandbox path remains gated on native:221edac8).
- Lessons for next run: a fuller dogfood should cover a BREAKING minor/major (fix_after_upgrade + revert paths) and the sec sandbox once 221edac8 lands; max_review_passes=1 (2 P2 passes max) is the right default for small repos.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v0.2.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: SCOPED pass: Phase 1 per-package pipeline untouched (ADR-055 unit-convergence — its security/CVE/revert/SBOM gates are deliberately reified). Phase 2's alternating relay (alt_review/reviewer_claude/reviewer_gpt/streak_check/fix_*/review_commit_auto + review_mode/mono_family) became ONE p2_campaign on the run's cumulative diff (git diff start_sha) + the deterministic p2_verify gate + review_pass_loop. With this, bots/review_topology_test.go's enforced list emptied and the file was deleted (machinery guarded by e2e/review_topology_test.go).
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-14 — first full validated run (safe mode, run 019ec5c5)

- Status: **validated.** Full end-to-end run in safe mode on a clean iterion
  clone; real upgrades applied, vendored, validated, committed to a storage
  branch — major bumps correctly skipped.
- Versions: iterion branch `c082-board-emit` (C082 worktree binary) ·
  secured-renovacy current.
- Method: dedicated worktree studio :4899, `worktree: auto` on a clean iterion
  clone, `sandbox: iterion-sandbox-sec:edge`. Safe-mode vars:
  `major_policy=skip`, `scope=patch,minor`, `max_packages_per_run=3`,
  `merge_into=none`. (`major_policy=skip` honours the standing "ask before
  `major_policy: attempt`" rule.)
- Result: `Run finished`; `final_commit be365eab` on storage branch
  `iterion/run/comet-haze-arctickazoo-3b09` (not merged — `merge_into=none`).
  Four commits: a batch of **15 patch upgrades**, plus minors
  `aws-sdk-go-v2/config v1.32.25`, `aws-sdk-go-v2/credentials v1.19.24`, and
  **`golang-jwt/jwt/v5 → v5.3.1`** (a security-relevant JWT lib bump). `vendor/`
  regenerated (117 files, +11257/-2927). Pipeline ran end-to-end:
  detect_stack → discover_outdated → bucket_patches → batch_upgrade_patches →
  install/validate → … → emit_sbom → done. No human pause was needed in safe mode.
- Value: **high.** Real, correctly-tiered dependency hygiene (patch+minor only,
  major skipped) with vendoring + a per-upgrade commit trail, on a repo it had
  never seen. The golang-jwt bump is the kind of security-relevant update this
  bot exists to surface.
- Robustness finding (positive): **devbox silently fails in the sec sandbox** —
  `~/.cache` is root-owned, so `devbox run …` returns EMPTY output, which a naive
  bot would read as "all dependencies up to date" (a façade). Renovacy's
  `discover_outdated` agent **detected the silent failure**, fell back to the
  image's `/usr/bin/go` (go1.26.0, matching go.mod) with writable `/tmp` caches,
  and **warned the downstream upgrade/install agents** to do the same. That's
  exactly the anti-façade behaviour the workflow-authoring pitfalls doc calls for.
- Engine/sandbox finding (to fix): the devbox wall above affects ANY sandboxed
  devbox-based bot (e.g. Devy's `devbox install` verify would hit it too). Root
  cause is `~/.cache` ownership inside the sec sandbox under `user: 1000:1000`
  with host-state mounts. Worth fixing (ensure `~/.cache` is writable by the
  container UID, or point devbox/Nix caches at a writable dir) so devbox bots
  don't depend on a host-go fallback.
- Finding (minor, recurring): claude_code nodes emit a spurious
  `Tool error: StructuredOutput — No such tool available: StructuredOutput`
  before recovering via iterion's fmt-pass — same family seen in Billy/Devy.
  Non-fatal here.
- Lessons for next run: safe mode (`major_policy=skip`, `scope=patch,minor`,
  small `max_packages_per_run`) is a reliable, bounded, valuable config. Fix the
  sandbox devbox `~/.cache` wall so detection doesn't rely on an agent noticing
  the silent failure. For a major-bump run, get explicit operator sign-off first.
