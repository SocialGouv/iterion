# Vetty — `dep-update-guard` run bilans

Reactive security + alignment guard for automated dependency-update PRs
(Dependabot / Renovate): audit the bump (supply-chain + CVE), align the
consuming code, prove the tree with the deterministic verify gate,
commit onto the PR branch, post the verdict comment. Never merges past a
check — and only ever the commit it audited. See
[bots/dep-update-guard/](../../bots/dep-update-guard/).

## 2026-07-29 — v2.4.0: the loop closed — Renovate → audit → gate → merge, unattended (run 019faef9)

- Status: **VALIDATED** — the first dependency PR to travel the whole chain
  with no human in it.
- Versions: bot 2.4.0 · iterion `v3.15.0+3a61d2da4` (runner `:edge`
  `sha256:8c625432`)
- Method: a real `renovate.yml` dispatch on `socialgouv/buildkit-operator`,
  authenticated as the dedicated `socialgouv-renovate` App.
- Result: PR #15 (`go toolchain 1.26.5 [security]`) created **17:43:03Z**,
  merged **17:58:04Z** by the forge App — **15 minutes** from Renovate opening
  it to the merge, with no human in between. `armed: true, reason: merged: the
  forge reported every required check already green`. Gate
  `iterion/review=success` posted on the head at 17:57:58Z; merge commit
  `6d02f3e46747`.

### What each link proved

- **The App switch is what unblocked everything.** Under `GITHUB_TOKEN` the
  four pre-existing Renovate PRs each had a `ci` run stuck in
  `action_required` with zero jobs — GitHub's anti-recursion rule. PR #15's
  `test`/`lint` started on their own. Nothing downstream can work without
  this, and no amount of bot logic substitutes for it.
- **The cooldown holds without hiding.** The dry run showed 17 upgrades marked
  `pendingChecks: true` with their held versions named (`undici` 8.8.0/8.9.0,
  …), while aged updates proceeded. `internalChecksFilter: strict` means the
  branch is not created at all rather than a PR opened with a pending check.
- **The merge targets the audited commit.** `commit` reported
  `committed: false` (nothing to align), so the pin fell back to `prepare`'s
  head — and the merge went to exactly that sha.

### The overclaim the run surfaced

The check displayed *"supply-chain audit clean; alignment committed, build
verified"* on a PR where the alignment was a no-op (1 commit, 1 changed file,
all Renovate's). The verdict is a graph PATH name stamped per-edge, so it read
`committed` whether or not anything was.

The first fix carried the commit agent's own `committed` flag down to the
message — which only moved the claim from one unreliable source to another,
since that flag is the agent grading its own work. The shipped version derives
it from two shas the run owns (`commit.sha` vs `prepare.head_sha`) and routes
to the `clean` verdict, which existed in every string table and was
unreachable. All the "committed" strings are now unconditionally true.

A required check that asserts work nobody did is the same false-statement
class this bot exists to catch in other people's diffs.

### Lessons for next run

- A PR whose base has moved far enough to conflict can never reach the merge:
  cancel the audit rather than spend it on a refusal that is already knowable
  from `mergeable`. Cost saved on this session: one 14-min run.
- Closing a stale Renovate PR is not a neutral cleanup — Renovate reads it as
  "this update was rejected" and stops offering it. Leave them; the bot
  rebases them under the new identity.

## 2026-07-29 — v2.1.0 live: the whole chain ran, the gate landed, and the merge never happened (run 019faad2)

- Status: **partial** — every step validated end to end except the last one,
  which turned out to be structurally impossible as designed.
- Versions: bot 2.1.0 → 2.2.0 · iterion `9d5efc6c` (runner image `:edge`
  `sha256:c499ba03`)
