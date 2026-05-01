# vibe_review_alternating — companion notes

Companion to the [`vibe_review_alternating.iter`](vibe_review_alternating.iter)
workflow. This document is a **design journal**: what works in practice,
what is still an open problem, and what should be improved.

## Workflow intent

Alternate two reviewers from distinct model families (Claude Opus via
`claude_code`, GPT-5.5 via `claw`) over the **entire codebase** to reach a
production-ready state, with:

- A fixer per family that inherits the corresponding reviewer's session
  (cache savings + context continuity)
- A deterministic stop condition: two consecutive positive verdicts from
  opposite families (cross-family double approval)
- A loop budget (`review_loop(15)` today, tunable to project complexity)

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
| `confidence: low` treated as soft-approval for fix routing (not for `stop`) | Prevents a doubt from looping the fixer |
| Strict `stop` (two cross-family `approved=true`) | Prevents a low/low chain from terminating the run |
| Fixer `pushback` + `prior_pushback` to the next reviewer | Stops a persistent false positive from blocking convergence |
| `previous_scanned_areas` | Encourages broadening coverage iter after iter, instead of revisiting the same files |
| `review_loop(N)` | Upper bound that guarantees termination |

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

See [SKILL-run-and-refine.md](../SKILL-run-and-refine.md) for the
general run/refine practice on any `.iter`.
