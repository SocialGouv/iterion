# Bmady

A [BMAD-METHOD](https://github.com/bmad-code-org/bmad-method)-inspired
agile delivery bot: a structured **Analyst → PM → Architect → Dev →
QA** pipeline with a human collaboration gate between every phase. You
approve the plan before any code is written, then steer the
implement → QA → sign-off loop.

Bmady is also iterion's reference exercise for the studio's
human-interaction surface — one run hits every form widget.

## The pipeline & its human gates

| Phase | Persona | Human gate | The decision (form widget) |
|-------|---------|-----------|----------------------------|
| Analysis | Mary (Analyst) | `elicit_brief` | Clarify the brief — **free-text** |
| PRD | John (PM) | `review_prd` | approve / expand / add_risks / revise — **radio menu** |
| Architecture | Winston (Architect) | `approve_arch` | approve or reject — **Approve/Reject** |
| Selection | — | `select_stories` | which stories + priority + WIP — **checkbox + select + number** |
| QA | Quinn (QA) | `final_review` | ship / request_changes / hold — **radio menu** |

On **ship**, iterion commits the work (worktree finalisation). On
**request_changes**, it loops back to Dev with your notes. The PRD
and architecture gates have their own bounded revise loops.

## Run it

```bash
# CLI
iterion run bots/bmady/main.bot --var brief="Add CSV export to the report page"

# Studio: open bots/bmady/main.bot → Launch, fill `brief`, pick a preset.
```

Each human gate pauses the run (`paused_waiting_human`); answer it in
the studio form or from the terminal:

```bash
iterion resume --run-id <id> --file bots/bmady/main.bot --answer approved=true
```

Budget is **suspended while paused**, so a multi-day collaborative
session costs nothing between gates.

## Presets

Pick a focus at launch (`presets/`):

- **greenfield** — net-new work; favour clean foundations.
- **brownfield** — existing codebase; minimal blast radius.

## Knobs (env vars)

| Var | Default | Effect |
|-----|---------|--------|
| `BMADY_MODEL_CLAUDE` | `claude-opus-5` | model for Analyst/PM/Architect/Dev |
| `BMADY_MODEL_QA` | `claude-opus-5` | model for QA |
| `BMADY_EFFORT_PLAN` | `high` | reasoning effort for the planning personas |
| `BMADY_EFFORT_DEV` | `high` | reasoning effort for Dev |
| `BMADY_EFFORT_QA` | `medium` | reasoning effort for QA |

For a cheaper/faster run (e.g. a demo), lower the effort vars and/or
point the models at a smaller spec.

## Persona discipline

Each persona reads its own skill at run time (`skills/bmady-*.md`):
the output contract and the discipline live there, not in the graph —
so Bmady stays repo- and stack-agnostic. See `skills/bmady.md` for
the operating playbook.
