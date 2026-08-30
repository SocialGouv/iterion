# test-coverage (Testy)

Autonomous test-coverage augmentation — **v2 minimal-framing** (ADR-058).
ONE adaptive `campaign` agent builds a living todo of coverage gaps for
the target (or picks the areas that matter most), writes
regression-catching tests with the repo's OWN framework, and commits each
gap in stride (`test(scope):`). A deterministic gate re-runs the repo's
own suite AND verifies genuinely-new test code landed in the diff; a
bounded continuation loop re-pokes the campaign until coverage is
complete and the gate is green. git is the durable state.

**Anti-façade by design:** the success metric is meaningful tests that
would catch a real regression, never coverage percentage. The campaign's
contract enforces the mutation test (a test must FAIL if the code under
test were stubbed broken) and forbids zero-assertion tests, tautologies,
unverified snapshots, over-mocking and happy-path-only; the gate's
negative-space check makes "I added tests" unfakeable — the diff must
show them. See [skills/test-coverage.md](skills/test-coverage.md) for the
doctrine.

## Inputs

| Var | Required | Default | Description |
|---|---|---|---|
| `target` | no | `""` | Path / package / area / free description to cover. Empty ⇒ Testy picks the lowest-coverage / most-critical / recently-changed code. |
| `test_unit` | no | `false` | ☐ Add unit tests. |
| `test_integration` | no | `false` | ☐ Add integration tests. |
| `test_e2e` | no | `false` | ☐ Add end-to-end tests. |
| `extra_test_kinds` | no | `""` | Free-text "other" kinds (property-based, contract, snapshot, smoke, performance…). |
| `baseline` | no | `""` | Known pre-existing failures to SKIP (empty = cheap stash-check once). |
| `max_passes` | no | `8` | Continuation-loop cap. |
| `workspace_dir` | no | `${PROJECT_DIR}` | Workspace root (resolves to the run worktree under `worktree: auto` — do not override). |
| `plan_review` | no | `auto` | Cross-model plan phase: `auto` resolves at launch from the run's credentials; `on` / `off` force it — see below. |
| `plan_review_policy` | no | `skip` | Mid-run peer failure: `skip` proceeds unreviewed, `wait` parks the run — see below. |

When **no** test type is checked and `extra_test_kinds` is empty (the default),
Testy chooses the types that fit the code and the repo's conventions.

## Shape (v2 — one agent, minimal framing)

```
plan_topology → campaign                        when the plan phase is off
plan_topology → plan → plan_review → plan_gate  when it is on
plan_gate     → plan_revise → campaign          peer served
plan_gate     → campaign                        peer skipped (unreviewed plan)

campaign → verify_build → verify_run → gate
gate → done            when converged (suite green AND new test code AND coverage_complete)
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → done            (loop exhausted — ship what is banked)
```

- `campaign` — one adaptive claude_code agent: coverage scan, living todo
  of gaps, one verified `test:` commit per gap, mutation-test discipline
  in the contract.
- `verify_build` + `verify_run` — the stack-agnostic deterministic gate:
  an agent writes `<scratch>/verify.sh` from the repo's own toolchain
  (see `skills/verify-build.md` + `skills/verify-tests.md`), a tool node
  re-runs it, gates on the real exit code, and checks the diff for
  genuinely-new test code (universal test-naming conventions — the
  in-tree `.test_coverage.verify.sh` scratch of v1 is gone).
- `gate` — deterministic compute:
  `converged = passed && new_test_code && coverage_complete`.

## Plan phase (cross-model pair review, ADR-091)

`plan_review: auto` resolves at launch from the run's credentials: when a
SECOND model family is available, the coverage plan is authored (claude,
read-only) from the target and the test-kind selection, critiqued by a
cross-family peer (`claw` + `openai/gpt-5.6-sol` by default), and revised
by the SAME author session before the campaign writes tests; otherwise
the phase is bypassed whole (the v2 shape, unchanged — `plan_topology`
routes straight to `campaign`). The plan reaches the campaign as a MAP,
not a contract.

`plan_review_policy` picks the mid-run peer-unavailability behaviour:
`skip` (default — the reviewer's `action: skip` route completes it with a
zero-value critique stamped `_skipped`, so the plan proceeds unreviewed
rather than parking the campaign on a dead peer credential) or `wait`
(the failure stays `failed_resumable` and the run-level usage-window
retry resumes it when the window reopens — the deliberate-spend
posture).

Only the first pass plans: the continuation back-edge blanks the plan
fields, so later passes read `git log` instead of re-anchoring on a stale
plan.

The v1 staged pipeline (plan → act → simplify → verify_run_tests →
repair_tests → alternating cross-family review/fix loop →
prepare_commit → commit_changes) is retired — see the header comment in
`main.bot` and git history for the design.

## Run

```bash
iterion run bots/test-coverage/main.bot \
  --var target='pkg/log' --var test_unit=true
```

Stack-agnostic: how to detect the runner, where tests live, and how to write
each test type lives in [skills/](skills/) — adding a language needs no DSL edit.
See [main.bot](main.bot) for the full DSL.
