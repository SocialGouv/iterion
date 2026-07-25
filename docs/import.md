# `iterion import` — workflow scripts (.js) → draft .bot

`iterion import` converts a Claude-Code **workflow script**
(`.claude/workflows/*.js` — the `export const meta` +
`agent()` / `phase()` / `log()` shape) into a **draft** `.bot`
workflow.

```bash
iterion import .claude/workflows/simple-bugfix.js            # writes simple_bugfix.bot next to the source
iterion import flow.js --out bots/drafts/flow.bot --name flow
iterion import flow.js --dry-run                              # print, don't write
iterion import flow.js --dry-run --json                       # machine-readable report + draft
```

## Guarantees

1. **Zero execution.** The source is parsed with the vendored goja
   parser into an AST and walked — no JS VM is ever constructed, no
   code runs. Importing a malicious script is exactly as dangerous as
   reading it.
2. **Never a plausible-but-wrong translation.** Anything the lowering
   cannot express in the DSL degrades **visibly**: an annotated
   `## IMPORT` comment in place, an entry in the report, and — for
   dynamic prompt fragments — a promoted `hole_N` var the operator
   must fill. The importer prefers an honest gap over an invented
   equivalent.
3. **The draft always compiles.** Before anything is written the
   generated source is run through the real parser + `ir.Compile`; a
   draft that fails to compile aborts the import (that is an importer
   bug, not an input problem). Non-error diagnostics are folded into
   the report.
4. **The report is embedded.** Every draft starts with an
   `## IMPORT REPORT` header — mapped constructs, holes, placeholders,
   drops, each anchored to a `js:<line>` — so the draft can never
   masquerade as a faithful translation.
5. **No silent overwrite.** `import` refuses to replace an existing
   `.bot`; pick another path with `--out`.

## Mapping table

| Script construct | .bot lowering |
|---|---|
| `export const meta = {name, description, phases}` | `workflow <name>:` + `##` header comments (description, phase list) |
| `const X_SCHEMA = { type:'object', properties:{…} }` | `schema x_schema:` — `string`/`boolean`/`integer`/`number`/`array of string` → `string`/`bool`/`int`/`float`/`string[]`; anything else → `json`; `enum` (strings) → `[enum: "a", "b"]`; `description` → field `##` comment |
| `await agent(prompt, {label, schema, phase, model, effort})` | `agent <label>:` + `prompt <label>_user:`; `schema` → `output:`; `model`/`effort` pass through; `phase` → `##` comment |
| `` `text ${x} text` `` and `'a' + x + 'b'` | prompt text with each hole resolved: static const → inlined; args-derived const → `{{vars.<name>}}` (var auto-declared); agent result → `{{outputs.<node>.<field>}}`; fan-out param → `{{outputs.<router>.<alias>.<field>}}`; anything else → `{{vars.hole_N}}` + report |
| `if (cond) return {…}` after an agent | `<node> -> done when "<cond>"` + the surviving path as `<node> -> <next> else` — only when `cond` reads that agent's own structured output (`x.field === 'v'`, `!x \|\| x.f !== 'v'`, `x && x.f === 'v'`, `!x.flag`, comparisons); otherwise the exit is dropped with an `## IMPORT TODO` |
| `if (cond) throw …` | same, targeting `fail` |
| `for (let i = 1; i <= N; i++) { …agent()… if (c) break }` | body chained; back edge `<last> -> <first> when "<not c>" as loop_K(N)`; exit `<last> -> <next> else`. No translatable break condition → the loop is NOT lowered (body imported once, `## IMPORT TODO`) because a `.bot` loop exits by condition — exhaustion fails the run |
| `while (…) { …agent()… if (c) break }` | same with `as loop_K(unbounded 25)` (adjust the fuel) |
| loop without `agent()` calls | body imported once + report note (compute loops have no .bot equivalent) |
| `Promise.all(items.map(x => agent(…)))` | `router fan_K: mode: fan_out_each / over: <items ref> / as: x` → stage agent(s) → synthesized `tool join_K` with `await: wait_all` |
| `pipeline(items, s1, s2, …)` | same router; the stages become a per-item agent chain before the join |
| `parallel([() => agent(…), …])` | `router fan_K: mode: fan_out_all` → one agent per thunk → `join_K` (`await: wait_all`) |
| `phase('X')` / `log(…)` | `## Phase: X` / `## log: …` comments |
| `const x = <expr mentioning args>` | promoted to `vars: x` (fill at launch with `--var x=…`) |
| final `return {…}` | `<last> -> done` |
| `try { … } finally { … }` | body inlined, handlers dropped + report (recovery belongs on Verified Action tool nodes) |
| assignments to locals (`lastFailure = …`) | dropped + report (loop-carried state: `loop.<name>.previous_output`) |
| helper `function f() {…}`, classes, `for…of`, switch, destructuring, anything else | annotated `## IMPORT` placeholder + report — **never a crash** |

## Semantics that intentionally do NOT carry over

- **Loop fall-through.** A JS `for` loop that never `break`s simply
  continues after N rounds; a `.bot` loop that exhausts its bound
  FAILS the run (`LOOP_EXHAUSTED`). The report flags every lowered
  loop with this note — add an explicit exhaustion path if the
  fall-through was load-bearing.
- **Mutable locals.** `.bot` has no mutable state; feedback across
  loop rounds is the reviewer/judge's own previous output
  (`loop.<name>.previous_output`), not a variable you assign.
- **Fan-out result aggregation.** `const all = await Promise.all(…)`
  binds an array; the lowered graph converges on a `join` node and
  downstream nodes read per-branch `{{outputs.<stage>}}` instead.
- **Parallel mutation.** JS fan-outs freely mutate a shared tree; the
  iterion runtime enforces workspace safety (one mutating branch).
  Keep fan-out stages read-only, or run them as `isolated:` subbots.

## After importing

1. Read the `## IMPORT REPORT` header top-to-bottom.
2. Fill every `hole_N` var (or replace the ref with real DSL) and
   resolve every `## IMPORT TODO`.
3. `iterion validate <draft>.bot`, then a dogfood run.
4. Promote it to a bundle when it earns a manifest: scaffold one with
   `iterion bots create <slug>` and move the workflow into it (`iterion
   bundle` only exposes `pack`; `bundle init` was retired).

## In the studio

The `/bots` gallery's **Import → From a workflow script (.js)…** entry
runs the same conversion over `POST /api/v1/bots/import` (local-mode
only). It previews the draft and its IMPORT REPORT before you commit,
then saves it into `bots/<workflow-name>.bot`. The endpoint mirrors the
CLI's contract: `dry_run` returns the draft + report without writing;
a write refuses to overwrite an existing file (409); unparsable JS or a
script with no `agent()` calls is a 422.

The importer is a **porting accelerator**, not a compatibility layer:
the goal is a readable first draft carrying over prompts, schemas and
graph shape, with every gap marked. Implementation:
[pkg/botimport](../pkg/botimport/botimport.go) (shared by the CLI and
the studio route).
