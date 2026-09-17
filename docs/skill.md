# AI Agent Skills

Iterion ships **two Agent Skills**, compatible with Claude Code, Codex, Cursor, Windsurf, GitHub Copilot, Cline, Aider, and other AI coding agents. Install the one you need — they are independent.

| Skill | Teaches your agent to |
|---|---|
| [`iterion-dsl`](#iterion-dsl--author-workflows) | Author, review and debug `.bot` workflows in the current DSL |
| [`adversarial-review-loop`](#adversarial-review-loop--ship-a-change-through-a-loop) | Break its own change before a reviewer does — repo-agnostic, no iterion required |

## `iterion-dsl` — author workflows

```bash
npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl
```

The skill teaches an agent to *author* workflows; its operating
counterpart is the [MCP server](mcp-server.md) (`iterion mcp`), which
gives the same agent typed tools to *run* Iterion — launch and follow
runs, answer questions, groom the board, and drive a cloud instance.
Install both for the full loop: write a bot, then operate it without
leaving the agent.

| File | Content |
|------|---------|
| [`SKILL.md`](../SKILL.md) | Complete DSL reference — node types, properties, edge syntax, templates, budget, MCP |
| [`SKILL-run-and-refine.md`](../SKILL-run-and-refine.md) | Practice guide for running, debugging and iteratively refining `.bot` workflows against real data |
| [`references/dsl-grammar.md`](references/dsl-grammar.md) | Formal grammar specification (EBNF) |
| [`references/patterns.md`](references/patterns.md) | 10 reusable workflow patterns with annotated snippets |
| [`references/diagnostics.md`](references/diagnostics.md) | Authoritative sparse catalogue of DSL diagnostics (C001–C199 plus async C240–C242) and bundle checks (C200–C234), with causes and fixes |

Once installed, just ask your agent to write workflows:

- *"Write a .bot workflow that reviews a PR with two parallel reviewers"*
- *"Create an iterion pipeline that fixes CI failures in a loop"*
- *"Add a human approval gate before the deployment step"*

The agent will use the skill reference to produce valid `.bot` files that pass `iterion validate`.

## `adversarial-review-loop` — ship a change through a loop

```bash
npx skills add https://github.com/SocialGouv/iterion --skill adversarial-review-loop --full-depth
```

**`--full-depth` is required here**, and it is not optional politeness: the `skills` CLI stops at a repository's root `SKILL.md` when it finds one, and iterion has one (that is `iterion-dsl`). Without the flag the CLI reports "Found 1 skill" and this one is invisible. Verify either way with `--list`.

A single file — [`skills/adversarial-review-loop/SKILL.md`](../skills/adversarial-review-loop/SKILL.md) — and **nothing in it is iterion-specific**: it works in any repository, with any review gate or none.

It is the practice this project ships its own changes through: a subagent whose posture is to *refute* the diff rather than bless it, a verification pass that judges the subagent as harshly as it judged the code, fixes applied at the class rather than the site, tests proved by mutation, and three named exits for a loop that has stopped converging. It carries a round budget (a ceiling on cost, never a target), the commit trailers that report what a review cost, the upstream plan review by a model of another family, and the journal/retrospective discipline that keeps the protocol honest instead of dogmatic.

How **iterion itself** applies it — where the round is required, the ceiling by change size, who pays a local round versus a gate cycle — is in [docs/agents/adversarial-review-loop.md](agents/adversarial-review-loop.md).

## Installing from iterion itself

`iterion skill import` installs a public skill pack as an enable/disable-able plugin, with no `npx` involved:

```bash
iterion skill import https://github.com/SocialGouv/iterion
iterion plugin enable iterion               # packs install disabled by default
iterion --json plugin info iterion          # lists what it contributes
```

It collects the markdown under the repository's `skills/` directory, so this installs `adversarial-review-loop`. The `iterion-dsl` skill lives at the repository root, next to its `references/`, and travels through `npx skills add` above.

**Do not check the result with `iterion skill list`**: that command reads the *skill library* (`iterion skill add`), a different store, and it answers "No skills in the library" for a perfectly installed pack. A plugin's skills show up under `iterion plugin info`, and in the workspace's `.claude/skills/` once a run starts.

The same command takes any third-party skill repository (`iterion skill import https://github.com/acme/awesome-claude-skills`) or a local directory holding a `skills/` tree. Once enabled, a plugin's skills are mirrored into the workspace's `.claude/skills/` at run start — as flat `<name>.md` files, which `claw` discovers natively and a prompt can always read by path. See [docs/plugins.md](plugins.md) and ADR-079.

For `claude_code`'s own Skill tool, which discovers only the `<name>/SKILL.md` directory form, prefer `npx skills add` above: it installs the directory form directly.
