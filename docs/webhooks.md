# Inbound webhooks

**Audience.** Org admins wiring a forge or a custom caller to iterion,
and operators reviewing the auth + rate-limit + audit story before
opening `/api/webhooks/*` to public traffic.

This document covers **inbound** webhooks — external events arriving on
`/api/webhooks/<provider>/<id>` to launch a bot. The mirror feature
("call me back when this run finishes") is documented in
[outbound-callbacks.md](outbound-callbacks.md).

> **Prefer the Integrations tab for forge repos.** The manual lifecycle
> below (mint a token, paste the URL + token into the forge) is the
> low-level path. For a GitLab/GitHub/Forgejo repo, the studio's
> **Integrations** tab connects the forge once (OAuth or PAT) and
> **provisions the hook + token + this webhook config for you** when you
> enable a bot — see [forge-integrations.md](forge-integrations.md). Such
> configs carry a `provisioned_by` marker, render read-only here, and
> reject direct delete/rotate (409) — manage them from Integrations. The
> manual path remains for the Generic JSON trigger and for webhooks you
> want to own by hand.

Four providers are wired: GitLab (incl. `/revi` re-review command),
GitHub, Forgejo/Gitea, and a bot-agnostic Generic JSON endpoint
([pkg/server/webhooks_routes.go:supportedProviders](../pkg/server/webhooks_routes.go)).

## Lifecycle

1. An org admin creates a webhook through the studio (`Webhooks` tab on
   `/teams/<id>`) or the API. Iterion mints an `iwh_…` token (32 bytes
   of randomness behind a recognisable prefix) and returns it **exactly
   once** alongside the new `Config` document
   ([pkg/webhooks/token.go:MintToken](../pkg/webhooks/token.go)).
2. The admin pastes the inbound URL + token into the forge:
   - GitLab → Settings → Webhooks → URL `https://…/api/webhooks/gitlab/<id>` + Secret Token `iwh_…`
   - GitHub → Settings → Webhooks → Payload URL `https://…/api/webhooks/github/<id>` + Secret `iwh_…`
   - Forgejo/Gitea → Settings → Webhooks → Target URL + Secret `iwh_…`
   - Generic → any HTTP client, header `X-Iterion-Webhook-Token: iwh_…`
3. From then on, each delivery is admitted through the middleware,
   parsed by the provider, dispatched to a bot, and recorded as a
   `Delivery` row. The token plaintext is **not** kept at rest — only a
   salted hash, the last 4 chars, and a SHA-256 fingerprint
   ([pkg/webhooks/types.go:Config](../pkg/webhooks/types.go)).

Rotate or revoke at any time: `POST /api/teams/{id}/webhooks/{webhook_id}/rotate`
returns a fresh plaintext (also shown once) and updates the forge's
"secret" field is then a manual step.

## Auth modes — token vs HMAC

Iterion's middleware has two authentication modes, picked per provider
to match how the forge actually signs the request
([pkg/webhooks/types.go:SignatureMode](../pkg/webhooks/types.go),
[pkg/server/webhooks_routes.go:defaultSignMode](../pkg/server/webhooks_routes.go)).

| Provider | Default `sign_mode` | What proves authenticity | Header iterion reads |
|---|---|---|---|
| GitLab | `token` | The forge echoes the `iwh_` plaintext verbatim | `X-Gitlab-Token` (or `X-Iterion-Webhook-Token`) |
| GitHub | `hmac` (forced) | HMAC-SHA256 of the raw body, key = `iwh_` plaintext | `X-Hub-Signature-256` |
| Forgejo/Gitea | `hmac` (forced) | HMAC-SHA256 of the raw body | `X-Forgejo-Signature` (falls back to `X-Gitea-Signature`) |
| Generic | `token` (default; `hmac` opt-in) | Header bearer token, or HMAC of body | `X-Iterion-Webhook-Token` / `X-Iterion-Webhook-Signature` |

The same `iwh_…` plaintext that's shown at create time is used in both
modes — operators paste it once into the forge's "secret" field. For
HMAC providers, iterion seals that plaintext at rest under an AAD bound
to the webhook ID
([pkg/webhooks/token.go:SealHMACSecret](../pkg/webhooks/token.go)) so
the same value can be reused on every delivery to recompute the
signature without storing cleartext. Rotating the token reseals it.

**Why per-provider, not per-org?** GitHub and Forgejo's hooks **only**
sign the body — they don't echo any token header at all, so an operator
who picks token-mode for them would lock themselves out. GitLab's
"Secret Token" field is exactly the bearer model. The middleware skips
the header check entirely under `sign_mode: hmac` so the body bytes
stay intact for the provider handler's signature recomputation
([pkg/server/middleware_webhook.go:webhookAuth](../pkg/server/middleware_webhook.go)).

## Per-provider behaviour

### GitLab (`POST /api/webhooks/gitlab/{id}`)

Single URL, two event kinds dispatched on `X-Gitlab-Event`
([pkg/server/webhooks_gitlab.go](../pkg/server/webhooks_gitlab.go)):

- **`Merge Request Hook`** — auto-review on `open`/`reopen`, and on the
  **draft→ready transition**. A **draft MR never auto-launches** (the author
  is still iterating — auto-running a bot wastes budget and churns an
  unfinished branch). GitLab has no dedicated ready action, so the trigger is
  the `update` whose `changes.draft` (or the deprecated `work_in_progress`)
  went `true→false`. Ordinary pushes (action `update` with an unchanged draft
  flag) deliberately do **not** re-trigger — auto-review on every push was
  found too noisy; cf.
  [pkg/webhooks/gitlab/parser.go:IsReviewable](../pkg/webhooks/gitlab/parser.go).
  An `update` whose `changes.reviewers` **(re-)requests a review from
  iterion's own bot account** is the exception — the re-request-review
  button; see <a href="#re-request-review">below</a>.
