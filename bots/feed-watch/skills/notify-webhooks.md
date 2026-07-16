---
name: notify-webhooks
description: feed-watch delivery mechanics — Mattermost/Slack incoming-webhook payloads, the webhooks secret map, dry-run, partial-failure semantics, troubleshooting a digest that did not arrive.
---

# Webhook delivery (deterministic notify step)

The digest is delivered by a tool node (no LLM): one HTTP POST per
configured sink to a Mattermost/Slack **incoming webhook**. Payload:

```json
{"text": "<message_markdown>", "channel": "#veille", "username": "Vigie", "icon_emoji": ":telescope:"}
```

- Mattermost accepts Slack-compatible incoming webhooks natively;
  `channel` override must be enabled on the Mattermost integration.
- Messages over 14000 chars are truncated with an explicit notice (the
  full digest stays in the run artifacts and the state archive).

## Resolution chain

`config sinks[].webhook` (a NAME) → looked up in the `webhooks` secret
(JSON map name → URL, mounted read-only as a file). A name missing
from the map fails the run loudly, listing the missing names. URLs
never appear in the repo, in prompts, or in logs.

## Failure semantics (by design)

- **dry_run=true** — payloads printed, nothing posted, run succeeds.
  The queue is NOT consumed and nothing is archived (only a delivered
  digest reaches commit_state), so the next real digest still sees
  every item.
- **no sinks configured** — not an error; the digest stays in the run
  artifacts (report-only mode). Queue kept, same rule.
- **partial failure** (some sinks 2xx, some not) — the run SUCCEEDS
  and the failures are listed in the notify output summary. Rationale:
  failing the run would re-post to the sinks that already delivered on
  resume (duplicates are worse than a visible partial).
- **total failure** (every POST failed) — the run FAILS
  (failed_resumable). Nothing was delivered, so `iterion resume` is
  safe and will re-attempt delivery.

## Troubleshooting a missing digest

1. `iterion report --run-id <id>` → check the `notify` node output:
   `delivered`, `targets`, `summary` (failures are spelled out).
2. Queue was empty? A digest with nothing pending ends at
   `load_pending` (`has_items=false`) — by design, no empty digests.
3. Webhook 4xx: URL revoked/wrong (`iterion secret set webhooks …`
   again), or the channel override is not permitted by the Mattermost
   integration settings.
4. The digest itself is always recoverable from the run artifacts
   (`synthesize` output) and `<state_dir>/<category>/digests/*.md`.
