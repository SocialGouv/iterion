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

## Long digests are SPLIT, not cut

A digest above a sink's per-message budget is posted as **consecutive
messages**, each ending with a `_(i/n)_` marker. The channel is the
digest's only audience: pointing its readers at "the run artifacts" for
the missing half points them somewhere they cannot go.

- **Budget** — `max_message_chars` (var, default 14000: under
  Mattermost's own 16383-char post limit). A sink may override it with
  its own `max_chars` (a Slack channel wants less), and each sink is
  split to its own budget.
- **Boundaries** — blank-line blocks first, then lines, then a hard cut
  as a last resort. A digest entry is one line, so **as long as every
  entry fits the budget** no entry and no link is broken in two. The
  hard cut only fires on a single line longer than the budget by itself
  (a synthesis rendering the whole "also seen" list inline, say) — it is
  the one place a markdown link can land across two messages, so keep
  the smallest configured `max_chars` above your longest entry. A
  section heading that would close a message travels to the next one
  instead, where its items are.
- **Ceiling** — `max_messages` (var, default 5, `0` = none) bounds one
  digest. It is reached only by an abnormal synthesis; the last message
  then carries the truncation notice, which is the only case left where
  a digest is cut at all.
- Incoming webhooks return no post id, so the parts **cannot** be
  threaded. They are ordinary consecutive posts; ordering comes from
  sequential blocking POSTs plus a 0.3s pause between parts (two posts
  created in the same millisecond can render out of order).
- A digest that fits is one message with **no marker** — unchanged.

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
  resume (duplicates are worse than a visible partial). The queue is
  consumed all the same, so a sink that is broken rather than flaky
  loses one digest per run, quietly, until someone reads a summary.
  Re-post from `<state_dir>/<category>/digests/*.md` once it is fixed.
- **partial failure WITHIN a sink** (parts 1–2 of 3 delivered) — that
  sink stops there: posting part 3 over a missing part 2 leaves a hole
  no reader can detect. It does not count in `delivered` (which means
  *sinks that got the whole digest*) and the failed part is named in
  the summary (`w1 part 2/3: …`).
  When NO sink got the digest whole, `posted` is false, so the queue is
  **not** consumed: the run still finishes, the items stay pending, and
  the next digest re-sends them — duplicating the parts that already
  landed. A visible duplicate beats a silent permanent drop, which is
  what consuming the queue on a half-delivered digest would be. A
  stderr line names the shape when it happens.
- **total failure** (not one POST accepted, anywhere) — the run FAILS
  (failed_resumable). Nothing was delivered, so `iterion resume` is
  safe and will re-attempt delivery. The guard is the `posts` count,
  **not** `delivered`: once a digest can span several messages, "no
  sink got all of it" no longer means "nothing was posted", and
  resuming there would re-post what already landed.

## Troubleshooting a missing digest

1. `iterion report --run-id <id>` → check the `notify` node output:
   `delivered` (sinks that got it whole), `parts` (messages the digest
   occupied on the sink that needed the most of them — the narrowest
   budget; `1` = it fit whole everywhere), `posts` (POSTs accepted),
   `targets`, `summary` (failures are spelled out, part by part).
2. Queue was empty? A digest with nothing pending ends at
   `load_pending` (`has_items=false`) — by design, no empty digests.
3. Webhook 4xx: URL revoked/wrong (`iterion secret set webhooks …`
   again), or the channel override is not permitted by the Mattermost
   integration settings.
4. Webhook 404 on ONE sink while its siblings deliver: the channel is
   gone under that name. `channel` matches a Mattermost channel's URL
   handle, not its display name — renaming the handle silently breaks
   every sink pointing at the old one, and renaming only the display
   name changes nothing. Retarget the sinks in the config.
5. The digest itself is always recoverable from the run artifacts
   (`synthesize` output) and `<state_dir>/<category>/digests/*.md`.
