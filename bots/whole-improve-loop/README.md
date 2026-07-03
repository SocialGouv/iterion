# whole_improve_loop — companion notes

Companion to the `whole_improve_loop` workflow ([`main.bot`](main.bot),
previously known as `vibe_review_alternating`). This document
is a **design journal**: what works in practice, what is still an open
problem, and what should be improved.

## Workflow intent

Alternate two reviewers from distinct model families (Claude Opus via
`claude_code`, GPT-5.5 via `claw`) over the **entire codebase** to reach a
production-ready state — **unit-convergent and incremental-commit**
(ADR-055):

- The workspace is split by package into token-budgeted chunks
  ("units"); the loop processes **one unit at a time**.
- A fixer per family that inherits the corresponding reviewer's session
  (cache savings + context continuity)
- **Per-unit convergence**: a unit loops review → fix → re-review until IT
  is clean and stable (two consecutive clean reviews — cross-family in DUAL,
  same-family twice in MONO), with pushback protection against false
  positives, then it is **committed** and the cursor advances. A stubborn
  unit is force-skipped after a bounded number of passes so it can never
  block the rest of the backlog.
- **Incremental commit, gated per unit**: each converged unit lands its OWN
  commit on the run's worktree branch, immediately, so an interrupted /
  budget-capped run keeps every unit it finished. (The old design committed
  nothing until an unreachable global clean sweep — see ADR-055 context.)
- **Per-unit build/test verify** (ADR-055 2a): before an incremental commit
  lands, the SAME deterministic, stack-agnostic build+test gate the
  finalization uses runs on the converged unit (`unit_verify_build` writes
  `.whole_improve_loop.verify.sh` from the repo's own tooling;
  `unit_verify_run` re-runs it and gates on the REAL exit code) — so **every**
  incremental commit is build+test green on its own, and an interrupted run
  leaves a chain of individually-green commits, not a possibly-broken
  intermediate one. A red per-unit verify does **not** commit: it loops back
  to `unit_verify_build` (bounded `unit_verify_loop(3)`) to make the unit
  green; if it still can't build, the unit is **skipped without committing**
  (force-skip semantics — its changes stay uncommitted for the next pass, the
  final whole-tree gate, or the operator), so broken code never lands and one
  un-buildable unit can't block the backlog.
- **Shared reviewer↔fixer context** (ADR-055 2c): the fixer records a concise
  `change_rationale` (what it changed + why, per unit) that is threaded
  forward to the next cross-family reviewer of the same unit
  (`prior_change_rationale`), so that reviewer verifies the fix with the
  fixer's intent visible instead of re-deriving it — killing re-litigation at
  the source. It **augments** `prior_pushback` / `previous_scanned_areas`; the
  reverse direction (reviewer verdict → fixer) is already wired via
  `blockers` + `fix_plan` + `session: inherit`.
- The **run terminates** when the ranked unit backlog has each been
  processed once (`units_done >= num_chunks`), not when a global streak
  saturates. A per-run pass budget (`max_review_passes`, default 15) bounds
  each run; per-unit state (cursor / unit_streak / unit_passes / units_done)
  is persisted (`.whole_improve_loop.state`) so a large repo finishes over
  successive re-dispatches, banking committed units each time.
- **Context-budget chunking** so the loop survives ~150k+ LoC workspaces
  without exhausting the reviewer's context window (issue #12, below)

## Context-budget chunking (issue #12)

Pointed at a large workspace (iterion itself is ~195k LoC of Go; the
chunker measures ~2.2M estimated tokens of source across ~1,200 files),
a reviewer told to "review ALL the code" simply reads files until it
hits `context_length_exceeded` — on the **first** iteration. To stay
usable at that scale the loop no longer hands the reviewer the whole
tree. Instead:

- A deterministic `snapshot_chunk` **tool** node (Python, no LLM) is the
  workflow entry and runs once per pass. It groups source files by
  **package/folder boundary first**, then bin-packs them into chunks of
  at most **`max_review_chunk_tokens` (default 30000)** estimated tokens
  (~4 bytes/token). A package larger than the budget is split by file
  size. It emits **one chunk** per pass with its source inline.
- The reviewer audits **that one chunk** (the source is in its prompt;
  read tools are for narrow cross-references only, never bulk reads), so
  a single review can never exceed the chunk budget.
- The cursor points at the **current unit** and advances only when that
  unit converges (or is force-skipped); the per-unit tuple (unit_streak /
  unit_passes / units_done) is persisted alongside it in a single JSON
  state file (`.whole_improve_loop.state` at the workspace root — **add it
  to your `.gitignore`**; delete it to restart the backlog from unit 0),
  so a re-dispatched run resumes ON the current unit and keeps banking
  converged units instead of re-scanning the first chunks.
