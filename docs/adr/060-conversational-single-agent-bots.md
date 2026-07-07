# ADR-060 — Conversational single-agent bots: the chat loop pattern

Status: **accepted** (2026-07-07). Extends ADR-058 (minimal framing — lean on
the agent) from code-writing campaign bots to **interactive** bots. Piloted by
rewriting `whats-next` (Nexie) v1 (form state machine, 15 nodes / 21 edges /
4 bespoke human forms + hardcoded studio form resolvers) → v2 (chat loop,
5 nodes). First live session: run `019f3beb`, 2026-07-07 —
[docs/bot-runs/whats-next.md](../bot-runs/whats-next.md).

## Context

whats-next v1 modelled the operator dialogue as a state machine: every
exchange was a typed human-node form (priorities radio, roadmap review
checkboxes, dispatch pickers, an `ask_continue` radio of five preset actions
+ a free-text `detail` field), each answer routed through compute/router
nodes into single-purpose agent nodes (`triage_board`, `load_dispatch_candidates`,
`assign_to_bots`…), and the studio carried a parallel per-node form registry
(`resolveDynamicForm`, QuickMode intent classifier) to make the forms bearable.

The 2026-07-07 dogfood run (`019f3b6b`) showed the failure squarely. The
operator asked, in chat, *"indique-moi les tickets les plus pertinents pour un
quickwin"*. The bot could only answer with a 13-item raw checkbox list (no
analysis), forced the question into a preset radio's `detail` field, burned a
full state-machine cycle (~40 s) per utterance, alternated reply languages,
and — because each `triage_board` invocation was a fresh LLM session — carried
no conversational memory between turns. The operator abandoned the session.

The root cause mirrors ADR-058's: the deficit was framing, not capability.
A dialogue is the *least* decomposable flow there is — every node boundary
inserted between two utterances destroys context, adds latency, and replaces
language with forms. Meanwhile the engine already had every primitive a real
conversation needs, each built for other purposes: `ask_user` pauses with
same-session resume (`claude --resume <id>`; interaction depth resets on every
human answer, so a conversation is unbounded), a budget that suspends entirely
during human pauses (a session can idle for days at zero cost), the operator
chatbox inbox drained into the running agent at every tool boundary, and the
board MCP tools.

## Decision

Design interactive/orchestrator bots as **one conversational agent in a chat
loop**, with the engine as the *interaction substrate* rather than the
dialogue script:

```
seed (compute) → nexie (agent) → gate (compute) ── close ──▶ done
                    ▲                 │ (default)
                    └── loop(N) ── chat (human, ONE free-text field)
```

- **One agent, one persistent session.** The loop edge maps
  `_session_id`/`_session_fingerprint` from the agent's previous output, so
  every turn resumes the SAME backend session — conversational memory without
  re-shipping context through prompts. (Load-bearing and easy to lose: session
  continuity resolves ONLY from the input map; claw's `session: inherit`
  eviction on success made v1's inherit a silent no-op.)
- **The `chat` human node is the standby home base.** One free-text field; the
  node's `instructions:` renders the agent's `reply` verbatim, so the pause IS
  the chat bubble + composer. Budget is suspended while paused; the run stays
  reachable indefinitely. The ONLY terminal path is an explicit operator
  close (`close: true` in the turn output, projected by the gate compute).
- **Mid-turn questions are `ask_user`** — now with structured `options`
  (clickable in the studio) — reserved for real decisions and the
  bulk-action confirmation ritual. Everything that can wait for the next
  message belongs in `reply`.
- **The dialogue contract lives in the prompt, not the graph**:
  recommendation-first (never a raw dump — analyse, shortlist ≤3, name a
  pick), operator-language mirroring, act-directly on targeted explicit
  instructions, dry-run + `ask_user` confirmation on bulk (≥3) or destructive
  mutations, evidence-grounded curation (verify an issue against `git log` +
  code before calling it obsolete), cross-session memory via a
  `CONTEXT_BRIEF.md` in the workspace memory tree.
- **The engine provides what it uniquely adds**: the pause/resume substrate,
  session-resume plumbing, the structured turn envelope
  (`{reply, close, quick_replies, dispatched_ids}` — the last feeding the
  server-side watched-issues stamp), the loop bound (`conversation_loop(N)`,
  persisted across resumes) as the lifetime backstop, and per-burst budget
  caps. Nothing else.
- **The studio renders conversation, not forms**: one always-on composer
  that answers the pending pause / queues into the running agent / re-seeds a
  fresh session; `assistant_text` narration events as the agent's speech
  bubbles while it works; `quick_replies` and `ask_user` options as chips.
  No per-bot form registry.

Generic engine/studio work shipped with the pilot (benefits every bot):
`ask_user` structured options across both backends, `assistant_text`
narration events, ask_user pauses on agent nodes surfacing as chat turns,
and the pending-input nav badge.

## Consequences

- whats-next v2: 15 nodes → 5; ~1 740 → ~350 DSL lines; studio −~3 000 lines
  of bot-specific form machinery. First live session replayed the failed
  scenario successfully in 2 turns (~1 m 40 / ~$0.40 per turn), including an
  unprompted obsolescence detection with commit-level evidence and a guarded
  traced close.
- A turn is only as good as the contract prompt — the graph no longer
  enforces sequencing. The counterweights: the structured turn envelope
  (schema-validated), the deterministic gate on `close`, the loop bound, the
  bulk-confirmation ritual, and golden replay
  (`pkg/botreplay` `nexie_turn_basic`, recorded live).
- Standby sessions accumulate: runs stay `paused_waiting_human` by design.
  The nav badge + the runs list make them visible; `close` archives.
- Rejected alternatives: (a) keeping the form state machine and improving
  the forms — v1 had already been through two UX rounds (QuickMode intent
  classifier, dynamic checkbox lifting) and each round added studio coupling
  without making the bot conversational; (b) driving the dialogue through
  `interaction: llm_or_human` auto-answering — wrong tool: the operator IS
  the counterpart, auto-answering them defeats the purpose; (c) a
  supervisor-style side channel (ADR-051/supervisors) — steers a working
  agent, but cannot BE the primary dialogue surface.
- Applicability: orchestrator/assistant bots whose primary artifact is the
  dialogue + board/state mutations (Nexie, future Evoly-style strategy
  sessions, review companions). Code-writing campaign bots stay on ADR-058's
  shape (mission + standing autonomy, no chat loop); the two patterns share
  the "lean on the agent" doctrine and differ only in what loops: work units
  there, conversation turns here.
