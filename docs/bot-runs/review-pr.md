# Revi — `review-pr` run bilans

Read-only code reviewer. Revi reviews with one selected family by default
(`review_mode: mono`) or independent Claude + GPT reviewers when dual mode is
explicitly selected; findings are normalised/de-duplicated, and one issue per
finding is published to the native board (label `source:revi`). With `--var
pr_url` it also posts an inline forge review and an optional deterministic
commit-status gate. Never edits or commits. See
[bots/review-pr/](../../bots/review-pr/).

## 2026-09-11 — the gpt family carried a whole review for the first time, and named what still pinned it to Claude (runs 01a092b2 + 01a092c3, PR #1150)

- Status: **validated** — `revi/review` posted `success` from a run in which
  Claude never executed.
- Versions: bot review-pr 0.9.0 · engine v3.140.0 (deployment) · reviewer
  `claw` + `openai/gpt-5.6-sol`.
- Method: `runs launch --bot review-pr --var mono_family=gpt --var
  review_mode=mono --var pr_url=… --var post_to_board=false`, with
  `--model-overrides '[{"selector":"judge",…},{"selector":"agent",…}]'` moving
  every LLM node onto claw. Occasion: the deployment's Anthropic seven-day
  window crossed the 95% hard cap at ~20:30Z, and every claude_code run on the
  repo — 4 reviews and 2 fixer runs — parked `failed_resumable`, so no PR could
  satisfy its required check.
- Result: converged, `finished` in ~5m30. `gpt_ran=true`, `claude_ran=false`.
  Verdict: 0 findings, and a summary naming the five changed areas by file plus
  the tests it ran — including which broader suite it could NOT finish inside
  its 30s limit. Not a façade: the honest gap is reported rather than absorbed.

### What blocked it, in the order the run met it

1. **`mono_family` switches the REVIEWER, never the graph.** `agent converge`
   carried a hardcoded `backend: "claude_code"`, so the gpt review completed and
   the merge step then died on the cap. A family switch that still requires the
   other family is a half promise — the shape CLAUDE.md's parity doctrine calls
   a defect. Fixed here: `${ITERION_VIBE_BACKEND_EMIT:-claude_code}`, the form
   its own siblings already use.
2. **The admission pre-flight refused the whole run.** `AnthropicWireReachable`
   exists precisely to spare a run pinned off that wire (#668, a fully pinned
   rite frozen five days) and it does read the launch's overrides — but it is
   STATIC: the untaken `reviewer_claude*` nodes and `converge` were enough to
   make the wire "reachable", whatever `mono_family` would route at run time.
   Covered by two KIND selectors (`judge`, `agent`) rather than by naming
   sites — the routers are `condition`/`fan_out_all`, so those two cover every
   LLM node by construction.
3. **A CLI launch gets no checkout.** `POST /api/runs` accepts `repo_url` /
   `repo_ref` / `connection_id` (`pkg/server/runs_launch.go`), and the webhook
   path sets them; `iterion remote runs launch` exposes none of the three. The
   reviewer opened on `fatal: not a git repository` and re-derived the diff
   through the forge API. It recovered and still reviewed well, but the local
   checkout the node is written for was never there.

### Lessons for next run

- To run Revi off the Anthropic wire, overriding the reviewer is not enough:
  override by KIND so the pre-flight sees no anthropic route at all. The
  per-run form needs no bot edit and is the mechanism the pre-flight was
  built to read.
- `resume` cannot carry `--model-overrides`; only a fresh launch can. A run
  parked by the cap therefore cannot be re-aimed at another family in place —
  worth knowing before spending a reviewer pass twice.
- A run parked on a usage window will be re-driven when the window reopens.
  Cancel a superseded one, or a second verdict lands on the same gate days
  later.
- Verify the binary before trusting a local `validate`: a v3.69 CLI reported
  `E002` on a bot the v3.140 build compiles cleanly, on the UNMODIFIED file —
  the control that separated "my edit broke it" from "my tool is stale".

## 2026-09-09 — forge-native ticket context: the first `covered` verdict, and the [high] the feature found in itself (run 01a085b8, PR #1017)

- Status: **validated locally**; the cloud half arrives on its own once 0.9.0 is baked.
- Versions: bot review-pr **0.9.0** (local) · engine `f633a830` (v3.122.3)
- Why the change at all — a measurement, not an intuition: `tracker_api_base`
  was set on **0 of the 16** connected GitHub repos. Ticket conformance had
  shipped in 0.6.0 and, outside demat-amiante, **had never run once**. A PR
  saying "Fixes #123" was reviewed without anyone reading #123, and nothing
  said so. Everything the check needed was already in the run (the PR URL, a
  `forge_token` bound at provisioning), so the per-repo var was an artificial
  limit for the common case.
- Method: `iterion run` on this very branch with `--var pr_url=<PR #1017>` and
  `scope_notes` carrying the PR title+body (what the webhook supplies), NO
  `tracker_api_base` — i.e. the forge-native path. 18 min, **$3.31** for the
  run (converge $0.29).
- Result: **`1014: covered`** — the first `covered` verdict ever observed here
  (every earlier test produced `not covered` or `unverifiable`), so the check
  is now known to bite in **both** directions rather than only rejecting. It
  named the four deliverables of the issue and anchored each in the diff, and
  it reported having read the issue *through the very path the PR builds*.
- What the events prove rather than assert: `api.github.com/repos/SocialGouv/
  iterion/issues/1014` was fetched with no `tracker_api_base` anywhere — the
  base was derived from the PR URL.
- **Three real defects found by Revi in this feature, on its own PR** (bot
  0.8.1 reviewing 0.9.0 — the reviewer that does not yet have the feature
  reviewing the one that adds it):
  - `[high] requirements` — the manifest declared no `issues` scope. A GitHub
    App provisioned from it cannot read issues, so every fetch 403s and every
    verdict degrades to `unverifiable`. **The feature could have shipped
    INERT**, and its failure mode reads like "no ticket" rather than "the
    token was not allowed to look" — the exact shape of a guard that looks
    like it works.
  - `[medium]` — the linked-issue GraphQL POSTed to `api.github.com`
    unconditionally, while the API-base table right above it derives
    `https://<host>/api/v3` for GitHub Enterprise: a self-hosted instance's
    token handed to a third party.
  - `[medium]` — the forge token can WRITE (that is why it exists), and the
    change put its path in front of a judge whose two inputs (diff, ticket
    body) are attacker-controlled on a public repo.
  All three fixed in the same session; the re-review then went green.
- Lessons for next run: (a) a capability that needs a token scope must declare
  it in the manifest — the runtime does not read `token_scopes`, the
  PROVISIONER does, so an undeclared scope is a feature that installs itself
  disabled; (b) when a skill derives a host, every other URL in that skill has
  to derive it too, or the one hardcoded line becomes the exfiltration path;
  (c) reviewing a feature with the version that predates it is a cheap and
  honest adversary — it has no stake in the design.

## 2026-09-08 — ticket conformance validated on a REAL private Jira, and the cross-tenant write it exposed (runs 01a082a0 / 01a082a8 / 01a082b2)

- Status: **validated** — the feature works end-to-end on the cloud instance
  against a real, private Jira Cloud ticket. One engine defect found on the way.
