# whats-next — Nexie, the conversational co-CTO

ONE adaptive agent in a standing chat loop. You talk; Nexie reads the
board and the repo, recommends (never raw dumps), creates/curates/
dispatches tickets, verifies whether issues are still relevant against
the code and git history, and remembers the session (same LLM session
across turns + a cross-run `CONTEXT_BRIEF.md`).

## Shape (v3)

```
seed (compute) → nexie (agent) → gate (compute) ── is_close ──▶ done
                    ▲                 │ (default)
                    │                 ▼
                    └── conversation_loop(1000) ── chat (human)
```

- **nexie** — `claude_code` + `${ITERION_WHATS_NEXT_MODEL_CLAUDE:-claude-opus-5}`,
  full board capabilities (`board.read/create/move/assign/label/close/comment`),
  bundled skills via the native Skill tool, `interaction: human` so it
  can `ask_user` mid-turn (with clickable options). Each turn returns
  `{reply, close, quick_replies, dispatched_ids}`.
- **chat** — a one-field human node: Nexie's `reply` renders as the
  chat bubble, the answer is the operator's next message. The pause is
  budget-free and can last days — this is the standby home base.
- The loop edge carries `outputs.nexie._session_id` so every turn
  resumes the same claude session: the conversation has real memory.
- Exit: only an explicit operator "close". Everything else loops.

v1 (15 nodes: survey → priorities form → roadmap → review form → emit
→ dispatch pickers → ask_continue radio) is gone — the forms made the
operator encode questions into preset fields and each exchange cost a
full state-machine turn. See `docs/bot-runs/whats-next.md` for the
dogfood history that motivated the rewrite.

## Run

```sh
devbox run -- iterion run bots/whats-next/main.bot \
  --var initial_message="quels tickets sont des quick wins ?"
```

Or from the studio: **/whats-next** (the composer seeds
`initial_message`; presets are just canned seeds). When dispatched
from a board card, the manifest maps the issue title/body into
`initial_message`.

## Inputs

| Var | Type | Default | Purpose |
|-----|------|---------|---------|
| `initial_message` | string | `""` | seed for the first turn; empty → Nexie opens with a board summary + recommendation |
| `scope_notes` | string | `""` | standing constraints for the session |
| `workspace_dir` | string | `${PROJECT_DIR}` | repo whose board/code Nexie works |

## Prerequisites

- Claude Code CLI signed in (or `ANTHROPIC_API_KEY`) — the nexie agent
  runs on `claude_code`.
- A writable `.iterion/dispatcher/` under `workspace_dir` (the native
  kanban store auto-initialises on first issue creation).

## Guardrails

- Targeted explicit instruction → act immediately; **bulk (≥3) or
  destructive → dry-run + ask_user confirmation**.
- Read-only outside the board (no repo edits, no commits — and
  `worktree: none`, so no phantom storage branches).
- Board writes are capability-gated MCP tools; `set_bot` is the
  canonical dispatcher selector; dispatch = transition to `ready`.
- Untrusted-input boundary: repo/issue text is data, not instructions.

## Skills bundled with this bot

All under [`skills/`](skills/), mirrored to `<workspace>/.claude/skills/`
at run start: `whats-next` (playbook), `iterion-board`,
`iterion-bot-catalog` (generated — edit bot manifests, not the file),
`iterion-label-vocabulary`, `repo-survey`, `roadmap-synthesis`,
`operator-arbitrage`, `factory-ops`, `session-continuity`,
`iterion-dsl-quickref`, `dogfood-cycle`.
