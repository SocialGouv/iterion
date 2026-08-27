# ADR-093: The model-spec registry is a leaf package, and its published prices are a cost tier

- **Status**: Accepted
- **Date**: 2026-08-27
- **Authors**: Claude
- **Completes**: [ADR-042](042-dynamic-model-specs.md)
- **Code**:
  [pkg/backend/modelspecs/modelspecs.go](../../pkg/backend/modelspecs/modelspecs.go)
  (`Registry`, `Default`, `SetDefault`, `NewSeeded`),
  [pkg/backend/model/modelspecs_merge.go](../../pkg/backend/model/modelspecs_merge.go)
  (`mergeSpec` — curated-is-the-floor stays here),
  [pkg/backend/cost/cost.go](../../pkg/backend/cost/cost.go)
  (`specRate`, `specLookup`),
  [pkg/server/models_routes.go](../../pkg/server/models_routes.go)
  (`GET /api/model-capabilities`)

## Context

ADR-042 shipped a dynamic model-spec registry that parses and caches, per
model, the context window, the max output tokens, the published input/output
prices, and three capability flags. It surfaced the window and the flags. It
also, in its own closing words, left the rest "cached but not yet surfaced …
parsed and persisted for future use."

Nothing read them for months. Meanwhile `pkg/backend/cost` — the estimator
that writes `_cost_usd` onto every generation output — resolved a price from
claw's live registry and then from a hand-maintained static table. So a model
models.dev published a price for could still report **no cost at all**, and
`_cost_usd`'s absence is documented to mean "unknown", which readers
routinely see as `$0`.

Two things stood in the way.

**The dependency is inverted.** `pkg/backend/cost` is a true leaf — `go list
-deps` shows zero iterion imports — and it is a leaf *because*
`pkg/backend/model` imports it from five call sites. The registry lived in
`model`. The estimator could not reach it.

**The registry could not be isolated in a test.** It was a package var built
from the environment at import time. In-package tests worked only by mutating
or swapping that unexported var directly. A `cost` test setting
`ITERION_MODEL_SPECS_CACHE` would run *after* construction and silently
resolve against the developer's own `~/.iterion` cache — an assertion about a
dollar figure, decided by whatever that machine last fetched.

## Decision

**1. Extract the registry into `pkg/backend/modelspecs`, a leaf.** Its only
iterion dependency is `pkg/store` (for the atomic cache write), whose own
dependencies are `git`/`appinfo`/`log` — so `cost → modelspecs` closes no
cycle. `merge()` does NOT move: deciding what the aggregator may override is
a `model` policy, and it belongs next to the curated table it overrides. The
new package *supplies*; it does not decide.

**2. Ship the test seam as the point of the move, not a nicety.** `Default()`
is built LAZILY (so the environment is read at first use, not at import),
`SetDefault(*Registry) (restore func())` swaps it for a test in any package,
and `NewSeeded(map[string]Spec)` builds a fixture registry bound to neither
disk nor network. Without these, extraction would have *removed* the only
isolation mechanism the code had.

**3. The published pair is the cost tier BETWEEN claw's live registry and the
static table.** Claw keeps precedence, so no run's charged rate moves because
iterion started reading its own aggregator. The table remains the offline
last-known-good.

**4. A published pair is used only when BOTH rates are positive.** The two
are parsed independently and zeroed independently by the consensus filter, so
a half-known pair is routine. A half pair falls through **whole** to the
table.

**5. A bare model id is served, not rejected**, by both the estimator and
`GET /api/model-capabilities` — from the consensus-filtered bare index, with
the capability flags omitted rather than resolved under an invented provider.

## Alternatives rejected

**`cost.RegisterSpecPricing(fn)`, called from `model`'s init.** No package
move, no import-graph change — and that is the problem. The wiring is
invisible at the point of use, and any binary that links `cost` without
linking `model` silently loses the tier: `iterion` would price one way and a
smaller tool another, with nothing to read that explains why. Explicit
imports over init-time side effects.

**Aggregator first, claw second.** Defensible on the merits — models.dev is
consensus-filtered across publishers, while OpenRouter is exactly the kind of
multi-provider source that filtering exists to neutralize. Rejected anyway:
it changes the rate every claw-answered run is charged at, and this file's
standing rule (see `pkg/cli/models_pricing.go`) is that a price change is
committed by a human, not slipped in by a refactor. The counter-argument is
recorded in the package doc so the next reader can take it up deliberately.

**Take a half-published pair.** The missing rate would price at zero. That is
the precise shape of the failure this area already shipped once — an unlisted
model reporting no cost while a run burned real money — and a rate mixing one
source's input with another's output is traceable to neither.

**Infer the provider from a node's backend** (for the studio endpoint).
Backend is not provider: claw and claude_code both serve anthropic, pi serves
some three dozen. An inferred provider attaches one vendor's numbers to
another's model. An explicit `provider=` param is honoured; nothing is
guessed.

## Consequences

- **A model the aggregator prices is priced.** Verified on the built binary:
  `glm-5.2`, which has no static-table entry by design (24 publishers, rates
  from 0 to 1.44/M), prices at the consensus 0.6/2.2 with claw disabled.
- **The pricing audit narrows, deliberately.** `EffectiveRate` now consumes
  published pricing, so `DISAGREES` means "claw's live registry quotes
  something other than models.dev" — it can no longer mean "the committed
  table is stale" — and `IGNORED` narrows to the half-published pair. The
  verdict comments, the `--check` text and `docs/cli-reference.md` say so,
  and the audit gained its first test file: it reported on budget decisions
  with nothing asserting it.
- **Every cross-package test of price or capability is hermetic** through
  `SetDefault`/`NewSeeded`, instead of reading the host's cache.
- **`cost.EstimateUSD` now touches the disk cache** on a miss of claw's
  registry, through the same non-blocking `ensureFresh` the capability
  resolver already used: one lazy read, then map lookups, with the network
  fetch strictly a background goroutine. No new blocking path.
- **`ITERION_MODEL_SPECS=off` now also disables the cost tier**, which is the
  honest reading of "no dynamic specs" and is documented in the package doc
  beside `CLAW_DISABLE_LIVE_REGISTRY=1`.
- **Still not consumed: `MaxOutputTokens` as a request-shaping input.** It is
  reported (CLI table, API, studio caption) but no backend sizes a request
  against it. Zero means unknown, never "no cap", so a consumer must decide
  what to do with an absent figure before that can change.
