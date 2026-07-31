# Session board (per-run Tasks tab)

The **Session board** is a per-run view in the studio run console — the
**Tasks** tab — that gives continuous, human-readable feedback on a work
session, distinct from the technical run view (event log, raw logs,
execution graph, cost meters, artifacts). It has two layers:

1. **Task-list board (Phase 1, deterministic).** The Tasks tab is always
   present; when an agent emits Claude Code's `TodoWrite` or claw's
   `todo_write`, its evolving list is shown in plain language and kept after
   the run finishes.
2. **Curated widgets (Phase 2, opt-in, LLM).** A cheap supervisor-style
   agent watches the run and adds a few session-specific widgets
   (narrative note, milestones, a small chart) that the run view does not
   already show.

## Layer 1 — the task-list board (deterministic)

The tab costs nothing and needs no configuration, but its task-list data is
backend-dependent.

- **Source.** The studio reducer recognises task lists on `tool_started`
  events (`data.input.todos[] = {content, status, activeForm}`) from exactly
  `TodoWrite` (Claude Code) and `todo_write` (claw). For claw, iterion
  auto-includes `todo_write` for tool-restricted agent nodes and the
  `agenticOperatingPosture` nudges the agent to maintain a list. Pi deliberately
  has no todo tool, and Kimi, Grok, and legacy Codex do not emit either
  recognised tool name; those runs keep the Tasks tab but may have no task-list
  snapshots.
- **History.** The studio run store keeps an ordered, de-duplicated
  **history** of task-list snapshots per execution (`todoHistoryByExec`),
  never cleared on `node_finished` / run termination (unlike the live Logs
  side panel's `latestTodosByExec`). A finished run reconstructs the board
  by folding the persisted `/events` log.
- **UI.** `studio/src/components/Runs/sessionboard/SessionBoardTab.tsx`
  renders **Now** (the current node's task list — the `in_progress` item
  in natural language + a done/total progress bar) and **Earlier this
  run** (collapsible history per node). Built from the existing `ui/`
  primitives; the checklist rendering + status vocab are shared with
  `LogSidePanel` via `todoStatus.ts` + `todoChecklist.tsx`.

## Layer 2 — curated widgets (LLM, opt-in)

Off by default. Enable per server/run with:

```sh
ITERION_SESSION_BOARD=on            # turn the curation layer on
ITERION_DEFAULT_SESSIONBOARD_MODEL=anthropic/claude-haiku-4-5  # optional pin
```

When enabled, a `sessionboard.Coordinator` (cloned from
[pkg/supervise](../pkg/supervise/coordinator.go)) is spawned for the run's
lifetime alongside the declared supervisors. It watches the event stream,
wakes on debounced turn boundaries (cooldown floor, hard `MaxEvals`
budget), and asks a cheap model via `GenerateObjectDirect` for a
**`BoardDecision`** — widget **diffs** (upsert / remove), not a full
redraw, so the board converges instead of thrashing. The system prompt
forbids duplicating the run view and biases toward stability.

- **Widgets.** A fixed registry: `note`, `metric`, `checklist`,
  `progress`, `bar_chart`. The agent emits a declarative spec against this
  registry — never code. The studio renders one card per widget
  (`sessionboard/widgets.tsx`, lazy-loaded so Recharts stays out of the
  main bundle) and ignores unknown kinds (forward-compat).
- **Persistence.** The spec is saved to `runs/<id>/sessionboard.json`
  (versioned) by `sessionboard.FileStore` and served at
  `GET /api/runs/{id}/session-board`. The studio fetches it only when
  `server_info.session_board_enabled` is true, refetching (debounced) as
  the run's event stream advances — so a curation-off deployment never
  polls.

Key files: [pkg/sessionboard/](../pkg/sessionboard/) (`spec.go` widget
model + diff applier, `store.go` FileStore, `bot.go` LLM evaluator,
`coordinator.go` watcher, `env.go` gate); the runview spawn +
emitter live in [pkg/runview/service_observe.go](../pkg/runview/service_observe.go).

## Planned follow-ons

- **Per-bot DSL opt-in.** A `session_board:` block (model / cooldown) so
  curation is opt-in per workflow, not only via the env gate.
- **Push instead of poll.** A `sessionboard_updated` event so the studio
  reacts to spec changes without the lastSeq-driven refetch.
- **Cloud mode.** A Mongo-backed `sessionboard.Store` (the FileStore needs
  a host store dir, so curation is local-only today).
