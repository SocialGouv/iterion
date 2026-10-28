# ADR-132 — A run reads no environment: every setting it observes is a database level, the catalogue loses its dials, and the run's settings arrive frozen at claim

- **Status**: Proposed (the operator's doctrine, 2026-10-22, is binding — this record designs its
  shape for review before any code)
- **Date**: 2026-10-22
- **Deciders**: jo (the doctrine and its scope, arbitrated 2026-10-22), Claude (inventory, design)
- **Note on the number**: 131 is the highest published ADR; this record claims **132**. Two
  parallel sessions may take neighbouring numbers the same day (the debt chantier reserved
  ADR-132 here and names a sister `route-selection-data`); a renumber at rebase costs a `git mv`,
  a `grep -rn ADR-132` and `task map:gen`, which regenerates the index row — `map-docs-adr.md`
  does not exist as a hand-edited file: the ADR's index line is a row of
  [`docs/references/map-docs.md`](../references/map-docs.md), generated.
- **Extends**: [ADR-121](121-adaptive-routing-policy.md) (the multi-level routing policy, its one
  fold and its run snapshot), [ADR-129](129-per-integration-typed-routing-overrides.md) (typed
  `overrides` + the `providers` allowlist; §8 named this day), [ADR-093](093-bot-vars-runtime-settings.md)
  (the bot-var overlay — the mechanism this record retires for runs), [ADR-090](090-runtime-operational-settings-db-backed.md)
  (env = deployment default, DB = runtime override), [ADR-130](130-env-expansion-opt-out-for-tool-bodies.md)
  (a tool body the engine leaves verbatim), [ADR-131](131-tenant-gateway-credential.md) (a team's
  own OpenAI-compatible gateway credential)
