---
name: session-continuity
description: >
  How to use the iterion workspace memory tools (memory_read,
  memory_write, memory_list) to keep a compact, frontmatter-tagged
  knowledge tree that survives claw's heuristic context compaction.
disable-model-invocation: true
---

<!-- TODO: duplicated from bots/whats-next/skills/session-continuity.md
     (iterion has no skill-sharing primitive yet — see CLAUDE.md "Skills
     ... live with their bundle"). Keep in sync with that copy until a
     shared-skill mechanism exists. Evoly-specific layout is in
     evolve-memory-layout.md. -->

# Session Continuity — iterion workspace memory

Three tools, scoped to this bot's memory tree:

- **memory_read(path)** — read a file from the scope.
- **memory_write(path, content)** — write/replace a file in the scope.
- **memory_list(path)** — list files in a scope subdirectory.

The scope lives **outside the repo**, under
`~/.iterion/projects/<encoded-workspace>/memory/<scope>/`. Iterion
**auto-generates an index** of every `.md` file in the scope at
node start (one line per file: path + title + tags + optional
description) and prepends it to your system prompt. You do NOT
maintain an `INDEX.md` by hand — the auto-index always reflects
ground truth.

The bot config may additionally **autoload** the full content of
specific files (e.g. `CONTEXT_BRIEF.md`) on top of the index.
Those are the files you should keep current.

## Writing files

When you call `memory_write`, prefix the body with YAML
frontmatter. The auto-index reads these three keys:

```
---
title: "Operator hard constraints"
description: Running list of constraints captured from feedback.
tags: [constraint, operator, hard]
---

# Body H1 (optional; falls back to frontmatter title)
…
```

- **title** — short label shown in the index.
- **description** — one sentence; surfaces in the index.
- **tags** — kebab-case keywords. Use them so other iterations can
  grep mentally through the index. Conventions:
  - `kind:*` — `kind:decision`, `kind:learning`, `kind:brief`, `kind:question`
  - `topic:*` — `topic:auth`, `topic:roadmap`, `topic:scope`
  - `status:*` — `status:open`, `status:resolved`, `status:revisited`

Frontmatter is **optional**. Without it the index falls back to
the first `# heading` as title, no tags. But tags are the cheapest
way to make a growing memory tree navigable.

## CONTEXT_BRIEF.md (the always-loaded brief)

This is the only file that's typically autoloaded in full. Keep
it under 400 words. Sections:

- **Objective** — one sentence, operator-facing.
- **Hard constraints** — bullets the operator made explicit.
- **Decisions** — durable choices, one-line rationale each.
- **Active files / paths** — recently referenced source files.
- **Open questions** — unresolved items blocking progress.
- **Next action** — single verb-led step.

Update it after every major decision or operator revision. Don't
restate raw artifacts (roadmap JSON, schemas) — those live in
iterion's artifact store.

## When to grow the tree

Beyond CONTEXT_BRIEF.md, create dated/topic-tagged files when:

- A decision has rationale worth preserving (e.g.
  `decisions/2026-05-18-dropped-feature-X.md`).
- A pattern recurs across operator feedback worth noting as a
  learning (e.g. `learnings/code-style.md`).
- You want a long-form note that would bloat the brief.

The auto-index will surface all of these on the next iteration —
no manual bookkeeping needed.

## Updating, superseding, deleting

Memory is long-lived: across many iterations the same fact gets revisited.
Before you write a fact, **reconcile it against what's already there** — read
the related file(s) first (the index makes them cheap to find), then classify:

- **New** — nothing covers it. Write a fresh file.
- **Covered** — an existing file already says this, correctly. Do nothing;
  don't write a near-duplicate.
- **Refines** — an existing file is right but incomplete. Edit it in place
  (`memory_write` the same path with the fuller content). One fact, one home.
- **Supersedes** — a new fact *contradicts* an old one (a decision reversed, a
  value changed). Do NOT delete the old file. Instead:
  - mark the old file `status:superseded` in its tags and add a
    `superseded_by: <new-path>` line to its frontmatter;
  - give the new file a `supersedes: <old-path>` line.
  The trail stays legible (why the change happened), and a future iteration
  reading the old file sees at a glance that it is stale and where to look.
  (`status:superseded` shows in the index; the two link lines are ignored by
  the indexer and read by humans/agents.)

**Delete only a memory that was outright *wrong*** — never one that merely
aged out. A superseded fact carries history; a wrong fact is noise. When in
doubt, supersede (reversible) rather than delete (destructive).

This keeps the tree from accreting silent contradictions — the failure mode
that makes a long-lived memory untrustworthy.## The findings/ scope (cross-bot inbox)

`findings/` is a **distinct memory scope** — a sibling of this
bot's own scope under `…/memory/`, not a subfolder of it. It is a
shared inbox where dispatcher-spawned bots drop one Markdown file
per observation they want a later session (or the operator) to act
on. Filenames are date-prefixed, `YYYY-MM-DD-<slug>.md`, and the
frontmatter writers use is:

```
---
title: "<one-line summary>"
description: "<one sentence>"
id: "<optional stable finding ID>"
kind: "bug" | "drift" | "idea" | …
source_bot: "<the bot that filed it>"
strong_keywords: ["<optional distinctive token/error-code/function>"]
tags: ["area:<x>", "severity:<low|med|high>"]
---
```

Lifecycle of a finding:

- **open** — the file exists; a future session should triage it.
- **ingested** — a whats-next roadmap item materialised it as a
  board issue (the file itself may still linger).
- **archived-by-bot** — *terminal; set by whats-next's `emit_action`
  auto-hygiene pass.* After emitting the roadmap, that node scans
  `findings/` and, for each file, greps **read-only** `git log`
  since the finding's date. On a **confident** match — a commit
  naming the finding's filename/slug, an exact stable finding `id`
  (or alias such as `finding_id`, `issue_id`, `key`, `uid`), an
  explicit distinctive `strong_keyword`, or a resolution verb plus
  the finding's exact title/phrase — it writes a one-line entry to
  the run's plan markdown and only then deletes the file:
  `- archived-by-bot: <filename> — <sha> "<subject>"`.
  The audit section `## Findings archived (auto-hygiene)` is created
  on the first archived (or dry-run) match and omitted when there are
  no matches. Because the file is removed, `archived-by-bot` lives in
  that audit log (and `emit_output.archived_findings`), NOT in any
  surviving frontmatter. Matching is deliberately conservative:
  ambiguous or keyword-only matches are LEFT IN PLACE —
  under-archiving is the safe failure mode, and git history is the
  recovery net. Operators can disable or dry-run the pass via
  `ITERION_WHATS_NEXT_FINDINGS_HYGIENE=off|dry`; dry-run uses the
  same audit heading with `(dry)` bullets but does not delete files or
  populate `archived_findings`. If the audit line cannot be written
  and verified first, the finding is left in place.

If you only ever **write** findings from another bot, the required
contract stays small: keep using `title`, `description`, `kind`,
`source_bot`, and `tags`. Add `id` when you have a stable finding ID,
and add `strong_keywords` only for distinctive tokens a future commit
is likely to mention exactly (bug IDs, namespaced error codes, unique
API/function names). You do **not** need a `status:` field — the
hygiene pass infers resolution from git, and an unresolved finding
simply survives to the next session.

## Path discipline

- Paths are scope-relative. Never absolute, never `../...`.
- Use kebab-case filenames. Date-prefix dated entries.
- One topic per file.

## Do not record

- Secrets (API keys, tokens, OAuth credentials, signed URLs).
- Verbatim schema-typed outputs (artifacts cover that).
- Speculative plans the operator hasn't ratified.
- PII beyond what the operator declared.
