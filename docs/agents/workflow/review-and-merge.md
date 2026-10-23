# Before merge — the required loop, and how a change reaches `main`

Every change lands through a pull request whose `revi/review` gate is green.
The documented exceptions are the admin bypass below — which allows a direct
push to `main`, not just a queue skip — the **operator direct-landing lane**
(§ below, decided 2026-10-22), and the release bot, whose
`chore: release` commits land on `main` with no PR and no gate.
The loop is:

**local adversarial round → fix → re-attack the fix → push → `/revi` → green**

1. **Run a local adversarial round on the diff BEFORE pushing.** A subagent
   whose posture is to break the change, not to bless it; every finding **and
   every fix it proposes** verified before a line is written. Protocol:
   [adversarial-review-loop.md](adversarial-review-loop.md). Measured on this
   repo: five consecutive gate verdicts at ≥ 1 medium on a fresh line (~6 h of
   queue) against one 15-minute local round followed by a first verdict at
   0 findings. That page also carries the **round budget** — a ceiling on cost
   by change size (≤ 8 files → 5 local rounds, 10 if the diff blocks · 9–25 →
   20 · > 25 → 50), never a
   target: what decides another round is whether the last one still returned
   verified high or critical findings.
2. **The gate closes the loop; the local round never does.** A sterile local
   round means "time to push", not "done". Revi's `questions` channel is
   non-blocking, but each question gets a doc fix or a written refusal — never
   silence. Symmetrically: at the **third** verdict with findings on the same
   PR, stop pushing — what is left is local-round work.
