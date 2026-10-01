# ADR-119 — One canonical instruction root, with its size gated

- Status: accepted
- Date: 2026-10-01
- Deciders: jo (direction: one root for every harness, a two-level tree, budgets enforced), Claude (analysis)
- Relates to: [097-github-projects-v2-board-sync.md](097-github-projects-v2-board-sync.md)
  (the work-tracking contract the root carries); issue #2071 under epic #1480;
  [docs/agents/README.md](../agents/README.md)

## Context

Every session on this repository pays its instruction files before it does
anything, and the bill repeats on every surface:

| Who | What it loaded |
|---|---|
| A Claude Code session | `CLAUDE.md`, 17.0 KB |
| pi, on **every call** | `CLAUDE.md` + `AGENTS.md`, 22.8 KB |
| Codex | `AGENTS.md` 5.8 KB, then `CLAUDE.md` by hand because it said so |
| A `claude_code` node in a run worktree | `CLAUDE.md` twice — the worktree's and the parent checkout's — 34 KB |

An audit of the two files found about 7.5 KB of the 22.8 KB needed in every
session. The rest was duplicated (the review loop stated in six places, "Billy
is paused" in nine), task-specific, or stale. `CLAUDE.md` told Claude to read
`AGENTS.md` at session start, but Claude Code (2.1.282) reads `AGENTS.md`
natively only when no `CLAUDE.md` exists: the work-tracking contract reached a
Claude session only when the model chose to open it. And "the router stays
short by construction" held while someone watched it — it grew to 17 KB.

## Decision

1. **`AGENTS.md` is the single canonical root**, the one file every harness
   loads: Codex and pi read it natively, and `CLAUDE.md` imports it with a bare
   `@AGENTS.md` line, which Claude Code expands at launch. `CLAUDE.md` keeps
   only the brand line the `brand` check requires, the import, and what is true
   of Claude Code alone.
2. **The root carries what every session needs and nothing else**: identity,
   how to find code before reading it, the setup and the gate every session
   owes, the non-negotiables, the philosophy in one line per principle, and
   the work-tracking contract. Everything else lives in
   [`docs/agents/`](../agents/README.md), read on demand, each page carried as
   one line by the index of its own directory.
3. **No rule changed** in the move: content went to its canonical page or was
   compressed, and what existed only in the old root (the claw bump procedure,
   the lint curation, the board's `Mode` field, …) moved to the page that owns
   it.
4. **Sizes are a gate, not a hope.** `pkg/repomap/agenttree_test.go`, in the
   required `test` check, fails when a root or an index exceeds its byte
   budget, when `CLAUDE.md` loses the import (written outside code and
   comments), or when a `docs/agents` page is missing from its directory's
   index — reachable through some other page is not enough. It reads links
   with the repository's own link checker (`internal/docsguard`); a
   synthetic-tree test proves each check bites. Raising a budget is an edit of one table, made in review —
   the escape hatch, greppable on purpose.
5. **Finding comes before reading**: the root names the deterministic map
   (`iterion map`, the committed indexes) for exact questions and graphify,
   when present, for semantic ones. graphify stays operator-local and outside
   the repository's guarantees.

## Alternatives considered

- **Two roots without overlap** (`CLAUDE.md` the router, `AGENTS.md` the
  work-tracking contract). Smaller change, but Codex still reads the router by
  hand, and Claude still sees `AGENTS.md` only when it decides to open it.
- **Claude Code path-scoped rules** (`.claude/rules/` with `paths:`). Lazily
  loaded and precise, but Claude-only — Codex, pi and the claw backend never
  see them, which breaks the claw ↔ claude_code parity this repository holds —
  and `.claude/` is gitignored here.
- **A generated index only** (`docs/references/map-docs.md`). It lists every
  page and its opening line, but cannot say *when* to read a page, which is
  the whole value of an agent index. It stays the twin, not the router.

## Consequences

- The always-loaded cost drops from 22.8 KB to about 6.5 KB for a Claude Code
  session and for every pi call; a `claude_code` node in a run worktree still
  loads the parent checkout's copy too, now about 13 KB instead of 34 KB.
- A paragraph added to the root now fails a test; it belongs in the tree.
- Pointers into the old `CLAUDE.md` sections are repointed to the canonical
  page; the tree's own reorganisation into domains follows in a second change
  under the same issue.
