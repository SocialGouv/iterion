# ADR-170 — An issue trigger is a re-armable lane: its claims form a sequence, and an event is known by the forge's delivery id

- **Status**: Accepted — the operator decided it on 2026-11-18, after the change that implements it landed (#3373, PR #3389, `e4bdfc52e`).
- **Date**: 2026-11-18
- **Deciders**: jo (the decision), Claude (the design and the implementation), codex gpt-6-sol (three adversarial review rounds)
- **Relates to**: [ADR-034](034-webhook-dual-mode-auth-token-header-vs-body-hmac.md) (webhook authentication) · [ADR-138](138-forge-provisioning-immutable-identities.md) (provisioned hook identities) · [ADR-168](168-an-event-source-is-a-connector-package.md) (its dedup identity for connector deliveries is the signed body; this record states the forge-issue counterpart)
- **Serves**: [#3373](https://github.com/SocialGouv/iterion/issues/3373) · follow-ups [#3386](https://github.com/SocialGouv/iterion/issues/3386) and [#3387](https://github.com/SocialGouv/iterion/issues/3387)

## Context

A label applied to a forge issue (and GitHub's zero-touch `opened`) wakes the bot an integration binds to that label. Until #3373 the server kept **one launch per (tenant, webhook, repository, issue, trigger), forever**:

- the direct lane keyed its delivery row on that tuple — "the SAME label replays no-op" (`pkg/server/webhooks_github.go`);
- the board lane, when a dispatcher owns execution, answered `carded` whenever any card carried `cmd:issue:<n>`, whatever its column (`boardCardExists`, `pkg/server/invocation_dispatch.go`). That label was designed as a per-*comment* key; reused for issue subjects it became a lifetime lock.

Re-applying the label — the natural way to ask again after the issue was edited, the first run failed a gate, or the fix was reverted — was dropped silently, with no escape hatch. `docs/philosophy.md`: a bound with no override is a defect. Measured on iterion.cloud on 2026-11-18: a second `repro` label on the same issue launched nothing.

Two promises pull against each other. A **redelivery** of the same forge event (GitHub *Redeliver*, a GitLab retry or *Resend request*) must stay a no-op and never charge the quota twice. A **fresh application** of the label is a new request and must launch once the previous one has ended — without two runs serving the same request at once.

## Decision

### 1. An issue trigger is a lane, and its claims form a sequence

A lane is `(repository, issue, trigger)`. Its claims are decided in `dispatchInvocation`, the one point both the direct and the board lane pass through, before anything is metered. Comment commands pass no lane: a comment is unique, and their path is unchanged.

- **Direct lane** — the n-th claim is the delivery row under `sha256(<base>|gen|n)`. Generation 0 is the handler's historical key, so rows written before this record count as first claims. The store's unique insert decides a race for the next generation.
- **Board lane** — lane cards carry `cmd:lane:<project>#issue:<n>#<trigger>` and a deterministic id, `uuidv5(tenant, lane, sorted ids of the lane's cards)`, so two racing creates collide on the id. A card from before this record counts toward every lane of its issue, scoped to its project when its launch arguments record one.

A claim is **active** while its run or card is non-terminal (or its launch is uncertain, or accepted but never booked); a fresh event then is **absorbed**. A claim that ended — run terminal or gone, launch exhausted, card in a terminal column — frees the lane: the next event takes the next generation or the next card id. `overlap: supersede` keeps its meaning on the direct lane: the serving run is cancelled by the existing supersede pass.

### 2. An event is known by the forge's delivery id, or by its body

An event's identity is the forge's own delivery id when it sends one — GitHub `X-GitHub-Delivery`, GitLab `webhook-id` / `Idempotency-Key`, which GitLab keeps across retries and *Resend request* — persisted as `webhooks.Delivery.ForgeDeliveryID`; otherwise the sha256 of the body. When both sides carry an id, the ids decide alone; the hash decides only when one side has none (rows from before this record).

Every event the lane does not launch for leaves a record under its own identity — a `duplicate` delivery row for an absorbed event, a marker label on the card it created — so its redelivery stays a duplicate after the serving claim ends. The identity survives a durable retry (the sweeper carries `ForgeDeliveryID`), an event displaced by a takeover of a failed claim is recorded as absorbed once the takeover holds, and the loser of a race for a free lane is recorded too (the launch tail reports its claim decision to an optional observer).

### 3. An unreadable lane refuses, retryably

If the delivery store, the run store or the board cannot be read, the delivery answers **503** with a `launch_error` audit row under a random key, so the forge redelivers and the lane decides with a readable store. Board mode with no board store, or a failing card create, answers 503 instead of a silent `carded`.

## Consequences

- A person can ask again by re-applying the label; nothing else changes for the forge or the bot.
- In board mode an issue's card no longer blocks a *different* label on the same issue, nor the same issue number in another repository — board mode now matches direct mode.
- Absorbed events leave `duplicate` rows; the studio already renders that status.
- `webhooks.Delivery` gains one field (`forge_delivery_id`); no migration — older rows match by body hash.

## Alternatives rejected

- **Release the subject key once the claim has ended**, and nothing else: a redelivery of an *old* event would then launch again.
- **Key every claim on the delivery alone**: two fresh labels applied in quick succession would run concurrently.
- **A time window** ("same label within N minutes is a replay"): a guess about intent, wrong both ways, and a bound with no override.

## Assumed limits (stated in `docs/webhooks.md`)

- Deleting a lane card forgets its event: a redelivery of that event cards again. It is the manual re-arm gesture.
- Without a dispatcher, a board card is a tracking record, not a claim; a dispatcher enabled later finds such a card serving until it is closed.
- Renaming a GitLab project while a card serves starts a second board lane (the board lane names the project path, the direct lane the project id).
- A claim stranded `accepted` or `launch_uncertain` by a crash keeps its lane locked, as before.

## Not decided here

- [#3386](https://github.com/SocialGouv/iterion/issues/3386) — the loser of a takeover race whose read-back fails is answered 200 duplicate and recorded nowhere; the fix (a 5xx) touches the shared launch tail.
- [#3387](https://github.com/SocialGouv/iterion/issues/3387) — Forgejo has no issue-label trigger; when it gets one, it joins this lane.
- `overlap: supersede` on the board lane.

## Witnesses this record owes

`pkg/server/webhooks_trigger_lane_test.go`, one scenario per promise on both lanes and both forges, each seen red under its mutation (41 mutants, matrix in the commit of #3373): `Direct_ARedeliveryAfterTheRunEndedStaysADuplicate`, `Board_ARedeliveryAfterTheCardEndedCardsNothing`, `ARedeliveryIsKnownByItsDeliveryID`, `Direct_AFreshLabelAfterTheRunEndedLaunchesAgain`, `Board_AFreshLabelAfterTheCardEndedCardsAgain`, `Direct_AFreshLabelWhileTheRunServesIsAbsorbed`, `Board_AFreshLabelWhileTheCardServesIsAbsorbed`, `Direct_TwoFreshLabelsRacingForAFreeLaneLaunchOnce`, `Board_TwoFreshLabelsRacingForAFreeLaneCardOnce`, `Direct_ADurableRetryKeepsTheEventIdentity`, `Direct_TwoDeliveryIDsAreTwoEventsWhateverTheBytes`, `ACommentCommandIsClaimedOnce`, `AnUnreadableLaneRefusesTheDeliveryRetryably`.

## Adversarial review disposition

Three rounds by codex gpt-6-sol (read-only; findings verified against the source before acting):

| Round | Finding | Severity | Disposition |
|---|---|---|---|
| 1 | a durable retry erased the event identity | high | fixed (§2) |
| 1 | equal bytes merged two delivery ids | medium | fixed (§2) |
| 1 | deleting a card forgets its event | medium | assumed limit |
| 1 | tracking cards without a dispatcher dedupe by subject | medium | assumed limit, documented |
| 2 | the loser of a race was answered duplicate without a record | high | fixed (§2) |
| 2 | a displaced event was recorded before the takeover held | medium | fixed (§2) |
| 2 | a GitLab rename starts a second board lane | medium | assumed limit |
| 3 | a failed read-back of a race winner records nothing | medium | not decided here (#3386) |
