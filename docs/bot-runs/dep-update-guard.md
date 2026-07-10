# Vetty — `dep-update-guard` run bilans

Reactive security + alignment guard for automated dependency-update PRs
(Dependabot / Renovate): audit the bump (supply-chain + CVE), align the
consuming code, prove the tree with the deterministic verify gate,
commit onto the PR branch, post the verdict comment. Never merges. See
[bots/dep-update-guard/](../../bots/dep-update-guard/).

## 2026-07-10 — first CLOUD runs on a real Dependabot PR: HOLD verdict with a real CVE finding, then safe re-verdict (runs 019f4ba8 / 019f4bcb / 019f4d3b)
- Status: **VALIDATED (cloud, real forge PR)** — the two paths the 07-07 bilan
  asked for both ran live: a real Dependabot PR
  ([#80](https://github.com/SocialGouv/iterion/pull/80), go-minor-patch, 22
  modules) and the real `post_feedback` forge POST, verdict comments posted
  under the App identity `iterion-forge-83fde406[bot]` with re-fetch verify.
- Versions: bot v2.0.0 · iterion `499957c31`→`6dd452c2a` (fixes landed mid-session).
- Method: `/vetty` PR comment → webhook command route (`scope: pr`, mode
  direct) → cloud runner (no sandbox, devbox image). ~$1.6/run, 5 min.
- Result & value: run 019f4bcb produced an exemplary **HOLD**: OSV batch over
  all 30 bumped (name, version) pairs → zero malicious/typosquat; the vendored
  wails `package.json` bump audited for lifecycle hooks (devDependencies only);
  **and the real finding — the PR bumps `x/crypto` 0.50→0.51 while the fix for
  7 CRITICAL + 2 HIGH SSH/agent advisories is 0.52.0, one minor short** — so
  the guard's tie-break (unsure → suspicious, a hold is cheap) fired exactly as
  designed. After Billy pushed the 0.52.0 bump, run 019f4d3b re-audited
  (osv-scanner v2.4.0 over all 134 pinned packages) and returned the clean
  ✅ safe/aligned verdict with an honest `committed=false, no alignment needed`.
- Findings / misses (engine, not bot): run 019f4ba8 no-op'd (`is_empty: true`)
  because the PR-comment command path launched on the DEFAULT branch — the
  `issue_comment` payload carries no head ref. Fixed in-session:
  `499957c31` resolves the PR head/base via the forge API at command time
  (failure = loud 502, closed PR = filtered). Without local scanners on the
  runner image the audit adapted (OSV REST batch), matching the
  skill-not-DSL universality contract; osv-scanner appeared in the later run.
- Lessons for next run: point it at an npm/pypi dep PR to exercise non-Go
  ecosystems; consider shipping osv-scanner in the runner-devbox image so the
  floor doesn't depend on the agent installing it.

## 2026-07-07 — first live dogfood: clean-bump path end to end, audit evidence exemplary (run 019f3d73)
- Status: **VALIDATED** (no-sandbox variant, clean-bump path) — every stage behaved with the exact honesty the v2 contract demands; the with-alignment path and a real forge POST remain to be exercised on a live PR.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8`.
- Method: CLI run FROM the PR-branch checkout of a Go fixture (bare origin + `dependabot/go_modules/...` branch bumping github.com/google/uuid 1.5.0→1.6.0), `--sandbox none` (sec image blocked by native:221edac8), pr_url empty (no forge), `--max-cost-usd 12`. ~11 min wall.
- Result: `finished`. prepare (deterministic) correctly scoped go.mod+go.sum; **security_audit verdict=safe with model evidence**: govulncheck actually RUN (no reachable vulns), OSV API queried for BOTH versions **with a query-shape control against a known-vuln package**, go.sum hashes checked against sum.golang.org's transparency log, and the absent image scanners honestly listed `not_available` ×3. align: `applied=false` with proof (`NewString()` stable; build+vet run) — no invented edits. Deterministic verify_run: real exit 0 (build+vet against the bumped dep). validate_gate stable → commit node: `committed=false, "no alignment needed"` — the honest no-op. post_feedback skipped (`no pr_url`, posted=false, never pretended); feedback_health degraded=false.
- Value: the v2 calibration is vindicated live — the read-only audit stage produced real, verifiable evidence, and the deterministic verify (which replaced the self-reporting v1 validate) gated on a real exit code.
- Findings / misses: none on the bot. The earlier sandboxed attempt (019f3d56) fell to native:221edac8 (in-container stream zero-byte + subprocess leak) and to an operator docker-exec cleanup that killed the container — recorded there.
- Lessons for next run: exercise (a) a breaking bump so align/commit actually land code on the PR branch, and (b) a real forge PR so post_feedback's REST + re-fetch verify path runs; both ideally back in the sec sandbox once 221edac8 lands.

## 2026-07-07 — converted to v2 calibrated shape (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — no live run yet in ANY shape (this file is new); structural validation only (`iterion validate` clean, catalog tests green).
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07).
- Shape: the LLM `validate` self-report was replaced by the deterministic verify_build/verify_run gate (fail-closed — no verify.sh ⇒ no commit); the read-only security_audit DELIBERATELY stays separate from the mutating align (anti-prompt-injection separation behind a deterministic verdict gate), and commit-after-green stays (shared PR branch). align gained the G5 pre-existing-failure protocol; the dead pr_review_mode var is gone.
- Next: first live dogfood on a real Dependabot/Renovate PR + bilan here.
