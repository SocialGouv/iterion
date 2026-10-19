# Flow — webhook-to-run: the admission gates, every refusal, and the one chokepoint

An inbound forge or CI delivery becomes a run by crossing an authenticating middleware, a provider handler's ordered refusals and one shared launch tail, which is where the delivery row, the metering and the run are created.

> Drawn from the code at commit `f564846d3`; how this page is kept true: [README](README.md).

## The path a delivery takes

The middleware authenticates and admits; the handler decides whether this event deserves a run and for which bots; `launchWebhookTarget` does everything that costs something. That tail is the chokepoint of five callers, and three of them — the debounce sweep, the merge-gate relaunch and the merge-gate auto-fix — enter it minutes to hours later without crossing a handler at all, which is why the lane and commit-pin refusals live there and not at the top of each provider. The refusal order drawn there is the GitHub and Forgejo one; the GitLab merge-request lane runs the same guards in its own sequence — the scope filter, the fork guard, the routing table, the replier gate, the hold label, then the bot-author skip — and carries no merge-queue lane at all.

A refusal answers `200` with `status: filtered` on purpose: a `4xx` on a forge hook is what makes the forge disable it, and with it every future review.

The whole path is drawn on the feature page: [webhooks.md § The path a delivery takes](../webhooks.md#the-path-a-delivery-takes).

## Steps

| Actor | Action | Code |
|---|---|---|
| provider | POSTs the delivery; the operator JWT layer skips every `/api/webhooks/` path | `isPublicPath` in [middleware.go](../../pkg/server/middleware.go) |
| server | resolves the config by URL id with no tenant, then authenticates: a header token, or nothing in HMAC mode | `webhookAuth`, `webhooks.VerifyToken` |
| server | admits: enabled, the per-webhook bucket, the monthly call CAS, and the team's existence, readability and launch right | `webhookAuth`, `MongoCounter.Allow` |
| server | stamps the synthetic principal and the config on ctx, and writes `last_used_at` detached | `auth.WithIdentity`, `store.WithIdentity`, `MarkUsed` |
| server | verifies `HMAC-SHA256` over the raw body before any delivery row | `verifyWebhookHMACBody`, `webhooks.VerifyHMACSignature` |
| server | dispatches on the event header and parses into one shape | `handleGitHubWebhook`, `prforge.ParsePullRequest` |
| server | ends what a closed pull request still owes: the parked row, then each run's armed retry and the run | `stopRunsForDeadPR`, `DeleteBySubject`, `AbandonRunRetry` |
| server | refuses a head it cannot prove is the base repo, then a held subject | `forkGuardRefusal`, `suppressedByHoldLabel` |
| server | heals a dequeued pull request, or stops a heal the queue took back | `autoHealMission`, `stopHealRunForRequeuedPR` |
| server | applies the scope filter, then the replier gate on a re-request | `webhooks.MatchEvent`, `realWebhookPRForgeReviewRequestGate` |
| server | fans out to every rule claiming the event, each with its own key and vars | `resolveForgeEventBots`, `forgeIdemKey`, `forgePREventTargets` |
| server | parks a synchronize volley, and the sweeper replays it through the same tail | `deferSyncLaunch`, `sweepDeferredWebhookLaunches` |
| server, store | refuses the lane, checks the replay, supersedes, meters, claims the row | `launchWebhookTarget`, `gateLaunch`, `Insert`, `ClaimFailedRetry` |
| server | resolves the bot over three tiers, then submits the spec the runner will claim | `launchWebhookBot`, `resolveBotSource`, `runs.Launch` |
| forge | takes the in-flight gate and fixer marks, for a trusted run only | `markGateInFlight`, `markFixInFlight` |
| studio | reads the audit rows the lane wrote | `GET /api/teams/{id}/webhooks/{webhook_id}/deliveries` |

## The delivery status machine

A delivery row is the durable record of one admitted event: a lane row under its idempotency key, or an audit row under a random uuid. Two of the declared statuses are responses only, and two more are written nowhere.

```mermaid
stateDiagram-v2
%% vocabulary: webhooks.Delivery.Status
    [*] --> accepted: the tail claimed the row - Attempts 1 and ClaimedAt stamped
    [*] --> filtered: a refusal verdict, filed under a RANDOM uuid key
    [*] --> invalid: a bad payload, an unsupported event, or a bot out of the webhook's scope
    accepted --> launched: runs.Launch returned a run id
    accepted --> launch_error: a publish-grant refusal, or a launch that failed
    [*] --> launch_error: a gate denial, an authz failure, or an abandoned parked review
    launch_error --> accepted: a redelivery retakes the row through ClaimFailedRetry, a CAS on status and attempts
    launched --> [*]: terminal - the only status the dead-PR stop scans, and the opt-in supersede pass with it
    accepted --> [*]: stranded - read as a launch in progress only inside the 10-minute acceptedLaunchWindow
    filtered --> [*]: terminal audit row, answered 200 so the forge keeps the hook enabled
    invalid --> [*]: terminal audit row, answered 400 or 403
    duplicate: duplicate - a RESPONSE status, never a stored row: the replay check echoes the existing one
    deferred: deferred - a RESPONSE status, 202: the park launches later as its OWN delivery
    unwritten: rate_limited and quota_exceeded - declared constants with no writer - the middleware answers 429 before any row exists
```

| Status | Means | Written by |
|---|---|---|
| `accepted` | the claim: this delivery owns the key and is launching | `launchWebhookTarget` step 3, `Insert` or `ClaimFailedRetry` |
| `launched` | a run exists; `run_id` and `launched_at` set | the same tail, after `runs.Launch` |
| `launch_error` | the one retryable terminal state | a gate denial, a grant refusal, a launch failure, an authz failure, an abandoned park |
| `filtered` | a normal verdict: out of scope, a draft or an action that does not trigger a review, a merge-queue re-entry, held, a fork, a closed PR, no bot, a lane mismatch, an out-of-order park | `recordTerminalWebhookDelivery` |
| `invalid` | a payload or a bot the endpoint cannot serve | `recordTerminalWebhookDelivery` |
| `duplicate` | a response only — the replay check's echo, with `claimed_at` when no run is named yet | never stored; `duplicateOf` |
| `deferred` | a response only — `202`, the park | never stored; `deferSyncLaunch` |
| `rate_limited`, `quota_exceeded` | declared and unreachable: the throttle answers before a row could exist | nothing; `iterion_webhook_throttled_total` carries the fact |

The parked row has a lifecycle of its own in `webhook_deferred_launches`: `Upsert` arms it with `fire_at`, a bumped `generation` and a reset attempt count, `ClaimDue` leases it for two minutes, and the fire either deletes it by generation or re-arms it — exponential from 30s to 10 min, floored by the denial's own `Retry-After`, terminal at eight attempts or on a reset further out than 30 minutes, with a `launch_error` row naming the loss.

## Guards that refuse

- **The reviewable gate** — `200 filtered` with an empty reason for a draft or for an action that does not trigger a review (`IsReviewable` admits `opened`, `reopened` and `ready_for_review` only), unless a gate resync or a re-requested review admits the delivery.
- **The config lookup and the token** — `401 invalid webhook` for an unknown id and for a provider mismatch alike, so neither the id space nor a webhook's provider is probeable; then `401 invalid webhook token`, a constant-time compare against `cfg.TokenHash`, skipped entirely in HMAC mode where the handler owes the proof instead.
- **The body HMAC** — `401 invalid signature` from the provider handler, before a row or a grant exists, though the rate token and the monthly count are already spent; GitHub reads `X-Hub-Signature-256`, Forgejo `X-Forgejo-Signature` then `X-Gitea-Signature`, generic `X-Iterion-Webhook-Signature`.
- **`cfg.Enabled`** — `410 webhook disabled`, in the middleware only: the debounce sweep never re-enters it, so the sweeper re-reads the flag itself and drops the parked row.
- **The two throttles, then the tenant** — `429` from the per-webhook bucket and from the monthly call CAS, both before any handler, so neither writes a delivery row, and a counter the store cannot read answers `500`; then three outcomes from one team lookup — `403 tenant no longer exists`, `503 tenant lookup unavailable` and `403 org suspended`.
- **The fork guard** — `200 filtered`, fail-closed on a withheld or unnamed head, because `head.repo: null` is the shape a deleted fork takes. The GitLab merge-request lane instead refuses only a proven fork and lets a payload naming neither project through — its own comment says so.
- **The hold label** — `200 filtered` for every auto lane, opt-in and bot-agnostic, with no exemption for a review re-request, since a CODEOWNERS auto-request is indistinguishable from a click.
- **The scope filter** — the event, project and author allowlists, the last read off the pull request's **author** rather than the pusher on the GitHub and Forgejo lanes, and off the event **sender** on the GitLab merge-request lane, whose payload carries only the author's id; an empty allowlist means the provider's natural set, not nothing.
- **The replier gate** — `authorized_repliers` or a collaborator permission at least `min_replier_role`, deliberately after the scope filter so an out-of-scope delivery never costs a forge API call; an unauthorized click is demoted when an automatic lane co-rides, and answered `200 filtered` with the gate's reason when the click was the delivery's only reason.
- **The bot-author skip** — `200 filtered` when the sender is iterion's own forge identity, exempt for a gate resync and for a re-request, and fail-safe: an identity that cannot be resolved leaves the pull request reviewable.
- **The routing table** — `200 filtered` when no rule claims the event; a config carrying no `bot_rules` at all has no author filter to apply and routes to the pinned reviewer-role bot; on the generic endpoint instead, `400 bot required` and `403` for a bot outside `cfg.AllowsBot`.
- **The supersede pass** — opt-in, not a step: both the launch tail's pass and the park's return before scanning anything unless the webhook's overlap policy resolves to supersede, so a config with no policy cancels nothing; when it runs, its scope is this project, subject and bot, the project half being what keeps a push to `acme/b#7` from cancelling the live review of `acme/a#7`.
- **Lane disjointness** — `200 filtered` in both directions, written as two predicates and never as `ForkLane != IsFork()`, which would admit an unrecognised trust onto an ordinary lane; and an untrusted target with an empty `ExpectedSHA` is refused, because an empty pin disables the runner's comparison.
- **The replay check** — `200 duplicate` before the metering CAS, since that increment is the metering; a `launch_error` row is the one status it reuses instead.
- **`gateLaunch`** — `403 no_workspace` or `org_suspended`, `402` for the repo cap and the monthly run quota or cost cap, `429` for concurrency and the launch bucket; fail-open and logged on a degraded team or usage read, and the `member` rung is exempt for a `KindWebhook` principal.
- **The publish grant** — `422` when a launch pins another team's grant or capacity is exhausted; an untrusted workspace silently loses the grant vars instead, because a grant-less review is what that lane is.
- **The forge marks** — written only for a trusted run: an untrusted one would claim a pending required check it can never answer, leaving the pull request permanently unmergeable.

## Collections touched

`webhook_configs` (read by URL id with no tenant filter, `last_used_at` written detached) · `webhook_deliveries` (the lane row under its idempotency key, the audit rows under random uuids, a 90-day TTL) · `webhook_quotas` (the monthly call counter, `_id`-keyed, bumped and rolled back by CAS) · `webhook_deferred_launches` (the parked synchronize targets, claimed cross-tenant by `fire_at`) · `teams` and `orgs` (the suspend and launch rights, read once in the middleware and reused by the gate) · `org_usage` (the metered run increment, and its rollback) · `runs` (launched, superseded, stopped on a closed pull request) · `retry_circuits` (armed usage-window retries disarmed with the pull request) · `bot_sources` (the team and platform tiers of the bot resolution) · `forge_connections` (read under a tenant, to derive iterion's own bot identity and the publish grant) · `repo_integrations` and `board_issues` (the forge→board projection one launched delivery schedules, once per delivery). The spine mirror writes no collection: `emitForgeTriggerEvent` publishes on the trigger bus — a broker publish, or a non-blocking send to a subscriber — and `trigger_subscriptions` is read only afterwards, by the spine's own evaluator.

## Related

- [webhooks.md](../webhooks.md) — the feature's home page: the auth modes, the per-provider behaviour, the allowlists, the key spaces and the delivery API.
- [merge-gate.md](../merge-gate.md) — `review_on_sync`, the required check, and the relaunch and auto-fix lanes that enter the tail directly; [quotas-and-limits.md](../quotas-and-limits.md) — the launch-admission ladder every surface shares, and what each counter meters.
- [adr/034-webhook-dual-mode-auth-token-header-vs-body-hmac.md](../adr/034-webhook-dual-mode-auth-token-header-vs-body-hmac.md) — why the body HMAC is verified in the handler and not in the middleware; [data-model-forge-bots.md](data-model-forge-bots.md) — all four webhook collections, the delivery document and the bot tiers.
- [data-model-runs.md](data-model-runs.md) (`runs`, `retry_circuits`), [data-model-identity.md](data-model-identity.md) (`teams`, `orgs`), [data-model-credentials.md](data-model-credentials.md) (`org_usage`), [data-model-automation.md](data-model-automation.md) (the trigger spine the launch publishes onto, `repo_integrations` and `board_issues`), [access-control.md](access-control.md) — the `webhook` principal kind among the others.

