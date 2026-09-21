# What's new in the `.bot` DSL — the authoring-first programme (2026-09)

Between v3.128.0 and v3.154.0 the DSL, its tooling and its documentation were
reworked with one measurable goal: an agent given the skill writes a complex,
correct bot on its first draft, and a human reads a `.bot` without guessing.
This page is the tour of what landed; the [DSL guide](dsl.md) is the language
reference, the [CLI reference](cli-reference.md) the commands, and the
[diagnostics catalogue](references/diagnostics.md) every `C` code named below;
the parse-stage `E0xx` codes are in
[`pkg/dsl/parser/diagnostic.go`](../pkg/dsl/parser/diagnostic.go).

## A version in the file

- `dsl: 2` as the file's first significant line (after comments and the
  `## ---` frontmatter). Absent means profile 1 — the grammar as it was,
  frozen. Since v3.141.0 ([ADR-098](adr/098-dsl-versioning-and-authoring-surface.md)).
- The profile governs changes of **meaning** only, and profile 2 carries four:
  a `"…"` string reads the standard escapes (`\n`, `\"`, `\\`…) with no
  directive; a blank line inside a prompt body is kept as the paragraph break
  the author wrote (profile 1 drops it, so a paragraph reaches the model as
  one newline); the `## strict-escape` directive and the retired
  `project_root:` are refused (E042, E043). Everything else reads the same in
  both profiles.
- `iterion dsl migrate --to 2 <file|bundle>` moves an existing bot: it
  re-spells the literals so their values do not change, names the prompts
  whose paragraph breaks now reach the model (`--show-prompts`), raises the
  bundle's `requires.iterion` to the build that reads the profile, and leaves
  every other byte alone; `--dry-run` and `--check` write nothing.
- `iterion validate` says when a headerless file is one profile 2 would read
  otherwise (C144), only at validation, never at launch. The engine floor stays
  where it was: `requires.iterion` in the manifest answers "does this build
  run it", `dsl:` answers "how is this text read" — two axes, never one.

## A bot in several files

- `import "lib/<file>.bot"` at the head of a file, one line per fragment. The
  unit — the main and every fragment its imports reach — compiles as one
  program: declarations append in the order the files are reached,
  `vars:`/`presets:`/`attachments:`/`secrets:` merge by key, and a name or a
  key declared twice is refused naming both places (E010). Since v3.145.0.
- One unit loader serves the CLI, the server, the runner and the studio; the
  studio saves each declaration back into the file that owns it. A path
  outside `lib/`, a missing fragment or a cycle is refused by name
  (E044–E047); a file that imports, compiled alone, is refused as such (C030).
  See [import](import.md).

## One description of the grammar, rendered everywhere

- A property registry (`pkg/dsl/spec`) describes every kind of the language —
  its properties, their forms and values — and a conformance test holds it to
  the parser in both directions: what the registry says is what the parser
  accepts. Since v3.133.0.
- Rendered from it: the [property reference](references/dsl-properties.md),
  the [readable grammar](references/dsl-grammar.md), the skill's reference
  section and the studio editor's keyword module. A stale rendering fails CI;
  `iterion dsl spec --write` regenerates.
- An unknown property names the closest accepted one and the block it belongs
  to (E012), and an unknown tool name the nearest built-in (C135); a node may
  carry any name but `done` and `fail`; `#` opens
  a comment. Every diagnostic carries `file:line:column`, a title and a `fix:`,
  and `validate --json` returns them as objects (code, severity, position,
  message, hint, node, edge). Since v3.128.0.

## Start from a shape that works

- `iterion bots templates` and `bots create --template <id>`: fourteen
  templates compiled in CI, nine of them composite canonical shapes — a campaign
  with a verify gate and a bounded loop, a reviewer fan-out with a deterministic
  convergence, plan → human gate → implement, a scheduled digest, one subbot
  per ticket, a verified action with a postcondition, asynchronous questions
  with one sync point, a bundle whose prompts and skills live outside the
  workflow file (since v3.140.0), and a bot in several files on `import` (since
  v3.145.0).
- The rules the grammar does not show are written in the skill, one bullet
  each: a loop's exhaustion exit, a `tool`'s stdout-JSON contract, `outputs.*`
  without threading, `expr:` is an expression and not a template, the `loop.`
  namespace, no quotes around a reference in a `command:`.

## The dry run: `validate --exec`

- A program that compiles is run twice under a simulation — every condition
  true and every enum at its first value, then the other way — with no model,
  no shell and no workspace. Each node's prompts, `command:`, `script:` and
  `postcondition:` are rendered by the production renderers, every reference
  left as written is named with its reason, shell text is held to `bash -n`,
  and humans, `wait`, `await_answers` and subbot children are answered or
  simulated within the bundle. Since v3.153.0
  ([ADR-100](adr/100-dry-run-at-the-executor-seam.md)).