- Versions: bot review-pr 0.6.0 (baked, prod) · instance
  iterion.fabrique.social.gouv.fr
- Method, in three runs:
  1. `01a082a0` — **probe** on `SocialGouv/iterion-test-appy-e2e` (GitHub) with
     `tracker_api_base=https://api.github.com` pinned in the integration's
     `launch_vars`, no credential bound. Proved the half the local dogfood
     could not: the ref is extracted from the PR body with no `ticket_refs`,
     the tracker is reachable **from the k8s sandbox**, and the
     `### Ticket conformance` section lands on a real PR. It also reported,
     unprompted, that `/run/iterion/secrets/tracker_token` was absent and that
     the authenticated attempt 401'd — i.e. it named the limit of my own probe.
  2. `01a082a8` — **first real Jira run** on the GitLab MR
     `…/dematamiante/code/demat-amiante!2` (ticket `DAM-1978`, private Jira
     Cloud `jira-mcas.atlassian.net`). Verdict: `unverifiable — tracker token
     file does not exist; anonymous GET returned HTTP 404`. **The canary was
     red**, and it was right: the secret was not reaching the run (see below).
  3. `01a082b2` — same MR, same ticket, after fixing the wiring. Verdict:
     `DAM-1978: not covered — ticket demands cutting the multi-hour
     Elasticsearch reindexation runtime to enable a daily run; the branch adds
     only a readme.md changelog template and changes no Java, batch, or
     configuration code, while the PR body claims "Closes DAM-1978"` — content
     it could only know by reading the private ticket. Plus a `[high]
     requirements` finding and four sharp questions, one of them load-bearing
     (*does `Closes DAM-1978` actually drive a Jira transition here? the
     blocking severity hinges on it*).
- Value: red-then-green on the SAME MR and the SAME ticket. The verdict flips
  only because the credential arrives, so this is a test that bites in both
  directions rather than a screenshot of a success.
- Secret hygiene, measured (not asserted): over the run's 112 KB of events,
  where the Jira host appears 5× and `DAM-1978` 112×, the token appears **0×**
  — verbatim, as an 8-char fragment, as the base64 of the `Basic` header, and
  as its `ATATT` prefix. The positive controls are what make that zero mean
  something.
- **Engine defect found (the reason run 2 was red): a team-scoped write goes
  to the tenant of the CALLER'S TOKEN, not the team in the path.**
  `POST /api/teams/{id}/secrets` and `POST /api/teams/{id}/bots/{bot}/bindings`
  authorize on the path team (`canManageTeam`, which a super-admin or org admin
  passes for any team) but never re-scope the request context —
  `generic_secrets_routes.go` and `bot_bindings_routes.go` contain **zero**
  `store.WithTenant`, where `forge_provisioning_routes.go` has four. The row is
  written into the JWT's tenant partition carrying the path team's scope, so it
  is invisible from BOTH teams' list endpoints and can never be resolved for a
  run — and every call returns 200/201. `iterion remote secrets set --team X`
  inherits the same fate. Reproduced twice here (secret, then binding); the
  workaround is `iterion remote teams switch <target>` **before** creating
  either. Issue #997.
- Lessons for next run: (a) wire secret + binding from the target team, never
  by path/`--team` from another one, until #997 lands; (b) `/revi` on a note is
  the fidelity path — it applies the integration's `launch_vars`, a manual
  launch does not; (c) `iterion remote runs list` is scoped to the active team,
  so a run launched on another team is simply absent — switch before concluding
  it never started.

## 2026-09-08 — a green gate is one pass, not a property: 1 finding → 0 across a docs-only commit, and four defects still there (#851)

- Status: **observation**, not a bilan of a launched run — recorded because it
  bears directly on how much a `revi/review: success` is worth.
- The measurement. PR #851 was reviewed twice, 21 minutes apart:

  | head | reviewed | verdict |
  |---|---|---|
  | `6f08d55f1` | 05:13:15Z | **1 finding (medium)** |
  | `fd61efe93` | 05:34:51Z | **0 findings** → `revi/review: success` |

  The only commit between them adds ten lines of prose to
  `docs/bot-runs/review-pr.md` — **no Go changed**. The severity floor is
  `medium`, so the earlier finding would have been kept had it been raised
  again. The PR merged on that green gate at 05:57Z.
