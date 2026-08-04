# ADR-004: Per-node provider fallback chain as a credential-routing-hint chain

- **Status**: Accepted (amended 2026-06-22 — per-element model shipped, see
  below; the cross-API deferral recorded here is discharged by
  [ADR-087](087-cross-backend-model-fallback-chain.md))
- **Date**: 2026-05-26
- **Authors**: devthejo
- **Code**: [pkg/backend/model/executor_retry.go](../../pkg/backend/model/executor_retry.go)
  (`dispatchWithProviderFallback`, `providerFallbackEligible`),
  [pkg/backend/model/executor_resolve.go](../../pkg/backend/model/executor_resolve.go)
  (`resolveProviderChain`),
  [pkg/dsl/ir/validate_provider.go](../../pkg/dsl/ir/validate_provider.go)
  (C087 / C088)
- **Docs**: [docs/backends.md §Per-node provider routing & fallback chain](../backends.md)

## Context

Operators already had a single-node escape hatch for provider outages:
the `provider:` field accepts `${RESCUE_PROVIDER:-zai}`, so when z.ai's
5-hour cap was hit the recovery playbook was "pause the run, set
`RESCUE_PROVIDER=anthropic`, resume from checkpoint." That works but is
manual, per-run, and racey — the operator has to notice the failure, flip
an env var, and re-drive every affected node.

We wanted to generalise this into a **declarative, per-node fallback
chain** — `provider: "anthropic,zai,openai"` — where the runtime falls
through to the next provider on a hard failure beyond the retry budget,
transparently, so the operator sees a log note instead of a failed run.

The friction is semantic. In iterion's execution stack, "provider" means
two different things depending on the backend:

- **`claude_code`** consumes `task.ProviderHint` and maps it to Anthropic
  credentials: `anthropic` (direct key / OAuth) vs `zai` (the z.ai
  Anthropic-compatible facade). Both serve the **same model id** over the
  **same wire API** — switching is a pure credential swap.
- **`claw`** ignores `ProviderHint` entirely; it derives the provider
  from the `model:` spec prefix (`openai/gpt-5.5`, `anthropic/claude-…`).
- **`codex`** ignores the hint too.

So the example chain `anthropic,zai,openai` mixes a same-API credential
swap (`anthropic`↔`zai`) with a *different API family* (`openai`). A pure
`ProviderHint` chain cannot transparently fail an Anthropic model over to
OpenAI, because the **model id must change too** — `claude-opus-4-7` is
not an OpenAI model.

## Decision

Implement the fallback chain as a **credential-routing-hint chain**, not a
cross-model chain.

1. `resolveProviderChain` expands `${VAR}` on the whole `provider:` field
   **first**, then splits on commas into an ordered list of hints. A
   single value (incl. the historical `${RESCUE_PROVIDER:-zai}`) yields a
   one-element chain, so existing workflows are byte-for-byte unchanged.
2. `dispatchWithProviderFallback` wraps the existing `retryDelegateLoop`:
   it sets `task.ProviderHint` per attempt and walks the chain. Each
   provider gets the **full retry budget**; only a hard failure *beyond*
   it (non-retryable error, or retryable-but-exhausted) falls through.
   Context cancellation aborts the chain immediately.
3. The chain is **backend-agnostic in mechanism** but only **meaningful on
   `claude_code`** today. `providerFallbackEligible` collapses a
   multi-element chain to its head for hint-ignoring backends
   (`claw`/`codex`) so the run never burns a second retry budget re-running
   an identical call. A compile-time warning (**C088**) tells the author a
   chain on those backends is inert.
4. Unknown literal hint tokens are flagged at compile time (**C087**,
   warning) and ignored at run time; `${VAR}` fields are left for run-time
   resolution and not statically validated.

Cross-provider / cross-model failover (e.g. `claw` Anthropic → OpenAI) is
**explicitly deferred**. It would require teaching `claw` to re-resolve
both provider *and* an appropriate model per chain element — a separate,
larger feature ("model fallback chain"). `providerFallbackEligible` is the
single, named seam where that backend would later opt in.

## Trade-offs

| Dimension | Credential-hint chain (chosen) | Cross-provider/model chain (deferred) |
|---|---|---|
| Scope of change | Executor loop + chain resolver + 2 diagnostics | + claw provider/model re-resolution, per-element model specs, credential override |
| Risk | Wraps the already-tested `retryDelegateLoop`; backends untouched | Touches claw's credential + model resolution (the hot path for all in-process LLM calls) |
| Matches `RESCUE_PROVIDER` | Exactly — same `anthropic`↔`zai` lane, now declarative | Superset, but the validated use case is the credential swap |
| `anthropic,zai` failover | ✅ works on `claude_code` | ✅ |
| `…,openai` failover | ⚠️ inert on claude_code/claw; warned (C088) | ✅ via model switch |
| Back-compat | Single value identical to today | Same |

The single honest concession is that the literal `openai` element in the
motivating example does not do cross-API failover yet. We surface that
limitation loudly (C088 at compile time, a dedicated docs section)
instead of shipping a chain that silently no-ops.

## Alternatives considered

### 1. Make the chain carry full `provider/model` specs

Let each element be `anthropic/claude-opus-4-7` or `openai/gpt-5.5` so a
chain can cross API families on `claw`.

