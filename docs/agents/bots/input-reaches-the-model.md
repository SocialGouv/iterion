# A value on an input reaches the model only if a channel of THAT node renders it

Read it when adding, dropping or re-referencing a field on an LLM node's input schema or on an edge's `with`.

## What "reaches the model" means

A key an edge's `with` maps onto an LLM node is written into the node's input
by the runtime (`pkg/runtime/engine_resolve.go`) whether the input schema
declares it or not. Nothing more happens. The model sees the value only if one
of **that node's own** channels renders it: its system or user prompt, its
`images:` list on the codex backend, or — for a node with no user prompt — the
whole input, JSON-serialized. A sibling node's prompt rendering the same name
does not count.

So a declared-and-mapped field no channel renders is plumbing that delivers
nothing, and any instruction reasoning about it is dead text. `bots/llm_input_reaches_its_prompt_test.go`
measures it by rendering each node's channels through the real resolver, and
holds the remaining debt as a two-sided ratchet.

## The remedy is per site, and each one has a precondition

| The value is | Remedy | Precondition |
|---|---|---|
| already on screen under another reference (`{{vars.x}}`, `{{outputs.p.x}}`) | **drop** the field from the input schema AND every `with` mapping of it | the input schema is this node's **alone**: one that is also another node's `output:` cannot lose a field (C034 on the node that reads it, C031 on every edge that maps it), so that node gets its own input schema first — what `adr-cartograph/survey_code` did against the differ's `scan_output`, and `copilot/copi` against `turn_input` |
| genuinely invisible | render `{{input.x}}` in the node's own prompt | the key **arrives on every route the node is entered by** — a declared field no edge maps never arrives, the schema seeds nothing |
| per-turn, and the prompt reads `{{vars.x}}` | switch the prompt to `{{input.x}}` | **every** incoming edge maps the key |
| carried only by an inherited session (`session: inherit_if_available`) | render `{{input.x}}` | the arrival condition of the invisible row — a session is not a second channel: a best-effort session whose backing state is gone degrades to fresh (`applySessionContinuity` marks it droppable, `pkg/backend/model/executor_retry.go` retries without it — lived on `branch-improve-loop/plan_revise`) |

All three renders stand on the same fact: the input is built from the selected
edges' `with` mappings (plus, on a loop re-entry, the forward edges the
back-edge overlays), never from the schema. "Arrives on every route" is not
"every edge maps it": a bounded back-edge that leaves a key alone lets the
forward edge's value survive, and the entry node's own floor answers a
defaulted var with no edge at all. A value that changes per turn is the case
where every route does owe the key — the third row. `drop` is the only remedy
with no arrival condition at all — it needs the schema to be this node's alone.

WHICH prompt the render goes in is part of the remedy when the value is
another model's text. A node's `UNTRUSTED INPUT BOUNDARY` paragraph calls
the system prompt authoritative, so a peer's critique rendered there
arrives as mission however carefully the paragraph names the field as data
— and the paragraph's own guards stop at its lines. Render it in the USER
prompt; `TestPlanRevisePromptCarriesThePlanAndNamesItAsData` refuses, in
the seven `plan_revise` system prompts, every value the run produced and
not an input reference alone: the fields this same resolver sees rendered
there, and every reference outside an allowlist of the operator's and the
engine's namespaces (`vars`, `run`, `secrets`) — because the same critique
is also `{{outputs.plan_review.concerns}}`, the name the `plan_gate` edge
writes.

