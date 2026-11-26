---
name: evolve-memory-layout
description: >
  The contract for Deep Blue's two memory scopes — the PER-BOT vision tree
  (private across sessions) and the SHARED findings inbox (cross-bot
  handoff to Nexie). What file goes where, and why they must not be
  confused.
disable-model-invocation: true
---

# Deep Blue memory layout

Deep Blue uses **two distinct memory scopes**. Keeping them straight is the
difference between a private vision that survives across sessions and a
handoff Nexie can actually read.

## 1. The per-bot VISION scope (private, cross-session)

- Declared as `memory: { visibility: "bot", scope: "vision" }`.
- On disk: `~/.iterion/projects/<repo-key>/bots/evolve/memory/vision/`.
- **Private to Deep Blue.** Nexie, feature-dev, and every other bot CANNOT
  read it. This is where the long-horizon vision accumulates across
  sessions without leaking.
- Autoloaded into your system prompt each run: `VISION.md` +
  `CONTEXT_BRIEF.md`. Everything `.md` in the scope is auto-indexed.

Files you maintain here:

| File | Purpose | Budget |
|---|---|---|
| `CONTEXT_BRIEF.md` | The always-loaded brief: Objective / Hard constraints / Decisions / Open questions / Next action. | ≤400 words |
| `VISION.md` | The vision: title, horizon, axes (current→target + rationale + evidence), guardrails. | ≤600 words |
| `decisions/<YYYY-MM-DD>-<slug>.md` | One per substantive operator answer. Frontmatter `tags: [kind:decision, source:operator, topic:<x>]`. | short |
| `axes/<axis>.md` | Optional long-form per-axis exploration when an axis needs more than VISION.md gives it. | as needed |

## 2. The shared FINDINGS scope (cross-bot inbox → Nexie)

- Declared as `memory: { scope: "findings" }` — **no `visibility:`**, so
  it resolves to *project* visibility (shared).
- On disk: `~/.iterion/projects/<repo-key>/memory/findings/` — the
  SAME directory Nexie's survey + emit_action hygiene scan.
- This is where you drop one Markdown file per proposed evolution (the
  deep plan / technical decisions). See `backlog-handoff.md` for the
  frontmatter contract.

**Do not** put evolution proposals in the per-bot vision scope — Nexie
can't see it there. **Do not** put the private vision in `findings/` —
it would leak to every bot.

## Why per-bot, not project

The vision is Deep Blue's accumulated judgement, refined with the operator
over many sessions. Scoping it per-bot means:

- it survives across sessions (cross-run continuity), and
- it stays out of every other bot's context, so Nexie's "what's next"
  survey isn't polluted by Deep Blue's half-formed long-horizon drafts —
  only the finished, ratified evolutions reach Nexie, through `findings/`
  and the board.

## Path discipline

- Paths are scope-relative. Never absolute, never `../…`.
- Kebab-case filenames; date-prefix dated entries.
- One topic per file.

## Never record

- Secrets (API keys, tokens, OAuth credentials, signed URLs).
- Verbatim schema-typed outputs (iterion's artifact store covers those).
- Speculative bets the operator hasn't ratified (use `hypotheses/` with
  a `status:hypothesis` tag if you must note them, and never promote one
  to the vision without operator confirmation).
