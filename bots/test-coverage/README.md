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
| `plan_review` | no | `auto` | Cross-model plan review (ADR-091). `auto` = on iff ≥2 model families are credentialed at launch; `on` forces it (a missing peer credential then fails loudly); `off` bypasses the plan phase. |
| `plan_review_policy` | no | `skip` | What a MID-RUN peer failure does: `skip` (continue unreviewed, loudly stamped) or `wait` (park `failed_resumable` and let the usage-window retry resume). |
| `workspace_dir` | no | `${PROJECT_DIR}` | Workspace root (resolves to the run worktree under `worktree: auto` — do not override). |

When **no** test type is checked and `extra_test_kinds` is empty (the default),
Testy chooses the types that fit the code and the repo's conventions.

## Shape (v2 — one agent, minimal framing)

```
plan_topology → plan → plan_review → plan_gate → plan_revise → campaign
              ↳ campaign                     (when plan_review resolves off)
campaign → verify_build → verify_run → gate
gate → done            when converged (suite green AND new test code AND coverage_complete)
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → done            (loop exhausted — ship what is banked)
```

**Plan phase (ADR-091).** `plan_review: auto` resolves at launch from the
run's credentials: when a SECOND model family is available, the plan is
authored (claude, read-only), critiqued by a cross-family peer
(`claw` + `openai/gpt-5.6-sol` by default), and revised by the SAME author
session before the campaign runs; otherwise the phase is bypassed whole
(`plan_topology -> campaign`, the v2 shape unchanged).
`plan_review_policy` picks the mid-run peer-unavailability behaviour:
`skip` (default — the reviewer's `action: skip` route: continue unreviewed,
loudly stamped; the peer is an optional enrichment and must never block the
campaign) or `wait` (the run parks failed_resumable, the usage-window retry
resumes it — the deliberate-spend posture).

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
