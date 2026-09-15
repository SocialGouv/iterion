---
name: adr-scope-detection
description: Heuristics for locating where ADRs live in an arbitrary target repo. Universal — Adry runs against any project, not just iterion.
---

# ADR scope detection

Adry is a **catalog bot** — it runs against any target repository, not
just iterion. Different projects place their ADR set in different
canonical locations. This skill captures the probe order Adry uses to
locate the ADR directory before authoring or auditing.

The `vars.adr_dir` default (`docs/adr`) is the Nygard convention used by
this repo and most projects. When that var is left at its default and
the directory does not exist, the workflow CREATES it on first author
— authoring is a from-zero bootstrap, the same way docs-refresh handles
a repo with no docs yet.

Override on the rare project that uses a non-default location:

```
iterion run bots/adr-cartograph/main.bot --var adr_dir=architecture/decisions
```

## Probe order (when the operator hasn't pinned `adr_dir`)

The `survey_code` agent uses this checklist to discover the ADR
directory before authoring. Stop at the first hit.

1. `docs/adr/` — the **Nygard universal convention** (this repo;
   ADR-tools default; spring-boot, kubernetes, others). Highest
   priority.
2. `docs/decisions/` — second-most-common Nygard variant.
3. `architecture/decisions/` — `adr-tools` default, used by some
   Spring / Java projects.
4. `adr/` (top-level) — used by some Rust and Go projects.
5. `documentation/adr/` — older convention, occasionally seen on
   enterprise projects.

If none of these directories exist, default to creating `docs/adr/` —
the Nygard convention. This matches the default `vars.adr_dir` and the
default `docs/` location the workspace's other documentation likely
uses.

## Recognising existing ADRs

Once the directory is located, an existing ADR file is recognised
when ALL of the following hold:

1. Filename matches `^[0-9]{3,4}-[a-z0-9-]+\.md$`. Tolerate 4-digit
   prefixes (some projects use them once they cross 1000 ADRs).
2. First non-blank line is an H1 heading.
3. The body contains AT LEAST one of: a `**Status**:` bullet, a
   `## Decision` heading, a `## Status` heading, a `## Context`
   heading. This catches both this repo's bullet-list-front-matter
   style and the YAML-front-matter Nygard style some projects use.

Files that match the filename pattern but fail (2) or (3) are
candidates for **rescue** (a malformed ADR), NOT for replacement.
Adry flags them as a high-confidence gap (`gap_kind:
no_error_handling` is the closest fit; suggest the operator fix the
header by hand) but does NOT overwrite them.

## Recognising the duplicate-prefix pattern

Concurrent PRs that each take the same NNN leave two files on one
prefix. It is common on an active tree and it is not a bug: iterion's
own `docs/adr/` carries EIGHT colliding prefixes as of 2026-09-15 —
`002`, `075`, `076`, `090`, `091`, `093`, `094` and `098` — so Adry's
`scan_adrs` emits
`duplicates: ["002", "075", "076", "090", "091", "093", "094", "098"]`
there. Treat the array as open-ended and expect it to grow; never
assume a single entry. Rules:

- Do NOT renumber existing ADRs (would break inbound references).
- Do NOT author an extra file on a colliding prefix to "fix" the
  duplicate.
- Advance `next_adr_number` (`max(NNN) + 1`) past every collision so
  new ADRs do not inherit one.
- When citing an ADR whose number collides, cite the filename slug
  rather than the bare number — "ADR-098" alone is ambiguous in a repo
  that has two of them.

## Recognising sibling structures

A few repos carry related-but-distinct structures next to their ADR
tree. Recognise them; do NOT confuse them with ADRs:

- `docs/rfcs/` or `rfcs/` — Request-for-Comments documents. These
  are PROPOSALS, not decisions; Adry's decision/constat shape does
  not apply.
- `docs/proposals/` — same as RFCs.
- `CHANGELOG.md` — chronological release notes. Not an ADR set.

When Adry finds such structures alongside the ADR directory, it does
NOT scan or modify them. It may, however, surface them in
`scope_notes` so the operator knows to pick a different bot for that
kind of work.

## When to widen the scope

The default `code_scope_globs: ""` (empty) means "scan the whole
workspace minus `excluded_dirs`". On a large monorepo this can be
slow; the operator narrows via:

```
--var code_scope_globs='pkg/**,cmd/**'
```

The narrowing is the operator's responsibility — Adry does not infer
"the interesting subdirectories" automatically. The
`survey_code` agent reads `vars.code_scope_globs` as authoritative.

## What this skill does NOT do

This skill is about **directory location**. The shape of an individual
ADR file lives in `adr-format.md`. The decision/mechanic distinction
lives in `decision-vs-mechanic.md`. The gap taxonomy lives in
`completeness-taxonomy.md`. Cross-reference those skills for those
questions.
