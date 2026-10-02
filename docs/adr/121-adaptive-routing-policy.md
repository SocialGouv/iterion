# ADR-121: adaptive routing — a multi-level policy that picks the (harness, credential) pair, at launch and mid-run

- **Status**: Proposed (design review — another model family, then the operator)
- **Date**: 2026-10-02
- **Authors**: Claude (draft), Jo (arbitration pending)
- **Extends**: [ADR-087](087-cross-backend-model-fallback-chain.md) (cross-backend fallback routes), [ADR-091](091-fallback-skip-route-and-plan-peer-review.md) (`action: skip`, `when:` gates), [ADR-090](090-shared-tier-credential-policy.md) (shared-tier ordering), #1956 (route-aware shared tiers)
- **Serves**: epic [#2000](https://github.com/SocialGouv/iterion/issues/2000), slice [#1999](https://github.com/SocialGouv/iterion/issues/1999)
- **First delivery only**: the policy foundation, launch-time harness selection, and same-harness mid-run fallback. Cross-harness relaunch with transcript handoff is the next delivery on this foundation (§ Out of scope).

## Context

Today a run finds its credential through pieces that do not compose. A bot author may hand-declare per-node `fallbacks:` routes (ADR-087) and a run-level `--fallback` chain; the delegates hold a hardcoded precedence inside their own family (claude_code: its bundle default, then a hinted key, then the forfait); the shared tiers fill wires in an order two knobs order (#1956); the facade policy (#2096) decides whether a z.ai key may be the wire's default at all; the pool is a last resort with a full-preference order; and when every tier abstains, the run either parks on a usage window or fails at its first LLM call.

What no piece does: **follow the credentials the run actually holds**. A run whose tiers hold only an OpenAI key still starts its `claude_code` nodes and dies at the first call; a run whose Claude forfait closes mid-run parks, while a z.ai key sits sealed in its own bundle able to serve GLM on the same session. Making that work by hand costs a `fallbacks:` chain per node, per bot, per repo — knowledge the operator, not the author, holds (which vendor is paid for, which is capped, which model answers a claude id).

The operator's decision (2026-09-30, on epic #2000): routing becomes a **policy, configurable at every level, automatic by default**, and the harness follows the credential.

## Decision

### 1. One policy object, five levels, locks

`RoutingPolicy` is a record whose fields are each independently settable and independently lockable. It lives at five levels; resolution folds from the broadest to the most specific, most specific wins, and **a field marked `locked` at any level stops the descent for that field** — the one primitive cost governance has.

| Level | Where it lives | Written by |
|---|---|---|
| platform | the platform-credentials settings record (`platformcfg`), env default | the deployment operator |
| org | a new org settings record | the org admin |
| team | a new team settings record | the team admin |
| bot | the bot manifest (`routing:` block), shipped with the bot | the bot author |
| run | launch vars (`--routing-*`) / the launch spec | whoever launches |

Fields of the first delivery:

- **`pair_order`** — the ordered list of `(harness, credential)` pairs a run may occupy: the default is `claude_code+claude_forfait`, `codex+chatgpt_forfait`, `claw+anthropic_key`, `claw+openai_key`, `claude_code+zai_key`, then the rest. Any level may reorder, prune or extend it.
- **`triggers`** — which ADR-087 failure categories may fire a fallback: `usage_window`, `unavailable`, `transient_exhausted`, `auth` by default; `budget` and `schema` are excluded always (they re-fail identically on every route). Any level may prune; never extend past the platform's set.
- **`model_classes`** — model *classes* (`top`, `standard`, `fast`) mapped to a concrete model per family (`top: {anthropic: claude-opus-5-5, openai: gpt-6-sol, zai: glm-5.3}`). iterion ships defaults; any level overrides a class per family. A route whose model names a class resolves to the concrete model of the family it lands on.
- **`refused_pinned_key`** — `park` (default, #1999's status quo) or `forfait` (a refused pinned key stands down; its routes fall through to the same-provider forfait). Exactly #1999's setting, expressed in the policy so it resolves per level like everything else.
- **`strict`** — marks a node's `backend:` pin (or the whole workflow's) as a requirement instead of a preference (§2). An author writes `strict`; a level may set it where the author did not; a level above the author may not unset it.

Compatibility with what ships today: `facade_default` (#2096) stays, and composes — the pair order says whether a z.ai key may serve; `facade_default` remains the per-wire veto on it being a route's DEFAULT. `park|forfait` for refused pinned keys becomes `refused_pinned_key` at the platform level when #1999 lands; until then #1999 ships its own setting and the policy folds it in.

### 2. Launch-time: the harness follows the credentials the run holds

At the entry of the credential resolution — the same seam #2038's predicate and #1998's probes now occupy — after the tier walk seals what the run holds, the resolved `RoutingPolicy` picks the `(harness, credential)` pair: the first pair in `pair_order` whose credential the bundle actually carries. The selection then expresses itself the one way the executor already honours without a new mechanism: **launch-time overrides**. A node whose routes are re-targeted gets a computed provider/backend override; an author's `backend:` pin is honoured as a preference (re-target only when the pinned harness holds no credential and a later pair does) unless `strict`.

A run that acquires no credential at all (env-funded, #2038) selects nothing: the env is the harness. A run that holds several families gets the first pair in its order — determinism over cleverness; the order is the knob.

### 3. Mid-run, same harness: the credential follows the window

When the serving credential closes mid-run (`usage_window` — window shut, provider refusal, operator cap), today's answer is a park: the usage-window retry arms on the forfait's reset and the run waits. The policy adds the automatic alternative, within the same harness: fall through to the **next compatible credential** — a z.ai key already sealed serving GLM on the same claude_code session, then the pool's donations marked fallback-usable (last resort, never before the run's own tiers).

Mechanism, not new: the delegates' precedence already walks a bundle's credentials per family; what changes is that a window-closed credential is **re-ordered at re-resolution** instead of left first, and the run's retry arms on the REPLACEMENT's window when one exists, on the closed one's when none does. Every switch emits the audit line ADR-087 routes already emit (which credential stopped serving, which took over, which trigger), and the usage ledger attributes the spend to the credential that served — no invoice moves in silence.

### 4. What deliberately does not change

- Tenant isolation: the policy reorders what the walk may consult; it never lets a tier read another tenant's credential. The fill, restore and preview keep their parity property (#1956, #1998, #2038 each carry it) — a policy resolved once per launch applies the same answer to the fill, the platform stage, the restore and the preview.
- Author intent: `fallbacks:` chains declared by hand remain authoritative for the nodes that declare them; the policy synthesizes nothing where the author already wrote a route. `strict` is the author's and the operator's tool to say the pin was the point.
- Budget and schema failures never fall back: they re-fail identically on every route.

## Alternatives considered

- **Per-node policy in the DSL** (`routing:` on each node): maximal author control, and exactly the per-bot per-repo duplication the epic exists to remove; rejected as the primary mechanism, kept as `strict`'s spelling.
- **Synthesizing `fallbacks:` routes at compile time**: reuses ADR-087 verbatim, but freezes the policy at authoring/compile time — the levels, the locks and the credential state at launch could not act. Rejected: the policy must resolve at launch and re-resolve mid-run.
- **A dedicated routing service deciding per call**: a second brain beside the executor, with its own view of the credentials — the divergence risk #1956 closed (fill, restore and preview disagreeing) rebuilt at the routing layer. Rejected: the decision stays in the resolution, read once per launch.
- **Doing nothing**: the per-bot `fallbacks:` authoring cost is what kept the capability from landing twice before.

## Consequences

- **Cost is automatic by default** — every allowed fallback is attempted, a subscription may spend as extra usage (claw on a Claude forfait), and the governing tool is the lock, not an opt-in. This is the operator's explicit stance; the ADR records it as such.
- **Every switch is said**: audit lines at launch selection and at each mid-run switch, the pair in the run document, the spend on the serving credential. A run that switched is legible after the fact.
- **The five levels are five stores and a fold** — four of them new (org, team, bot manifest block, run spec fields), each with its writer and its read path; the platform level rides the settings record the facade and #1999 knobs already use.
- **Model classes add a vocabulary** that must ship defaults and accept per-level overrides without a migration: a class the policy does not map, on a family with no mapping, resolves as today (the route stays as written) and warns.
- **Testing obligation**: the resolution is deterministic and pure — policy fold tests, pair-selection tests against seeded bundles, mid-run switch tests against window states, and the parity property (fill/restore/preview agree) asserted for the policy-resolved answer exactly as #1998 and #2038 carry theirs.

## Open questions for the design review

1. `pair_order` names pairs concretely (`claude_code+claude_forfait`) — should a deployment be able to name abstract slots (`forfait+harness`) resolved per held credential, or is the concrete list enough?
2. Does the **run** level get to SET policy, or only to choose among what the levels above allow? (A launch flag that could unlock a platform lock would make the lock decorative.)
3. Should `model_classes` be a separate record shared with the cost estimator, or a field of this policy?
4. Pool donations "marked usable for fallback" — a new donor-facing field, or a pool-level policy switch?
5. Where mid-run re-resolution reads the replacement's window from — the usage ledger's own readings, or a fresh provider probe?

## Out of scope (delivery 2)

Cross-harness relaunch with transcript handoff: a node whose only remaining credential belongs to another harness is relaunched on that harness, the new agent reading a harness-neutral transcript rendered from the run's own events and extracting the work already done (economical) or restarting clean (costlier). It sits on this foundation — the pair order, the triggers and the levels are its vocabulary.