- Method: `/vetty` on
  [socialgouv/buildkit-operator#5](https://github.com/SocialGouv/buildkit-operator/pull/5)
  (a `golang:1.26` digest bump), cloud run on `ovh-prod`, sandbox
  `iterion-sandbox-sec:edge`, `gate_context: iterion/review`,
  `arm_automerge: true`.
- Result: finished in **14 min**, 15 nodes, no human intervention. `prepare` →
  `security_audit` → `align` → `align_gate` → `verify_build` (5 min) →
  `verify_run` **exit 0** → `validate_gate` → `commit` → `post_feedback` →
  `feedback_health` → `arm_automerge` → `done`.
- Value: **the merge gate landed for the first time** —
  `iterion/review=success` on the head SHA, posted through the server's
  publish endpoint. That link had never worked before (see the 401 below).

### The gate: a redirect was degrading the POST

`post_feedback` had been failing with `401 authentication required` on a route
that is deliberately auth-exempt. Everything else had been eliminated with
evidence — the route answers a bogus token differently, the URL and token were
correct in the run inputs, the same request reached the handler by hand from
both a laptop and a runner pod, Revi's own gate still worked. The remaining
hypothesis was that `urllib` follows redirects, and a redirected POST becomes a
GET, which misses a method-specific Go route and falls through to the auth
middleware.

Refusing the redirect fixed it. The value of the fix is not only that it works:
it **names the URL it called and the one the server redirected to**, so the
next occurrence needs no investigation.

Lesson: an unexplained `401` on an auth-exempt route is worth suspecting the
*shape* of the request before its credentials.

### `arm_automerge` armed nothing, and could never have

```
armed: false
reason: auto-merge request refused: [{'type': 'UNPROCESSABLE',
  'message': 'Pull request Pull request is in clean status'}]
```

`enablePullRequestAutoMerge` only accepts a PR that still has something to wait
for. The audit takes ~14 min and CI ~3 min, so **by the time the bot decides,
the PR is always already green** — the arm always fails. The feature was not
merely buggy on this PR; as shipped it would have merged nothing, ever, on any
repo whose CI is faster than the audit. Which is every repo.

v2.2.0 merges through `mergePullRequest` pinned with `expectedHeadOid` when the
forge itself reports the PR `CLEAN` and `MERGEABLE`. The invariant is
unchanged — the bot never decides that checks passed, it only acts on the
forge's own answer — but the guarantee is now "never merges past a check"
rather than "never merges".

Lesson: **a capability that only fires in a state your own latency prevents is
dead code with a green test.** The unit test passed because it stubbed the arm
call as succeeding; nothing modelled the state the real API is in when the bot
actually calls it.

### Other engine defects this run surfaced

- **Plugin-source checkout race** (fixed): `git init` creates `.git` before the
  fetch lands, and the fetcher treated `.git` as "tree complete". Five launches
  hitting a freshly rolled pod at once left one of them with an empty
  directory, and the loader reported it as *"has no plugin.yaml"* — a 502 that
  names the wrong cause, and a run row left `queued` forever with no error on
  it. The checkout is now staged and renamed into place.
- The status description read `no blocking findings (≥verdict)` — the shared
  phrasing assumes a severity floor, while this gate turns on the audit
  verdict. Now written per verdict.

### Lessons for next run

- A green CI image build is **not** a deployment: iterion's CI separates the
  build from the `finalize` job that re-tags `:edge`. Poll until the published
  digest *changes*, then grep the fix inside the pod, and only then launch. I
  lost a run to trusting a green workflow.
- The dogfood cost stayed at ~$0.60 for a digest bump with a real
  two-image trivy delta. The audit is not where the time goes — `verify_build`
  is (5 min of the 14).

## 2026-07-28 — v2.1.0: the classifier was auditing empty diffs, and the gate could never work (no run; defects found by inspection + adversarial review)

- Status: **partial** — code validated locally and by review; no live cloud run
  yet (the target repo is not in the forge App's installation scope, which
  needs an Organization Owner).
- Versions: bot 2.0.0 → 2.1.0 · iterion PR #306 (7 commits)
- Method: wiring Vetty onto `socialgouv/buildkit-operator`'s Renovate PRs
  end-to-end (audit → align → verdict → gate → auto-merge). Deterministic
  nodes exercised against real fixtures; Revi reviewed the branch.

### What the attempt actually found

The bot did not fail loudly on this repo — it would have reported **"safe" on
three of the four open Renovate PRs without reading anything**. `prepare`
only recognised package manifests, so a PR moving a Dockerfile digest, a
`devbox.json` pin or a `Taskfile.yml` tool matched nothing. Crucially it did
not stop: `is_empty` means "no files changed", not "no manifest matched", so
the run continued and handed the auditor an empty `bump_summary`. Verified on
the real `renovate/golang-1.26` branch: 3 Dockerfiles, a 1684-char diff where
the old classifier produced an empty string.

Lesson worth keeping: **a scope flag and a coverage flag are not the same
flag.** Conflating them is what turns "we found nothing to look at" into "we
looked and found nothing".

### Engine defects this surfaced

- `review_on_sync` was **unreachable** — absent from the webhook API request
  type (a PATCH carrying it returned 200 and changed nothing) and never set by
  provisioning, so it could only ever be false in production. Since a commit
  status lives on one SHA, that made every merge gate self-defeating: the
  status went absent from the head after any push. Observed live on
  SocialGouv/iterion#300 (20 checks green, PR blocked). Now derived from the
  declared `statuses` scope.
- A PR event could only launch **one** bot, via a hardcoded fallback, and the
  shared author allowlist was nil'd as soon as one co-enabled bot was open —
  so a dependency guard co-enabled with a reviewer was silently dropped along
  with its author filter.
- Author routing read the event **sender**, not the PR author, so a human
  pushing a fix onto a dependency PR handed it to the wrong bot.

### The adversarial review earned its keep

Revi found four defects on the branch, two of which would have shipped broken:

- `arm_automerge` sent **syntactically invalid GraphQL** (a sigil swap that
  also rewrote GraphQL's own separators). The feature was dead, and the test
  certified it green — the stub answered success to any body. The fix now
  includes a stub that rejects what the API would, verified by reintroducing
  the bug and watching the test fail.
- The `ReviewOnSync` derivation ran only on a *fresh* provision, while the
  already-provisioned repo — the production case — hits the idempotent
  short-circuit. The fix fixed nothing that was already deployed.

Lesson: **a test whose stub accepts anything certifies nothing.** Both of
those were green in CI and broken in production; the pattern is a test that
asserts on what we sent rather than on what a real peer would accept.

### Lessons for next run

- Point it at an npm/pypi PR: live validation is still Go/Docker-only.
- The gate context must be pinned per repo (`launch_vars.gate_context`), the
  same value on every bot that can gate — a per-bot required check deadlocks
  whichever PRs that bot does not review.
- `arm_automerge` is only safe on a repo that requires at least one check;
  with none, a forge merges an armed PR immediately.

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