- **Serves**: [#2699](https://github.com/SocialGouv/iterion/issues/2699), epic
  [#2000](https://github.com/SocialGouv/iterion/issues/2000)'s tenancy line; co-owned with the
  debt plan's `env-doctrine-engine-half` stream (#2018 #2025 #2244 — its wave 1 converts the
  `ITERION_` reads to the chokepoint byte-identically and waits for this record's §3 and §4)

## Context

**The doctrine (operator, 2026-10-22, binding):** a run must not depend on environment variables.
Every setting a run reads — its routing (model, backend, provider, effort) AND the behaviour knobs
bots read today through `${ITERION_*}` dials — comes from a database level: platform, org, team,
binding (integration / schedule), bot manifest, launch. Only the engine PROCESS's own knobs stay in
the environment (database connection, ports, internal bounds such as `ITERION_PROC_WAIT_DELAY`,
#2469, explicitly kept).

**What the catalogue reads today (measured on this tree, `65b3b1ee7`).** 307 `${ITERION_*}`
occurrences across 32 `.bot` files. 287 sit on routing-field lines — 133 `model:`, 122
`reasoning_effort:`, 21 `backend:`, 11 `provider:` — reading 139 distinct names
(`bots/review-pr/lib/nodes.bot` alone: 29 lines, `model: "${ITERION_VIBE_MODEL_EMIT:-claude-opus-5-5}"`
at :36, `provider: "${ITERION_VIBE_PROVIDER_GPT:-}"` at :284). Five non-routing lines carry the
rest, and they classify cleanly (§1): three bot knobs written as var defaults
(`bots/rgaa-audit/main.bot:2106`, `bots/assessment/main.bot:81`, `bots/sec-audit-source/main.bot:6196`),
one tool command that forwards effort dials into a child's env as reporting data
(`bots/review-pr/lib/nodes.bot:478`), and one engine-process knob read through a tool body
(`bots/adr-cartograph/main.bot:899`, `ITERION_DISPATCHER_PORT`). The debt inventory's broader count
(251 run-reachable `os.Getenv`/`LookupEnv` sites, 87 of them `ITERION_`-namespaced, 11 through the
chokepoint; 71 vendor-credential reads; 21 bare `os.Environ()` forwards) is the same defect seen
from the engine side, and §1 gives every one of them a verdict class.

**The failure that dated this record.** On 2026-10-22 review-pr 0.9.16 (#2467) introduced the
`_EMIT_GPT` / `_SUPERVISOR_GPT` dials. The sovereign runner pools did not pin them, so a mono-GPT
review there would have died on the authored default `openai/gpt-6-sol`
(`nodes.bot:118`) — and nothing in the tree could have said so, because an env pin belongs to a
deployment, not to a record any gate reads. The stopgap was a pin in the pools' pod env
(infra-apps `864e62c`, to be removed by this work). That is the drift class in miniature: the
catalogue grew a twelfth slot dialect and the only place it was configured was a pod spec three
repositories away from the `.bot` that reads it. The same day produced a second data point: the
vao pool's review-pr run `01a16360…` ran NOTHING because a record beat its env dial at the
overlay, and #2702 hotfixed it by inverting the precedence for pinned names (env ahead of record,
`ITERION_BOT_VARS_ENV_PINS`). A doctrine that ends the dial ends the whole class: with one
carrier there is no precedence to invert, twice a day.

**What blocks the database path from serving runs today.** The database side exists — ADR-121's
policy folds at every level with one chokepoint (`pkg/server/route_resolve.go:47`), ADR-129's
`overrides` and `providers` ride it, and ADR-093's bot-var settings reach every `${…}` expansion
through `ir.SetEnvOverlay` (`cmd/iterion/server.go:364`, `cmd/iterion/runner.go:252`). Three facts
keep runs on the environment anyway:

1. **An env-templated routing field is UNDECIDED at the launch gate.** `envDeferred`
   (`pkg/backend/model/override_fold.go:463`) treats a `${…}` that expands to nothing on the
   walk's process as the runner's to resolve: `envSeen`, `narrowSafe = false`. A non-empty
   `providers:` list then refuses the launch naming the unreadable field (ADR-129 §5), and
   review-pr's empty-default provider hints (`${ITERION_VIBE_PROVIDER_GPT:-}`, #2665 item 4) make
   that the shipped shape — the gate cannot prove a pin while the catalogue defers to a pod.
2. **The overlay is a process-global TTL resolver, not a run's.** `envOverlay`
   (`pkg/dsl/ir/validate_nodes.go:1113`) is one atomic pointer per process, consulted by every
   concurrent run, converging within a 30 s TTL. ADR-093 said so honestly: propagation is per NODE
   boundary, "a run longer than the TTL can straddle a change (node 1 old value, node 30 new)".
   #2025's measured bug is that straddle: an operator raises a team's model override mid-run and
   the budget clamp, the next node's build and the router each see a different value. A global
   cannot express a per-run snapshot.
3. **A stored bot var is refused exactly where a run's knobs live.** `botVarsInfraPrefixes` and
   `botVarsInfraExact` (`pkg/platformcfg/platformcfg.go:145`, :207) refuse the spend and ceiling
   names — `ITERION_CLOUD_`, `ITERION_ORG_DEFAULT_`, `ITERION_LOOP_BUDGET_GUARD`'s family — with
   the reason that "a stored bot var never tunes the engine's patience … the operator's
   environment does", so a bot can never raise its own ceiling. The reason is right and the
   spelling is wrong: those knobs are run settings whose honest home is a level ABOVE the bot, not
   the pod environment. The refusal list is a classification waiting to be re-read (§2).

**Why not stay here.** The gate's blindness and the overlay's straddle are not independent debts:
both exist because the run's configuration is resolved LATE (at each expansion, from whatever the
process happens to hold) instead of ONCE (at claim, from records). One move fixes both, and it is
the move the tenancy work already made for credentials — ADR-121/129 froze the routing policy on
the run document and replayed it; ADR-131 moves the gateway key from the pod env into a team
record. What is left on the environment is the catalogue's dials and the knobs, and this record
is their retirement.

## Options considered

- **1. Per-knob settings fields (a typed field per dial).** 139 names become 139 validated
  fields. *Pros*: maximal typing; each knob's write surface states its own vocabulary. *Cons*:
  the Nth bot re-opens the engine (philosophy §2's failing variant — an engine `if` per knob);
  the routing dials already have a typed home (the policy's `overrides` cells) and would
  duplicate it; a new dial costs a release in every direction. *Where it wins*: never as the
  mechanism; individual knobs that deserve typing graduate INTO the families that exist
  (usage caps, sandbox image did exactly this).
- **2. Levels inside the existing bot-var overlay (team/org records behind `SetEnvOverlay`).**
  One resolver folds platform < org < team bot-var records; the overlay stays. *Pros*: smallest
  diff; every expansion site keeps working. *Cons*: it inherits all three blockers — the gate
  still cannot read a dial (1), the overlay is still global and TTL-bound (2), and a flat
  `ITERION_` name space is a naming convention masquerading as governance: nothing stops a team's
  `ITERION_VIBE_MODEL_CLAUDE` value from reaching every other team's runs. Rejected as the
  mechanism; its one good half — more levels feeding the resolver — is kept in §2/§3.
- **3. The policy + a resolved document (chosen).** Routing settings ride the routing policy they
  already have; behaviour knobs ride the bot-var ladder, which gains the org and team levels and
  loses the environment; every run's settings are folded ONCE at claim into a document that rides
  the queue message and replays frozen; the catalogue is de-dialled so the gate reads literals;
  the pod env is retired from the run's reading by a staged, loud transition.
- **4. Do nothing.** The 2026-10-22 incident is the steady state: every new bot slot re-opens it,
  the sovereign pools accumulate pins no gate sees, and ADR-129's acceptance line stays a test
  over a catalogue that cannot pass it.

**Arbitrated 2026-10-22**: option 3, ADR first, then slices ordered so that no sovereign run ever
falls back to a non-sovereign default in between (§7 is that ordering).

## Decision

### 1. The rule, stated so a gate can enforce it

The classification answers one question: **who does the knob bind?**

- **The RUN binds** — the value changes what a run's text, routing, tools, budgets or verdicts
  observe: it is a run setting. Its sources are the database levels (§2), resolved once per run
  (§3). On a cloud process the environment is NEVER a source for it — not as a default, not as a
  fallback, not through a tool body's `${…}`.
- **The PROCESS binds** — the value bounds or configures the engine process itself (the store
  URI, the HTTP port, `ITERION_PROC_WAIT_DELAY`'s child-drain bound, the scheduler's cadence, the
  board claim reaper): it keeps its environment, and each one is named in the classification test
  so the next knob classifies itself instead of inheriting a neighbour's answer.
- **The engine CONSTRUCTS a child's environment** from database values and process config: that
  is a write, not a run read, and stays (a tool process's `PATH`, a sandbox's `ITERION_RUN_*`).
- **Credentials** are not settings. On the cloud plane they already leave the environment (the
  tier walk, the sealed bundles, ADR-131's team records); the process-env policy refuses
  credential-shaped reads to workflow text (`platformcfg.go:546`). On the LOCAL lane the
  operator's own environment stays a credential source — the env-funded class (#2038) is local
  parity, unchanged by this record.

The verdict is written once per name, in a classification test, in three files that must agree:
the table in `pkg/platformcfg` (machine-checked), the page in
[`docs/agents/engine/tenancy-credentials-spend.md`](../agents/engine/tenancy-credentials-spend.md),
and the deployment inventory this record's plan carries. The load-bearing verdicts:

| name(s) a run reaches today | class | verdict |
|---|---|---|
| `${ITERION_VIBE_*}` (32 names), `${ITERION_<BOT>_*}` routing dials (107 more) | run setting — routing | catalogue de-dialled to literals (§5); the tenant's pin is a policy `overrides` cell (§2) |
| `${ITERION_RGAA_BACKEND_ADJUDICATE}` / `_MODEL_ADJUDICATE` / `_BACKEND_REFUTE` / `_MODEL_REFUTE` / `_MODEL_PUBLISH` (egapro pool) | run setting — routing | team-level `overrides` cells, exact node selectors (§7) |
| `ITERION_LOOP_BUDGET_GUARD`, `ITERION_OUTPUT_CORRECTION_BUDGET`, `ITERION_NODE_MAX_RETRIES`, `ITERION_BUDGET_EXIT_GRACE`, `ITERION_BRANCH_CANCEL_GRACE` | run setting — ceiling | bot-var ladder ABOVE the bot (org/team/platform); the bot can read, never write (§2) |
| `ITERION_RGAA_MAX_DURATION`, `ITERION_SEC_AUDIT_MAX_DURATION`, `ITERION_ASSESSMENT_GATE_PROBE_TIMEOUT_S` | run setting — knob | authored literal (§5); per-tenant tuning on the ladder |
| `ITERION_PLATFORM_PAIR_ORDER` / `_TRIGGERS` / `_REFUSED_PINNED_KEY` / `_ROUTING_STRICT` (`pkg/llmroute/env.go:17`) | run setting — routing default | the platform settings record is the carrier; the env spelling retires with the same loud transition (§4) |
| `ITERION_SANDBOX_DEFAULT` / `_DEFAULT_IMAGE` | deployment posture (carve-out below) | the platform `sandbox` settings record carries the value (ADR-090 family); the env spelling retires with §4's stages like any run setting |
| `ITERION_USAGE_CAP_*` percentages | run setting, ADR-090 family | the platform `usage_caps` record carries them; the env spelling retires with §4 |
| the two MODES and the percentages | platform posture — kill-override | MIGRATED into the platform `usage_caps` record in §2's slice (S4): they are platform governance, and today they are read from `os.Getenv` per resolution and BEAT the record by design (`usagecap/settings.go`: "the kill switch WINS over a DB percentage"). Env is not their "default", it is a kill-override; the doctrine-clean home is the record the override overrides |
| `ITERION_USAGE_CAP` (the switch alone) | process config — enforcement brake (**D22**, operator review 2026-10-22) | KEEPS its environment, reclassified per §1's process class like `ITERION_PROC_WAIT_DELAY`: it bounds the runner process's enforcement posture, not a run setting, and it is the one out-of-band spend brake that works when the control plane it brakes against is the thing that is down. The refusal lists keep refusing it to stored vars |
| `WEB_SEARCH`, `PLUGINS_ENABLE`, `ITERION_PLUGIN_FIRECRAWL_API_URL` | deployment posture (carve-out below) | keep their environment — the operator's cost/abuse decision for a platform-funded capability, identical for every tenant; a tenant rung would let one team spend the deployment's scrape budget |
| `ITERION_BOT_VARS_ENV_PINS` (#2702's chart key, with `platformcfg.PinnedOverlay` / `EnvOverlayFor`) | process config — precedence mechanism of the transition | process env of the engine, retired at §7 with the pool env block: its purpose (a pinned name answers env AHEAD of the record) is the precedence this record inverts, and it exists only to keep the pools' pins authoritative until §7 removes them. §1's rule has no second run-settings hatch: while the pin list exists the record is SHADOWED for those names (the §4.4 warning names the shadowing), and the list dies with the pins |
| `ITERION_CLOUD_*` budgets/enforcement (`MAX_COST_USD`, `MAX_DURATION`, `MAX_ITERATIONS`, `MAX_TOKENS`, `MAX_PARALLEL_BRANCHES`, `RETRY_*`, `REQUIRE_LLM_CREDENTIAL`, `SCHEDULE_GUARDS`, `BOARD_TICK`, `MARKETPLACE`, `ENV_PASSTHROUGH` beyond the hatch), `ITERION_ORG_DEFAULT_*` | platform governance — ceilings and switches | stay REFUSED to the ladder (they remain in the refusal lists): the code is explicit that they are the operator's hard, tenant-unraisable caps (`pkg/runner/loop.go`: "a hard, tenant-unraisable cap"; `pkg/retrypolicy/env.go`: "applied last, can only lower — an operator raising a default and a platform capping a tenant are opposite intents"). If a record form is wanted, it is the `usagecap.Settings` shape: platform rung, super-admin write — never team/org. The bounded party must not write the bound; "above the bot" includes the tenant |
| `ITERION_BOTS_PATH` (as a source-of-record fallback) | none for long | deleted by #2696, not migrated |
| `ITERION_DISPATCHER_PORT` (read by `adr-cartograph`'s tool body) | process knob reached through run text | the engine CONSTRUCTS it into the tool process's env (a write); the body reads it bare; the dial form retires with the catalogue floor |
| `ITERION_PROC_WAIT_DELAY` (#2469) | process bound | keeps its env — the reference example of what the doctrine keeps |
| `ITERION_CLOUD_ENV_PASSTHROUGH` | process config — the hatch | stays, as the escape hatch of §8 |
| `ANTHROPIC_API_KEY`, `ZAI_API_KEY`, … (71 vendor reads) | credentials | local lane keeps them (env-funded); the engine's expansion never answers them (the process-env policy + the refusal lists, unchanged); on iterion.cloud the pods carry ZERO keys — every key is a record (D20, §7), unconditionally — until then a tool body the engine leaves verbatim (ADR-130) expands in the shell from the child's INHERITED env (`executor_tool.go` spawns with `os.Environ()`; the scrub deliberately excludes provider keys), which is GO-ENV-03's residual, named here as the window this record leaves open until §7 and the allowlist builder close it |
| `SMTP_*`, `CANONICAL_REDIRECT`, `CODEX_VERSION`, `OAUTH_FORFAIT_*`, `BOARD_CLAIM_REAPER`, … (the server/runner deployment's remaining names) | process / enforcement config | keep their environment; out of a run's reach (`botVarsInfraExact` already refuses them to stored vars) |

**The deployment-posture carve-out, stated once:** a knob the operator sets per DEPLOYMENT,
identical for every tenant, whose effect is run-observing (which tools exist, whether a
platform-funded capability is on) is process config even though a run observes it — the bounded
party (a tenant) cannot write it and the write rung it would need (team) is exactly what the
ladder refuses. What this carve-out does NOT cover: anything whose value varies per tenant, and
any name the refusal lists already hold — those keep their verdicts.

The rule a gate can enforce is then exactly: **on a cloud process, a `${…}` or direct read that
resolves from `os.Getenv` is a defect** — for run settings and for the carved-out rows alike,
once §7's final state removes the deployment-posture reads; §4 stages up to refusing it; §6 is
the compile-time half. The usage-cap kill switch and modes leave the exception set when S4
migrates them into the record.

### 2. Where each setting lives

**Routing per slot** — model, backend, provider, effort: the routing policy's typed `overrides`
cells, at the levels ADR-129 shipped (platform < org < team < shipped bot < binding < run), folded
by the one chokepoint, snapshotted and replayed as ADR-121 already does. ADR-129 §8 said "the
platform level of `overrides` is the successor of the `${ITERION_VIBE_*}` dials, and nothing is
removed to say so" — this record removes them. The mapping rule is by REACH, not by name: a dial
whose reach is deployment-wide (every bot reading `ITERION_VIBE_MODEL_CLAUDE`) maps to a
platform-level cell with a kind selector (`agent` / `judge`); a dial one tenant pins maps to that
tenant's team-level cells; a dial one integration pins maps to the binding's block. §7 writes the
three production mappings concretely.

**One gap this record closes rather than leaves**: the dials also reach RESCUE routes —
`model: "${ITERION_VIBE_MODEL_CLAUDE_FALLBACK:-glm-5.3}"` on a `fallbacks:` entry
(`nodes.bot:93`), and `baselineModel` (`executor_resolve.go:341`) lets a node-level cell fill a
route that names no model of its own but not re-pin one that does. An `overrides` cell has no
route-scoped selector, so after de-dial a tenant cannot pin a rescue tier — and its own allowlist
would then prune the authored `glm-5.3`/`zai` rescue as outside its funded providers, which is
correct tenancy and a lost safety tier at the same time. So `overrides` gains the route-scoped
selector the site vocabulary already spells: `node#name`, the exact key `ProviderResolution.Pins`
uses for an authored route (`override_fold.go:98`, :293). A cell `{selector: "reviewer_claude#glm",
model: "openai_compatible/scaleway/glm-5.3"}` pins that rescue route through the same fold, the
same locks, the same provenance map — no second mechanism, the Nth-variant test answered by
extending the existing one. Two guards the route-scoped selector owes, from the plan review: it
carries a BOT dimension (`rgaa-audit/publish#<route>`) — node ids are bare per-workflow names and
collide across bots at scale (`campaign` is declared in 11 bots), so a team-level cell without
the bot dimension re-pins every bot's same-named node that team runs; and a route-scoped cell
pins at least one field that exists on a route (`backend`, `model`, `provider` — `effort` is a
node field, not a route field, and a pin list made of fields a route lacks would be a silent
no-op). The fold records per-cell match counts in the provenance map with a once-per-name
warning on zero match (a typo'd selector matches nothing and nothing else would say so).
RESOLVED by the operator's review (**D21**, 2026-10-22): the route-scoped rescue cells SHIP — the slices that carry them are unconditional. A team pins its own rescue tier inside its funded providers; without them, a tenant allowlist prunes the authored rescue and a degraded slot becomes a dead one.

**Behaviour knobs per bot** — durations, timeouts, budgets a TENANT may want to tune: the bot-var
ladder. `FamilyBotVars` gains the org and team levels: the same `org:<id>` / `team:<id>` id
scheme and write-role shape as `llm_routing_policies`, while the platform rung stays the
`bot_vars` family document in `platform_settings` — not a new collection; the same CAS on
`updated_at`, the same validated write surface per rung (super-admin for platform;
`canManageTeam` / `canManageOrg` for the new ones), the same audit rows. The fold is
**platform < org < team**, read by the resolver the claim freezes (§3); the binding level of a
knob is the provisioning record's `launch_vars`, which exists; the launch level is the dedicated
typed flags the CLI already ships (`--loop-budget-guard`, the `Budget` block, …) — NOT a generic
`--var` spelling, which the workflow-var gate would refuse anyway (`--var` accepts only declared
workflow vars) and which must never grow to accept `ITERION_*` names without walking the refusal
lists, because the launch rung is the one closest to the bounded party. A
knob's name space stays `ITERION_[A-Z0-9_]+`, credential-shaped names stay refused, and the value
charset stays the one `botVarValueRune` already enforces — a stored value is substituted raw into
bodies today and into nothing else tomorrow.

**The refusal list, re-read.** The rule ADR-093's addendum wrote — a stored bot var never lifts a
guard, configures the process, names an outside identity, or forges an engine-written name — is
kept verbatim. What changes is that the list SPLITS by the §1 classification instead of refusing
wholesale: names that are **guard or process** (`ITERION_PERMISSION`, `ITERION_PI_TRUST_PROJECT`,
`ITERION_BOARD_`, `ITERION_RUNNER_`, …) stay refused at every level, exactly today's lists; names
that are **platform governance** — every `ITERION_CLOUD_*` ceiling, enforcement switch and retry
cap, every `ITERION_ORG_DEFAULT_*` — stay refused too (§1's table row: the code names them "a
hard, tenant-unraisable cap" and "applied last, can only lower"; a team-wins ladder cannot
express can-only-lower, and "above the bot" includes the tenant). What LEAVES the list is the
third class: the genuinely tenant-tunable ceilings (`ITERION_LOOP_BUDGET_GUARD`,
`ITERION_OUTPUT_CORRECTION_BUDGET`, `ITERION_NODE_MAX_RETRIES`, `ITERION_BUDGET_EXIT_GRACE`,
`ITERION_BRANCH_CANCEL_GRACE`) and the bot knobs (`ITERION_RGAA_MAX_DURATION`, the audit-gate
probe timeout, …). Two rules follow the names up the ladder: **ceilings fold down-only** — a
lower rung may lower a ceiling, never raise it (the red-first test: the platform sets a cap, a
team rung tries to raise it, the effective value stays the platform's) — and **the verdict is
per NAME, never per prefix** (the classification test holds it per literal name; the code's own
comment on `ITERION_CLOUD_RETRY_*` is the reason: "one variable cannot serve both" intents).
The runner-side readers migrate in the same slice as the server-side ones —
`pkg/runner/loop.go`'s `applyCloudBudgetCeiling`, `pkg/runner/subbot.go`, `pkg/retrypolicy/env.go`
— so a ladder value never silently does nothing where it binds most.
`TestEveryEngineEnvNameIsClassified`,
`TestGuardNamesAreNeverBotVars` and `TestEveryShippedBotKnobIsABotVar` keep holding the whole
classification, re-pointed at the split.

### 3. The one resolver, and the resolved document

**The read contract, declared once (the debt plan's G65), as the operator refined it the same
evening (disposition D20):** a setting resolves **DB levels > default** on a cloud run, and
**process env > default** on a local run — and on the cloud half the DEFAULT itself may come
from the environment: **env may define defaults, as the lowest rung of the chain
(DB levels > env default > built-in default), but carries no OVERRIDE** — it never answers a
routing slot or a knob, because the dials die from the catalogue side (§5) and the levels live
in records (§2). And the hard half of D20: **zero credentials in the environment on the
iterion.cloud deployment** — every key is a record (ADR-131's tier), and no hatch unlocks a key
back into the pods there (`ITERION_CLOUD_ENV_PASSTHROUGH` remains the self-hosted operator's
hatch; iterion.cloud does not use it). On main since
2026-10-22 the chain carries #2702's pin mechanism in front of it: a name listed in the chart's
`ITERION_BOT_VARS_ENV_PINS` answers from the process env AHEAD of the record (`platformcfg.
EnvOverlayFor` / `PinnedOverlay`), the exact inversion of the precedence this record ends — it
is the transition's precedence config for the pools' pins (§1's table row) and dies with them
(§7). The local-floor citation of G65 stands in the debt plan's stream; #1690 itself is a parser
allocation ticket and cites nothing here.

**The document.** At submit, the launching process folds the run's settings — the routing policy
it already snapshots (`store.RunLLMRoutePolicy`, `pkg/store/run.go:573`, :1100, which already
carries the resolved routing block) plus the bot-var ladder for the run's tenant —
into one resolved settings document, stamped on the run record and carried on the queue message
beside the compiled IR, in the shape `Contributions` already established
(`pkg/queue/types.go:230`): a present-but-empty document is a statement ("no setting is set for
this run"); a MISSING document is the pre-v28-publisher case, and its contract is a DATED COMPAT
WINDOW, not a refusal: the runner keeps today's chain and logs the loud once-per-name warning of
§4.4, naming the run and the absent document — silence is the one forbidden outcome. The refusal
arms in §7's final state, once the deployment's publishers are v28 (the queue's
`MinSchemaVersion` floor or the runbook's fleet check), and it refuses NEW submissions, never a
run already in flight: a run submitted before the document existed resumes under the old chain
with the same warning, never stranded mid-flight — the document is identity for the runs that
have one, and the warning is honest about the runs that do not. The runner installs the document
as the run's settings view for the run's
lifetime: every expansion site a RUN reaches reads the view, not the process. The global overlay
retires from the run path (it remains for genuinely process-scoped reads — supervise, sessionboard —
which §2 classified as process config); the 30 s TTL and the per-node straddle die with it.

**Who freezes, and when: the publisher, at submit** — the same fold in which `RunLLMRoutePolicy`
is already stamped, so there is exactly one fold and one stamp; the runner builds the run's
settings view from the document at claim. The document is
**identity, in ADR-121's sense**: frozen at submit, replayed by both resumes and every
redelivery, so a settings change
takes effect on the next submit, never mid-run. This deliberately reverses ADR-093's posture ("a
redelivered message re-expands with the CURRENT settings — nothing is pinned on the RunMessage"),
and that reversal is the point: the straddle #2025 measured is ADR-093's own consequence, named
there as a limitation and now paid. What a resume RE-resolves is unchanged — the volatile half
(usage windows, credential availability) was never in the document.

**The local contract.** A local `iterion run` has no queue and no tenant records; its settings
view is empty and the floor is the operator's own environment, exactly today's behaviour — the
doctrine must not leave the local lane with less power (philosophy §1). What the local lane gains
instead of dials is what the cloud lane gains: the catalogue's authored literals,
`--var`, `model_overrides`, the `routing:` block of a bundle, and — the named follow-up this
record shares with ADR-129's closing paragraph — a local reading of the policy levels where a
local store can carry them. Until then the local limit is ADR-129's, restated: the fold is
server-side, local runs honour flags and env, and that is a stated limit, not a silent gap.

### 4. The transition: staged, loud, never silent

Philosophy §1 forbids silently replacing an operator's explicit choice. Every env value this
record stops consulting is retired in stages that each say so:

1. **Warn (compile + gate).** The new diagnostic (§6) names every catalogue dial. Nothing changes
   at runtime; the deployment's pins keep working. The platform's routing env defaults
   (`ITERION_PLATFORM_*`) gain a boot warning when the platform record already carries the field
   they would default: the record wins, the env dial is dead weight, and the operator is told.
2. **De-dial the catalogue** (§5) — behaviour is identical where no env is set, which is the
   operator-proved invariant of the slice; where env IS set (the pools), §7 has already moved the
   pin into a record that says the same thing.
3. **Refuse new dials in the catalogue** — the guard becomes the floor test (§6).
4. **Demote the pod env to the default rung; empty it of keys.** A cloud server/runner resolves
   run settings from the document first; the environment keeps exactly one role, D20's: the
   lowest DEFAULT rung (never an override — a name that is not on the dial-retired table and
   that the document does not set still reads the env as its default, unwarned, because that is
   the contract). What the stage removes: every name on the retired-dial table stops being read
   (the boot warning names any a deployment still sets); the pools' pin mechanism dies (§7's
   step 4); and **the pods carry zero credentials** — iterion.cloud's vendor keys move wholly to
   records (ADR-131's tier walk), the env-funded cloud class ends, and the boot check refuses a
   credential-shaped name in a cloud pod's env rather than warning. The document's own rule is
   unchanged: a runner facing a missing document keeps today's chain PLUS the loud warning — the
   dated compat window of §3; refusal arms only in the final state, for new submissions, once
   the fleet's publishers are v28 — and when a retired dial WOULD have answered, the process
   logs the loud warning naming the variable, the run and the record
   that now carries the setting, once per name. Silence is the one forbidden outcome. The same
   warning exists for the LOCAL lane: when the operator's environment sets a name on the
   retired-dial table, `iterion run` warns ("the catalogue no longer reads it — pin with
   --model/--backend/--effort-for"), because the dial text is gone from the `.bot` and nothing at
   expansion time could otherwise detect the stale pin.

The escape hatch is `ITERION_CLOUD_ENV_PASSTHROUGH` (`platformcfg.go:538`), which already exists
for credential-shaped names and stays the greppable, boot-visible way a deployment says "this
environment's tenants are its operators". Run settings have no second hatch because they need
none: every env value a dial ever carried is writable to the ladder, so "keep my pin" is always
expressible in the system of record — and for every name except the pools' pinned ones, a value
set in BOTH places answers from the record, with the §4.4 warning saying the env copy is dead.
The pinned exception is #2702's, stated as the transition's own: while a name sits in the pool's
`ITERION_BOT_VARS_ENV_PINS` list, the record is shadowed, and §7's runbook therefore requires
record and env to be VALUE-IDENTICAL for the pinned names (proved by diffing the two), removing
the pin list only at §7's final step — after which the record answers alone. An operator who wants the env copy honoured
has no flag, by the doctrine's own sentence; what they have is the removal of the env copy, which
the warning names.

### 5. De-dialling the catalogue

Routing fields become their authored defaults: `${ITERION_VIBE_MODEL_EMIT:-claude-opus-5-5}` →
`claude-opus-5-5`, `${ITERION_VIBE_BACKEND_EMIT:-claude_code}` → `claude_code`,
`${ITERION_VIBE_EFFORT_EMIT:-medium}` → `medium`. The empty-default provider hints
(`${ITERION_VIBE_PROVIDER_GPT:-}`, `nodes.bot:127`, :284, :334) are REMOVED, not literalised — an
absent field is what an empty hint resolves to, so the no-env behaviour is byte-identical, and
the #2665 refusal class (any non-empty `providers:` list refusing these launches) dies with them.
The LOAD-BEARING authored hints stay exactly as written (`provider: "anthropic"` where the author
meant it, `nodes.bot:66`'s default arm; `provider: "zai"` on the GLM rescues) — de-dialling
changes WHO may pin a route, never what the author wrote.

The five non-routing lines: the three var-default knobs become their literals; the
`publish_review` command (`nodes.bot:478`) stops forwarding `${ITERION_VIBE_EFFORT_*}` into a
child's env and forwards instead the values the run actually used — the command already receives
`AI_REVIEWER_CLAUDE_MODEL={{input.ai_reviewer_claude_model}}` from the run's inputs (:478), so
effort joins the same input set, computed from the same fold, and the report stops being a
reconstruction from a dial; `adr-cartograph`'s `ITERION_DISPATCHER_PORT` read moves to the bare
`$PORT` of the env the engine constructs for tool processes (§1's write, not a read).

The manifest story: a de-dialled bot changes no declared var (the dials were never declared —
they were env conventions), so no `vars:` migration is owed; what bumps is the bot's own
behaviour contract, and each bot's doc page states the new way to pin it (the policy block), with
`docs/bot-runs/review-pr.md`, `docs/platform-bots.md`, `docs/auto-maintenance.md` and
`docs/usage-caps.md` losing their `ITERION_VIBE_*` recipes in the same slice. The invariant the
slice proves, per bot and catalogue-wide: **with no `ITERION_*` set, compiled routes and effective
settings are identical before and after** — `TestCatalogCurrentModelDefaults` already compiles the
whole catalogue with an env that answers nothing (`catalog_current_models_test.go:15`), so it is
the harness the slice extends rather than a test to placate.

### 6. The guard

A new compile diagnostic, **C314** (warning — the next free code; `diagnostics_codes.go` ends at
`DiagHumanClauseIgnoredInMode` = C313): *a `${ITERION_*}` reference in a routing field or a
catalogue bot's knob*. Its predicate covers BOTH halves of the class from the start — routing
fields AND tool bodies/knobs — on the same `ir.BracedEnvReads` segmentation the C188 lint already
runs over bodies (`braced_env_reads.go:33`): a new tool-body dial (the class `adr-cartograph`'s
`ITERION_DISPATCHER_PORT` is) must be warned, not silently passed to the floor. Its remedy names
the successor — the policy block, the
ladder, or the literal. Warn first (philosophy §1), then, in the same tranche as §5's last bot
group, the catalogue floor: `bots/catalog_env_dials_test.go` loads every shipped bot and refuses
any `${ITERION_}` in a routing field or knob — the hard floor that cannot drift back, beside the
other `catalog_*` floors. Third-party `.bot` files outside the catalogue KEEP dials: warned by
C314, and at the gate their reading does not change — an env-deferring field is undecided, widens
(`narrowSafe = false`), and never counts as env-funded, which is ADR-129 §4's honest posture and
stays. The guard's predicate is the same segmentation the C188 reader uses
(`ir.BracedEnvReads`), so it does not lie about `${X#y}` forms.

### 7. Production migration, ordered so no sovereign run falls back

The unsafe order is the tempting one: de-dial first, migrate after — in the window, the pools'
runs resolve the authored defaults (`claude-opus-5-5`, `claude_code`, the `anthropic` hint) and a
sovereign team's spend leaves its gateway. The safe order keeps the env pin and the database
record saying the SAME thing while both exist, then removes the env pin; nothing re-resolves to a
default that was never anyone's choice. The reason this is safe is ADR-129 §6's precedence line —
`node DSL field < policy overrides < launch model_overrides`: an `overrides` CELL beats the node's
field whatever the field's value came from, so writing the cells first changes only the
PROVENANCE of the answer, never the answer. That argument is a property of the CELLS — it does
NOT extend to the `providers:` allowlist, which is a refusal gate, not an override: against a
still-dialled catalogue its walk finds undecided routes and REFUSES the launch (the empty-default
provider hints are undecided no matter what the pod pins). Hence the split below: the allowlist
lands in the SAME operator window as the de-dial deploy, never before it.

1. **Prerequisite — the teams hold their gateway credentials** (ADR-131 PR-C', session aef05ba6).
   The pools' runs are env-funded today; the records must have a key to freeze before the env
   stops funding.
2a. **Write the overrides CELLS, on the operator's go, proved by preview.** Per pool:
   - `honorabilite` (the deployment-wide pool): its VIBE pins are deployment-wide, so they are
     the PLATFORM record's `overrides` cells — kind selectors (`agent`, `judge`) for the
     claude/gpt/emit slots, `backend: claw`, `provider: openai_compatible`, the pinned gateway
     models — the CELL list maps exactly the names the pool pins, never more: the pools pin no
     effort dial, and an effort cell written at authored-default value would keep beating the
     node field after the catalogue retunes an effort (ADR-129 §6 puts policy above the node
     field), freezing today's default as tomorrow's record. The runbook line: never promote an
     authored default into a record; an effort pin is written only if the operator asks for it.
   - `vao`: the same shape on ITS team record (the pool serves one tenant).
   - `egapro`: team-level cells with bot-scoped selectors for rgaa-audit's adjudicate/refute/
     publish slots (`ITERION_RGAA_*` → `{selector: "rgaa-audit/<node id>", backend: claw, model:
     <gateway model>}` — the bot dimension is §2's guard against shared node ids).
   The dial-to-cell table is generated from the §5 measure and reviewed as data. Proof BEFORE any
   de-dial: the credential preview (`runview.CredentialPreviewRequest`) against the real records
   shows the resolved chains the pools' env showed — fill, preview and gate agreeing is
   ADR-129 §7's parity property, now asserted at the new levels — followed by one staging run per
   pool on the records alone (env pins still present, proving provenance-change-only). The
   preview MUST also cover at least one team with NO record of its own, asserting its resolved
   chain is byte-identical before and after the platform write: a platform record INHERITS
   downward (`providers:` omits to inherit; `overrides` cells match by `(nodeID, kind)` with no
   tenant dimension), and a deployment hosting any team not on the gateways would otherwise have
   its runs constrained as a side effect. If that byte-identical assertion cannot be made, the
   honorabilite pins belong on per-team records, not the platform record — the mapping question
   is a precondition of the write, not a preference to confirm later.
2b. **Write the `providers:` allowlists** — in the SAME operator window as step 3's deploy,
   never before: against the still-dialled catalogue the walk refuses launches (§7's opening
   argument does not reach the list). The list and the de-dial are one step.
3. **De-dial the catalogue** (§5/§6) and merge the de-dial PRs — with step 2b's allowlists going
   live in the same window.
4. **Remove the env pins — and the keys — from the deployment** (infra-apps `864e62c` and its siblings, the vendor keys from the pods' env, and
   #2702's `ITERION_BOT_VARS_ENV_PINS` list with the pool env block — the pin mechanism's purpose
   dies here), then
   **demote the pod env to the default rung and empty it of keys** (§4.4, D20), including §3's
   refusal arming for new submissions once the fleet's publishers are v28. Production writes at steps 2 and 4 happen on
   the operator's explicit go; the slices ship the capability and the runbook, never the write.

Steps 2 and 3 are reversible in place (clear the record's block; revert the catalogue commit);
step 4's reverse is re-adding an env var, which §4.4's warning will then name as dead until the
records are cleared — the honest direction for a doctrine whose point is that the record is the
truth.

### 8. What the gate can then prove

Once routing fields are literals and the pins are records, the launch pre-flights read CONCRETE
routes: `EffectiveProviders` sees the real provider of every slot, the `providers:` allowlist
refuses and prunes with named sites, the sovereign refusal (#2006 S5) names what the run spends,
and ADR-129 §7's acceptance line — a team funding only its gateway, with `providers:
[openai_compatible]`, running the shipped catalogue — becomes TRUE for the shipped catalogue
instead of true for a catalogue that does not exist. `TestReviIntegrationAcceptance_TheShippedSlotsAreRefused`,
whose freeze documents today's refusal of the shipped slots, flips to its positive form: the
shipped slots are READABLE, and the gate certifies their pin. The witnesses: the acceptance test
inverted; the parity property at the new levels (§7.2); a red-first test that a NEW
`ITERION_VIBE_*`-style dial in a catalogue bot reddens C314 and the floor test; and the
§3 document tests — two runs on either side of a settings change resolving different documents,
and one run's nodes all reading one document across the TTL boundary that used to straddle them.

### 9. Parity, twin, and the neighbouring records

**Backend parity** (`claw` ↔ `claude_code`): nothing here is backend-shaped. `overrides` cells
carry both harnesses and are typed-refused where a value cannot serve one (ADR-129 §3's three
provider classes); the de-dial touches both backends' slots identically; the document is
backend-neutral wire state.

**Cloud-native twin (philosophy §3)**: every new durable field ships its Mongo record, its write
surface and its regeneration in the same slice — the ladder's org/team records with their routes
and CLI, the document with its queue schema bump (current `SchemaVersion = 26`; #2005's PR-C'
reserves 27, so this work takes 28 or renumbers at rebase, the routine here), the run-doc stamp
beside `LLMRoutePolicy`. Generated artefacts, never hand-edited: `openapi.json` +
`studio/src/api/schema.ts` (`task openapi:gen`), the repository maps including this record's index
row (`task map:gen`), the bots' canonical form (`task bots:fmt`).

**Sister records, positioned.** `route-selection-data` (the debt plan's G84) owns route
selection as DATA — vars in selectors, the selector vocabulary's growth; this record owns the
SOURCES (who may set a route fact and from which level) and the SNAPSHOT (when it is read). The
one place they touch is §2's `node#name` cells, which give that record a route-scoped selector to
grow. studio-09 / G137's per-run routing override hangs on the precedence line as completed here:
node field < policy (platform < org < team < bot < binding < run) < launch `model_overrides`,
with the settings document supplying the values the first term resolves from — it can hang its
override at the run level the launch request already accepts. The `env-doctrine-engine-half`
stream's byte-identical conversion of the 87 `ITERION_` reads to `ir.LookupEnv` is wave 1 of THIS
record's §3/§4: the chokepoint is where the document plugs in.

## Consequences

- **A run's configuration is answerable from its own document**: "why did this run route to X /
  clamp at Y" is a read of the run record, not an archaeology of three pod specs and a TTL.
- **The next bot slot cannot re-open the 2026-10-22 incident**: there is no dial to forget to
  pin; a new slot's default is a literal a tenant overrides by record, and the floor test refuses
  the dial spelling in the catalogue.
- **Sovereign tenancy becomes provable**: the allowlist reads every route; a rescue outside the
  tenant's providers is pruned BY the gate, named, not silently dialled around; the acceptance
  line of #2000 delivery 1 holds for the shipped catalogue.
- **Settings changes are claim-bounded**: an operator's edit lands on the next claim, visibly —
  never mid-run, never half-straddled. The cost, stated: tuning a long-running bot's knob means
  re-launching it, which is the honest price of one snapshot per run.
- **Becomes impossible**: configuring a run's routing or knobs through a pod env on a cloud
  deployment — env keeps the DEFAULT rung only, never an override (warned, then dead — §4, D20);
  a catalogue bot deferring a routing field to the
  environment (C314, then the floor); a team's stored var reaching another team's runs (the
  ladder's records are per tenant, the fold is per run's tenant); a vendor key in an
  iterion.cloud pod's environment (the boot check refuses it — every key is a record).
- **Migration and compatibility**: the queue document is additive and `omitempty`; a message from
  a pre-document publisher runs today's chain under §4.4's loud warning — the dated compat window
  of §3 — and the refusal arms only for new submissions, in the final state, once the fleet's
  publishers are v28; a run submitted before the document existed resumes under the old chain,
  never stranded; old
  records, manifests and run documents load unchanged; the local lane is byte-identical. The
  de-dial is behaviour-preserving exactly where no `ITERION_*` is set, which is the invariant
  §5's slice proves per bot; where env IS set, §7 has already moved the value into a record.
- **Docs to update**: `docs/cloud-llm-credentials.md` (the ladder's new rungs, the de-dialled
  recipes), `docs/platform-bots.md`, `docs/auto-maintenance.md`, `docs/usage-caps.md`,
  `docs/bot-runs/review-pr.md` (+ `feature-dev.md`, `whole-improve-loop.md`) — the
  `ITERION_VIBE_*`/`ITERION_RGAA_*` sections become policy-block recipes;
  [`docs/agents/engine/tenancy-credentials-spend.md`](../agents/engine/tenancy-credentials-spend.md)
  (the classification table, this record's single home for the deployment inventory);
  `docs/environment-variables.md` and `docs/sovereign-pools.md` (#2702's sections move to the
  §7 end-state); `docs/dsl.md` + `docs/workflow_authoring_pitfalls.md` (C314);
  `docs/references/diagnostics.md` (C314's row); ADR-093 gains a pointer ("the overlay's
  run-path half is retired by ADR-132; its write-surface rules live on as the ladder's", read
  with #2702's precedence addendum, which §7 retires with the pins); ADR-129
  §8's "nothing is removed to say so" is acted on by §5 and says so in an amendment line.
- **Residuals, accepted and named**: the 21 bare `os.Environ()` forwards to children are a
  construction-surface defect, not a run-read one — the allowlist builder is the debt plan's
  GO-ENV-03, and this record only states the direction (a child gets what the engine constructs);
  the 71 vendor-credential env reads remain the local lane's credential floor, by §1's verdict,
  and their cloud-plane retirement is ADR-131's continuing work, not a dial migration; a dial
  whose value no level can express today (none found in the 139 — every one is a model id, an
  effort, a provider, a backend, a duration or a boolean) would need its family graduate, per
  option 1's "where it wins".

## Review disposition (iterion-36, on the operator's delegated review — 2026-10-22)

**Verdict: APPROVED.** Jo delegated the review (`::g` trust); the reviewer checked the record
against the doctrine decisions (D0, #2699 verbatim, D20) and spot-verified the load-bearing claims
in the tree: C313 is the last diagnostic code (C314 free), `ProviderResolution.Pins` carries the
site keys the route-scoped selector reuses (`override_fold.go:103`), the rescue dials exist
(`nodes.bot:93`, :270), `usagecap/env.go:20` defines the switch ("off" disables both caps). The
3-reviewer plan review's 27 dispositions and the D20 integration are already in (f277e4fa6).

**Dispositions integrated in this commit:**
- **D21 — the route-scoped rescue cells SHIP** (§2's deferred operator question): `node#name`
  selectors with the bot dimension, the route-field guard and the zero-match warning; the
  conditional slices are unconditional. A team pins its own rescue tier inside its funded
  providers — the degradation ladder survives tenancy.
- **D22 — `ITERION_USAGE_CAP` (the switch alone) stays a process-enforcement env name** (§1's
  table amended): the modes and percentages migrate to the `usage_caps` record in S4; the switch
  is the one out-of-band spend brake that works when the control plane is the thing that is down.

The lock is lifted: the implementation slices start from this record's plan.

## Implementation disposition — S1/S2 (session-vagues16, debt-leap wave 16 — 2026-10-28)

The record landed in `docs/adr/` byte-identical to the reviewed text (number 132 was still free),
and §6's warn half shipped in the same wave. Two facts the record states moved between its review
and its implementation; they are recorded here rather than edited into the Decision, so a reader
of §6 finds the shipped shape without the decision being rewritten after approval:

- **The diagnostic is C315, not C314.** C314 was free when the reviewer spot-checked it; it was
  taken while this record waited, by #2460's compile-time refusal of a `session_slot` colliding
  with another node's id. The code is an identifier — the record's own argument about ADR numbers
  applies to it — so the guard took the next free one instead of a number already bound to
  another check.
- **The predicate names ANY env name, not the `ITERION_` family** (§6's opening sentence and its
  floor-test line say `${ITERION_*}`). The chantier's D0 default settled the scout's question that
  way, on a measure retaken in the tree: 35 of the catalogue's 333 dialled routing lines carry
  another family — `RESCUE_` (13), `BMADY_` (10), `VETTY_` (9), `REARCHI_` (8), `FEED_` (4),
  `DEVBOX_` (2). A doctrine with a prefix-shaped hole only moves the next dial to another
  spelling, and `dep-update-guard`'s human-node escalation model (`${VETTY_MODEL_ESCALATE:-…}`)
  would have stayed undecided at the launch gate forever under a prefix guard.

§5's de-dialling and §7's production migration are NOT in this wave, by this record's own
sequencing: §7 forbids de-dialling before the pools' pins exist as records, and that write happens
on the operator's go, on targets outside this repository. The wave's arbitration box carries the
question (ARB-13).
