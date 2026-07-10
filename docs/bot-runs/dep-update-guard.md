# Vetty — `dep-update-guard` run bilans

Reactive security + alignment guard for automated dependency-update PRs
(Dependabot / Renovate): audit the bump (supply-chain + CVE), align the
consuming code, prove the tree with the deterministic verify gate,
commit onto the PR branch, post the verdict comment. Never merges. See
[bots/dep-update-guard/](../../bots/dep-update-guard/).

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
