# ADR-121: adaptive routing — a multi-level policy that picks the (harness, credential) pair, at launch and mid-run

- **Status**: Proposed (design review — another model family, then the operator)
- **Date**: 2026-10-02
- **Authors**: Claude (draft), Jo (arbitration pending)
- **Extends**: [ADR-087](087-cross-backend-model-fallback-chain.md) (cross-backend fallback routes), [ADR-091](091-fallback-skip-route-and-plan-peer-review.md) (`action: skip`, `when:` gates), [ADR-090](090-shared-tier-credential-policy.md) (shared-tier ordering), #1956 (route-aware shared tiers)
- **Serves**: epic [#2000](https://github.com/SocialGouv/iterion/issues/2000), slice [#1999](https://github.com/SocialGouv/iterion/issues/1999)
- **First delivery only**: the policy foundation, launch-time harness selection, and same-harness mid-run fallback. Cross-harness relaunch with transcript handoff is the next delivery on this foundation (§ Out of scope).

### Context

Today a run finds its credential through pieces that do not compose. A bot author may hand-declare per-node `fallbacks:` routes (ADR-087) and a run-level `--fallback` chain; inside one family the delegates hold a precedence that is easy to mis-state and matters here in full: a `provider:` hint overrides everything; without a hint the anthropic wire is walked `zai → moonshot → anthropic → forfait → env` (the forfait passes a key only for a **pinned** same-provider route — `RegisterForfaitFirst`); the walk is over a bundle **sealed at launch** — it has no notion of a window state. The shared tiers fill wires in an order two knobs order (#1956); the facade policy (#2096) decides whether a z.ai key may be the wire's default; the pool is a last resort with a whole-bundle gate ("only for a run that has no credential of its own at all"); and when every tier abstains, the run either parks on a usage window or fails at its first LLM call. Host-side `ITERION_BACKEND_PREFERENCE` is detection, not a run's sealed credentials.

What no piece does: **follow the credentials the run actually holds**. A run whose tiers hold only an OpenAI key still starts its `claude_code` nodes and dies at the first call; a run whose Claude forfait closes mid-run parks, while a z.ai key sits sealed in its own bundle. Making that work by hand costs a `fallbacks:` chain per node, per bot, per repo — knowledge the operator, not the author, holds.

The operator's decision (2026-09-30, on epic #2000): routing becomes a **policy, configurable at every level, automatic by default**, and the harness follows the credential.

## Decision

### 0. The form: a per-field fold, on the retrypolicy pattern

`RoutingPolicy` resolves exactly the way `retrypolicy.Resolve` already does (field by field, the first level that sets a field wins, a **provenance map** answers "which level decided this", and the platform level can only **lower** what the levels below may do — on `triggers`, the platform ceiling prunes and never extends). List fields carry their own merge rule, stated per field in §1 — a reordered prefix of a longer list being a semantics trap, `pair_order` **replaces** at the highest level that sets it rather than merging. A field marked `locked` at a level stops the descent for that field: the one primitive cost governance has. The resolved policy is snapshotted onto the run document at launch, as retrypolicy already snapshots.

**Levels and sequencing.** Platform (the settings record the facade and keys-first knobs already ride), bot (a `routing:` block in the manifest — orchestration, not workflow semantics, the same rule ADR-087's alternative 6 set for `retry:`), and run (launch fields) are delivery 1. Org and team are **delivery 2**: they are greenfield — `identity` records are directory entries, admission lists are not settings, and each level means a new collection, API routes, an RBAC answer to "who is an org admin", tenancy threading, CAS and preview support. Named, costed, sequenced — not smuggled into "four of them new".

Fields of the first delivery:

- **`pair_order`** — the ordered list of `(harness, credential)` pairs a run may occupy, named per FAMILY (the instance follows the tier walk: `claw+anthropic_key` does not name WHICH anthropic key — the shared order and the walk do, which is what keeps fill/restore/preview parity and per-fingerprint audit). Default: `claude_code+claude_forfait`, `codex+chatgpt_forfait`, `claw+anthropic_key`, `claw+openai_key`, `claude_code+zai_key`, then the rest. A level replaces it.
- **`triggers`** — a subset of the closed vocabulary the fallback machinery already classifies: `usage_window`, `unavailable`, `transient_exhausted`, `auth`. `budget`/`schema` were never categories and are not offered; `unclassified` and `any` are excluded by rule — per ADR-087, a non-classifiable failure advances a hand-declared chain but never fires a policy switch, and when the proof is flattened text (`UsageWindowInFlattenedError` is the retry side's carve-out) the policy side inherits exactly that carve-out or fires nothing. The platform ceiling prunes; no level extends.
- **`model_classes`** — an extension of the MODEL REGISTRY, not a field of this policy (the class is consumed outside routing: the cost estimator bills per model, spec validation, display). The policy references it. Classes (`top`, `standard`, `fast`) map to a concrete model per family; iterion ships the defaults in the registry; a level overrides a class per family; a class that resolves nowhere on its family leaves the route as written and warns — and its switch effects (stamps, cost, the C173 analogue) follow the registry's rules.
- **`refused_pinned_key`** — `park` (the default #1999 decided) or `forfait`. The epic's "falls back to the forfait by default" contradicts #1999's `park` — an arbitration the operator owes (§ Open arbitrations). Migration: #1999 ships its knob AS the platform-level policy field (one source, never two), and #1999's carve-out travels with the field — a facade key pinned beside a Claude forfait is out of its scope, and the fold may not pair what the carve-out excludes.
- **`strict`** — marks a node's `backend:` pin (or the workflow's) as a requirement instead of a preference (§2). An author writes it; a level may set it where the author did not; a level above the author may not unset it.

`facade_default` (#2096) stays and composes (§3's rule); `ITERION_BACKEND_PREFERENCE` remains host DETECTION and is replaced at launch by the policy's selection — the detector's answer is what the policy decides with, not a sixth level.

### 1. Launch-time: the pair selection happens DURING the resolution, and the program it produces is screened

The pair selection is not a post-walk retarget — a retarget after the walk would seal into channels the executor never reads (a facade key sealed pinned-only is invisible to the default precedence, and a facade hint without a reachable key refuses rather than degrades) and would leave the resolution's derivations (`spendableProviders`, `wantsFor`, `derivePinnedProviders`, `envFunded`, `mayReadWireDefault`, `unfundedPinnedProviders`, the fingerprint stamps, the launch gate) reading a program nobody rewrote. Instead:

1. The resolved policy is computed once, before the tier walk.
2. The tier walk runs, and the pair selection shapes it: the first pair in `pair_order` whose credential a consulted tier holds decides which channels the walk seals into — a facade key serving a selected `claude_code+zai` pair seals as the wire's DEFAULT where the facade policy permits, pinned-only where it does not (§3's composition rule).
3. The program is rewritten through the SAME screened path `--fallback` already uses: computed routes materialized on the IR by `ApplyRunFallback`'s screen — **agent nodes only** (a judge's verdict is load-bearing and is never re-targeted by policy), every computed stage through the C176/C135 predicates, refusals landing on `run_fallback_refused` as run-level fallbacks do today. A `permission: ask|deny` node is never re-targeted onto a harness that cannot enforce its gate: refused by the screen, said on the event, never degraded in silence.

After that rewrite, every derivation listed above reads the rewritten program — once, with the same answer for fill, restore, preview and launch gate. This is the honest version of the alternative the first draft rejected on a false premise: synthesis CAN act at launch, after compile, pruned by what the launch funds — the screen is what makes it legitimate.

The selection is deterministic: the first pair in the order whose credential the run holds. A run that acquires no credential at all (env-funded, #2038) selects nothing — the env is the harness.

### 2. Between attempts: the re-resolution re-picks the pair; in-run, the cooldown ledger already re-orders

"Mid-run" needs its two halves named, because they are different mechanisms:

- **In-run (per node, per dispatch)**: the route cooldown ledger exists — `(backend, provider, model)` keyed, category and `until` recorded, cause preserved for the retry classifiers. A closed window already de-orders a route at dispatch. The policy's `triggers` filter what that ledger may de-order, and this ledger is where a switch's audit line is emitted in-run.
- **Between attempts (per run)**: the only re-resolution is the one `SubmitResume` already performs after a park — the full `resolveAndSealCredentials` replay. §1's selection runs there: the retried attempt spends what the re-picked pair serves. A JetStream redelivery re-plays the original message and SecretsRef and re-resolves nothing — unchanged.

The retry **arming** does not change: `usageWindowRetryAt` remains authoritative from the FAILED credential's terminal proof, `skippedReopensAt` remains a speculative earlier wake refused at the last attempt if the authoritative wall is still reachable, and the last-attempt reservation stands. The switch changes what the retried attempt spends, not when it is armed.

Session semantics at a switch are part of the contract: a same-family credential switch (GLM via z.ai on a `claude_code` session) keeps the session and answers GLM **only where the facade policy permits it** — under `facade_default: auto`, the veto wins and the run parks on its forfait (#2096's doctrine stands: no GLM in silence); `always`, `tier`, or a lock opens it. A cross-family or cross-backend switch evicts the session (ADR-087 §3) and emits the `session_degraded` equivalent (ADR-091). The design ticket owns the full harness × credential × family compatibility matrix — who can serve what, what answers, which session event fires.

Resume classifies every policy field as **identity or volatile**: identity fields are frozen at launch and replayed by the resume (the `PinnedProviders` doctrine — the resume replays the launch's answer), volatile fields (windows, availability) re-resolve. A `pair_order` change between launch and resume moves the harness of a living session only through the session-event contract above, never in silence.

### 3. What deliberately does not change

- Tenant isolation: the policy reorders what the walk may consult; it never lets a tier read another tenant's credential. The parity property keeps its full cast — fill, platform stage, restore, preview, the launch gate (`usagecap` pre-flight judges the routes on what they then spend), `reviewtopology`'s families (injected from the SEALED credentials by design; the policy does not change the topology), and the three route derivations agreeing.
- Author intent: hand-declared `fallbacks:` chains remain authoritative where declared; the policy synthesizes nothing there. `strict` is how a pin stops being a preference.
- Budget and schema failures never fall back: they re-fail identically on every route.
- The pool stays a whole-bundle last resort in this delivery ("only for a run that has no credential of its own at all"). A per-family pool fallback is a contract change — it would hand the pool to a run holding a credential of another wire — and is explicitly a future decision, not a subordinate clause here.

## Alternatives considered

- **Per-node policy in the DSL** (`routing:` on each node): maximal author control, exactly the per-bot per-repo duplication the epic removes; rejected as the primary mechanism, kept as `strict`'s spelling.
- **Launch-time overrides without the screen** (the first draft's mechanism): computed overrides reach past the C176/C135 screen ADR-087 built precisely so a machine could not do what an author is refused — an ungated node, a tools-less claw node, a permission gate no harness can enforce. Rejected: the synthesis goes through the screen or it does not land.
- **A dedicated routing service deciding per call**: a second brain beside the executor with its own view of the credentials — the divergence risk #1956 closed, rebuilt at the routing layer. Rejected: the decision stays in the resolution, read once per launch.
- **Doing nothing**: the per-bot `fallbacks:` authoring cost is what kept the capability from landing twice before.

## Consequences

- **Cost is automatic by default** — every allowed fallback is attempted, a subscription may spend as extra usage (claw on a Claude forfait), and the governing tool is the lock, not an opt-in. The operator's explicit stance, recorded.
- **Every switch is said**: audit lines at launch selection and at each in-run de-order, the resolved pair and its provenance in the run document, the spend on the serving credential.
- **The fold is pure; the walk is not**: the policy fold is table-testable; the resolution is tested by properties over injected probes — fill/restore/preview/launch-gate agreement, and `spendableProviders`/`wantsFor`/`derivePinnedProviders` reading the same rewritten program.
- **Delivery 1 is platform + bot + run.** Org and team are delivery 2 with their stores, API, RBAC and audit named as the cost they are.
- **Testing obligation**: fold tables; pair-selection tests against seeded bundles; switch tests against window states; the parity property asserted for the policy-resolved answer; mutation-checked like #1998/#2038.

## Open arbitrations (the operator's)

1. **`refused_pinned_key`'s default**: #1999 decided `park`; the epic says `forfait` by default. One of the two words is wrong — the ADR ships `park` until arbitrated.
2. **The facade veto vs the pair order**: this ADR fixes "the veto wins by default; `always`/`tier`/a lock opens" — confirm, or the flagship scenario of epic #2000 needs `always` as the deployment default, reversing #2096's refusal of "GLM in silence".
3. **Run level SET or CHOOSE**: the recommendation is SET within bounds — own-tier extension is the launcher's right; what a lock binds is extension toward SHARED credentials (org, platform, pool): other people's money. Confirm the boundary as written.

## Open questions resolved by the review (recorded)

1. Pair order is CONCRETE per family in delivery 1; the instance follows the tier walk (abstract slots would rebuild per-tier semantics inside the policy).
2. The run level SETS within the bounds above; locks bind it; the platform ceiling can only lower `triggers`.
3. `model_classes` lives in the model registry, referenced by the policy — the class is consumed outside routing.
4. Pool fallback-usable is a DONOR-facing field (the epic already decided it); the policy reads a rollup. The question is retired.
5. A replacement's window reads the usage ledger first, `forfaitWindowClosed`'s probe on absence — no third source; retry arming is unchanged.

## Out of scope (later deliveries)

Cross-harness relaunch with transcript handoff (delivery 2 on this foundation: the pair order, the triggers and the levels are its vocabulary). Per-family pool fallback (a contract change, named above). Org and team levels (delivery 2, costed above).
