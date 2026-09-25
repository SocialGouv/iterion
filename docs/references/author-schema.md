# The author document — keys and values

An author document (`x.bot.yaml`) is the YAML twin a `.bot` can be written as: the same declarations under the same names, with YAML's own values. It is a draft of `x.bot`, never a program — why, and how to work with one, is in [dsl.md](../dsl.md#writing-the-twin-in-yaml) and [ADR-102](../adr/102-yaml-author-twin.md). This page says what the document holds and how each value is written; the properties of every kind are the `.bot`'s, listed in [dsl-properties.md](dsl-properties.md).

The JSON Schemas beside this page — [`iterion-author.schema.json`](iterion-author.schema.json) for every profile, one per profile beside it — check the same shape in an editor.

<!-- dsl-spec:begin author -->
_Generated from the parser's property registry (`pkg/dsl/spec`) by `iterion dsl spec --write`; do not edit by hand. The author JSON Schemas beside this page are rendered from the same registry._

### Top-level keys

In the order the writer puts them (`iterion fmt`, `iterion fmt --to yaml`).

| Key | Written as | Holds |
|---|---|---|
| `dsl` | an integer, required | The syntax profile the document is read in — `1` or `2`, a bare integer, never `2.0`. A document always names it (E052). |
| `catalog` | a mapping of `name`, `description`, `triggers`, `capabilities` | The catalog identity the .bot carries in its `## ---` frontmatter. |
| `imports` | a list of `lib/<file>.bot` | The fragments of the bot's unit, read beside the .bot the document stands for, as `import` lines are. |
| `vars` | a mapping keyed by name | Typed run parameters, overridable with --var and presets. |
| `presets` | a mapping keyed by name | Named bundles of var values selected with --recipe / --preset. |
| `attachments` | a mapping keyed by name | Operator-supplied files and images the run receives. |
| `secrets` | a mapping keyed by name | Secrets the run resolves by name from the team's or the local store (docs/secrets.md). |
| `schemas` | a mapping keyed by name, each value a mapping keyed by field | A structured-output shape; a bare header declares an empty schema. |
| `prompts` | a mapping keyed by name, each value its text | A named text block, referenced by `system:` / `user:` / `instructions:`; its body is free text with `{{…}}` references and `{{include "file"}}` directives, the first line's indentation stripped from every line. Blank lines in the body are dropped by the lexer in profile 1 and kept as paragraph breaks in profile 2; a bare header declares an empty prompt. |
| `cursors` | a mapping keyed by name, each value a mapping of its properties | A prompt-engineering dial: an enum (values:) or a numeric band map (bands:) over [0, 1], each entry carrying a prompt fragment (C083–C086). |
| `supervisors` | a mapping keyed by name, each value a mapping of its properties | A concurrent LLM watcher of agent nodes that enqueues steering messages the watched node reads at its next turn (docs/supervisors.md); run metadata, not a graph node. |
| `mcp_servers` | a mapping keyed by name, each value a mapping of its properties | An MCP server the workflow may activate: stdio (command/args) or http/sse (url), optionally OAuth2. |
| `contracts` | a mapping keyed by name, each value a mapping of its properties | The public interface of a bot, named by the workflow's `contract:`; no prompts, tools or provider configuration. A bare header followed by a blank line or the end of the file declares an empty contract. |
| `groups` | a list, each item `group: <name>` with its header parts and its nodes | A reusable node cluster with parameters, whose body holds agent/judge/router/human/tool/compute declarations and edges; instantiated by `use`, expanded at compile time (C141 warns on a use of an empty group). Prompts read `{{params.name}}`; nodes are addressed as <prefix>.<node> once instantiated. |
| `uses` | a list, each item `use: <group>` with its header parts | One instance of a group, on a single line with no body: the name after `use` is the group's, `as` gives the instance its prefix, the with map binds the group's parameters. |
| `nodes` | a list, each item `<kind>: <name>` and that kind's properties | The graph's nodes, in order. |
| `workflow` | a mapping of `name`, `entry`, `edges` — one `.bot` edge line per item — and the workflow's properties | The one workflow of the file. |

