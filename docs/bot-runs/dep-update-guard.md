# Vetty — `dep-update-guard` run bilans

Reactive security + alignment guard for automated dependency-update PRs
(Dependabot / Renovate): audit the bump (supply-chain + CVE), align the
consuming code, prove the tree with the deterministic verify gate,
commit onto the PR branch, post the verdict comment. Never merges. See
[bots/dep-update-guard/](../../bots/dep-update-guard/).

## 2026-07-14 — cloud run on a real Dependabot go-minor-patch PR (#182); excellent audit/align/verify, wired via `/vetty` command (run 019f60cf)

- Status: **VALIDATED (cloud, real forge PR)** — Vetty ran end-to-end on a live
  Dependabot PR ([#182](https://github.com/SocialGouv/iterion/pull/182),
  go-minor-patch, 10 modules incl. x/crypto, mongo-driver, aws-sdk, go-selfupdate)
  and produced a correct verdict. The `post_feedback` comment step failed the first
  time on the Anthropic forfait's **session rate-limit** (`failed_resumable`); a
  fresh token + `iterion remote runs resume` finished it.
- Versions: bot dep-update-guard v2.0.0 · iterion runner `:edge` (this session's
  `:edge`, digest 42665a30, i.e. incl. #178/#180/#184/#185) · claude_code backend on
  claude-opus-4-8 via the Anthropic OAuth forfait.
- Method: **wired the bot** via `POST /api/teams/{id}/forge/repo-bots`
  (`bot_ids:[dep-update-guard]`, GitHub App `iterion-forge-61934180[bot]`, forge_token
  `forge_github_f73ba902`) → registers the `pull_request` + `pull_request_comment`
  webhook and the `/vetty` command. Triggered deliberately with
  `gh pr comment 182 --body "/vetty"` (routes only to dep-update-guard; the comment
  gate checks the commenter's CollaboratorPermission).
- Result: converged. audit ($0.87) → align (no changes; all minor/patch, no breaking
  API in-tree) → verify_build ($1.12) wrote an out-of-tree `verify.sh`
  (`go build -mod=vendor ./...` + a vendor-drift `go mod vendor` + `git diff --quiet`
  check) → verify_run gate GREEN → verdict "safe, no alignment, nothing pushed".
- Value: a genuine supply-chain audit — queried the **OSV API per package**, correctly
  identified that the x/crypto bump *resolves* CVE-2025-58181/47914, flagged a
  version discrepancy in the PR description, and reasoned correctly about the
  blocking criteria (not a new HIGH/CRITICAL → don't block). This is the reference
  "dependency-PR guard" behaviour working on a real PR.
- Findings / misses:
  - **#1 (FIXED, engine)** — the skill mirror produces the CC-2.x directory form
    `.claude/skills/<name>/SKILL.md`, but Vetty's prompt (and ~8 other catalog bots)
    Reads the flat `.claude/skills/<name>.md`. The Read failed twice + cost a
    filesystem `find` before recovering from the baked `/opt/iterion/bots/...` copy —
    a recovery **absent on a non-iterion target repo**. Fix: `mirrorFileSkill` now
    writes the flat alias too (PR #187, `pkg/runtime/bundle.go`).
  - **#2 (FIXED, security)** — the forge integration auto-launched improve/review
    bots on *every* PR incl. fork PRs (untrusted code + budget-exhaustion vector) and
    dependency PRs. Fork PRs are now never auto-launched (a repo collaborator triggers
    manually via `/command`, gated on CollaboratorPermission); dep-bot PRs never route
    to the improve loop (PR #189, `pkg/server/webhooks_{github,common}.go`).
  - **#3** — `verify_build` is slow (~17 min) on a cold devbox `go build`; ~$2 total
    run cost. Acceptable; a shared go/devbox cache (ADR-066-bis) would help.
  - **#4** — the Anthropic forfait has a **~5h session rate-limit**; a long run + many
    same-session runs exhaust it and the *last* node (`post_feedback`) failed
    `rate_limited`, losing an otherwise-complete verdict until resume. **#4b
    (follow-up):** make `post_feedback` a DETERMINISTIC tool node (compose the comment
    from the structured verdict + POST via the forge REST API) so it needs no LLM turn
    — resilient to rate-limits, faster, cheaper.
- Engine hardening: PR #187 (skill mirror flat alias) + PR #189 (fork/dep-bot webhook
  guards) — both dogfood-driven.
- Lessons for next run: keep triggering via `/vetty` (controlled, one bot, authz-gated)
  rather than the auto pull_request path; provision a FRESH forfait token before a long
  run (the access token is short-lived and the session limit is real); land #4b so a
  rate-limit at the comment step can't sink a good verdict.

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
