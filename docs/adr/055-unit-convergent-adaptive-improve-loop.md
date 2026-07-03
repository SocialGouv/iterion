# ADR-055 — Unit-convergent, adaptive improvement loop (inverting the "manual Claude Code beats iterion" gap)

Status: **accepted** (2026-07-03). Supersedes the whole-repo-sweep +
terminal-commit + blind-chunk-review design of the loop-bot family
(`whole_improve_loop`, `branch_improve_loop`, and the review half of
`feature_dev`) for the *improvement* use case. Piloted on
`whole_improve_loop`, then rolled to the family.

## Context

A production-readiness dogfood of `whole_improve_loop` (Willy) against this
repo — run `019f2247`, mono-Claude (sonnet-5 fixer / sonnet-5 reviewer),
~9h of real (OS-suspend-excluded) execution across 48 loop iterations and
2.46M tokens — produced **52 files of genuinely good hardening** (a net-new
`secretguard` package, an OOM guard in the streaming aggregator, ~55% net-new
tests) but **committed zero of it**, and the reviewer was still surfacing
fresh high-confidence blockers at iteration 50. Verdict trend over the last
16 iterations oscillated (28 reject / 23 approve overall); the streak gate
never came close to saturating.

The operator's blunt summary, which this ADR takes as the problem statement:

> *"Today, a **manual** improvement loop with Claude Code works better than
> iterion. It should be the inverse."*

That is correct, and the run is the evidence. A human driving Claude Code
by hand, in the same wall-clock, would have **committed a dozen landed
improvements**. iterion committed none. The reasons are structural, not
model quality — the same model, driven by the graph, underperforms itself
driven by a human:

