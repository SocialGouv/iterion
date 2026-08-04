# ADR-087: Cross-backend model fallback chain (`fallbacks:`) — loud, IR-visible, opt-in

- **Status**: Accepted
- **Date**: 2026-08-04
- **Authors**: Victor Zeinstra
- **Code**: [pkg/backend/model/executor_retry.go](../../pkg/backend/model/executor_retry.go)
  (`dispatchWithProviderFallback`, `providerFallbackEligible`),
  [pkg/backend/model/executor_build_task.go](../../pkg/backend/model/executor_build_task.go)
  (`executeBackend`, `buildTask`, `stampDelegateOutputMeta`),
  [pkg/runtime/workspace_safety.go](../../pkg/runtime/workspace_safety.go)
  (`unrestrictedCLIBackendCanWrite`),
  [pkg/backend/delegate/delegate.go](../../pkg/backend/delegate/delegate.go)
  (`ErrRateLimited`, `Task`)
- **Supersedes**: the deferral recorded in
  [ADR-004](004-provider-fallback-chain.md) §Decision (5) and Alternative #1

## Context

A run that dies because one provider's subscription window shut is a
run's worth of work thrown away. [ADR-004](004-provider-fallback-chain.md)
addressed the narrow half of this: `provider: "zai,anthropic"` walks an
ordered list of **credential hints** on a task already built for one
backend, swapping `ProviderHint` and (since the 2026-06-22 amendment)
`Model` between attempts. It explicitly deferred the other half — "teaching
`claw` to re-resolve both provider *and* an appropriate model per chain
element … a separate, larger feature (*model fallback chain*)" — and named
`providerFallbackEligible` as "the single, named seam where that backend
would later opt in". Alternative #1 pre-named the future surface: "a
dedicated `model:` chain or a `fallbacks:` block, decided when there's a
concrete cross-API requirement".

The concrete requirement arrived from the opposite direction to the one
ADR-004 anticipated. The interesting failure is not `claw`-anthropic →
`claw`-openai; it is a **CLI backend exhausting its subscription forfait**
and needing to continue on a metered API through `claw`. That is the case
operators actually hit, and it is the one ADR-004's mechanism cannot serve
at all, because it never changes the backend.

Generalising the existing loop is not, however, a matter of relaxing
`providerFallbackEligible`. Three properties of the current execution stack
make the naive generalisation produce **silently wrong runs** rather than
resilient ones:

1. **The `delegate.Task` is backend-shaped in at least seven places.**
   `buildTask` bakes `SystemPromptMode`, `UserContent` multimodal blocks,
   `AllowedTools`, `ToolDefs`, `MCPServers`, the model-spec format and
   `Hooks` from the `backendName` argument
   ([executor_build_task.go:561](../../pkg/backend/model/executor_build_task.go)).
   `ToolDefs` — the only tool channel `claw` reads — is populated solely
   under `len(effectiveTools) > 0 && backendName == claw`, and the claw
   backend wires tools only when `len(task.ToolDefs) > 0`. Executing a
   claude_code-built task on `claw` therefore yields an agent with **zero
   tools** that still carries an output schema, and will return a
   schema-valid verdict it never verified. Today's loop, which mutates two
   fields on a shared `*delegate.Task`, would ship a façade generator of
   exactly the class [docs/workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md)
   exists to prevent.

2. **Three analyses decide safety BEFORE the run, from one static backend
   name.** `containsClawNode` walks literal `.Backend` fields to decide
   whether to bind-mount the `iterion` binary into the sandbox
   ([sandbox_mounts.go](../../pkg/runtime/sandbox_mounts.go), `addClawBinaryMount`);
   `unrestrictedCLIBackendCanWrite` returns `backend != "claw"` to admit
   parallel branches ([workspace_safety.go:129](../../pkg/runtime/workspace_safety.go));
   `validateFanOutEachWorkspaceSafety` gates fan-out on that classification.
   A chain resolved inside the executor is invisible to all three: a
   tools-less `claw` judge with a CLI fallback is admitted read-only, then
   runs N concurrent write-capable agents on one worktree.

