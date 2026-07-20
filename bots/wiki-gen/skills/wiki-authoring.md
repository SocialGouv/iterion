---
name: wiki-authoring
description: Operating playbook for building and incrementally maintaining a navigable, grounded, Open-Knowledge-Format wiki for any repository. Read this first before writing any wiki page.
---

# Wiki authoring — the playbook

You build a **navigable wiki** for the repository in front of you: a
structured tree of concept pages a newcomer (human or agent) can start
from and understand what the repo does, how it is organized, and where
to change things. You write under `wiki/` only. The companion skill
[okf-format.md](okf-format.md) is the exact frontmatter + link contract a
deterministic validator enforces — follow it precisely.

## Inviolable boundaries (a deterministic gate enforces these)

1. **Write ONLY under the wiki directory.** Never edit source code,
   config, build files, or anything outside the wiki tree. A post-pass
   scope check fails the run if any file outside the wiki changed. If you
   discover a real code bug while surveying, do **not** fix it — file a
   board finding (`board.create`) and keep documenting.
2. **Never hand-write directory `index.md` files.** They are regenerated
   deterministically from each concept page's frontmatter after your
   pass. You write the *concept* pages; the tool owns the indexes. If you
   create one, it will be overwritten.
3. **Ground every claim.** Do not invent files, modules, APIs, commands,
   or behaviour. Every substantive statement must be backed by a file you
   read, a command you ran, or git history. If you cannot verify it, do
   not write it.

## The shape of a productive pass

### 1. Discover — targeted, not exhaustive
- Read the entrypoints, the build manifest (package.json / go.mod /
  pyproject / Cargo.toml / Makefile / Taskfile), and the top-level
  package layout. Identify the real subsystems.
- Read the highest-signal source and any existing docs (README, `docs/`,
  `CLAUDE.md` / `AGENTS.md`) — treat existing docs as primary material to
  summarize and link, not duplicate.
- Use `git log` / `git show` selectively on important files to explain
  **why** code exists, not only what it does. Do not over-index on
  ancient history.
- Do **not** glob `**/*` from the root or read every file. Sample by
  subsystem.

### 2. Plan before writing
Write a temporary `wiki/_plan.md` listing:
- the concept pages you intend (path under `wiki/` + the one concept each
  page owns + the source evidence for it), and
- the relationships between concepts, one per line, as
  `source concept -> relationship meaning -> target concept`
  (e.g. `dispatcher -> dispatches to -> runner`), so cross-links are
  designed before pages exist.

**Delete `wiki/_plan.md` before you finish** — it must not remain in the
wiki.

### 3. Write the concept pages
- **One canonical home per concept.** Do not repeat the same explanation
  across pages; give each concept a single page and link to it.
- **Organize like human documentation**, not a file inventory. Group into
  meaningful sections — e.g. `architecture/`, `workflows/`, `domain/`,
  `operations/`, `integrations/` — chosen to fit *this* repo.
- **`wiki/quickstart.md` is the mandatory entrypoint.** It states what the
  wiki covers and how it is organized, and links to every major concept.
- **Put a concept link in the sentence that explains the relationship**
  (see okf-format.md). Prose carries the meaning; the link is the edge.
- **Capture both technical detail and product/business logic**, and
  include change-oriented guidance: where to start, what to watch out
  for, which tests/checks matter when changing each area.
- **Keep it concise enough to maintain.** A wiki no one can keep current
  is worse than a small accurate one.

### 4. Commit in stride
After each page (or small coherent group), stage everything with
`git add -A` and commit: `docs(wiki): <what>`, body ending with the
trailer line `Bot: wiki-gen`. Git is your durable state — an interrupted
run keeps every page you committed, and a fresh pass reads `git log` to
see what is already done.

## Incremental update (a wiki already exists)

When `wiki_exists` is true, do **not** regenerate wholesale:
- Your `changed_code_files` input is the code that moved since the last
  wiki update. Touch only the concept pages that code affects.
- Keep a tight diff budget: a handful of changed source files should
  produce a handful of page edits, not a rewrite.
- Never churn formatting-only. If nothing material changed for a page,
  leave it. **A no-op update is a correct outcome** — say the wiki is
  already current and stop.

## Reacting to gate feedback

If your `fail_log` input is non-empty, the deterministic validator
rejected the previous pass. It names exactly what failed:
- **INVALID OKF FRONTMATTER** — a concept page lacks a valid opening
  `---`/`type:`/closing `---`. Fix the named pages (see okf-format.md).
- **DEAD INTRA-WIKI LINK** — a `[text](target)` points at a missing page
  or `#anchor`. Fix the link or create the target.
- **SCOPE VIOLATION** — you wrote outside the wiki tree. Revert those
  changes (`git checkout <base> -- <path>` or revert the commit).

Fix exactly what it names. Do not re-litigate accepted pages.

## Termination contract (your structured output)

- `wiki_complete`: `true` **only** when every concept you intended this
  pass is written with valid frontmatter and resolvable links, the
  quickstart links every major concept, and nothing outside the wiki tree
  was touched. Under-reporting costs one extra pass; over-reporting lands
  you right back here with the same failures.
- `pages_written`: concept pages created/updated this pass.
- `remaining`: short note on what a future pass should still cover (empty
  when complete).
- `findings_filed`: true if you filed any code-bug board finding.
- `summary`: 1–3 sentences.
