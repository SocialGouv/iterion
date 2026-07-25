---
name: okf-format
description: The Open Knowledge Format (OKF v0.1) contract for wiki pages — required YAML frontmatter, Markdown-links-as-concept-edges, reserved files, and exactly what the deterministic validator checks. Follow precisely so the validation gate passes.
---

# Open Knowledge Format (OKF v0.1) — the wiki contract

The wiki you write conforms to Google's Open Knowledge Format v0.1. It is
a deliberately small, standard, tool-agnostic shape: each page is a
concept node, and Markdown links between pages are the relationship
edges. A knowledge-graph explorer can ingest the result directly, so
getting the shape right matters. A deterministic validator enforces the
hard rules below and **fails the run** on any violation.

## Frontmatter (required on every concept page)

Every concept page begins, on line 1, with a YAML frontmatter block:

```markdown
---
type: <Concept type>
title: <Human title>
description: <One retrieval-optimized sentence for search/index>
tags: [tag-one, tag-two]
---

# <Human title>

...prose...
```

Rules the validator checks (hard — a miss fails the gate):
- The **first line** is exactly `---`, and there is a matching closing
  `---` before the body.
- A **`type:` field is present with a non-empty value.** This is the only
  strictly required field.

Strongly recommended (not hard-failed, but expected):
- `title` — the human page title (also used as its index label).
- `description` — ONE sentence optimized for retrieval; it becomes the
  page's line in the auto-generated directory index, so make it say what
  the concept *is*.
- `tags` — a YAML list of short strings, when useful.

`type` is producer-defined prose describing what kind of concept the page
is — e.g. `Architecture Overview`, `Workflow`, `Domain Concept`,
`Operations Guide`, `Integration`, `Quickstart Guide`. Pick a `type` that
reads well; there is no closed enum.

Producer-defined extension fields are allowed and must survive updates —
if a page already carries a field you do not recognize, preserve it.

## Reserved files (NOT concept pages)

- **`index.md`** — every directory's index. **Generated deterministically
  from the concept pages; never hand-write it.** The root `wiki/index.md`
  carries `okf_version: "0.1"`; nested `index.md` files carry no
  frontmatter. If you create one, the tool overwrites it.
- **`log.md`** — reserved changelog document (optional; no concept
  frontmatter).

The validator skips these two names when checking concept frontmatter.

## Links model concept relationships

Plain Markdown links between concept pages **are** the OKF relationship
edges. There is nothing else to declare — no separate graph file.

- **Put the link in the sentence that states the relationship**, and let
  the surrounding prose carry its meaning:
  > The [dispatcher](../architecture/dispatcher.md) *dispatches a run to*
  > the [runner](../architecture/runner.md) for each eligible issue.

  Good relationship verbs: `dispatches to`, `depends on`, `is configured
  through`, `is surfaced by`, `is secured by`, `shares infrastructure
  with`, `persists to`.
- **Every intra-wiki link must resolve.** The validator checks that the
  target file exists and, for a `path#anchor` link, that the `#anchor`
  matches a real heading (GitHub-style slug) in the target `.md`. A dead
  link or missing anchor fails the gate. Use relative paths
  (`../architecture/core.md`, `core.md#section`).
- **Do not pad the graph.** Do not add links solely to raise density, and
  do not auto-add reciprocal links. Add an inverse link only when it
  genuinely helps explain the target and the evidence supports it.
- **Prefer linking a canonical concept over duplicating it.** Do not mint
  thin pages just to create more nodes.
- The `quickstart.md` must link to every major concept for navigation
  (those navigation links are expected and do not count as semantic
  padding). Directory `index.md` links are navigation too.

## Orphans and the quickstart (warnings)

The validator **warns** (does not fail) when:
- a concept page has no concept links in or out (an orphan) — either wire
  it into the graph with evidence-backed relationships, merge it into a
  broader page, or leave it if it is genuinely standalone; or
- `wiki/quickstart.md` is missing — you should always provide it.

## What the validator will reject (summary)

| Check | Result |
|---|---|
| Concept page missing `---` / non-empty `type:` / closing `---` | **fail** |
| Intra-wiki link to a missing file or missing `#anchor` | **fail** |
| Any file changed outside the wiki tree | **fail** |
| Concept page with no concept links (orphan) | warn |
| No `quickstart.md` | warn |
