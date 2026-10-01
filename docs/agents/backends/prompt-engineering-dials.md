# Cursors, supervisors, ultracode

Read it when authoring `cursor:` / `supervisor:` blocks or
`reasoning_effort: ultracode`; [docs/supervisors.md](../../supervisors.md)
and [docs/ultracode.md](../../ultracode.md) are the references.

### Cursors (prompt-engineering dials)

`cursor <name>:` is a top-level declaration alongside `prompt:` /
`schema:`. Each cursor defines either an enum (`values:`) or a
numeric band map (`bands:`) over `[0.0, 1.0]`, with each entry
carrying a prompt fragment. Agent/judge nodes activate cursors via
a `cursors:` block (`enabled` toggle + per-cursor settings), and
the runtime appends the resolved fragments under a `## Calibration`
section in the system prompt. Diagnostics: `C083` (unknown cursor
reference, warning), `C084` (invalid value, error), `C085`
(malformed declaration, error), `C086` (duplicate name, error).
Resolution honours `${VAR}` substitution; the assembled prompt is
sorted alphabetically by cursor name for prompt-cache stability.

Cursors are framing dials, **not gates**. See
[docs/cursors.md](../../cursors.md) for the full contract — Goodhart
resistance still lives in judges, scanners, and deterministic
coverage gates. Reference catalogue:
[examples/cursors/cursors.bot](../../../examples/cursors/cursors.bot)
ships `ambition` / `depth` / `rigor` / `autonomy`.

### Supervisors (`supervisor <name>:`)

A **supervisor** is an LLM agent that watches another running agent and
enqueues steering messages the supervised agent picks up **at its next
turn** — like a human watching a Claude Code session and typing a
correction. It is **node-scoped**: it watches one or more *agent nodes*
(`watches: [implement]`), is armed only while a watched node is active,
and its injected messages are node-tagged
(`store.QueuedUserMessage.NodeID` + `WithMessageNode`) so a late message
can't leak into the next node. It is a top-level **declaration**, not a
graph node — the engine spawns a concurrent
[pkg/supervise](../../../pkg/supervise/coordinator.go) `Coordinator` (shaped like
`watch_coordinator`) at run start and tears it down when the run ends.
The coordinator wakes the bot on debounced turn boundaries (cooldown) and
on **monitor** matches (event patterns the bot registers — Bash failure,
edit to a path, cost threshold); the bot returns a structured `Decision`
(intervene/message/watch/done) via `GenerateObjectDirect`. Injection
reuses `runview.Service.QueueMessage`, so steering shows in the studio
conversation and is delivered by the same inbox-drain hooks as operator
chat. Three surfaces: the in-`.bot` `supervisor <name>:` block (primary,
auto-spawned; diagnostics C190–C193), `iterion supervise --run-id --node
--system --monitor` (attach to an already-running iterion run), and
`iterion supervise --claude-session <cwd>` (attach to a **raw** `claude`
CLI/VSCode session — iterion tails its
`~/.claude/projects/<key>/<sessionId>.jsonl` transcript and steers via a
`Stop`/`PostToolUse` hook, installed by `iterion supervise install-hook`,
that runs the hidden `iterion __claude-hook-drain` to inject from an
inbox under `~/.iterion/claude-sessions/<key>/`). The transcript tailer
is an `Observer` and the inbox an `Injector`, so the same Coordinator/bot
drive both managed and raw targets. The block may pre-seed `monitors:`
(CLI `--monitor` grammar, armed from the first event — the bot-registered
kind only exists after its first eval). Declared supervisors spawn by
default on every launch surface (CLI run/resume, studio/runview, the
dispatcher's direct engine path, cloud runner pods), with the usual
escape hatch: run-level `--supervisors on|off` / launch-API field →
`ITERION_SUPERVISORS` → on (skip always logged; the resolution lives in
`pkg/supervise`, shared by every spawn site) — and like `auto_memory:`
the run-level override travels onto the cloud queue
(`RunMessage.supervisors`, schema v8) so a pod never re-decides an
operator's `off`. The supervisor hub rides BOTH event seams (engine
observer + backend-hook `ExecutorSpec.EventObservers`) — hook events
(`assistant_text`, `tool_*`) never fire the engine seam, and text
monitors are blind without the second wire. Persy (perseverance coach)
is the shipped use — carried by all seven campaign bots on their
`campaign` node, feature-dev's being the reference and
`bots/campaign_supervisor_test.go` the fleet guard. Reference:
[docs/supervisors.md](../../supervisors.md),
[examples/supervisor/sample.bot](../../../examples/supervisor/sample.bot).

### Ultracode (`reasoning_effort: ultracode`)

`ultracode` is the top of the `reasoning_effort` dial
(`none|low|medium|high|xhigh|max|ultracode`) but is a **mode, not a wire
value**: Anthropic's API only accepts up to `xhigh`/`max`. It means
**xhigh + a standing prerogative to orchestrate multi-agent
workflows**, delivered via a `## Workflow Orchestration` system-prompt
section, and is **reliable only on Opus 4.8 and the Claude 5 family**
(`claude-opus-5`, Fable 5.1 — the orchestration half rides Anthropic
mid-conversation system messages). The runtime remaps `ultracode` to
`xhigh` on the wire
([model.wireEffort](../../../pkg/backend/model/effort.go)), makes the `agent`
subagent tool available, and emits diagnostic **C089** (warning) when
the node's model is neither — degrading to plain `xhigh`. Adaptive
thinking rides the claw backend. The studio effort picker offers
`ultracode` accordingly. Full contract:
[docs/ultracode.md](../../ultracode.md).