3. **The run-level machinery keys on the terminal error's TYPE.**
   `usageWindowEvidence` ([pkg/runner/usage_retry.go](../../pkg/runner/usage_retry.go))
   and `classifyPoolCondition` ([pkg/runner/loop_spend.go](../../pkg/runner/loop_spend.go))
   both `errors.As` for `*delegate.ErrRateLimited{Kind: usage_window}` — to
   arm the durable retry and to rest a credential-pool donor. Today's chain
   wraps only the LAST element's error, and a chain that SUCCEEDS returns
   `nil`. So a chain silently disarms both, and the donor keeps handing the
   next run the same shut credential.

A fourth constraint is economic. Cost is priced by **model**, never by
credential ([pkg/backend/cost/cost.go](../../pkg/backend/cost/cost.go),
`Annotate`), and claude_code's `annotateCost` falls back to the token
estimate precisely when the CLI reports none — the forfait case. A
forfait→metered fall-through therefore moves real money while `_cost_usd`,
`max_cost_usd`, the org monthly cap and the studio cost chip all show the
same shape of numbers. Every guard in the tree points the other way
(`ITERION_FORBID_SUBSCRIPTION_OAUTH` refuses metered→subscription); there
is no metered-spend guard at all.

ADR-004 could afford a transparent, one-log-note fall-through because its
elements swapped a credential on the same model over the same wire API.
That justification does not survive the scope change.

## Decision

Ship the cross-backend chain as a **loud, IR-visible, opt-in** mechanism,
not a transparent one. Ten decisions.

### 1. The chain is a compiled IR property of the node

`fallbacks:` lands on `ir.AgentNode` / `ir.JudgeNode`, not in executor-private
state. `containsClawNode` takes the **union** of every element's backend, so
a `claw` element that appears only in a fallback block still gets the
in-container binary mount. `unrestrictedCLIBackendCanWrite` takes the **most
permissive** element: a node is mutating if ANY element would be. Both
`EffectiveBackendName` callers keep working; only their input widens.

This is a prerequisite, not a follow-on — an executor-private chain makes
all three pre-run analyses wrong by construction.

### 2. Surface: named entries, dispatched from `TokenIdent`

The DSL has no sequence token: `-` is consumed only as the first character
of `->` ([pkg/dsl/parser/lexer.go:352](../../pkg/dsl/parser/lexer.go)), so a
YAML bullet list is unparseable. `fallbacks:` uses the **named-entry** shape
already used by `secrets:` and `attachments:`, parsed from `parseLLMProp`'s
`case TokenIdent:` arm following the tool node's `recovery:` precedent — no
lexer, token-table or `isKeywordToken` change:

```
agent implement:
  backend: "claude_code"
  model: "claude-opus-5"
  fallbacks:
    api:
      backend: "claw"
      model: "anthropic/claude-opus-5"
      on: [usage_window]
    gpt:
      backend: "claw"
      model: "openai/gpt-5.5"
      metered: true
```

The entry name is not decoration: it is the stable id the fall-through
event and the node-output stamp name, so an operator reading a report sees
`fell through to "api"`, not an ordinal.

The block is wired through all seven hops — parser, `ast` struct, `jsonenc`
**encode and decode**, `ir` struct + compile, `validate`, `unparse`.
Skipping either `jsonenc` decode or `unparse` is not a missing feature but
silent data loss: the studio round-trips every save through
`/api/parse` → `/api/unparse`, so an unhandled block is deleted from the
`.bot` on the next unrelated edit — the exact bug `TestCapabilitiesRoundtrip`
was written for.

### 3. The per-element unit is rebuild-and-evict, never mutate

`dispatchWithProviderFallback` takes a per-element **build closure** instead
of a prebuilt `(backend, *task)` pair, and both dispatch sites supply one —
`executeBackend` and `executeLLMRouterUnified`, the latter of which
hand-builds its task inline today and would otherwise keep the wrong
backend's `SystemPromptMode`.

