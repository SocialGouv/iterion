---
name: repo-maturity-assessment
description: >
  Heuristic for deciding whether a repository is mature/stable enough to
  warrant a long-horizon vision. Consulted by Deep Blue's survey and (before
  routing) by Nexie. Stack-agnostic — the agent picks the right commands
  for whatever repo it is pointed at.
disable-model-invocation: true
---

# Repo maturity assessment

A long-horizon vision is **valuable on a settled project** and **waste on
a churning one**. A greenfield repo needs throughput (Nexie + feature
work), not a five-year architecture. This skill is how you decide which
situation you're in.

You are stack-agnostic: detect the languages/ecosystems present and pick
the right commands for THIS repo. The signals below are described in
neutral terms; translate them to the stack in front of you.

## The signals (score each present / weak / absent)

1. **Commit-cadence stability.** Read recent history
   (`git log --oneline -n 60`, `git log --since="60 days ago"`). Steady,
   purposeful commits → positive. A flood of `WIP` / `fixup!` / force-
   pushes / "oops" → weak (the project is still thrashing).

2. **Architectural decision records.** Look for `docs/adr/`,
   `docs/decisions/`, `*/adr/`, or an architecture/design doc set. Their
   presence signals a team that deliberates direction → positive.

3. **CI stability.** Look for a CI config and, if visible, its recent
   pass/fail trend (e.g. `gh run list` if a GitHub remote, or the badge /
   workflow files). Mostly-green → positive; frequently-red → weak.

4. **Public-API / interface stability.** Low breaking-change cadence in a
   CHANGELOG, release notes, or `git tag` history → positive. Frequent
   breaking changes → the surface is still moving → weak.

5. **Test presence (rough, adaptive).** Estimate test density for the
   repo's stack (count test files vs source files in whatever convention
   the language uses). A real test suite → positive; near-zero → weak.

6. **Operational footprint.** Signs the thing is actually used/run:
   deployment configs, a release process, issue/board history, prior bot
   runs. Present → positive.

## The verdict

- **4+ signals positive → mature/stable.** A vision is appropriate;
  proceed with full confidence.
- **3 positive → stable enough.** Proceed, but keep the vision
  conservative and evidence-anchored.
- **≤2 positive → premature.** A vision now is likely waste. Your FIRST
  `ask_user` should surface this: tell the operator the project hasn't
  settled and ask whether they want a vision anyway (some do — e.g. to
  set direction for a young-but-deliberate project). If they proceed,
  **lead the vision with a stability guardrail** ("before any of these
  axes, the project needs N stabilising iterations") and bias the axes
  toward foundations (tests, CI, API shape) over ambitious expansion.

Map the verdict to `survey_output.maturity_verdict`:
experimental | early | stable | mature | legacy.

- **legacy**: settled but ossifying. A vision here is often about
  *managed evolution* — extraction, decoupling, sunset paths — not new
  capability. Say so.

## Honesty over flattery

Don't inflate maturity to justify a grand vision. An honest "this isn't
ready for a vision yet, here's why" is more valuable than an ambitious
roadmap a churning project can't act on.