### Nodes

Each item of `nodes:` names its kind and its node on its first key, then carries that kind's properties — the .bot's, under the same names:

- `agent: <name>` — [properties](dsl-properties.md#agent)
- `judge: <name>` — [properties](dsl-properties.md#judge)
- `router: <name>` — [properties](dsl-properties.md#router)
- `human: <name>` — [properties](dsl-properties.md#human)
- `tool: <name>` — [properties](dsl-properties.md#tool)
- `compute: <name>` — [properties](dsl-properties.md#compute)
- `subbot: <name>` — [properties](dsl-properties.md#subbot)
- `emit: <name>` — [properties](dsl-properties.md#emit)
- `wait: <name>` — [properties](dsl-properties.md#wait)
- `await_answers: <name>` — [properties](dsl-properties.md#await_answers)
- `fail: <name>` — [properties](dsl-properties.md#fail)

### Values

A property's value is written by its form — the form the .bot reference gives it:

| Form | Written in the author document as |
|---|---|
| string | a YAML string — plain when it reads back as the same text; quoted when it holds `: ` or ends in `:` (the document is refused otherwise, E050) or holds ` #` (otherwise a comment starts there and the value ends), when it starts with a character YAML reserves — `{`, `[`, `!`, `&`, `*`, `\|`, `>`, `%`, `@`, a backtick; a `{{…}}` template written unquoted is a YAML mapping (refused) — or when it would read as a number, a bool, a date or null (refused, E051: quote it); inside single quotes a `'` is written twice (`''`); a `\|` block for several lines |
| quoted string | a YAML string, as `string` |
| string\|ident | a YAML string, as `string` |
| ident | a name — letters, digits, `_` — plain (quoted, it reads the same) |
| dotted ident | a name, dotted for a group instance's node or a node's field (`r1.look`, `node.field`) |
| int | an integer as digits, without a leading 0: `3` — never `3.0`, and none of YAML's other spellings (`010`, the octal 8; `0x10`, `+3`, `1_000`), which are refused, YAML's reading named |
| number | a finite non-negative number as digits, without a leading 0: `3`, `0.8` — YAML's `1e2`, `.5`, `01.5` or `+3` are refused, YAML's reading named |
| bool | `true` or `false` — YAML 1.2 reads `yes`, `no`, `on` and `off` as strings, refused |
| json value | a YAML value of the JSON subset: text, a non-negative number, a bool, null, a list, a mapping |
| enum | one of the listed words |
| enum\|env | one of the listed words, plain — or any other value quoted, kept as written for a run-time substitution (`'${EFFORT:-high}'`) |
| prompt ref | a declared prompt's name, plain — or the prompt's own text, quoted or as a `\|` block |
| string\|number | a string, a number or a bool; a number written as digits, a `-` included (`3`, `0.5`, `-10`), or `true`/`false`, is the text it spells (`30s` is a string) — YAML's other spellings (`010`, `0x10`, `+3`, `1e2`, `True`) are refused: quote them if they are text |
| ident list | a list of names: `[a, b]`, or one `- a` per line |
| string list | a list of strings: `[a, b]`, or one `- a` per line |
| tool list | a list of strings, as `string list` |
| skill list | a list of strings, as `string list` |
| string\|ident list | a list of strings, as `string list` |
| ident \| ident list | a name, or a list of names |
| map | a mapping of names to strings: `{KEY: v}`, or one `KEY: v` per line |
| `with { … }` | a mapping of names to strings; a number or a bool written as the .bot writes one, a `-` included (`3`, `0.5`, `-10`, `true`), is the string it spells — YAML's other spellings (`007`, `True`) are refused: quote them |
| block | a nested mapping of that kind's properties |
| ident \| block | a word, or a nested mapping of that kind's properties |
| literal | a string, an integer or a float as digits, or a bool |
| type | a builtin type or a declared schema's name, `[]` suffixes allowed |
| ident \| number \| string | a string or a number, as `string\|number` |
| int \| string list | an integer (a count), or a list of strings (a pool of member ids) |
<!-- dsl-spec:end -->