- **`Note Hook`** — the generic slash-command and conversation surface.
  A note's first non-whitespace token is the command; quoting "please run
  /revi" mid-text never triggers (anti-oscillation guard;
  [pkg/webhooks/gitlab/note.go:IsReviewCommand](../pkg/webhooks/gitlab/note.go)).
  Four routes, in the order the handler tries them
  ([pkg/server/webhooks_gitlab.go:259-345](../pkg/server/webhooks_gitlab.go)):
  1. a `/command` on an **open issue** → the generic command handler
     with `surface="issue"`, so the bot opens an MR back-linking the
     issue. A non-command issue note is filtered.
  2. any command **other than** `/revi` on an open MR → resolved through
     the command registry to a bot + execution mode; an unknown command
     is filtered.
  3. `/revi` on an open MR → on-demand re-review. `/revi <question>`
     routes to the conversation bot instead.
  4. a plain reply **in a thread Revi is part of**, with no command at
     all, when `revi-converse` is enabled — so "just replying" to Revi
     works.
- **`Issue Hook`** — adding a trigger label (e.g. `implement`) launches the
  webhook's bot, same as GitHub `issues` (below). GitLab has no `labeled`
  action, so the parser diffs `changes.labels` (previous→current) and fires
  only on a *freshly-added* label that passes `label_allowlist`, on an OPEN
  issue ([pkg/webhooks/gitlab/issue.go](../pkg/webhooks/gitlab/issue.go)).

Default event allowlist: `{merge_request, note}` — both kinds reach a
zero-config webhook
([pkg/webhooks/match.go:MatchEvent](../pkg/webhooks/match.go)).
Operators who want only the auto-review path list `["merge_request"]`
explicitly; that disables `/revi` while keeping open/reopen.

Vars stamped on the run: `pr_url`, `base_ref`, `scope_notes`,
`post_to_board=false`, `pr_review_mode=inline`, plus `re_review=true`
for the note path. The webhook's `LaunchVars` override these.

### GitHub (`POST /api/webhooks/github/{id}`)

HMAC over the body, header `X-Hub-Signature-256` (`sha256=<hex>`). Three
event paths trigger; ping / push / everything else is silently filtered
(returns 200 — a 4xx makes GitHub disable the webhook after repeated
failures; [pkg/server/webhooks_github.go](../pkg/server/webhooks_github.go)):

- **`pull_request`** with action **`closed`** (merged or not) → every run
  still bound to that PR is **stopped**, and any armed usage-window retry is
  **disarmed**. That includes the runs a **comment** launched (`/billy`, a
  review-thread reply): those record the comment's own id as their subject,
  so the delivery carries a `parent_subject_id` pointing at the pull
  request, and the stop matches on either. Scoped to the PR across every
  bot, unlike
  `overlap: supersede` which replaces one bot's work with newer work of the
  same bot. Without it a review in flight keeps spending provider quota on a
  diff nobody will merge, and a review PARKED on a quota window wakes hours
  later to comment on a dead pull request. Nothing is ever launched on this
  action.
- **`pull_request`** with action `opened`, `reopened`, or `ready_for_review`
  — **plus `synchronize` when the webhook sets `review_on_sync`**, so a push
  to the head re-reviews and the `revi/review` status re-evaluates on the new
  head SHA (this is what makes a required check track the fixed revision; see
  [merge-gate.md](merge-gate.md)). The synchronize lane is **debounced**: the
  launch waits out a quiet window (`ITERION_WEBHOOK_SYNC_DEBOUNCE`, default
  `3m`, `0` disables) and a newer push on the same PR replaces the parked
  launch and re-arms the window, so a volley of pushes costs ONE review of
  the final head instead of N−1 runs cancelled mid-flight
  ([pkg/server/webhooks_debounce.go](../pkg/server/webhooks_debounce.go); the
  delivery answers `202 {"status":"deferred"}`, a 20s sweep launches due
  entries, multi-replica-safe via a store lease, and the launch tail's
  idempotency key keeps a lease replay from double-launching). PR open,
  `/revi` and a re-request click stay immediate — a human is waiting on
  those. During the window the required check is simply absent, the same
  honest "nothing is reviewing this yet" as the seconds between push and
  launch; the in-flight claim still lands at the real launch. What gets
  parked is the **newest** head, not the last-arrived one: forges do not
  guarantee delivery order, so the payloads are ordered by the forge's
  own event timestamp and a delivery that lost the race answers
  `200 {"status":"filtered"}` rather than overwriting a newer parked
  push. Two further properties are worth knowing when a parked review
  does not appear: the **config at fire time governs** — disabling the
  webhook, clearing `review_on_sync`, or removing the bot from
  `bot_ids` during the window drops the parked launch (with a
  `filtered` delivery naming why), because the sweep re-enters none of
  the admission the inbound request passed; and a parked launch the
  admission gate refuses (org concurrency, launch rate) or that fails
  outright is **re-armed with backoff**, not dropped — the forge was
  answered `202 deferred` and will never redeliver, so the retry has to
  live here. That chain is bounded (8 attempts, ~45 min); past it, or
  on a monthly quota/cost denial that resets weeks away, the review is
  abandoned with a `launch_error` delivery naming the loss rather than
  disappearing.
  → PR auto-**review** (Revi / `review-pr`). This lane is **review-only**: a
  PR-open NEVER auto-launches the mutating branch-improve loop (Billy) — see
  *PR auto-lane: review, not mutate* below. A **draft PR never auto-launches**
  (the `draft` flag is honoured on every action — the trigger is
  `ready_for_review`, which clears it). A **fork PR** (head branch in a
  different repo) is likewise never launched — not by this lane, and not by a
  `/command` either, read-only `/revi` included: every lane hands the runner
  the base repo's clone URL paired with a head-repo ref, a pair that names one
  repository only when head and base are the same repo. The guard is
  **fail-closed**: a payload that omits `head.repo` is refused too, a head
  repo being missing only once it was deleted or blocked — which only a FORK
  head can be
  ([pkg/webhooks/prforge/parser.go:IsReviewable](../pkg/webhooks/prforge/parser.go) +
  `HeadIsSameRepo`). It is both the untrusted-code and the anti
  budget-exhaustion boundary; running a bot on fork code deliberately would
  need a head-repo checkout iterion does not build yet. A PR opened by
  iterion's **own forge bot** (another iterion bot's PR — see below) is also
  skipped.