| Property that makes a manual Claude-Code loop work | What the loop-bot graph did instead |
|---|---|
| **Whole-repo context**; the agent navigates freely, follows cross-refs | **Blind chunking** (issue #12, context-budget): each reviewer sees ONE slice → incomplete/incorrect findings (a "dead" symbol may be called from an unseen chunk) |
| **Commits as it goes** → every improvement lands, GC-safe, reviewable | **No commit until global convergence** → on a large repo that never converges, nothing lands; 9h of value stranded in a gitignored worktree |
| Human says "good enough, ship" → **convergence is reachable** | **Streak of N clean cross-family sweeps of the whole repo** → unreachable: a real repo always has another real issue, so the streak keeps resetting |
| **One agent** reviews+fixes in one context → doesn't re-litigate | **Reviewer and fixer are separate agents** with disjoint context → the reviewer re-derives every pass; `prior_pushback` is a patch over the split |
| ~every cycle = a landed improvement | 22 of 54 cycles were review-only: the loop's dominant activity is *reviewing*, not *fixing* |

The deeper lesson: the three safety mechanisms — blind chunking
(anti-overflow), commit-after-global-review (anti-bad-commit), and the streak
gate (anti-oscillation, ADR-052 / the "converge to an asymptote" rule) —
each **inverted into a liability** at whole-repo scale. They *fragment*
context, *withhold* commits, and *prevent* completion. The asymptote
machinery, designed to stop a loop oscillating, here stops it *finishing*.

## Decision

Redesign the improvement loop around one principle:

> **iterion should orchestrate a capable, adaptive agent — amplifying it with
> what a human-driven manual loop cannot cheaply do (persistence, parallelism
> across units, multi-model adversarial verification, auto-landing,
> resumability) — WITHOUT breaking the two things that make the manual loop
> work: whole-unit context and incremental commits.**

Concretely, five changes, replacing "whole-repo-sweep → terminal-commit"
with "per-unit-convergence → incremental-commit":

1. **Adaptive, whole-context worker — not blind chunks.** The fixer is a
   native-adaptive agent (the `AppendToNative` / authored-base system-prompt
   posture, ADR on adaptivity) that navigates the repo itself within a
   budget, exactly like manual Claude Code. Deterministic chunking is
   demoted to a **fallback for genuine context overflow**, and even then the
   unit is a **coherent** one (a package / feature / directory the agent can
   hold in full, with cross-references reachable), never an arbitrary byte
   slice. The unit is the atom of work, not a context-budget artifact.

2. **Convergence per UNIT, and the loop is bounded + monotonic.** Each unit
   runs `review → fix → re-review` until *that unit* is clean and stable
   (bounded per-unit iterations, the asymptote guarantee now scoped to the
   unit where it IS reachable), then it is **committed** and removed from the
   working set. The loop terminates when the ranked backlog of units is
   processed once — a finite, monotone process — not when a global streak
   saturates. Coverage is guaranteed by "every unit processed once", the way
   the old streak gate *intended* but at a scope where it terminates.

3. **Incremental commit — landing is decoupled from global convergence.**
   Each converged unit produces a real commit (on the run's worktree branch)
   as it lands. A run that is interrupted, budget-capped, or forfait-paused
   has already banked every completed unit — no stranded diff, no GC risk.
   This is the single highest-value change and stands alone even if 1/2/4/5
   were deferred.

4. **Multi-model verification becomes the value-add, applied per commit.**
   iterion's real edge over a manual loop is cheap parallel adversarial
   verification (claude + gpt independently confirm a fix is real, not a
   façade — the anti-Goodhart guarantee). Keep it, but as a **per-unit /
   per-commit gate** ("does THIS landed change hold up?"), not as an
   unreachable global sweep. This preserves the ADR-052 mono/dual topology
   and the cross-family streak — re-scoped from "the whole repo" to "this
   unit".

5. **Completion is bounded by backlog / budget / value — or the operator —
   not by a global asymptote.** Units are ranked by severity; the loop
   processes highest-value first and stops when the backlog is exhausted or
   the budget/forfait bound is hit, reporting what remains. The operator can
   call "enough" at any point and keep every committed unit — the manual
   loop's reachable stop condition, restored.

The net: `whole_improve_loop` becomes *"a manual Claude-Code improvement
loop, but persistent, parallel across units, multi-model-verified per commit,
auto-landing, and resumable"* — strictly dominating the manual loop, because
it does everything the manual loop does **plus** the orchestration a human
cannot do at low cost.

## What is preserved (not thrown away)

- **The asymptote rule (ADR-052, `docs/workflow_authoring_pitfalls.md`)** —
  re-scoped from repo-sweep to per-unit, where "converge and stop" is
  actually reachable. `streak_check`, `prior_pushback`,
  `previous_scanned_areas`, `loop.<name>.previous_output` all still apply
  *within a unit*.
- **The right-artifact rule** — reviewers still diff the uncommitted working
  tree (`git diff HEAD`, `git add -N .` for untracked), now per unit.
- **Mono/dual review topology (ADR-052)** — unchanged; the cross-family
  verification runs per unit.
- **Anti-façade / Goodhart resistance** — strengthened: a per-commit verify
  is a tighter oracle than a whole-repo verdict.

## Alternatives considered

- **Keep the sweep, add only incremental commit (Chantier D alone).**
  Cheaper, and it does fix the "nothing lands" symptom. Rejected as the
  *primary* lever because it leaves the blind-chunk fragmentation and the
  unreachable global streak in place — the loop would land commits but still
  waste most cycles re-reviewing and still never terminate. Kept as the
  fallback/first-increment if the full redesign proves too large.
- **Side-by-side prototype (`improve-adaptive`) benched vs the old bot.**
  Good for proof, but delays the fix and doubles maintenance; the dogfood run
  is already the proof. We pilot the redesign *on* `whole_improve_loop`
  behind its existing name and bench the before/after with
  `iterion bench asymptote` + a landed-commits/hour metric.
- **Drop chunking entirely, always whole-repo.** Rejected: genuine
  context-overflow repos exist; chunking-by-coherent-unit as a fallback is
  the safety net. The change is *what* a chunk is (coherent unit vs byte
  slice) and *when* it is used (overflow fallback vs always).
- **Unify reviewer+fixer into one agent.** Attractive (kills re-litigation
  at the source) and allowed per-unit, but keeping them separate preserves
  the independent-verification value. Decision: keep separate but **share the
  unit's context** (feed the fixer's reasoning to the reviewer and vice
  versa) rather than fully merging — get non-re-litigation without losing the
  adversarial second opinion.

## Consequences

- A long/interrupted/capped run **always leaves landed, reviewable commits** —
  the stranded-worktree GC-risk class (this run, and the prior "dispatched
  runs strand commits" note) is closed for the improve family.
- The loop **terminates** (backlog exhausted / budget hit), removing the
  "grinds forever, never ships" failure mode this run exhibited.
- Yield/hour rises: cycles shift from re-review overhead to per-unit
  fix+land; verification is scoped, not repeated over the whole repo.
- Migration is incremental: (a) incremental commit + per-unit convergence
  first (the highest-value, smallest-risk slice), (b) coherent-unit chunking
  replacing byte-slice chunking, (c) shared review/fix context. The bot's
  DSL keeps its schema/topology; the graph shape changes from a single global
  loop to a per-unit loop over a ranked unit list (a `foreach`/`group` over
  units, ADR "groups+iteration").
- Risk: per-unit commits can be noisier than one squashed terminal commit.
  Mitigated by a final optional squash/curate step and by ranking so the
  first commits are the highest-value.
- The counting-truth fixes (ADR-054: monotonic active-duration + semantic
  loop counting) are the *instrumentation* that made this diagnosis legible —
  without them the run read as "15h / 49 iterations" and the waste was
  invisible. Good telemetry is a precondition for this redesign's before/after
  measurement.

## Rollout

1. Pilot on `whole_improve_loop`: per-unit loop over a ranked unit list,
   per-unit convergence, incremental commit, per-unit cross-family verify.
2. Bench before/after: landed-commits/hour and `iterion bench asymptote`
   (per-unit convergence must still settle) on the same repo/models.
3. Roll the pattern to `branch_improve_loop` and the `feature_dev` review
   half; update `docs/workflow_authoring_pitfalls.md` (the asymptote section)
   to state the per-unit scoping.
4. Write the dogfood bilan (`docs/bot-runs/whole-improve-loop.md`) closing
   the loop on run `019f2247`.