- `streak_check` scopes the asymptote to the **unit** (ADR-055): a unit
  converges after **2 consecutive clean reviews of THAT unit**
  (`unit_streak + 1 >= 2`). The count is family-agnostic — in DUAL the two
  passes are cross-family (the condition router flips family each pass); in
  MONO they are the single family twice (the same accepted trade-off the
  old count-based stop made, so ADR-052 mono/dual is unchanged here). A
  converged unit is committed and removed from the working set. The **run**
  stops when every unit has been processed once (`units_done >= num_chunks`)
  — a finite, monotone process — rather than when a global clean sweep
  saturates, which a real repo can never satisfy (the ADR-055 failure this
  replaces). A single clean review cannot converge a unit (needs 2), and a
  unit the loop cannot satisfy is force-skipped after a bounded number of
  passes so it cannot block the backlog.

Tune with `--var max_review_chunk_tokens=N`: raise it to review more per
pass (fewer chunks, faster convergence, larger prompt); lower it for a
smaller-context model.

The reviewer + fixer **share** this snapshot: the fixer inherits the
same-family reviewer's session, which already holds the chunk it must
fix. Design rationale and the rejected alternatives (per-pass fan-out +
merge; `__scan-shards` child runs; loop-counter rotation) are in
[ADR-011](../../docs/adr/011-whole-improve-loop-context-chunking.md).

### Focused runs: `scope_globs` (the WHERE)

`improvement_prompt` / `scope_notes` are the **WHAT** (the review axis);
they do **not** restrict which files are chunked. So a focused
`improvement_prompt` ("just pkg/runtime") still chunks the *whole*
workspace and the reviewers no-op every irrelevant chunk at full
per-chunk review cost — ~$30 to crawl to the one chunk you care about on
an iterion-sized repo (the 2026-06-14 finding in
[docs/bot-runs/whole-improve-loop.md](../../docs/bot-runs/whole-improve-loop.md)).

`scope_globs` is the **WHERE**: a comma/space-separated list of fnmatch
globs matched against workspace-relative paths, applied by
`snapshot_chunk` at the `os.walk` source **before** chunking. Empty
(default) = the whole workspace (unchanged behaviour). A bare directory,
or a `dir/**` / `dir/*` form, matches the whole subtree.

```sh
iterion run bots/whole-improve-loop/main.bot --var scope_globs=pkg/runtime
iterion run bots/whole-improve-loop/main.bot --var scope_globs="pkg/runtime,pkg/store"
```

Because the prune happens at the source, `total_files` / `num_chunks` /
`loop_max` all scale to the focused set — a focused run converges
**in-bound like a small repo**
instead of inheriting the whole-repo pass budget. It does **not** loosen
review rigor (the reviewer still audits its chunk against the full
production-ready grid); it only restricts which files are chunked. A glob
that matches nothing yields the empty-workspace sentinel (`num_chunks=1`,
`chunk_label=empty`) rather than silently reviewing everything, so a typo
fails loud. Pair it with `improvement_prompt` to focus both axis and
files (e.g. `--var scope_globs=pkg/server --var improvement_prompt="auth
and input validation only"`).

### Large workspaces: the backlog spans passes (and sometimes runs)

Reviewing ~2.2M tokens of source with premium models needs ~2·`num_chunks`
clean reviews (each unit confirmed twice) plus fix passes — more than the
`loop_max = 2·num_chunks + max_review_passes` a single run affords on an
iterion-sized repo, and a meaningful fraction of the `max_cost_usd: 60` /
`max_duration: 2h` budget. So on such a repo a single run makes **bounded,
context-safe progress** (it will not crash on context), **commits the units
it converges**, and exits via `fail` ("backlog not finished yet") rather
than processing every unit in one shot.

Crucially, unlike the old design a `fail` here still **leaves committed
work** — every unit that converged this run is already a commit on the
worktree branch, GC-safe. Nothing is stranded.

This is genuinely multi-run, and it finishes: the per-unit tuple (cursor /
unit_streak / unit_passes / units_done) is persisted in
`.whole_improve_loop.state`, so each re-dispatch (or manual re-run of the
acceptance command) resumes mid-backlog and keeps banking converged units
until `units_done >= num_chunks`. A run that finishes the backlog exits via
`stop -> done`.

This is **crash-safe**. The cursor and `units_done` advance only once a
verdict exists (a unit converged or was force-skipped), so the on-disk
state is always consistent: `cursor` points at the unit currently under
review, and everything before it is either committed or skipped. If a run
dies on a non-clean pass before the fixer finishes (a `fix_*` failure
routes to `fail` + re-dispatch — the normal large-repo path), the
re-dispatch resumes **on** that unit and re-reviews it rather than skipping
past it. So a blocker can never be "credited" by a crash.

To finish a **mid-size** repo in a single run, raise `max_review_passes`
(and the budget) so one run can process every unit, and/or raise
`max_review_chunk_tokens` (fewer, larger units). On a repo as large as
iterion the multi-run path is the intended one — an inherent cost ceiling,
not a defect. See ADR-055.

## Convergence pattern observed in practice

Method applied manually with Claude Code on projects of comparable
complexity (engine + vendored SDK + DSL): **5 to 40+ iterations** before
convergence depending on code maturity and threshold strictness. The
typical profile:

- **Early iterations**: high density of real blockers (concrete bugs,
  races, leaks, vulnerabilities). The fixer applies, the code progresses.
