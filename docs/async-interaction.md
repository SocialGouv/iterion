# Async human interaction — ask, keep working, sync later

ADR: [081-async-human-interaction](adr/081-async-human-interaction.md)

An `interaction: async` agent can pose questions to the operator
**without stopping its work** — like a Claude Code session where you
answer whenever you're ready. The answer is delivered into the agent's
message queue as soon as the operator replies; the run only blocks at a
**sync point**, deterministic (the `await_answers` node) or at the
LLM's discretion (the `await_answers` tool).

## DSL

```iter
agent draft:
  interaction: async        # grants ask_user_async + await_answers (+ blocking ask_user)
  system: draft_sys
  output: draft_out

await_answers gate:          # deterministic sync point
  from: draft                # optional: only this node's questions ("" = whole run)
  timeout: "30m"             # mandatory — the no-silent-infinity invariant

workflow w:
  entry: draft
  draft -> gate
  gate -> finalize
```

`interaction: async` is valid on agent/judge nodes only (C240 on a
human node — a human node IS the blocking question). `await_answers`
requires a positive `timeout:` (C241); a `from:` naming a missing or
non-async node warns C242 (the await could only ever time out).

Runnable demo: [examples/async-questions/main.bot](../examples/async-questions/main.bot).

## The tools (identical on claw and claude_code)

- **`ask_user_async(question, options?, allow_free_text?)`** — persists
  a pending interaction (`Kind: "async"`, `interactions/<id>.json`),
  emits `human_input_requested{async:true}`, and returns immediately
  ("question posted; keep working"). Never cancels the stream.
- **`await_answers()`** — all posted questions answered → returns the
  collected answers inline. Any pending → the run pauses
  (`paused_waiting_human`, interaction `Kind: "await"` listing the
  pending IDs); answering the last question auto-resumes, and the
  agent's paused tool_use receives the aggregated answers.
- **`ask_user`** — the blocking variant stays available for hard stops
  (destructive/irreversible decisions).

The system prompt of an async node carries an `[ASYNC QUESTIONS]`
protocol section: front-load questions, keep working, sync only when
truly blocked.

## Answering

While the run is **running or paused**:

- **Studio** — the run conversation shows a non-blocking question card
  (the run keeps executing); answering posts to the API below.
- **REST** — `POST /api/runs/{id}/interactions/{iid}/answer`
  `{"answer": "…"}` → `{queued, resumed}`;
  `GET /api/runs/{id}/interactions/pending` lists pending questions.
- **CLI** — `iterion runs questions <run-id>` then
  `iterion runs answer <run-id> <interaction-id> "<answer>"` (direct
  store access — works cross-process against a live `iterion run`).

Delivery rides the existing operator-message queue, **node-scoped** to
the asking node: claw injects between tool iterations (plus a final
end-of-turn drain so a late answer forces one more turn instead of
being lost), claude_code via its PostToolUse/Stop hooks. The message
shape is `[Answer to question <id>] Q: "…" — A: …`.

## Semantics & guarantees

- **Parallel branches are never frozen** by pending questions. Inside a
  fan-out branch, an `await_answers` node parks only its branch
  (releasing its semaphore slot, like `wait`); the `await_answers`
  TOOL called from inside a branch returns an explicit error (a pause
  is run-global by construction — put the sync point in the graph).
- **Level-triggered, store-backed**: the await predicate is "no pending
  `Kind: async` interaction in scope", re-checked on an in-process
  doorbell (immediate) and a 5s poll (cross-process answers). Answers
  that arrived while the process was down are honoured on resume.
- **Bounded**: the node's `timeout:` fails the branch with an explicit
  list of unanswered questions; the tool escalation rides the normal
  pause/resume machinery (no idle CLI session, survives restarts).
- **Node output**: `{answers: [{interaction_id, node, question,
  answer}, …]}` — reference it as `{{outputs.<gate>.answers}}`.
- **Events**: `human_input_requested` with `data.async=true` on post;
  `interaction_answered` (with the answer text) on reply.
- Answering the same question twice is a 409 conflict
  (`ErrInteractionAlreadyAnswered`) — never a silent overwrite.

## Limits (v1)

- Sandboxed claude_code nodes: the async tools are unavailable (same
  stdio-MCP transport gap as the blocking `ask_user`); a loud per-node
  warning is logged.
- An in-flight `await_answers` node keeps the run status `running`
  (in-process park bounded by `timeout:`); the durable
  `paused_waiting_event`-style parking is deferred (see ADR-051/081).
