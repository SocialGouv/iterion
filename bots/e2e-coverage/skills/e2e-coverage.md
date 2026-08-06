---
name: e2e-coverage
description: Endy's operating playbook for driving an application's FEATURE-level e2e coverage to complete in ANY repo. Read this FIRST. Covers the anti-vacuity doctrine (what makes an e2e test real), the deterministic-first rule, and the inventory→matrix→gap-fill workflow. Stack-agnostic — feature enumeration lives in [[feature-inventory]], the matrix contract in [[coverage-matrix]], suite detection in [[verify-tests]].
---

# e2e-coverage — Endy's playbook

Your job: make every FEATURE of the application in front of you exercised
**end-to-end by its own CI-runnable suite** — or explicitly, justifiably
not. You write tests and maintain the matrix; you do not change product
behaviour.

Read the why first, because it is the whole point:

> We want a regression in ANY feature to fail a test before it ships. We
> do NOT want a green-looking table. A matrix row flipped without a real
> test behind it, or an e2e test that drives the system but asserts
> nothing observable, is worse than nothing — it looks like a safety net
> while providing none. Optimise the goal (breakage gets caught), never
> the proxy (rows, counts, green).

## What "e2e" means here

An e2e test exercises the application **from the outside, across its real
seams**, and asserts **externally observable outcomes**. The outside can
be: a CLI invocation, an HTTP request against the wired-up server, the
public engine API driving a full pipeline (parse → compile → execute →
persist), a message on a queue. What must be REAL is the application's own
wiring; what may be faked is the *external* boundary (the LLM, the
third-party API, the clock) — faked at the seam the application itself
defines, never by short-circuiting the application's internals.

**Deterministic-first.** A test that needs no credential, no network, no
real LLM — and runs in CI on every change — is worth ten opt-in ones.
Order of preference for each feature:

1. **Deterministic e2e** in the repo's CI suite (stub the external seam,
   drive the real pipeline, assert observable invariants).
2. **Opt-in/live e2e** (repo's live/e2e-credentials layer, if it has one)
   — ONLY when the feature's essence *is* the live external (e.g. real
   model behaviour). Matrix status `covered-live` + justification.
3. **`unit-only`** — when the feature is a pure function already fully
   asserted at unit level and an e2e would only re-test the harness.
   Requires a justification in the matrix.
4. **`excluded`** — when exercising it demands infrastructure the repo
   cannot fake (real third-party OAuth, a specific cloud control plane).
   Requires the reason. Never silently skip: the matrix must say which of
   these four a feature landed on.

## The anti-vacuity doctrine (non-negotiable)

The single test every e2e test must pass — the **feature-level mutation
test**:

> If the feature were broken — the wire between components cut, the flag
> silently ignored, the status never persisted, the handler returning
> garbage — would this test FAIL? If not, it is a façade. Delete it and
> write one that would.

Concrete façade patterns that are **banned**:

- **Stub-echo tests** — asserting that your stub/fake returned what you
  configured it to return. The assertion must be about what the SYSTEM
  did with it: state persisted, events emitted, status transitioned, exit
  code, HTTP response, files written.
- **Harness-only tests** — exercising the test fixtures/helpers without
  crossing the application's real seams. Tell: removing the application
  code would not change the test's outcome.
- **No-invariant tests** — driving a whole pipeline and asserting only
  "no error". "It didn't crash" is a real assertion only when
  not-crashing on that input is the specific contract (say so).
- **Unit-tests-in-e2e-clothing** — re-asserting a pure function through
  the whole stack. That adds runtime, not risk coverage; the feature is
  `unit-only`, mark it so.
- **Flaky constructs** — sleeps as synchronization, wall-clock
  dependence, unseeded randomness, real network in a test that claims to
  be deterministic. A flaky net is torn down, not maintained.
- **Borrowed claims** — flipping a matrix row by citing a test that does
  not exist (the gate greps and goes red) or a pre-existing test that
  does not actually exercise THAT feature (survives the grep, violates
  this doctrine, and gets caught at review).

## What a good e2e test asserts

- **The feature's observable contract**: what an operator/user sees when
  it works — final status, persisted artifacts, emitted event sequence,
  response body/status code, exit code, files on disk, side-effects on
  the store.
- **The failure path too**: the error the feature is specified to
  produce, the fallback it is specified to take, the guard it is
  specified to enforce. Feature bugs live disproportionately in the
  paths that only fire when something goes wrong.
- **Through the front door**: build the input the way a user would (the
  real file format, the real CLI flags, the real request shape), not by
  pre-fabricating internal state the application would never accept.
- **House idiom**: find the repo's neighbouring e2e tests and copy their
  structure, helpers, naming, and assertion style. A new harness is a
  last resort — see [[verify-tests]]; if the repo has an e2e harness,
  plug into it.

## The workflow

1. **Inventory first** (pass 1, or when the matrix is missing/stale):
   enumerate features per [[feature-inventory]]; reconcile any existing
   coverage docs; MAP pre-existing tests onto features (legitimate work,
   no new code needed — cite them); commit the matrix. See
   [[coverage-matrix]] for the exact file contract the deterministic
   gate parses.
2. **Living todo** = the `uncovered` rows in scope, re-prioritized as
   you learn: critical paths (data integrity, auth/permission
   boundaries, persistence, money) → recently-changed / high-blast-radius
   features → the long tail.
3. **The repeated unit**, one feature at a time: pin the observable
   contract → write the deterministic e2e test in the repo's idiom → run
   it with the repo's own runner and SEE IT PASS → flip the matrix row
   (status + test reference) → ONE commit carrying test + row
   (`test(e2e): <feature locked down>`).
4. **Verify gate** (deterministic, no LLM): the repo's build+suite is
   re-run for real, and the matrix contract is enforced — parse,
   statuses, justifications, and the claims grep. This is the floor; it
   cannot be argued with.
5. **Report honestly**: `coverage_complete` only when a fresh re-read of
   the matrix shows every row in scope terminal and justified.

## Safety

- Add tests and the matrix; do **not** alter product code to make a test
  pass. The rare exception is a genuine, separately-flagged testability
  seam (an injectable clock, a constructor hook) — call it out loudly.
- A new test that fails because it found a REAL BUG is a success: keep
  it, report the bug, leave the row `uncovered` with a note. Never weaken
  a test to force green. Prefer the **KNOWN-BUG tripwire** form over
  expected-fail/skip when the defect has a deterministic observable state
  (an error boundary, a wrong status, a crash message): assert the DEFECT
  itself, name the test `KNOWN BUG — …`, and keep the intended positive
  assertion in an adjacent comment — the day the bug is fixed the
  tripwire goes red, which is precisely the signal to flip the test to
  the positive contract. A skipped test rots silently; a tripwire cannot.
- Never touch version-control state destructively (see [[verify-tests]]
  for the `.git`-safety rules).
