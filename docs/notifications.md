# User notifications (web push)

Iterion cloud can notify a user **through the browser** when one of their
runs needs attention — most importantly when a run pauses on a **human
form** (`paused_waiting_human`): without this, nothing signals the pause
unless the run's console page is open. Notifications are delivered by
**Web Push** (RFC 8030 + VAPID) through a service worker, so they arrive
even when the studio tab is in the background or closed.

Diagram: [Flow — notifications and callbacks: one outcome event, three outbound families](diagrams/flow-notifications-callbacks.md)

Notified events (v1):

| Event | Notification |
|---|---|
| `human_input_requested` (run paused on a human form) | "Input needed: *run name*" + the paused node's id |
| `run_finished` | "Run finished: *run name*" |
| `run_failed` (incl. `failed_resumable`) | "Run failed: *run name*" (+ failing node) |
| `run_cancelled` | "Run cancelled: *run name*" |
| `usage_window_parked` (parked on a provider quota window, retry armed) | "Run paused on a usage window: *run name*" + the instant it resumes at |

An **operator soft-pause** (`paused_operator`, no pending interaction)
deliberately does not notify — the operator initiated it.

A run parked on a provider usage window is **not** a failure, and its episode
is not a transition. It arrives as `run_failed` (that is the status the engine
wrote), and the fact that tells the two apart is the **wall** the row is
waiting on — the instant the provider announced, persisted on the retry row
(`RunRetryState.Wall`) and carried on the event as `usage_park_wall`. The
episode id is the run plus that wall, so the claim that already dedups the bus
against the reconciliation sweep also dedups every re-park of one window: the
park that opens it, the sweeper's pull-forward probe, the free re-arm that
follows each probe's refusal, and a charged park that re-states the same
instant are one episode. A wall the provider **moves** is news and notifies
again, and a run whose retry budget is spent has nothing armed — no wall — so
it notifies as the failure it has become.

The wall identifies the episode; the message names the instant the row is
armed for (`usage_park_resumes_at` on the event). They are not the same: the
arming takes the earlier of the wall and the reopening of a credential the
launch's resolution passed over, so a run can come back three hours from now
while the wall it is parked on is Monday's.

Measured before that: one weekly wall pushed about twenty-five "Run failed"
messages in a day per parked run (the probe ships armed with a budget of 24),
every one of them wrong about what had happened.

Clicking the notification focuses (or opens) the studio on `/runs/<id>`,
where the pending form renders.

## Who gets notified

- **Default:** the run's owner (`Run.OwnerID`, the user who launched it).
- **Team-wide opt-in:** each user can enable "also notify me for every run
  of my team" in **Account → Notifications** (persisted server-side,
  `PUT /api/v1/notifications/prefs {"scope":"team"}`).

The title names the run; the body names the paused or failing node.

## Architecture

```
Engine.doPause / run end (checkpoint + status persisted)
  │  in-process run: runview.emitRunOutcome          (existing seam)
  │  runner pod:     Runner.fireOutcomeEvent          (pkg/runner/loop.go)
  ▼
trigger.BuildRunOutcome (pkg/trigger/runoutcome.go — shared authority:
  kind derivation, tenant+owner enrichment, per-episode event ID)
  ▼
eventbus.Bus   — InProcBus locally; NATSBus in cloud (subject tree
  iterion.events.>, same NATS connection as the work queue)
  ▼
usernotify.Dispatcher (pkg/usernotify) — queue group "usernotify":
  exactly ONE server replica handles each event. Resolves recipients
  (owner + team opt-ins), claims the episode in the SentStore
  (first-writer-wins), renders the Notification, fans out to sinks.
  ▼
usernotify.Sink — the channel abstraction:
  • webpush.Sink (pkg/usernotify/webpush)  — ships now (cloud)
  • desktop OS notification (Wails)         — future sink, same interface
  • email (pkg/mail)                        — future sink
```

A sibling consumer rides the same spine for the OPERATOR (deployment-wide,
not per-user): `alert.OpsDispatcher` (queue group `operator-alerts`) turns
run.failed outcomes into parked/failed alerts on the deployment webhook
(`ITERION_ALERTS_WEBHOOK_URL`), with the same episode-claim dedup (shared
`sent_notifications` collection, `ops|`-prefixed keys) and its own 2-minute
sweep. See the operator-webhook note in
[docs/usage-caps.md](usage-caps.md).

