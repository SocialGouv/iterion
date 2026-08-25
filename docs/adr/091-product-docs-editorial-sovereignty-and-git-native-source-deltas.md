# ADR-091 — Editorial sovereignty in the target repo, and git-native cross-repo source deltas

- **Status**: Accepted
- **Date**: 2026-08-25
- **Applies to**: `bots/product-docs` (Prody)
- **Extends**: CLAUDE.md "Catalog bots are repo-agnostic" + "Universal code bots — stack knowledge lives in skills"; the git-native philosophy (§4)
- **Neighbours**: ADR-058 (one campaign agent + a deterministic gate), ADR-044 (deterministic gates never an LLM judgment)

## Context

`product-docs` is the first catalog bot whose **workspace and subject
matter are in different repositories**: it runs inside a dedicated
documentation repository and writes about code that lives in N *other*
repositories, named by a product catalog. It is also the first whose
output is read by **non-developers** — the product's own users — which
makes the editorial line (structure, allowed blocks, vocabulary, tone) a
load-bearing part of the deliverable rather than a matter of taste.

Two decisions follow from that shape, and neither is obvious from the
code alone.

### 1. Where does the editorial line live?

Every other catalog bot ships its discipline in its own `skills/`: the
bundle is the authority, and a target repo that wants something else has
no say. Applied here that would mean iterion dictating the house style of
someone else's user-facing documentation — the structure of their pages,
the words their product uses, the tone their users read. Different
products, run by different teams, legitimately disagree about all three,
and the team that owns the product is the one that knows.

The alternatives considered:

- **Bundle-only skills** (the convention). Uniform output, zero
  configuration — and wrong for any team with an existing editorial
  charter. Their first run would rewrite their pages into iterion's
  shape, which is precisely the "destroyed human work" failure the
  campaign contract otherwise forbids.
- **Vars carrying the editorial line.** `--var tone=…`,
  `--var model=hub-and-step`. Editorial guidance is prose, not a flag;
  a var big enough to hold it is a file with worse ergonomics, invisible
  to review, and re-typed on every launch.
- **A closed set of named editorial presets.** The Nth-variant smell:
  the second product that fits none of them costs a bundle PR.

### 2. Where does "what changed in the sources" live?

Incremental mode needs to know which source commits the documentation
was last written against. `docs-refresh` answers the same question with
a commit trailer (`Bot: docs-refresh`) because the docs and the code
share one history — one `git log` sees both. Here they do not: the docs
repo's history says nothing about the source repos' shas, and the source
clones are throwaway (a fresh scratch dir on every cloud pod).

The obvious answer is a side-car state file in the scratch dir mapping
repo → last-documented sha. It is also the answer that loses the state
exactly when it matters: a run that dies mid-pass, a wiped scratch, a
different machine, a cloud pod that never sees the previous run's disk.
Worse, it decouples the record from the work — the file can claim a sha
whose pages were never committed, and nothing reconciles the two.

## Decision

### 1. The docs repository owns its editorial line; the bundle ships a default

The campaign loads, in this authority order:

1. **`<workspace>/<editorial_dir>/*.md`** (default `.product-docs/`) —
   the docs repo's own editorial skills. **Authoritative.** Where they
   disagree with anything the bundle says, they win, silently and
   completely.
2. **The bundle's `skills/`** — `modele-documentaire`, `blocs-gitbook`,
   `glossaire-produit`, `ton-et-style`: a generic default, in French,
   used for whatever the docs repo did not specify. Each one opens by
   declaring itself a default the docs repo may override.

`scan_hints` enumerates the overrides in force and reports them on every
pass, so the campaign is told which authority it is under rather than
having to discover it; the writeable-set gate **excludes**
`<editorial_dir>` so the bot can never rewrite the charter it is
governed by.

What stays in the bundle, non-overridable, is the part that is not
editorial taste but product-documentation *integrity*: sourced facts or
`[à confirmer]` and never an invented business rule; human-validated
prose preserved; a repository that could not be read declared as a hole;
and the four working-note artefacts the deterministic `page_lint` gate
refuses. A docs repo may restyle every page; it may not authorise the
bot to invent.

### 2. Source deltas are anchored on a commit trailer, in the docs repo

Every in-stride commit carries **two** trailers:

```
Bot: product-docs
Product-Docs-Sources: <repo-id>@<sha>,<repo-id>@<sha>,…
```

`catalog_ingest` reads the newest `Product-Docs-Sources:` line out of the
**docs repo's own history** and diffs each fresh clone from the shas it
records. The consequences are the point:

- **The record and the work are the same object.** A stamp exists only
  because a page was committed against those exact source commits. There
  is no way for the two to disagree.
- **Nothing to lose.** A crashed run, a wiped scratch dir, a fresh cloud
  pod, another machine, a different operator: `git log` in the docs repo
  is the whole state.
- **It is reviewable.** A human reading the PR can see which source
  commits each page was written against.

When a shallow clone cannot reach a recorded commit, the ingest attempts
a targeted `git fetch --depth 1 <sha>` and, failing that, reports
`delta_unavailable` with the reason on the inventory entry — the campaign
is told to treat the pass as a full survey. **A delta that could not be
computed is never reported as an empty delta**, which the campaign would
read as "nothing changed" and act on by writing nothing.

## Consequences

**Gained**

- A product team adopts the bot without adopting iterion's house style,
  and their charter is versioned and reviewed in their own repo.
- Adding an editorial dimension the bundle never anticipated costs a
  markdown file in the docs repo — no bundle PR, no engine PR, no enum.
- Incremental mode works on the first cloud run, on a machine that has
  never seen the product before, with no durable state outside git
  (which also keeps the bot clear of the "filesystem-only durable seam"
  hole ADR-073 had to close retroactively).

**Paid**

- **Two authorities to reason about.** A page that looks wrong may be
  the docs repo's charter being honoured. The run report names the
  overrides in force so the answer is one line away, but the ambiguity
  is real and inherent.
- **An override can be bad advice.** A poorly-written `.product-docs/`
  file degrades the output, and iterion cannot lint prose it deliberately
  does not own. The integrity rules above are the floor that keeps a bad
  charter from producing *false* documentation rather than merely ugly
  documentation.
- **A dropped trailer costs the next run its base.** If the campaign
  omits `Product-Docs-Sources:`, the next incremental run finds an older
  stamp (or none) and degrades to a wider survey — expensive, never
  wrong. The contract states both trailers as required in three places
  (system prompt, user prompt, playbook skill); the failure mode is
  deliberately the safe one.
- **The stamp names only readable repos.** A repo that was `degraded`
  this run contributes no sha, so the next run re-reads it from scratch.
  That is the intended behaviour: there is no documented baseline for a
  repository nobody could read.

## Alternatives rejected

| Alternative | Why not |
|---|---|
| Editorial line in the bundle only | Dictates a house style to teams who own their product's voice; first run rewrites their pages |
| Editorial line as launch vars | Prose does not fit a flag; invisible to review; re-typed every launch |
| A closed set of editorial presets | Nth-variant smell — the second unfitting product costs a bundle PR |
| Side-car scratch file for source shas | Lost on a crash, a wiped scratch, or any other machine; can disagree with what was actually committed |
| A state file committed into the docs repo | A second source of truth next to the commits, and a file the writeable-set gate would have to special-case |
| Full clones so the delta is always computable | Pays a full history for every source repo on every run to avoid an honest, reported degradation |
