# ADR-098 — DSL versioning, explicit imports, and the authoring surfaces

- Status: accepted (2026-09-09)
- Relations: ADR-050 (totality and `unbounded`), ADR-059 (skill library),
  ADR-093 (leaf packages), the `.bot` as a git-native artifact
  ([docs/philosophy.md](../philosophy.md) §4), issue #1010 (the authoring
  program this ADR governs, lots 0.5 → 5), issues #1012 / #1013 / #1015
  (the transport defects lot 0.5 closes).

## Problem

An agent given the DSL documentation can write a complex `.bot` that is
right the first time — measured twice (opus 331 lines, sonnet 215 lines, 0
static errors) — but only after reading 200–280 k tokens and reverse-
engineering six rules that were written nowhere. Along the way three
structural facts showed up that need a decision, not a lot:

1. **The language has no version.** The only per-file syntax switch is the
   ad-hoc `# strict-escape: on` directive. Every lift of a lexical trap
   (`#` comments, YAML-style lists, keyword-named nodes) is either additive
   or a silent change of meaning for an existing file, and nothing in the
   file says which reading it expects.
2. **A bot is one file.** `{{include}}` covers prompt text, `prompts/*.md`
   covers a bundle's prompts, `subbot` covers another run; schemas, nodes,
   groups and fail nodes live in a single `main.bot` (median 1 300 lines,
   maximum 7 100). An agent editing one holds the whole file in its head,
   and an indentation slip is global.