**Reliability.** The bus is deliberately lossy (at-most-once): if no
server replica is subscribed at publish time, the event is gone. The
**reconciliation sweep** (`usernotify.Sweeper`, every 2 min on each
server replica) closes that gap: it re-derives the outcome event for
every run still `paused_waiting_human` (no time bound) and every
terminal run updated in the last 24h — keyset-paginated by `updated_at`
so a backlog beyond one page cannot starve the oldest episodes — and
replays it through the dispatcher. The `sent_notifications` collection
(unique episode key, 30-day TTL) makes replays idempotent across the
bus path, the sweep, and every replica; the claim is **two-phase**
(pending at claim, confirmed after ≥1 sink delivered), so a pod killed
mid-delivery leaves a pending claim that is taken over after a 10-min
grace instead of silencing the episode forever. A delivery that fails
on **every** sink releases its claim so the next sweep retries. The
episode key folds in the run's `updated_at`, so a resumed run failing
again (same terminal status) or a review gate re-pausing on the same
interaction notifies again rather than deduping against the earlier
episode.

**Stores (Mongo, cloud):** `push_subscriptions` (endpoint-unique, per
user × browser), `notification_prefs` (per tenant × user scope),
`sent_notifications` (episode claims).

## Enabling it (cloud)

1. Generate the VAPID keypair **once** per deployment:

   ```sh
   iterion server webpush-keys
   # ITERION_WEBPUSH_VAPID_PUBLIC_KEY=...
   # ITERION_WEBPUSH_VAPID_PRIVATE_KEY=...
   ```

2. Set both env vars on every **server** replica (chart:
   `secrets.auth.webpush.*` in the auth secret bundle, or add the two
   keys to the externally-managed auth secret), plus the contact:

   ```sh
   ITERION_WEBPUSH_SUBSCRIBER=mailto:ops@example.org   # chart: config.webpush.subscriber
   ```

   The feature is enabled iff both keys are set; `server_info` then
   reports `web_push_enabled: true` and the studio shows the
   **Account → Notifications** panel. Runner pods need no webpush env —
   they only publish outcome events onto NATS.

3. Every replica must share the **same** keypair. Rotation invalidates
   every stored browser subscription: pushes to old subscriptions get
   404/410 and are pruned; users just re-enable notifications in their
   settings (the toggle detects the key mismatch and re-subscribes).

## Browser side

- The service worker ships at `/service-worker.js` (root scope, unhashed,
  from `studio/public/`). It is registered **only** when the user flips
  the toggle — never at page load — and the permission prompt likewise.
- Enrollment: `studio/src/lib/webPush.ts` (`enablePush`) — permission →
  SW registration → `pushManager.subscribe` with the server's public key
  → `POST /api/v1/notifications/push/subscriptions`.
- One subscription per browser profile × device; a user can enroll
  several browsers. Subscriptions belong to the **user**, not a team: a
  multi-team user enrolls once and receives notifications for runs of
  every team that resolves them as a recipient. Dead subscriptions
  (browser revoked, 404/410 Gone) are pruned automatically on the next
  push.
- "Send a test notification" in the panel exercises the full path for
  the current user only.

## REST surface

All cookie-authed under the standard middleware. With the feature off
none of them is registered, and the request falls through to the SPA
catch-all: `GET …/prefs` answers a JSON 404, the other methods a bare
405:

| Route | Purpose |
|---|---|
| `POST /api/v1/notifications/push/subscriptions` | register this browser's `PushSubscription` |
| `DELETE /api/v1/notifications/push/subscriptions` | remove own subscription (body: `{endpoint}`) |
| `GET/PUT /api/v1/notifications/prefs` | per-user scope (`own` \| `team`) |
| `POST /api/v1/notifications/push/test` | canned push to the current user |

`GET /api/server/info` exposes `web_push_enabled` +
`web_push_vapid_public_key`.

## Desktop (planned)

The Wails desktop app will implement `usernotify.Sink` with a native OS
notification and attach to the local in-proc bus — no dispatcher or
interface change required; that seam is the reason the sink abstraction
exists. Until then, the desktop app keeps its existing in-window alert
notifications (`pkg/alert` → `studio/src/lib/desktopNotify.ts`).
