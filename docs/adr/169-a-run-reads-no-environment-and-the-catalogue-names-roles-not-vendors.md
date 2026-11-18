# ADR-169 — A run's settings come from the database, and the catalogue names roles, not vendors

- **Status**: Proposed
- **Date**: 2026-11-18
- **Extends**: ADR-132 (a run reads no environment), ADR-129 (typed
  per-integration routing overrides), ADR-093 (the bot-var overlay),
  ADR-122 (a model id travels verbatim)
- **Tickets**: #2699 (de-dial the catalogue, migrate the pools), #3251
  (drop the claude/gpt families)

## Context

Twenty-nine catalogue bots read their routing from `${ITERION_VIBE_*}`
expansions — 287 routing-field lines, 32 distinct dials at the last
measurement (#2699) — and production pins those dials in the runner
pods' environment (`ITERION_VIBE_*_GPT` plus `ITERION_BOT_VARS_ENV_PINS`
in the deployment's production values). The same surface carries
behaviour knobs such as `ITERION_MODERNIZE_EFFORT`.

That single mechanism costs three things at once.

- **A routing surface outside the database.** ADR-129 gave every level
  (platform, org, team, binding, launch) a typed `Override{selector,
  backend, model, provider, effort}`; the dials are a second, untyped
  seat for the same four fields, invisible to the credential preview and
  to the run's stamp, and changed by editing a Helm value and rolling
  pods.
- **An undecidable launch.** ADR-129 §4 rule 1 refuses to guess: an
  env-templated routing field is undecided when the gate runs, so a
  launch cannot answer which model or credential it will spend before
  it starts.
- **A vendor-baked vocabulary.** Revi names its two reviewers
  `reviewer_claude` and `reviewer_gpt` and selects one with
  `mono_family: claude|gpt`. On the 14 gateway products the run is
  deepseek, served through claw by a gateway key: the configuration says
  *gpt* for a model that is neither GPT nor served by OpenAI. The name
  stopped describing anything. Measured on 2026-11-18, the deployment's
  `ITERION_VIBE_*_GPT` block is exactly what routes the "gpt" reviewer
  to `claude_code`/`glm-5.3`/provider `zai` in production — a *claude*
  backend, serving GLM, selected by a var that says gpt.

The database side already exists and needs no new seam: `llmroute`
carries backend, model, provider **and** effort per selector (ADR-129),
and the per-integration `launch_vars` route every Revi slot (#2006).
What blocks the move is the catalogue itself: an env-templated field is
undecided at the gate, and Revi's empty-default provider hints make any
non-empty `providers:` list refuse its launches.

## Decision

1. **The catalogue authors literals.** A bot's routing fields (`model`,
   `backend`, `provider`, `reasoning_effort`) and its behaviour knobs
   carry a literal value. No `${ITERION_*}` dial in a routing or knob
   field of a catalogue bot.
2. **The database re-targets, at every level.** A different model,
   backend, provider or effort is an `llmroute` override cell at the
   platform, org, team, binding or launch level — the seat ADR-129
   already defined and the preview already reads.
3. **The retired names are `ITERION_VIBE_*` and `ITERION_MODERNIZE_EFFORT`.**
   Both families go from the catalogue, from the deployment's pod env and
   from `ITERION_BOT_VARS_ENV_PINS`. The bot-var overlay itself (ADR-093)
   survives as the mechanism for the knobs not yet de-dialled (its
   `ITERION_RGAA_*` family, #2699's later stage) and for engine-injected
   settings; it is simply no longer how routing is expressed.
4. **The class is closed, not only its sites.** A guard refuses a new
   `${ITERION_VIBE_*}` or `${ITERION_MODERNIZE_*}` expansion in a
   catalogue bot's routing or knob field (warning first, then the
   catalogue floor), so the surface cannot grow back one bot at a time.
5. **The review topology names roles, not vendors.** `review_mode`
   becomes `auto | single | paired`; the nodes become `reviewer`,
   `reviewer_alternate` and `converge` (each tier's variants follow);
   `mono_family` is deleted. `paired` selects the alternate through its
   routing cell, not through a var naming a vendor: which model answers
   the second opinion is a routing decision like any other.

## Consequences

- **The launch gate decides.** With literal fields the walk answers
  which model and credential a run will spend before it starts; the
  preview, the stamp and the admission read the same answer.
- **One seat per setting.** An instance that needs another model writes
  one override cell; nothing shadows it in a pod env, and a roll is no
  longer the way to re-tune a bot.
- **Production migrates in the same change, not after it.** The nine
  routing pins of the deployment become team or binding override cells
  (a routing policy per integration), then the env block and its
  `ITERION_BOT_VARS_ENV_PINS` entries are removed. The operator
  validates the result with a credential preview before the pins go.
- **Revi's vocabulary leaves `claude` and `gpt`.** Integrations carrying
  `mono_family` are migrated in one pass; there is no compatibility
  shim, and an old value is refused rather than silently honoured.
- **One consequence falls out for free:** a Revi whose routing is its
  authored literal (claw, `openai/gpt-…`) spends the key pool's codex
  forfait instead of the deployment's GLM pin — the dial removal is what
  makes the pool's forfaits reachable at all.
- **What breaks.** Every catalogue bot carries a manifest bump and the
  dials disappear from their docs; a profile that relied on an env pin
  must re-express it as an override cell (the migration list is #2699's
  step 3).

## Rejected

- **Keep the dials as harmless defaults.** They are not defaults: the
  gate cannot decide them, the preview cannot see them, and the vendor
  vocabulary they carry is the reason `gpt` can mean GLM.
- **Rename the dials (`ITERION_ROUTE_*`, `_PRIMARY`/`_ALTERNATE`)
  instead of removing them.** Same shadow surface, same undecidable
  gate, one more migration for nothing.
- **Keep `mono_family` with neutral values (`primary`/`alternate`).**
  Two knobs (`review_mode` and the family) then answer one question; the
  role-based mode plus a routing cell answers it once.
- **Auto-migrate old bot bundles on load.** A silent translation hides
  which route a run took, which is the property this ADR buys.
