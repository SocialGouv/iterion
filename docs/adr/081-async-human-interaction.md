# ADR-081 — Async human interaction: ask, keep working, sync later

Date: 2026-07-21
Status: accepted

## Context

Every human question in iterion was **blocking and run-global**: the
`ask_user` tool cancels the agent's stream (claude_code PreToolUse hook /
claw `ErrAskUser`) and the run pauses `paused_waiting_human`; a pause-mode
`human` node exists only on the trunk and freezes everything. Claude Code's
UX is better: the agent poses a question, **keeps working**, and receives
the answer whenever the human replies — blocking only when it genuinely
cannot proceed.

All the asynchronous transport already existed:
- `store.Interaction` records are persisted per run (pending =
  `AnsweredAt == nil`);
- `store.QueuedUserMessage` is a node-scopable, durable message queue with
  a cross-process doorbell, delivered mid-run WITHOUT pausing on both
  backends (claude_code PostToolUse/Stop inbox-drain hooks; claw drain
  between tool iterations);
- pause/resume can re-enter the exact backend session by answering a
  pending tool_use (`ResumePendingToolUseID` / `ResumeAnswer`);
- ADR-051 `wait` nodes park ONE branch (semaphore slot released) under a
  mandatory timeout.

What was missing: a non-blocking ask, the decoupling of "question created"
from "run paused", and the sync points.

## Decision

**DSL surface.** `interaction: async` (new `types.InteractionAsync`) on
agent/judge nodes grants `ask_user` (still available for a hard block)
plus two new tools on BOTH backends with identical semantics:

- **`ask_user_async`** — writes a pending `Interaction` (`Kind: "async"`),
  emits `human_input_requested{async:true}`, and returns immediately
  ("question posted; the answer will arrive in your message queue; keep
  working"). Never cancels the stream.
- **`await_answers`** (tool — the LLM-discretion sync point) — if all
  posted questions are answered, returns the collected answers inline; if
  any is pending, escalates through the EXISTING pause machinery
  (`ErrNeedsInteraction` → `paused_waiting_human`) with a synthetic
  `Kind: "await"` interaction referencing the pending IDs. Resume answers
  the await tool_use via `ResumePendingToolUseID` and fans the answers out
  onto the original async interaction records.

**Answer delivery.** Answering a pending async interaction (REST
`POST /api/runs/{id}/interactions/{iid}/answer`, CLI, studio card — valid
while the run is `running` OR paused) sets `Answers`+`AnsweredAt`, then
queues a **node-scoped** `QueuedUserMessage` (`[Answer to question <iid>]
Q: … — A: …`) delivered by the existing inbox drains at the next turn
boundary. When the run is paused on an `await` interaction whose refs all
become answered, the service auto-resumes.

**Deterministic sync point.** A new special node kind **`await_answers`**
(sibling of `emit`/`wait`):

```iter
await_answers gate:
  from: gatherer      ## optional: only questions posted by this node
  timeout: "30m"      ## mandatory (no-silent-infinity invariant)
```

Level-triggered predicate "no pending `Kind: async` interaction for
`from:` (or the whole run)", re-checked on an in-run answers doorbell and
a 5s store-poll ticker. Inside a fan-out branch it parks ONLY that branch
(the WaitNode slot-release pattern). Output:
`{answers: [{interaction_id, node, question, answer}, …]}`.

**Parallel branches.** The ask never blocks, so siblings advance by
construction. The `await_answers` TOOL called from inside a fan-out branch
returns an explicit tool error (a run-global pause from a branch would
unwind the run); branch-level sync is the `await_answers` NODE's job.

**No checkpoint change.** Pending questions are rediscovered from the
interaction store (the durable source of truth); an await-tool pause
reuses the existing single-`InteractionID` checkpoint (the await
interaction IS the pause point). **Zero claw-repo changes** — the
generation loop lives in iterion; the only addition is a final inbox
drain before accepting a no-tool final answer (the claw analog of
claude_code's Stop hook).

**Events.** `human_input_requested` gains `data.async=true`; new
`interaction_answered` event type.

**Diagnostics.** C240 (error: `interaction: async` on a human node),
C241 (error: `await_answers` without `timeout:`), C242 (warning: `from:`
references a missing/non-async node). C240-band because C200–C234 are
claimed by pkg/bundlelint.

## Rejected alternatives

- **`wait: true` param on one tool** — forks hook logic on input
  inspection and muddies the always-blocking `ask_user` contract.
- **A `capabilities:` entry instead of `interaction: async`** — the
  capabilities registry is host-side (board.*) semantics; question-asking
  is interaction semantics, already the seam that grants `ask_user`.
- **Pending-set in the Checkpoint** — redundant with the already-durable
  interaction files.
- **In-process blocking inside the await tool** — invisible status, dies
  with the process, and a blocking claude_code hook stalls the CLI's hook
  RPC.
- **Reusing ADR-051 `wait` + an `interaction.answered` emit** — the
  run-events registry is edge-triggered and latching: the first answer
  would satisfy the wait forever; "ALL pending answered" is
  level-triggered.
- **Relaxing `/answer-human` to running runs** — it is a resume
  primitive; answering an async question must not imply resume.

## Deferred

- Durable cross-process parking for the await node
  (`paused_waiting_event`, ADR-051's own deferral) — mandatory timeout +
  resume-time re-evaluation make in-process v1 safe.
- Tool-level blocking await inside a branch (covered by the node; the
  tool errors loudly).
- Async tools under sandboxed claude_code via the HTTP MCP transport
  (pre-existing `ask_user` sandbox gap; loud warning in v1).

## Consequences

- Agents on either backend can front-load questions and keep producing;
  operators answer on their own schedule from the studio while the run
  is live.
- Convergence discipline is preserved: every sync point is bounded (tool
  escalation rides the existing pause; the node has a mandatory timeout).
- The `paused_waiting_human` status now also covers await-tool pauses;
  the interaction's `Kind` disambiguates for UI/tooling.
