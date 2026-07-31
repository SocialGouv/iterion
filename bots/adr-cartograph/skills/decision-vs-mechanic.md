---
name: decision-vs-mechanic
description: The dam against ADR-spam. What counts as ADR-worthy (non-obvious trade-off with real alternatives) vs what is a mechanical refactor that should NOT get an ADR.
---

# Decision vs mechanic — the ADR-worthiness dam

The single most common failure mode of an LLM-driven ADR pass is
**ADR-spam**: filling `docs/adr/` with one entry per refactor,
extract-function, or rename, until the directory becomes a list of
trivia and the genuinely architectural decisions drown in the noise.

This skill defines the dam. Apply it as a HARD GATE before raising any
decision in `survey_code`'s output: if it does not pass all three
checks below, set `is_mechanic: true` and Adry's review loop will drop
it.

## The three checks

A finding is **ADR-worthy** only when all three are true:

### 1. Non-obvious trade-off

A future maintainer reading the code without the ADR would NOT
predict the trade-off. If the trade-off is "we used the standard
library's `sort.Sort` because it's there", that's not an ADR — that's
common sense. If the trade-off is "we used `sort.SliceStable` despite
the perf hit because the records have a secondary stable order
contract", that's ADR-worthy.

Test: write the trade-off as one sentence. Could a competent peer
have guessed it from the code alone, without reading the ADR? If yes
— mechanic. If no — candidate.

### 2. At least one real alternative was considered (and rejected)

ADR-worthy decisions sit at a fork in the road. If there was no fork
— if the chosen path is the only path — it's not a decision, it's a
mechanic. The alternative must be **specific enough to write a
section about** in the ADR's `## Alternatives considered`.

"We could have used a different library" is NOT specific enough.
"We could have used `golang.org/x/sync/errgroup` instead of our own
fan-out helper, but errgroup's first-error-wins semantics conflict
with our `await: best_effort` requirement (we need ALL branches even
when some fail)" IS specific enough.

Test: name the alternative AND the constraint that ruled it out. If
you cannot do both, the trade-off is mechanical.

### 3. The decision will be re-challenged when constraints change

ADRs exist so future maintainers know what to reconsider when the
constraints shift. A decision that is robust to all foreseeable
constraint changes is not worth an ADR — it's just correct code.

Test: name ONE plausible future constraint change that would invite
reconsidering this decision. ("If we move from Go 1.21 to a runtime
with native generics over slices, the helper could be replaced by a
type-parameterised stdlib function.")

If no plausible re-challenge condition exists, drop the entry.

## What is NOT ADR-worthy — concrete examples

The following findings are **mechanical** and MUST be marked
`is_mechanic: true`:

| Finding shape | Why it's mechanical |
|---|---|
| "Renamed `foo()` to `parseFoo()` for clarity." | A rename. No trade-off. |
| "Extracted shared logic into `pkg/util/helper.go`." | A refactor. No fork in the road. |
| "Replaced `if-else` chain with a `switch`." | Same semantics, different syntax. |
| "Added a logger to package X." | Tooling. Not a decision. |
| "Bumped dependency Y from v1.2 to v1.3." | A patch upgrade. Belongs in a changelog, not an ADR. |
| "Moved a function from one file to another within the same package." | A reorganisation. |
| "Replaced a hand-rolled `for { ... }` with `slices.Sort`." | Adopting stdlib idiom. Trade-off is too obvious to write. |

## What IS ADR-worthy — concrete examples

(Drawn from the real ADRs in this repo's `docs/adr/`.)

- **ADR-001** (round-robin router mode): the IR adds a new router
  mode `round_robin`. Alternative: extend the existing `condition`
  mode with explicit counters. Constraint: the counter would have
  bled into the IR's edge state, breaking the no-side-effect contract
  of edges.
- **ADR-004** (per-node provider fallback chain): a `provider:`
  string can now be a comma list, falling through on hard error.
  Alternative: a separate `fallback:` block. Constraint: would have
  required per-element model resolution on the claw backend, a
  larger feature.
- **ADR-008** (bot golden replay framework): record/replay at the
  `runtime.NodeExecutor` seam, NOT at the LLM client seam.
  Alternative: inject a fake `api.APIClient` and replay through the
  runtime. Constraint: each affected bot contains human/tool nodes
  the runtime cannot run unattended.

Each of these names a fork, the alternative not taken, and the
specific constraint that ruled the alternative out.

## What about borderline cases

A pattern that **adds new public API surface** (a new CLI command, a
new exported function consumed across packages, a new DSL primitive)
is usually borderline. Apply the three checks:

- If the new API is the obvious shape given the constraints → mechanic.
- If the new API picked one of several plausible shapes → ADR-worthy.

When genuinely uncertain, lean toward `is_mechanic: true`. `build_manifest`
drops those entries before the campaign; for every surviving candidate, the
campaign independently re-reads the code and re-applies all three checks.
Goodhart's law favours over-permissive ADR filters, so this skill biases the
dam shut.

## Output shape (reminder for survey_code)

Each decision the agent emits MUST carry:

```
title:        short kebab-slug-able phrase
context:      ≤500 chars — the constraint
decision:     ≤500 chars — what the code does, active voice
code_paths:   string[] of repo-relative paths embodying the decision
alternative:  ≤300 chars — one specific alternative + the reason it was rejected
rechallenge:  ≤200 chars — when would this be worth reconsidering?
is_mechanic:  bool — true ⇒ build_manifest drops the entry before campaign
```

If `is_mechanic: false` requires you to LIE about any of the four
content fields (rechallenge "when constraints change" is not specific
enough), set `is_mechanic: true` and move on.

## Why this dam is strict

Once `docs/adr/` is polluted with mechanical entries, every
subsequent review pass has to read past the noise to find the
genuine decisions. The cost is paid forever. The cost of dropping a
borderline candidate that was actually ADR-worthy is one missed ADR
this run — recoverable on the next pass when the operator notices
and adds a hint via `scope_notes`. Asymmetric error costs ⇒
asymmetric filter.