ONE typed field keeps N prompts one text. Where the same remedy is owed by a
family — the seven `plan_revise` nodes, whose mission differs per bundle while
their prompt does not — the per-bundle difference belongs in the EDGE value and
the prompt renders a single reference: `plan_revise_input.mission_context`,
which each `plan_gate` edge fills from the same `{{vars.*}}` that bundle's own
`plan_user` renders. That buys a pair of guards no per-bundle prose could
carry: `TestEveryMissionVarThePlanNodeRendersReachesPlanRevise` discovers the
mission mechanically (every var the `plan` node's own channels render) and
requires it back out of `plan_revise`'s channels on every incoming route, and
`TestThePlanRevisePromptsAreOneText` holds the seven prompts to one text, so
the block cannot be dropped from one bundle while six keep it.

The reverse shape is a REGISTER the graph already carries from node to node
and no prompt renders — copilot's `turn_state.mode`, the conversation posture.
There the remedy is the input-schema field AND the edge mapping in the same
change, because a key an edge maps onto a node the schema does not declare
draws C034 on the reader and C031 on the edge: `copi_input` is copi's own
schema precisely so that pair is possible (#2562).
`TestCopilotTurnOnePostureReachesTheModel` walks the four stages a value
crosses on the turn-1 path — the seed's expression, the register's, the edge's
mapping, then the node's own prompts — each read from the compiled bundle and
run by the engine's own evaluator and resolver, so dropping any one of them
reddens.

The third row is the one that bites. `{{vars.x}}` is the launch value for the
whole run; a loop-back edge that maps `x` to something fresher (the operator's
newest steering, a counter) is invisible to a prompt reading `{{vars.x}}`,
which keeps showing turn 1. Switching that prompt to `{{input.x}}` fixes the
turn, and what a route that does **not** map the key then delivers is never an
error — it is the route's and the surface's own form of nothing:

- an exclusive **forward** route leaves the key absent. On a text a MODEL
  reads — an agent's, a judge's or an LLM router's prompt, and the system
  prompt of an `interaction: llm` / `llm_or_human` human node — the resolver
  keeps an unresolvable reference as written, so the model reads the literal
  `{{input.x}}` placeholder (`pkg/backend/model/template_resolver.go`). The
  operator-facing texts go the other way: a human node's `instructions:`, its
  `review_url` and a review gate's system prompt are rendered by
  `Engine.renderHumanInstructions`, which substitutes the empty string — the
  same miss, silent (`TestHumanInstructionsDropAnAbsentInputToTheEmptyString`).
  Outside the prompts, a tool's `command:` hands the braces to the shell,
  while a `script:` and a connector action's `params:` read the language's
  null; a compute expression reads nil, which travels on as a null or a zero
  where it types and fails the node where it does not. `absentReadDelivers`
  (`bots/catalog_input_mapping_test.go`) holds the reading per surface, and the
  guard's message quotes it;
- a bounded **back-edge** only overlays, so the forward edge's value survives:
  the stale launch value, the very defect the switch was made against
  (`pkg/runtime/incoming_test.go`,
  `TestSelectedIncoming_LoopReentryOverlaysPartialBackEdge`).

A per-turn value the launch also carries is two values, not two spellings of
one: `{{input.x}}` overwrites the launch brief from turn 2, so render both,
labelled (evolve's `investigate_user`), or the turn gains what the mission
loses.

No catalogue-wide guard holds that count, on purpose: a loop's back-edge
mapping only what changes per iteration is the normal case, so
`TestCatalogInputReadsAreMappedByAnIncomingEdge` asks for **some** edge and no
more. Where the value changes per turn on *every* route, pin the node:
`TestEvolveInvestigateTakesItsSteeringFromEveryIncomingEdge` reads the
`{{input.*}}` its prompts render and requires every incoming edge to fill
them.

## Two traps a drop leaves behind

They are [input-drop-traps.md](input-drop-traps.md).

## Proving one site

The guard is its own witness, both ways:

1. run `go test ./bots/ -run TestLLMInputFieldsReachTheirOwnPrompt` before
   the remedy: the baseline holds no entries, so the sweep itself must fail
   naming that exact field — a dead field reddens on its own, there is no
   baseline entry to delete first;
2. apply the remedy: green;
3. undo the remedy, keeping the baseline shrunk: red again, on that site.

The only entry a site can carry is one someone re-registered into
`unrenderedBaseline`; deleting it gives the same red, and a name re-entered
there fails both ways until a real dead site sits under it — a baselined
field that is no longer dead is refused like a new one, so the list can only
shrink.