3. **Three surfaces claim to be the program.** The `.bot` text, the JSON AST
   (the cloud transport and the studio document) and the unparser's output
   (the studio's save path) — and the last two dropped whole constructs
   (`group`/`use`, `as foreach`, named pools; raw strings) until lot 0.5.
   A YAML authoring twin was proposed on top.

## Decision

### 1. A `dsl:` header versions the TEXT, `requires.iterion` the ENGINE

The first significant declaration of a file may be `dsl: N`. Absent means
`1`: today's grammar, frozen — bug fixes only. This is the compose
`version:` contract: nothing existing moves. The lexer pre-scans the header
the way it pre-scans `strict-escape` today, the version rides in the JSON
AST and in the cloud snapshot, and a runner never guesses.

Two orthogonal axes, never conflated:

| axis | where | the question it answers |
|---|---|---|
| syntax profile | `dsl: N` in the `.bot` | how to READ this text |
| engine floor | `requires.iterion` in the manifest | can this build RUN it |

The version governs **changes of meaning only**. A trap lifted additively
(a text that was invalid becomes valid, no valid text changes meaning) is
accepted in every profile without a bump — `#` as a comment shipped that
way in lot 0. `dsl: 2` carries at least one real change of meaning from
the start, so it is not decorative: standard escapes by default (`"\n"` is
a newline in v2, a backslash and an `n` in v1; the `strict-escape`
directive disappears in v2), removal of the legacy forms (`delegate:`,
`project_root:`, the dead `join`), and reservation — a new keyword enters
only in the next profile.

`iterion migrate dsl --to 2` rewrites mechanically (parse v1 → AST →
unparse v2), which is why the unparser had to become lossless first
(#1015). A file without `dsl:` validates with a warning ("profile v1
assumed"), never an error, never at launch.

### 2. v2 desugars to a v1-compatible AST; anything else bumps the queue schema

The v2 profile lifts the lexical and collision traps (YAML-style lists,
`#`, keyword-named nodes AND references to them, non-string `with` values,
tool-name aliases after an exact-match miss and never on `mcp.*`, `\{{` as
a literal). Each is a compile-time desugaring into the AST v1 already
expresses: chained edges become N edges, an inline prompt string becomes an
anonymous prompt, a YAML list becomes a list, a coercion becomes a string.
The runner therefore compiles the same AST whichever profile the author
wrote in, and a mixed fleet (a v2-aware server, an older runner) keeps
working. A v2 construct that cannot desugar to the v1 AST is a **queue
schema bump** (`pkg/queue/types.go`), and an older runner refuses it
loudly. Three axes, then: document profile, transport version, engine floor.

The load-bearing rules do not move with the profile: declared bounded
cycles, exhaustive conditions, `wait`/`await_answers` timeouts, typed
computes, refused unknown properties. They are guarantees the product
sells; the lots make them easier to satisfy, they do not lift them.

### 3. `import "file.bot"` is explicit, relative, confined, and a compilation UNIT

A file declares its imports in its header; a path is relative to the
importing file, confined to the bot's directory (no absolute path, no `..`,
symlinks resolved — the `{{include}}` guard). The unit of compilation is the
transitive closure, merged at the `ast.File` level: lists concatenate,
`vars`/`secrets`/`attachments`/`presets` merge by key, a duplicate is an
error naming BOTH files, one `workflow` per unit, an import cycle is an
error. **Each imported file carries its own `dsl:`** (absent = v1); the AST
is profile-independent.

Why explicit and not "every `.bot` in the directory": `subbot source:` is
another run, another unit, and `examples/composition/` keeps three
workflows side by side. Fragments live under `<bot>/lib/*.bot`, which the
catalogue discovery never compiles as a workflow. The loader returns the
unit as `{files, digest}`, and everything that fingerprints a source —
`resume`'s change detection, `rewind --auto`, the registry cache — reads
that digest. An inline launch flattens the unit through the (now lossless)
unparser before upload, and the cloud snapshot freezes the whole
collection; a runner never resolves an import against its own filesystem,
as it never resolves an include (#1013, this lot).

The studio saves **by provenance** from lot 3: the document carries the
owning file of every declaration (`Span.File`) and a save rewrites each
owning file — never a flattened `main.bot` that would erase the split.

### 4. Two surfaces are the program's truth; the rest are projections

- The **`.bot` text** is the committed truth (git-native, reviewable,
  diffable), pre-arbitrated in the philosophy.
- The **JSON AST** is a transport and a document: it must carry the whole
  language (`TestEveryASTFieldHasAJSONCounterpart` holds every AST field
  to a mirror field; the corpus compiles identically through it), and
  `ir.Compile` must never mutate what it is given (it used to expand
  groups into its input).
- The **unparser** must read back as the same program
  (`unparse.Verify` + `ir.SameProgram`; the studio refuses a save that
  would not).
- A **YAML authoring form** (`.bot.yaml`), if lot 5 justifies it, is a
  **conversion surface only**: read by `validate` and `fmt --to bot`,
  never discovered, never launched, never a second committed truth. One
  truth cannot diverge; two need a consumer matrix nobody maintains.

### 5. One skill, generated from the source of truth

The property registry (lot 1a) is the source the readable grammar, the
EBNF, the JSON Schema and the skill's syntax sections are rendered from,
held to the parser by a bidirectional conformance test; every ```iter
fence in the docs compiles in CI (lot 0). The five hand-written copies of
the syntax that drifted apart are the defect this closes.

## Consequences

- Lot 0.5 (this ADR's first delivery, #1012 #1013 #1015) makes the three
  surfaces agree before any profile or import work builds on them.
- `dsl: 2` and `import` are additive to the catalogue: the 35 shipped bots
  stay v1 until migrated in a reviewed lot of their own, and
  `catalog_parse_compile_test.go` stays the floor at every step.
- A reader of a `.bot` learns its reading from its first line, and an
  agent authoring one is told which build to validate against
  (`requires.iterion`, C138/C040 for a builtin the floor lacks).
- What this ADR refuses: a version that governs engine capabilities
  (that is the manifest's floor), an implicit directory-wide merge, and a
  second committed source of truth in YAML.

## Amendments (lot 2, 2026-09-12)

Facts the implementation verified, recorded so they are not re-litigated:

1. `delegate:` never existed in the parser — no token, no arm; only the
   quickref named it to say "do not write it". Its "removal" is doc-only.
2. `join` was a keyword with no parser rule behind it and no use in the
   corpus. It goes in every profile, not in profile 2 alone: a dead token
   becoming an ordinary identifier changes no valid text's meaning.
3. The migration is a surgical rewrite by spans on the ORIGINAL bytes, not
   parse → unparse: the parser drops the comments inside a block and the
   unparser hoists the top-level ones, so a rewrite through them would
   erase a bot's documentation and reformat every file. The oracle is the
   span-free JSON mirror of the AST (never vacuous, unlike a comparison of
   two compilations with no workflow), plus the file's frontmatter read on
   its original bytes; the paragraph breaks profile 2 keeps are reported
   by prompt, and refused on request, since the text is identical and the
   meaning is not.
4. The command is `iterion dsl migrate`, beside `dsl spec`: `iterion
   migrate` is the hidden operator root (to-cloud, run-paths, orgs), and
   moving a `.bot` is an author's gesture.
5. Tool-name aliases (`Read` → `read_file`) leave the lot: the tool registry
   already resolves a bare name against a unique MCP tool suffix, so an
   alias after an exact-match miss changes the meaning of a valid profile-1
   file, and an alias resolved at runtime does not travel in the AST an
   older runner compiles. A follow-up ticket carries both constraints.
6. The engine floor a profile-2 bundle declares is the VERSION OF THE BUILD
   that wrote it — the migrating binary, the scaffolding binary — never a
   release number guessed in code: that build reads the profile, so every
   later one does. `iterion validate` asks for the floor where it is
   missing (C252) and the push admission refuses without it; a headerless
   file that profile 2 would read otherwise is told so (C144), while a
   headerless file both profiles read alike draws nothing.
7. An inline prompt is named after its body (`_inline_<hash>`), not after
   the node and property: stable under a node's rename, collision-resolved (a second body under the same twelve-digit prefix takes a longer one)
   across groups, and shared by two references to the same text.