Because `buildTask` is not a pure assembly, it splits: the **effects** —
the `llm_prompt` store event and the board MCP run-token mint — fire
**once per node execution**; only the assembly re-runs per element.
Otherwise a 3-element chain writes three `llm_prompt` events for one
logical call and hands out three board tokens.

The token is minted once but **not** revoked here. The registry
documents a Register/Revoke contract that nothing has honoured since it
shipped — a pre-existing leak, bounded by a 1024-token cap and a TTL
sweep, orthogonal to this decision and touching six files of wiring
(`server.boardMCPServiceOption` → `runview.WithBoardMCP` →
`ExecutorSpec` → `model.WithBoardRegister` → the subbot path). Minting
once is what this ADR owes: it stops a chain from multiplying the leak.
Closing it is a named follow-on.

Evict, too: on every fall-through the node's claw session is dropped from
the `(runID, nodeID)` store and `ResumeConversation` / `ResumePendingToolUseID`
are cleared. The session key carries no provider fingerprint and the failed
attempt's conversation is captured *before* the error check, so without
eviction a claw→claw fall-through replays one provider's signed thinking
blocks into another — a 400 at best, a mangled conversation at worst. The
cost is stated plainly: a fall-through **discards the failed attempt's
work**, which the current code deliberately preserves for compaction.

### 4. Triggers are a closed positive list; the default is `[usage_window, unavailable]`

`on:` filters on a category derived from the typed error. The default when
omitted is `[usage_window, unavailable]`. Three sub-rules matter more than
the list:

- **An unclassifiable error advances the chain.** Today the loop falls
  through on any non-nil error; unclassifiable is the majority case for
  sandboxed `claw` (the IPC envelope flattens every error to a string) and
  for `kimi`/`grok` (no error channel at all). A filter that *stopped* on
  unclassifiable would silently regress every shipped `provider:` chain.
- **`any` is never the default.** A budget cap or a schema-validation
  failure re-fails identically on every element; the runner's existing
  `ErrBudgetExceeded` carve-out is the precedent for "do not retry this
  class".
- **`auth` is opt-in only.** `AuthFailedRecipe` deliberately routes
  AUTH_FAILED to `ActionPauseForHuman`, reasoning that a rejected credential
  needs re-auth and automating it makes a dispatcher re-spend sandbox cost
  in a loop. Enabling it by default would reverse a shipped, argued
  decision.

`billing` (hard no-credit) is **not** in the vocabulary: no backend carries
it today, `codex` actively conflates `insufficient_quota` into an untyped
`ErrRateLimited`, and Anthropic's "credit balance is too low" is matched by
zero code in the repo. It must not fold into `usage_window` — waiting
cannot cure it.

The canonical classifier lives in `pkg/backend/delegate` and
`recovery.Classify` consumes it; the reverse direction is an import cycle.
`usage_window` detection stays what it is today — **prose matching on
vendor phrasing**, two regexes and one literal — and the ADR records that
as a best-effort signal, not a protocol guarantee.

### 5. `usage_window` skips the in-node retry budget — shipped as a prerequisite

`isDelegateRetryable` returns true for any `*ErrRateLimited` with no `Kind`
check, so a shut 5-hour window burns a full backed-off budget (3 attempts,
6 when network-classified) at **every** element, under one per-node timeout
context. A 3-element chain can hit the node deadline before it is even
exhausted — the motivating scenario producing a worse outcome than today's
clean failure.

When a chain element remains, a `usage_window` classification advances
immediately. This ships **before and independently of** the chain: it is
correct on its own (it removes two dead CLI re-spawns per forfait window
for every existing bot) and it is what makes the chain cheap enough to be
worth having.

### 6. An exhausted chain surfaces a typed aggregate

The exhausted-chain error exposes `Unwrap() []error` over every element's
error, so `errors.As` still finds the first `*ErrRateLimited{usage_window}`.
Without this, a chain starting on an exhausted forfait and ending on an
unrelated 401 disables the durable retry **and** the credential-pool donor
cooldown — the single decision that most determines whether the surrounding
machinery keeps working.

### 7. A SUCCEEDING chain still reports what it absorbed