3. **The developer fixes the findings** — by hand, or through another local
   round. **Do not comment `/billy`**: see [the pause](billy.md#billy-is-paused).
4. **Every commit that ships reviewed work says what the review cost.**
   `Adversarial-Rounds:` and `Adversarial-Model:` trailers, `0 (trivial: …)`
   written explicitly when nobody attacked the change — format, what survives
   the squash, and rationale in
   [adversarial-review-loop.md](adversarial-review-loop.md#say-what-the-review-cost-in-the-commit).

## The operator direct-landing lane (decided 2026-10-22)

For operator-led chantiers where the queue's wall clock is the bottleneck, the
operator (admin) may land a change **without a PR**, by push to `main`. This
amends the sentence above for that lane only — every existing PR keeps
`revi/review` as its blocking gate, and the queue keeps serialising the PRs
that exist. The lane's blocking proof replaces the PR's:

1. **The gate, locally reproduced.** The full required-check set a workflow
   job can express — everything but `revi/review`, which item 2 replaces —
   runs locally. Its definition is `task ci:gate`, and the lane is CLOSED
   until that stream lands: no direct landing before the checked-in
   exhaustive mapping from `tests.yml` jobs to local members exists in this
   tree (the mapping lives with the chantier's evidence until then). Exit
   codes are read per member, never piped.
2. **A converged local adversarial loop.** Per
   [the loop](adversarial-review-loop.md): rounds until no unassumed
   high/critical finding stands, the fixes re-attacked, the budget respected.
3. **The train.** The branch rebases onto `origin/main` into an integration
   branch and the gate runs ON THE INTEGRATED TREE — this is the queue's one
   unique guarantee (it tests the merge ref) reproduced locally. A red train is
   fixed on the train, never pushed.
4. **The trailers, greppable.** `Adversarial-Rounds:` / `Adversarial-Model:` as
   everywhere, plus `Landing-Train:` naming the train and its evidence
   directory — a bypass that cannot be grepped is a bypass that did not happen.
5. **The witnesses, post-hoc.** The GitHub CI still fires on every push to
   `main`; a cancelled or missing witness run is NOT success in the landing
   record; a red `main` outranks every other work until green.

Courtesies that are duties: warn the sessions with PRs in flight before the
first push of a wave (a direct push invalidates their merge groups), and warn
sessions with unpushed branches (each `main` advance costs them a rebase).

## The merge queue

**`main` is protected by a merge queue** (ruleset "main protected — merge
queue"). PRs merge THROUGH the queue (`gh pr merge <n> --auto --squash`), which
rebuilds each on `main` + earlier-queued PRs and merges only if that combined
tree is green — closing the semantic inter-PR conflict class (two PRs green
apart, red combined). Repo **admins bypass** the queue for hotfixes (direct
push / `--squash` without `--auto`). Required checks: `test`, `race`,
`vendor-check`, `mongo-conformance`, `golangci`, `brand`, `revi/review`
(`test` and `race` are aggregator contexts — the work runs in the gated legs
behind them; see docs/merge-policy.md).
`nats-conformance` remains advisory until an admin adds it to ruleset
18857412; `fmt-check` reports on the PR and in the queue but is not required
yet (staged — `internal/ciguard`'s `requiredChecks`). A pull request titled
`chore(brew): update tap to …` skips the heavy ones (in the queue: an entry
whose squash message starts so — the commit subject of a single-commit pull
request, its title otherwise) and `brand` verifies it moves nothing but the
Homebrew tap's `version` and `sha256` values — any other diff under that
title is refused
([the one verified skip](../../merge-policy.md#the-one-verified-skip-the-homebrew-tap-update)).
Full details + revert command:
[../merge-policy.md](../../merge-policy.md). How long merges take, what they cost
and what fails the queue: `task ci:queue-stats`
([measuring the queue](../../merge-policy.md#measuring-the-queue)).

## The Revi merge gate

**Revi merge gate.** Revi (`bots/review-pr`) posts a
deterministic `revi/review` commit status on a PR head — `success` when 0
findings meet `gate_severity` (default `high`), else `failure`. Add that
context to another repository's required checks to make its verdict block the
merge; it is already required here by ruleset 18857412. The verdict is a COUNT
computed in the bot, never an LLM judgment; the review comments stay
non-blocking advice. Pairs with the webhook
`review_on_sync` opt-in (re-review each push so the status tracks the fixed
head) and Revi's falsifiability `questions` channel (non-blocking assumptions,
never gate). The forge-agnostic write path is `forge.CommitStatusClient`
([../../pkg/forge/status.go](../../../pkg/forge/status.go)); the endpoint posts it after the
review ([../../pkg/server/forge_publish.go](../../../pkg/server/forge_publish.go)). See
[../merge-gate.md](../../merge-gate.md) — which also covers the two bots
sharing one context on the same PR, and the per-repo **opt-in** zero-touch lane
(`auto_fix_on_gate_failure`) where a red gate launches the repo's fixer once per
head sha, off by default so the developer keeps the choice.

**GitHub workflow delivery preflight** — Billy checks the runtime token’s actual `workflows:write` proof before analyzing a workflow-changing diff; Vetty checks it before pushing an ALIGNMENT that touches that path, and holds `hold_delivery_permission` rather than attempting the push. The hold needs an ANSWERED refusal: `FORGE_PERMISSION_DENIED` is a credential configuration plus a new launch (approve the App's delivery permissions, or reissue a classic token with the `workflow` scope), while a preflight that did not answer ends the run `FORGE_PREFLIGHT_UNAVAILABLE` publishing no comment and no verdict — the cure is the one the run's reason names (a relaunch where the service went silent, the configuration gap where it names one), and what the PR carries is the server's reconciler closing the launch's own gate claim. Installation grants alone are not proof. See [../../revi-billy-loop.md](../../revi-billy-loop.md).

`review_on_sync` is what makes `/revi` a *loop* rather than a one-shot: each
push re-reviews the new head, so the status tracks the code you actually fixed.
It stays on.

## A parked gate is a rerouting signal, not a wait

When `revi/review` parks on provider quota (a `gate-paused` comment carrying
the retry instant and, when the provider said so, the window it hit), the pull
request is fine — the credential is not. Waiting for the window to reset is the last resort, never the plan:

1. **Identify the blocked credential.** `iterion remote admin llm oauth` and
   `iterion remote admin llm api-keys` list what the deployment draws on. A
   stored **dated** reading stops being trusted at the earlier of its reset
   instant and `ITERION_USAGE_CAP_TRUST_WINDOW` (three hours by default after
   it was observed); a refusal with no reset instant (auth, rate, spend) is
   trusted for its escalating rest instead — an hour at first, up to six
   ([usage caps](../../usage-caps.md)). Either way a provider that reopened a
   window early self-heals within that bound. To cut the wait when the
   provider's dashboard disagrees with the ledger, forget the credential's
   readings: `iterion remote admin usage-readings clear <fingerprint>`; the
   next run re-measures the windows itself.
2. **Lend a live credential to the deployment** — no redeploy needed. A
   forfait: `iterion remote admin llm oauth set <claude_code|codex> --rank <n>
   --from-file <credentials.json|auth.json>`; a provider key:
   `iterion remote admin llm api-keys create --provider <p> --name <n>
   --from-env <VAR>`. Any credential write to a shared deployment waits for the
   operator's explicit go, with the exact command in the question.
3. **Keep the review pressure locally.** Run the
   [local adversarial loop](adversarial-review-loop.md) on a forfait you hold
   while the gate is parked; it does NOT replace the required status — the
   merge still waits for a real `revi/review` verdict.


## Billy, and the release path

[Billy is paused — the zero-touch fixer lane, and its re-arm](billy.md) ·
[Release and changelog](release.md).
