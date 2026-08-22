# ADR-087: a model registry, and letting the operator choose the model

- **Status**: Accepted
- **Date**: 2026-08-01
- **Code**: [pkg/modelcatalog/](../../pkg/modelcatalog/catalog.go) (the crossing), [pkg/server/models_routes.go](../../pkg/server/models_routes.go) (`GET /api/models`), [pkg/modelprefs/](../../pkg/modelprefs/prefs.go) (the remembered choice), [pkg/server/model_prefs_routes.go](../../pkg/server/model_prefs_routes.go), [pkg/backend/model/model_override.go](../../pkg/backend/model/model_override.go) (the effort dimension), [studio/src/components/models/ModelPicker.tsx](../../studio/src/components/models/ModelPicker.tsx)
- **Extends** [ADR-061](061-per-backend-system-prompt-composition-mode.md) (backend composition) and the existing `ModelOverrides` launch mechanism

## Context

Nobody could say which model the studio assistant ran on, and nobody could
change it. `bots/whats-next/main.bot` pinned `model:` and
`reasoning_effort:` behind environment variables read at server start — not per
user, not per session, not visible anywhere in the UI.

Two distinct gaps sat behind that.

**The chat session was the one launch surface without model selection.** The
per-run override mechanism (`model.ModelOverrides`, `model_overrides` on
`createRun`, `--model`/`--backend`, LaunchView's dropdowns) already
existed and was already generic. `useSessionLifecycle` simply never passed it.
The only conversational surface in the product was the only launch path that
could not retarget its model.

**There was no registry, so the "picker" was a text field.**
`ModelOverridesSection` built a datalist from the detected providers'
`suggested_model` plus the nodes' own DSL defaults — hints, not a choice. An
operator could not see what they had access to, what it cost, or what it could
do. The raw material existed in Go (`model.KnownModelSpecs`,
`model.ResolveSpec`, `pkg/backend/detect`, `pkg/cli/models_pricing.go`) but was
reachable only through `iterion models`.

And the choice is **not neutral**. A model without tool-calling breaks an agent
outright — no board, no skills, no run introspection. `reasoning_effort:
ultracode` holds only on `claude-opus-4-8` and degrades silently to `xhigh`
elsewhere (C089). Presenting a flat list of names would make "I picked a cheap
model" indistinguishable from "the product got dumber".

## Decision

### 1. One crossing, one code path — `pkg/modelcatalog`

A model entry is the crossing of four sources that already existed separately:

| source | answers |
|---|---|
| `model.KnownModelSpecs` | which specs are ENUMERATED (curated; the aggregator enriches them, it does not widen the set — see `docs/models.md`) |
| `detect.Report` | what THIS host holds credentials for |
| `llmtypes.ModelCapabilities` | context window, tool-calling, reasoning |
| `cost.EffectiveRate` | what a run is actually charged |

`iterion models` was rewritten onto it rather than `GET /api/models` being
written beside it. **Rejected:** a second aggregation for HTTP. The CLI and the
studio disagreeing about whether a model is reachable is a bug class, not a
feature, and it would have been invisible until an operator compared them.

Two mappings the naive version gets wrong, both with tests:

- **The spec prefix is the API dialect, not the vendor.** GLM speaks the
  Anthropic API, so its specs read `anthropic/glm-*`, but the credential is
  z.ai's. Treating the prefix as the vendor reported GLM usable on any host
  with an `ANTHROPIC_API_KEY`, which 401s at runtime.
- **`claude_code` carries its own OAuth credential.** It makes Claude models
  usable with no `ANTHROPIC_API_KEY` at all — the single most common local
  setup — and cannot be pointed at another vendor.

Availability is computed from `detect`, never from a compiled-in list. A model
iterion has never heard of stays a legal choice (the DSL takes any
`provider/model-id`, and the curated set is explicitly not exhaustive).

**Trade-off accepted:** the catalog reports *plausible* reachability from
credential presence, not *proven* reachability. It does not call the provider.
A revoked key reads as usable until the run fails. Probing every model on every
picker open is not worth the latency or the spend, and a wrong "usable" costs
one failed node — whereas a wrong "unusable" hides a working model, which is
why the unreachable ones are listed rather than removed.

### 2. Free choice, guarded — not a restricted list

Every model is selectable. The guard is a graded warning at the point of
choosing, before launch:

- **unreachable** → blocking: no credential on this host can serve it.
- **no tool-calling** → blocking: the agent loses the board, skills and run
  introspection. This is a breakage, not a downgrade.
- **ultracode off `claude-opus-4-8`** → warning: it degrades to plain `xhigh`.

**Rejected:** hiding or disabling unusable models. Seeing that a model exists
but wants a credential is exactly what tells an operator which key to set;
removing the row removes the diagnosis.

**Rejected:** hard-blocking the launch on a missing capability. The engine
already runs whatever a `.bot` pins, and a launch-time veto in one UI would be
a rule the CLI and the API do not share. The guard informs; the operator
decides.

The host's recommended model stays visible and one click away, so a cheap pick
is undoable without remembering what the good one was called.

### 3. `reasoning_effort` becomes the fourth override dimension

A model, the backend driving it and how hard it is asked to think are one
decision. Only two of the three were re-targetable at launch, so the studio
could point the assistant at a cheaper model but not at the effort that model
is worth.

Effort now rides the existing selector machinery (node id / glob / kind / `*`),
resolved per-field like the others, applied at all three effort-resolution
sites through one `effortForNode` helper.

**Precedence, deliberately:** the override outranks BOTH the node's static
`reasoning_effort:` and a dynamic `_reasoning_effort` edge mapping — matching
model and backend, which already sit at the top of the chain. The consequence
is real and accepted: a bot that escalates effort per branch is flattened by a
run-wide `*` override. That is what asking for one means, and the alternative
(a workflow-authored value quietly outranking the operator) is worse.

Unlike a model or backend name — host state this process cannot enumerate —
the effort levels are a closed set that reaches the provider verbatim, so an
unknown one is rejected at admission rather than surfacing as an API error on
the run's first node.

### 4bis. The choice rides the queue, at the cost of a schema bump (2026-08-01)

Everything above is built on `ModelOverrides`, which the **cloud** launch path
dropped: `runview.Service.Launch` hands off to `SubmitLaunch` before the
in-process fold, and `queue.RunMessage` had no field for it. The result was the
exact façade this repo's invariants exist to catch — the operator picked a
model, `model_prefs` persisted it, the session header rendered it, and the
runner pod executed the bot's DSL default. No error, no warning; the tests
passed because they all exercised the in-process path.

The alternative considered was to scope the claim instead of the code: document
that overrides are in-process only and have the picker say so in cloud. It was
rejected — the preference is *most* useful exactly where runs are remote, and a
feature that is documented as "not on this deployment" is a feature nobody
reaches for.

So `ModelOverrides []ModelOverride` joins `Budget` and `Contributions` on
`RunMessage`, and **SchemaVersion goes 5 → 6**. That bump is the real trade-off:
`Validate` rejects on strict equality, so a version-skewed fleet fails *every*
launch, not just the minority carrying overrides. We accept it for the same
reason v=4 and v=5 did — a stale runner must fail loudly rather than run
something other than what was asked for — and it keeps the deploy contract
(publisher and runners upgrade together) honest instead of letting skew express
itself as a quality regression nobody can trace.

One conversion serves both halves: `runview.RunModelOverrides` produces the rows
stamped on the queued doc *and* published on the wire, and
`runview.ModelOverridesFromRun` is the single fold the in-process launcher and
the runner pod both go through. What the Overview shows and what the pod calls
cannot drift, because they are the same values through the same function.

**Resume replays them** (added after the first cut shipped without it). A
resume receives no launch spec, so every launch-time decision has to be read
back off the run document — and this one was not, on any of the three paths.
The consequence was invisible rather than loud: the executor fell back to the
`.bot`'s own `model:` while the studio header went on displaying the choice.

It matters most where it shows least. A conversational run pauses on its chat
node, so **every operator reply is a resume**: the chosen model applied to
exactly the first turn, and the spend landed on a provider the operator had
deliberately steered away from.

All three launch surfaces stamp the rows they parsed, `iterion run`
included — otherwise the inheritance would have been inert on the one path
with no other surface to fall back on, and the flag help would have had to
say two different things depending on where the run came from.

### 4. The preference is keyed on an opaque scope string

`pkg/modelprefs` stores `(model, backend, effort)` per `(tenant, user, key)`.
`key` is **opaque**: the studio passes a bot id and nothing in `pkg/` interprets
it.

This is the CLAUDE.md rule that the engine must never know about a specific
catalog bot. "The assistant's model" appears nowhere in Go. A second
conversational bot (#333/#334) needs a bundle, not an engine PR.

Two distinctions the obvious version loses, both surfaced in the API:

- **absent vs recorded-and-empty.** "Never chosen, fall back to the bot's
  defaults" is not the same as "deliberately chose the default". The response
  carries `set`.
- **an omitted dimension CLEARS the stored one.** Otherwise returning to the
  bot's default for one dimension is inexpressible.

Storage follows the `usernotify.PrefsStore` precedent: file-backed next to the
local run store, Mongo in cloud, both held to one contract test — a divergence
between them would only ever surface on an operator's machine.

**Trade-off accepted:** a server with no prefs store 404s, and the studio
degrades to a per-tab choice rather than blocking. A model preference is
convenience; refusing to launch because it cannot be remembered would be
absurd.

## Consequences

- `iterion models` gained reachability and price columns, and its `--json`
  envelope is now `modelcatalog.Catalog` (the previous `{refreshed, models[]}`
  shape is a subset — `models[]` kept its fields and gained more).
- `iterion models` now probes host credentials, which on a host with a
  `claude` CLI and no credentials file may shell out once (3 s cap).
- `GET /api/models` reveals capability values and credential SOURCE names
  (`ANTHROPIC_API_KEY`), never credential values — pinned by a test at the HTTP
  boundary.
- `--effort-for` exists on `run` and `resume`. On resume it layers over the
  run's persisted rows per field (`ModelOverrides.MergeOver`), so re-targeting
  the effort alone does not discard the launch's model.
- `RunModelOverride` gained an `effort` field, so a run's Overview shows the
  effort it launched with.
- The assistant's `ITERION_WHATS_NEXT_MODEL_CLAUDE` /
  `ITERION_WHATS_NEXT_EFFORT_CLAUDE` env vars still work and still set the
  floor; the preference overrides them per user.
- **The queue schema is v=6.** A publisher and a runner at different versions no
  longer interoperate at all: the runner rejects the message rather than
  executing it with the overrides missing. Deployments must upgrade both halves
  together (see §4bis).
- `queue.RunMessage` now carries model/backend/provider/effort, so a cloud run's
  launch-time choice is applied by the pod and not merely displayed —
  `SubmitResume` republishes it too, so it survives every turn of a
  conversational run and not only the first.