A run-scoped `SawUsageWindow(provider, resetAt)` accumulator, mirroring the
existing `SawAuthFailure`, is fed by the fall-through event and consumed by
`recordPoolSpend`. Otherwise "chain first, run-level policy after" means
"chain first, run-level policy **never**": a successful run reports
`ConditionOK`, the donor's exhausted subscription is never rested, and the
next run walks into the same shut window forever.

Precedence is otherwise unchanged and needs no engine work: the chain lives
inside the executor, i.e. below the recovery dispatch, so
in-node retry → chain → recovery recipe → run-level wait/resume is already
the structural order. Chain fall-throughs do **not** decrement
`retrypolicy.MaxAttempts`; the two budgets are orthogonal.

### 8. The fall-through is loud on every surface

- A new `store.EventType` emitted from a wired `OnProviderFallback` (which
  today fires into nothing — `NewStoreEventHooks` never registers it),
  added to the studio's `PassthroughEventType` union and to
  [docs/persisted-formats.md](../persisted-formats.md).
- `_backend` / `_model` on the node output come from `result.BackendName` /
  `result.EffectiveModel` — the element that **served** — never the
  pre-dispatch requested name. This is a bug fix the chain makes
  load-bearing: the studio's `backends_used` chip and `iterion report`'s
  per-step tag read those keys and would confidently assert a false fact.
- `_fallback_used` and `_served_by` (the entry name) are stamped so a bot's
  **deterministic gate can fail closed on a degraded input**, the same
  posture as `findings_parse_failed`.
- The **credential instrument** (`subscription-oauth` / `metered-byok` /
  `pooled-donation`) is stamped alongside. Without it a forfait→metered
  switch is unobservable by construction, since cost is priced by model.
- The checkpoint records the serving element (backend + provider + model).
  Resume either re-enters on it or explicitly drops backend-specific
  continuity with a visible degradation event; silently carrying a
  conversation across a backend boundary is excluded.

### 9. Failed elements' usage is accumulated

Each failed element's tokens, cost and duration roll into the returned
`Result` before the chain advances. The precedent is `validateAndRetry`,
whose comment records that dropping it "understated the run's real usage and
broke budget enforcement at the margins" — a margin that becomes an entire
agentic session under a cross-backend chain, invisible to `max_cost_usd`,
the org monthly cap and a lending donor's ledger.

### 10. Capability crossings that cannot degrade safely are refused at compile time

**Hard refusal (error, C176):**

| Crossing | Why |
|---|---|
| `permission: ask\|deny` + an element on a backend that cannot enforce it (`kimi`, `grok`, `codex`; `claw` under sandbox until `IOTask` carries the policy) | The gate is the anti-prompt-injection boundary. `IOTask` has **zero** `Permission` field, so a sandboxed claw element is ungated by construction. `pi` already refuses rather than degrades — adopt its precedent verbatim |
| A claw⇄CLI crossing on a node with an **empty `tools:` list** | The list inverts meaning: empty = zero tools on claw, empty = full unrestricted native toolset under `bypassPermissions` on every CLI backend. A read-only reviewer becomes an agent that can edit its own subject |
| An element that changes backend without its own `model:` | The model-spec formats are mutually incompatible (`provider/model` for claw, `anthropic/`-only prefix stripping for claude_code) |
| A secret-bearing node (`MaterializeSecrets`) with a `pi`/`kimi`/`grok` element | The agent is instructed to emit `__ITERION_SECRET_*__` placeholders the element never substitutes |
| `session: inherit\|fork` with a cross-backend element | Session continuity has no cross-backend meaning |

**Warning (C177), run proceeds degraded:** `reasoning_effort` drift
(including the unset default: claude_code `xhigh` vs claw none vs kimi no
dial), `ultracode` on a chain whose elements lack a subagent tool,
`compress:` / `memory:` / `mcp_server:` / board `capabilities:` on a chain
containing an element that ignores them, and `on: [usage_window]` declared
on a chain whose failing element cannot produce that classification.

