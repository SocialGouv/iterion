# Merge gate — Revi arbitrates, determinism disposes

Revi (`bots/review-pr`) can post a **deterministic merge-gate status** on a
PR so an unresolved blocking finding keeps the PR out of the merge queue —
without ever letting an LLM be the yes/no arbiter of a merge.

The split is deliberate:

- **Revi (LLM) proposes** — one reviewer by default (`review_mode: mono`),
  or independent Claude + GPT reviewers when `review_mode: dual` is an
  intentional extra spend, find issues that are normalised, de-duplicated,
  and posted as inline PR comments.
- **Determinism disposes** — the bot computes a **count** of findings at or
  above a severity floor and the server posts a `revi/review` **commit
  status** (`success` when the count is 0, else `failure`). The gate is a
  count, never the review verdict. Non-blocking [`questions`](#questions)
  never count.
- **A human arbitrates** — a false positive is cleared by pushing a fix
  (which re-reviews) or, for a disputed finding, by a maintainer override
  (see [Overriding](#overriding-a-finding)).

This mirrors the repo's standing doctrine: **gates stay deterministic**
(see `CLAUDE.md` → "Improvement loops must converge"). The reviews
themselves stay non-blocking advice (`forge.NewReview` never
approves/requests-changes); the entire gate lives in the separate commit
status.

## How it works

```
PR opened / pushed ──▶ launch claims the head:  revi/review = pending
                              │                 ("review in progress")
                              ▼
                       Revi runs (selected reviewer topology → merge → publish)
                              │
                              ├─ inline comments  (advisory)
                              └─ revi/review status on the head SHA
                                    success  ⟺  0 findings ≥ gate_severity
                                    failure  ⟺  ≥1 finding  ≥ gate_severity

               run dies without publishing ──▶ reconciler posts failure
                                               (event + 1-min sweep)
       run PARKS on a provider quota ──▶ check stays claimed, pause notice
                                          comments when it resumes
              pull request closed/merged ──▶ its runs stop, retries disarmed
```

The context is claimed at launch and answered at the end, so the check is never
silent while a review is running — see [the in-flight
claim](#inflight) and [the repair](#interrupted).

1. **Trigger.** Revi auto-reviews on PR `opened`/`reopened`. For the gate to
   track fixes, enable **`ReviewOnSync`** on the webhook so a push
   (`synchronize`) re-reviews the new head — otherwise the required check
   would never appear on the pushed SHA and the merge would deadlock.
2. **Verdict.** The bot's deterministic `publish_review` node counts findings
   whose severity is at or above `gate_severity` (default `high`), and sends
   `{enabled, blocking_count, threshold, total_findings}` in the publish
   payload.
3. **Status.** The server (`/api/v1/forge/publish-review`) resolves the PR
   head SHA and posts the `revi/review` commit status through the team
   connection's **live** forge client (a GitHub App mints a fresh token per
   call — no workspace credential, no ~1h token freeze). Forge-agnostic:
   GitHub commit-status / GitLab commit status / Forgejo commit status all
   expose the same primitive ([`pkg/forge/status.go`](../pkg/forge/status.go)).

   > **Forge permission (required).** Posting a commit status needs write on
   > statuses: a **GitHub App** must grant **Commit statuses: Read and write**
   > (a token connection needs `repo:status`); GitLab/Forgejo tokens need `api`
   > / `write:repository`. Without it `SetCommitStatus` returns 403
   > *insufficient scope* — the review still posts and the failure is reported
   > (`gate_error`) + logged (`forge gate: … not posted: … insufficient
   > scope`), so the gate silently *advises* instead of blocking. Grant the
   > permission and re-accept the installation before relying on the gate.
4. **Enforcement.** List `revi/review` in the repo's **required status
   checks** (branch-protection ruleset). Until you do, the status is a
   harmless advisory check you can preview on every PR.

The status write is **additive**: if it fails (missing capability, forge
error, no head SHA), the review still publishes and the failure is reported
in the response (`gate_error`) and logged — never a silent no-op, never a
failed publish.

## Configuration

Bot vars ([`bots/review-pr/main.bot`](../bots/review-pr/main.bot)):

| Var | Default | Meaning |
|-----|---------|---------|
| `gate_enabled` | `true` | Post the `revi/review` status. `false` = advisory-only reviews. |
| `gate_severity` | `high` | Severity floor that blocks (`low`<`medium`<`high`<`critical`). `high` = high+critical block; low/medium advise. |

Webhook config ([`pkg/webhooks/types.go`](../pkg/webhooks/types.go)):

| Field | Default | Meaning |
|-------|---------|---------|
| `review_on_sync` | `false` | Re-review on each push so the required status re-evaluates on the fixed head. **Required for a blocking gate.** |
| `block_fork_prs` | `false` | Persisted and returned by the webhook CRUD API, but **no launch path reads it** — the only references are the struct field and the two CRUD assignments. Setting it changes nothing on any provider. See the caution for what actually guards fork PRs. |

> **Caution — budget with `review_on_sync`.** The sync lane re-runs Revi's
> selected topology on **every push** (each new head SHA): one LLM reviewer in
> the default mono mode, two when dual is explicitly selected. It is gated only
> by the webhook's `AuthorAllowlist` (empty = any author) and per-head idempotency —
> there is no author-trust gate on this lane. On a public repo a fork
> contributor pushing repeatedly can drive repeated full reviews, bounded only
> by the org launch gate + webhook rate limit.
>
> **Where fork PRs actually stand, per provider.** On **GitHub** and
> **Forgejo** the guard is unconditional and needs no configuration: an
> inbound PR event whose head is a fork is filtered before the sync lane
> is even considered, so the fork re-review exposure above cannot occur
> there. The `/command` lanes refuse a fork (or an unnamed head repo) too —
> same-repo only, silently: the fork's work needs a branch in the base repo
> before any bot runs on it. On **GitLab** the inbound MR lane has no fork/cross-project
> guard, so a forked-MR auto-review *is* reachable — bound that lane with
> an `AuthorAllowlist` / `MinAuthorRole`, since `block_fork_prs` is inert.
> The two lanes that resolve the MR through the API instead — auto-fix and
> gate relaunch, both fail-closed on an unproven head repo — do guard on
> GitLab: a same-project MR qualifies (the MR's source and target project
> ids agree, so the head lives in the project the lane queried), a fork MR
> is refused (GitLab's MR payload names the source project's id only, never
> its path, so the head stays unproven).

## <a name="review-tiers"></a>Review tiers — glance / guard / audit

A repo's criticality or budget policy can pick ONE preset (`review_tier`)
instead of tuning five separate vars ([SocialGouv/iterion#685](https://github.com/SocialGouv/iterion/issues/685)):

| Tier | severity_threshold | max_findings | post_to_board | review_mode | Reviewer model |
|------|---------------------|--------------|----------------|-------------|-----------------|
| `glance` | `high` | `5` | `false` | mono only | cheaper same-family (`claude-sonnet-5` / `openai/gpt-5.4-mini`) |
| `guard` (**default**) | `medium` | `15` | `true` | mono (auto-resolved) | full-strength (`claude-opus-5` / `openai/gpt-5.5`), unchanged since 0.7.0 |
| `audit` | `low` | `40` | `true` | **forced dual**, regardless of `review_mode` | full-strength, both families |

`guard` is byte-identical to the bot's pre-#685 posture — an unpinned repo
sees no behaviour change. The tier is a **preset, never a cage**: every
knob above stays individually overridable via its own `--var` — a
sentinel default (`"auto"` on the string vars, `0` on `max_findings`)
means "let the tier decide"; any concrete value is an explicit operator
override and wins, on every tier. `gate_severity` (the merge-blocking
floor) is deliberately **not** tier-varied — every tier keeps the same
blocking bar by default, so `audit`'s lower `severity_threshold` surfaces
more low/medium findings as advisory PR comments without silently making
them merge-blocking. A deterministic `tier_expand` compute node (no LLM)
resolves the concrete values right after `diff_precheck`, so a capped or
frugal review is deterministic, not a judgment call.

**The measured floor argument (why glance attacks ingestion, not just
output).** A day of production cost data on this repo fit `cost ≈ $2.24 +
$0.00072 × added lines` — between +39 and +210 lines (a 5× size range),
cost moved by only $0.60. Below roughly 500 lines **the floor dominates**:
what a reviewer ingests before it reads a single diff line (a claude_code
node's context injection — this repo's own `CLAUDE.md`/`AGENTS.md` — the
plausible reason iterion's own floor ≈ $2.24 against
code-du-travail-numerique's ≈ $1.84). A tier that only caps the diff, the
findings, or the max_findings ceiling cannot move a small PR below ~$2 —
it is trimming the part that already costs the least. So `glance`
attacks the floor two ways: a cheaper model (see below), and a prompt
instruction telling the reviewer to skip exploratory reads beyond the
diff itself (`--stat` + the hunks + at most one targeted grep — never a
whole surrounding package). Skipping claude_code's own context-file
injection is a further, larger lever this pass did NOT take — it would
need a per-node `setting_sources:` DSL field (today `ITERION_CLAUDE_CODE_SETTING_SOURCES`
is engine-wide only), a genuinely new capability, filed as a follow-up
rather than bundled into this preset.

**The model-per-tier mechanism.** A node's `model:` (and `reasoning_effort:`)
field resolves ONLY `${ENV_VAR:-default}` from the process environment,
never `{{vars.x}}` — true on every backend, including `claw`'s in-process
path, not just the CLI-delegated ones. So a `--var review_tier=glance`
cannot retarget `reviewer_claude`'s/`reviewer_gpt`'s model directly; the
bot instead declares two extra judge nodes, `reviewer_claude_glance` /
`reviewer_gpt_glance`, pinned to the cheaper defaults, and the EXISTING
`topology` condition router (ADR-052) picks between the full-strength and
glance variant per family — the pattern to imitate for any future
tier/variant knob that needs a different model, never a new engine branch.

**Per-repo pin.** `review_tier` is an ordinary launch var, so it is
pinned exactly like `gate_context` or `post_to_board` — through the
integration's `launch_vars` (durable across re-provisioning, generic
pass-through, no new engine code):

```sh
iterion remote forge repo-bots create --data '{
  "connection_id": "<conn-id>",
  "repo": "owner/repo",
  "bot_ids": ["review-pr"],
  "launch_vars": { "review_tier": "glance" }}'
```

The studio's repo detail page (`/repos/:key`) surfaces a three-position
"Review tier" selector once `review-pr` is bound to that repo, writing the
same `launch_vars.review_tier` field. A webhook-triggered launch never
overrides an operator's pin — `reviewPRVars` / `buildPRForgeCommandVars`
apply `launchVars` LAST, the same precedence every other operator pin on
this bot already relies on.

## Activating the blocking gate on a repo

The code posts the status unconditionally (advisory). To make it **block**,
add the gate's context to the branch-protection ruleset's required status
checks (for this repo, the "main protected — merge queue" ruleset — see
[merge-policy.md](merge-policy.md)):

```sh
# inspect the current ruleset, add the context to required_status_checks,
# then PUT it back:
gh api repos/OWNER/REPO/rulesets/<id>
```

Re-review on push is **not** something you have to remember: a status lives
on one commit, so a gate that did not follow the head would leave the check
absent — indistinguishable from "never reviewed" and unblockable by another
review. Provisioning therefore turns `review_on_sync` on by itself for any
repo where a co-enabled bot declares the `statuses` scope. The field is
still settable per webhook (`review_on_sync` on the webhook API) for the
cases the derivation does not cover.

Repo admins keep their merge-queue bypass, so a stuck gate is never a hard
block for an admin.

### A paused review says so on the PR

A review that hits the provider's quota does not die: the runner arms a
durable retry and the reconciler leaves the check alone, because that armed
retry is the promise. But the check then sits on its in-flight claim, which
reads exactly like a review that died — and nothing said how long to wait.
So the park **comments on the pull request**, once per park (fired from the
run-outcome event, never the every-minute sweep): what happened, the instant
the retry fires, the attempt number, and the provider's own sentence. When
that sentence is an account **spend ceiling** rather than a time window, the
notice says so explicitly — a ceiling reopens when an admin raises it (or
the month rolls), so the armed retries can otherwise exhaust themselves
against a wall.

The notice is posted for **every run that owes the gate a verdict** —
which includes a fixer that gates the head it pushes — but its wording is
the review's ("the verdict lands here … a new push restarts it sooner"), so
a parked fixer reads as a parked review, and the advice is the one thing not
to do while a fixer works. Known gap, tracked as SocialGouv/iterion#650: the
notice should name the parked run's role.

Symmetrically, a pull request that **closes or merges** ends every run bound
to it and **disarms** their retries: an in-flight review would keep spending
quota on a diff nobody will merge, and a parked one would wake hours later
to comment on a dead PR. See [webhooks.md](webhooks.md). Known gap
(SocialGouv/iterion#663): a redelivery already in flight at the close can
re-claim the run seconds after it was stopped — check `iterion remote runs
list` after a merge and cancel a reviver by hand.

## Disabling the gate per repo — first review only, re-review on demand

Some repos want the opposite posture: reviews as **advisory comments only**,
with the automatic review on MR/PR open the only automatic one and every
re-review a deliberate human gesture (budget-frugal — no run per push). One
operator pin buys the whole posture. On the integration's launch vars
(`launch_vars` on the repo-bots API, or the studio integration settings):

```json
{ "gate_enabled": "false" }
```

Two properties of the pin worth knowing before setting it:

- it is **repo-wide**: the operator var layer applies to every co-enabled
  bot, so pinning it to quiet one gating bot also releases head-tracking
  for the others on that repo;
- the release is **logged at Warn** at the moment it becomes definitive
  (a provision that drops a previously-forced `review_on_sync`), and any
  later launch-vars update that replaces the map WITHOUT the pin silently
  restores the gating posture (fail-safe direction — the gate follows the
  head again);
- the derivation only rewrites **unpinned** syncs: a `review_on_sync` an
  operator set explicitly through the webhook API is provenance-pinned
  (`review_on_sync_pinned`) and never silently replaced in either
  direction — so per-push advisory reviews WITHOUT a gate (sync pinned
  true + `gate_enabled: "false"`) is expressible and survives
  re-provisions.

What the pin disarms, end to end:

- the bot's publish step skips the commit status — no verdict context ever
  lands on a head;
- the server-side gate machinery never arms: the in-flight `pending` claim
  at launch, the reconciler, the sweeper and the auto-fix lane all read the
  SAME pin (`forge.GateValueDisables`) — so even the half-configured shape
  (gate disabled while a stale `gate_context` pin remains) claims nothing
  and paints no synthetic failure; dropping the `gate_context` pin as well
  is still the clean form;
- provisioning **stops forcing `review_on_sync`** — and releases one it had
  forced earlier ([`pkg/forge/orchestrator.go`](../pkg/forge/orchestrator.go),
  `operatorGateDisabled`). The forced sync exists solely to keep a REQUIRED
  check alive across pushes; with the gate off it would only burn a review
  per push. The pin survives re-provisions, unlike a bare
  `review_on_sync: false` PATCH on the webhook config (which the next
  provision's derivation would overwrite).

Re-review stays on demand through two gestures — with different hold-label
postures:

- a **`/revi` comment** on the MR/PR — exempt from the hold-label pause, like
  any `/command`: a comment is unambiguously a deliberate human trigger;
- the forge-native **"Re-request review" button** on iterion's bot reviewer
  (see [webhooks.md](webhooks.md#re-request-review)) — **vetoed by the hold
  label**: the forge emits the same event for a CODEOWNERS auto-request,
  which needs no permission from the requester and carries nothing to tell it
  from a click, so the lane cannot claim a command's deliberateness (the
  rationale lives with the lane in webhooks.md). On GitLab the publish step
  self-assigns the bot as an MR reviewer after each review precisely so this
  button exists; each click re-reviews the current head, even twice on the
  same head.

Removing the pin restores the gating posture: the next provision re-derives
`review_on_sync: true` from the `statuses` scope.

**Reading back whether the pin took.** `GET /api/teams/{id}/webhooks`
serialises the config. Read `operator_launch_vars`: it carries the pin
verbatim (mirrored onto the config at provision) and is the authority BOTH
consumers read — the `review_on_sync` derivation and the gate machinery's
own `runGateDisabled`. `"gate_enabled": "false"` there is the answer.

Two neighbouring fields say what the pin *did*, with one JSON trap: they
are `omitempty` bools, so **absence means `false`**, not "unknown".

- `review_on_sync` — `true` = a push still re-reviews; absent = released.
  It is a *consequence*, not the pin: in the sync-pinned-true +
  `gate_enabled: "false"` shape described above (advisory reviews on every
  push, no gate) it reads `true` while the gate is off, so it answers
  "does a push re-review", never "is the gate armed".
- `review_on_sync_pinned` — whether an explicitly-PATCHed sync is
  provenance-protected from the derivation (see above).

Do not read the reviewer's `bot_rules.actions` for this: that list is
materialised from the bot manifest's invocation
([`resolveBotRules`](../pkg/forge/orchestrator.go)) and is identical either
way, while the push decision reads `cfg.ReviewOnSync` directly
(`gateResync` in the webhook handlers). It would report "released" on an
armed gate.

Pair it with two run-time observations, which is what actually proves the
posture end to end: a push's webhook delivery is recorded **`filtered`**,
and the head of an open MR/PR carries **no** status. Query that last one
with the **full** SHA — GitLab's statuses endpoint returns `[]` for an
abbreviated one whether or not a status exists (measured on 19.2: full SHA
→ `["iterion/review"]`, same commit at 8 chars → `[]`).

### GitHub merge queues

A merge queue tests a synthetic `merge_group` SHA, not the PR head that Revi
reviewed. A required status posted only on the PR therefore never appears on
the queue branch. This repository handles that with
[`.github/workflows/merge-queue-gate.yml`](../.github/workflows/merge-queue-gate.yml):
it extracts the PR from the queue ref, reads the latest `revi/review` status on
the PR head, and mirrors that exact state and target URL onto the queue SHA. It
never invents success: no source status means no mirrored status, and a
non-success verdict makes the workflow fail after publishing it.

The workflow currently names `revi/review` explicitly. If a repository pins a
different shared `gate_context`, its merge-group workflow must mirror that same
context (or otherwise run the gate on the merge-group SHA), or the required
check will remain expected forever.

#### Auto-heal, and when it stands down

A PR the queue **ejects** for a healable reason (`MERGE_CONFLICT`,
`CI_FAILURE`, `INVALID_MERGE_COMMIT`, `MERGE_CONFLICT_ERROR`) dispatches the
brancher bot to rebase, reconcile the branch with the new base, and push so
the PR re-enters the queue — the queue *detects* the break, the bot *repairs*
it, no human. The heal is bounded to one attempt per head sha, and gated on
same-repo + project/author allowlist + bot-permitted like every other lane.

The heal **stops the moment the queue takes the PR back** (`enqueued`). This
is not an optimisation: a heal still running past that point force-pushes the
branch, and that push cancels the queue build in flight — ejecting the PR a
second time, so the repair becomes the next breakage. The stop is keyed on the
heal's own idempotency key at the PR's *current head*, which gives it two
properties worth knowing:

- a fixer a developer asked for with `/billy` rides a different key and is
  never touched;
- a heal that has **already pushed** advanced the head, and that push is what
  re-enqueued the PR — so the enqueue it caused does not match its own key,
  and the run is left to finish its delivery tail.

Note what auto-heal cannot tell on its own: `CI_FAILURE` covers both "this
branch genuinely breaks when combined with the base" and "an unrelated flaky
test failed on the queue branch". In the second case the bot is dispatched
against a PR with nothing to fix; it should report that and push nothing, but
it still costs a run. If a flaky test is ejecting PRs, fix the test — the heal
lane is not the place to absorb it.

## One gate, several bots

A required check applies to **every** pull request. So on a repo where
different bots review different PRs — a dependency guard (Vetty) on the
update bot's PRs, the reviewer on the humans' — neither bot's own context
can be the required one: whichever bot did not run leaves the check
permanently absent and blocks the PR. Requiring both is worse, since each
then blocks the other's PRs.

Give them the same context instead. `gate_context` is a var on every bot
that can gate (`revi/review` on Revi, `vetty/deps` on Vetty by default), so
the repo declares one shared name and each bot fills it for the PRs it owns:

```sh
iterion remote forge repo-bots create --data '{
  "connection_id": "<conn-id>",
  "repo": "owner/repo",
  "bot_ids": ["review-pr", "dep-update-guard"],
  "launch_vars": { "gate_context": "iterion/review" }}'
```

A review webhook is also where `overlap: supersede` earns its keep: with
re-review on push, a burst of commits launches a run per push and the earlier
ones reach a verdict about code that no longer exists. Set it on the same
call (`"overlap": "supersede"`); it is persisted on the integration like the
launch vars, so a later `bots enable` does not silently drop it.

Pin it through the **integration's** `launch_vars`, not the webhook's:
provisioning rewrites the whole webhook config from the bots' manifests, so
an override PATCHed onto the webhook is dropped at the next enable. The
integration's launch vars are persisted and re-applied on every provision.

Then require `iterion/review` — one check, whichever bot owns the PR.

### Two bots on the SAME pull request

Revi and Vetty share the context by owning **disjoint PRs** (`author_scope:
exclusive` routes the update bot's PRs to one and the humans' to the other), so
they never write the same status.

A fixer is different: it acts on the pull requests a reviewer already reviewed.
They share the context **sequentially**, and the ordering is what keeps them
from fighting:

1. the reviewer reviews head A and posts its count on A;
2. the fixer runs, pushes, and head B appears — the required check is now
   **absent** on B, which blocks the PR with nothing explaining why;
3. the fixer posts its own verdict on B, immediately after its push;
4. that push is a `synchronize`, so with `review_on_sync` on (derived ON for
   any repo where a bot declares the `statuses` scope) the reviewer re-reviews
   B and its verdict — the authoritative one, from a reviewer that did not
   write the code — lands minutes later and supersedes.

Step 4 is deliberately exempt from the iterion-bot guard, which otherwise skips
a delivery our own forge bot sent. Here the sender *is* the bot by
construction — it is the fixer that pushed — and this is the one delivery the
gate cannot lose.

Step 3 is the one that needs care, because the fixer wrote the code it is
grading. Three rules keep it honest, and a fixer that gates must implement all
three:

- **The verdict is a count, never a judgement.** Findings not fixed, plus the
  deterministic build gate, plus its own re-review of the diff.
- **A contested finding still blocks.** A fixer may argue a finding is wrong —
  in the open, with its reasoning, on the PR — but the argument goes to a
  human, never to the gate. If a refusal could green the check, a fixer would
  clear any review by contesting every finding.
- **It speaks only for the revision it produced.** Pushed nothing → post
  nothing, or it overwrites a verdict it has no standing to replace. Pushed
  code that no review has read → not green, since there is no review of that
  revision to report.

A green from a fixer says so in its description, so it is never mistaken for an
independent review.

Step 3 needs two things the bot cannot mint for itself: a **forge-publish
grant** and the repo's **pinned `gate_context`**. The server resolves both from
the repo's integration on every lane that targets a pull request — the webhook
tail, the studio/API launch, and the cloud board coordinator, which claims a
card long after the webhook that created it is gone. Without them the run
pushes and reports `no forge publish grant on this run`: no verdict, no gate,
and the required check left on the pre-push revision, which blocks the PR on a
check that is *absent* rather than red. If a fixer posts nothing, look at the
run's inputs for `forge_publish_url` and `gate_context` before looking at the
bot.

### <a name="autofix"></a>Zero-touch: letting a red gate launch the fixer itself (opt-in)

By default nothing happens when the gate goes red: the findings are on the pull
request and the developer decides — fix them, argue one, or hand the work over
with a `/command`. That is deliberate. A reviewer already leaves the human in
the middle, and making the hand-over automatic everywhere removes that choice
from every developer on the repo to save one comment.

A repo that wants the loop closed anyway turns it on per repo:

```sh
iterion remote forge repo-bots create --data '{
  "connection_id": "<conn-id>",
  "repo": "owner/repo",
  "bot_ids": ["review-pr", "branch-improve-loop"],
  "launch_vars": { "gate_context": "iterion/review" },
  "auto_fix_on_gate_failure": true }'
```

Then a review that leaves `iterion/review` red launches the repo's fixer on that
head, with no command typed. The fixer is not named anywhere: it is whichever
enabled bot declares `consumes: kind: review`, since that declaration already
means "I start from a review and act on it".

**What bounds it.** Two limits, because one is not enough:

- **One attempt per head sha.** The fixer pushes → the head moves → a re-review
  produces a fresh verdict → a new attempt becomes available. A fixer that
  pushes *nothing* leaves the head where it is, and the claim on that head is
  already spent, so the loop ends there.
- **Five passes per pull request.** That first bound only stops a fixer that
  stops pushing; one that keeps pushing without converging frees a fresh claim
  every cycle. After five unattended passes the lane stops and leaves the PR to
  a human — the `/command` road is still open.
- **Three launch attempts per head.** A fixer that cannot even be *started* (a
  queue outage, a deploy window, a broken plugin source) spends neither of the
  two bounds above — the claim binds only once a launch succeeds, and the
  per-PR ceiling counts launched passes — so it used to be re-attempted on
  every sweep offer, once a minute, for as long as the run stayed in the sweep
  window (2026-08-26: ~90 minutes of it). The launch failure is now retried on
  a backoff (5 min, then 10 min) from the count the claim row itself carries
  (`attempts` / `failed_at` on the delivery), and after the third failure the
  lane files a board card labelled `source:gate-autofix` + a PR comment naming
  the failure and stops — a new head gets a fresh budget.

It also obeys the ordinary launch gate (org quota, cost cap, concurrency) and
the hold label, which pauses this lane like every other. Note the org cost cap
defaults to *unlimited*, so it is a backstop only where you configured one.

**What it refuses.** It reads the verdict back from the forge rather than
trusting our own bookkeeping, and abstains when the provider cannot list
statuses. It acts only on the check the repo itself pinned as its gate — a run
naming some other context does not qualify — only on the revision the finished
run actually judged, only inside the repo the run's publish grant covers, only
for a bot the webhook permits, and never on a fixer's own red verdict. Where a
brake cannot be *evaluated* (the hold label unreadable, the attempt audit
unreadable) it does not launch: an unevaluable bound is not a cleared one. Omitting
`auto_fix_on_gate_failure` on a later call leaves the repo's current choice
alone — enabling one more bot never switches automation on or off by itself.

## <a name="three-roles"></a>Revi / Billy / Vetty — one gate, three roles

The close collaboration between the reviewer (Revi), the fixer
(Billy/`branch-improve-loop`) and the dependency guard (Vetty/
`dep-update-guard`) on a **shared gate** is a design point, not an
accident — but until it is written down in one place, the next agent
re-derives it from three scattered sections. This is that place
([SocialGouv/iterion#650](https://github.com/SocialGouv/iterion/issues/650)).

**What IS wired today:**

- **Disjoint ownership, one context.** Revi and Vetty share `gate_context`
  by owning disjoint PRs (`author_scope: exclusive` routes the dependency
  bot's own PRs to Vetty, everyone else's to Revi — [above](#one-gate)), so
  they never write the same status. A fixer is different: it acts
  SEQUENTIALLY on a PR a reviewer already reviewed, and [the ordering that
  keeps them from fighting](#two-bots-on-the-same-pull-request) is what
  "Two bots on the SAME pull request" describes — the fixer posts its own
  verdict on the head it pushed, then the reviewer's re-review supersedes
  minutes later.
- **The blind window is short, not zero.** A push is followed by nothing on
  the required check until `review_on_sync`'s re-review launch claims
  `pending` on the new head — measured on PR #646 (2026-09-03): the claim
  landed **4 seconds** after the push, twice in the same PR's lifecycle.
  `review_on_sync` derives ON automatically whenever a bot on the webhook
  gates merges ([`pkg/forge/orchestrator.go`](../pkg/forge/orchestrator.go)),
  so this is the default posture, not something each repo has to remember
  to enable for the loop to close.
- **Hand-off by KIND, never by bot id.** A reviewer's `produces: kind:
  review` and a fixer's `consumes: kind: review_ledger` are what let Billy
  start from Revi's findings and answer them back, with neither manifest
  naming the other bot — the generic mechanism documented in CLAUDE.md's
  "The ENGINE stays bot-agnostic" section and exercised end to end in
  [revi-billy-loop.md](revi-billy-loop.md#what-the-command-seeds).
  Adding a second reviewer or a second fixer is a bundle, never an engine
  PR.

- **The pause notice names the parked run's role.** A run that parks on a
  provider quota gets [a comment naming when it resumes](#a-paused-review-says-so-on-the-pr),
  worded for the role the run's own manifest declares through its
  `consumes:`/`produces:` kinds — the reviewer's ("the verdict lands here …
  a new push restarts it sooner"), the fixer's ("don't push to this branch
  meanwhile — the run re-clones the head when it resumes"), and a neutral
  one for any other role — never a bot-id branch in the engine
  ([SocialGouv/iterion#683](https://github.com/SocialGouv/iterion/pull/683); before it, a
  parked Billy read as a parked Revi, observed live on PR #646).

**What is NOT wired (yet):**

- **No "fixer in flight" signal exists BEFORE its first push.** From the
  moment `/billy` (or the zero-touch lane) launches to its first commit,
  `revi/review` stays green on the OLD head and nothing on the PR says a
  fixer is working — the only signals are the run console itself and,
  once it parks, the pause notice above. This is the phase the operator
  rules below are written for; see
  [revi-billy-loop.md's "What to expect on the PR"](revi-billy-loop.md#what-to-expect-on-the-pr)
  for the exact wording and (SocialGouv/iterion#664) for the tracking card.

**Operator rules, one line each:**

1. **Don't push to a PR while its fixer runs** — his commits land on that
   branch; a manual push mid-run recreates the exact collision the "no
   in-flight signal" gap above cannot warn you about. `git pull` after his
   push before resuming any local work on the branch.
2. **`/billy` is the escalation from a review, not a replacement for one.**
   Comment it once Revi has left findings — never hand-fix them in a
   session on this repo (the dogfood habit in
   [revi-billy-loop.md](revi-billy-loop.md)).
3. **The zero-touch lane (`auto_fix_on_gate_failure`) makes step 2
   automatic** on repos that opt in — a red `revi/review` launches the
   fixer with no comment, bounded by [its own brakes](#autofix). Check
   `iterion remote runs list` (or the gate's `pending` link) before
   hand-fixing a red PR: a manual fix racing an already-launched fixer is
   the same collision as rule 1.

## Overriding a finding

Three ways, in order of preference:

1. **Push a fix** — the status re-reviews on the new head (needs
   `review_on_sync`) and flips green when the finding is gone.
2. **`/revi approve [reason]`** — a **maintainer** comments this on the PR to
   force-green the `revi/review` status on the current head, for a finding
   they dispute. Authorized through the **same PR-comment command gate as
   every other `/command`**: the commenter must hold a live repo role of at
   least **maintainer**, verified via the forge permission API. The webhook's
   `min_replier_role` pin may RAISE that floor (pin `owner` and only owners
   may approve) and never lowers it: that pin is the talk-back floor — who
   may question a bot — and an operator who lowers it so reporters can ask
   the converse bot must not lower the merge-queue bypass with it. Role
   only, for the same reason: the webhook's `AuthorizedRepliers` allowlist
   (who may talk back to the bot) does not apply to a force-green. On
   Forgejo/Gitea, whose collaborator vocabulary is `owner | admin | write |
   read` (there is no `maintain`), the `maintainer` floor resolves to **owner
   or admin** — deliberately narrower than on GitHub, never wider. Two additional guards close self-approve
   loops: the review bot's own comment is rejected (WhoAmI loop-guard), and
   the PR author cannot approve their own PR (a maintainer must). The status
   carries "approved by @user: reason" and links to the comment as the audit
   trail. It does **not** launch a re-review. Works on GitHub App
   integrations (posts through the connection's installation token so the
   `statuses` scope is present) and hand-owned webhooks with a `forge_token`
   binding. The token client serves when no connection covers the repo,
   and when the covering connection cannot serve the write — for exactly
   two reasons: its installation-token mint fails (a grant that lags the
   requested permissions, a rotated App key), or the installation
   withholds `statuses:write` (one created before the merge gate, or one
   that declined the permission: the App client re-mints without it, so
   its reads still work and only the status write would 403). In both
   cases the lane warns and reads, writes and replies through the
   binding; with no binding, the refusal names the withheld grant to
   approve on the App. **Who is told what:** the role gate runs before
   anything the lane says on the PR. A commenter it refuses — below the
   floor, or the bot's own comment — gets **no reply**; the webhook's
   delivery audit records the refusal. `/revi approve` is intercepted
   before any scope or route admission, so that branch is reachable by
   anyone who can comment, and a bot comment there would be one any
   drive-by could drive, N times for N comments. Past the gate the
   commenter is a maintainer, and every configuration refusal or forge
   failure (bot not enabled, no connection and no binding, withheld
   grant, no gate context, no head sha, a rejected status write) is told
   on the PR as **what to fix**; connection ids and the forge's own error
   text stay in the server log and on the audit row, never in the
   comment. *(GitHub + Forgejo today;
   GitLab `/revi approve` on a note is a follow-on.)*
3. **Admin merge-queue bypass** — the last resort, always available to repo
   admins.

## <a name="questions"></a>Questions vs findings

Revi separates two channels:

- **findings** — issues it would block a merge on. These feed the board, the
  inline comments, and the gate count.
- **questions** — non-blocking, load-bearing assumptions the diff now relies
  on that the reviewers could not verify end-to-end ("EditorView no longer
  opens the file — does EditorTabHost guarantee it does?"). They make a
  0-finding review **falsifiable** (they show what was checked and where the
  residual risk hides) and are surfaced in the report + the PR review summary
  body. They **never** become findings, reach the board, or gate a merge.

## <a name="inflight"></a>The check says "running" while the review runs

A review takes minutes. For all of them the gate context used to carry **no
status at all**, and a forge renders that as *"Expected — waiting for status to
be reported"* — the same rendering as a review that was never launched, a bot
that crashed on boot, and a webhook that never fired. Read next to Revi's
review comment on the *previous* commit, it looks exactly like "the bot
commented but the gate never went green".

So the launch **claims** the context: the moment a run is admitted on a
revision, the server posts `pending` on that head, described as *review in
progress — the verdict will replace this*, pointed at the live run console. The
absence of a status once again means what it says.

Two rules keep the claim from doing harm:

- **It never overwrites a verdict.** A repo pins one gate context precisely so
  a required check can span several bots ([below](#one-gate)), so a second bot
  launching on a head another bot already judged must not blank that judgment
  back to "running". It writes only over nothing, over a previous claim, or
  over a synthetic interruption (a fresh review on that head IS the recovery).
  A provider iterion cannot read statuses back from is left alone.
- **Every consumer knows the marker, and whose it is.** A guard written as
  "this head already has a status, so someone answered" would read the claim as
  a verdict — which would make posting it *worse* than the absence, by
  silencing the repair below. But the mirror error is just as bad: treating
  *any* claim as unanswered lets a dead run paint "review died" over a review
  that is running right now (the recovery run this repair itself launched, or a
  second bot sharing the repo's one context). So every status iterion writes
  names the run it speaks for, in its target URL, and ownership decides:
  **its own claim** is unanswered and gets repaired; **another run's** means
  somebody is working — stand down.

  A corollary: with no `PublicURL` configured a status cannot name its run, so
  the launch does not claim at all. The check then behaves exactly as it did
  before this feature — an ambiguous claim would be worse than none.

The claim is not a substitute for the repair: a run that dies still holding it
leaves a `pending` nothing will resolve, which blocks a required check exactly
like an absent one. It makes the window legible; the next section is what
closes it.

## <a name="interrupted"></a>A review that dies still leaves a verdict

A required check that is **absent** — or stuck on the in-flight claim above —
is indistinguishable from one still running: the pull request waits for a
context that will never arrive, and no error appears on the run, the PR or the
check. That is worse than a red check, because nothing points at the cause.

It happened twice in one day in production. A rolling deploy drained a review
mid-flight (at the time the lame-duck drain was not deployed, so a rollout
cancelled in-flight runs; the chart has rendered `config.runner.drainMode`
since 3.78.0 — see docs/probes-and-graceful-shutdown.md). Separately, a bot
bug made the publish step skip on every run, so
`revi/review` stopped landing repo-wide and every pull request became
unmergeable — with every other check green.

So the server reconciles. When a run that held a publish grant reaches a
terminal state without a verdict on the PR head, it posts a `failure` carrying
the reason and the way out, pointed at the run that owed it. Three rules keep
that from doing harm of its own:

- **It reads before it writes.** The forge is the authority on whether the
  verdict landed — not any bookkeeping of ours, which a second replica would
  not share and a restart would lose. A provider iterion cannot read statuses
  back from is left alone: overwriting a real success with a synthetic failure
  is worse than the problem being fixed.
- **It acts only where the operator pinned the gate context.** Holding a
  publish grant is not owing a verdict: the server mints one for ANY bot
  launched with a `pr_url` — the brancher, the docs amender, the implementer —
  and a repo's gate context is deliberately SHARED between the bots that gate
  it. The anchor is therefore `launch_vars.gate_context` on the integration,
  which is already what a repo must set to make one required check span several
  bots. **A repo that does not pin it gets no repair.** (Inferring it from
  contexts the server had posted before was tried and dropped: that memory is
  empty in exactly the two situations this exists for — a bot whose publish
  step never succeeds, and a rollout that restarts every replica.)
- **It only speaks for the revision that run reviewed.** The head moves while a
  run is alive — the author pushes a fix, a brancher commits, `review_on_sync`
  starts a fresh review. A newer head is a newer review's responsibility, so
  the run must name the revision it read (`head_sha`, stamped at launch) and
  the reconciler abstains when it does not.
- **It leaves paused runs and ARMED retries alone — and nothing else.** A
  `failed_resumable` run whose usage-window retry is armed (persisted before
  the outcome event fires) will resume and post its own verdict. One with no
  retry armed — budget exceeded, retries exhausted, a plain execution failure —
  has nothing coming back for it and IS reconciled: skipping those left a
  Vetty-gated PR silently unmergeable for hours in production (2026-08-03,
  a 15-module go bump whose audit died on the run's own duration budget).
  When the retry sweeper permanently abandons a retry, it republishes the run
  outcome so this rule fires then too.
- **It stays inside the grant's scope.** `pr_url` is a launch var, and the
  server honours a caller-pinned publish token, so the repo and forge host are
  re-checked against the grant exactly as the publish endpoint checks them. A
  red status is not a merge, but posting one on any repo a team connection
  reaches is precisely the blast radius the grant exists to bound.
- **`failure`, not `success`.** A review that did not happen has approved
  nothing.

A paused run is not reconciled: it is expected to resume and post its own
verdict.

### Two triggers, because one event is not a guarantee

The repair is driven by a run-outcome event on the internal bus — and that bus
is **lossy by design**. Every other consumer carries a reconciliation net for
exactly that reason (usernotify's 2-minute sweep, the dispatcher's 30s poll
behind its board fast path, the retry sweeper because no in-pod timer survives
a rollout). The merge gate — the one consumer whose miss *blocks a pull
request* — had none: a dropped event left the check absent forever, with the
run reading `failed_resumable` and nothing anywhere saying a PR was waiting.

Observed 2026-08-10: four review runs died on one provider weekly cap inside 90
seconds, all four gates stayed absent for hours, and the reconciler had left no
trace of having considered any of them.

So a **sweep** offers the same runs to the same repair a second time: every
minute, terminal runs in a bounded window (a 3-minute grace so the two paths
race only on the dropped ones, a 60-minute lookback so it never reaches back
and paints a failure onto a long-merged PR). The repair re-reads the live
status before writing, so the redundant offer costs one API read.

Telling "already answered" from "must escalate" is what makes the second offer
safe, and the answer turns on WHOSE marker is on the head. **Its own** — the
event and the sweep racing, or the sweep re-reading its 60-minute window every
minute for an hour — is already answered: the repair returns immediately,
writing no status and walking no tail. **Another run's** is a different
question, and only that case falls through: a **second** death on one head (a
relaunched run dying too) must still walk the relaunch/escalation tail instead
of mistaking the first death's marker for an answer. Even then the STATUS
WRITE stands down: one marker per head is enough, and re-posting from a run
the marker does not speak for is a storm —
two dead runs re-pointing the target URL at themselves on every sweep tick
produced **116 status writes on one head in 15 minutes**
(buildkit-operator#21, 2026-08-17). The status's target URL names the run it
speaks for, which separates "mine" from "another's" with no bookkeeping a
second replica would not share.

The sweep is **not elected** — the repair is idempotent by re-reading the live
status, so a leader would buy nothing. One consequence needs care: the
relaunch's once-per-head bound is a read-then-insert claim, so two replicas
offering one dead run give a launch and a *duplicate*. A duplicate alone is
therefore not evidence the replacement died; the board card that tells a human
"automation is out of moves" is filed only once the named run has itself
stopped.

Finally, when a repair genuinely declines to act, **it says so**. Every branch
past "this run held a publish grant and died" now logs the reason it is posting
nothing (`forge gate: run … held a grant on … but posts nothing: …`). Those
branches used to return silently, which is why the four blocked PRs above were
indistinguishable from four runs that gated nothing.

### The verdict survives a long quota wait

A run parked on a provider usage window is resumed by the retry sweeper up to
`retrypolicy.DefaultMaxWait` later — a **weekly** forfait cap resets as much as
seven days out. The per-run publish grant has to outlive that: at a flat 24h it
expired long before the resumed run reached its publish node, so the review
completed and then had no way to post the verdict it had computed. The grant's
TTL is therefore derived from the max retry wait, plus a margin for the resumed
run itself.

### The dead review is re-run — once per head

The synthetic `failure` makes the interruption visible; on an automated lane
(a Dependabot PR guarded by Vetty) nobody is watching to act on it. So after
posting it, the server relaunches the SAME bot on the same pull request —
crash-recovery of a launch the webhook already admitted, re-run through the
same tail (idempotency claim, quota metering, fresh publish grant, hold-label
veto, `overlap: supersede`). The bound is the idempotency key itself: **one
relaunch per (PR, head sha), ever.** The fresh run posts the real verdict over
the synthetic failure when it completes.

A relaunch that **cannot start** — the launch itself fails (a queue outage,
a deploy window, the 2026-08-26 plugin-source parse error) rather than the
relaunched run dying — does not spend the claim, and the sweep keeps offering
the dead run every minute for an hour. Those offers are the retry, and the
retry is bounded: the launch tail counts the attempts on the claim row
(`attempts` / `failed_at` on the delivery), the lane retries on a backoff
(5 min, then 10 min), and the **third** failure escalates exactly like a second
death and then stops. An admission denial (org quota, cost cap, concurrency)
still escalates on the first refusal — its horizon is not one the sweep window
outlasts. Human-driven redeliveries carry no such budget: an operator retrying
after a fix must be able to.

When the one relaunch is already spent and the gate dies AGAIN on the same
head — or the relaunch cannot start within that budget — the problem graduates
to the team's board: a card labelled `source:gate-reconcile` naming BOTH dead runs
(the relaunch stamps a `gate_relaunch_of` launch var so its own death can name
the original), the failure reasons, and the remedy. The same escalation is
ALSO posted as a **PR comment** through the connection's review client: the
board is the operator's queue, but the PR is where the people waiting on the
merge look — a card alone sat unseen in a team inbox for 7 days while a
security PR stayed blocked. The comment rides the card's insert, so the two
travel together in both directions: a deployment with **no board** (no
`CloudBoardFor`, or a team without one) gets neither, and the synthetic
`failure` status is then the only surface carrying the interruption — worth
knowing before pointing a board-less deployment at a required check. Card and
comment are bounded to once per (PR, head) by a **deterministic card id**
(UUIDv5 of team/repo/PR/head): the
sweep runs unelected on every replica, and two replicas racing past a
List-based dedup would each file the card AND each post the comment — the
store's unique-id insert is what serialises them. A required check dying
repeatedly on one revision is a structural signal (a run budget too short for
the workload, a recurring provider quota, a bot defect), which is a human's
call.

The auto-fix lane ([above](#autofix)) deliberately ignores these synthetic
failures: `review died` means there are no findings to fix, so the recovery is
re-running the REVIEWER (this lane), never launching the fixer.