- What makes it falsifiable rather than a hunch: a `branch-improve-loop`
  campaign was reading the same branch at the same time, in an independent
  context. It re-verified the dropped finding **still present at the current
  head** ("the branch moved twice since that review but this anchor is
  untouched") and found three more defects plus two comments the branch's own
  commits had inverted. Four real defects survived the pass that reported none.
- Not a same-sha pair, so it still does not measure run-to-run variance — the
  heads differ by one commit. But it is the closest approach in the data, and
  it constrains the shape: what varies between passes is **the finding set**,
  on input that is identical where it matters.
- Consequence for the gate. `revi/review: success` is a statement about one
  pass ("this reading found nothing ≥ `gate_severity`"), never about the
  branch. That is the honest reading of a deterministic count over a
  non-deterministic input, and it is why the gate blocks merges rather than
  certifying code. Do not read a green gate as "reviewed clean"; on a change
  that matters, a second reader is a second sample, and the campaign bots are
  the cheaper one to add.
- Open question this sharpens rather than answers (#685): whether the 0.7.0
  frugality clause ("triage, don't sweep") is what drops findings between
  passes. The experiment is unchanged — re-review with the clause disabled and
  diff the finding sets — but the target is now the *finding set*, not the cost.

## 2026-09-05 — two guard-tier reviews die at the cost cap, no verdict (#780, #785)

- Runs: `01a072d6-24ab` (#780, head `4aee1d641`) — `budget exceeded: cost_usd (36/12)`,
  superseded by the automatic relaunch `01a072eb-067d`, itself dead at `(30/12)`;
  #785 (head `ecca4b03c`) dead at `(14/12)`. Push → final failure status: 84 min
  for the two runs of #780, 53 min for #785 (the runs are not in the campaign
  store; the timestamps are the head commit and the `revi/review` status).
- Outcome: the merge gate posted a synthetic `failure` twice with no review — a
  7-file PR and a 12-file one (7 of them vendor), both open since.
- Reading: the cap (12, sized 2026-09-03 on median $3.6 / p95 $10.5) is 3× short
  of the guard tier that reproduces its findings and answers the fixer's rounds;
  the engine refuses new nodes at 90% of the cap with no grace, so the usable
  spend was $10.8 — and the $36 is a lower bound (the run died there).
- Change: `max_cost_usd` 12 → 48 (0.9 × 48 = $43 usable), `max_duration` 90m →
  120m (same wall on the duration axis), pacer 12 evals / 6m with `cost_gt=32`
  (manifest 0.8.1). Re-run on both PRs after the catalogue is deployed.

## 2026-09-05 — what a Revi review COSTS: 33 production reviews, and the model that fits them
- Status: validated (measurement, not a dogfood run — 33 organic
  webhook-launched reviews on this repo, 11:57Z → 16:42Z, no run launched
  for the purpose)
- Versions: bot **0.7.0 throughout** · iterion `d57851fb` then `2defa1a0`
  (v3.103.0 → v3.104.0). The dataset is homogeneous *because* of the
  override described in the entry below: 0.8.0's tiers never served during
  the window, so no bot-version change straddles these numbers.
- Method: default guard-shaped run — `reviewer_claude` (opus-5, effort
  high), `converge` on sonnet, `severity_threshold: medium`,
  `max_cost_usd: 12`. Cost read from each run's checkpoint (the run's
  `budget_cost_usd` field reads 0); diff sizes from `gh pr view --json
  additions`.

### The model

Not linear. **It saturates.**

| added lines | rule | evidence |
|---|---|---|
| < ~500 | `$2.24 + $0.00072 × added`, ±15% on a SHORT pass (1–2 findings, <8 min) | #754 −8.6%, #756 −13.8%, #757 +4.1% |
| ~500–1500 | same rule, +6% to +29% | #758 +7.6% / +5.9%, #760 +29% |
| > ~1500 | **predict $3.5–$6.0 and ignore size** | see below |

Above ~1500 lines the line count stops carrying information — it does not
even order the plateaus:

| PR | added | hand-written | observed |
|---|---|---|---|
| #764 | +1598 | — | $4.81, $4.81, $4.28, $5.36 |
| #761 | +2738 | — | $4.47, $5.82 |
| #745 | +10078 | 4804 | $3.96, $3.99, $4.01 |

**The largest PR is the cheapest.** The cause is the 0.7.0 frugality
contract working as designed: `review_system` tells the reviewer to read
`git diff --stat` first, prioritise the risky hunks and stop sweeping, so a
10k-line diff becomes a triage rather than a linear read. The findings
support that reading — all four on #745 were `medium` and all four sat in
the files carrying the new logic (`board_project.go` ×2,
`board_binding_store.go`, `projects_app.go`).

The floor is ~**$1.95–2.00** (#766 at 4.4 min), and `converge` is the most
stable quantity in the whole dataset: **$0.61–$0.71** across all 33 runs.

### Two claims of mine that did NOT survive more data

- *"Cost is a stable property of a given PR"* — I measured #764 twice at
  0.12% apart and said so publicly. Two further passes landed at $4.28 and
  $5.36: the real spread is **25%**, and the 0.12% was a two-point
  coincidence. Keep **±30%** as the per-PR predictor's band.
- *"Excellent reproducibility"* — withdrawn outright. **There is no
  same-sha pair anywhere in this dataset**; every repeat straddles at least
  one commit (#758 gained `3f27d71c5`, #745 four commits, #761
  `2c9c06385`). Nothing here measures run-to-run variance on identical
  input. Measuring it needs two reviews launched on the same head.
- *"Cost saturates at $3.5–$6.0"* — true of the reviews that **finished**,
  and only of those. Every one of the 33 was sampled from `runs list` as a
  `finished` row, so the population is conditioned on completion and the
  expensive tail is invisible by construction. The #780/#785 entry above
  measures that tail on the SAME day: `(36/12)`, `(30/12)`, `(14/12)` —
  three reviews that died at the cap, none of which could appear here.
  So the shape is **bimodal**, not saturating: most reviews land at $2–6,
  a few explode past the cap and take the run with them.
  **Do not size a budget from the table above** — it describes survivors.
  The cap that the tail actually required is 48 (that entry's change).

### Open question — do NOT close it with cost

Does frugality cost *coverage* on very large PRs? Cost alone cannot answer
it: absence of findings is not evidence of absence of defects. The
experiment that would answer it: re-review #745 with the frugality clause
disabled and diff the two finding sets.

### Lessons for next run
- Read cost from the checkpoint, not `budget_cost_usd` (which reads 0).
- Predict from a prior review of the SAME PR when one exists — it beats any
  function of the diff, but only to ±30%.
- Before attributing a cost shift to a bot change, verify which bundle
  actually served (`GET /api/admin/bots`, and look for the version-specific
  nodes in the run's checkpoint outputs). That check is what turned this
  window's apparent version straddle into a homogeneous dataset.

## 2026-09-05 — review tiers shipped (0.8.0, SocialGouv/iterion#685) — design note, live measurement pending
- Status: implemented + covered by DSL-level tests (stub executor + expr-level
  unit tests); **not yet dogfooded live** — no LLM credentials in this
  session, so the cost/quality claims below are the design's PREDICTION,
  not a measured result. Flag this entry's status back to `validated` once
  a real glance-tier run on a small PR is measured against the guard-tier
  baseline documented in the 2026-09-03/04 entry below.
- Versions: bot 0.7.0 → 0.8.0 · iterion `c6f8bac0f` (v3.102.1) at write time
- Landed: SocialGouv/iterion#742 (merged 2026-09-05 11:54Z, in v3.102.6);
  prod runners carry the ENGINE since 12:25Z (`edd5b9dcf`).
- **Correction (2026-09-06 16:50Z): the tiers did not serve a single
  production review for the first 29 hours.** Carrying the engine is not
  carrying the bot. A platform bot override for `review-pr`, pushed
  2026-09-04 06:52Z for the 0.7.0 cost pass, still held the 0.7.0 bundle —
  and a platform override outranks the baked catalog at every launch
  surface, by design. Measured: prod review graphs on both 09-05 and 09-06
  ran `diff_precheck → topology → reviewer_claude → merge_reviews →
  converge → pr_gate` with **no `tier_expand` node**, and
  `GET /api/admin/bots/review-pr` returned manifest version `0.7.0` whose
  `main.bot` contained `TOKEN FRUGALITY` and `supervisor pacer` but none of
  `tier_expand` / `review_tier` / `reviewer_claude_glance`. The override was
  deleted at 17:00Z after verifying the repo's 0.8.0 is a strict superset
  (it carries all three 0.7.0 markers plus #758 and #816); the effective
  bundle then read `version 0.8.0` from `/opt/iterion/bots/review-pr`. The
  first real tier measurement is still owed — record it here.
- What shipped: `review_tier` (glance/guard/audit), a deterministic
  `tier_expand` compute node resolving severity_threshold/max_findings/
  post_to_board/effective_review_mode from sentinel-defaulted vars (any
  explicit `--var` still wins), two new judge nodes
  (`reviewer_claude_glance` / `reviewer_gpt_glance`) the `topology` router
  picks for the glance tier, and audit's dual fan-out forced independently
  of `review_mode`. Full design + preset table:
  [docs/merge-gate.md#review-tiers](../merge-gate.md#review-tiers).
- What to measure after a real glance run: (1) actual $ on a small PR
  (<500 lines) against the guard-tier baseline (median $1.8–3.6 per the
  0.7.0 bilan) — the floor argument in the issue predicts the gap will be
  SMALLER than a naive "cheaper model ⇒ proportionally cheaper" reading,
  since claude_code's own context-file injection floor is untouched by
  this pass; (2) whether the glance prompt's "skip exploratory reads"
  instruction measurably shortens the reviewer's tool-call count without
  degrading finding quality on a PR with a real, catchable bug (reuse the
  live fixture in `e2e/live_bot_review_pr_test.go`); (3) that
  `reviewer_claude_glance`'s sonnet-level findings don't regress silently
  below what a maintainer would want gated (compare against a guard-tier
  review of the SAME diff).
- Engine finding (worth keeping in view, out of scope for this ticket): a
  stub e2e test of review-pr's real dual path (`fan` → `reviewer_claude` /
  `reviewer_gpt` → `merge_reviews` with `await: best_effort`) showed
  `merge_reviews`/`converge`/`pr_gate` firing TWICE in one run when the two
  reviewer branches complete asynchronously — `best_effort` appears to
  trigger once per ARRIVING branch rather than once overall. Pre-existing
  (unrelated to tiers — the same fan/merge/converge graph, unchanged by
  #685; audit's forced dual just reaches it a second way), and easy to miss
  with real LLM latencies serialising the two branches' completion further
  apart. Worth a dedicated look before leaning harder on dual-mode reviews.
  **Root-caused and fixed as #741**: not `best_effort` at all — the collector
  election counted `topology -> reviewer_claude` (the mono edge) as a second
  predecessor of the fan-out target, elected `reviewer_claude` itself as the
  collector, so its branch executed nothing while the gpt branch ran the whole
  `merge_reviews -> converge -> pr_gate` tail inside its branch, and the trunk
  then ran `reviewer_claude` + the same tail again. Only predecessors inside
  the fan-out count now (`ir.ExecBranchConvergencePoint`); evolve's
  `review_fanout` had the identical shape. Guarded by
  `e2e/review_pr_convergence_test.go`.

## 2026-09-03/04 — cost-reduction pass (0.7.0) shipped through 4 rounds of its own review (PR #651)
- Status: validated (the review loop itself; the cost delta is measured over the following days)
- Versions: bot 0.6.0 reviewing → 0.7.0 shipped · iterion `2bd3bbca3` (v3.100.0)
- Method: the PR that ships the cost levers was reviewed BY the pre-change bot
  (mono/claude, opus/high) — 4 rounds, webhook-launched on each push, merge
  queue at the end. Baseline measured first on prod (9 finished runs,
  2026-09-03 morning): reviewer_claude = 85–93% of $1.42–$10.54/run
  (median ~$3.6), ~28 runs/3h, ~18% cancelled "superseded" mid-flight.
- Result: merged through the queue after rounds of 7 → 2 → 3(medium-only,
  gate green) → 0-blocking findings. Round-1 review cost ~$3.6 (46k tok) on a
  31-file diff — consistent with the baseline. One queue ejection (silent,
  during an unrelated merge) and one Anthropic session-limit parking
  (USAGE_LIMIT_BLOCKED, reset on the 5h window) crossed the loop; the
  usage-window retry machinery resumed the parked review on its own.
- Value: every round produced REAL findings. Round 1: the debounce subject
  key missed the project path (cross-repo collision on an org webhook,
  [high]) + 6 mediums/lows all legitimate. Round 2 caught the pacer being
  SILENTLY INERT (unexpanded `${VAR:-…}` model pin split into a garbage
  provider — the exact "declared capability dead with green tests" class) and
  GitLab having no closed-MR lane at all. Round 3's mediums (redelivery
  self-supersede, denial-drop, arrival-order parking) were all real too.
- Findings / misses: nothing false-positive across 12 findings; the
  open-questions channel (6 + 1) was sharp (cost_gt mono/dual semantics,
  audit of debounced pushes, lease/batch sizing). The reviewer twice found
  defects OUTSIDE its diff impossible (supersede ProjectPath class, fixed
  proactively by grep; arrival-order, deferred then fixed by the night
  fixer).
- Engine hardening: the whole PR — source-level severity floor
  (`severity_threshold` default medium), TOKEN FRUGALITY contract, pacer
  supervisor (haiku, cost_gt=8), mid-node `usage_progress` events feeding a
  now-live cost_gt, `ITERION_VIBE_MODEL_EMIT` (converge → sonnet), budget
  20→12 / findings 40→15, and the 3-min synchronize debounce
  (`ITERION_WEBHOOK_SYNC_DEBOUNCE`). Plus C190 widened to judge nodes and
  supervisor model env expansion.
- Zero-touch loop note: the red gate auto-launched Billy twice. The first
  died on the provider session limit ($5.75 spent, work stranded in the pod
  workspace — cancelled, hand-fixed instead). The second (after the queue
  ejection) rebased the branch and fixed all 3 round-3 findings overnight,
  including one (R4f7eab) already carded as follow-up — card closed.
- Lessons for next run: (1) a supervisor/model pin with `${…}` must be
  proven by a LIVE eval, not a unit test that hands it pre-expanded;
  (2) when the merge queue holds the branch, park local fixes — the fixer
  may land them first; (3) the deploy is three layers (platform bot push =
  instant; server follows :edge on rollout; runner needs the infra-apps
  digest bump) and a lever is only live when ITS layer rolled.

## 2026-09-02 — ticket conformance (v0.6.0) first dogfood: fetch + verdict + 3 real findings on its own feature branch (run 01a06405)
- Status: validated (local half — the PR-summary section and the forge publish path stay for the prod e2e)
- Versions: bot 0.6.0 · iterion 754321763 (worktree revi-ticket-context)
- Method: mono/claude (claude_code, opus, effort high), `--var base_ref=main`
  on the feature branch itself, `--var tracker_api_base=https://api.github.com`,
  `--var ticket_refs=627` (explicit ref — extraction not exercised), no
  `tracker_token` bound (the no-credential path), `post_to_board=false`, no
  `pr_url`, `ITERION_SANDBOX_DEFAULT=none` (a *linked git worktree* workspace:
  its `.git` is a file pointing into the main repo's `.git/worktrees/`, which a
  sandbox bind-mount would break — a real limitation to know when dogfooding
  from a Claude worktree).
- Result: converged; first pass died `BUDGET_EXCEEDED` at pr_gate on a too-tight
  `--max-cost-usd 5` (opus review of a 4-commit multi-surface diff ≈ $6) —
  `iterion resume --max-cost-usd 8` finished it in seconds (only deterministic
  nodes remained). Total ≈ $6.
- Value: the whole new chain proved live — the reviewer read the skill, curled
  `api.github.com/repos/SocialGouv/iterion/issues/627` unauthenticated (token
  file absent → the skill's fetch-without-auth fallback), returned the exact
  verdict `#627: not covered — <the ticket's real demand, correctly summarised,
  vs what the branch actually delivers>`, filed it as a `requirements` finding,
  and `ticket_conformance` threaded converge → pr_gate → done intact.
- Findings / misses: on top of the expected requirements verdict, Revi returned
  **3 real findings on the feature branch it was reviewing** (its own new
  capability's diff): [high] the delegated org-admin caps PATCH could raise a
  team ABOVE the platform default (`orValue` semantics — any non-zero team value
  wins), [medium] the update-path approval gate keyed only on the bot set, so
  `auto_fix_on_gate_failure:true` with an unchanged bot set bypassed the org
  approval, [low] the approver UI showed repo+bots but not the automation
  switches it was approving. All three fixed in the same session (platform
  ceiling 422, `expandsProvisionSurface` predicate, `approvalExtras` rendering)
  with regression tests.
- Engine hardening: none needed — the budget-exceeded → raise-cap → resume
  recovery worked exactly as documented.
- Lessons for next run: budget an opus mono review of a real feature branch at
  ~$6–8, not $5; prod e2e must exercise the two halves this run could not — the
  PR review summary's "Ticket conformance" section and a real authenticated
  Jira fetch through a bound `tracker_token` (egress `AllowedHosts` observed).

## 2026-09-02 — 🔁 re-request lane live pilot on questions-ecrites (runs 01a0620c / 01a0620f / 01a06211)

- Status: **validated** — the three behaviours of the GitHub re-request lane
  (#605, `review_request_logins`) proven end-to-end on the production
  instance, on a real repo, through the real GitHub events.
- Versions: iterion cloud prod v3.88.0 (`9ff26bc47`, deployed minutes
  before) · bot `review-pr` via the provisioned webhook lane
  (`overlap: supersede`, `review_on_sync: true`, PAT connection posting as
  the `iterion-bot` User account).
- Method: throwaway PR SocialGouv/questions-ecrites#64 (one added doc file);
  reviewer re-requests driven by the API equivalent of the sidebar 🔁 button
  (`POST …/requested_reviewers`). No board writes (`post_to_board=false`).
- Result, all three lanes:
  1. **open → auto-review** (`01a0620c`): launched 3 s after the PR opened,
     review posted in ~3 min **as `iterion-bot`** — which clears the pending
     request, i.e. re-arms the button;
  2. **🔁 after a posted review** (`01a0620f`): click → new run in 5 s (the
     salted-key lane — the button is repeatable per head);
  3. **🔁 during a live review** (`01a06211`): the in-flight run was
     cancelled with `superseded by a newer delivery for the same subject`
     and replaced 3 s later (the round-5 supersede-defer fix, observed
     verbatim).
- Value: this is the UX the whole migration existed to reproduce (the Revu
  App's re-request button, on a bot **User** identity — a GitHub App cannot
  be a requested reviewer). Generalised the same day to vao / domifa /
  code-du-travail-numerique / qe-front / egapro.
- Findings / misses: none on the lane itself. The **cost of the road** was
  upstream, on #605's own gate: round 7 (`01a06146`) computed a clean
  0-finding verdict in 41.4 min then died at `converge` on the 45 m
  duration budget — the verdict was in the checkpoint but never published,
  and the gate's supersede net relaunched a full ~$11 re-review of an
  unchanged diff. The operator published the checkpoint verdict manually
  (PR comment + `revi/review` status) rather than pay another round.
- Engine hardening (cards): `native:85d7752d` (a review bot whose PUBLISH
  node can be refused by the duration budget wastes the entire run —
  exit-grace carve-out or a reviewer budget share); `native:3b3562ad`
  (claude.ai "monthly spend limit" text misclassified as
  `EXECUTION_FAILED: structured output invalid` → 9 futile retries → DLQ
  instead of `USAGE_LIMIT_BLOCKED` + usage-window retry; and the claw
  structured-output recovery is dead on the review schema's union-typed
  `findings`).
- Lessons for next run: budget elapsed rides the checkpoint — a run whose
  clock was eaten by infra failures cannot be rescued by resume (cloud
  `ResumeSpec` has no budget override); relaunch fresh instead. And a
  22-second "structured output invalid" failure is a dead credential
  wearing a schema-error costume: read the run log before blaming the
  schema.

## 2026-09-01 — four passes as closure judge of an adversarial loop (runs 01a05daf / 01a05e57 / 01a05e6f)

- Status: **validated** — Revi as the external reviewer closing a `::loop`
  on the re-request-review + gate-opt-out engine work (PR #604, follow-up
  PR #608).
- Versions: iterion cloud prod v3.83.1→v3.84.0 · bot `review-pr` (webhook
  lane on github.com/SocialGouv/iterion, mono topology, `revi/review`
  required by the merge queue).
- Method: the PRs went through 3 parallel adversarial subagents + 1
  verification agent BEFORE each Revi pass; Revi reviewed what that
  internal loop had already declared clean. 4 passes total (open +
  re-review-on-push on #604; open + re-review on #608).
- Result: **every pass found real defects the internal loop had missed; 0
  false positives across all four.** Retained findings, each
  mutation-proven red before fixing: R7e050f (the new GitLab
  re-request lane bypassed the replier authorization gate `/revi` has),
  R6a15fe (the SAME gap on the twin prforge/GitHub lane — the internal
  loop had fixed GitLab only), R34eb8c (an authz ERROR 502'd the delivery
  and stranded a co-riding gate resync), R0c3aab (the authz gate ran
  before the scope filters, spending forge calls on out-of-scope events),
  R68edf4 (comments describing the pre-gate behaviour on both lanes).
  Passes 1–2 folded into #604 before merge (`c5eb31847`); passes 3–4
  shipped as #608.
- Value: the external-family pass is what caught the **twin-site class
  miss** three separate times (a guard added to one lane but not its
  structural sibling). Four internal agents staring at the same diff
  shared the same blind spot; Revi did not.
- Findings / misses: none of Revi's findings were noise. Its one
  over-reach (fear of "review-per-push returning through
  AddedReviewers") was refuted by a live GitLab payload probe rather
  than argued — a push emits no reviewers diff.
- Engine hardening: whole feature is engine work; see #604/#608. One
  operational gotcha surfaced by Revi's own machinery: **its pending
  claim ejects a PR from the merge queue** — the `mirror-revi-verdict`
  job read `revi/review=pending` on #608's head (a re-review was in
  flight after a usage-cap unblock) and rejected the queue entry twice.
  Don't enqueue while a re-review is pending on the head; re-enqueue
  once the verdict lands.
- Lessons for next run: keep Revi as the closure judge of adversarial
  loops — it is measurably a different failure-mode detector than N
  same-family internal agents. Two passes stalled on
  `USAGE_LIMIT_BLOCKED` at 99% of the weekly window; the runtime-mutable
  cap (`iterion remote admin caps set`) unblocked them without a deploy.

## 2026-08-13 — first review on a self-hosted GitLab, and the two engine gaps it surfaced (runs 019ffad9 / 019ffadb / 019ffb04)

- Status: **validated on the direct-launch path** (webhook lanes still blocked
  by the target instance's outbound allowlist — see below).
- Versions: iterion cloud prod v3.40.3 → v3.40.6 · bot `review-pr` 0.5.7
- Method: `POST /api/runs` with `repo_url` + `connection_id` (repo-targeted
  launch, no webhook), mono topology, `gate_context: iterion/review`,
  `post_to_board=false`. Target: a seeded merge request on a private GitLab
  19.2.1-ee instance (subgroup path five levels deep), forge credential a
  **group access token** so the bot posts under its own identity.
- Result: converged in ~8 min. 7 findings on a 14-line handler (1 critical
  SSRF-with-echo, 3 high, 1 medium, 2 low), 3 carrying one-click suggestions,
  posted as inline discussions + one summary note. The merge gate landed as
  `iterion/review=failure` on the MR head and renders as an external pipeline —
  advisory, since a GitLab commit status blocks nothing unless the project
  requires pipelines to succeed.
- Value: the reviewer **executed** the code it was judging. It copied the
  handler into a throwaway module outside the repo, reproduced the nil-deref
  panic and the content-sniffing XSS against a local server, then deleted the
  copy and showed `git status` clean. It also reported that `go vet` already
  fails on the branch — a claim about the build gate, not a taste judgement.

### Engine gaps (both fixed in #421, deployed as v3.40.6)

- **A repo-targeted launch could not clone from GitLab.** The runner clones
  with `http.followRedirects=false` (SSRF hardening) and GitLab 301s a bare
  repo path to its `.git` twin, so the clone died — through all 8 NATS
  deliveries, into the DLQ, with the redirect never named as the cause. GitHub
  serves both spellings, so nothing had ever exercised it. The launch handler
  now canonicalizes `repo_url` through `forge.CloneURLFor`.
- **GitLab could not READ commit statuses.** `CommitStatusLister` was
  GitHub-only, and both consumers fail closed without it: the gate reconciler
  abstains (a review that dies leaves the required context absent forever) and
  the zero-touch auto-fix lane refuses to act on a gate it cannot see. So the
  gate could be *posted* on GitLab but never *repaired* — the exact hole that
  had already been closed for GitHub. Implemented as the read-side mirror,
  keeping the newest row per name.

### Lessons for the next run

- **A BYOK key is a team-wide default, so billing isolation is a team
  boundary.** `secrets.Resolve` returns the first key of a provider visible to
  the run's team — `is_default` only orders candidates, it does not gate them.
  Putting a second sponsor's key on a team that already runs other repos
  silently moves those repos onto that sponsor's credit. A dedicated team per
  billing owner is the only clean separation. Once a key resolves, the CLI
  receives exactly `{ANTHROPIC_API_KEY}` and the pod's ambient forfait token is
  not forwarded, so the fallback cannot quietly take over.
- **Neither cost nor token counts prove which credential paid.** Both are
  estimated from token counts whatever the path (cost-signal work, native
  ticket), `env_keys=` in the spawn log counts the whole SDK env, and
  `last_used_at` is not serialised by the api-keys endpoint. The provider
  console is the only local proof.
- **A self-hosted forge may refuse outbound webhooks by policy.** GitLab's
  Admin → Network → Outbound allowlist rejects hook creation with a flat
  `Invalid url given` (HTTP 422) that names neither the setting nor the host —
  it looks like a malformed URL. Probe it early with a throwaway hook: the
  whole webhook half of an integration depends on it, while direct launches
  work regardless.
- Verifying a status on GitLab needs the **full** SHA: `GET
  /repository/commits/<short-sha>/statuses` returns `[]` rather than an error.

## 2026-08-04 — five review passes over the credential pool: what Revi caught that adversarial subagents did not (runs 019fc939 / 019fc94a / 019fc95c / 019fc972 / 019fcb7b)

- Status: **validated, and the highest-value reviewer signal measured so far.**
- Versions: iterion cloud prod v3.23.2 → v3.24.0 · bot `review-pr` as deployed
- Method: mono topology, gate `revi/review` required by the merge queue. Five
  passes across two PRs (#350, then #356) on a ~6000-line feature
  (`pkg/credpool` + publisher tier + runner accounting + studio + CLI).
- Result: 14 findings kept across the five passes (3 high, 8 medium, 3 low).
  Every high was real and fixed. #350 merged once the gate went green.

### The finding that matters most for calibration

The feature had already been through **four parallel `/simplify` agents and
two adversarial review agents at max effort** before Revi saw it. Those layers
produced 38 fixes. Revi then found, among others:

- **a `[high]` fail-open on the donor's bot allow-list.** `LaunchSpec.BotID` is
  empty for an inline uploaded `.bot`, and the availability check treated an
  empty bot id as "no filter to apply". A donor who restricted their
  subscription to one bot would have had it handed to arbitrary uploaded code —
  the restriction yielding on the single input the requester fully controls.
- **claw cost counted twice**, which would have drained every donor at 2× and
  silently tightened every tenant's monthly cap.
- **a `[high]` defect in one of my own fixes**: the per-slot allowance share
  was a no-op on the *renew* path, because `decideRenew` re-synthesises the
  `Limits` it judges with and dropped `MaxConcurrentRuns`. My regression test
  for that fix only exercised fresh acquisitions — which is exactly why it
  passed.

**Lesson.** Adversarial subagents share the context that produced the code, so
they inherit its blind spots. Revi reads the diff cold, and that is where its
value is: not in finding *more* problems, but in finding a *different class* of
them. Worth the ~15–20 min and ~$1–2 per pass on anything touching money,
credentials, or consent.

### The one no reviewer could have found

`iterion remote pool` could never work at all: `identityFromPAT` never sets
`OrgID`, so every CLI caller resolved to no pool and got "no credential pool
accepts contributions on this instance". Three review layers read that handler;
none *ran* it. It took one real call against prod. **A reviewer verifies what
the code says; only an execution verifies what it does.**

### Frictions

- Revi cannot run the repo's tests (no network in its sandbox; devbox cannot
  realise its nix closure), and says so in a verification caveat. Its findings
  are reading-derived — accurate here, but it means the gate is a *review*
  gate, not a test gate.
- The prod instance's Claude forfait hit its **weekly** window mid-session,
  blocking Revi and therefore the merge — the single-point-of-failure this very
  feature exists to remove. It could not have saved itself: not deployed, and
  no donor had pledged.

## 2026-08-01 — the hand-off measured working in production, and a second hole under it (runs 019fbc1d / 019fbc26)

- Status: **the hand-off is validated live.** A separate, pre-existing defect
  keeps the fixer from posting its verdict on the board lane.
- Versions: iterion cloud prod v3.18.1 @ `4bd82c830` (the `publish:` fix)
- Method: same PR, redeployed instance, `/revi` then `/billy`.

### Measured

`prior_review` reached the fixer at **5829 characters** — it was **0** before the
fix. It carried the stable id (`R727eac`), the anchor note pinned to the current
head, `confidence: medium`, the reviewer's ready-made replacement block, and the
open-questions channel. The engine emitted **6 `artifact_written` events** for
`diff_precheck`, `merge_reviews` and `converge`, under the exact publish names
declared; there were **zero** before.

That also settles the caveat left on the previous entry: the cause was the
missing `publish:`, not cloud storage. `GET /api/runs/<id>/artifacts` still
returns `[]` — that endpoint has no mongo listing behind it and is a red herring;
the targeted read the hand-off actually performs works.

The loop is real, not just wired: this review reads the commits the fixer pushed
in the earlier run and raises a second-order defect **in the fixer's own fix**
(`Warm` re-introducing an unrecoverable crash through the panic-recovery the
same branch had just added).

### The remaining hole: a board-launched bot cannot post anything

The fixer pushed, then `publish_verdict` returned *"no forge publish grant on
this run"*. Measured side by side on the same PR:

| | forge_publish_url | forge_publish_token | gate_context |
|---|---|---|---|
| reviewer (`mode: direct`) | yes | yes | `iterion/review` |
| fixer (`mode: board`) | — | — | absent |

So the fixer posts no verdict table, no finding ledger and no merge-gate status,
and the required check stays on the revision before its push. The grant is minted
in the webhook launch tail (`injectForgePublishVars`) and the operator's
`gate_context` is layered there too; a board-mode command materialises a card and
the cloud coordinator launches from `BotArgs` ONLY, so both are dropped. The
declared hand-off vars survive because `ensureBoardCard` copies them explicitly —
nothing else does.

Not caused by this work, and it is the exact pre-flight the plan for the gate
phase demanded ("verify `gate_context` and the publish grant actually reach a
board-launched fixer before designing on top"). The measured answer is no.

Fix direction: mint the grant at board-launch time rather than carrying it (a
grant has a TTL and a card can be claimed much later), and layer the
integration's operator launch vars there too.

## 2026-07-31 — the Revi→Billy hand-off read an artifact no node ever wrote (runs 019fb9bc / 019fb9c6)

- Status: **partial — Revi validated end to end, the hand-off to the fixer proved
  non-functional in cloud.** The defect predates the declarative rework: the
  shipped `stampPriorReview` read the same artifact.
- Versions: review-pr 0.5.7 · branch-improve-loop 1.1.0 · iterion cloud prod
  v3.17.7 @ `af787562c` (verified to contain the hand-off work)
- Method: `SocialGouv/iterion-test-appy-e2e` PR #2, seeded with a real module and
  three planted defects (unsynchronised map written from a `Warm` fan-out;
  a failed fetch cached for the whole TTL; a loop-variable capture that is NOT a
  bug under the declared `go 1.22`). Revi auto-launched on PR open; `/billy` by
  comment afterwards. Repo provisioned with both bots, `gate_context` pinned.

### What worked, verified on the forge

- **Revi found the real bugs and refused the planted false positive.** critical:
  the concurrent map access, *"reproduced empirically … crashed in 4 of 5 runs"*;
  high: the cached failure, *"call 1 returns connection refused, call 2 returns
  body=\"\" err=<nil>"*. The loop-capture did **not** become a finding — it went
  to `questions` with the reason (`go.mod` declares 1.22, per-iteration
  semantics, verified empirically). The falsifiability channel did its job.
- **Stable finding ids are live**: `Ra34eca`, `R1dce3f`, plus the arbitration
  line the review now carries — *"Fix them yourself, or comment /billy … adding
  e.g. skip Ra34eca and your reason leaves that one alone."*
- A `replacement` was produced and rendered ("Proposed replacement:").
- The gate landed: `revi/review = FAILURE` on the head.
- Mono topology reported honestly, no cross-confirmation claimed.

### The defect: the producing node never wrote an artifact, anywhere

`/billy` launched with the right PR context (`pr_url`, `head_sha`,
`push_branch`) and **`prior_review` empty**.

The first read of the evidence — `GET /api/runs/<id>/artifacts` returning `[]`
for every run checked, and 89 events with **zero `artifact_written`** — looked
like a cloud-storage gap. It is not (see the caveat below), and the correction
matters: the engine
persists an artifact **only for a node that declares `publish:`**
(`runtime.persistArtifactIfPublished` returns early otherwise, [engine_exec.go](../../pkg/runtime/engine_exec.go)).
Neither bot declared it on any node — `grep -c "publish:"` was **0** for both.

So the hand-off resolved `LoadLatestArtifact(runID, "converge")` against an
artifact that had never existed, on any run, local or cloud. Not a regression of
the declarative rework either: the version it replaced read the same node the
same way. It was recorded as shipped and never once exercised end to end.

Every test stayed green because every test **wrote the artifact by hand** —
the one thing that had to be true in production was the one thing never asserted.

**Fixed**: the four nodes the manifests name as hand-off sources now declare
`publish:`, and two guards make the omission impossible to repeat — a catalog
test requiring `publish:` on any node a manifest declares as a source, and an
e2e running the REAL engine over one node per source kind (agent, compute,
tool) plus an unpublished twin.

**Caveat, stated because the evidence does not cover it.** That the cause is the
missing `publish:` and *not* a cloud-storage gap is an inference from the engine
code plus a local run, not a measurement on prod: no cloud run has been observed
writing an artifact since. The mongo store implements `LoadLatestArtifact` and
the conformance suite pins multi-version latest-wins, so it is very likely — but
the honest status is unverified until a redeployed instance is re-dogfooded.

### Lessons for next run

- **A declaration is not a mechanism.** `produces: node: converge` reads as if
  naming the node makes its output available; it does not — the node must also
  publish. Any future hand-off kind needs the same pairing, and the catalog
  guard now enforces it.
- **A test that writes the fixture by hand cannot prove the producer writes it.**
  That is what hid this for the whole build. Where a contract spans producer and
  consumer, at least one test has to run the producer for real.
- The forge identity matters: the first `/billy` was refused *"self comment
  (loop-guard)"* because the repo was provisioned on a **PAT connection whose
  account is the operator's own**. Re-provisioning onto the GitHub App
  connection fixed it. The guard was right; the provisioning was the mistake.
- A freshly provisioned repo shows `auto_fix_on_gate_failure` absent — the
  zero-touch lane is off unless asked for, confirmed on real config.

## 2026-07-30 — Revi had stopped publishing on every repo, and finished green doing it

- Status: **defect found and fixed** — the runs were fine, the publishing step
  was dead. Found while wiring `iterion/review` as a required check, which is
  the only reason it surfaced at all.
- Versions: bot review-pr 0.5.6 · iterion `main` @ `7b87b5f37` + `34bd00879`
- Method: `/revi` on buildkit-operator #4 (run `019fb403-530a`, 2m42s) and #7
  (`019fb403-5f28`, 3m39s), plus the iterion PRs of the day (#323 →
  `019fb408`, ~12min). Cloud prod, mono topology.
- Result: after the fix, all three posted their review **and** their commit
  status. Before it, every one of them finished `finished` having posted
  nothing. Verbatim from #323 — both fixes visible in one line:

  > ## Code review by Revi (iterion)
  > 4 finding(s) kept after threshold/cap. — medium: 2, low: 2
  > Reviewed by a single model family (mono topology): no finding is
  > cross-confirmed, and none is meant to be.

  with `revi/review=SUCCESS` on the head.

### The defect: a template that never resolved

`publish_review` built its guard input as `REVIEWED_SHA={{outputs.…}}` inside a
tool node's `command:`. That body is resolved by
[resolveCommandTemplate](../../pkg/backend/model/executor_tool.go), which
substitutes `{{input.X}}`, `{{vars.X}}`, `{{secrets.X}}` and `{{run.id}}` —
**`{{outputs.…}}` resolves only in edge mappings**. Written in a body it
survives as literal text.

So the stale-anchor guard compared the literal string `{{outputs.…}}` to the
PR's head sha, concluded the anchors were stale, and skipped the whole publish:
review, inline comments, and gate status. No error anywhere — the node
succeeded, the run finished, and the PR simply never heard from Revi. This had
been true **repo-wide**, on every review, for as long as the guard existed.

Had the required check been switched on before this was found, it would have
blocked every pull request on the repo — an outage caused by a check that was
never posted, on runs reporting success.

### Second defect on the same path: the guard took the gate down with it

Even a *genuinely* stale anchor set skipped the entire publish. But a stale
inline anchor only means the line numbers moved — it says nothing about the
verdict. Dropping the gate along with the comments turns a cosmetic problem
into a permanently absent required check. `stale_anchors` now drops the inline
comments and **keeps publishing** the summary and the status.

### Third: mono claimed a cross-family confirmation that never happened

The summary printed `N finding(s) cross-confirmed by both model families` even
in mono topology, where one family ran. Spotted by jo on the real comment on
buildkit-operator #6 — the reviewer was describing a corroboration it had no
way to perform. Mono now says so in as many words.

### Guards added

- `bots/catalog_command_refs_test.go` — catalog-wide: **no** `{{outputs.…}}` in
  any tool `command:`/`script:`/`postcondition:`, walking every `.bot`. The
  class, not the instance: the same silent no-op was available to every bot in
  the catalog.
- `bots/review_pr_stale_anchor_test.go` — drives the real publish body against
  a stub, shell-quoting substitutions the way the engine does, so the guard is
  exercised on the code that ships rather than on a paraphrase of it.

### Lessons for next run

1. **`{{outputs.…}}` in a command body is a silent no-op**, not an error. Any
   comparison against one is a comparison against a constant string — it will
   take whichever branch that constant happens to select, forever.
2. **A guard that suppresses output must never suppress the verdict.** Degrade
   the part that is unsafe (the anchors), keep the part a required check
   depends on.
3. A bot that publishes nothing looks exactly like a bot with nothing to say.
   Neither the run status, nor the logs, nor a green test suite distinguished
   them here — making the check *required* is what finally did.

## 2026-07-08 — GitHub PR webhook e2e on iterion cloud prod

- Status: **validated — full end-to-end via the inbound webhook.**
- Versions: bot review-pr 0.2.0 (post the `emit`→`converge` rename below) · iterion cloud prod `:edge` @ 93bc604+
- Method: cloud prod (ovh-prod). Connected a GitHub forge (PAT) on a fresh team,
  enabled Revi on a test repo (`SocialGouv/iterion-e2e-mathkit`), opened a PR
  with an intentional defect (`subtract` skipping the module's `assertFinite`
  input-validation invariant). The `pull_request` webhook launched Revi on a
  cloud runner (no sandbox).
- Result: both reviewer families ran (reviewer_claude/claude-code +
  reviewer_gpt/gpt-5.5), `converge` merged them, `publish_review` posted a GitHub
  review (COMMENTED) — "2 findings (1 medium, 1 low; 1 cross-confirmed)" + 2
  inline comments (src/calc.mjs:26 medium correctness, test/subtract.test.mjs:8
  low tests). Both families independently caught the planted defect;
  cross-confirmation worked.
- Engine hardening surfaced by this run:
  - **The bot didn't parse in prod** (`agent emit:` shadowed the reserved `emit`
    node keyword, ADR-051 → E002 → webhook 502). Fixed by renaming the node to
    `converge`; added a CI guard (`TestCatalogBotsParseAndCompileClean`) that
    fails on any catalog bot that doesn't parse+compile — the gap that let it
    ship (both catalog-loading tests skipped on parse failure).
  - **Webhook idempotency poisoned by a failed launch**: the initial `opened`
    delivery 502'd but still consumed the idempotency key, so redeliveries
    returned `duplicate` (empty run_id) forever. Fixed: a StatusLaunchError row
    is now retryable. Only a NEW head sha (close/reopen after a push) unblocked
    the validation.
- Lessons for next run: `synchronize` does NOT re-trigger Revi by design
  (opened/reopened only) — to re-review, close/reopen or push a new head sha.
  Revi posts as the PAT's account (`devthejo` here); a dedicated bot account
  would read cleaner.

## 2026-06-13 — review the campaign diff (run 019ec0e8)

- Status: **validated — high value.**
- Versions: bot review-pr 0.2.0 · iterion 7fea84cd (binary refreshed mid-campaign)
- Method: `POST /api/runs`, `base_ref=9197bcfd` (review the campaign's own fresh
  commits `9197bcfd..HEAD` — the `scan_shards`/`botregistry` fixes + the bilans),
  `severity_threshold=low`, `post_to_board=true`. Read-only, no sandbox. Backends:
  `claude_code` (reviewer_claude, emit) + `claw` gpt-5.5 (reviewer_gpt). ~37k tokens,
  ~$1.18, 151 steps, status `finished`.
- Result: `diff_precheck` (found changes) → fan-out **reviewer_claude ‖ reviewer_gpt**
  (parallel, confirmed) → `emit` → **1 deduped board issue** (`source:revi`,
  `severity:medium`, `type:correctness`). No commits (read-only, as designed).

### Value (genuinely high — caught a real second-order bug)
- The single finding is excellent: **"Cloud request-construction failures block until
  shard timeout" at `cmd/iterion/scan_shards.go:458`** — i.e. Willy's fix `4c525a6e`
  (handle the dropped `http.NewRequestWithContext` error) is *masked* by `awaitTerminal`,
  which polls a run document that never exists for a never-launched shard, hanging until
  `--timeout` (default 2h) instead of failing fast. Precise anchor, correct mechanism,
  actionable fix sketch. **Verified against the code and fixed** (`59cfedcc`, with a
  regression test). The pre-existing `ITERION_SERVER_URL`-unset / read-workflow paths
  had the same latent hang.
- **No noise:** the diff was mostly docs (≈280 of 387 lines) + two small code changes;
  Revi flagged 0 in the clean botregistry dedup, 0 in docs, and 1 real issue in the
  changed Go. Cross-family dedup worked; severity/type/confidence labels are clean.
- **Dogfood dynamic worth keeping:** a *breadth* bot (Revi) caught an incompleteness in
  a *depth* bot's (Willy) committed fix. Running review-pr over each loop bot's output is
  a cheap, high-signal second line of defence.

### Findings / misses
- The finding came from the **gpt** reviewer only (confidence `medium`) — Claude's
  reviewer didn't independently raise it. Single-family findings are real but lower-
  confidence; the cross-family agreement signal didn't fire here (still correctly
  published at the `low` threshold). No false positives.
- Minor: the `emit`/`reviewer_*` node outputs aren't surfaced in `run.json.checkpoint`
  in a easily-parsed shape (had to read the board to see findings) — cosmetic.
- **Repo scatter (low — repo-agnostic):** `report_path` defaults to
  `.review-pr/findings.md`, so Revi drops an **untracked `.review-pr/` dir into the
  target repo root** (not gitignored). Per CLAUDE.md "Catalog bots are repo-agnostic",
  a default that writes into the target tree should be gitignore-friendly. Fixed here by
  adding `.review-pr/` to iterion's `.gitignore`; for a pure dry-run pass
  `--var report_path=/tmp/revi-findings.md`. (A nicer bot-side default would append the
  dir to the target's `.gitignore`, or write under a path the operator already ignores.)

### Engine hardening
- `awaitTerminal` pre-dispatch-failure hang — **fixed `59cfedcc`** (+ regression test
  `TestAwaitTerminal_PreDispatchFailureDoesNotHang`). Directly attributable to this run.

### Lessons for next run
- Revi is a strong, low-noise read-only reviewer; point `base_ref` at the commit before
  the work to review a clean range (`base..HEAD`). Default `post_to_board=true` lands one
  issue per finding under `source:revi` — fine for real triage, set `false` for a pure
  dry-run.
- Use Revi as a routine second pass over Willy/Featurly/Billy output — it catches
  second-order issues the implementer's own review loop can miss.

<!-- Live probe note: this very PR exercised the 0.7.0 stack end to end —
     PR-open review (immediate), then this push (debounced). -->