- **`issue_comment`** → the universal `/command` slash path (e.g.
  `/featurly <prompt>`, `/billy`), routed through the command registry —
  including the `/revi <question>` ⇄ bare `/revi` split, resolved by the
  manifests' complementary `when_args_empty`/`when_args_present`
  disambiguators (question → the converse bot, bare → re-review).
- **`pull_request_review_comment`** with action `created` → the
  conversational reply lane: replying inside one of the bot's review
  threads launches the converse bot, which answers **in the same
  thread**. Loop-guarded (the bot's own answer echoes back as this
  event), thread-classified (a human↔human thread never triggers), and
  replier-gated like every comment lane. Requires the converse bot in
  `bot_ids` and the event in `event_allowlist` (a re-provision
  regenerates both from the converse bot's own manifest event
  `pull_request_review_comment` — deliberately separate from
  `pull_request_comment`, so repos without the conversational bot
  never subscribe the per-inline-comment delivery firehose). GitHub
  only for now. See
  [forge-conversations.md](forge-conversations.md).
- **`issues`** with action `labeled` → launches the webhook's bot with
  the labeled issue turned into a feature task. The handler derives
  `feature_prompt` (issue title + body), `open_mr=true`, and
  `source_issue_ref` (the issue URL), so an implementer bot (featurly)
  implements the issue, opens a PR, and comments the PR URL back onto the
  issue. Scope which label fires with **`label_allowlist`** (below);
  re-applying the same label is an idempotent replay.
- **`issues`** with action `opened` + **`auto_implement_on_open`** → the
  zero-touch lane, now **author-gated**: the issue AUTHOR must be
  trusted — on the static `author_allowlist`, OR
  `author_association` ∈ OWNER/MEMBER/COLLABORATOR (decoded from the
  payload, no API call), OR live `CollaboratorPermission` ≥
  **`min_author_role`** (gitlab vocabulary, `""` → developer ≡ write;
  read through the same client the command gate resolves — see *Which
  credential a lane reads through* below). Unknown = untrusted (**fail-closed** —
  this is the budget boundary against drive-by issues, unlike the
  fail-open org quotas). An untrusted author's delivery filters (200,
  visible reason) and the issue's board card parks with
  `needs:approval` for the operator's "Approve & triage". The `labeled`
  lane is NOT author-gated: applying the trigger label already requires
  triage+ rights on the forge — labeling IS the approval gesture.

The label path (GitHub `issues` and GitLab `Issue Hook`) routes through
the same dispatcher sink as the `/command` path, so when a tenant cloud
board is wired it also **materialises a one-way tracking card** for the
issue — a read-only mirror linked back to the source issue via
`source_issue_ref` (idempotent per issue). GitHub/GitLab stay the source
of truth; iterion only writes back to them (PR + back-link comment). With
a board *coordinator* running, the card is the unit of work the dispatcher
executes; without one, the card is a tracking record and the run launches
directly ([pkg/server/invocation_dispatch.go:dispatchInvocation](../pkg/server/invocation_dispatch.go)).

### Forgejo / Gitea (`POST /api/webhooks/forgejo/{id}`)

Same wire shape as GitHub-style PRs, two header spellings accepted:
`X-Forgejo-Signature` (current) or `X-Gitea-Signature` (older Gitea
deployments); same for `X-Forgejo-Event` / `X-Gitea-Event`. The
signature header is treated as a hex digest with or without the
`sha256=` prefix
([pkg/server/webhooks_forgejo.go:forgejoSignatureHeader](../pkg/server/webhooks_forgejo.go)).

The same draft/fork guards as GitHub apply (shared `prforge` parser):
a draft PR (`pull_request.draft == true`) never auto-launches, and a fork
PR launches nothing at all. **Caveat:** Forgejo/Gitea has no `ready_for_review`
webhook action (marking a WIP PR ready arrives as `edited`, which does not
auto-trigger), so on Forgejo the draft→ready re-trigger is on-demand — a
collaborator reopens the PR or uses the `/command` path (which re-triggers a
draft, but still not a fork). The no-draft and no-fork guarantees hold
regardless.

### PR auto-lane: review, not mutate (Revi vs Billy)

The PR/MR **open** lane (GitHub `pull_request`, GitLab `merge_request`,
Forgejo PR) is **review-only**. Opening a PR auto-launches the read-only
reviewer **Revi** (`review-pr`) and nothing else — it never runs the
mutating branch-improve loop **Billy** (`branch-improve-loop`). Two
carve-outs sit around that rule
([pkg/server/webhooks_github.go:handlePRForgeReview](../pkg/server/webhooks_github.go),
[pkg/server/webhooks_common.go:isIterionForgeBotAuthor](../pkg/server/webhooks_common.go)):