Two more codes: **C173** (error — element declares neither backend nor
model) and **C175** (warning — unknown `on:` token, soft set). Every code
needs a row in [docs/references/diagnostics.md](../references/diagnostics.md)
or `TestDiagCodesAreDocumented` reds CI. The new validator copies
`validateCommand`'s effective-backend resolution (`default_backend:`
fallback + `${VAR}` skip), not `validateProviders`', which reads
`f.Backend` raw and therefore under-fires today.

### 11. Judges and gate-feeding nodes never inherit a chain

A judge takes a chain **only from its own node block** — not from a
workflow default, not from a launch `*` or kind selector. Revi's
`revi/review` is a required check on a merge-queue-protected `main` whose
verdict is a finding **count**; a weaker model emitting a well-formed
review with 2 findings instead of 7 turns a red gate green, and the gate's
only fail-closed rung is unparseable output. In mono topology — the default
— that one judge *is* the whole finding set.

Note this is a **behaviour change** to the existing resolver: `selectorScore`
returns `(1, true)` for `*` against judges today. Note also that `*` matches
synthetic nodes the engine invents (`<node>__recover`, `<node>_interaction`),
so an ADR-044 recovery agent would otherwise inherit a chain nobody authored.

### 12. Spend consent is explicit, and credentials are a launch-time precondition

An element whose credential is metered carries `metered: true` — authoring
it is the consent. `ITERION_FORBID_METERED_FALLBACK` mirrors the existing
`ITERION_FORBID_SUBSCRIPTION_OAUTH` in the opposite direction, and a
platform ceiling with `retrypolicy.Clamp`'s can-only-lower semantics lets a
cloud operator forbid subscription→metered instance-wide. This matters
beyond one run: metered spend is charged to the parent **org**, so a chain
that escapes onto a metered key can trip the monthly cost cap and deny
other teams' launches.

Credential availability is resolved at **launch**, not discovered at
runtime: sandbox forfait credentials are mounted at container boot and
cannot be added mid-run. Elements with no resolvable credential are pruned
at launch with a warning; a chain with no usable element fails at launch,
not on the first LLM call. In cloud the publisher already knows exactly
what it sealed, which is a strictly cheaper and more accurate oracle than
`detect.Report` — which reads the *host's* environment and in a runner pod
reports the pod's ambient credential, not the tenant's.

## Trade-offs

| Dimension | Loud/IR-visible/opt-in (chosen) | Transparent generalisation of ADR-004 (rejected) |
|---|---|---|
| Scope of change | Chain in IR + 3 plan-time consumers + dispatch seam refactor + 4 diagnostics + event + stamps | `providerFallbackEligible` returns true |
| Tool-surface correctness | Full task rebuild per element | claude_code task on claw = zero tools, schema-valid unverified output |
| Parallel-branch safety | Most-permissive element admits | N concurrent writers on one worktree, silently |
| Permission gate | Refused where unenforceable | Vanishes exactly when the run is under stress |
| `usage_window` evidence | Typed aggregate + absorbed-window accumulator | Durable retry and donor cooldown silently disarmed |
| Spend | Instrument stamped, consent explicit, platform ceiling | Real money moves, no number changes |
| Judge integrity | Chain never inherited + machine-readable degradation marker | A red merge gate turns green with no artifact recording it |
| Time to first value | Longer — prerequisites ship first | Immediate |

The honest concession: this is **substantially more work than the feature
sounds like**, and most of it is not the fallback logic. Roughly a third of
the change is fixing things that are already wrong and only become
load-bearing under a chain — the mis-stamped `_backend`, the unrevoked
board token, the unwired `OnProviderFallback`, the retry budget burned
against a shut window, `validateProviders`' backend resolution. A reader
looking for "make the chain work on claw" will find a one-line predicate and
should be told, in this record, why that one line is not the change.

## Alternatives considered

### 1. Relax `providerFallbackEligible` and reuse the existing loop

Return true for `claw`, let the existing chain mutate `ProviderHint` and
`Model`.

