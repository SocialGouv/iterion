# ADR-129 — Per-integration routing: typed node overrides and a provider allowlist ride the routing policy, and the launch pre-flights read the run's vars by provenance

- **Status**: Arbitrated (2026-10-12 — the operator chose options 2+3 of § Options; this record designs
  them, amended by three adversarial reviews of the first draft, § Objections)
- **Date**: 2026-10-12
- **Deciders**: jo (direction, arbitration), Claude (analysis, design)
- **Note on the number**: 124 is the highest published ADR; 125 is free. The live collision is open
  PR [#2124](https://github.com/SocialGouv/iterion/pull/2124), which adds a SECOND `121-` file
  (`docs/adr/121-a-lots-contract-is-a-directory-judged-on-what-lands.md`) and must renumber — 125 and
  127 are its candidates. This record claims 126 by landing with its `map-docs.md` row; a renumber at
  rebase costs a `git mv`, a `rtk proxy grep -rn ADR-129` and `task map:gen`.
- **Extends**: [ADR-121](121-adaptive-routing-policy.md) (the multi-level routing policy and its
  fold — this record adds two fields to it and one obligation to its parity property),
  [ADR-087](087-cross-backend-model-fallback-chain.md) (per-node `fallbacks:` routes, author-owned),
  [ADR-091](091-fallback-skip-route-and-plan-peer-review.md) (`when:` route gates),
  [ADR-122](122-gateway-models-resolved-exactly.md) (a gateway model id travels verbatim)
- **Serves**: [#2006](https://github.com/SocialGouv/iterion/issues/2006) (every Revi LLM slot
  routable per integration), epic [#2000](https://github.com/SocialGouv/iterion/issues/2000)
  delivery 1's acceptance line, [#2005](https://github.com/SocialGouv/iterion/issues/2005) (a
  team's own OpenAI-compatible endpoint), [#1221](https://github.com/SocialGouv/iterion/issues/1221)
  (the config half)

## Context

An operator cannot pin, **per forge integration**, the route of a bot's LLM slots. The dials that
exist are deployment-wide `${ITERION_VIBE_*}` env references read from the pod environment or the
single platform bot-vars record (`cmd/iterion/remote_admin.go:532`). Revi is the measured case —
six slots, two bots:

- `bots/review-pr/lib/nodes.bot`: `agent converge` (:13), `judge reviewer_claude` (:67) and its
  glance twin (:199), `judge reviewer_gpt` (:157) and its glance twin (:222);
- `bots/revi-converse/main.bot`: `agent converse_agent` (:239), whose `backend: "claude_code"`
  (:241) is a literal with no dial at all.

Five facts make this more than a missing field.

**1. The adaptive-routing policy will never route four of the six slots.** `ApplyRunFallback`
(`pkg/dsl/ir/fallback_apply.go:71`) and `ApplyPolicyLadder` (:398) both `continue` on
`nn.NodeKind() != NodeAgent`, for the reason written at :35 — "a judge's verdict is load-bearing".
Four of Revi's slots are `judge` nodes. Whatever carries a per-integration pin for them is the only
routing contract they will ever have.

**2. The mechanism the ticket proposed is invisible to the launch pre-flights.** The engine
resolves `{{vars.…}}` in `model:`/`backend:`/`provider:` at dispatch
(`pkg/backend/model/executor_resolve.go:269`) — but the pre-flights read the same fields *without*
the run's vars, by documented design (:266-268). A templated backend makes a wire route unreadable
(`wire_reach.go:140`) and keeps the Anthropic wire "reachable" (:159); a templated hint stays a hint
AND marks the chain unresolved (`override_fold.go:597`), a templated model prefix decides nothing
(:614). The debt is already live and nothing in the tree notices:
`bots/docs-refresh/main.bot:883` and :922 template their backend from a declared default today.

**3. A route's `when:` gate is read at DISPATCH and nowhere else.** `fb.When` has exactly one
non-DSL reader, `(*ClawExecutor).fallbackWhenActive` (`executor_resolve.go:189`, called from
`resolveChain`:159). The launch walk iterates every route and skips only
`fb.Action == ir.FallbackActionSkip` (`override_fold.go:180-195`). So a gated rescue route the run
will never take still contributes its provider to the launch's pinned set — and, through
`spendableProviders` (`pkg/server/cloudpublisher/publisher.go:1605`) and `pinnedUnfundedProviders`
(:1723), to the refusal. Eight catalog bots carry a `when:`-gated route today.

**4. That blindness is exactly what epic #2000's delivery-1 acceptance asks to prove.** The
operator's tenancy case — "no request may reach Anthropic, OpenAI or z.ai: a run that cannot be
served that way must be **refused, never re-routed**" — is enforced by the sovereign refusal at
`publisher.go:1395`, which names the unfunded providers the run's routes pin. A pin the walk cannot
read contributes nothing to that set: `spendableProviders` returns the zero value on a
non-narrow-safe walk, and under `llm_fallback=none` a team that holds a credential of its own then
**starts** a run whose routes nobody could attribute.

**5. A var cannot carry the whole decision.** `reasoning_effort:` resolves `${VAR}` only
(`ir.ResolveEffortLiteral`, `pkg/dsl/ir/validate_nodes.go:941`) — no `{{vars.…}}` — so effort is
unreachable by vars. A var must be **declared** by the bot to exist at all (`pkg/runview/executor.go:733`
drops an undeclared launch value), so every new slot dial costs a bot release. And `cfg.LaunchVars`
is the UNION of every co-enabled bot's manifest vars (`pkg/webhooks/types.go:318`), so ~24 slot
names would share one namespace with every other bot enabled on the repo.

Meanwhile the mechanism that does exactly this already exists and is already read everywhere:
`OverrideEntry{Selector,Backend,Model,Provider,Effort}` + `OverridesFrom`, "the ONE fold"
(`pkg/backend/model/override_fold.go:17`) that runview, the cloud publisher and the runner adapt
onto, honouring judge-kind selectors (#668, `model_override.go:144`), read with top precedence at
dispatch (`executor_resolve.go:32`), persisted on the run document (`pkg/store/run.go:894`) and
replayed by both resumes (`publisher.go:3834`, `pkg/cli/resume_helpers.go:154`). What it lacks is a
**level**: the integration config cannot carry one, so a repo cannot say it.

And one carrier is already threaded into most launch lanes. ADR-121's policy rides four binding
surfaces — `webhooks.Config.Routing` (`types.go:374`), the trigger subscription, the scheduled bot
(`pkg/cloudsched/schedule.go:66`), the bot manifest — each projecting `RoutingPolicy()`, folded at
one chokepoint (`pkg/server/route_resolve.go:43`), snapshotted on the run doc
(`store.RunLLMRoutePolicy`, `run.go:535`) and replayed frozen on resume.

## Options considered

- **1. Per-slot bot vars (the ticket as written).** ~24 `*_backend`/`*_model`/`*_provider` vars
  declared on the two bots, defaulted to today's `${ITERION_VIBE_*}` dials, set per repo through
  `launch_vars`. *Pros*: zero engine change, carrier live, every piece precedented, fully reversible,
  one bot-only PR, collides with no open PR. *Cons*: every templated field goes unreadable at the
  pre-flights, so the tenancy refusal cannot name what the run spends; ~24 names in a shared
  namespace; effort unreachable; a third routing mechanism beside `ModelOverrides` and ADR-121's
  policy — the per-bot duplication ADR-121 § Alternatives rejected as the primary mechanism.
  *Where it wins*: "pin my slots today", for a deployment that accepts an over-conservative gate.
- **2. Integration-carried typed model overrides (reuse the built fold).** *Pros*: the pre-flights
  see the pin concretely, so the refusal keeps its precision; judge selectors honoured; no new var
  surface; a slot moves without re-releasing the bot; effort included. *Cons*: needs the field on the
  integration record and its write surfaces; does **not** by itself switch off a bot's hard-coded
  `provider: "zai"` rescue routes (`nodes.bot:63`, :153, :218; `revi-converse/main.bot:255`).
  *Where it wins*: #2005's tenancy lock, which only a gate that can read the pin can enforce.
- **3. Give the pre-flights the run's vars.** *Pros*: pays the live `docs-refresh` debt; makes a
  templated field as readable at the gate as it is at dispatch; needed whatever option 1/2 wins,
  because bots will keep templating routing fields. *Cons*: changes 7 production call sites, 6 in
  `pkg/server/cloudpublisher`, and owes ADR-121's parity property (fill / restore / preview / gate
  agree) as a test; naively done it is a **guard-lifting** change (§ Objections, O1).
  *Where it wins*: any run whose routing fields are templated — today, silently.
- **4. Do nothing.** The six slots stay deployment-wide; #2005's team gets Revi or gets its lock,
  not both.

**Arbitrated 2026-10-12**: options **2 and 3**, as one contract — the most general, longest-lived
mechanism even at a higher cost. Option 1 is not the primary mechanism and no slot var is shipped.

## Decision

### 1. The carrier is the routing policy, not a new record

`llmroute.Policy` gains TWO fields:

```go
// Overrides pins the route of individual NODES — the typed, per-level twin of
// the launch's model_overrides. Entry-wise fold per (selector, field) cell
// (§2); values are literals (§3); lockable as a block.
Overrides []Override `yaml:"overrides,omitempty" json:"overrides,omitempty" bson:"overrides,omitempty"`

// Providers is the tenant's provider ALLOWLIST: the credential-routing
// providers a run of this level may spend. A route resolving outside it is
// not this tenant's spend — a primary refuses the launch, a rescue is pruned
// (§5). Nil = unset (inherit / no allowlist). Lockable; first-setter-wins
// like pair_order — a list never merges.
Providers []string `yaml:"providers,omitempty" json:"providers,omitempty" bson:"providers,omitempty"`

type Override struct {
    Selector string // node id | glob | "agent"|"judge"|"router" | "*"
    Backend  string // a harness name (llmroute.validHarness)
    Model    string // a model id, verbatim (ADR-122)
    Provider string // a credential-routing hint chain, literal, never "auto"
    Effort   string // a reasoning-effort level
}
```

The levels, their order, the locks, the provenance map, the validated write surfaces, the run-doc
snapshot and the frozen resume replay are ADR-121's and are reused as they are:
**platform < org < team < shipped bot < binding < run**. A forge integration pins its slots by
storing a routing block on its own record — the level ADR-121 calls the binding, per provisioning
surface.

Durable twin, cloud-native per philosophy §3: `forge.RepoIntegration.Routing *llmroute.Policy`
(`pkg/forge/repo_integration_store.go`, beside `LaunchVars`) is the operator's record, and **every**
projection of that record is rebuilt from it:

- the provisioned webhook copy (`webhooks.Config.Routing`) is written from the record by `Provision`
  and its carry becomes conditional — `carryOperatorWebhookSettings` carries `prev.Routing`
  unconditionally today (`pkg/forge/orchestrator.go:1286`), which would make the field **write-once**
  (§ Objections, O4);
- the integration's **scheduled** rows (`cloudsched.ScheduledBot.Routing`), which are the only layer
  the scheduled launch resolves (`pkg/server/webhooks_gitlab.go:1288-1292`) and which `syncSchedules`
  rebuilds from a literal, carrying prior state by name (`orchestrator.go:1072-1090`) — Routing is
  absent from that list, as are the four retry fields, so a schedule's own block is erased by the
  next `bots enable`;
- the org-approval **park and apply** path, which copies the request field by field
  (`pkg/server/forge_provisioning_routes.go:248-258`, `forge_approval_routes.go:135-155`, :295-305);
- `verifyOperatorSettings` (`orchestrator.go:1749-1770`), which certifies both stores field by field
  and would otherwise certify a configuration the webhook does not enforce.

The reason the field lives on the integration and not only on the webhook is the one already written
for `LaunchVars`: provisioning rewrites the whole webhook document, so anything set only there is
silently lost at the next `bots enable`.

Deliberately NOT chosen, two doors:

- **A `model_overrides` field beside `launch_vars`.** It would be a second routing vocabulary on the
  same record, needing its own threading into five launch lanes and its own precedence against the
  policy — the third mechanism option 1 was rejected for.
- **`overrides` (or `providers`) on `bundle.RoutingSpec`.** That type's own doc states the doctrine:
  the manifest projection is hand-picked so "a field added to the platform record … does not
  silently become author-writable manifest YAML" (`pkg/bundle/manifest.go:300-305`). The bot layer
  sits ABOVE the binding (`pkg/server/route_resolve.go:46-48`) and `RoutingSpec` already carries
  `Locks`, so an author's `locks: [overrides]` would veto the operator's pin with no error and no
  refusal — and `manifest.yaml` is writable through
  `PUT /api/teams/{id}/bot-sources/{slug}/files/{path...}` behind `canEditBots` →
  `canEditConfigShares`, which admits `identity.RoleConfigEditor` (`pkg/server/auth_authz.go:45-58`),
  while the team routing record needs `canManageTeam`. The write-role table this record owes:

  | level | record | who may write it |
  |---|---|---|
  | binding (integration) | `forge.RepoIntegration.Routing` | team admin/owner, org-gated by the approval path |
  | bot | `manifest.yaml` `routing:` | `config_editor` or team admin (ADR-078) |
  | team / org | the routing-policy records | `canManageTeam` / `canManageOrg` |
  | platform | the settings record | super-admin |

  An author door for `overrides` is a separate decision, and it needs two things this record does not
  ship: `overrides`/`providers` excluded from the manifest's lockable set (a bot may set, never lock,
  the tenancy carrier), and the bot level placed BELOW the binding for these fields.

### 2. The fold: entry-wise per cell for `overrides`, first-setter-wins for `providers`

`overrides` folds like `model_classes`, not like `pair_order`: **per (selector, field) cell**, the
first setter within the range the locks allow winning each cell, with provenance keyed
`overrides.<selector>.<field>`. The block key `overrides` answers only when no cell spoke, so a lock
that pinned the field to nothing still says `"<level>_lock"` rather than `"default"`.

Entry-wise, not replace-wholesale, because a cell is independent by construction — `ForNode`
resolves per field (`model_override.go:144`) — so merging cells cannot produce the "reordered prefix"
trap that made `pair_order` replace. Concretely: a team pins `judge → backend: claw`, an integration
pins `reviewer_gpt → model: <gateway id>`, and both hold.

`providers` is a single list and folds like `pair_order`: a level REPLACES it, first setter wins,
lockable. A list that merged would let a level ADD a provider the level below meant to exclude, which
is the one direction an allowlist must never move.

**One vocabulary for the chain**, because the draft used two and inverted itself: layers arrive
highest-priority first and the FIRST setter wins (`pkg/llmroute/policy.go:470-508`). The integration
sits **higher** in the chain than the team — closer to the run — so it answers first. "A lock on a
field at level L vetoes every setter ABOVE L" reads with the same compass.

Specificity is **not** precedence. Two different mechanisms are at work: within one resolved set,
`ModelOverrides.ForNode` picks the most specific selector; across levels, the fold picks the level.
§6 says what had to change for the second to hold.

`locks: [overrides]` and `locks: [providers]` lock the block, not a cell. A level that must pin one
slot locks the block and states every cell it wants. Coarse on purpose, and said out loud: a per-cell
lock vocabulary (`locks: ["overrides.reviewer_claude.backend"]`) is a second grammar inside a list of
field names, and nothing has yet needed it. Per ADR-121, the binding surfaces hold no lock primitive
at all — their write path refuses `locks` with the reason named, exactly as `decodeScheduleRouting`
(`pkg/server/schedules_routes.go:113`) already does.

### 3. Values are LITERALS, and the vocabulary is checked against the code that reads it

`llmroute.Validate` refuses an override value containing `{{` or `${`, naming the field. A typed
override is a literal, always; that is what makes "the pre-flight can read this pin" a property of
the type rather than a hope about its contents. A bot's own field may still be templated — §4 is
what makes that readable too — but the operator's pin is never a deferred reference.

The rest of the validation, with the refusal naming the offending field:

- `selector` non-empty; a wildcard selector must compile as `path.Match` syntax; the kind keywords
  are `agent|judge|router` and `*`, the vocabulary `selectorScore` already implements
  (`model_override.go:171-192`). A selector containing `.` or `$` is **refused**: the provenance key
  is `overrides.<selector>.<field>` and that map is persisted as a BSON document
  (`store.RunLLMRoutePolicy.Sources`, `pkg/store/run.go:556`), where a `.` makes the key ambiguous
  and a `$` makes it a field name Mongo reads as an operator.
- an entry pinning nothing at all is refused: `ModelOverrides.add` drops it silently
  (`model_override.go:134`), which at a write surface is a typo that looks like it landed.
- `backend` ∈ `llmroute.validHarness` (`policy.go:276`) — the same vocabulary the pairs use.
- `provider` splits on `,` and on the `provider:model` step form, like `chainHints`; each hint is a
  known provider or `openai_compatible`. **`auto` and an empty step are refused**: an override exists
  to PIN, "let the process decide" is already spelled by OMITTING the cell, and the de-pin is not
  free — `resolveNode` sets `narrowSafe = false` run-globally for an override provider that names no
  resolvable hint (`override_fold.go:371-386`), after which `spendableProviders` fails open and the
  sovereign refusal can name nothing. At dispatch the same cell would replace the author's chain, and
  `nodes.bot:70-87` records what that costs: "The anthropic hint is LOAD-BEARING … Unpinned, this
  node would then send its claude id to the z.ai facade, which ACCEPTS it and serves GLM in silence,
  under the claude label."
- `provider` is **refused on a backend that does not consume it**, naming the harness. There are three
  classes, not two (`executor_retry.go:470-487`): `claude_code` walks the chain; `claw` honours a
  single-value `anthropic` hint and otherwise derives its provider from the model-spec prefix;
  `codex`, `pi`, `grok`, `kimi` and `opencode` do not consume it. The accumulator is harness-blind
  (`chainDecides`, `override_fold.go:485-500`), so without this rule a cell
  `{backend: codex, provider: zai}` narrows the gate onto a hint codex never reads — the gate
  certifying a pin the harness ignores, at the write surface whose purpose is to make the lock
  provable. `pkg/llmroute` imports nothing of iterion by design, so the predicate stays a local
  vocabulary with a drift test against `model.ConsumesProviderHint` in `package llmroute_test` — the
  pattern `validKeyProvider` uses against `secrets.Provider`.
- `effort` ∈ the reasoning-effort vocabulary, local for the same import reason, with the agreement
  test against `ir.ValidReasoningEfforts` homed in `pkg/llmroute/projections_test.go`
  (`package llmroute_test`): `pkg/dsl/ir/fallback_apply.go:8` imports `pkg/llmroute`, so the same
  assertion inside `package llmroute` is an import cycle.
- `providers` entries are known providers or `openai_compatible`; an empty list is refused (a level
  that must forbid everything has no run to make — the honest spelling is to refuse the bot).
- two cells with the same (selector, field) **within one level** are refused: the later would
  silently win. Across levels that is the fold's job, and the fold says who won.

A record carrying a block the fold cannot read refuses the **launch**, naming the level and the
field — `route_resolve.go:96` already does this for every layer, and the new fields inherit it.

### 4. One reading of a routing field, and vars that narrow the gate only by PROVENANCE

`resolveRoutingField` (`executor_resolve.go:269`) is extracted to a package function taking the vars
explicitly; the executor method calls it. The launch-time pre-flights then read routing fields
through **that same function**, not through a twin of it:

```go
// RouteView is the one value every launch-time routing derivation reads.
type RouteView struct {
    Workflow     *ir.Workflow
    Overrides    ModelOverrides
    RunFallbacks []FallbackEntry
    Vars         map[string]any  // resolved as dispatch resolves them; nil = today's reading
    LateVars     map[string]bool // vars whose value this process decided, not the run
}
```

`AnthropicWireRoutes`, `AnthropicWireReachable` and `EffectiveProviders` take a `RouteView`. The 7
production call sites pass one value instead of three arguments, and the next field that resolves
late is threaded once instead of seven times. A nil `Vars` is exactly today's behaviour, which keeps
the change inert for every caller that has no vars to give.

**Why a var may narrow the gate at all, and where that stops.** The draft justified the reading with
"a run's vars are identity — resolved once at run start (`pkg/runtime/engine_run.go:1022`), stamped
on the run document as `Inputs`, replayed by both resumes — so the publisher and the runner read the
same values". Identity is not trust, and a VALUE is not always the run's (§ Objections, O1 and O6).
Three provenance rules, each of which keeps the gate's conservatism where the draft lost it:

1. **A var whose value this process decided is as opaque as `${…}`.** `ir.LaunchVarsView`
   (`fallback_apply.go:196`) resolves every declared default and launch override through
   `ResolveVarText(raw, vt, LookupEnv)` — **the publisher's own environment**. A bot declaring
   `llm_backend: string = "${ITERION_VIBE_BACKEND:-claude_code}"` and templating its nodes from it
   resolves to the gateway on the server pod and to `claude_code` on the runner Job: two container
   specs, one var name. So `LaunchVarsView` returns the view **plus** the set of names whose raw text
   contained `${` or which took the coercion-failure branch (:196-240), `RouteView.LateVars` carries
   it, and a field resolved through a late var is treated exactly as a raw `${…}` is today —
   `envSeen = true`, `narrowSafe = false`. Without this rule, `OnlyEnvFunded()` becomes reachable
   through a var and the whole sovereign refusal is skipped (`publisher.go:613`, :1395), with no tier
   consulted and nothing sealed — "an env-dependent route counts as UNRESOLVED, never env-funded"
   (:609-612) is the posture this record must not lift.
2. **Only the declared default and the operator's pinned launch vars may narrow.** The webhook lanes
   build a launch's vars from the delivery payload: `reviewPRVars` seeds `scope_notes` with the PR
   title and body, `fixerPRVars` takes `push_branch` from the source branch
   (`pkg/server/webhooks_common.go:214-274`), and `forgePREventTargets` says out loud that "the
   PR-event lane IS the fork review lane's primary path" (:534-574). A routing field templated from a
   lane-filled var would hand the harness, model and credential choice of a team-funded run to
   whoever wrote the PR — and after this record the gate would CERTIFY that choice. So the view
   narrows through a declared default always, and through a launch override only when the lane
   declares the name operator-pinned (`LaunchSpec.TrustedVarNames`, filled from
   `cfg.OperatorLaunchVars`, the schedule's own vars, the CLI's `--var`). An untrusted name leaves the
   field undecided, which widens — the pre-#1606 posture, which is the safe direction.
3. **The runner reads a VIEW of `msg.Vars`, never `msg.Vars` raw.** `msg.Vars` is the raw inputs
   (`publisher.go:3370` `Vars: inputs`); the declared-default layering and the drop of undeclared
   names happen later, in `pkg/runview/executor.go:725-735`, and every unattended lane sets
   `AllowUnknownInputs: true` (`boarddispatch.go:1868`, `trigger_launcher.go:90`,
   `webhooks_gitlab.go:1369`, :1438, `pipeline_admission.go:801`). A pre-flight reading `msg.Vars`
   raw would therefore both MISS a declared default — `docs-refresh`'s own case — and read a key
   dispatch discards, which is the guard-LIFTING direction on Revi's own lane. One constructor,
   `RouteViewForLaunch` / `RouteViewForDispatch`, builds `Vars` from the same filter at both ends.

The same constructor is threaded into the two ADR-121 screens the publisher calls with `nil` today
(`ApplyRunFallback` at `publisher.go:1236`, `ApplyPolicyLadder` at :1244, the door probe at :2910):
a publisher whose ladder screen reads one view while its credential walk reads another is the
publisher disagreeing with itself, and §7's parity would be asserted over half the surface.

**The `when:` gate joins the reading.** `fallbackWhenActive` becomes a package predicate over
`RouteView.Vars` and the launch walk calls it (`override_fold.go:180-195`; `wire_reach.go` reads
primaries only and is unaffected). Conservative by construction: a gate the vars cannot answer keeps
the route ACTIVE, so the walk never drops a provider the run might spend. The class is three readers
of `GetFallbacks()` — the walk, the ladder, dispatch — and one predicate.

The gateway classification keeps its "raw spelling only" rule for `${…}` (`rawGatewayPrefix`,
`override_fold.go:507`) and gains the vars reading under the three rules above: a `{{vars.model}}`
whose TRUSTED, non-late value is `openai_compatible/<id>` is a gateway route for both processes, so
an env-funded run stays provable through a var — which is #2005's shape exactly.

**The attribution the walk has to carry.** `ProviderResolution` accumulates provider names and
nothing else (`override_fold.go:253-300`), so no refusal built on it can say which node or route
pinned what. It gains `Pins map[string][]string` (provider → the node ids and `node#route` keys that
named it), filled where `chainDecides` and `prefixOrWiden` record a hint. A refusal that names the
provider AND the site is a refusal an operator can comply with; a bot-specific literal in the
engine's tenancy gate is not (philosophy §2's Nth-variant test).

