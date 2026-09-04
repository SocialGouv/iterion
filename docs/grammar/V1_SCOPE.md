# Current scope of the V1 grammar and AST

The filename and grammar version remain `V1` for compatibility: Iterion has
evolved the original language additively rather than introducing a second file
format. This is therefore a living scope statement, not the 2025 launch
feature list. For exact productions, use
[`iterion_v1.ebnf`](iterion_v1.ebnf); for authoring guidance, use
[`../dsl.md`](../dsl.md).

## Parsed declaration families

| Family | Current surface |
|---|---|
| Inputs and reuse | top-level or workflow `vars` / `attachments`; top-level `presets` / `secrets`; workflow `resources`; compile-time `group` / `use` |
| Prompt and shape | `prompt` with bounded relative `include`, flat `schema`, cursor declarations |
| LLM execution | `agent`, `judge`, model/backend/provider selection, prompts, sessions, tools, permissions, skills, MCP, memory, cursors, compaction, reasoning/timeout limits |
| Deterministic execution | `tool`, Verified Action policy/recovery, `compute`, event `emit`, bounded event `wait` |
| Human control | `human` with `none`, `human`, `llm`, `llm_or_human`, or `review` interaction and review/merge fields; `interaction: async` on `agent` / `judge`, with `await_answers` as its deterministic sync-point declaration |
| Routing | `router` modes `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, and `llm` |
| Nested execution | `subbot` child runs, variable mapping, resource leases, and `isolated` workspace-safety assertion |
| Concurrent observation | `supervisor` declarations with watched nodes, cooldown, evaluation cap, model, and system prompt |
| Workflow | one `workflow` per file, entry, edges, defaults, permissions/capabilities/skills/MCP, budget, resources, interaction, worktree, compression, sandbox |

Supported scalar/schema types are `string`, `bool`, `int`, `float`, `json`,
and `string[]`; string declarations may carry an enum constraint.

## Edges, iteration, and convergence

Edges support data mapping with `with`, guarded routing with `when` or `else`,
and one iteration clause:

- bounded named loops, including runtime-resolved caps;
- explicit `unbounded` loops with fuel and runtime liveness protection;
- ordered finite `as foreach name(item in collection)` iteration.

Every graph cycle must be declared. Static and data-driven fan-out converge on
a supported node with `await: wait_all` or `await: best_effort`.
`fan_out_each` adds `over`, `as`, and optional `key` / `depends_on` fields for
parallel map or DAG scheduling.

## Expressions and templates

Quoted `when` and `compute` fields share the bounded expression language:
field/index access, arithmetic, comparisons, booleans, conditionals,
map/filter/reduce forms, and total collection built-ins. Expressions do not
admit arbitrary functions or recursion.

Runtime templates cover `vars`, `input`, prior `outputs` and histories,
artifacts, attachments, secrets, loop/foreach state, and run metadata.
Compile-time group expansion additionally consumes `{{params.name}}`.
Environment references use `${...}`. Tool commands distinguish shell-escaped
`{{input.x}}` from explicit raw `{{!input.x}}` substitution.

## Still outside V1

| Concept | Boundary |
|---|---|
| General source modules/imports | Prompt files can be included and groups can be expanded, but a `.bot` cannot import arbitrary declarations from another source file. Use a `subbot` for runtime composition. |
| Multiple workflows in one file | The parser can represent them, but IR compilation emits C007; one file selects exactly one workflow. |
| Nested schema declarations | Schemas remain flat; use `json` for nested or open shapes. |
| Runtime node creation or inheritance | Groups clone a statically declared cluster at compile time; they do not create dynamic graph nodes or provide `extends`. |
| Arbitrary expression-language effects | Expressions cannot perform I/O, recurse, or invoke user-defined functions. Use a `tool` or an LLM node for effectful work. |
| Computed variable declarations | Variables are declared statically and resolved from defaults/presets/recipes/launch inputs. Dynamic values flow through node outputs and edge mappings. |
| Arbitrary annotations | The DSL exposes defined metadata such as descriptions and artifact labels, not an open annotation bag. |

Parsing only builds the AST. Cross-reference checks, group expansion, graph
validation, capability checks, routing safety, diagnostics, and IR generation
remain compiler responsibilities under [`../../pkg/dsl/ir/`](../../pkg/dsl/ir/).
