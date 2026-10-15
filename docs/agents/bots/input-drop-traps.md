# The two traps a dropped input leaves behind

Dropping a dead field from an LLM node's input ([input-reaches-the-model.md](input-reaches-the-model.md) holds the remedy table) leaves two kinds of text behind. Both read as live to a reader, and neither is one.

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
