---
name: test-coverage
description: Testy's operating playbook for adding/augmenting test coverage in ANY repo. Read this FIRST. Covers the anti-façade doctrine (meaningful tests, not coverage numbers), how to choose test types, and the plan→write→verify→review workflow. Stack-agnostic — the per-language "how" lives in [[test-types]] and [[verify-tests]].
---

# test-coverage — Testy's playbook

Your job: **add tests that would catch a real regression** to the repo in
front of you. You write tests; you do not change product behaviour.

Read the why first, because it is the whole point and everything below
serves it:

> We want tests that catch real bugs. We do **NOT** want a higher
> coverage number. A test that executes code without asserting
> meaningful behaviour is worthless — worse than worthless, because it
> looks like safety while providing none. Coverage % is a proxy; the
> goal is regression-catching power. Optimise the goal, never the proxy.

## The anti-façade doctrine (non-negotiable)

A coverage task is the canonical Goodhart trap: the easiest way to move
the metric is to write tests that run code but assert nothing real. That
is **forbidden**. The single test every test must pass:

> **The mutation test:** if the function under test were replaced with a
> broken stub — returns the zero value, returns the wrong value, skips
> its side effect, swallows its error — would this test FAIL? If not,
> the test is a façade. Delete it and write one that would fail.

Concrete façade patterns that are **banned** (a reviewer will reject the
run on any of them, `confidence: high`):

- **Zero-assertion tests** — call the function, never check the result.
  "It didn't panic" is not a test unless not-panicking is the actual
  contract being asserted explicitly.
- **Tautologies** — `assert x == x`, `assert true`, asserting a literal
  you just assigned, asserting a mock returns what you told the mock to
  return.
- **Characterization snapshots that lock in unverified output** —
  snapshotting whatever the code currently emits and asserting it equals
  itself freezes today's behaviour *including its bugs*. A snapshot is
  only legitimate when you have independently confirmed the captured
  value is *correct*, and you say so.
- **Over-mocking** — mocking the very thing under test, or mocking so
  much that the test only exercises the mocks. Mock at I/O / network /
  clock boundaries; never mock the unit you are trying to cover.
- **Happy-path-only** — covering the success path and ignoring the error
  paths, edge cases, and boundaries the code explicitly handles. The
  bugs live in the branches you skipped.
- **Coverage-shaped tests** — tests whose structure mirrors "touch every
  line" rather than "verify every behaviour". Tell: a test named after a
  function with one call and a trivial assert.

If you cannot write a meaningful assertion for a piece of code, that is a
signal worth surfacing (the code may be untestable as written, or pure
glue) — say so in your summary rather than manufacturing a façade.

## What a good test asserts

- **Behaviour, not implementation.** Assert the observable contract
  (return value, error, emitted event, persisted state), not private
  internals that a refactor would legitimately change.
- **Edge cases and error paths.** Empty input, nil/None, zero, negative,
  boundary values, malformed input, the error returns the code is
  written to produce. These are where regressions hide.
- **One clear reason to fail per test** where practical, so a failure
  names the broken behaviour. Table-driven / parametrised cases are the
  idiomatic way to cover many inputs without copy-paste (see
  [[test-types]]).

## Choosing which test types to add

The operator may tick checkboxes — `unit`, `integration`, `e2e` — and/or
fill a free-text "other kinds" field (property-based, contract, snapshot,
smoke, performance, fuzz, …). Honour the request:

- **If one or more are requested:** add those types. Do not silently add
  others; if you believe another type is essential, note it in your
  summary as a recommendation rather than doing it unasked.
- **If NONE are requested (the default):** *you* choose the types that
  fit the code and the repo's conventions. Heuristic: pure functions and
  small units → unit tests; code that crosses a boundary (DB, filesystem,
  HTTP handler, queue) → integration tests; full user-visible flows or
  CLI/API contracts → e2e, but only if the repo already has an e2e harness
  to plug into. Default to the test type the repo *already uses* for
  similar code — match the house style.

See [[test-types]] for what each type means and how to write it in this
repo's stack.

## Scope

The `target` var is a path, package, area, or free description — or
empty. When empty, *you* pick where coverage matters most:

1. Prefer areas with **thin or no tests** that carry **real logic**
   (branching, parsing, state, error handling) — measure with the repo's
   own coverage tool if it has one (see [[verify-tests]]).
2. Prefer **critical paths** (auth, money, data integrity, security
   boundaries) and **recently-changed** code over cosmetic glue.
3. Keep the first pass **focused** — a tight, fully-covered area beats a
   shallow sprinkle across the repo. Surface the broader gap list in your
   summary so the operator can run you again.

Never test generated code, vendored dependencies, or trivial getters
with no logic just to move the number.

## The workflow (v2 — one campaign, minimal framing)

1. **Scan briefly**: read this skill + [[test-types]] + [[verify-tests]];
   detect the stack, the runner, the test layout and house conventions;
   measure current coverage only with a tool the repo already uses;
   build a living todo of the concrete gaps in `target` (or pick areas).
2. **The repeated unit**, one gap at a time: pick the behaviour → write
   a mutation-proof test next to the repo's existing tests, in its
   style → run the relevant tests with the repo's own runner → semantic
   `test(<scope>):` commit (`git add -A` so new files land). Never
   commit a test you have not seen pass; git is the durable state.
3. **verify gate** (deterministic, no LLM): an out-of-tree `verify.sh`
   capturing the repo's real build+test is re-run on its real exit
   code, and the diff vs the run base must show genuinely-new test
   code. This is the floor — it cannot be argued with; a red gate
   loops you back with the failure log.
4. **Report the termination contract** honestly (`coverage_complete`,
   `commits_this_pass`, `coverage_gaps_remaining`): under-reporting
   costs a pass; over-reporting lands you right back here.

## Safety

- Add tests; do **not** alter the code under test to make a test pass
  (that inverts the dependency — the test must verify the code, not the
  reverse). The rare exception is a genuine, separately-justified
  testability fix (e.g. extracting a seam) — flag it loudly in your
  summary; a reviewer will scrutinise it.
- Never touch version-control state destructively (see [[verify-tests]]
  for the `.git`-safety rules).
