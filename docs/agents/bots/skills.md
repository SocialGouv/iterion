# Skills live with their bundle

Read it when editing a bundle's `skills/` — the mirror into `.claude/skills`, naming, and the sharing rule.

## Skills (Claude Code SKILL.md) live with their bundle

Claude Code-style skills ship inside the `.botz` bundle they
support, not at a repo-global location. Iterion's runtime mirrors
`<bundle>/skills/*.md` into `<workspace>/.claude/skills/` at run
start (and on each resume), regardless of backend. Three backends
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
  `session-continuity` (iterion workspace memory — `memory_read` /
  `memory_write` / `memory_list` for the cross-run knowledge tree under
  `~/.iterion/projects/<key>/memory/<scope>/`), and `dogfood-cycle`
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
  workspace-memory work — see
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
- Added a new DSL primitive or changed edge syntax → update
  `iterion-dsl-quickref`.
- Discovered a better survey heuristic → fold it into `repo-survey`.

When adding a new skill, place it under the bundle's `skills/`
directory with the standard frontmatter (`name`, `description`)
plus an imperative-voice body grounded in real files. Skills must
be self-contained: a reader who lands on one should not have to
chase context across the repo.

If a skill ends up duplicated across multiple bundles, accept the
duplication for now (iterion has no skill-sharing primitive yet)
and add a TODO comment in each copy pointing to its peers.

