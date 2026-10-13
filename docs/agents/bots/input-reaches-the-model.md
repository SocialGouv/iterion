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
| already on screen under another reference (`{{vars.x}}`, `{{outputs.p.x}}`) | **drop** the field from the input schema AND every `with` mapping of it | the input schema is this node's **alone**: one that is also another node's `output:` cannot lose a field (C034 on the node that reads it, C031 on every edge that maps it — copilot's `turn_input`), so that node gets its own input schema first |
| genuinely invisible | render `{{input.x}}` in the node's own prompt | the key **arrives on every route the node is entered by** — a declared field no edge maps never arrives, the schema seeds nothing |
| per-turn, and the prompt reads `{{vars.x}}` | switch the prompt to `{{input.x}}` | **every** incoming edge maps the key |

Both renders stand on the same fact: the input is built from the selected
edges' `with` mappings (plus, on a loop re-entry, the forward edges the
back-edge overlays), never from the schema. "Arrives on every route" is not
"every edge maps it": a bounded back-edge that leaves a key alone lets the
forward edge's value survive, and the entry node's own floor answers a
defaulted var with no edge at all. A value that changes per turn is the case
where every route does owe the key — the third row. `drop` is the only remedy
with no arrival condition at all — it needs the schema to be this node's alone.

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

- **Prompt text that names the reference.** A line like "Stay inside
  `input.workspace_dir`" is prose, not a template: the compiler says nothing,
  the renderer leaves it alone, and after the drop it names a field the node no
  longer carries. Reword it to the reference the prompt renders — or, where the
  instruction needs a value nothing shows (secured-renovacy's `security_audit`
  decided malware on a pin it was never handed), carry the field and render it.
  `TestCatalogPromptProseNamesAnInputTheNodeShows` sweeps every catalogue
  prompt — agent, judge, router and human surfaces — and asks what a channel
  of that node SHOWS, read through the real renderer, not what its input
  could hold: a field the ratchet baselines is carried and dead just the
  same. A human node is judged per surface, because its two surfaces have
  different readers and each gets the whole input beside its body — the
  operator reads the instructions and the pause's inbound payload, the
  stand-in model of an `llm` / `llm_or_human` gate reads the system prompt and
  the whole input as its user message. Only a review gate's system prompt
  keeps the strict reading: that companion is handed the change under review
  instead (`TestAReviewCompanionReadsItsInputThroughItsSystemPromptAlone`).
  No exemption list. It reads the `input.x` spelling; `vars.x` and
  `outputs.p.x` in prose go dead the same way, and no guard holds that count
  — a forked session can carry such a value, so the reading would have to
  follow session edges.
- **The schema that empties out.** A node whose input schema loses its last
  field gets C140, "which has no field — the node's input will always be
  empty". Delete the schema and the node's `input:` line instead. An edge left
  with an empty `with { }` raises nothing at all, so remove that block by hand;
  and `iterion fmt` is what settles the trailing comma a one-key block loses.

A field a node's text reads but **no** incoming edge maps is the mirror
defect: on every run that read delivers its surface's form of nothing (above).
The compiler warns
(C308) and `TestCatalogInputReadsAreMappedByAnIncomingEdge` holds it — the
entry node aside, whose floor is the launch payload plus the vars that
declare a DEFAULT, so a read of a defaulted var is answered there and a read
of an undefaulted one the launch omits delivers that nothing like anywhere
else (`pkg/runtime/engine_resolve.go`,
`TestBuildNodeInput_EntryFloorSeedsOnlyDefaultedVars`). `ir.NodeInputKeys`
answers what a node's input CAN hold, which on the entry node is an upper
bound: every declared var, because a launch may supply any of them.

Two texts read `input` against something else entirely, so neither C308 nor
this guard speaks for them — and a `with` mapping there is dead plumbing:

- a **fail `message:`** is rendered at fail time against the RUN's inputs,
  the launch payload (`failOutcome` → `resolveMapping`): declare the value in
  `vars:` and write `{{vars.x}}`, which the compiler checks. An
  `{{input.x}}` there is C149's warning, the run-inputs class;
- an **`interaction_prompt:`** is the stand-in model's system prompt when the
  node's own delegate asks questions, and ITS input is the QUESTION map
  (`ExecuteHumanLLMForInteraction`): `{{input.<question_key>}}` is the
  spelling that renders there, and the keys exist only at run time, so the
  compiler says nothing either way.

A human node's `system:`/`model:` outside `interaction: llm`/`llm_or_human`/
`review` — and its review-gate clauses outside `interaction: review` — are
dropped by the compiler; that is C313, not a channel to render.

Every guard named here is part of `task check`.

## Proving one site

The guard is its own witness, both ways:

1. delete the site's entry from `unrenderedBaseline` and run
   `go test ./bots/ -run TestLLMInputFieldsReachTheirOwnPrompt`: it must fail
   naming that exact field;
2. apply the remedy: green;
3. undo the remedy, keeping the baseline shrunk: red again, on that site.

A fix that forgets step 1 fails too — the ratchet's other half refuses a
baselined field that is no longer dead, so the list can only shrink.
