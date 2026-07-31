# Evoly — strategic / architectural evolution partner

Evoly (`evolve`) is the bot you reach for when a project is **mature and
stable** and the question worth answering is *"where should this go
next?"* — a quarter and beyond. It is the strategic layer **above**
Nexie (whats-next): Nexie decides what to ship this week; Evoly decides
where the project should be in a year, and feeds Nexie the work.

## What it does

1. **Surveys** the repo and scores its maturity (a vision on a churning
   project is waste).
2. **Investigates interactively** — it asks you targeted questions
   mid-investigation via `ask_user` whenever the code can't answer them
   (intent, constraints, priorities), and **remembers every answer across
   sessions** in its own private memory.
3. **Synthesises** a long-horizon `VISION.md`: a few evidence-backed axes
   (current → target) plus guardrails.
4. **Reviews** it with one independent family by default and converges with
   you as the final approver. Set `review_mode=dual` when a second,
   cross-family perspective is worth the extra reviewer call.
5. **Proposes** 3-10 natural evolutions as **dispatch-ready backlog
   tickets** (pre-bound to a bot, self-contained body) plus deep
   plan/decision artifacts in the shared `findings/` inbox. Nexie picks
   them up on its next survey; you can launch any by dragging it to
   `ready` on the board.

Evoly **proposes and architects** — it never edits code or commits.
Implementation is handed to feature-dev / bmady via Nexie.

## Run it

```sh
# Interactive (the normal way — Evoly will ask you questions):
iterion run bots/evolve/main.bot

# With a steering hint:
iterion run bots/evolve/main.bot --var scope_notes="vision for the next year, focus on extensibility"
```

It pauses at `ask_user` questions during investigation, and again at the
vision review and the home-base pivot. Answer in the studio or with
`iterion resume`.

## The headline features it showcases

- **Per-bot cross-session memory** (`visibility: "bot"`). The vision
  accumulates under `~/.iterion/projects/<repo-key>/bots/evolve/memory/vision/`
  — private to Evoly, surviving across sessions, never leaking into
  Nexie or any other bot's context.
- **Mid-investigation `ask_user`**. The investigation agent interrogates
  you only when it must (an ambiguity the code can't resolve), the run
  pauses for your answer, and Evoly persists each answer to memory so it is
  never asked twice.

## How it partners with Nexie

Evolutions are handed off on two channels, both read by Nexie's next
survey with zero changes on Nexie's side:

- the shared `findings/` memory inbox (the deep plan/decision artifact),
- `backlog` kanban tickets (the actionable, dispatch-ready card).

## Models & credentials

All memory-bearing nodes run on the `claw` backend (every CLI backend —
`claude_code`, `pi`, Kimi, Grok, and legacy Codex — leaves `memory:` without
effect and triggers C047). Defaults are forfait-friendly:
claw nodes use `openai/gpt-5.5` (ChatGPT forfait), the one cross-family
"claude" reviewer uses `claude_code` (Claude Code OAuth forfait).
Override per node via `ITERION_EVOLVE_MODEL_GPT`,
`ITERION_EVOLVE_MODEL_CLAUDE`, `ITERION_EVOLVE_EFFORT*`.

## Known limitation

Memory tools are claw-only by design; this bot is therefore all-claw on
its memory-bearing nodes. Wiring per-bot memory into the `claude_code`
backend is a larger, separate piece of engine work (a `__mcp-memory`
subprocess) and is intentionally out of scope here — Evoly does not need
it.
