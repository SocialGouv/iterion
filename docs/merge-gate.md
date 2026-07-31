# Merge gate — Revi arbitrates, determinism disposes

Revi (`bots/review-pr`) can post a **deterministic merge-gate status** on a
PR so an unresolved blocking finding keeps the PR out of the merge queue —
without ever letting an LLM be the yes/no arbiter of a merge.

The split is deliberate:

- **Revi (LLM) proposes** — two independent reviewers (Claude + GPT) find
  issues, merge + de-duplicate them, and post them as inline PR comments.
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
PR opened / pushed ──▶ Revi runs (2 reviewers → merge → publish)
                              │
                              ├─ inline comments  (advisory)
                              └─ revi/review status on the head SHA
                                    success  ⟺  0 findings ≥ gate_severity
                                    failure  ⟺  ≥1 finding  ≥ gate_severity
```

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
| `block_fork_prs` | `false` | Filter fork PRs from any auto-launch. **Recommended ON whenever `review_on_sync` is enabled on a public repo** (see caution). |

> **Caution — budget with `review_on_sync`.** The sync lane re-runs the two
> LLM reviewers on **every push** (each new head SHA), gated only by the
> webhook's `AuthorAllowlist` (empty = any author) and per-head idempotency —
> there is no author-trust gate on this lane. On a public repo a fork
> contributor pushing repeatedly can drive repeated full reviews, bounded only
> by the org launch gate + webhook rate limit. Enable **`block_fork_prs`**
> (and/or set an `AuthorAllowlist`/`MinAuthorRole`) alongside `review_on_sync`
> so untrusted fork PRs don't auto-re-review.

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

### Zero-touch: letting a red gate launch the fixer itself (opt-in)

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

## Overriding a finding

Three ways, in order of preference:

1. **Push a fix** — the status re-reviews on the new head (needs
   `review_on_sync`) and flips green when the finding is gone.
2. **`/revi approve [reason]`** — a **maintainer** comments this on the PR to
   force-green the `revi/review` status on the current head, for a finding
   they dispute. Authorized through the **same PR-comment command gate as
   every other `/command`**: the commenter must hold a live repo role at or
   above `MinReplierRole` (or be in `AuthorizedRepliers`), verified via the
   forge permission API, and the review bot's own comment can't self-approve
   (WhoAmI loop-guard) — an arbitrary contributor cannot wave a finding
   through. The status carries "approved by @user: reason" and links to the
   comment as the audit trail. It does **not** launch a re-review. *(GitHub +
   Forgejo today; GitLab `/revi approve` on a note is a follow-on.)*
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

## <a name="interrupted"></a>A review that dies still leaves a verdict

A required check that is **absent** is indistinguishable from one still
running: the pull request waits for a context that will never arrive, and no
error appears on the run, the PR or the check. That is worse than a red check,
because nothing points at the cause.

It happened twice in one day in production. A rolling deploy drained a review
mid-flight (the lame-duck drain is not deployed, so a rollout cancels in-flight
runs). Separately, a bot bug made the publish step skip on every run, so
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
- **It leaves resumable and paused runs alone.** The cloud runner republishes
  a run outcome on every delivery attempt, before deciding to retry, so a
  transient rate limit is not a dead run.
- **It stays inside the grant's scope.** `pr_url` is a launch var, and the
  server honours a caller-pinned publish token, so the repo and forge host are
  re-checked against the grant exactly as the publish endpoint checks them. A
  red status is not a merge, but posting one on any repo a team connection
  reaches is precisely the blast radius the grant exists to bound.
- **`failure`, not `success`.** A review that did not happen has approved
  nothing.

A paused run is not reconciled: it is expected to resume and post its own
verdict.
