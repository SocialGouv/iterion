# ADR-057 — Axis-driven work-list sweep (whole-improve-loop's real mechanism)

Status: **accepted** (2026-07-03). Replaces the chunked-review mechanism of
`whole_improve_loop` (ADR-011 chunking + ADR-055 per-unit convergence over
chunks) with an **axis-driven work-list sweep**. ADR-055's landing/convergence
machinery (per-item verify gate, incremental commit, bounded loops) is kept;
what changes is the **unit of work** and how it is discovered.

## Context

Two dogfood facts and one observed human workflow forced this.

1. **Chunked review can't converge or go global.** `whole_improve_loop`
   deterministically byte/package-chunks the repo and hands each reviewer one
   slice to find *whatever* is wrong. On a whole repo it never converges (run
   `019f2247`: 9h, 48 iterations, 0 commits — there is always another local
   issue in the next chunk), and it structurally **cannot produce a global,
   cross-cutting change** because no agent ever holds the whole system: a
   reviewer handed a slice can only find slice-local issues. ADR-055 made the
   loop converge + land *per unit*, but the unit was still a chunk, so the
   output stayed local and incremental-but-small (proof runs `019f2750` /
   `019f275e`: one dead-code fix each).

2. **The operator's proven manual pattern is a sweep, not a review.** Inspecting
   the operator's own Claude Code sessions against this repo shows the shape of
   every successful whole-codebase improvement: **a to-do work-list + frequent
   incremental commits** — e.g. one session with 178 `TodoWrite`s, 1053 edits,
   **112 commits**; another 75 / 280 / 94. The git log is the output side of the
   same pattern: `split the six largest source files into cohesive smaller
   files`, `converge hand-rolled <kbd> chips onto the ui/Kbd primitive`,
   `extract store-agnostic streaming package`, `make generated spec types the
   source of truth`. Each is **one determined axis applied to every matching
   site across the codebase, committed site-by-site** — never "review each
   chunk for unknown issues".

3. **Chunking is the wrong mechanism for that.** Chunking slices the repo to
   *review everything for the unknown*; the operator's pattern *searches* for
   the sites of a *known axis* and *transforms* each. Different verb (transform
   vs review), different unit (a matching site vs a byte slice), different
   discovery (search vs partition), different done-condition (the axis is
   applied everywhere vs a clean review streak).

## Decision

