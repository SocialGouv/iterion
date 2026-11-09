# Key pools — named, attachable per scope, forfait-first fallback

Who a run borrows credentials from when its own do not fill every slot:
named pools (forfaits + gateway keys) attached at a cascade level, with a
switchable strategy. [ADR-164](../../adr/164-configurable-key-pools-attach-per-scope-with-a-fallback-strategy.md)
is the decision; this page is the operator and debugging map.

## The four laws — read before touching anything

1. **An explicit credential beats a pool entry.** Pin, team key, org key,
   personal key: a pool fills only EMPTY slots, never displaces.
2. **Zero pool without an opt-in.** No attachment at the level considered
   = today's legacy behavior, byte for byte. `Enabled: false` BLOCKS
   inheritance at that level; delete = inherit again.
3. **Sovereignty intact.** `llm_fallback=none` + a repo pin + an
   attachment present resolves as if the pool did not exist — pinned by
   test, bit for bit.
4. **The strategy is per attachment and switchable**: mode
   (priority / least_consumed), margin % (nil = 85), triggers (the
   llmroute subset, platform-narrowed), reset_hold, borrower draw caps.

## Surfaces

- **API**: pool CRUD `/api/me/pools`, `/api/orgs/{id}/pools`,
  `/api/admin/llm/pools`; entries
  `/api/pools/{id}/entries` (+ `/import`, `/{pledge}/reset`);
  attachments `/api/admin/llm/pool-attachment`,
  `/api/{orgs,teams}/{id}/pool-attachment`,
  `/api/teams/{id}/bots/{bot}/pool-attachment` (CAS 409 on a stale
  `expected_updated_at`).
- **CLI**: `iterion remote pools list|create|entries|entry|import|reset|attach|detach|sync`
  — scopes: yours by default, `--org`, `--team`, `--platform`.
- **Studio**: the org and team "Key pool" tab — the attachment editor and
  the entries' derived status.

## Authoring an attached pool (the onboarding recipe)

```
iterion remote pools create --name "dose gratuite"
iterion remote pools import --pool <id> --kind claude_code \
  --file ~/.claude/.credentials.json --priority 0
iterion remote pools entry --pool <id> --source api_key \
  --ref openai_compatible --key-id <pliage-key> \
  --model openai_compatible/scaleway/deepseek-v4.1-flash --priority 5
iterion remote pools attach --pool <id> --org <org> --mode priority
```

Forfaits ride first (priority 0 beats 5; equal priorities keep forfaits
before metered keys), the paid gateway key is the LAST rung. `sync`
runs one signal pass inline — the proof the readings are warm.

## What switches, and when

- **Proactive** (claude_code subscriptions only today): the launch reads
  the shared usage ledger; an entry at or past the margin with a future
  reset is skipped PER CANDIDATE — the next entry serves, and the run's
  retry arms at the reset. The `pool-signal-sync` lease warms those
  readings every 5 minutes.
- **Reactive** (keys, gateway, zai/moonshot): the provider's own refusal
  sets `CooldownUntil` from its `ResetAt`; no margin exists, and the
  preview says "reactive — no provider telemetry". Reset by hand:
  `pools reset --pool <id> --entry <pledge>`.
- **In-run**: the entries' rungs ride the run's fallback chain
  (`On:` the attachment's triggers) — a usage-window failure walks
  forfait → next entry → gateway, exactly the composed order.

## Debugging

- **The pool never serves**: is there an attachment at the resolved
  level? (`GET .../pool-attachment` per level — absent = inherit.)
  `Enabled: false` anywhere between the launch and the platform floor
  BLOCKS. Is the pool `Enabled`? Private pools answer only their
  attachment.
- **A slot never draws**: an explicit tier sealed it (law 1), or the
  routing whitelist holds the slot out — the credential preview shows the
  per-slot answer the launch will produce.
- **A gateway entry refuses**: it needs the verbatim `model` at pledge
  time, and its grant carries the paired URL; a URL-less grant is
  refused and its lease released (the log names it).
- **Borrower caps**: the `att:` ledger row books per attachment scope —
  a cap refusal reads "exhausted" in the abstention, NOT the donor's
  exhaustion.
- **Sovereign teams**: the attachment is inert there, warned at launch
  and in the preview. That is law 3 working, not a bug.

Full contract: [ADR-164](../../adr/164-configurable-key-pools-attach-per-scope-with-a-fallback-strategy.md);
the legacy donation tier: [credential-pool.md](../../credential-pool.md).
