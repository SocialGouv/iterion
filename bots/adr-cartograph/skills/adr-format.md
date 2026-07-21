---
name: adr-format
description: Exact Nygard ADR format used in docs/adr/ — filename convention, markdown bullet-list front-matter, required sections, repo-relative reference style.
---

# ADR format — what an Adry-authored ADR looks like

This skill captures the **exact** Nygard-derived ADR format already in
use under `docs/adr/`. The format is **not** YAML-frontmatter Nygard;
this repo uses a markdown bullet-list head followed by H2 sections. When
authoring or editing, follow this shape verbatim — the
`completeness-taxonomy.md` review loop checks the structure.

Two canonical repository-relative examples to read before authoring are
`docs/adr/008-bot-golden-replay-framework.md` and
`docs/adr/004-provider-fallback-chain.md`.

## Filename

```
docs/adr/NNN-kebab-slug.md
```

- `NNN` is a **3-digit zero-padded** number, strictly monotonic. The
  next number is computed by `scan_adrs` as `max(NNN of existing ADRs)
  + 1` and emitted as `next_adr_number`. Use that number — do not
  guess.
- `kebab-slug` is a short descriptive phrase, lowercase, words joined
  by `-`. Aim for 3 to 6 words. The slug describes the **decision**,
  not the affected subsystem. `kebab-slug` does NOT include the word
  "adr" or the number.
- `.md` extension. No other suffix.

### Duplicate prefix tolerance

The repo historically contains TWO ADRs prefixed `002-*` (a
non-bug-blocking artifact of two PRs landing the same NNN). When
`scan_adrs` reports a non-empty `duplicates[]` array, treat it as a
WARNING only — do not author a third `002-*` to "fix" it, and do not
renumber existing files (which would break inbound references).
`next_adr_number` advances past the duplicate; new ADRs use the next
free integer.

## Front-matter — markdown bullet list, NOT YAML

The ADR opens with an H1 title, then a 4-item bullet list:

```markdown
# ADR-NNN: <descriptive phrase>

- **Status**: Accepted
- **Date**: YYYY-MM-DD
- **Authors**: Adry
- **Code**: [path/to/file.go](../../path/to/file.go) (`Symbol`),
  [pkg/x/y.go](../../pkg/x/y.go) (`OtherSymbol`)
```

### Field rules

- **Status** — for Adry-authored ADRs, default `Accepted` (Adry records
  decisions the code already embodies; they are accepted by virtue of
  being shipped). Other Nygard values (`Proposed`, `Deprecated`,
  `Superseded by ADR-NNN`) are valid for hand-authored revisions but
  Adry does not emit them.
- **Date** — the date Adry observed the decision (today, ISO
  `YYYY-MM-DD`). NOT the date the code was first committed.
- **Authors** — `Adry` for solo-authored ADRs. When a human revises an
  Adry ADR, the human's handle is appended (`Adry, devthejo`).
- **Code** — at LEAST one link. **Tolerate both**: existing ADRs in
  this repo use either `**Code**:` (ADR-004 style) or `**Code
  context**:` (ADR-008 style). When AUTHORING, prefer `**Code**:`.
  When REVISING an existing ADR, preserve whichever key it already
  uses.

The Code list:

- Each entry is a markdown link with a repo-relative `../../` path.
  ADRs live two directories deep under repo root
  (`docs/adr/NNN-*.md`), so `../../` lands on repo root.
- The link text is the bare path. The path-after-link can optionally
  carry a `(`Symbol`)` or short description.
- Cite the SMALLEST set of files that embody the decision. A decision
  spanning 30 files cites the 2–4 key seams, not all 30.

## Required sections (H2)

In this exact order:

```markdown
## Context

## Decision

## Trade-offs

## Alternatives considered

## Consequences
```

### `## Context`

Why this decision had to be made. The constraints that ruled out the
obvious shape. 1 to 4 paragraphs. Do NOT explain the decision itself
here — that belongs in `## Decision`.

### `## Decision`

What was decided, stated in the **active voice** (the code does X).
Cite the key seams inline with repo-relative `../../` paths, the same
shape as the front-matter `Code` list. 2 to 6 paragraphs.

### `## Trade-offs`

What this decision costs and what it buys. The repo's house style
favours a **comparison table** with one column per option and rows for
the dimensions traded. Example (from ADR-008):

```markdown
| Dimension | Chosen approach | Rejected approach |
|---|---|---|
| Fixture shape | … | … |
| Replay determinism | … | … |
```

The table is OPTIONAL — when the trade-off is one-dimensional, a
single paragraph suffices. Always end the section with the **one
honest concession** sentence (the cost the chosen path actually
incurs).

### `## Alternatives considered`

Numbered subsections, one per non-trivial alternative:

```markdown
### 1. <short name of alternative>

<one-paragraph description of the alternative>

**Rejected because**: <the constraint that ruled it out>.
```

At least ONE alternative is mandatory. An ADR with zero alternatives
considered is a mechanic, not a decision — drop it.

### `## Consequences`

The downstream effects of the decision, both costs and benefits.
Bulleted list, 3 to 8 entries. Each consequence is stated as a
bolded lede + a 1–2 sentence elaboration:

```markdown
- **Schema drift is a loud failure, by design.** Tightening a schema
  that an existing fixture no longer satisfies fails the gate. …
```

### Optional sections

- `## Deviations from the source plan` — used when the implementation
  drifted from an earlier design doc and the ADR captures the
  divergence. Used by ADR-008.
- `## Docs` — link to the user-facing docs that elaborate the
  decision (e.g. ADR-004 links to `docs/backends.md`). Goes in the
  front-matter bullet list, not as an H2 section.

## Inline reference style

Within prose, file references use **repo-relative `../../` paths**
(ADRs live 2 dirs deep):

```markdown
The retry loop in [`pkg/foo/retry.go`](../../pkg/foo/retry.go) …
```

NOT absolute paths. NOT root-relative (`/pkg/...`). NOT
`@pkg/foo/retry.go`. The `../../` form means a maintainer clicking the
link from GitHub's rendering of `docs/adr/NNN-*.md` lands on the
referenced file.

Symbol citation inside a code span uses backticks:
`` [`pkg/foo/bar.go`](../../pkg/foo/bar.go) (`Handler`) ``. The
symbol is OPTIONAL — bare path is enough when the file does one thing.

## What NOT to write

- **YAML front-matter.** This repo uses the markdown bullet-list head.
  Don't introduce `---\n...\n---` blocks.
- **Future tense.** "We will use X" — wrong, the code already does.
  Use active voice present: "The X is …".
- **Marketing prose.** "Elegant" / "robust" / "powerful" are not
  trade-offs. State the cost.
- **Hand-waved alternatives.** "We could have used Y" without naming
  what Y would have cost is filler. At least one alternative must be
  specific enough to grep for.