### 5. The `provider: "zai"` rescue routes: a provider ALLOWLIST, not a route edit and not a bot var

The requirement is a statement about a **provider** — "no request may reach z.ai" — and the two
shapes the arbitration framed are both dishonest for it.

**An override entry may not drop or replace a node's `fallbacks:` routes.** Three reasons, in order
of weight: the route belongs to the author (ADR-087 and ADR-121 § "What deliberately does not
change"; there is no screen for a *removal* — C176/C135 screen routes that are added); "drop" is not
expressible in this fold, where every field means "unset = inherit" and a negative directive needs
the sentinel `Validate` already refuses for `triggers: []`; and a per-selector drop must be repeated
for the next bot that carries a rescue route.

**A bot-side `when:` var is not the contract either**, which the draft got wrong three ways:

- `bots/review_pr_glm_route_test.go:115-120` — in `package bots`, inside `task test` → `task check` —
  REFUSES any `when:` on the zai route of `reviewer_claude`, `reviewer_claude_glance`, `converge` and
  `converse_agent`: exactly the four routes the draft proposed to gate. Its reason is fact 3 above.
  The guard is the author's reasoned refusal of this mechanism and it stays as it is.
- Even after §4 teaches the walk to read `when:`, the knob is a string: `vars.glm_rescue != 'off'`
  arms the z.ai route for `false`, `no`, `0`, `OFF` and every typo, and a coercion failure "logs and
  runs the RAW value" (`fallback_apply.go:213-222`). C173 guarantees the var is declared and
  `vars.`-scoped (`validate_fallbacks.go:283`, :290, :298), never that its value is in a vocabulary.
  A sovereignty lock does not ride a fail-open string.
- The refusal cannot name it bot-agnostically (§4, `Pins`): hard-coding `glm_rescue` into the generic
  sovereign refusal fails silently for the next bot with a rescue route.

**So the mechanism is `providers:`, on the policy.** Semantics, stated as the gate reads them:

- a route whose resolved provider is **outside** a non-empty allowlist is not this tenant's spend: it
  is excluded from the walk's pinned set and recorded in `ProviderResolution.Forbidden` with its
  site;
- a **primary** route outside the allowlist **REFUSES the launch**, naming the node, the provider and
  the level that set the list — there is no way to serve that node, and "refused, never re-routed" is
  the contract;
- a **rescue** route outside it is pruned, so the launch is not refused for a route the tenant
  forbids. Nothing re-routes and nothing leaks: no credential is sealed for a pruned provider, and
  dispatch refuses the route before the CLI spawns, naming the provider and its env var
  (`ErrNoFacadeCredential` — the behaviour `review_pr_glm_route_test.go:41-43` already documents);
- a non-empty allowlist makes readability **compulsory**: a route the walk cannot read refuses the
  launch, naming the node and the field. A lock that cannot be checked must not pass — and this is
  where §4 earns its cost, and where `overrides` earns its "values are literals" invariant: pinning a
  slot is how an operator makes a route readable.

One statement per tenant instead of one var per bot; a provider vocabulary the write surface
validates instead of a free string; a refusal that names provider, site and level; and no catalog bot
is touched. The dispatch-side half — dropping a forbidden route from the resolved chain so it does
not burn a retry budget on a call the tenant forbade — is the same rule read in the runner and lands
in the same work (plan slice 4), because a capability wired for one side is wired or typed-refused
for the other.

**Named and not shipped here**: a per-cell `overrides` CEILING (prune cells the way `Clamp` prunes
triggers, `policy.go:630`) so a tenant can bound the field without locking it shut; and a `deny`
spelling beside the allowlist. Both are routing-policy fields, both generalize, both are tickets of
their own.

### 6. Precedence, the single stamp, and what resume replays

One line, end to end:

> node DSL field < policy-resolved `overrides` (platform < org < team < bot < binding < run routing
> layer) < the launch's own `model_overrides`

That line is **not what the code did**, and saying it required a fix (§ Objections, O2). `MergeOver`
only concatenates (`model_override.go:119-131`) and `ForNode` then resolves per field by SELECTOR
SPECIFICITY, keeping the last rule whose score is `>=` the best so far (:144-166), where
`selectorScore` gives `"*"` = 1, a kind keyword = 10, a glob = 100+, an exact node id = 100000
(:171-192) — and the launcher's canonical gesture is the LEAST specific selector: `--backend claw`
and the studio dropdown both record `"*"` (`splitSelectorValue`, :206-210). So an integration cell on
`reviewer_claude` would silently outrank an operator's explicit `--backend claude_code` at the launch
button, on exactly the nodes this feature exists to pin — philosophy §1's "never silently replace an
operator's explicit choice". `modelOverrideRule` therefore gains a layer `rank`, `ForNode` compares
`(rank, score)` lexicographically, and `MergeOver` stamps `o`'s rows one rank above `base`'s. Eight
lines, one file, and the stated line becomes true for every selector pair.

The launch folds the two into **one** list at a named helper — `runview.EffectiveModelOverrides(launchRows, policy)` —
called by **both** launch-shaped entries: `Service.Launch` (`service_launch.go:221`, :320) and
`Service.PreviewCredentials` (`pkg/runview/credential_preview.go:141`). The preview is not reachable
from `Service.Launch` and derives every route fact from `spec.Launch.ModelOverrides` alone —
`derivePinnedProviders`, `OnlyEnvFunded`, `wantsFor`, `doorWantsFor`
(`pkg/server/cloudpublisher/credential_preview.go:44`, :47, :94, :327) — under the comment that
states the parity doctrine verbatim (:48-52). A fold in `Launch` alone would show the operator
candidates, pool wants and warnings for credentials the launch never acquires, and §7's parity test
would stay green over the wrong surface.

That one list is what is stamped on the run document, what rides `queue.RunMessage.ModelOverrides`,
what both resumes replay, and what every pre-flight reads. No reader anywhere learns a second input;
philosophy §5's "never a second source of truth" is honoured by folding once rather than by teaching
seven readers to fold.

The fold happens **before** `ValidateModelOverridePermissions`
(`pkg/runview/model_override_validation.go:19`). This is load-bearing: that check refuses a
launch-time backend override that would remove a node's effective `permission: ask|deny` gate, and an
integration-carried override folded in *after* it would reach past a guard the launcher's own
override cannot. The runner re-validates at `BuildExecutor` (`pkg/runview/executor.go:369`), so the
property has two witnesses.

**The blast radius of a `*` cell, stated because the write surface cannot see it.** An integration's
block applies to every bot the repo's rules fan out to, and the permission check REFUSES the launch
for any LLM node whose effective ask-rules the overridden backend cannot gate. A repo pinning
`* → backend: claw` to move Revi onto its own gateway can therefore fail every webhook delivery of a
co-enabled bot the operator never meant to touch. The write surface WARNS, naming the bot and the
node that would be refused, screened against the manifests of the bots enabled on the integration at
write time; it does not refuse, because a bot enabled later cannot be screened there and a hard
refusal would make a legitimate `*` cell unusable. The launch refusal stays the enforcement
(philosophy §1: warn rather than reject).

`store.Run.ModelOverrides` keeps its shape and gains **no** field: provenance for the run Overview is
already answerable from `RunLLMRoutePolicy.Sources` (`run.go:556`), and a display-only `Source`
column would break the one property the ONE fold's doc leans on — `model.OverrideEntry`,
`store.RunModelOverride`, `queue.ModelOverride` and `runview.ModelOverrideEntry` are field-identical
(`override_fold.go:13-16`), which a reflect test now pins. Its doc comment IS corrected in the same
change: it claims the overrides are "never re-read from here" and "left untouched on resume"
(`run.go:888-894`), and both resume paths have read them since (`publisher.go:3755`, :3834,
`pkg/cli/resume_helpers.go:154`). A stale comment on a durable field is how a replay gets dropped by
someone who believed it.

A launcher's `model_overrides` is **not** lockable, and that is stated rather than hidden: the field
predates the policy, carries no level, and is the studio dropdown's wire. A launcher who wants the
fold's governance uses the run-level routing layer the launch request already accepts
(`req.LLMRouting`, `runs_launch.go:630` — `Launcher: true`), where `overrides` is lockable like any
other field. The tenancy lock does not depend on that: a launcher who re-targets a node onto a
provider the team cannot fund, or onto one outside the allowlist, is refused at launch by name.

### 7. What this makes provable, and the parity it owes

The acceptance line of epic #2000 delivery 1 becomes a test rather than a hope: a team funding only
`key:openai_compatible`, with `providers: [openai_compatible]` and an integration pinning all six
Revi slots to `claw` + `openai_compatible/<id>`, and the run resolves **no** `claude_code` route and
**no** `zai` route, is classified env-funded, and seals nothing — asserted through the public entry
(`SubmitLaunch`) over the resolved chain, never over the `.bot` text. A deliberately unpinned slot is
refused at launch, naming the provider, the node and the level.

ADR-121's parity property gains the vars and the preview: **fill, restore, preview and the launch
gate** must agree for a run whose routing fields are templated. That is one property with four
readers in two packages, and it is owed as a test driven from an integration-level cell — a test
written over launcher rows would pass without ever exercising the new level.

### 8. Backend parity and the escape hatches

Backend parity (`claw` ↔ `claude_code`): an override is a (harness, model, hint, effort) tuple on a
node; both harnesses are reachable by it, and the existing refusals do the typing — a GLM id on claw
is `anthropic/glm-*` and on claude_code is bare (`GLMOnAnthropicWire`). The `provider:` hint has
three classes, not two (§3), and `Validate` is what makes that typed instead of silent.

Escape hatches, per philosophy §1: the `${ITERION_VIBE_*}` dials stay exactly as they are and keep
working (an integration that sets nothing behaves identically); an override block or an allowlist is
removed by clearing it (`null` clears, an object replaces wholesale — the schedule's rule,
`schedules_routes.go:391-393`); the launcher's own `model_overrides` outranks an unlocked policy cell at
every selector (§6); and a local `iterion run` keeps its flags-only behaviour.

**A literal field is worth more than an env dial now.** `bots/revi-converse/main.bot:241` keeps its
`backend: "claude_code"` literal and gains no `${ITERION_VIBE_BACKEND_CONVERSE:-…}` dial, reversing
the draft: an env-templated routing field is UNDECIDED at the gate by rule 1 of §4, so converting a
readable literal into a dial would cost that slot its contribution to the lock. The deployment-wide
need the dial would serve is served better by the policy's **platform** level, which is typed,
validated, visible to the gate and lockable. The platform level of `overrides` is the successor of
the `${ITERION_VIBE_*}` dials, and nothing is removed to say so.

Local parity, stated as the limit it is: the fold is a **server-side** resolution — its other levels
are server records — so a local CLI run honours flags only, as it does for ADR-121's policy today. A
local resolution (a `routing:` block read from the bundle beside the manifest) is a follow-up, not a
silent gap.

## Objections considered

Three independent adversarial reviews attacked the first draft of this record and its plan. Six
objections changed the design; they are kept here because each one is a property an implementer can
lose again.

- **O1 — "A vars-aware gate narrows on a value the PUBLISHER decided."** `launchVarsView` resolves
  through `LookupEnv`, so a declared default of `${ITERION_VIBE_MODEL:-…}` reads the server pod's
  environment while the runner Job reads its own; relaxing the `{{` branch makes `OnlyEnvFunded()`
  reachable through such a var, and that flag skips the entire sovereign refusal
  (`publisher.go:613`, :1395) and every credential tier (:644-660). **Adopted**: §4 rule 1 — the
  narrowing is provenance-based, not spelling-based, and a late var is as opaque as `${…}`.
- **O2 — "The stated precedence is not what the code does."** Selector specificity beats the
  launcher's `"*"`. **Adopted**: §6, the `(rank, score)` comparison.
- **O3 — "`Service.Launch` is not the one chokepoint."** The credential preview is a second
  launch-shaped reader. **Adopted**: §6, one named helper called by both entries, and §7's parity
  test driven from the new level.
- **O4 — "The carrier is write-once, and three projections never see it."**
  `carryOperatorWebhookSettings` overwrites the provision literal with `prev.Routing`;
  `syncSchedules`, `verifyOperatorSettings` and the approval park/apply path do not carry the field
  at all. **Adopted**: §1's four-projection list, the conditional carry, and the park rule — a
  provision update that carries a routing block parks for org approval, on `expandsProvisionSurface`'s
  own doctrine ("fail toward approval, never past it"): a per-repo provider, model or effort pin
  moves the repo's unattended spend and cannot be told from a tightening at that surface.
- **O5 — "The bot-side `when:` var is already refused by a committed guard."** **Adopted**: §5 —
  the guard stands, the var is not shipped, and the mechanism is `providers:`.
- **O6 — "`a run's vars are identity` conflates identity with trust."** The webhook lanes fill vars
  from the delivery payload, including on the fork lane. **Adopted**: §4 rule 2.

Four more were adopted as written and are visible in §3 (the `auto` cell, the harness-blind
`provider` cell, the `.`/`$` selector) and §4 (the three missing accumulator readers —
`chainHints`, `hint` and `providerFromModelPrefix` — without which a `{{vars.p}}` hint is recorded as
an *unknown provider* and the operator-facing diagnostic prints a template).

One proposal was **declined**: re-polarizing the rescue gate to `vars.glm_rescue == 'on'`. The
polarity reading is right — a typo must disarm a rescue, not arm it — but the var is removed rather
than re-polarized, so there is no string knob left to fail open.

## Consequences

- **A repo can pin its slots without a bot release, and the gate can still say what it spends.** The
  two halves of #2006 that the ticket's own shape put in tension.
- **The four judge slots get the only routing contract they can have.** The policy ladder will never
  re-target them (`fallback_apply.go:71`, :398); a typed override honours judge-kind selectors by
  construction (#668). `Override.Effort` also makes the glance tier's effort selectable per
  integration for the first time — `reasoning_effort:` is reachable by no var at all.
- **A tenancy lock is a statement about providers, enforced in one place, for every bot.** No catalog
  bot carries a per-tenant switch, and the next bot with a rescue route inherits the rule.
- **`{{vars.…}}` routing fields stop being invisible at launch — for the vars whose value is the
  RUN's.** `bots/docs-refresh/main.bot:883` gains a readable route the day its var's default stops
  being an `${…}` reference; until then it reads exactly as today, which is the honest answer.
- **A new obligation on every pre-flight**: it reads through `RouteView`. A derivation added later
  that reads the IR directly re-opens the blindness — the parity test is what reddens on it.
- **Becomes impossible**: dropping an author's rescue route from the operator side; writing an
  override whose value is a template, whose provider is `auto`, or whose provider the pinned harness
  does not read; carrying a routing pin that no level claims (the provenance map always names one);
  changing an integration's routing block without the org seeing it, where the org gates provisioning.
- **Migration and compatibility**: every field is additive and `omitempty`. Old integration records,
  webhook configs, manifests, run documents and queue messages load unchanged; a run launched before
  the fields existed resolves an empty `overrides`, a nil `providers`, and behaves as today. No
  backfill, no dual read.
- **Generated artefacts, never hand-edited**: `openapi.json` + `studio/src/api/schema.ts` by
  `task openapi:gen` (the `Policy`, `RunLLMRoutePolicy` and `RunModelOverride` schemas move; the
  `repo-bots` routes carry no request schema, so the integration half regenerates nothing and its
  studio field is hand-typed); `docs/references/map-packages.md`, `map-docs.md` (this record's index
  row) and `map-bots.md` by `task map:gen`; the bots' canonical form by `task bots:fmt`.
- **Docs to update**: `docs/cloud-llm-credentials.md` § "The routing block" (both fields, their fold,
  their locks, the write-role table), `docs/byok.md` (the per-integration routing block beside
  `key_overrides`), `docs/auto-maintenance.md` § 4 (the integration recipe gains `routing`),
  `docs/agents/engine/tenancy-credentials-spend.md` (what the sovereign refusal can now read and
  name), `docs/bot-runs/review-pr.md` (the rescue route is governed by the tenant's allowlist, not by
  a bot var), and ADR-121 gains a pointer from its § Delivery 2 to both fields.
- **Residuals, accepted**: `locks` is per block, not per cell, and there is no `overrides` ceiling —
  a team that must keep ONE cell locks the block and restates the rest (named follow-up, §5); the
  launcher's `model_overrides` is outside the lock primitive (§6); the board-dispatch and
  pipeline-admission lanes resolve no routing policy at all today (`pkg/server/boarddispatch.go:1864`,
  `pipeline_admission.go:796`) and are wired by this work — an inherited ADR-121 gap; a pipeline
  queued root recovered after a restart re-launches from a minimal spec that carries no overrides
  (`pkg/runview/pipeline_scheduler.go:124-128`, a documented V1 limitation, local mode only); and
  `syncSchedules` carries neither the retry fields nor Routing today — this work adds both, in one
  hunk, because they are the same class.
