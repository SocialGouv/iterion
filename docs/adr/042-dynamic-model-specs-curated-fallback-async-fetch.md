# ADR-042: Dynamic model specs over a curated fallback, fetched async

- **Status**: Accepted
- **Date**: 2026-06-23
- **Authors**: Featurly
- **Code**: [pkg/backend/model/modelspecs.go](../../pkg/backend/model/modelspecs.go), [pkg/backend/model/capabilities.go](../../pkg/backend/model/capabilities.go)

## Context

`capabilitiesForModel()` resolves a model's `ModelCapabilities` —
`ContextWindow` (consumed by reactive context compaction and the studio
context gauge), plus the `Reasoning` / `ToolCall` / `Temperature` flags. It was a
purely static heuristic table: anthropic claude reasoning heuristics, openai
o1/o3, and a per-model GLM context-window branch (glm-5.2=1M, glm-5.1/4.6=200K).

Hardcoded capabilities drift. Providers ship new models and revise context
windows faster than we edit this table — every drift is a silent
mis-compaction or a wrong gauge until someone notices and patches the constant.
The operator asked for specs to be sourced dynamically from an online
aggregator instead.

Two forces are in tension:

1. **Freshness** — we want current numbers without a code change.
2. **Correctness for brand-new models** — aggregators lag. glm-5.2 (released
   2026-06-13, 1M window) is not yet in any aggregator, so a naive
   "fetch and trust the source" would *regress* a value we already know.

And a hard constraint: spec resolution sits on the run hot path and must
**never block or slow a run** on network I/O.

## Decision

### 1. Source: models.dev, not LiteLLM

We fetch [models.dev `api.json`](https://models.dev/api.json). It is
provider-keyed (`anthropic` → `claude-…`, `openai` → `gpt-…`), which maps
directly onto iterion's `provider/modelID` spec, and it exposes all three
capability flags we need — `reasoning`, `tool_call`, `temperature` — alongside
`limit.context`, `limit.output` and `cost.input/output`. LiteLLM's
`model_prices_and_context_window.json` is a flat, provider-prefix-noisy key
space with no `temperature` flag, so it would need more normalization for less
coverage. It remains a documented secondary source; the parser is isolated
(`parseModelsDev`) so a second source can be added without touching the
registry.

### 2. Curated static table stays authoritative as fallback

The old heuristics are preserved verbatim as `curatedCapabilities()`. Merge
semantics (`specRegistry.merge`): start from curated, then overlay a fetched
spec **only when present** — a fetched `ContextWindow>0` overrides the static
one; each flag overrides only when the source provides it (non-nil pointer),
otherwise the heuristic stands. When the aggregator lacks the model entirely
(the glm-5.2 case) or the fetch fails, curated wins unchanged.

### 3. Async background fetch, never synchronous on the hot path

`capabilitiesForModel()` does only a cheap in-memory map lookup plus a
one-time disk-cache read. The network fetch runs in a background goroutine
(short 3s timeout) triggered when the in-memory table is stale; it writes a
`~/.iterion/model-specs-cache.json` cache (TTL 24h, default) that warms the
*next* resolution and the next process. Every failure path (DNS, timeout,
non-2xx, malformed JSON) is swallowed and degrades silently to curated. At most
one fetch per TTL.

## Trade-offs

| Dimension | Async fetch + curated fallback (chosen) | Synchronous fetch, trust source |
|---|---|---|
| Run latency | Zero — network is off the hot path | First node blocks up to the HTTP timeout |
| Freshness | Next run after a refresh; one TTL of lag | Immediate, but at a latency cost |
| Brand-new models | Correct — curated wins when source lags | Regresses (e.g. glm-5.2 → wrong/absent) |
| Offline / CI | Identical to today (pure curated) | Run slowed or failed by a dead aggregator |
| Failure blast radius | None (silent degrade) | A bad aggregator response can break runs |

The honest concession: a freshly-corrected upstream value is not seen until the
next run after the background refresh lands (up to one TTL late), and the very
first run on a cold cache always uses curated values. Both are acceptable
because the curated table is already a correct baseline — the dynamic layer is
an *upgrade*, never a dependency.

## Alternatives considered

### 1. Synchronous fetch on first resolution

Fetch inline the first time a model is resolved, with a short timeout.

**Rejected because**: it puts network latency (and a dead-aggregator failure
mode) directly on the run's first LLM node, violating the "never block or slow
a run" constraint. The async path gives the same freshness one run later at zero
hot-path cost.

### 2. Trust the aggregator as the source of truth (thin/no fallback)

Drop the curated table once dynamic resolution works.

**Rejected because**: aggregators lag new releases. glm-5.2 shipped a 1M window
that no aggregator lists yet; trusting the source would silently regress a known
value and mis-size reactive compaction. The curated table must remain the floor.

### 3. Embed a build-time-generated spec table (vendor models.dev at build)

Generate the table at build time instead of fetching at runtime.

**Rejected because**: it re-introduces the drift problem (specs are only as
fresh as the last build/release) and adds a build-pipeline dependency, while
still needing the curated fallback for models newer than the build.

## Consequences

- **Drift is self-healing.** Context windows and flags track the aggregator
  without a code change, refreshed at most once per 24h per process.
- **Curated values remain the floor.** Brand-new and aggregator-missing models
  resolve exactly as before; the fallback is the authoritative baseline.
- **No new failure modes on the hot path.** Offline, slow, or malformed
  aggregator responses are invisible to runs — behaviour collapses to today's
  static table.
- **Tunable and disable-able.** `ITERION_MODEL_SPECS=off` forces pure curated;
  `ITERION_MODEL_SPECS_URL` / `_CACHE` / `_TTL` / `_REFRESH` override source,
  cache path, TTL, and force-refresh.
- **Model-id matching is conservative.** Lookup is exact `provider/model` then
  bare `model`; dated ids (e.g. `claude-…-20250514`) that don't match a
  models.dev short id fall back to curated rather than risk a wrong match. A
  normalization/trim helper is a deliberate follow-up, not v1 scope.
- **The bare-`model` index is a stand-in for provider-aliasing.** It exists to
  rescue the one known cross-provider case (GLM arrives as `anthropic/glm-…`
  but lives under `z-ai` in models.dev). Because it is global, a model id
  shipped by two providers resolves last-writer-wins — low blast radius today
  (merge only overrides `ContextWindow>0` + three flags, and curated wins for
  brand-new models), but the cleaner seam if a second alias appears is a
  `canonicalProvider(provider, modelID)` normalization before lookup, keeping
  the index 1:1.
- **Pricing/max-output are surfaced and consumed** (2026-08-27). The
  follow-through that closed this: `ModelCapabilities` carries
  `InputCostPerM` / `OutputCostPerM` / `MaxOutputTokens`; the registry moved
  to the leaf package `pkg/backend/modelspecs` (ADR-093) so
  `pkg/backend/cost` can read it; the estimator prices from the published
  pair as the tier BETWEEN claw's live registry and the committed static
  table, taking it only when both rates are positive; and the studio's three
  model pickers caption their selection through
  `GET /api/model-capabilities`.
- **Rechallenge if a second source or fuzzy matching is needed.** Adding
  LiteLLM or id normalization should keep the merge-over-curated contract
  intact.