- The report says what the first paid run would have met: the passes and
  their paths, the findings by node, the nodes whose output was a shape, the
  nodes and edges no pass reached, and two verdicts — `clean` and `failing`.
  `--strict` turns `failing` into an exit code for a CI gate (an expression
  left `inconclusive` on a shaped `json` value is printed, not failed); `--fixtures f.json` replays recorded
  outputs; `--exec-timeout` bounds a pass; `--var k=v` and `--preset` give the
  run its launch values (v3.154.0). The MCP tool `local_validate` takes the
  same switches.
- An end the shapes imposed — a loop whose exit rides a verdict the dry run
  shapes, the bot's own budget — is said as a **ceiling** and is not a death;
  a bounded loop spent with no exit is the program's death, the shape C145
  names. See [the CLI reference](cli-reference.md).

## Format, fix

- `iterion fmt [--check]` writes a file in its canonical form — the studio's
  writer, verified to read back as the same program — and refuses a file whose
  comments sit after the head rather than rewrite it without them.
- `iterion fix [--dry-run]` applies the mechanical remedies of diagnostics
  (C137: the quotes an author wrote around a reference in a `command:`) and
  proves every edit by recompiling. Both write atomically, through a symlink.

## Diagnostics that catch the first paid run's death

- C145: a bounded loop with no exit at its cap — the most frequent death of a
  new bot; the engine now fails it as a typed `LOOP_EXHAUSTED` and says why it
  declined the loop's edge.
- C146: a division under an `int` field without `floor`/`round`. C147: a
  `{{loop.x}}` that names no loop. C137 tightened: a quoted `artifacts`,
  `attachments` or `loop` reference in a tool body is refused (it read as
  braces and would have read as a value at upgrade), and `iterion fix` removes
  the quotes. C142: `worktree:` is checked. C144: profile 1 assumed, and it
  matters.

## A public contract, held to the program

- `contract <name>:` declares what a bot takes, produces, delivers and checks;
  the workflow names the one it keeps with `contract: <name>`. A contract the
  program contradicts is a compile error (C300–C304), never a display, and
  `validate` projects it. Since v3.150.0
  ([ADR-099](adr/099-public-contracts.md)). See [the guide](dsl.md#the-public-contract--contract).

## The skill

- One skill, `iterion-dsl` (`SKILL.md` at the repository root, installed with
  `npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl`),
  mirrored in the whats-next quickref the runs carry. Its reference section is
  generated from the registry; its "Rules the grammar does not show" are the
  rules the authoring probes had to guess; its loop is written down — write,
  `validate --json`, fix, `validate --exec`, then run, against the build the
  bot's `requires.iterion` names. See [skill.md](skill.md).

## Measured

The [authoring probe](references/dsl-authoring-probe.md) writes the same
ten-requirement bot from a fresh session, first draft before any `validate`:

| Probe | Model | Errors at first draft | Read before the first line | Minutes | `validate` rounds to green |
|---|---|---|---|---|---|
| 2026-09-09, before the programme | opus, sonnet | 0, 0 | the whole documentation, ~2 300 and ~2 500 lines (~213k and ~278k session tokens) | 20, 37 | 0, 0 |
| 2026-09-10, registry and gallery | opus, sonnet | 0, 0 | 1 509 and 2 497 lines | 12, 16 | 1, 1 |
| 2026-09-16, the complete DSL, a mid-size model (haiku 4.5) reading the skill and three templates | haiku | 4 (one graph, three template-for-expression), 0 lexical | 1 113 lines | 3.5 | 2 |
| 2026-09-16, the same with no access to the documentation or the catalogue | haiku | 4 (graph and reference), 0 lexical | ~1 200 lines | ~4 | 2 |

Rounds count the agent's `validate` runs up to and including the green one;
the 2026-09-09 agents were forbidden to validate, so their rounds read 0 and
their drafts were green when validated afterwards.

Held: no lexical or keyword-collision error at all, even from a mid-size model
reading only the skill; the cost of entry went from a session of reading to a
few minutes; the errors left are semantic (exhaustiveness, a loop's exit, a
condition's field, the loop namespace, a template written where an expression
goes), and the diagnostics have them fixed in one round.
Not held yet: the target of at most two semantic errors at the first draft is
missed by the capped haiku run (four at the draft; the standard run holds it),
and `--strict` stays a per-bot gate on the catalogue until the dry run crosses
a bounded loop a few times only (#1307).

## Compatibility

A profile-1 bot reads as it did: the header governs meaning, and its absence
means profile 1. The catalogue and the examples compile clean on every CI run
under the new compiler; three changes outside the profile could touch a
third-party bot, each said by a diagnostic that names its fix: the C137 error
on a quoted namespace reference in a tool body, the registry's values held to
the compiler (a `sandbox.mode` or `network.mode` the parser used to let
through), and `{{input.a.b}}` in a tool body now reading the leaf, as it always
did in a prompt.
