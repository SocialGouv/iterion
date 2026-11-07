# ADR-163 — The gateway price feed: operator pricing syncs into the database and rides the existing IPC; the operator table stays the floor

- **Status**: Accepted — the operator decided it on 2026-11-07 (architecture choice, option A, over a gitops bot regenerating the env table and a runner-side feed)
- **Date**: 2026-11-07
- **Deciders**: jo (architecture), Claude (options, analysis)
- **Relates to**: [ADR-122](122-gateway-models-resolved-exactly.md) (the gateway catalog and its pricing) · [ADR-131](131-tenant-gateway-credential.md) (gateway routes) · [ADR-161](161-one-resolver-decides-the-credential-a-call-spends.md) (cost lands on the credential that served)
- **Serves**: [#3105](https://github.com/SocialGouv/iterion/issues/3105); proof chain #3099 → #3103 → v5.12.2

## Context

The gateway cache-cost parity chain (#3099, #3103) proved metering, pricing and provenance end to end: the operator table (`OPENAI_COMPATIBLE_MODELS`) drives every price a gateway run is metered at, and the `_gateway_spec` record states which table answered. But the table itself is a **manual mirror** of operator tariffs (Scaleway EUR × 1.12 → USD at the time of writing). Nothing resynchronizes it. When an operator changes a price, the drift is silent: runs keep metering at the old rate until a human edits a gitops values file, rolls the chart and restarts runners.

The catalogue already has every seam the feed needs:

- `modelspecs.Registry` — a URL-overridable fetch (`ITERION_MODEL_SPECS_URL`), a `Refresh`, a disk cache, an embedded snapshot fallback: the "refreshable remote table" pattern exists.
- `compatgw.resolveCatalog` — a source ladder with provenance attached to every answer: host IPC record → operator table → catalog-live → catalog-snapshot.
- The IPC record — the host's resolved catalog answer already rides to the runner in `ResolvedEnvRecord`; agreement across the IPC beats any local read.
- The override cascade — provider/model configuration already lives in the database at global/org/team/bot/bot-in-team/binding levels (ADR-131's ladder). A price table is configuration of the same kind.

## Decision

1. **Where the feed lives.** A price feed is server-side, database-backed state at the org/team level of the existing override cascade — the same ladder that already carries provider/model configuration. Runners do not fetch prices: the server resolves the effective table (feed entry if fresh, else the operator floor) and the answer reaches the runner through the IPC record it already reads. No new runner egress, no per-runner freshness drift, multi-replica safe by construction.
2. **Refresh.** The server refreshes each enabled feed on a configurable period (per feed: on/off, interval, timeout), under elected ownership — one replica reconciles, restart is normal, the reconciliation is idempotent. A feed that has not refreshed successfully inside its freshness window is **stale**, and staleness is a first-class state, not an error.
3. **Precedence — the operator table stays the floor.** A fresh feed entry answers before the operator table for the ids it carries. A stale or failing feed steps down with a warning, never a refusal: the operator table answers, then the ladder as today. Philosophy 1 — a bound with no override is a defect, and an operator's explicit table is never silently overridden by a dead feed. Drift the other way is surfaced: when a fresh feed disagrees with the operator table, the warning names the id, both values and the source.
4. **Pluggable sources.** A feed is config naming a source type plus its parameters; source types are adapters behind one seam (the Nth-variant test): `scaleway`, `litellm`, `models.dev`-shaped JSON, and an `exec` adapter running an operator-supplied command that prints the table on stdout — a new operator is configuration (or a small script), never an engine `if`.
5. **Manual sync.** An operator can force a refresh of one feed or all feeds through the API and `iterion remote`; the result reports what changed (per id: old → new, source, freshness).
6. **Provenance rides along.** The `_gateway_spec` record gains the feed's identity and age when a feed answered (`source: "feed:<name>"`, refreshed-at), so the record that already answers "WHICH table priced this run" also answers "HOW fresh was it" — the audit trail the #3103 fix established is extended, not forked.
7. **Cloud twin in the same change.** The feed's durable state, its elected reconciler and the admin endpoints ship together with the single-process implementation; a feed configured on a replica deployment converges the same as on one process.

## Consequences

- **What becomes automatic**: operator price changes reach metering within one refresh period, with a named provenance trail; an operator's manual table remains the authoritative floor and the offline answer.
- **What stays manual**: wiring a *new* operator still means writing its adapter parameters (or script) — the plug is config, the shape of the data is the operator's.
- **Rejected alternatives**:
  - *A gitops bot regenerating `OPENAI_COMPATIBLE_MODELS` in the appsets repo* — git-native and reviewable, but the table stays frozen between rolls (a git cycle plus an ArgoCD rollout is hours, not minutes), and it covers only the operator-table slot. Kept as a possible later complement for change *review*, not as the sync mechanism.
  - *A runner-side feed tier in `resolveCatalog`* — smallest diff, but N runners fetch in parallel, sandbox egress must open, config lives in env rather than the database, and freshness varies per runner — the IPC record exists precisely so the host's answer beats any local read.
- **Watch**: feed failures must be loud (one warning per feed per staleness transition, not per dispatch), and the drift warning must not spam — one per id per freshness window.


## Amendment 2026-11-07 — the delivery's channel and bounds

The implementation this ADR's ticket shipped (#3105) clarifies three
points the decision left to the delivery:

1. **The channel.** "The answer reaches the runner through the IPC
   record it already reads" (§1) lands as TWO carried things: the run
   BUNDLE gains `GatewayPriceFeed` — the folded table as bytes the
   server sealed, an older runner decoding the bundle unchanged — which
   the runner plants on the run's environment
   (`ITERION_GATEWAY_PRICE_FEED`, host-written, refused when planted)
   for the catalog to read between the IPC record and the operator
   table; and the per-node IPC RECORD (`_gateway_spec`,
   `ITERION_OPENAI_COMPATIBLE_RESOLVED`) still carries the dispatched
   id's resolution, now naming `feed:<name>` with its `refreshed_at`
   (§6 verbatim).
2. **Freshness is judged once, at the fold.** The server resolves the
   effective table (§1) — fresh entries only; the runner's readers
   freeze the verdict per table value, so the stamp at dispatch and
   every later meter (the node's bill, the org cap, the credential
   ledger) read ONE answer for one invocation, ADR-122's
   "never read again in mid-flight" held at the feed rung. A resumed
   run is folded anew.
3. **The `exec` source is the platform operator's** (§4's "an
   operator-supplied command" made precise): a tenant admin cannot
   configure one (super-admin only on the write) and it executes only
   where the deployment exports `ITERION_PRICE_FEED_EXEC=1`. The HTTP
   sources dial through the deployment's public-unicast guard
   (`ITERION_LLM_ENDPOINT_ALLOW_PRIVATE`, the existing LLM-endpoint
   hatch). §3's drift bound and §2's staleness are unchanged.
