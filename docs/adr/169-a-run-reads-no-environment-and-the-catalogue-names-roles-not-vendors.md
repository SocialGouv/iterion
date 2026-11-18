# ADR-169 — A run's settings come from the database, and the catalogue names roles, not vendors

- **Status**: Proposed
- **Date**: 2026-11-18
- **Extends**: ADR-132 (a run reads no environment), ADR-129 (typed
  per-integration routing overrides), ADR-093 (the bot-var overlay),
  ADR-122 (a model id travels verbatim)
- **Tickets**: #2699 (de-dial the catalogue, migrate the pools), #3251
  (drop the claude/gpt families)

## Context

The catalogue reads its routing from `${ITERION_*}` expansions: 32 bots
read 141 distinct names, on 287 routing-field lines at #2699's last
count. Only 13 of those bots use the `ITERION_VIBE_*` family; the rest
read per-bot families such as `ITERION_ASSESSMENT_*_MODEL`,
`ITERION_COPILOT_*`, `ITERION_ADR_MODEL_CLAUDE`, `ITERION_MODERNIZE_*` and
`ITERION_RGAA_MODEL_*`. Production pins some of those dials in the runner
pods' environment (`ITERION_VIBE_*_GPT` plus `ITERION_BOT_VARS_ENV_PINS`
in the deployment's production values), and the same surface carries
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
3. **Every `${ITERION_*}` dial in a catalogue routing field is retired**
   — `model`, `backend`, `provider`, `reasoning_effort` and a fallback
   route's own — whatever its family; `ITERION_RGAA_MODEL_*` is routing,
   not a knob. `ITERION_VIBE_*` and `ITERION_MODERNIZE_EFFORT` go
   **first**, because they are the ones the deployment pins today: they
   leave the catalogue, the pod env and `ITERION_BOT_VARS_ENV_PINS` in the
   first delivery, and the other families follow bot by bot under the same
   rule. The bot-var overlay itself (ADR-093) survives for knobs that are
   not routing — a threshold, a cap, a flag — and for engine-injected
   settings; it is simply no longer how routing is expressed.
4. **The class is closed, not only its sites.** The guard is the one
   ADR-132 §6 already names: C315 warns on any `${ITERION_*}` in a routing
   field, and for a catalogue bot it becomes the floor once that bot is
   de-dialled — so a de-dialled bot cannot grow a dial back, and no new
   bot ships one. A family is not closed by retiring its sites while a
   sibling family stays open.
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
- **Production migrates before the de-dial, not after it.** The nine
  routing pins of the deployment are first written as override cells
  (platform, team or binding, per ADR-132 §7) and proved by a credential
  preview while the env pins still say the same thing; only then is the
  catalogue de-dialled, and the env block and its
  `ITERION_BOT_VARS_ENV_PINS` entries are removed last. De-dialling first
  would send a sovereign pool's spend to the authored literals — off its
  gateway — for as long as the cells are missing.
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
