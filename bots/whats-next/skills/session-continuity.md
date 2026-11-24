---
name: session-continuity
description: >
  Why this bundle keeps nothing of its own across sessions, where its
  durable record lives instead, and how to read the cross-bot findings/
  inbox another bot's run left behind.
disable-model-invocation: true
---

# Session Continuity — this bundle has no memory tree

**This bundle cannot write the tree.** iterion's workspace memory
tools, and the auto-index and autoload they feed, are claw-only, and
`whats-next` runs its turn on `backend: "claude_code"` with no
`memory:` block. The write surface that would reach the tree directly
is withheld twice over: the node's `tools:` list does not name it, and
a declaration withholds every native tool it omits; the runtime
permission gate denies `Bash`, `Write` and `Edit` on top of that.

So keep no durable brief here, and promise none — **the board is this
bundle's durable record**: the epic body, a ticket, or a comment on the
ticket a fact belongs to. The next session reads it there.

Reading stays open (`allow: ["Read(**)"]`): a file another bot left in
a scope is yours to read with `read_file`, at an absolute path. The
scope directory is iterion's store encoding of the workspace path —
every `/`, `:` and `\` becomes `-`, a leading `-` is added if the
result lacks one, e.g. workspace `/home/u/repo` under `$ITERION_HOME`
(default `~/.iterion`) is
`~/.iterion/projects/-home-u-repo/memory/<scope>/`.

The write contract itself — the tools, the frontmatter the auto-index
reads, when to grow the tree, how to supersede a fact instead of
deleting it — belongs to a bundle that carries a `memory:` block on
`backend: "claw"`. It is authored in that bundle's copy of this skill,
the family's source
([bots/evolve/skills/session-continuity.md](../../evolve/skills/session-continuity.md)),
and this copy deliberately does not restate it — a page teaching a
write its own node cannot perform is what makes this copy a variant
rather than the family's source.

## The findings/ scope (cross-bot inbox)

`findings/` is a **distinct memory scope** under `…/memory/` — a
sibling of the per-bot scopes, not a subfolder of one. It is a
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

## Do not record

Whatever you make durable — a board comment, an epic body, a quote
from a finding you read — leaves out:

- Secrets (API keys, tokens, OAuth credentials, signed URLs).
- Verbatim schema-typed outputs (artifacts cover that).
- Speculative plans the operator hasn't ratified.
- PII beyond what the operator declared.