- **Middle iterations**: density falls. Reviewers begin proposing more
  subtle improvements. The "blocker = breaks production" discipline
  becomes critical to avoid sliding into perfectionism.
- **Late iterations**: redundancy with earlier passes, false positives
  caught via `previous_scanned_areas`, stylistic or hypothetical
  blockers. **This is the asymptotic-convergence signal.**
- **Alarm signal**: a sudden burst of critical blockers late in the run.
  Hypotheses: hallucinating reviewer, fixer not actually applying changes,
  or re-flagging of items already pushed back.

The goal is for a reviewer to **detect this pattern themselves** and
eventually approve — not to be told "from iteration 5 onward, be more
lenient." Such an instruction would bias the verdict (the reviewer might
over-approve to satisfy the instruction, regardless of actual code
quality). The aim is self-regulation by observation, not prescription.

## The threshold

The fundamental trade-off:

- **Too lenient** → false-positive approval, broken code shipped to prod.
- **Too strict** → infinite loops, the workflow never terminates and
  burns the budget.

No single prompt resolves the trade-off. Current levers:

| Lever | Effect |
|---|---|
| `confidence: low` treated as soft-approval for fix routing (not for unit convergence) | Prevents a doubt from looping the fixer |
| Per-unit convergence: 2 consecutive clean reviews of the SAME unit before it commits | Prevents a single lucky pass from landing an unimproved unit; scopes the asymptote where it is reachable (ADR-055) |
| Force-skip after a bounded number of per-unit passes | A stubborn unit cannot block the backlog; the run still terminates |
| Fixer `pushback` + `prior_pushback` to the next reviewer | Stops a persistent false positive from blocking convergence |
| `previous_scanned_areas` | Encourages broadening coverage iter after iter, instead of revisiting the same files |
| `max_review_passes` (ADR-055) | Per-run pass bound that guarantees termination (wires `review_loop` = 2·num_chunks + max_review_passes). Raise it (with budget) to finish a mid-size repo in one run; on large repos the persisted per-unit state carries the backlog across re-dispatches, banking committed units each run |
| `max_review_chunk_tokens` (issue #12) | Per-pass context budget. Smaller → more chunks, safer context, more passes to converge; larger → fewer chunks, faster convergence, bigger per-pass prompt |
| `scope_globs` | Path-scope filter (the WHERE). Prunes the chunk plan to matching subtrees before chunking, so a focused run converges in-bound like a small repo instead of paying the whole-repo sweep cost. Empty = whole workspace |

## Open prompt-engineering directions

1. **Auto-calibration by trend**: the idea is that a reviewer compares
   its own blocker density against earlier iterations (via the relayed
   verdict). If its blockers strongly resemble already-handled ones, it
   should lower its bar. Worth testing by passing
   `loop.review_loop.previous_output.history` concisely — without an
   explicit "approve at iter N+" instruction, which biases.
2. **Cumulative scanned_areas**: today only the last iteration is passed
   via `loop.previous_output.scanned_areas`. True accumulation (union of
   `scanned_areas` across all iterations) would require either a union
   operator in iterion's expression engine, or a compute+history
   pattern. To explore.
3. **Blocker quality scoring**: add a `blocker_severity_count` field
   (count of blockers by severity: critical / important / info) instead
   of a flat blocker list. The streak check could then trigger on "0
   critical for 2 iters" rather than pure `approved=true`.
4. **Anti-perfectionism on the fixer side**: the fixer could push back
   more aggressively when a blocker looks like polish. Today, pushback
   is under-used.
5. **Comparison to a manual reviewer**: observe whether the
   Claude→GPT→Claude→GPT sequence introduces a bias (Claude more
   rigorous, GPT more pragmatic). Also worth testing GPT-only and
   Claude-only alternation with varied prompts.

## Empirical observations from this session

During the first real validation (hardened workflow, April 30, 2026):

- **Run 1**: 3 iterations of `review_loop`. Real bugs found (claw
  `recovery.go` WorkDir guard, iterion `resume.go` vars re-seed). Early
  exit caused by a GPT hallucination (`family: "missing-patch"` →
  fallback `done`). Fix: enum on `family` + fallback
  `streak_check -> alt`.
- **Run 2**: 1 iteration of `review_loop`, cross-family convergence in
  ~5 min. Reviewers focused on the recent commits (via `git log` /
  `git show`), not the global codebase. **Not a "natural" convergence —
  a convergence on a sub-perimeter.** Fix: extended `review_system`
  prompts to require a global production-ready audit and to record
  `scanned_areas`.
- **Run 3** (upcoming with this commit): the goal is to check whether
  the broadened scope produces more iterations with decreasing blocker
  density, or whether more levers are still needed.

## Companion plan-file path

For multi-day refinement sessions, keep here:

- Design decisions and the rationale for each choice (session modes,
  loop bounds)
- Empirical data per run (duration, # iterations, cost, longest stretch
  without human intervention)
- Adaptation guide when reusing the pattern for another project

See [SKILL-run-and-refine.md](../../SKILL-run-and-refine.md) for the
general run/refine practice on any `.bot`.