- **Iterion-bot PRs are skipped.** A PR opened by iterion's OWN forge bot
  (another iterion bot — Doki, Willy, Featurly… — pushing through the
  tenant's forge integration) is **not** auto-reviewed: it already
  converged inside its own loop, so re-reviewing it just wastes budget and
  adds noise. The author is matched against the tenant's provisioned forge
  connection — a GitHub/Forgejo App's `<app_slug>[bot]` login, or (GitLab)
  the connected bot account — **not** a generic `[bot]` suffix, so
  Dependabot / Renovate PRs stay reviewable. A human can still force a
  review with a manual `/revi`. Filtered as a clean 200 (visible reason).
- **Merge-queue auto-heal is preserved.** A PR *ejected from the GitHub
  merge queue* for a healable reason (`dequeued`, `NeedsAutoHeal`) still
  dispatches **Billy** to rebase, resolve the conflict / fix the combined
  break, and re-enter the queue — a narrow, distinct trigger
  (same-repo + project/author allowlist + bot-permitted, one attempt per
  head SHA), unrelated to the review lane.

**Billy on demand — `/billy` (alias `/improve`).** To run Billy on a PR,
a repo collaborator issues a **`/billy`** slash-command in a PR/MR comment.
The command reuses the SAME authorization gate as every other
`/command` / `/revi` (loop-guard + `AuthorizedRepliers` allowlist OR a
repo permission ≥ the route's `min_replier_role`), so a non-collaborator
cannot invoke it. Billy then commits its hardening onto the PR's own
branch (or opens a separate PR with `branch_improve_as_pr`). When Revi has
already reviewed that PR, the handler seeds Billy's run with Revi's most
recent findings under the **`prior_review`** var — so Billy starts from
that review instead of re-deriving it (best-effort: with no prior review,
Billy reviews the diff from scratch;
[pkg/server/webhooks_handoff.go](../pkg/server/webhooks_handoff.go)).

### <a name="re-request-review"></a>On-demand re-review: the "Re-request review" button

Alongside `/revi`, the forge-native **"Re-request review" button** is a
second on-demand re-review gesture — the one non-comment surface a product
developer already knows. Clicking it on iterion's bot reviewer (or adding
the bot to the reviewer set in the first place) relaunches the review bot on
the MR/PR's current head:

- **GitLab** — a `merge_request` `update` whose `changes.reviewers` carries
  `re_requested: true` on the bot's account (GitLab ≥ 18.5,
  [gitlab-org/gitlab!205274](https://gitlab.com/gitlab-org/gitlab/-/merge_requests/205274)),
  or simply shows the bot newly added (the only expressible form on older
  GitLab). To make the button exist, the server's publish step
  **self-assigns the bot as an MR reviewer** after each posted review
  ([forge.ReviewerAssigner](../pkg/forge/reviews.go) — read-modify-write,
  never dropping human reviewers; best-effort, a miss only costs the
  button). The assigner resolves **who it is at call time**, through a live
  `WhoAmI` on the connection's own token, and refuses to write when that
  answer is not a usable numeric user id — GitLab reads a `0` in
  `reviewer_ids` as "add nobody", which would report success while adding
  no one. On "no button on this repo", the server has already said why: the
  failure logs at **Warn** naming provider, repo, MR number and the
  underlying error (`resolve own account: …` or `own account id %q is not a
  usable GitLab user id`), while the review itself completes untouched.
- **GitHub / Forgejo** — a `pull_request` event with action
  `review_requested` whose `requested_reviewer` is iterion's identity.
  That identity is the **App bot login only** (`<app_slug>[bot]`): a
  PAT/OAuth connection's account may be a HUMAN's, and treating it as the
  bot would turn an ordinary human-to-human review request into an LLM
  launch (and disarm the anti-loop actor guard). A GitHub App cannot be a
  PR reviewer at all (forge restriction) and a Forgejo connection carries
  no App slug (there is no Forgejo App kind), so the derived identity
  leaves the lane inert on both.

  **`review_request_logins` is what lights it up.** The operator names the
  review identity explicitly on the webhook, and those logins join the same
  set both halves of the guard read — so the lane answers their request AND
  the actor guard recognises their own writes. On GitHub that identity has
  to be a **User account reached through a `pat` connection**: only a user
  can be a requested reviewer, and the review must be POSTED by that same
  account for the forge to clear the pending request and re-arm the button.
  Nothing is derived from the connection for this — see the config table.

Semantics, shared with `/revi` (deliberate manual gesture):

- **NOT exempt from the hold-label pause** — unlike `/revi`. The forge
  emits the same event for a CODEOWNERS auto-request, which needs no
  permission from the requester and carries no field distinguishing it
  from a click ([about-code-owners](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)),
  so the lane cannot claim a command's deliberateness — and the label's
  promise is that it freezes *every* automation on one PR;
- **repeatable once the head's review has FINISHED** — such a click is its
  own delivery (the idempotency key is then salted with the MR/PR
  `updated_at`), so re-requesting twice on the same head reviews twice. On
  GitHub a click landing while EVERY fanned-out bot's review of that head is
  still in flight collapses onto them instead (the CODEOWNERS auto-request
  dedupe) — unless the webhook's `overlap` is `supersede`, which takes
  precedence: the click salts, the stale run is cancelled and the fresh one
  replaces it ("newest request wins" is the operator's explicit choice). A
  click on a head no review has claimed yet takes the ordinary per-head
  key. GitLab keeps the unconditional salt (its lane only arms on an
  `update` action, so the open itself never double-fires; an auto-assign
  arriving as a follow-up update salts per click, bounded by the overlap
  policy); forge redeliveries of the same click stay deduped;
- **open PRs/MRs only** — reviewer edits arrive freely on closed/merged
  ones and never burn a run;
- **replier-gated like `/revi`** — the click is authorized through the
  same `authorized_repliers` allowlist / `min_replier_role` project-role
  gate (default developer) as every other manual trigger, on every
  provider. "The forge gates reviewer edits" is not enough: GitLab lets
  an MR AUTHOR edit their own MR's reviewers without holding a project
  role, and GitHub grants "request review" at the **Triage** role —
  below the write floor the command gate enforces — either of which
  would hand an under-privileged account a repeatable trigger. An
  unauthorized click is demoted — the delivery rides whatever automatic
  lane still admits it (with the hold label honoured) or is filtered;
- **never self-triggering** — a reviewers change whose *actor* is the bot
  itself (the self-assign echoing back) is filtered
  ([pkg/server/webhooks_common.go:isIterionBotReviewRequest](../pkg/server/webhooks_common.go)).

Pairs with the per-repo gate opt-out (`gate_enabled: "false"` pinned on the
integration's launch vars): first review automatic on open, every re-review
a button click or a `/revi` — see
[merge-gate.md](merge-gate.md#disabling-the-gate-per-repo--first-review-only-re-review-on-demand).

### Which credential a lane reads through

Every GitHub/Forgejo lane that asks the forge something — the `/command`
gate's `CollaboratorPermission` + `WhoAmI`, the PR-head resolution, the
review-request gate, the author-trust gate, and `/revi approve`'s status
write — resolves one client through
[pkg/server/webhooks_prforge.go:prforgeReplierAPIFor](../pkg/server/webhooks_prforge.go),
in descending order of how well each credential is PROVEN to speak for the
repo:

1. **the connection of the repo's own integration row** — what
   `Orchestrator.Provision` writes when the repo is connected. A GitHub App
   connection mints its installation token per call, so a connection-only
   integration authorizes with no `forge_token` binding at all;
2. **the webhook's `forge_token` binding** — the credential an operator bound
   to THIS webhook (the hand-owned setup);
3. **any team connection on the forge host** — the zero-config tier for an
   org-wide App install nobody provisioned repo by repo.

Tier 3 sits *below* the binding on purpose. It proves nothing about the repo,
and a credential that cannot see a repository does not fail loudly: GitHub
answers `404`, which iterion maps to permission `"none"` — a *successful*
answer. Preferred, it would silently rank every commenter at 0 and refuse
authorized `/command`s and approvals on any team that merely holds another
connection on the same host.

A connection whose client cannot serve is passed over with a `Warn` — an App
client mints lazily, so a mint that 422s (a grant lagging the requested
permissions) or an installation withholding `statuses:write` only shows up on
the first call, not at construction. When no tier serves, the refusal names
what was tried. The write lanes that hold no second credential (gate publish,
pending, reconcile) keep reading `forgeConnectionForPR`, which folds the same
three tiers into one lookup.

### Generic (`POST /api/webhooks/generic/{id}`)

Bot-agnostic: the caller picks which bot to launch by name (or relies
on the webhook's `default_bot_id` / single-bot scope). Request shape
([pkg/webhooks/generic/generic.go:Request](../pkg/webhooks/generic/generic.go)):

```json
{
  "bot": "review-pr",
  "vars": { "pr_url": "https://gitlab.local/group/repo/-/merge_requests/7" },
  "idempotency_key": "ci-build-42",
  "repo_url": "https://gitlab.local/group/repo.git",
  "repo_ref": "feature/x",
  "project_path": "group/repo"
}
```

Field bounds: `vars` is capped at **256 keys**, each key must match
`[A-Za-z_][A-Za-z0-9_]{0,63}`, each value at **4 KiB**. Anything else
is a 400 (`generic: too many vars` / `bad var key` / `var value too
large`).

Curl example:

```bash
curl -X POST https://iterion.example.com/api/webhooks/generic/<id> \
  -H "X-Iterion-Webhook-Token: $IWH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "bot": "review-pr",
    "vars": { "pr_url": "https://gitlab.local/group/repo/-/merge_requests/7" },
    "idempotency_key": "ci-build-42"
  }'
```

**Var precedence.** Body vars merge in first; then the webhook's
configured `LaunchVars` **override** them. The operator is the
security-critical knob — a malicious caller cannot escalate by renaming
a key the org-admin has pinned (`handleGenericWebhook` in
[pkg/server/webhooks_generic.go](../pkg/server/webhooks_generic.go)).

## Matching: project + event + author allowlists, bot scope

Every webhook carries four selection filters plus a bot-agnostic hold gate
([pkg/webhooks/types.go:Config](../pkg/webhooks/types.go)):

- **`event_allowlist`** — provider-event names allowed; empty defaults
  to the provider's natural set (GitLab uses `{merge_request, note}`,
  the others use `{pull_request}`, and the GitHub/Forgejo `issues`
  path defaults to `{issues}`). A bare `*` matches everything.
- **`label_allowlist`** — for the `issues` (labeled) path only: which
  freshly-applied label fires (e.g. `["implement"]`). Empty = any label;
  case-insensitive; a bare `*` matches everything. No effect on the
  `pull_request` / `issue_comment` paths. On a **provisioned** webhook, set
  it through the integration (`label_allowlist` on
  `POST/PATCH /api/teams/{id}/forge/repo-bots`) rather than the webhook
  config: provisioning rebuilds that config from the manifests, so a
  narrowing living only there is dropped by the next bot-set change — and
  that regression is fail-*open*. An allowlist already PATCHed onto the
  config is adopted onto the integration by the next provision.
- **`hold_labels`** — a **bot-agnostic suppression** set. When the
  triggering PR or issue carries any of these labels, the auto-launch
  lanes (PR-open review, merge-queue auto-heal, auto-implement-on-open)
  suppress the launch — *whatever* bot would have run — and record a
  filtered delivery. It is the operator's escape hatch to pause
  automation on one PR/issue without disabling the webhook; a human can
  still trigger a bot manually via a `/command`. Applies to all four
  auto-launch lanes (GitHub/Forgejo PR review + auto-heal, GitHub issue,
  GitLab MR review, GitLab issue). Case-insensitive; empty = off; a `*`
  entry holds everything. Fail-open: a minimal payload that doesn't carry
  the label set simply isn't suppressed. Unlike `label_allowlist` (which
  *selects* a bot), `hold_labels` *vetoes* them.
- **`project_allowlist`** — `owner/repo` patterns. Empty = every project
  the forge fires for. Supports `*` (any), `owner/*`, or exact paths.
- **`author_allowlist`** — PR/MR author logins allowed to trigger a
  launch. Empty = any author. Case-insensitive; entries may be bot
  logins like `dependabot[bot]` / `renovate[bot]`, so a webhook can
  react ONLY to a dependency bot's PRs and ignore human PRs on the same
  repo. A bare `*` opts back into allow-all. The matched author login is
  also stamped onto every review run as the `pr_author` var. (Applies to
  the PR/MR *open* path; comment/`/revi` triggers use `min_replier_role`
  / `authorized_repliers` instead.) When auto-provisioning, a bot sets
  this from its manifest `forge.webhook.author_allowlist`; co-enabling a
  bot that reviews all authors re-opens the shared webhook. With a
  per-bot routing table (next section) this flat filter is superseded by
  each bot's own `author_allowlist`, kept per rule.
- **`bot_ids` + `wildcard_bots`** — the only bots a delivery may
  launch. A wildcard (`["*"]` with `wildcard_bots=true`) must be
  declared **explicitly** so the studio + audit can flag it; the create
  endpoint logs a `webhooks: wildcard-bot webhook` warning at WARN level
  and the audit row carries `wildcard_bots: true`
  ([pkg/server/webhooks_routes.go:normalizeBotScope](../pkg/server/webhooks_routes.go)).

For zero-config forge webhooks (no explicit `default_bot_id`, no single
bot in scope) iterion auto-selects the **`review-pr`** bot — the same
default that ships with the Revi catalog
([pkg/server/webhooks_common.go:defaultWebhookBotReviewPR](../pkg/server/webhooks_common.go)).
The generic webhook deliberately does **not** apply this default — a
bot-agnostic endpoint must pick deterministically, so missing-bot is a
400.

### Per-bot routing — co-enabling several bots on one repo

The filters above describe a **single-bot** webhook. A repo usually wants
**several** bots on the same delivery — a reviewer *and* a dependency
guard — and a flattened config cannot express that: `SelectBot()` returns
`""` as soon as two bots are in scope, so the lane fell back to the
hardcoded reviewer and the guard was lost entirely.

A webhook that an auto-provisioned integration has migrated therefore
carries a **per-bot routing table** (`BotRules`, one entry per
co-enabled bot). An inbound delivery then **fans out to every bot whose
own rule claims the event and admits the author** — each with its own
author filter, label filter, and launch vars, its own idempotency key,
and a FRESH vars map (so two bots never share one publish grant)
([pkg/webhooks/types.go:BotRule](../pkg/webhooks/types.go),
[pkg/server/webhooks_common.go:resolveForgeEventBots](../pkg/server/webhooks_common.go)).

A bot's rule is materialised by the forge orchestrator from its manifest
`forge.webhook` block:

- **`events`** — the normalized event kinds this bot claims (e.g.
  `pull_request`, `pull_request_comment`). A command-only bot claims none
  and is routed only by its `CommandMap`.
- **`author_allowlist`** — restricts THIS bot to PRs/MRs from these
  logins (empty = any author). Entries support a **suffix wildcard**:
  `*renovate[bot]` matches `acme-renovate[bot]` too, because
  self-hosting Renovate/Dependabot under an org GitHub App (the usual way
  to make their PRs trigger CI) **renames** the bot.
- **`author_scope: exclusive`** — the authors in `author_allowlist` are
  MINE: provisioning adds them to every OTHER co-enabled bot's
  `author_denylist`, so a general reviewer stops double-reviewing the
  dependency PRs this guard owns — without the reviewer's manifest
  naming the guard. The default `shared` leaves the authors open to all.
  (Meaningful only with a non-empty `author_allowlist`.)
- **`launch_vars`** — THIS bot's default run vars, kept per bot so two
  bots' vars cannot collide in one flat map.

A config written before this field existed carries no `BotRules`, and
the legacy single-bot path — `SelectBot`/`default_bot_id` and its
idempotency keys — is kept **byte-identical**; re-provisioning an
unchanged bot set backfills the table without minting a token or calling
the forge. One contract change follows from the fan-out: a delivery no
enabled bot claims is now **filtered (200)** rather than 403 — a 4xx on
a forge hook is what makes GitHub disable it.

## Idempotency

Iterion durably dedupes deliveries via a unique index on
`(tenant_id, idempotency_key)` — a duplicate insert returns
`ErrDuplicate` and the handler replies 200 with `{status:"duplicate",
run_id, delivery_id}` ([pkg/server/webhooks_common.go:insertAndLaunchWebhook](../pkg/server/webhooks_common.go)).
The key space is **path-disjoint** so the same event id can't collide
across paths — most paths carry a literal prefix (`mr|`, `gh|`, `fj|`,
`generic|`), while the GitLab note path stays disjoint via its
`note:<note_id>` subject segment rather than a prefix:

| Key prefix | Identifying tuple | Bumps on |
|---|---|---|
| `mr\|` | `(tenant, webhook, project_id, mr_iid, head_sha)` | a new push (new head SHA) → fresh launch |
| _(none)_ | `(tenant, webhook, project_id, note:note_id)` | a new `/revi` comment → fresh launch |
| `gh\|` | `(tenant, webhook, project_path, pr_number, head_sha)` | a new push → fresh launch |
| `fj\|` | `(tenant, webhook, project_path, pr_number, head_sha)` | a new push → fresh launch |
| `generic\|` | `(tenant, webhook, request.idempotency_key OR sha256(body))` | any change in dedup token or body → fresh launch |

Terminal (non-launched) rows — `invalid`, `filtered`, `quota_exceeded`,
`rate_limited`, `launch_error` — get a **random UUID** as their
idempotency key so they never collide with the real dedup key
([pkg/server/webhooks_common.go:recordTerminalWebhookDelivery](../pkg/server/webhooks_common.go)).
A retry of the same upstream event after a transient failure can
therefore launch successfully.

## Limits + admission

A delivery passes through these gates in order, each enforced by the
middleware before the provider handler runs
([pkg/server/middleware_webhook.go:webhookAuth](../pkg/server/middleware_webhook.go)):

| Step | Outcome on fail | HTTP |
|---|---|---|
| Resolve `Config` by URL id | not found / provider mismatch | 401 `invalid webhook` |
| Verify token (token-mode only) | bad token | 401 `invalid webhook token` |
| Config not disabled | `enabled=false` | 410 `webhook disabled` |
| Per-webhook token-bucket rate (default `rate=1.0, burst=10`) | bucket empty | 429 with `Retry-After` |
| Per-org monthly call quota (default **10 000**, override `monthly_call_limit`) | quota exhausted | 429 `monthly call quota exceeded` |
| Org status `active` | suspended / read-only | 403 `org suspended` |

Then the provider handler verifies the body (HMAC for github/forgejo,
optional HMAC for generic), parses, applies the event/project/bot
filters above, and finally hands off to `insertAndLaunchWebhook`
which runs the **launch-admission gate** (org-level quotas / cost cap
/ concurrency / launch rate — see
[quotas-and-limits.md](quotas-and-limits.md)) before publishing the
run.

A denial at the launch-admission step writes a `launch_error` delivery
row carrying the stable denial reason (`monthly_run_quota_exceeded`,
`monthly_cost_cap_exceeded`, …) and returns the standard launch-denial
envelope to the forge — so the forge sees a 402/429 it can decide what
to do with, not a synthetic 200.

## Delivery audit + statuses

Every accepted request lands in `webhook_deliveries` with one of these
statuses ([pkg/webhooks/types.go status constants](../pkg/webhooks/types.go)):

| Status | Meaning |
|---|---|
| `accepted` | Auth/quota passed, awaiting launch result (intermediate state) |
| `launched` | Run published to the queue; `run_id` set, `launched_at` stamped |
| `duplicate` | Same idempotency key replayed — `run_id` of the original launch is returned |
| `filtered` | The event didn't match `event_allowlist` / `project_allowlist` / `author_allowlist` / `IsReviewable` |
| `invalid` | Bad payload, missing token, bot not permitted by scope |
| `rate_limited` | Per-webhook bucket empty |
| `quota_exceeded` | Per-org or per-webhook monthly call quota exhausted |
| `launch_error` | The launch-admission gate refused (cost cap / run quota / concurrency / org suspended) OR the runner publisher failed |

Delivery rows never carry the raw payload — only a SHA-256 hash, the
selected fields (`event_kind`, `event_action`, `project_path`,
`subject_id`, `subject_sha`), the source IP, and (for launched rows)
the resulting `run_id`. Read them at
`GET /api/teams/{id}/webhooks/{webhook_id}/deliveries` (last 100 by
default).

## Webhook CRUD API

All routes are mounted under `/api/teams/{id}/webhooks/…` and require
team admin (`canManageTeam`) for mutations; team membership
(`canViewTeam`) for reads
([pkg/server/webhooks_routes.go:registerWebhookRoutes](../pkg/server/webhooks_routes.go)).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/api/teams/{id}/webhooks` | team member | List webhooks for the team |
| `POST` | `/api/teams/{id}/webhooks` | team admin | Create + mint token (returned once) |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}` | team member | Read a single webhook |
| `PATCH` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Update name/enabled/scope/rate/quota/vars/key_overrides and the config keys below |
| `DELETE` | `/api/teams/{id}/webhooks/{webhook_id}` | team admin | Remove (deliveries kept for audit) |
| `POST` | `/api/teams/{id}/webhooks/{webhook_id}/rotate` | team admin | Rotate token + re-seal HMAC secret |
| `GET` | `/api/teams/{id}/webhooks/{webhook_id}/deliveries` | team member | List recent deliveries (default 100) |
| `POST` | `/api/webhooks/{provider}/{id}` | webhook token / HMAC | Public delivery endpoint, one per provider |

The `POST` create response shape:

```json
{
  "config": { "id": "…", "tenant_id": "…", "provider": "gitlab", "token_last4": "Vp3a", … },
  "token": "iwh_…"
}
```

The `token` field is the **only** way to recover the plaintext — once
the response is closed, only the salted hash remains. The studio shows
it inside a "copy now, you won't see it again" affordance.

### `key_overrides` — pin a BYOK key per webhook

`key_overrides` maps a provider name (`"anthropic"`, `"openai"`, …) to a
BYOK API-key id owned by the same team. Runs launched through this
webhook then use that exact key for the named provider, overriding the
org/user default in
[pkg/secrets/byok.go:Resolve](../pkg/secrets/byok.go). Use it to bill
several webhooks for the same bot against different keys (e.g. one
"production" webhook on the org's primary key, one "internal-CI"
webhook on a sandbox key). Mismatched provider, missing key, or a key
that belongs to another org are 400s
([pkg/server/webhooks_routes.go:validateKeyOverrides](../pkg/server/webhooks_routes.go)).

### `launch_vars` — pin run vars from the org config

Anything in `launch_vars` is merged into the run's variable map **after**
the handler-derived vars, so the operator's keys always win. Useful for:
e.g. forcing `severity_threshold=high` on a security webhook, or pinning
`pr_review_mode=summary` regardless of what the forge said (the review-pr
enum is `inline|summary`, default `inline`).

### Other `Config` keys settable through the CRUD API

All live on [pkg/webhooks/types.go:Config](../pkg/webhooks/types.go) and
are accepted by `POST` / `PATCH`:

| Key | Default | Meaning |
|---|---|---|
| `review_request_logins` | *(empty)* | Logins whose `review_requested` / reviewer-add delivery relaunches the reviewer, IN ADDITION to the identity derived from the connection. **This is what makes the lane work on GitHub**, where only a User account can be a requested reviewer: name a bot user reached through a `pat` connection, so the review is posted by that same account and the forge re-arms the button. Explicit only — never derived from a connection's account, which on the PAT path is typically a maintainer's own, and deriving would turn every reviewer ping addressed to that human into a bot run. The logins join the shared identity set, so the anti-loop actor guard recognises them too. |
| `review_on_sync` | `false` | Re-review on each push to a PR head, so a required status re-evaluates on the revision that fixed it. Required for a blocking [merge gate](merge-gate.md). The lane is debounced (`ITERION_WEBHOOK_SYNC_DEBOUNCE`, default `3m`): a push volley costs one review of the final head — see the GitHub section above. |
| `overlap` | *(empty = allow)* | Concurrency policy for runs this webhook launches, keyed on (webhook, subject, bot) — one PR's reviews, not the whole repo's. `allow` / `skip` / `supersede`. **Empty means allow**, not `pkg/schedgate`'s `skip` default: a webhook is event-driven and every delivery has always launched, so the gate applies only when explicitly set. `supersede` is the one worth setting alongside `review_on_sync` — three pushes in two minutes otherwise launch three runs, two of which review dead commits. |
| `operator_launch_vars` | — | Vars layered **between** the handler-derived base and a bot's own rule vars (precedence: base < bot rule vars < these). Kept separate from `launch_vars` so co-enabling two bots that declare the same key does not make them share whichever value won. |
| `secret_overrides` | — | Pins a stored secret per workflow-secret name, so several webhooks for the same bot can post under different forge tokens / bot identities. The secret twin of `key_overrides`. |
| `retry_usage_window`, `retry_max_attempts`, `retry_max_wait`, `retry_jitter` | *(bot manifest, then machine default)* | The launch-surface layer of the [retry policy](scheduling.md#retry--a-provider-quota-window-is-waited-out-not-re-attempted) for a run that dies on an exhausted provider usage window. Only what is set here overrides the layers below. A webhook-launched run is often one an author is waiting on, so a shorter `max_wait` than a nightly's is usually right. |
| `forge_base_url` | *(derived)* | Explicit forge base URL for a self-hosted instance. |
| `block_fork_prs` | `false` | Persisted but **never read** by any launch path — see [merge-gate.md](merge-gate.md) for what actually guards fork PRs, which differs by provider. |

`authorized_repliers` / `min_replier_role` gate who may talk back to a
bot in a note thread — see
[forge-conversations.md](forge-conversations.md).

### `branch_improve_as_pr` — how the branch-improvement bot lands its work

A boolean toggle ([pkg/webhooks/types.go:Config.BranchImproveAsPR](../pkg/webhooks/types.go),
patchable via the CRUD API) that changes how the branch-improvement bot
(Billy) delivers its hardening on a PR it reviews. Default (`false`): Billy
commits and pushes **directly onto the PR's own source branch** in place, so
the author merges their PR and gets the improvements with it. `true`: Billy
instead opens a **separate PR targeting that source branch** (routed through
`open_mr=true` + `mr_base=<source branch>`), so the author reviews the bot's
changes as an isolated diff before integrating — the right posture for a
third-party contributor who should stay in control of their branch. Applied
on the GitHub / GitLab / Forgejo PR and `/revi` comment paths
([pkg/server/webhooks_github.go:branchImproveVars](../pkg/server/webhooks_github.go),
[pkg/server/webhooks_prforge.go:stampBranchImprovePushBack](../pkg/server/webhooks_prforge.go)).

## Observability

Every delivery bumps a label set on `iterion_webhook_deliveries_total`
(`provider`, `status`) and pre-handler throttles bump
`iterion_webhook_throttled_total` (`provider`, `reason`)
([pkg/cloud/metrics/metrics.go](../pkg/cloud/metrics/metrics.go)). There
are **deliberately no tenant labels** — cardinality discipline — so
per-org accounting lives in Mongo (`org_usage` + `webhook_deliveries`),
not Prometheus.

The starter PrometheusRule pack ships an alert on
`increase(iterion_webhook_throttled_total[1h]) > 50` that surfaces a
noisy forge integration or an abusive caller. See
[charts/iterion/README.md](../charts/iterion/README.md) for the full
alert pack.
