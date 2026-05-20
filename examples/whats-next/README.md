# whats-next.bot

Orchestrator bot. Given a repository, it:

1. Surveys the code with `claw + openai/gpt-5.5` (read-only agentic
   exploration — bash, glob, grep, read_file).
2. Asks you (free-text human node) about your current priorities and
   blockers.
3. Proposes a structured roadmap — long-term + short-term + one
   immediate next action — where each item is a candidate kanban issue
   with a bot assignee.
4. Loops on your free-text feedback until you mark the proposal
   `approved`. The revise step runs on `claw + openai/gpt-5.5`.
5. **Materialises the approved roadmap as issues on the iterion native
   kanban board** (`<workspace>/.iterion/dispatcher/`) via `iterion
   issue create`. The dispatcher takes over from there — auto-pilot.

The bot does NOT shell out `iterion run …`. The dispatcher dispatches.

## Iterion feature gap (current limitation)

`iterion dispatch` today binds **one workflow per dispatcher instance**
— it does not yet route dispatch by `issue.assignee`. whats-next still
records the assignee on every issue (e.g. `assignee=feature_dev`)
so the data is there for the future routing feature. Until that ships,
the operator either:

- Runs multiple dispatchers (one per assignee, filtering by state /
  label), or
- Waits for the assignee-routing feature.

If you run whats-next.bot on the iterion source repo, it may well
propose **"ship the dispatcher assignee-routing feature"** as its
`next_action` — self-bootstrapping the autopilot.

## Why a mixed backend (claw + claude_code)?

The three exploration/proposal nodes use **`claw + openai/gpt-5.5`** to
dogfood `claw-code-go`'s agentic tool-use loop against OpenAI. The
final `emit_action` node uses **`claude_code`** so the bundled skills
(`skills/*.md`) are mirrored automatically into
`<workspace>/.claude/skills/` and available via Claude Code's native
Skill mechanism when it materialises issues.

## Skills bundled with this bot

The bundle ships **six** SKILL.md files, all under [`skills/`](skills/).
Iterion mirrors them to `<workspace>/.claude/skills/` for the duration
of any `backend: claude_code` node.

| Skill | What it does |
|-------|--------------|
| `whats-next` | operating playbook — 5 phases, principles, anti-patterns |
| `repo-survey` | systematic checklist for the `explore` phase |
| `iterion-bot-catalog` | catalog of iterion bots with assignee-mapping rules |
| `roadmap-synthesis` | how to compose the roadmap (one item per issue) |
| `priority-elicitation` | how to parse the operator's free-text priorities |
| `iterion-dsl-quickref` | DSL quick-ref (loaded only when authoring DSL is on the table — rare) |

The five domain skills were produced by a one-shot dogfood run of
`claw + openai/gpt-5.5` against this repository — see
[scripts/adhoc/whats-next-skills-gen.iter](../../scripts/adhoc/whats-next-skills-gen.iter)
for the generator (the seed for a future `generate-skills.bot`).

## Prerequisites

- `OPENAI_API_KEY` set in `.env` at the repo root (or exported). The
  iterion CLI auto-loads `.env` from `$CWD` upwards.
- `ANTHROPIC_API_KEY` or the Claude Code CLI for the final
  `emit_action` node.
- A writable `.iterion/dispatcher/` under `workspace_dir` (the native
  kanban store auto-initialises on first `iterion issue create`).

## Run

```bash
# From the source directory
devbox run -- iterion run examples/whats-next/main.bot

# Or from the packed bundle
devbox run -- iterion bundle pack examples/whats-next/
devbox run -- iterion run examples/whats-next.botz
```

You can override `workspace_dir` and pass optional `scope_notes`:

```bash
devbox run -- iterion run examples/whats-next/main.bot \
  --var workspace_dir=/path/to/my-repo \
  --var scope_notes="focus on the dispatcher layer, ignore the studio"
```

After the run completes, inspect what landed:

```bash
devbox run -- iterion issue list
devbox run -- iterion issue board       # opens the local board UI
```

## Inputs

| Var | Type | Default | Purpose |
|-----|------|---------|---------|
| `workspace_dir` | string | `${PROJECT_DIR}` | repo to survey + own kanban store |
| `scope_notes` | string | `""` | optional steering hints |

## Outputs

- N kanban issues at `<workspace>/.iterion/dispatcher/issues/`, each
  with `--assignee <bot_name>` (or unassigned + `needs-manual-triage`
  label).
- `<workspace>/.iterion/plans/whats-next-<timestamp>.md` — audit
  markdown listing every created/failed issue.

## Graph

```
explore → ask_priorities → propose_roadmap → carry_roadmap → human_review
                                                                ↓
                                                  approved ──┐  │
                                                                ↓  not approved
                                                          emit_action ← ↩ revise_roadmap
                                                                ↓        (approval_loop(10))
                                                              done
```