**Rejected because**: `ToolDefs` is populated only for `claw` and only when
the tool list is non-empty, and the claw backend wires tools only when
`ToolDefs` is non-empty. A claude_code-built task executed on claw is a
tool-less agent that still carries an output schema — it returns a
schema-valid verdict it never verified. The same mutation loses image
blocks, MCP servers and the operating posture. This alternative does not
produce a degraded run; it produces a confidently wrong one.

### 2. Keep the chain executor-private

Resolve the chain inside `ClawExecutor` as today, leave the IR untouched.

**Rejected because**: `containsClawNode` decides the in-container `iterion`
bind-mount from static `.Backend` fields at sandbox start, and
`unrestrictedCLIBackendCanWrite` admits parallel branches from one resolved
backend before the run. Executor-private means the sandbox fallback dies
with `exec: iterion: not found` and a `fan_out_each` over N items can run N
write-capable agents on one worktree — both after every refusal that would
have caught them has already passed.

### 3. Extend `ModelOverrides` into an ordered list of triples

Make `NodeModelOverride` a list and let the studio Launch UI build an
ordered chain per node.

**Rejected because**: `ForNode` resolves backend / model / provider through
three **independent** specificity scores, which is precisely what lets
`--backend '*=claw'` compose with `--model 'reviewer_*=X'`. A list element
is an atomic triple with no coherent per-field merge, so the two contracts
cannot both hold. The chain is therefore a separate rule kind that replaces
wholesale at its winning specificity level; the scalar per-field path is
untouched.

### 4. Ship an ordered-list UI per node

The originally-proposed operator surface.

**Rejected because**: the existing section is one input + one select per
node inside a collapsed disclosure, with state in a plain `useState` that
persists nothing between launches — on a 15-node bot the operator already
rebuilds every override each launch, and an ordered list multiplies that by
chain length. It cannot pre-fill from the bot's chain, because the studio's
`AgentDecl` has no `provider` field and the existing chain is invisible
there. Its availability greying reads `/api/backends/detect`, which probes
the **server host** — in cloud it would grey out the tenant's working key
and offer the platform's. The value ("don't lose a 40-minute run to a
forfait wall") is delivered by one fallback, so the UI is a **single
run-level opt-in row**, off by default, applying only to nodes that opted in
via the DSL, plus a run-header banner and a timeline row when it fires. The
per-node chain stays DSL-only, exactly as `provider:` is today.

### 5. Desugar `provider:` chains into `fallbacks:` elements

One chain type, one runtime path, one set of tests.

**Rejected because**: `provider:` is a credential-hint chain whose elements
share a backend, and C088 ("a chain has no effect on claw/codex") is
correct for it and load-bearing documentation — two shipped catalog bots
author against that prose in their own comments. Desugaring makes C088 false
and forces a migration of 13 `${RESCUE_PROVIDER:-zai}` nodes as a side
effect of an unrelated feature. `provider:` stays as-is; the
`providerFallbackEligible` collapse and C088 key on the chain's
**provenance** (legacy field vs `fallbacks:` block), not on the backend
name. Elements resolving to the same `(backend, credential, model)` as their
predecessor are deduped, which is the protection ADR-004's Alternative #3
bought with the eligibility guard.

### 6. Put the chain in `manifest.yaml` next to `retry:`

The manifest already hosts retry policy.

**Rejected because**: the manifest comment states the line explicitly —
"retry decides whether a NEW run is launched, which is orchestration, not
workflow semantics". `fallbacks:` changes how a node **executes inside** a
run, like `compress:`, `permission:` and `budget:`. It belongs in the DSL by
the repo's own stated criterion.

## Consequences

- **The chain becomes an IR fact, and three pre-run analyses widen with
  it.** `containsClawNode` takes the union of element backends; the
  workspace-safety classifier takes the most permissive element. The second
  is a real cost: a tools-less claw node that declares a CLI fallback stops
  being admitted as a parallel read-only branch, so some fan-outs that run
  in parallel today serialise. That is the correct trade against a silent
  lost-write race, and it is visible at compile time rather than at 3am.

