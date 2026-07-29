# AI Agent Skill

Iterion ships as an **Agent Skill** compatible with Claude Code, Codex, Cursor, Windsurf, GitHub Copilot, Cline, Aider, and other AI coding agents. Once installed, your agent knows the full `.bot` DSL and can write correct workflows for you.

## Install the skill

```bash
npx skills add https://github.com/SocialGouv/iterion --skill iterion-dsl
```

## What the skill provides

| File | Content |
|------|---------|
| [`SKILL.md`](../SKILL.md) | Complete DSL reference — node types, properties, edge syntax, templates, budget, MCP |
| [`SKILL-run-and-refine.md`](../SKILL-run-and-refine.md) | Practice guide for running, debugging and iteratively refining `.bot` workflows against real data |
| [`references/dsl-grammar.md`](references/dsl-grammar.md) | Formal grammar specification (EBNF) |
| [`references/patterns.md`](references/patterns.md) | 10 reusable workflow patterns with annotated snippets |
| [`references/diagnostics.md`](references/diagnostics.md) | Authoritative sparse catalogue of DSL diagnostics (C001–C199) and bundle checks (C200–C234), with causes and fixes |

## Usage

Once installed, just ask your agent to write workflows:

- *"Write a .bot workflow that reviews a PR with two parallel reviewers"*
- *"Create an iterion pipeline that fixes CI failures in a loop"*
- *"Add a human approval gate before the deployment step"*

The agent will use the skill reference to produce valid `.bot` files that pass `iterion validate`.
