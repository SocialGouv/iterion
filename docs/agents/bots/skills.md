# Skills live with their bundle

Read it when editing a bundle's `skills/` — the mirror into `.claude/skills`, naming, and the sharing rule.

## Skills (Claude Code SKILL.md) live with their bundle

Claude Code-style skills ship inside the `.botz` bundle they
support, not at a repo-global location. Iterion's runtime mirrors
`<bundle>/skills/*.md` into `<workspace>/.claude/skills/` at run
start (and on each resume), regardless of backend. A page may equally
ship as the directory form `skills/<name>/SKILL.md`, copied through
unchanged and judged by the same checks (C231-C234, C274). Three backends
consume the same directory: `claude_code` through its native lookup,
`claw` through the `skill` tool (registered by
[pkg/backend/tool/claw_builtins.go:RegisterClawSkill](../../../pkg/backend/tool/claw_builtins.go)),
and pi through the explicit `--skill <workspace>/.claude/skills`
argument. Each bundle therefore gets exactly the skills it ships,
with no implicit dependency on the host
filesystem. The collision policy (workspace wins, with marker-aware
refresh for upgrade cases) is documented in
[docs/bundles.md](../../bundles.md#resource-resolution-at-run-time).

Current bundles and their skills:
- [bots/whats-next/skills/](../../../bots/whats-next/skills) —
  `whats-next` (operating playbook), `iterion-bot-catalog`,
  `iterion-dsl-quickref`, `iterion-board` (reference for the
  capability-gated board MCP tools on claude_code, claw, and pi RPC),
  `iterion-label-vocabulary`, `repo-survey`, `roadmap-synthesis`,
  `session-continuity` (the VARIANT: this bundle has no memory tree, so
  its copy carries the refusal, the board as its durable record and the
  read-only `findings/` inbox — the write contract is rooted in
  `bots/evolve`), and `dogfood-cycle`
  (the operator's measured ritual for validating a bot by a real run).
  Unlisted: `factory-ops`, `operator-arbitrage` — 11 files; the generated
  [map-bots.md](../../references/map-bots.md) is the inventory.
  (the operator's measured ritual for validating a bot by a real
  run — launch visible, monitor actively, fix both bot and engine,
  land + bilan; from the session-mining work behind
  [docs/references/productive-session-patterns.md](../../references/productive-session-patterns.md)).
  Six of the original eight were produced by a dogfood run of claw +
  `openai/gpt-5.5` against this repo; `iterion-board` was added by
  the board-capabilities work and `session-continuity` by the
  workspace-memory work (rooted on `evolve` since iterion#2832) — see
  [scripts/adhoc/whats-next-skills-gen.bot](../../../scripts/adhoc/whats-next-skills-gen.bot)
  for the generator (the seed for a future formalised
  `generate-skills.bot`).
- [bots/copilot/skills/](../../../bots/copilot/skills) — 5 skills, **one per
  posture** plus the playbook and a second design skill:
  `iterion-concepts` (info), `iterion-dsl-authoring` +
  `iterion-bot-architecture` (design), `iterion-run-debug` (debug),
  `copi-conversation` (the operating playbook, loaded every turn).
  The two design skills are deliberately separate because they fail
  differently: `iterion-dsl-authoring` is how to SPELL a `.bot` (the
  syntax traps that compile clean and break at runtime),
  `iterion-bot-architecture` is how to DESIGN one (responsibilities,
  closed contracts, the PASS/RETRY/BLOCKED shape, why a retry re-enters
  its producer rather than a correction twin, subbot boundaries,
  idempotency, budgets, proof categories). Folding them together makes
  a reader treat architecture rules as compiler rules — the one thing
  the design posture must never tell an operator. A repo may override
  the second with its own standard (`authoring_standard:` declaration
  or `ITERION_AUTHORING_STANDARD_PATH`), which the skill tells Copi to
  read and prefer.

**A `skills:` entry that resolves to nothing is silent.** It is not a
compile error and not a bundle-lint finding — the runtime mirrors
nothing, the agent's Skill tool finds nothing, and the bot answers from
the model's priors instead of the authored knowledge (it reads as "the
bot got dumber", not as a typo). `bots/catalog_skill_refs_test.go`
closes that gap: every name in a catalog bot's `skills:` list must exist
as `<bundle>/skills/<name>.md` with matching frontmatter. The exception
is a skill a bot deliberately does NOT ship so the operator attaches it
from the skill library (ADR-059) — `deploy-target` for app-dev /
review-env, where shipping one would pin a catalog bot to a platform.
Those live in a commented allowlist in that test.

**Maintain skills inline with the code they describe.** Each time
you touch a skill's subject area and notice the skill is wrong,
incomplete, or out of date, fix it in the same change — the cost
of a small inline correction is much lower than the cost of an
agent later following stale guidance. Concrete examples:
- Changed a bot's purpose/persona/triggers, or renamed/moved it →
  edit that bot's `manifest.yaml` (`display_name` / `description` /
  `when_to_use` / `triggers` / `enabled`), NOT the catalog skill: the
  generated region of `iterion-bot-catalog.md` is rebuilt from
  manifests. Only the hand-authored `iterion-bot-catalog-static.md`
  preamble (decision tree / distinguishers) is edited by hand; run
  `iterion bots regen-catalog` to refresh the committed generated file.
  Every manifest field the card embeds owes that document one rule: no
  line of it may begin with a fence marker (```` ``` ````, `~~~`), because
  such a line opens a code block that swallows every card below it and the
  owning bot's own ritual with them. `description` and `when_to_use` are
  spliced as markdown BLOCKS, so a word wrap is enough; `name`,
  `display_name`, `category`, `triggers`, `tags` and `capabilities` are
  spliced INSIDE a line, so only a line break in the value puts text at
  column 0. The
  generator answers for both — it escapes an opener a field never closes
  (`internal/mdcode.EscapeDanglingFence`) and collapses a value spliced
  inside a line (`internal/mdcode.Inline`) — and
  `bots.TestBotManifestProseLeavesNoFenceOpen` reddens on a field this
  tree's own reader sees leaving a block open, so a manifest here does not
  lean on either repair. That reader's limits are stated and measured on
  `EscapeDanglingFence`, and the gate shares them: a field of one of those
  shapes passes it.
- Added a new DSL primitive or changed edge syntax → update
  `iterion-dsl-quickref`.
- Discovered a better survey heuristic → fold it into `repo-survey`.

When adding a new skill, place it under the bundle's `skills/`
directory with the standard frontmatter (`name`, `description`)
plus an imperative-voice body grounded in real files. Skills must
be self-contained: a reader who lands on one should not have to
chase context across the repo.

## A skill several bundles carry is DECLARED, not duplicated

A skill file lives with its bundle, so a skill several bundles need exists
as several files. A comment pointing at the peers is a convention, not a
mechanism: the copies drift the first time somebody edits one. Measured
before this was closed — 20 file names shared over 72 copies, 16 of the 20
already diverged, five different spellings of the "keep these in sync"
comment, and exactly one family with a test behind it.

[`internal/skillshare`](../../../internal/skillshare) is the mechanism.
One declaration (`families.go`) names, per shared file name, the bundle
whose copy is the **source**, the bundles whose copies are **mirrors** of
it, and the bundles that carry a deliberate **variant** with the reason it
diverges — the reason lives in the declaration, where one reader sees every
copy's verdict at once, instead of in a comment each copy words
differently.

- Editing a shared skill means editing its **source** and running
  `task skills:sync` (also a step of `task gen`), which writes every
  mirror. The other copies are not yours to edit.
- Adding a skill a second bundle carries means adding its family to
  `families.go`. The gate reddens on a file name several bundles carry that
  no family declares — that is the hole this closes, so a new one cannot
  land quietly.
- A copy that must diverge is declared a variant with its reason, and
  pinned by its own content in
  [`bots/testdata/shared_skills.txt`](../../../bots/testdata/shared_skills.txt).
  No generator rewrites a variant, so the pin is the only thing that makes
  an edit to one visible: re-pin it deliberately with `task skills:bless`,
  whose diff is the review. "Declared variant" must not come to mean
  "exempt forever" — a byte-identity guard laid over drift that already
  happened would freeze it rather than prevent it.

The gate is `go test ./internal/skillshare/` (it rides the required `test`
check) and `task skills:check`. It reddens on a drifted mirror, a variant
that collapsed onto its source, an unreviewed edit to any declared copy, a
declared copy that is gone, and an undeclared shared skill.

Two shapes of divergence are worth knowing because they recur. A copy that
differs only by the `[[bundle]]` line cross-referencing its own playbook is
a shared document with an unshareable self-reference; those stay variants
until the sharing primitive can substitute a per-bundle token — the one
axis [ADR-144](../../adr/144-shared-skills-are-bundle-dependencies.md)
leaves open, because a templated skill is an authoring surface of its own
(seven of the declared variants are that single line and nothing else). A
copy whose generated region a per-bundle generator rebuilds
(`iterion bots regen-catalog`) can never be a byte-mirror.

Sharing a skill BETWEEN bundles is a bundle dependency, not a copy:
`exports.skills` / `dependencies.skills`, resolved at install into the
consumer's own `skills/` — shape, refusals and the pin in
[bundles.md](../../bundles.md#shared-skill-dependencies).

