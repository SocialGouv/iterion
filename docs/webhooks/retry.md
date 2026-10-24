# Webhook receipts and launch retries

A claimed inbound delivery answers **202** with
`{"status":"accepted","delivery_id":"…"}` before its bot launches. A fan-out
also returns `launches`, with one outcome and delivery ID per claimed bot.
The receipt promises a recorded claim, not a successful run. Read the eventual
`status`, `run_id`, `attempts`, `failed_at` and `next_attempt_at` at
`GET /api/teams/{id}/webhooks/{webhook_id}/deliveries`.

The shared path covers GitHub PR events, slash commands, review-thread replies
and issues, and the GitLab, Forgejo and generic direct-launch handlers. A board
coordinator's `carded` response and the push debounce's `deferred` response keep
their own execution owners.

## Connection and shutdown

The HTTP adapter registers the delivery worker with the server's existing
`tryGoSafe` shutdown join. It returns the network response when the claim is
acknowledged, including HTTP/2's end-of-stream. The worker uses
`launchBookingContext`: `context.WithTimeout(context.WithoutCancel(ctx), 30s)`.
Identity and tracing values survive; request cancellation does not cancel the
launch. Each claimed target and its final bookkeeping use this same bounded
primitive. A fan-out confirms concurrency again before each execution.

Shutdown stops admitting new deliveries and stops the periodic sweep. A launch
already claimed is not canceled by that signal. Shutdown joins it within the
existing shared background budget (500ms by default, also bounded by the shutdown
caller's deadline); an overrun is logged. The worker retains its own deadline.
This is a bounded drain, not a guarantee against process termination. A process
crash or a failed final store write can leave an `accepted` row: automatic retry
of an uncertain launch would risk creating a second run, so this sweep does not
reclaim such rows.

## Automatic retry

Every 20 seconds the sweep reads at most 50 due failed deliveries. Memory and
Mongo implement the same selection and `ClaimFailedRetry` compare-and-set:
only the reader of the still-current failed attempt may claim it. An old sweep
snapshot cannot consume the backoff of a newer attempt.

A claimed direct launch stores its resolved target before generating per-run
publish grants. This internal target survives Mongo persistence and includes
the bot, vars, repository/ref and trust/commit pin; it is excluded from delivery
JSON responses. The raw webhook payload is not stored. A retry reads the current
webhook config, checks its enabled state, tenant, provider, bot scope and trust,
then passes ordinary tenant admission. A super-admin's retry does not bypass
the webhook tenant's quota.

The first failed attempt waits 30 seconds. Later failures double that delay up
to 10 minutes. The default ceiling is **8 total attempts**, initial attempt
included; `ITERION_WEBHOOK_MAX_ATTEMPTS=<positive integer>` changes that ceiling.
A retry admission failure also consumes an attempt and schedules backoff. At the
ceiling the delivery becomes **`launch_exhausted`**, with no next retry.
A removed/disabled webhook, withdrawn bot scope or incompatible provenance also
ends the chain with `launch_exhausted` and an explanatory error.
The durable subject audit also ends an obsolete retry: a later PR/MR closure
covers its comment and thread replies, and with `overlap=supersede` a later claim
of the same bot/subject takes precedence. This check includes forge redeliveries
and closes received while the failed launch was still in flight. It uses exact
tenant/webhook/project scope and retains nanosecond receipt ordering across Mongo persistence.

The sweep owns direct inbound launches only. Push-debounce, gate relaunch and
gate auto-fix retain their existing recovery policies and current-forge checks.
Pre-claim audit failures and old rows without a saved target cannot be replayed
through this endpoint; send a fresh authenticated event after fixing their cause.

## Manual retry

```http
POST /api/webhooks/deliveries/{id}/retry
Authorization: Bearer <super-admin access token>
```

No body. A successful claim answers **202** with the original `delivery_id` and
`status: accepted`. It bypasses the scheduled delay, not the attempt ceiling.
The claim races safely with the periodic sweep and forge redelivery.

| HTTP status | Meaning |
|---|---|
| 401 / 403 | Authentication / super-admin authority required |
| 404 | `delivery_not_found` |
| 409 | `delivery_not_retryable`; `status` names the delivery state |
| 500 | Delivery store failure |
| 503 | `server_draining` or webhook stores unavailable |

A healthy `launched` delivery is refused with **409**, as are `accepted`,
`launch_exhausted`, target-less audit rows and failures carrying a run ID.
The original idempotency key and delivery ID remain unchanged through retries.
If a launcher reports a persisted run despite its error, that run ID is retained
and no second launch is permitted. If the launcher reports that publication may
have landed without naming a run, the status is **`launch_uncertain`** and no
automatic or manual launch retry is permitted. Inspect the existing run/queue
outcome rather than starting another run.

The executable witnesses and their mutations are in
[`webhooks_durability_test.go`](../../pkg/server/webhooks_durability_test.go);
the paired store contract is in
[`delivery_retry_test.go`](../../pkg/webhooks/delivery_retry_test.go).