`whole_improve_loop`'s mechanism becomes an **axis-driven work-list sweep**.
`improvement_prompt` is the **axis** (e.g. "split every file > 600 lines into
cohesive units", "converge duplicated X onto a shared helper", "make error
handling use pattern Y"). The graph:

1. **`enumerate`** — a whole-repo, adaptive agent (claude_code, full tools,
   whole-repo context like native Claude Code) reads the codebase *by its real
   structure* (grep / glob / read — NOT chunks) and emits an **ordered
   work-list**: `[{id, title, targets (files/symbols/sites), change_spec}]` +
   `total_items`. This is the operator's "write the todos" step. Persisted to a
   crash-safe state file (resumable, like the old cursor state).
2. **Sweep loop** over the work-list, one item at a time:
   - **`transform`** — an adaptive fixer (whole-repo context) applies the axis
     change for the current item.
   - **`verify`** — the deterministic build/test gate (reuse
     `verify_build`/`verify_run`): the change must be green; red → bounded
     verify-fix retry; still red → skip the item uncommitted (never land broken
     code) or pushback.
   - **`review`** (multi-model, the iterion edge over a manual loop) — one
     cross-family reviewer confirms the transform *correctly and safely applies
     the axis* at this site (consistency + no regression), not an open-ended
     re-audit.
   - **`commit_item`** — an incremental commit for this item (reuse
     `commit_unit`'s `git add -A` incl. untracked, minus scratch, empty-guard);
     message = axis + item title. One commit per item, matching the operator's
     ~1-commit-per-work-item cadence.
   - advance to the next item.
3. **Converge** — when the work-list is exhausted, a **`re_enumerate`** pass
   re-scans for any remaining sites matching the axis (the done-oracle: the
   axis is fully applied iff a fresh scan finds nothing). New sites → appended,
   sweep continues; none → **done**. Bounded by `max_items` / a loop cap so a
   pathological axis can't run forever.

Kept from ADR-055: the per-item verify gate, incremental commit, bounded
verify-fix loop, crash-safe resumable state, stack-agnostic behaviour (the axis
+ the repo define the sites; no language literals in the DSL). Retired: the
`snapshot_chunk` byte/package chunker, the per-chunk streak, the partial-view
guard (2b) — all chunk-specific. The old chunked-review mode MAY be preserved
behind a flag as a rarely-used "find-unknown-issues" fallback, but the axis
sweep is the default and the primary mechanism.

## Why this is strictly better

- **Goes global.** The `enumerate`/`transform` agents hold whole-repo context,
  so a cross-cutting change (introduce a shared abstraction, converge N call
  sites onto a primitive) is a first-class work-item — the exact thing chunked
  review cannot express.
- **Converges by construction.** Done = "the axis's sites are exhausted and a
  re-scan finds none", a finite monotone condition, instead of an unreachable
  clean-sweep streak over a whole repo.
- **Lands continuously.** One verified commit per item = the operator's proven
  cadence; an interrupted/capped run has banked every completed item.
- **Amplifies the manual loop.** It *is* the operator's manual Claude Code
  workflow (todo-list + incremental commits) plus what a human can't cheaply
  do: parallel/persistent execution, a deterministic per-item verify gate, and
  cross-family review of each transform — the ADR-055 north star ("orchestrate
  a capable adaptive agent; don't fragment it") realized.

## Alternatives considered

- **Keep chunked review, add a global architect phase in front.** The prior
  sketch. Rejected: it bolts a whole-context phase onto a mechanism (chunking)
  the operator says is wrong, and keeps the review verb where the workflow
  needs the transform verb.
- **A separate new "axis-sweep" bot, leave whole-improve-loop as chunked
  review.** Rejected by the operator: they want *whole_improve_loop* to be this.
  One bot, their pattern encoded; chunked review demoted to an optional
  fallback.
- **Deterministic enumeration (pure grep/AST, no LLM).** Insufficient alone —
  "sites matching the axis" needs judgement for most axes. The re-enumeration
  done-oracle is the deterministic backstop; the enumeration itself is adaptive
  (mirrors the operator).

## Consequences

- A large rewrite of `bots/whole-improve-loop/main.bot` (graph, schemas,
  prompts) — `snapshot_chunk` → `enumerate`/`next_item`/`re_enumerate`; the
  chunk state tuple → a work-list + cursor. e2e rewritten around the sweep.
- The bot's identity sharpens: "apply a determined improvement axis across the
  whole codebase, site by site, verified and committed" — a campaign engine,
  not a scanner.
- Risk: a vague axis yields a vague work-list. Mitigation: `enumerate` must emit
  concrete `targets` per item (a site it can point to), and `verify` +
  per-item review keep each landed change honest; a fuzzy item that can't name
  a target is dropped, not guessed.
- Validation needs a live dogfood on a concrete axis (e.g. "split files > N
  lines") — stub e2e proves the graph/flow, the live run proves the sweep
  actually enumerates + transforms + lands across the codebase.

## Family rollout (follow-on, after the whole_improve_loop pilot is proven)

The axis-sweep mechanism generalizes to the loop-bot family — but only where the
job is *"systematically apply a determined change across a body of code"*, NOT
everywhere. Align by fit, not blanket:

- **`branch_improve_loop` (Billy)** — YES, the natural sibling: an axis sweep
  whose **scope is the branch's touched files** (`git diff <base>..HEAD` name
  set) instead of the whole workspace. `enumerate` runs the axis over that file
  set; everything else (transform/verify/review/commit_item/re_enumerate) is
  identical. It shares whole_improve_loop's convergence machinery today, so it
  inherits the rewrite most directly.
- **The shared review machinery** — the `alt`→`reviewer_*`→`streak_check` +
  ADR-052 mono/dual topology is currently common to both improve loops; factor
  the reusable sweep pieces (verify gate, `commit_item`, re-enumeration
  done-oracle, work-list state) so both bots and any future sweep share one
  implementation rather than diverging copies.
- **Does NOT fit** (leave as-is): `feature_dev` (build ONE feature — a
  goal-directed task, not a codebase-wide axis), and any pure diff/PR *review*
  bot (judge a given change, not enumerate-and-transform). `docs-refresh` is a
  borderline case — "align docs with code" is axis-like and MAY adopt the sweep
  (enumerate stale-doc sites → transform), evaluate after Billy.

Rollout order: (1) whole_improve_loop (this ADR), prove live; (2)
branch_improve_loop as the branch-scoped sweep, factoring the shared pieces; (3)
evaluate docs-refresh. Each step keeps the ADR-052 topology + universality +
right-artifact invariants and its own dogfood bilan.
