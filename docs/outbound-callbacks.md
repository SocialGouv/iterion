# Outbound run-completion callbacks

> **Inbound** webhooks (an external event triggering an iterion run)
> live in [docs/webhooks.md](webhooks.md). This page covers the
> opposite direction: a run reaching a terminal state and POSTing the
> result somewhere.

A **run-completion callback** lets an external integration trigger an
iterion run asynchronously and be told — without polling — when the run
finished and what its final answer was. It is a generic engine
primitive: the payload is a neutral JSON envelope, and all
platform-specific handling lives in the receiver.

The headline consumer is the Mattermost
[`@clarify-bot` adapter](../contrib/mattermost-clarify/), but the
contract is platform-neutral: a CI bridge, a Slack adapter, or any
service that wants "call me back when this run is done" uses the same
mechanism.

## Requesting a callback

Supply three optional fields when launching a run via `POST /api/runs`
(they also exist on `runview.LaunchSpec` for programmatic callers):

```jsonc
{
  "source": "<.bot source>",
  "vars": { "...": "..." },
  "callback_url": "https://my-service.example.com/iterion-callback",
  "callback_token": "<opaque correlation value>",
  "callback_answer_node": "facilitator"   // optional
}
```

- **`callback_url`** — the http/https endpoint iterion POSTs to when the
  run reaches a terminal state. Empty (the default) = no callback.
- **`callback_token`** — an opaque value echoed back verbatim in the
  payload. Use it to correlate the callback to the originating request
  (e.g. a chat thread id) without keeping server-side state. iterion
  never interprets it.
- **`callback_answer_node`** — optionally names the node whose latest
  artifact holds the run's user-facing answer (read from its
  `final_answer` field). When omitted, iterion scans every
  artifact-producing node for a `final_answer` field and uses the first.

The three fields are persisted on the run and propagated across the
cloud queue (`queue.RunMessage`), so both local (in-process) and cloud
(runner-pod) executions deliver the callback identically.

## The payload

When the run terminates, iterion POSTs this JSON to `callback_url`:

```jsonc
{
  "v": 1,
  "run_id": "01J…",
  "status": "finished",            // finished | failed | failed_resumable | cancelled
  "workflow_name": "clarify",
  "final_answer": "Did you mean staging or prod?",
  "final_answer_node": "facilitator",
  "error": "",                     // populated when status != finished
  "callback_token": "<your value, verbatim>"
}
```

- **Versioned** (`v`) — gate on it when parsing.
- **Fired once per terminal transition.** A run that pauses
  (`paused_waiting_human` / `paused_operator`) does **not** fire — it is
  waiting, not done. A later `resume` that actually terminates fires
  then.
- **`final_answer` may be empty.** A workflow that legitimately produces
  no user-facing answer (e.g. a facilitator choosing silence) yields an
  empty string; receivers should treat that as "post nothing".

Delivery is **best-effort**: a webhook failure is logged and never
affects the run outcome. The `callback_url` is never logged (it may embed
a secret in its query string).

## Authenticating the payload (HMAC signature)

The `callback_token` is for **correlation, not authentication** — iterion
echoes it back but never verifies it, so on its own it does not stop a
forged POST to a known callback URL. To let the receiver prove a delivery
genuinely came from iterion, set a shared secret on the iterion server:

```bash
ITERION_COMPLETION_WEBHOOK_SECRET=<random-secret>
```

When set, iterion HMAC-SHA256-signs **the exact response body** and sends
the result in a header:

```
X-Iterion-Signature: sha256=<hex>
```

The receiver recomputes `HMAC_SHA256(secret, raw_body)` over the bytes it
received (before JSON parsing) and compares — constant-time — against the
header. A mismatch means a forged or tampered request; reject it. The
helper that produces and checks this is
[`notify.Sign` / `notify.Verify`](../pkg/notify/sign.go) (the same
primitive a future native inbound webhook would use to authenticate
trigger requests).

When the secret is empty (default) iterion sends **no** signature header.
A receiver should then either run only on a trusted private network or
refuse to start — `notify.Verify` returns false for every input when its
secret is empty, so "no secret configured" fails closed rather than
silently accepting everything.

## Security: SSRF guard

`callback_url` arrives over the launch API, so it is treated as
attacker-influenced. Before delivering, iterion vets it
([`pkg/notify`](../pkg/notify/completion.go)):

- scheme must be `http` or `https`;
- the host must resolve **exclusively** to public-unicast addresses —
  loopback, link-local, RFC-1918 private ranges, and cloud-metadata
  endpoints (e.g. `169.254.169.254`) are refused;
- unresolvable hosts and cluster-internal aliases
  (`*.svc.cluster.local`, `kubernetes.default`,
  `metadata.google.internal`) are refused;
- resolution **fails closed**.

This mirrors the SSRF guard used by the preview proxy — both call the
shared [`pkg/secure/httpdial`](../pkg/secure/httpdial/httpdial.go)
(`ResolvePublicHost` / `IsPublicUnicast`).

### Allowing private targets (self-host)

Many self-hosted deployments run the callback receiver on the same
private network as iterion (e.g. the Mattermost adapter beside the
server). Set:

```bash
ITERION_COMPLETION_WEBHOOK_ALLOW_PRIVATE=1
```

on the iterion **server** (and on cloud **runner** pods, if applicable)
to relax the guard and permit private/loopback callback URLs. Off by
default — cloud runners must not be able to gateway into a private
network.

## Implementation map

| Concern | Location |
|---------|----------|
| Payload + notifier + SSRF guard | [`pkg/notify/completion.go`](../pkg/notify/completion.go) |
| Run fields | `store.Run.{CallbackURL,CallbackToken,CallbackAnswerNode}` |
| Queue propagation | `queue.RunMessage.{CallbackURL,…}` |
| Launch option | `runtime.WithCallback` |
| Launch API | `launchRunRequest` in `pkg/server/runs_launch.go`; `runview.LaunchSpec` |
| Fired (local) | `runview` `spawnRun`, after the run body |
| Fired (cloud) | `runner` `processOne`, after `executeRun` |