- **Two prerequisites ship before the feature, and are independently
  correct.** (a) `usage_window` stops burning the in-node retry budget when
  a fallback exists — behaviour change for every existing bot, strictly an
  improvement. (b) `_backend` / `_model` are stamped from the serving
  element. Both are small, both are bug fixes, both are meaningless to
  revert once the chain exists.

- **`buildTask` splits into assembly and effects.** One `llm_prompt` event
  and one board run-token per node execution, however long the chain. The
  registry's unhonoured Revoke contract stays open as a named follow-on:
  minting once stops the chain multiplying a leak it did not create.

- **Two dispatch sites converge on a build closure.** `executeBackend` and
  `executeLLMRouterUnified` (which hand-builds its task today) supply the
  same per-element builder. This breaks two tests at compile time — they
  call the private function with its current 6-argument signature — and
  `executor_fallback_test.go`'s stub keys on `ProviderHint` alone, so it is
  a harness rewrite, not an assertion update.

- **v1 is deliberately narrow: `claude_code` → `claw`.** `kimi` and `grok`
  are excluded — they sit on the legacy `ParseOutput` contract which
  structurally cannot return an error, so no typed trigger can ever fire;
  migrating them to `ParseOutputRich` is a prerequisite for their inclusion,
  not a follow-on. `codex` is frozen. A **sandboxed `claw` element stays
  usable** — the trigger comes from the failing element, and `claude_code`
  types it correctly — but with two bounded limits: it is refused outright
  on a node declaring `permission: ask|deny` (the IPC envelope carries no
  policy), and its own failure is unclassifiable, so per decision 4 it
  always advances the chain and can never be filtered by `on:`. Since
  sandbox is on by default, typing that envelope is the highest-value
  follow-on. The ~8 direct-claw
  generation sites (human `interaction: llm`, review companion, ADR-044
  self-repair, subagent, supervisor, sessionboard) never see a
  `delegate.Backend` and stay out of scope: they still hard-fail on a
  forfait wall.

- **A fall-through discards the failed attempt's work.** Session eviction is
  what prevents replaying one provider's signed conversation into another,
  and it is a data-loss decision made deliberately here rather than
  defaulted into.

- **Three subsystems now react to one `usage_window` signal**, and all three
  keep working only because of decisions 6 and 7: the in-run chain, the
  run-level wait/resume, and the cloud credential-pool donor cooldown. The
  local surface has **no** run-level retry at all — `--auto-resume` defaults
  to 0 and the filesystem store has no retry state — so on the primary
  dogfood surface the chain is not redundant with an existing automation, it
  *is* the automation.

## Staging

Each stage is shippable and independently valuable.

1. **Prerequisites** — `usage_window` skips the in-node retry budget;
   `_backend`/`_model` stamped from the serving element; `OnProviderFallback`
   wired to a real store event + studio union + `persisted-formats.md`. No
   new DSL. This alone makes today's `provider:` chains observable and
   cheap.
2. **Dispatch seam** — `buildTask` split into assembly + effects; both
   dispatch sites converge on a per-element build closure; per-element usage
   accumulation; typed aggregate error; session eviction on fall-through.
   Still no new DSL — today's chain simply becomes correct under rebuild.
3. **`fallbacks:`** — the seven-hop DSL surface, the IR field, the widened
   plan-time consumers, C173/C175/C176/C177, launch-time credential
   pruning, and the `metered:` + `ITERION_FORBID_METERED_FALLBACK` consent
   layer. Migration fixture: `bots/sec-audit-source`, whose comment block
   already documents the C088 contract.
4. **Operator surface** — one run-level route (studio Launch row + CLI
   `--fallback <backend>:<model>`, re-passable on `resume`), applied to
   agent nodes that declare none of their own and never to judges. It is
   **materialised onto the compiled workflow** (`ir.ApplyRunFallback`)
   rather than resolved inside the executor, so it passes the same
   safety screen as an authored route — refused crossings are dropped
   with a warning — and decision 1 holds for it too; a `fallbacks_used` row in the
   run header naming each node a route served; the `model_fallback`
   event rendered in the timeline. The chain is deduped across all three
   sources, so a route resolving to the call that just failed is dropped
   rather than paying a second retry budget.