**Rejected for this change**: it overloads the `provider:` field with
model semantics (we already have `model:`), and forces every chain author
to repeat the model per provider. It also doesn't help `claude_code`,
which can't talk to OpenAI at all. This is the deferred "model fallback
chain" — a cleaner future home is a dedicated `model:` chain or a
`fallbacks:` block, decided when there's a concrete cross-API requirement.

### 2. Teach `claw` to honour `ProviderHint` now

Have `claw` override its model's provider prefix from the hint.

**Rejected**: a hint like `openai` can't sensibly apply to a
`claude-opus` model id — the resolved model would be invalid. Making it
work requires per-provider model resolution anyway (alternative #1), so
this is not a smaller step.

### 3. Always walk the chain regardless of backend

Drop `providerFallbackEligible` and let the loop run on every backend.

**Rejected**: on `claw`/`codex` (which ignore the hint) every fall-through
re-runs the identical call, doubling the retry budget — real cost and
latency for zero behavioural change. The eligibility guard + C088 make the
no-op explicit and free.

### 4. Fall through only on retryable-exhausted errors (not hard errors)

Treat a non-retryable error (e.g. a 401 on z.ai) as terminal.

**Rejected**: a dead/misconfigured first provider is exactly when you want
the next one. We fall through on *any* non-nil dispatch error except
context cancellation/timeout (which is terminal for the whole node). The
one log note per fall-through keeps a misconfigured-first-provider
situation visible rather than silent.

## Consequences

- **`RESCUE_PROVIDER` is now declarative.** `provider: "${RESCUE_PROVIDER:-zai},anthropic"`
  starts on z.ai and auto-falls-back to Anthropic with no pause/flip/resume
  ritual. The env var still works as the head-of-chain override.
- **Backends stay chain-unaware.** `task.ProviderHint` remains a single
  string; the executor owns the loop. Adding a hint-honouring backend is a
  one-line change in `providerFallbackEligible`.
  *(Superseded by [ADR-087](087-cross-backend-model-fallback-chain.md): the
  one-line change holds only for backends that swap a CREDENTIAL on an
  otherwise identical task. A backend swap re-shapes at least seven
  `delegate.Task` fields, so a cross-backend chain rebuilds the task per
  element and the chain becomes an IR property the pre-run analyses read.)*
- **One observability seam.** A new `OnProviderFallback` hook fires once
  per fall-through; the runtime can map it to a `provider_fallback` event
  later. Operators get exactly one note per route change, and the run only
  fails when the whole chain is exhausted (error names the chain).
- **Two new diagnostics.** C087 (unknown hint token, warning) and C088
  (multi-element chain on a hint-ignoring backend, warning). Both are
  warnings — the runtime degrades gracefully in both cases.
- **Deferred work is bounded and named.** Cross-API failover lives behind
  `providerFallbackEligible` + the C088 escape hatch (vary `model:` on
  claw); no architectural rework is needed to add it later.

## Amendment 2026-06-22: per-element model (`provider:model`)

Alternative #1 above ("make the chain carry full provider/model specs")
was rejected *for the original change* but flagged as the deferred "model
fallback chain". A real z.ai 5-hour rate-limit cap hit mid-dogfood proved
it necessary: a chain like `zai,anthropic` falls through correctly, but a
**provider-specific model** (z.ai's `glm-5.2`) is rejected by Anthropic on
fall-through because the hint swaps but the model does not.

We shipped the **narrow, claude_code-scoped** form of alternative #1: an
element may pin its own model with a `provider:model` token
(`provider: "zai:glm-5.2,anthropic:claude-opus-4-8"`). The objections in
alternative #1 don't apply to this scope:

- *"It overloads `provider:` with model semantics / forces repeating the
  model."* — Only chains that genuinely need a per-provider model write
  one; a model-less element inherits the node's `model:`, so single-model
  chains and the historical single-value form are byte-for-byte unchanged.
- *"It doesn't help claude_code, which can't talk to OpenAI."* — This is
  **not** cross-API failover. Both `zai` and `anthropic` speak the
  Anthropic wire API; only the model id differs. That is exactly
  claude_code's lane (the z.ai facade ↔ direct Anthropic), where the chain
  was already meaningful (`providerFallbackEligible`).

Mechanism: `resolveProviderChain` now returns `[]providerStep`
(`{Provider, Model}`), splitting each token on the **first** colon (after
env-expansion, so `${VAR:-x}`'s `:-` is never misread). `dispatchWith
ProviderFallback` swaps `task.Model` alongside `task.ProviderHint` per
element — overriding with the element's model, or restoring the node
baseline when the element has none. `OnProviderFallback` gained
`FromModel`/`ToModel`. A new warning **C172** flags a malformed
`provider:model` element (empty provider or model part). Cross-API
failover on `claw`/`codex` (a model whose provider prefix changes) remains
deferred behind the same `providerFallbackEligible` seam.

Rejected here too — a **parallel `models:` list** alongside `provider:`:
it needs a new parser token + AST + IR field and couples the two fields
positionally (lengths must match), adding a failure mode the inline form
has by construction none of. The inline `provider:model` token keeps the
DSL surface and the diff minimal.
