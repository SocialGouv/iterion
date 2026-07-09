# ADR-063: Post-compaction todo-reseed nudge

- **Status**: Accepted
- **Date**: 2026-07-07
- **Authors**: Adry
- **Code**: [pkg/backend/model/system_reminder.go](../../pkg/backend/model/system_reminder.go) (`todoReseedMessage`, `hasTodoTool`), [pkg/backend/model/generation.go](../../pkg/backend/model/generation.go) (both compaction sites)

## Context

The claw path compacts conversation history when it approaches the
context-window threshold, or reactively when a force-compact is
triggered. Compaction replaces older turns with a summary. The agent's
on-disk todo list survives compaction untouched — but the model's
**view** of it does not: the tool-call turns that read and wrote the
todo list are exactly the kind of history the summary squeezes out.

The observed failure is a tool-equipped agent drifting after a
compaction — losing track of which steps remain, or re-asking questions
the operator already answered — because the summary reconstructed the
narrative but not the live task-list state.

## Decision

After any claw compaction, iterion injects a synthetic **user** turn
([`todoReseedMessage`](../../pkg/backend/model/system_reminder.go)) that
tells the agent the history was just compacted, to re-read its todo list
(`todo_write` with action `"read"`), reconcile it against the summary,
and continue directly without re-asking answered questions. The message
is wrapped in the `<system-reminder>` envelope (see ADR-062).

The nudge is gated by
[`hasTodoTool`](../../pkg/backend/model/system_reminder.go): only agents
that actually expose the `todo_write` tool receive it, so tool-less
judges are not fed irrelevant noise. It is wired at **both** compaction
sites in [generation.go](../../pkg/backend/model/generation.go) (the
threshold-triggered and the reactive force-compact paths).

## Trade-offs

The reseed re-anchors the agent's task view for the price of one extra
injected turn plus one `todo_write` read call after each compaction —
cheap, and only on the tool-equipped path. The honest concession: the
nudge is a behavioural prompt, not a structural guarantee — a model that
ignores the reminder still drifts; iterion is relying on the agent
honouring the instruction rather than restoring the view mechanically.

## Alternatives considered

### 1. Embed the live todo list into the compaction summary

Have the compactor splice the current todo list verbatim into the
summary it produces.

**Rejected because**: the list already survives on disk, and re-reading
it through the existing `todo_write` tool is cheaper and more reliable
than reconstructing view-state inside every summary. It also keeps
compaction payload-agnostic — the compactor stays a generic
history-squeezer with no special knowledge of the todo tool's shape.

### 2. Persist and restore the model's todo view across compaction

Make the compactor treat the todo tool-state as a first-class,
compaction-survivable block that is carried through verbatim.

**Rejected because**: that is a larger change to the compaction data
model (structured tool-state blocks that compaction must recognise and
preserve). The reseed nudge achieves the same practical outcome — the
agent operating from an accurate task list — with a one-message injection
and no compaction-format change.

## Consequences

- **Agents re-anchor instead of drifting.** After compaction a
  tool-equipped agent re-reads and reconciles its todo list before
  continuing, cutting post-compaction drift and duplicate questions.
- **Tool-less judges are unaffected.** `hasTodoTool` gates the nudge, so
  judges and other tool-free generations see no extra turn.
- **Both compaction triggers are covered.** Threshold and reactive
  force-compact both inject the reseed, so no compaction path leaves the
  agent unanchored.
- **It is a nudge, not a mechanism.** The reseed depends on the agent
  honouring the instruction; it does not mechanically restore tool-state.
- **Re-challenge**: if compaction gains the ability to preserve
  tool-state blocks, or the todo tool becomes a first-class
  compaction-survivable structure, the reseed nudge becomes redundant.
