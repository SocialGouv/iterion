# ADR-121: adaptive routing — a multi-level policy that picks the (harness, credential) pair, at launch and mid-run

- **Status**: Arbitrated (2026-10-02 — the operator's four decisions recorded in § Arbitrated; the design ticket gates implementation)
- **Date**: 2026-10-02
- **Authors**: Claude (draft), Jo (arbitration recorded 2026-10-02, § Arbitrated)
- **Extends**: [ADR-087](087-cross-backend-model-fallback-chain.md) (cross-backend fallback routes), [ADR-091](091-fallback-skip-route-and-plan-peer-review.md) (`action: skip`, `when:` gates), [ADR-090](090-runtime-operational-settings-db-backed.md) (db-backed operational settings — the record the facade and keys-first knobs ride), #1956 (route-aware shared tiers)
- **Serves**: epic [#2000](https://github.com/SocialGouv/iterion/issues/2000), slice [#1999](https://github.com/SocialGouv/iterion/issues/1999)
- **First delivery**: the policy foundation, launch-time harness selection, same-harness mid-run fallback, and the per-family pool fallback door (§ Arbitrated 4). Cross-harness relaunch with transcript handoff is the next delivery on this foundation (§ Out of scope).

### Context

Today a run finds its credential through pieces that do not compose. A bot author may hand-declare per-node `fallbacks:` routes (ADR-087) and a run-level `--fallback` chain; inside one family the delegates hold a precedence that is easy to mis-state and matters here in full: a `provider:` hint overrides everything; without a hint the anthropic wire is walked `zai → moonshot → anthropic → forfait → env` (the forfait passes a key only for a **pinned** same-provider route — `RegisterForfaitFirst`); the walk is over a bundle **sealed at launch** — it has no notion of a window state. The shared tiers fill wires in an order two knobs decide (#1956); the facade policy (#2096) decides whether a z.ai key may be the wire's default; the pool is a last resort with a whole-bundle gate ("only for a run that has no credential of its own at all"); and when every tier abstains, the run either parks on a usage window or fails at its first LLM call. Host-side `ITERION_BACKEND_PREFERENCE` is detection, not a run's sealed credentials.

What no piece does: **follow the credentials the run actually holds**. A run whose tiers hold only an OpenAI key still starts its `claude_code` nodes and dies at the first call; a run whose Claude forfait closes mid-run parks, while a z.ai key sits sealed in its own bundle. Making that work by hand costs a `fallbacks:` chain per node, per bot, per repo — knowledge the operator, not the author, holds.

The operator's decision (2026-09-30, on epic #2000): routing becomes a **policy, configurable at every level, automatic by default**, and the harness follows the credential.

## Decision

### 0. The form: a per-field fold, on the retrypolicy pattern

`RoutingPolicy` resolves exactly the way `retrypolicy.Resolve` already does (field by field, the first level that sets a field wins, a **provenance map** answers "which level decided this", and the platform level can only **lower** what the levels below may do — on `triggers`, the platform ceiling prunes and never extends). The chain runs platform < org < team < shipped bot < bot binding < run: the binding outranks the shipped bot because deployment context decides what shipped content may spend on THIS tenant. List fields carry their own merge rule, stated per field in §1 — a reordered prefix of a longer list being a semantics trap, `pair_order` **replaces** at the highest level that sets it rather than merging. A field marked `locked` at a level stops the descent for that field: the one primitive cost governance has. The resolved policy is snapshotted onto the run document at launch, as retrypolicy already snapshots.

**Levels and sequencing.** Five sources, and TWO of them are about the bot, on purpose:

- **platform** — the settings record the facade and keys-first knobs already ride;
- **the shipped `.bot`** — a `routing:` block in the manifest (orchestration, not workflow semantics — the same rule ADR-087's alternative 6 set for `retry:`). Written by the AUTHOR, who may have tuned and tailored the bot to a model: this level is settable **and lockable by the author**;
- **the bot binding** — the provisioned instance of that bot on a team/repo. Written by whoever provisions it, it overrides every UNLOCKED field of the shipped bot: deployment context outranks shipped content, and the author's lock is what stops it (the same gesture as `strict`). it rides the EXISTING provisioning records — no new collection. *(Carrier amended 2026-10-04, slice 2: the "bot-bindings store" named here is in the code the bot-SECRET-binding credentials record, which holds no policy; the binding level rides the launch-surface records the retry chain already called binding-level — the schedule row, the trigger subscription, the webhook config — so one bot on one team may hold one binding policy per provisioning surface, and each launch reads the record that launched it.)*
- **run** — launch fields;
- org and team are **delivery 2**: they are greenfield — `identity` records are directory entries, admission lists are not settings, and each level means a new collection, API routes, an RBAC answer to "who is an org admin", tenancy threading, CAS and preview support. Named, costed, sequenced — not smuggled into "four of them new".

Fields of the first delivery:

- **`pair_order`** — the ordered list of `(harness, credential)` pairs a run may occupy, named per FAMILY (the instance follows the tier walk: `claw+anthropic_key` does not name WHICH anthropic key — the shared order and the walk do, which is what keeps fill/restore/preview parity and per-fingerprint audit). Default: `claude_code+claude_forfait`, `codex+chatgpt_forfait`, `claw+anthropic_key`, `claw+openai_key`, `claude_code+zai_key`, then the rest. A level replaces it.
- **`triggers`** — a subset of the closed vocabulary the fallback machinery already classifies: `usage_window`, `unavailable`, `transient_exhausted`, `auth`. `budget`/`schema` were never categories and are not offered; `unclassified` and `any` are excluded by rule — per ADR-087, a non-classifiable failure advances a hand-declared chain but never fires a policy switch, and when the proof is flattened text (`UsageWindowInFlattenedError` is the retry side's carve-out) the policy side inherits exactly that carve-out or fires nothing. The platform ceiling prunes; no level extends.
- **`model_classes`** — an extension of the MODEL REGISTRY, not a field of this policy (the class is consumed outside routing: the cost estimator bills per model, spec validation, display). The policy references it. Classes (`top`, `standard`, `fast`) map to a concrete model per family; iterion ships the defaults in the registry; a level overrides a class per family; a class that resolves nowhere on its family leaves the route as written and warns — and its switch effects (stamps, cost, the C173 analogue) follow the registry's rules.
- **`refused_pinned_key`** — `park` or `forfait`; `forfait` is the arbitrated default (§ Arbitrated 1, 2026-10-02 — reversing #1999's `park`), which remains a settable and lockable value. Migration: #1999 ships its knob AS the platform-level policy field (one source, never two), and #1999's carve-out travels with the field — a facade key pinned beside a Claude forfait is out of its scope, and the fold may not pair what the carve-out excludes.
- **`strict`** — marks a node's `backend:` pin (or the workflow's) as a requirement instead of a preference (§2). An author writes it; a level may set it where the author did not; no level unsets it — only the launcher, for their own run, through an explicit lock.

`facade_default` (#2096) stays and composes (§1's second step); `ITERION_BACKEND_PREFERENCE` remains host DETECTION and is replaced at launch by the policy's selection — the detector's answer is what the policy decides with, not a sixth level.

### 1. Launch-time: the pair selection happens DURING the resolution, and the program it produces is screened

The pair selection is not a post-walk retarget — a retarget after the walk would seal into channels the executor never reads (a facade key sealed pinned-only is invisible to the default precedence, and a facade hint without a reachable key refuses rather than degrades) and would leave the resolution's derivations (`spendableProviders`, `wantsFor`, `derivePinnedProviders`, `envFunded`, `mayReadWireDefault`, `unfundedPinnedProviders`, the fingerprint stamps, the launch gate) reading a program nobody rewrote. Instead:

1. The resolved policy is computed once, before the tier walk.
2. The tier walk runs, and the pair selection shapes it: the first pair in `pair_order` whose credential a consulted tier holds decides which channels the walk seals into — a facade key serving a selected `claude_code+zai` pair seals as the wire's DEFAULT where the facade policy permits, pinned-only where it does not (§1's second step).
3. The program is rewritten through the SAME screened path `--fallback` already uses: computed routes materialized on the IR by `ApplyRunFallback`'s screen — **agent nodes only** (a judge's verdict is load-bearing and is never re-targeted by policy), every computed stage through the C176/C135 predicates, refusals landing on `run_fallback_refused` as run-level fallbacks do today. A `permission: ask|deny` node is never re-targeted onto a harness that cannot enforce its gate: refused by the screen, said on the event, never degraded in silence.

After that rewrite, every derivation listed above reads the rewritten program — once, with the same answer for fill, restore, preview and launch gate. One reads earlier by design: `envFunded` is computed before the selection, on the program as admitted — what it decides, acquire nothing, is what makes the selection idle. This is the honest version of the alternative the first draft rejected on a false premise: synthesis CAN act at launch, after compile, pruned by what the launch funds — the screen is what makes it legitimate.

The selection is deterministic: the first pair in the order whose credential the run holds. A run that acquires no credential at all (env-funded, #2038) selects nothing — the env is the harness.

### 2. Between attempts: the re-resolution re-picks the pair; in-run, the cooldown ledger already re-orders

"Mid-run" needs its two halves named, because they are different mechanisms:

- **In-run (per node, per dispatch)**: the route cooldown ledger exists — `(backend, provider, model)` keyed, category and `until` recorded, cause preserved for the retry classifiers. A closed window already de-orders a route at dispatch. The policy's `triggers` filter what that ledger may de-order, and this ledger is where a switch's audit line is emitted in-run.
- **Between attempts (per run)**: the only re-resolution is the one `SubmitResume` already performs after a park — the full `resolveAndSealCredentials` replay. §1's selection runs there: the retried attempt spends what the re-picked pair serves. A JetStream redelivery re-plays the original message and SecretsRef and re-resolves nothing — unchanged.

The retry **arming** does not change: `usageWindowRetryAt` remains authoritative from the FAILED credential's terminal proof, `skippedReopensAt` remains a speculative earlier wake refused at the last attempt if the authoritative wall is still reachable, and the last-attempt reservation stands. The switch changes what the retried attempt spends, not when it is armed.

Session semantics at a switch are part of the contract, and the word that decides is BACKEND, not model family: a switch that keeps the session's backend (GLM via z.ai answering a claude id on a `claude_code` session — the vendor family changes, the wire and harness do not) keeps the session and answers GLM — **the deployment default is `facade_default: always`** (arbitrated 2026-10-02: the pair order acts through the facade policy by default; `auto`, `never` and `tier` remain settable and lockable per level, and under them the run parks on its forfait — #2096's mechanism stands, its default value changes). A cross-BACKEND switch evicts the session (ADR-087 §3) and emits the `session_degraded` equivalent (ADR-091). The design ticket owns the full harness × credential × family compatibility matrix — who can serve what, what answers, which session event fires.

Resume classifies every policy field as **identity or volatile**: identity fields are frozen at launch and replayed by the resume (the `PinnedProviders` doctrine — the resume replays the launch's answer), volatile fields (windows, availability) re-resolve. A `pair_order` change between launch and resume moves the harness of a living session only through the session-event contract above, never in silence.

### 3. What deliberately does not change

- Tenant isolation: the policy reorders what the walk may consult; it never lets a tier read another tenant's credential. The parity property keeps its full cast — fill, platform stage, restore, preview, the launch gate (`usagecap` pre-flight judges the routes on what they then spend), `reviewtopology`'s families (injected from the SEALED credentials by design; the policy does not change the topology), and the three route derivations agreeing.
- Author intent: hand-declared `fallbacks:` chains remain authoritative where declared; the policy synthesizes nothing there. `strict` is how a pin stops being a preference. Where a computed route and the operator's `--fallback` chain reach the same node, ADR-087 stage 4's multi-source rule applies (dedup; a route resolving to the call that just failed is dropped) — one composition rule, already written.
- **The pool joins the fallback chain, per family** (arbitrated 2026-10-02): when a policy trigger fires on a wire and no owned credential of the run serves it, the pool is consulted for THAT family — restricted to donations whose donor marked them fallback-usable, leased and ceilinged by the pool's own rules. The whole-bundle gate ("only for a run that holds nothing") remains the rule of the NON-fallback path. This changes the pool's contract by adding a door, and is why the pool stage of the walk is policy-shaped like the others.
- Budget and schema failures never fall back: they re-fail identically on every route.
- On the NON-fallback path the pool stays a whole-bundle last resort ("only for a run that has no credential of its own at all"); the per-family fallback door above (§ Arbitrated 4) is in the first delivery and is the arbitrated exception — the contract change it carries was accepted there.

## Alternatives considered

- **Per-node policy in the DSL** (`routing:` on each node): maximal author control, exactly the per-bot per-repo duplication the epic removes; rejected as the primary mechanism, kept as `strict`'s spelling.
- **Launch-time overrides without the screen** (the first draft's mechanism): computed overrides reach past the C176/C135 screen ADR-087 built precisely so a machine could not do what an author is refused — an ungated node, a tools-less claw node, a permission gate no harness can enforce. Rejected: the synthesis goes through the screen or it does not land.
- **A dedicated routing service deciding per call**: a second brain beside the executor with its own view of the credentials — the divergence risk #1956 closed, rebuilt at the routing layer. Rejected: the decision stays in the resolution, read once per launch.
- **Doing nothing**: the per-bot `fallbacks:` authoring cost is what kept the capability from landing twice before.

## Consequences

- **Cost is automatic by default** — every allowed fallback is attempted, a subscription may spend as extra usage (claw on a Claude forfait), and the governing tool is the lock, not an opt-in. The operator's explicit stance, recorded.
- **Every switch is said**: audit lines at launch selection and at each in-run de-order, the resolved pair and its provenance in the run document, the spend on the serving credential.
- **The fold is pure; the walk is not**: the policy fold is table-testable; the resolution is tested by properties over injected probes — fill/restore/preview/launch-gate agreement, and `spendableProviders`/`wantsFor`/`derivePinnedProviders` reading the same rewritten program.
- **Delivery 1 is platform + the shipped bot + the bot binding + run.** Org and team are delivery 2 with their stores, API, RBAC and audit named as the cost they are. The binding level rides the existing provisioning records (see the § Decision levels carrier amendment).
- **Testing obligation**: fold tables; pair-selection tests against seeded bundles; switch tests against window states; the parity property asserted for the policy-resolved answer; mutation-checked like #1998/#2038.

## Arbitrated (the operator, 2026-10-02)

1. **`refused_pinned_key`: `forfait` is the default.** #1999's `park` remains a settable and lockable value; the epic's default stands. The contradiction between the two 2026-09-30 decisions is resolved for `forfait`.
2. **The facade policy's deployment default is `always`.** The pair order acts through it by default; `auto`, `never` and `tier` remain settable and lockable per level. #2096's refusal of "GLM in silence" is thereby reversed AS A DEFAULT by explicit operator decision — a claude id may be answered GLM wherever the deployment holds a z.ai key and no owned Claude credential, and the audit line says so.
3. **The run level SETS within bounds**: own credentials freely; shared credentials (org, platform, pool) governed by the locks above.
4. **The per-family pool fallback is in the first delivery**, restricted to donor-marked fallback-usable donations; the whole-bundle gate remains on the non-fallback path.

## Open questions resolved by the review (recorded)

1. Pair order is CONCRETE per family in delivery 1; the instance follows the tier walk (abstract slots would rebuild per-tier semantics inside the policy).
2. The run level SETS within the bounds above; locks bind it; the platform ceiling can only lower `triggers`.
3. `model_classes` lives in the model registry, referenced by the policy — the class is consumed outside routing.
4. Pool fallback-usable is a DONOR-facing field (the epic already decided it); the policy reads a rollup. The question is retired.
5. A replacement's window reads the usage ledger first, `forfaitWindowClosed`'s probe on absence — no third source; retry arming is unchanged.

## Out of scope (later deliveries)

Delivery 2 — org and team levels, per-level model classes, cross-harness relaunch with transcript handoff, and the re-dated pool door — has its contract in § Delivery 2 (below).

## Delivery 2 (amended 2026-10-05, slice 1 of #2210)

The § Out of scope items land under this section's contract, which their tests
cite. Shipped when the section says so; described here so each slice's
arbitration is named where the flipped tests point.

### Org and team levels

The chain gains the two greenfield levels: platform < org < team < shipped bot
< bot binding < run. Each level is one policy document per tenant in its own
collection (`llm_routing_policies`, doc ids `org:<id>` / `team:<id>`), with
compare-and-set on `updated_at` from day one and its own validated write
surface (org admin / team admin, audit rows, a 409 on a lost race). The fold
reads them between the bot and the platform layer.

- **Fail-closed, scoped**: an unreadable ORG or TEAM record refuses the launch
  (a read failure must never lift cost governance), and so does a team whose
  org cannot be resolved. The BOT layer keeps its delivered fail-open shape
  (an unresolvable manifest folds to the levels below) — this posture does not
  change it. Absence is not failure: a missing record or an org-less team is
  an absent level.
- **No resolver, no TTL**: the platform record earns its 30-second resolver
  because the publisher polls it per launch and serve-stale is the availability
  trade it chose. The tenant levels are one bounded point read per launch, and
  an outage refuses instead of serving stale — serve-stale would lift
  governance on a blip.
- **Lock doctrine, as delivered** (this is `Resolve`'s semantics, now named):
  a lock at a level vetoes every MORE SPECIFIC level's setter and answers
  at-or-below itself. A team lock does NOT bind the org below it — the org's
  value answers with its own provenance. Only the platform's lock binds every
  tenant-configurable level, because nothing answers below it. The platform's
  triggers stay the ceiling: org and team may narrow, never extend.
- The run snapshot's provenance map gains the `team` and `org` labels; the
  snapshot, the resume replay and the publisher consumption are unchanged.
- Named, not changed: a manual retry of a webhook/schedule run folds the
  RETRYER's tenant policy (the fold is a function of the launching tenant —
  pre-existing shape, the tenant levels give it weight); and a team moving
  orgs between the fold's two reads can fold team:X + org:A once, healing at
  the next launch. No transaction is asked for either.

### Per-level model classes

`model_classes` extends the MODEL REGISTRY's vocabulary (the classes —
`top`/`standard`/`fast` — and the shipped per-family table live in
`pkg/llmroute`, beside the crossing default they generalize: the fold is a
zero-dependency leaf and the table is table-testable because of it;
modelcatalog's display binding is a named follow-up), and each policy level
carries OVERRIDES: a
class → family → model map folded ENTRY-WISE per class×family (the opposite
of `pair_order`'s wholesale replace — the rule is stated per field, here). A
cell that resolves nowhere on its family leaves the route as written, warned
and named at the drop; unknown family or model spellings are accepted on
write and warned at consumption. Locks bind the whole block.

### Cross-harness relaunch with transcript handoff

The policy field is `cross_harness: off|reuse|restart` — FLAT, deviating from
the design ticket's `fallback.cross_harness`: delivery 1 shipped flat keys and
exact-name locks, and a dotted key would introduce a second namespace
convention one field before any `fallback.*` sibling exists. First-setter-
wins like `refused_pinned_key`; lockable; NOT ceilinged (the ceiling stays
triggers-only — the operator's veto here is the lock, not a prune).

- **The session contract's THIRD state** (this amends §2's "a cross-BACKEND
  switch evicts the session"): under `reuse` or `restart`, a cross-harness
  fall-through on a session-bearing node REPLACES the session, said on the
  `model_fallback` line with the handoff state — it is neither kept (one
  provider's turns are never replayed into a harness that never issued them)
  nor silently evicted. Under `off` (the default), delivery 1's refusal
  stands byte-identical. Every other screen predicate — the C176 ask-rules,
  tools inversion, unresolvable tools, codex under a sandbox — stands under
  every value.
- `reuse` = the fresh harness CONTINUES the node mid-work: the sealed
  transcript is the continuation's input. `restart` = the node relaunches
  from its ORIGINAL prompt, transcript as reference (arbitrated 2026-10-05).
  The transcript is a read-only, harness-neutral rendering of the run's own
  `assistant_text` / `tool_called` / `llm_step_finished` events, scoped to
  the failed node, sealed into the run's shared scratch area and attached as
  `Task.Handoff` where continuity is cleared today. The workspace is
  unchanged (executor-level by construction).

### The per-family pool fallback door

§ Arbitrated 4 placed the door in the FIRST delivery; it was never coded (the
delivery-1 slice notes listed it as "pool door (5)", deliberately out, and
#2126 closed without it). Owned here and RE-DATED: delivery 2, slice 5 — the
donor-facing fallback-usable consent, the per-family consult on a fired
trigger when no owned credential serves it, the whole-bundle gate unchanged
on the non-fallback path. Never silently re-dated; this paragraph is the
record.
