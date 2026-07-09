# secured-renovacy (Renovacy) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

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
