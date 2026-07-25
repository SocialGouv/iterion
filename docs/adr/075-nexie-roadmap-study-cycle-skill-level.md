# ADR-075 — Nexie v3: the roadmap-study cycle as a skill-level behavior

Status: **accepted** (2026-07-16). Ships with the whats-next 0.3.0
upgrade (skills rewrite + prompt/effort/budget deltas; no graph
change). Extends ADR-060 (conversational single-agent bots) without
modifying it.

## Context

The 2026-07-15 roadmap session demonstrated the target shape for
Nexie (`bots/whats-next`, the conversational co-CTO): the operator
manually led parallel audits (docs/ADR follow-ons, code gaps,
operational state) → memory cross-check → verify-before-claim →
synthesis into named chantiers with now/next/later tiering,
quick-wins and blind spots → grouped arbitrage questions → an epic +
framed tickets with a limited `ready` lot → factory drain observation
→ an evidence-based bilan. Board ticket `native:bbcd96aa` captures
the referential: Nexie should lead this cycle end-to-end instead of
only recommending.

Nexie v2 (ADR-060) is ONE claude_code agent in a 5-node chat loop
(`seed → nexie → gate → chat ⇄`), with all seven `board.*`
capabilities, a schema-validated turn envelope
`{reply, close, quick_replies, dispatched_ids}`, and skills mirrored
into `.claude/skills/`. Three of its skills were still v1-shaped
(written for the deleted 15-node form pipeline), and nothing encoded
the study cycle, the framed-ticket discipline, or the live-factory
operating rules.

## Options

**A — skills + prompt, no graph change (chosen).** The cycle becomes
a *named on-demand behavior* carried by rewritten/new skills
(`repo-survey` fan-out protocol, `roadmap-synthesis` chantiers +
framed tickets, `operator-arbitrage` decision blocks, `factory-ops`
live-factory rules) plus small prompt deltas. The audit fan-out rides
the claude_code backend's native Task tool — `reasoning_effort:
ultracode` adds the standing orchestration consent (model default is
already opus-4-8, so diagnostic C089 stays silent). Dispatch stays
board-promotion (`set_bot` + `transition_issue → ready`); monitoring
stays the watched-cards channel (`dispatched_ids` → server-side
auto-subscribe → state-change messages at tool boundaries).

**B — subbot orchestration (rejected).** Modeling the audits as
`subbot` nodes breaks the conversational contract: subbots are
synchronous graph nodes, so the chat parks while they run; parallel
fan-out would force `isolated: true` + lifting `worktree: none` and
`max_parallel_branches: 1`; parent budgets do not bound children.
Every one of those contradicts an ADR-060 invariant pinned by
`e2e/whats_next_loop_test.go`.

**C — engine work first (deferred).** Give in-run agents a
run-monitoring tool (child run status/logs), a first-class
"launch bot now" tool, or dispatcher-control MCP surface. Real gaps,
but none is required for the cycle: board state transitions are
sufficient telemetry for the bilan, and board-promotion is the
sanctioned launch lever. Deferred until a dogfooded need proves them.

## Decision

Option A, with four operator-arbitrated choices:

1. **Full cycle in-run** — audits, synthesis, arbitrage, board
   execution, factory observation and bilan are all Nexie's job; the
   engine gaps of option C stay out of v3.0.
2. **Nexie stays code-read-only** — quick-wins are framed tickets
   routed to worker bots (or listed for the operator), never coded in
   session; `worktree: none` and the read-only-outside-the-board
   guardrail hold.
3. **The cycle is doctrine, not graph** — it fires on roadmap-scale
   asks; a small question keeps its small answer. Skills guide, they
   don't script (the over-framing anti-pattern from ADR-055/058
   applies to prompts too).
4. **Factory limits are operator-owned** — Nexie promotes ≤~5 cards
   per turn, never raises a cost cap, never spawns a second
   server/studio on an active store, and recommends (with the exact
   command) instead of driving the dispatcher.

## Consequences

- The turn envelope is unchanged, so the golden replay
  (`nexie_turn_basic`, plus the new hand-authored
  `nexie_study_synthesis`), the graph-contract e2e, and the studio
  watched-issues stamp all survive as-is.
- The synthesis lives in `reply` markdown and on the board (framed
  tickets: `## Context / ## Done criteria / ## Verify`) — no new
  schema fields, no studio work.
- Per-burst budget grows to 45m / $15 / `tool_max_steps: 80` to fit a
  study turn; ordinary turns use a fraction and the chat pause still
  costs nothing.
- The `horizon:` label namespace is re-canonised on the study tiers
  (`now|next|later`); legacy spellings remain readable equivalents and
  are never mass-relabeled silently.
- Known accepted limits: Nexie cannot read child-run internals (bilan
  evidence = board states + fresh `git log`/greps), and cannot verify
  a dispatcher is running other than by observing that promoted cards
  do not move — both are stated honestly in `factory-ops` and are the
  first candidates for option C follow-ups if dogfooding demands them.
